package extmeta

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/diagnostics"
	"golang.org/x/sync/singleflight"
)

var (
	ErrInvalidConfig  = errors.New("invalid metadata configuration")
	ErrBusy           = errors.New("metadata task already running")
	ErrUnavailable    = errors.New("metadata source is unavailable")
	ErrRemoteMetadata = errors.New("could not fetch metadata repository manifest")
)

type ArchiveMetaReader func(context.Context, database.Book) (*EmbeddedMeta, error)

type ManagerOptions struct {
	ProxyURLProvider  func(context.Context) string
	ArchiveMetaReader ArchiveMetaReader
	// HTTPClient, when supplied, replaces the default SSRF-protected client.
	HTTPClient *http.Client
}

type Manager struct {
	store   *database.Store
	bangumi *bangumi.Service
	covers  *CoverService
	root    string
	logger  *slog.Logger
	opts    ManagerOptions

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	stop   sync.Once

	mu             sync.RWMutex
	closed         bool
	scheduler      bool
	activeSync     map[string]bool
	syncs          []SyncRun
	scrapes        []ScrapeRun
	scrapeCancel   context.CancelFunc
	scrapeFlush    chan struct{}
	online         OnlineStatus
	nextOnline     time.Time
	sidecars       map[string]*SidecarStore
	dictionaries   map[string]*TagDictionary
	mappings       map[string][]MappingRecord
	mappingIndex   *MappingIndex
	fallbackCovers map[string]fallbackCoverLookup
	fallbackEpoch  uint64
	fallbackFlight singleflight.Group
}

func NewManager(store *database.Store, bgm *bangumi.Service, covers *CoverService, dataDir string, logger *slog.Logger, opts ManagerOptions) (*Manager, error) {
	root := filepath.Join(dataDir, "extmeta")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		store: store, bangumi: bgm, covers: covers, root: root, logger: logger, opts: opts,
		ctx: ctx, cancel: cancel, activeSync: make(map[string]bool),
		sidecars: make(map[string]*SidecarStore), dictionaries: make(map[string]*TagDictionary),
		mappings: make(map[string][]MappingRecord),
	}
	sources, err := store.ListMetadataSources(ctx, "")
	if err != nil {
		cancel()
		return nil, err
	}
	for _, source := range sources {
		if source.Ready {
			if err := m.loadSource(source); err != nil {
				ready := false
				msg := "metadata artifacts need to be synchronized"
				_, _ = store.UpdateMetadataSourceSyncState(ctx, source.ID, database.MetadataSourceSyncState{Ready: &ready, LastError: &msg})
			}
		}
	}
	m.rebuildMappings(sources)
	if bgm != nil {
		bgm.SetMetadataArchiveClient(func(ctx context.Context) *http.Client { return m.client(ctx, 3*time.Hour) })
	}
	return m, nil
}

func (m *Manager) client(ctx context.Context, timeout time.Duration) *http.Client {
	if m.opts.HTTPClient != nil {
		return m.opts.HTTPClient
	}
	return NewSafeHTTPClient(m.proxyURL(ctx), timeout)
}

func (m *Manager) proxyURL(ctx context.Context) string {
	if m.opts.ProxyURLProvider != nil {
		return m.opts.ProxyURLProvider(ctx)
	}
	return ""
}

func (m *Manager) Close() {
	m.stop.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		m.mu.Unlock()
		m.wg.Wait()
		m.mu.Lock()
		defer m.mu.Unlock()
		for id, sidecar := range m.sidecars {
			_ = sidecar.Close()
			delete(m.sidecars, id)
		}
	})
}

func (m *Manager) SubscribeRepo(ctx context.Context, rawURL string) (database.MetadataRepo, error) {
	return m.fetchRepo(ctx, database.MetadataRepo{URL: strings.TrimSpace(rawURL), Enabled: true})
}

func (m *Manager) RefreshRepo(ctx context.Context, repoID string) (database.MetadataRepo, error) {
	repo, err := m.store.GetMetadataRepo(ctx, repoID)
	if err != nil {
		return repo, err
	}
	return m.fetchRepo(ctx, repo)
}

func (m *Manager) fetchRepo(ctx context.Context, repo database.MetadataRepo) (result database.MetadataRepo, err error) {
	started := time.Now()
	action := "subscribe"
	if repo.ID != "" {
		action = "refresh"
	}
	defer func() {
		level, proxy := diagnostics.LevelInfo, ""
		if err != nil {
			level, proxy = diagnostics.LevelWarn, m.proxyURL(ctx)
		}
		event := metadataDiagnostic("extmeta_repo", action, level, err, proxy)
		event.DurationMs = time.Since(started).Milliseconds()
		if err != nil && m.logger != nil {
			m.logger.Warn("metadata repository operation failed", "action", action, "code", event.Code, "error", event.Detail)
		}
		diagnostics.Default().Record(event)
	}()

	u, err := validateHTTPURL(repo.URL)
	if err != nil || u.User != nil {
		return repo, ErrInvalidConfig
	}
	client := m.client(ctx, 30*time.Second)
	defer client.CloseIdleConnections()
	manifest, err := FetchRepoManifest(ctx, client, repo.URL)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
			errors.Is(err, ErrSSRFBlocked), errors.Is(err, ErrInvalidProxyURL), errors.Is(err, ErrInvalidManifest):
			return repo, err
		default:
			return repo, fmt.Errorf("%w: %w", ErrRemoteMetadata, err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return repo, ErrUnavailable
	}
	if repo.ID != "" {
		repo, err = m.store.GetMetadataRepo(ctx, repo.ID)
		if err != nil {
			return repo, err
		}
	} else {
		repos, err := m.store.ListMetadataRepos(ctx)
		if err != nil {
			return repo, err
		}
		for _, existing := range repos {
			if existing.URL == repo.URL {
				repo = existing
				break
			}
		}
	}
	sources, err := m.store.ListMetadataSources(ctx, "")
	if err != nil {
		return repo, err
	}
	existing := make(map[string]database.MetadataSource)
	for _, src := range sources {
		if repo.ID != "" && src.RepoID == repo.ID {
			if m.activeSync[src.ID] {
				return repo, ErrBusy
			}
			existing[src.SourceKey] = src
		}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return repo, err
	}
	repo.Name, repo.Description, repo.Homepage = manifest.Name, manifest.Description, manifest.Homepage
	var oldManifest RepoManifest
	_ = json.Unmarshal([]byte(repo.RawJSON), &oldManifest)
	oldDownloads := make(map[string]string, len(oldManifest.Sources))
	for _, item := range oldManifest.Sources {
		oldDownloads[item.ID] = item.DownloadURL
	}
	repo.SchemaVersion, repo.RepoVersion, repo.RawJSON = manifest.Schema, firstNonEmpty(manifest.Version, manifest.UpdatedAt), string(raw)
	repo.LastSyncedAt, repo.LastError = timestamp(), ""
	// Declare dictionaries first so manifest-local dictionary keys can be resolved to DB IDs.
	ordered := slices.Clone(manifest.Sources)
	slices.SortStableFunc(ordered, func(a, b RepoSource) int {
		if a.Kind == KindTagDictionary && b.Kind != KindTagDictionary {
			return -1
		}
		if b.Kind == KindTagDictionary && a.Kind != KindTagDictionary {
			return 1
		}
		return 0
	})
	updates := make([]database.MetadataRepoSourceUpdate, 0, len(ordered))
	for _, item := range ordered {
		src, found := existing[item.ID]
		var dictionaryKey string
		if !found {
			src = database.MetadataSource{RepoID: repo.ID, SourceKey: item.ID, Enabled: item.Default}
			src.AutoUpdate = firstNonEmpty(string(item.AutoUpdateDefault), "manual")
			dictionaryKey = item.DefaultTagDictID
		}
		// A changed kind would give the same ID incompatible on-disk artifacts and bindings.
		if found && src.Kind != item.Kind {
			continue
		}
		src.Kind, src.Name, src.Description = item.Kind, item.Name, item.Description
		src.ManifestURL, src.ReleaseAPIURL, src.AssetName = item.ManifestURL, item.ReleaseAPIURL, item.AssetName
		if !found || src.DownloadURL == "" || oldDownloads[item.ID] == src.DownloadURL {
			src.DownloadURL = item.DownloadURL
		}
		if !found || !src.Ready {
			src.SchemaType = item.Schema
		}
		src.Compression, src.NSFWRule = item.Compression, item.NSFWRule
		updates = append(updates, database.MetadataRepoSourceUpdate{Source: src, TagDictionaryKey: dictionaryKey})
	}
	saved, err := m.store.UpsertMetadataRepoWithSources(ctx, repo, updates)
	if err == nil {
		m.invalidateFallbackCoverCacheLocked()
	}
	return saved, err
}

func validateSource(src database.MetadataSource, local bool) error {
	if !isValidSourceKind(src.Kind) || strings.TrimSpace(src.Name) == "" {
		return ErrInvalidConfig
	}
	switch strings.ToLower(strings.TrimSpace(src.AutoUpdate)) {
	case "", "manual", "weekly", "monthly":
	default:
		return ErrInvalidConfig
	}
	switch strings.ToLower(strings.TrimSpace(src.TagDisplayMode)) {
	case "", "zh", "bilingual", "raw":
	default:
		return ErrInvalidConfig
	}
	for _, raw := range []string{src.DownloadURL, src.ReleaseAPIURL, src.OnlineAPIURL, src.ManifestURL} {
		if raw == "" || raw == src.ManifestURL && src.Kind == KindBangumiArchive && (raw == bangumi.ArchiveSourceZH || raw == bangumi.ArchiveSourceOfficial) {
			continue
		}
		if u, err := validateHTTPURL(raw); err != nil || u.User != nil {
			return ErrInvalidConfig
		}
	}
	if local && src.LocalPath != "" {
		if src.Kind != KindOfflineSQLite {
			return ErrInvalidConfig
		}
		_, err := checkedLocalFile(src.LocalPath)
		return err
	}
	return nil
}

func checkedLocalFile(path string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || slices.Contains(strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }), "..") {
		return nil, ErrInvalidConfig
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(path) {
		return nil, ErrInvalidConfig
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrInvalidConfig
	}
	return info, nil
}

const walWarningMessage = "检测到同目录存在未 Checkpoint 的 -wal 日志，可能缺少最新增量数据；请先停止 Komga 或执行 Checkpoint 后再同步"

// SourceView enriches a MetadataSource record with resolved SQLite schema format
// and un-checkpointed WAL status for API responses.
type SourceView struct {
	database.MetadataSource
	SchemaFormat      string `json:"schemaFormat,omitempty"`
	WALWarning        bool   `json:"walWarning"`
	WALWarningBytes   int64  `json:"walWarningBytes"`
	WALWarningMessage string `json:"walWarningMessage,omitempty"`
}

func (m *Manager) SourceView(ctx context.Context, src database.MetadataSource) SourceView {
	view := SourceView{
		MetadataSource: src,
		SchemaFormat:   strings.TrimSpace(src.SchemaType),
	}
	if strings.EqualFold(view.SchemaFormat, "auto") {
		view.SchemaFormat = ""
	}
	if src.Kind != KindOfflineSQLite {
		return view
	}

	m.mu.RLock()
	sidecar := m.sidecars[src.ID]
	m.mu.RUnlock()
	if sidecar != nil && (view.SchemaFormat == "" || (view.SchemaFormat != SchemaMangaAlphaMetaV2 && sidecar.Schema() != "")) {
		if view.SchemaFormat == "" {
			view.SchemaFormat = sidecar.Schema()
		}
	}

	upstream := m.upstreamPath(src)
	if upstream != "" {
		if info, err := os.Stat(upstream); err == nil && info.Mode().IsRegular() {
			walWarn, walBytes := DetectSQLiteWALWarning(upstream)
			view.WALWarning = walWarn
			view.WALWarningBytes = walBytes
			if walWarn {
				view.WALWarningMessage = walWarningMessage
			}
			if view.SchemaFormat == "" {
				if probe, err := ProbeSQLiteFile(ctx, upstream); err == nil && probe.Format != "" {
					view.SchemaFormat = probe.Format
				}
			}
		}
	}
	return view
}

func (m *Manager) SourceViews(ctx context.Context, sources []database.MetadataSource) []SourceView {
	out := make([]SourceView, len(sources))
	for i, src := range sources {
		out[i] = m.SourceView(ctx, src)
	}
	return out
}

func schemaCompatible(requested, detected string) bool {
	req := strings.ToLower(strings.TrimSpace(requested))
	det := strings.ToLower(strings.TrimSpace(detected))
	if req == "" || req == "auto" {
		return true
	}
	if req == det {
		return true
	}
	return (req == SchemaMangaAlphaMetaV1 || req == SchemaMangaAlphaMetaV2) &&
		(det == SchemaMangaAlphaMetaV1 || det == SchemaMangaAlphaMetaV2)
}

func probeOfflineCatalogSQLite(ctx context.Context, path, requestedSchema string) (SQLiteProbeResult, string, error) {
	probe, err := ProbeSQLiteFile(ctx, path)
	if err != nil {
		return SQLiteProbeResult{}, "", err
	}
	switch probe.Format {
	case SchemaKomgaSQLiteV1, SchemaMangaAlphaMetaV1, SchemaMangaAlphaMetaV2, SchemaGalleryV1:
	default:
		return SQLiteProbeResult{}, "", fmt.Errorf("%w: %s", ErrUnsupportedSchema, probe.Format)
	}
	if !schemaCompatible(requestedSchema, probe.Format) {
		return SQLiteProbeResult{}, "", fmt.Errorf("%w: expected %s, got %s", ErrUnsupportedSchema, requestedSchema, probe.Format)
	}
	resolved := probe.Format
	return probe, resolved, nil
}

func (m *Manager) AddCustomSource(ctx context.Context, src database.MetadataSource) (database.MetadataSource, error) {
	// Only configuration fields may come from the request.
	src = database.MetadataSource{
		Kind: strings.TrimSpace(src.Kind), SchemaType: strings.TrimSpace(src.SchemaType), Name: src.Name, Description: src.Description,
		DownloadURL: src.DownloadURL, ManifestURL: src.ManifestURL, ReleaseAPIURL: src.ReleaseAPIURL,
		AssetName: src.AssetName, Compression: src.Compression, LocalPath: strings.TrimSpace(src.LocalPath),
		TagDictSourceID: src.TagDictSourceID, TagDisplayMode: src.TagDisplayMode, NSFWRule: src.NSFWRule,
		AutoUpdate: src.AutoUpdate, OnlineAPIURL: src.OnlineAPIURL, OnlineEnabled: src.OnlineEnabled, Enabled: src.Enabled,
	}
	if err := validateSource(src, true); err != nil {
		return database.MetadataSource{}, err
	}
	if src.Kind == KindOfflineSQLite && src.LocalPath != "" {
		_, resolvedSchema, err := probeOfflineCatalogSQLite(ctx, src.LocalPath, src.SchemaType)
		if err != nil {
			return database.MetadataSource{}, err
		}
		if src.SchemaType == "" || strings.EqualFold(src.SchemaType, "auto") ||
			(src.SchemaType == SchemaMangaAlphaMetaV1 && resolvedSchema == SchemaMangaAlphaMetaV2) {
			src.SchemaType = resolvedSchema
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return database.MetadataSource{}, ErrUnavailable
	}
	return m.store.UpsertMetadataSource(ctx, src)
}

type SourceConfig struct {
	Name            *string `json:"name"`
	Description     *string `json:"description"`
	SchemaType      *string `json:"schemaType"`
	ManifestURL     *string `json:"manifestUrl"`
	ReleaseAPIURL   *string `json:"releaseApiUrl"`
	AssetName       *string `json:"assetName"`
	Compression     *string `json:"compression"`
	NSFWRule        *string `json:"nsfwRule"`
	Enabled         *bool   `json:"enabled"`
	TagDictSourceID *string `json:"tagDictSourceId"`
	TagDisplayMode  *string `json:"tagDisplayMode"`
	AutoUpdate      *string `json:"autoUpdate"`
	LocalPath       *string `json:"localPath"`
	DownloadURL     *string `json:"downloadUrl"`
	OnlineAPIURL    *string `json:"onlineApiUrl"`
	OnlineEnabled   *bool   `json:"onlineEnabled"`
}

func setOption[T any](field *T, option *T) {
	if option != nil {
		*field = *option
	}
}

func (m *Manager) UpdateSourceConfig(ctx context.Context, id string, cfg SourceConfig) (database.MetadataSource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return database.MetadataSource{}, ErrUnavailable
	}
	if m.activeSync[id] {
		return database.MetadataSource{}, ErrBusy
	}
	src, err := m.store.GetMetadataSource(ctx, id)
	if err != nil {
		return src, err
	}
	before := src
	if src.RepoID != "" && (cfg.Name != nil || cfg.Description != nil || cfg.SchemaType != nil ||
		cfg.ManifestURL != nil || cfg.ReleaseAPIURL != nil || cfg.AssetName != nil || cfg.Compression != nil || cfg.NSFWRule != nil) {
		return src, ErrInvalidConfig
	}
	setOption(&src.Name, cfg.Name)
	setOption(&src.Description, cfg.Description)
	setOption(&src.SchemaType, cfg.SchemaType)
	setOption(&src.ManifestURL, cfg.ManifestURL)
	setOption(&src.ReleaseAPIURL, cfg.ReleaseAPIURL)
	setOption(&src.AssetName, cfg.AssetName)
	setOption(&src.Compression, cfg.Compression)
	setOption(&src.NSFWRule, cfg.NSFWRule)
	setOption(&src.Enabled, cfg.Enabled)
	setOption(&src.TagDictSourceID, cfg.TagDictSourceID)
	setOption(&src.TagDisplayMode, cfg.TagDisplayMode)
	setOption(&src.AutoUpdate, cfg.AutoUpdate)
	setOption(&src.LocalPath, cfg.LocalPath)
	setOption(&src.DownloadURL, cfg.DownloadURL)
	setOption(&src.OnlineAPIURL, cfg.OnlineAPIURL)
	setOption(&src.OnlineEnabled, cfg.OnlineEnabled)
	src.LocalPath = strings.TrimSpace(src.LocalPath)
	src.SchemaType = strings.TrimSpace(src.SchemaType)
	if err := validateSource(src, cfg.LocalPath != nil); err != nil {
		return src, err
	}
	if src.Kind == KindOfflineSQLite && src.LocalPath != "" && (cfg.LocalPath != nil || cfg.SchemaType != nil) {
		_, resolvedSchema, err := probeOfflineCatalogSQLite(ctx, src.LocalPath, src.SchemaType)
		if err != nil {
			return src, err
		}
		if src.SchemaType == "" || strings.EqualFold(src.SchemaType, "auto") ||
			(src.SchemaType == SchemaMangaAlphaMetaV1 && resolvedSchema == SchemaMangaAlphaMetaV2) {
			src.SchemaType = resolvedSchema
		}
	}
	if src.LocalPath != before.LocalPath || src.DownloadURL != before.DownloadURL || src.ManifestURL != before.ManifestURL ||
		src.ReleaseAPIURL != before.ReleaseAPIURL || src.AssetName != before.AssetName || src.Compression != before.Compression ||
		src.SchemaType != before.SchemaType {
		src.Ready, src.EntryCount, src.FileSize = false, 0, 0
		src.RemoteETag, src.RemoteLastMod, src.RemoteUpdatedAt, src.LocalSHA256 = "", "", "", ""
		src.LastCheckedAt, src.LastError, src.SidecarPath = "", "", ""
		if src.Enabled || (!before.Ready && before.LastUpdatedAt == "") {
			src.LastUpdatedAt = ""
		}
	}
	src, err = m.store.UpsertMetadataSource(ctx, src)
	if err != nil {
		return src, err
	}
	m.invalidateFallbackCoverCacheLocked()
	if !src.Ready {
		m.unloadSource(id)
	}
	if src.Kind == KindMappingPack {
		err = m.reloadMappingIndex(ctx)
	}
	return src, err
}

func (m *Manager) DeleteRepo(ctx context.Context, repoID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sources, err := m.store.ListMetadataSources(ctx, "")
	if err != nil {
		return err
	}
	for _, src := range sources {
		if src.RepoID == repoID && m.activeSync[src.ID] {
			return ErrBusy
		}
	}
	if err := m.store.DeleteMetadataRepo(ctx, repoID); err != nil {
		return err
	}
	m.invalidateFallbackCoverCacheLocked()
	for _, src := range sources {
		if src.RepoID == repoID {
			err = errors.Join(err, m.removeArtifacts(src.ID))
		}
	}
	return errors.Join(err, m.reloadMappingIndex(ctx))
}

func (m *Manager) DeleteSource(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeSync[id] {
		return ErrBusy
	}
	if err := m.store.DeleteMetadataSource(ctx, id); err != nil {
		return err
	}
	m.invalidateFallbackCoverCacheLocked()
	return errors.Join(m.removeArtifacts(id), m.reloadMappingIndex(ctx))
}

func (m *Manager) unloadSource(id string) {
	if sidecar := m.sidecars[id]; sidecar != nil {
		_ = sidecar.Close()
		delete(m.sidecars, id)
	}
	delete(m.dictionaries, id)
	delete(m.mappings, id)
}

func (m *Manager) removeArtifacts(id string) error {
	m.unloadSource(id)
	if !isValidSourceID(id) {
		return ErrInvalidConfig
	}
	return errors.Join(
		os.RemoveAll(filepath.Join(m.root, id)),
		removeIfPresent(m.sqlitePath(id)),
		removeIfPresent(m.indexPath(id)),
		removeIfPresent(m.dictionaryPath(id)),
		removeIfPresent(m.dictionaryPath(id)+".gz"),
	)
}

func removeIfPresent(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (m *Manager) reloadMappingIndex(ctx context.Context) error {
	sources, err := m.store.ListMetadataSources(ctx, KindMappingPack)
	if err == nil {
		m.rebuildMappings(sources)
	}
	return err
}

func (m *Manager) rebuildMappings(sources []database.MetadataSource) {
	var records []MappingRecord
	for _, src := range sources {
		if src.Kind == KindMappingPack && src.Enabled && src.Ready {
			records = append(records, m.mappings[src.ID]...)
		}
	}
	m.mappingIndex = NewMappingIndex(records)
}

func (m *Manager) SetLibrarySources(ctx context.Context, libraryID string, ids []string) ([]database.LibraryMetadataSource, error) {
	if err := checkAdmin(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.store.LibraryBasic(ctx, libraryID); err != nil {
		return nil, err
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || id == TargetSourceBangumi {
			continue
		}
		src, err := m.store.GetMetadataSource(ctx, id)
		if err != nil {
			return nil, err
		}
		if src.Kind != KindOfflineSQLite && src.Kind != KindBangumiArchive {
			return nil, ErrInvalidConfig
		}
	}
	if err := m.store.SetLibraryMetadataSources(ctx, libraryID, ids); err != nil {
		return nil, err
	}
	m.invalidateFallbackCoverCacheLocked()
	return m.store.ListLibraryMetadataSources(ctx, libraryID)
}

func timestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func newRunID() string {
	var data [16]byte
	_, _ = rand.Read(data[:])
	return hex.EncodeToString(data[:])
}

func checkAdmin(ctx context.Context) error {
	if scope, ok := database.UserScopeFromContext(ctx); ok && !scope.IsAdmin {
		return sql.ErrNoRows
	}
	return nil
}
