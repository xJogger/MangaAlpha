package extmeta

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/diagnostics"
)

type SyncRun struct {
	ID              string `json:"id"`
	SourceID        string `json:"sourceId"`
	Status          string `json:"status"`
	Phase           string `json:"phase,omitempty"`
	StartedAt       string `json:"startedAt"`
	CompletedAt     string `json:"completedAt,omitempty"`
	Error           string `json:"error,omitempty"`
	DownloadedBytes int64  `json:"downloadedBytes"`
	TotalBytes      int64  `json:"totalBytes"`
	Indexed         int64  `json:"indexed"`
	IndexedEntries  int64  `json:"indexedEntries"`
}

const runHistoryLimit = 100

func (m *Manager) StartSourceSync(ctx context.Context, id string, force bool) (SyncRun, error) {
	return m.startSourceSync(ctx, id, force, false)
}

func (m *Manager) startSourceSync(ctx context.Context, id string, force, scheduled bool) (SyncRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return SyncRun{}, ErrUnavailable
	}
	src, err := m.store.GetMetadataSource(ctx, id)
	if err != nil {
		return SyncRun{}, err
	}
	if !isValidSourceID(src.ID) {
		return SyncRun{}, ErrInvalidConfig
	}
	if scheduled && !sourceDue(src, time.Now()) {
		return SyncRun{}, ErrUnavailable
	}
	if m.activeSync[id] {
		return SyncRun{}, ErrBusy
	}
	phase := "downloading"
	if src.Kind == KindOfflineSQLite &&
		((src.LocalPath != "" && filepath.Clean(src.LocalPath) != m.sqlitePath(src.ID)) ||
			(src.LocalPath == "" && src.DownloadURL == "" && src.ReleaseAPIURL == "")) {
		phase = "indexing"
	}
	run := SyncRun{ID: newRunID(), SourceID: id, Status: "running", Phase: phase, StartedAt: timestamp()}
	m.activeSync[id] = true
	m.syncs = append(m.syncs, run)
	m.wg.Go(func() {
		ctx, cancel := context.WithTimeout(m.ctx, 3*time.Hour)
		defer cancel()
		skipped, err := m.syncSource(ctx, src, force, scheduled, run.ID)
		m.finishSync(src, run.ID, skipped, err)
	})
	return run, nil
}

// ImportUploadedSQLite streams an uploaded SQLite (or compressed .gz/.zst/.zip) database file
// into staging, validates its SQLite magic header and offline catalog schema, builds the sidecar
// FTS5 index, and atomically promotes both artifacts for sourceID without buffering in memory.
func (m *Manager) ImportUploadedSQLite(
	ctx context.Context,
	sourceID string,
	filename string,
	reader io.Reader,
	onProgress func(written int64),
) (database.MetadataSource, error) {
	if strings.TrimSpace(sourceID) == "" {
		return database.MetadataSource{}, ErrInvalidConfig
	}
	return m.ImportConfiguredSQLite(ctx, sourceID, filename, reader, SourceConfig{}, onProgress)
}

// ImportConfiguredSQLite validates the upload and publishes the source and its
// index together. An empty sourceID creates a source only when publication succeeds.
func (m *Manager) ImportConfiguredSQLite(ctx context.Context, sourceID, filename string, reader io.Reader, cfg SourceConfig, onProgress func(int64)) (database.MetadataSource, error) {
	sourceID = strings.TrimSpace(sourceID)
	filename = strings.TrimSpace(filename)
	create := sourceID == ""
	if create {
		sourceID = "msrc_" + strings.ToLower(rand.Text())
	}
	if !isValidSourceID(sourceID) || filename == "" || reader == nil {
		return database.MetadataSource{}, ErrInvalidConfig
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return database.MetadataSource{}, ErrUnavailable
	}
	if m.activeSync[sourceID] {
		m.mu.Unlock()
		return database.MetadataSource{}, ErrBusy
	}
	m.activeSync[sourceID] = true
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.activeSync, sourceID)
		m.mu.Unlock()
	}()

	previous := database.MetadataSource{}
	src := database.MetadataSource{ID: sourceID, Kind: KindOfflineSQLite, Enabled: true}
	if !create {
		var err error
		previous, err = m.store.GetMetadataSource(ctx, sourceID)
		if err != nil {
			return database.MetadataSource{}, err
		}
		src = previous
	}
	if src.Kind != KindOfflineSQLite {
		return database.MetadataSource{}, ErrInvalidConfig
	}
	configured := src
	setOption(&configured.Name, cfg.Name)
	setOption(&configured.Description, cfg.Description)
	setOption(&configured.Enabled, cfg.Enabled)
	setOption(&configured.TagDictSourceID, cfg.TagDictSourceID)
	setOption(&configured.TagDisplayMode, cfg.TagDisplayMode)
	setOption(&configured.AutoUpdate, cfg.AutoUpdate)
	if err := validateSource(configured, false); err != nil {
		return database.MetadataSource{}, err
	}
	if src.RepoID == "" {
		src = configured
	}
	if cfg.Enabled == nil && !src.Ready && src.LastUpdatedAt == "" {
		src.Enabled = true
	}

	staging, err := os.MkdirTemp(m.root, ".upload-")
	if err != nil {
		return database.MetadataSource{}, err
	}
	defer os.RemoveAll(staging)

	upstream := filepath.Join(staging, "upstream.sqlite")
	dlRes, err := StreamDecompressSQLiteToFile(ctx, filename, reader, upstream, defaultMaxUploadStreamBytes, onProgress)
	if err != nil {
		return database.MetadataSource{}, err
	}

	_, resolvedSchema, err := probeOfflineCatalogSQLite(ctx, upstream, "")
	if err != nil {
		return database.MetadataSource{}, err
	}

	var indexed int64
	index := filepath.Join(staging, "index.db")
	if err := BuildSidecarIndex(ctx, upstream, index, resolvedSchema, func(count int64) {
		indexed = count
	}); err != nil {
		return database.MetadataSource{}, err
	}

	src.LocalPath = ""
	src.SchemaType = resolvedSchema
	src.SidecarPath = m.indexPath(src.ID)
	src.Ready, src.EntryCount = true, indexed
	src.FileSize, src.LocalSHA256 = dlRes.DecompressedBytes, dlRes.SHA256
	src.LastUpdatedAt = timestamp()
	src.LastCheckedAt = src.LastUpdatedAt
	src.LastError, src.RemoteETag, src.RemoteLastMod, src.RemoteUpdatedAt = "", "", "", ""
	files := map[string]string{
		m.sqlitePath(src.ID): upstream,
		src.SidecarPath:      index,
	}
	var saved database.MetadataSource
	err = m.publishSource(ctx, src, previous, staging, files, func() error {
		var err error
		saved, err = m.store.UpsertMetadataSource(ctx, src)
		return err
	})
	return saved, err
}

func (m *Manager) Syncs() []SyncRun {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]SyncRun{}, m.syncs...)
	slices.Reverse(out)
	return out
}

func (m *Manager) updateSync(id string, update func(*SyncRun)) {
	for i := range m.syncs {
		if m.syncs[i].ID == id {
			update(&m.syncs[i])
			return
		}
	}
}

func (m *Manager) setSyncPhase(id, phase string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateSync(id, func(run *SyncRun) { run.Phase = phase })
}

func (m *Manager) finishSync(src database.MetadataSource, runID string, skipped bool, err error) {
	checked, message, status, level := timestamp(), DescribeError(err), "success", diagnostics.LevelInfo
	switch {
	case errors.Is(err, context.Canceled):
		status, level = "canceled", diagnostics.LevelWarn
	case err != nil:
		status, level = "failed", diagnostics.LevelError
	case skipped:
		status = "skipped"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var phase string
	m.mu.Lock()
	m.updateSync(runID, func(run *SyncRun) { phase = run.Phase })
	m.mu.Unlock()

	if _, stateErr := m.store.UpdateMetadataSourceSyncState(ctx, src.ID, database.MetadataSourceSyncState{
		LastCheckedAt: &checked, LastError: &message,
	}); stateErr != nil && err == nil {
		status, level, message = "failed", diagnostics.LevelError, "无法保存元数据源的同步状态。"
		err = stateErr
	}

	var proxy string
	if err != nil {
		proxy = m.proxyURL(ctx)
	}
	event := metadataDiagnostic("extmeta_sync", "sync", level, err, proxy)
	event.Meta["kind"], event.Meta["phase"], event.Meta["skipped"] = src.Kind, phase, skipped
	if status == "failed" && m.logger != nil {
		m.logger.Warn("metadata source synchronization failed", "sourceId", src.ID, "code", event.Code, "error", event.Detail)
	}
	diagnostics.Default().Record(event)

	m.mu.Lock()
	delete(m.activeSync, src.ID)
	m.updateSync(runID, func(run *SyncRun) {
		run.Status, run.Error, run.CompletedAt = status, message, checked
	})
	for len(m.syncs) > runHistoryLimit {
		index := slices.IndexFunc(m.syncs, func(run SyncRun) bool { return run.CompletedAt != "" })
		if index < 0 {
			break
		}
		m.syncs = slices.Delete(m.syncs, index, index+1)
	}
	m.mu.Unlock()
}

func (m *Manager) StartScheduler() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.scheduler {
		return
	}
	m.scheduler = true
	m.wg.Go(func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			sources, err := m.store.ListMetadataSources(m.ctx, "")
			if err == nil {
				for _, src := range sources {
					if sourceDue(src, time.Now()) {
						_, _ = m.startSourceSync(m.ctx, src.ID, false, true)
					}
				}
			}
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

func sourceDue(src database.MetadataSource, now time.Time) bool {
	if !src.Enabled || src.AutoUpdate != "weekly" && src.AutoUpdate != "monthly" {
		return false
	}
	last, err := time.Parse(time.RFC3339Nano, src.LastCheckedAt)
	if err != nil {
		return true
	}
	if src.AutoUpdate == "monthly" {
		return !now.Before(last.AddDate(0, 1, 0))
	}
	return !now.Before(last.AddDate(0, 0, 7))
}

func (m *Manager) sqlitePath(id string) string { return filepath.Join(m.root, id+".sqlite") }
func (m *Manager) indexPath(id string) string  { return filepath.Join(m.root, id+".index.db") }
func (m *Manager) dictionaryPath(id string) string {
	return filepath.Join(m.root, "tagdicts", id+".json")
}
func (m *Manager) mappingPath(id string) string { return filepath.Join(m.root, id, "mappings.ndjson") }

func (m *Manager) upstreamPath(src database.MetadataSource) string {
	if src.LocalPath != "" {
		return src.LocalPath
	}
	return m.sqlitePath(src.ID)
}

func (m *Manager) loadSource(src database.MetadataSource) error {
	if !isValidSourceID(src.ID) {
		return ErrInvalidConfig
	}
	switch src.Kind {
	case KindOfflineSQLite:
		if src.LocalPath != "" && filepath.Clean(src.LocalPath) != m.sqlitePath(src.ID) {
			if _, err := checkedLocalFile(src.LocalPath); err != nil {
				return err
			}
		}
		sidecar, err := OpenSidecarStore(src.ID, m.upstreamPath(src), m.indexPath(src.ID), src.SchemaType)
		if err != nil {
			return err
		}
		m.sidecars[src.ID] = sidecar
	case KindTagDictionary:
		dict, err := LoadTagDictionaryFile(m.dictionaryPath(src.ID))
		if err != nil {
			return err
		}
		m.dictionaries[src.ID] = dict
	case KindMappingPack:
		file, err := os.Open(m.mappingPath(src.ID))
		if err != nil {
			return err
		}
		defer file.Close()
		records, err := ParseMappingsNDJSON(file)
		if err != nil {
			return err
		}
		m.mappings[src.ID] = records
	}
	return nil
}

func (m *Manager) syncSource(ctx context.Context, src database.MetadataSource, force, scheduled bool, runID string) (bool, error) {
	client := m.client(ctx, 3*time.Hour)
	defer client.CloseIdleConnections()
	autoEnable := !scheduled && !src.Ready && !src.Enabled && src.LastUpdatedAt == ""
	if src.Kind == KindBangumiArchive {
		return m.syncBangumi(ctx, client, src, force, autoEnable, runID)
	}

	staging, err := os.MkdirTemp(m.root, ".sync-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(staging)
	now, ready := timestamp(), true
	state := database.MetadataSourceSyncState{Ready: &ready, LastUpdatedAt: &now}
	if autoEnable {
		state.Enabled = &ready
	}
	files := make(map[string]string)
	var dict *TagDictionary
	var mappings []MappingRecord
	switch src.Kind {
	case KindOfflineSQLite:
		upstream := m.upstreamPath(src)
		if src.LocalPath != "" && filepath.Clean(src.LocalPath) != m.sqlitePath(src.ID) {
			info, err := checkedLocalFile(upstream)
			if err != nil {
				return false, err
			}
			size := info.Size()
			state.FileSize = &size
		} else if src.DownloadURL == "" && src.ReleaseAPIURL == "" {
			info, err := os.Stat(m.sqlitePath(src.ID))
			if err != nil || !info.Mode().IsRegular() {
				return false, ErrInvalidConfig
			}
			upstream = m.sqlitePath(src.ID)
			size := info.Size()
			state.FileSize = &size
		} else {
			info, err := CheckRemoteUpdate(ctx, client, src.DownloadURL, src.ReleaseAPIURL, src.AssetName,
				src.RemoteETag, src.RemoteLastMod, src.FileSize, src.RemoteUpdatedAt)
			if err != nil {
				return false, err
			}
			if !force && src.Ready && !info.HasUpdate {
				return true, nil
			}
			upstream = filepath.Join(staging, "upstream.sqlite")
			out, err := DownloadAndDecompressToFile(ctx, client, info.DownloadURL, src.Compression, upstream, func(downloaded, total, _ int64) {
				m.mu.Lock()
				m.updateSync(runID, func(run *SyncRun) { run.DownloadedBytes, run.TotalBytes = downloaded, total })
				m.mu.Unlock()
			})
			if err != nil {
				return false, err
			}
			if digest := strings.TrimPrefix(strings.ToLower(info.Digest), "sha256:"); len(digest) == 64 && digest != out.DownloadSHA256 {
				return false, errors.New("metadata download checksum mismatch")
			}
			etag, lastMod := firstNonEmpty(out.ETag, info.ETag), firstNonEmpty(out.LastModified, info.LastMod)
			size := info.Size
			if size <= 0 {
				size = out.DownloadedBytes
			}
			state.RemoteETag, state.RemoteLastMod, state.RemoteUpdatedAt = &etag, &lastMod, &info.UpdatedAt
			state.LocalSHA256, state.FileSize = &out.SHA256, &size
			files[m.sqlitePath(src.ID)] = upstream
		}
		_, resolvedSchema, err := probeOfflineCatalogSQLite(ctx, upstream, src.SchemaType)
		if err != nil {
			return false, err
		}
		if src.SchemaType == "" || strings.EqualFold(src.SchemaType, "auto") ||
			(src.SchemaType == SchemaMangaAlphaMetaV1 && resolvedSchema == SchemaMangaAlphaMetaV2) {
			src.SchemaType = resolvedSchema
		}
		var indexed int64
		index := filepath.Join(staging, "index.db")
		m.setSyncPhase(runID, "indexing")
		err = BuildSidecarIndex(ctx, upstream, index, src.SchemaType, func(count int64) {
			indexed = count
			m.mu.Lock()
			m.updateSync(runID, func(run *SyncRun) { run.Indexed, run.IndexedEntries = count, count })
			m.mu.Unlock()
		})
		if err != nil {
			return false, err
		}
		sidecarPath := m.indexPath(src.ID)
		files[sidecarPath] = index
		state.SidecarPath, state.EntryCount = &sidecarPath, &indexed
	case KindTagDictionary:
		path := filepath.Join(staging, "dictionary.json")
		out, err := downloadAndDecompressToFileWithLimit(ctx, client, src.DownloadURL, src.Compression, path, maxTagDictBytes, nil)
		if err != nil {
			return false, err
		}
		m.setSyncPhase(runID, "indexing")
		dict, err = LoadTagDictionaryFile(path)
		if err != nil {
			return false, err
		}
		count := int64(dict.Len())
		state.EntryCount, state.FileSize, state.LocalSHA256 = &count, &out.DecompressedBytes, &out.SHA256
		files[m.dictionaryPath(src.ID)] = path
	case KindMappingPack:
		mappings, err = m.downloadMappings(ctx, client, src, staging)
		if err != nil {
			return false, err
		}
		m.setSyncPhase(runID, "indexing")
		ndjson, err := SanitizeAndExportMappings(mappings, ExportMappingsOptions{IncludeNSFW: true})
		if err != nil {
			return false, err
		}
		if int64(len(ndjson)) > maxMappingsStreamBytes {
			return false, errors.New("combined mapping packs exceed the size limit")
		}
		mappings, err = ParseMappingsNDJSON(bytes.NewReader(ndjson))
		if err != nil {
			return false, err
		}
		count, size := int64(len(mappings)), int64(len(ndjson))
		state.EntryCount, state.FileSize = &count, &size
		path := filepath.Join(staging, "mappings.ndjson")
		if err := os.WriteFile(path, ndjson, 0o600); err != nil {
			return false, err
		}
		files[m.mappingPath(src.ID)] = path
	default:
		return false, ErrInvalidConfig
	}
	m.setSyncPhase(runID, "syncing")
	return false, m.publishSync(ctx, src, staging, state, files, dict, mappings)
}

func (m *Manager) publishSync(ctx context.Context, src database.MetadataSource, staging string, state database.MetadataSourceSyncState, files map[string]string, dict *TagDictionary, mappings []MappingRecord) error {
	previous, err := m.store.GetMetadataSource(ctx, src.ID)
	if err != nil {
		return err
	}
	return m.publishSource(ctx, src, previous, staging, files, func() error {
		if src.Kind == KindOfflineSQLite {
			setOption(&src.Enabled, state.Enabled)
			setOption(&src.Ready, state.Ready)
			setOption(&src.EntryCount, state.EntryCount)
			setOption(&src.SidecarPath, state.SidecarPath)
			setOption(&src.FileSize, state.FileSize)
			setOption(&src.LocalSHA256, state.LocalSHA256)
			setOption(&src.RemoteETag, state.RemoteETag)
			setOption(&src.RemoteLastMod, state.RemoteLastMod)
			setOption(&src.RemoteUpdatedAt, state.RemoteUpdatedAt)
			setOption(&src.LastUpdatedAt, state.LastUpdatedAt)
			_, err := m.store.UpsertMetadataSource(ctx, src)
			return err
		}
		var mappingSources []database.MetadataSource
		if src.Kind == KindMappingPack {
			mappingSources, err = m.store.ListMetadataSources(ctx, KindMappingPack)
			if err != nil {
				return err
			}
		}
		saved, err := m.store.UpdateMetadataSourceSyncState(ctx, src.ID, state)
		if err != nil {
			return err
		}
		switch src.Kind {
		case KindTagDictionary:
			m.dictionaries[src.ID] = dict
		case KindMappingPack:
			m.mappings[src.ID] = mappings
			for i := range mappingSources {
				if mappingSources[i].ID == src.ID {
					mappingSources[i] = saved
				}
			}
			m.rebuildMappings(mappingSources)
		}
		return nil
	})
}

func (m *Manager) publishSource(ctx context.Context, src, previous database.MetadataSource, staging string, files map[string]string, commit func() error) (err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.closed {
		return ErrUnavailable
	}
	if src.Kind == KindOfflineSQLite {
		m.unloadSource(src.ID)
	}
	var undo []func() error
	defer func() {
		if err == nil {
			return
		}
		if src.Kind == KindOfflineSQLite {
			m.unloadSource(src.ID)
		}
		for i := len(undo) - 1; i >= 0; i-- {
			err = errors.Join(err, undo[i]())
		}
		if previous.Kind == KindOfflineSQLite && previous.Ready {
			err = errors.Join(err, m.loadSource(previous))
		}
	}()
	for dest, file := range files {
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		backup := filepath.Join(staging, filepath.Base(dest)+".backup")
		if err := os.Rename(dest, backup); err == nil {
			undo = append(undo, func() error { return os.Rename(backup, dest) })
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(file, dest); err != nil {
			return err
		}
		undo = append(undo, func() error { return os.Remove(dest) })
	}
	if src.Kind == KindOfflineSQLite {
		if err := m.loadSource(src); err != nil {
			return err
		}
	}
	if err := commit(); err != nil {
		return err
	}
	m.invalidateFallbackCoverCacheLocked()
	return nil
}

func (m *Manager) syncBangumi(ctx context.Context, client *http.Client, src database.MetadataSource, force, autoEnable bool, runID string) (bool, error) {
	if m.bangumi == nil {
		return false, ErrUnavailable
	}
	source, custom := bangumi.ArchiveSourceCustom, src.ManifestURL
	switch src.ManifestURL {
	case "", bangumi.ArchiveSourceZH, bangumi.ChineseArchiveMetaURL:
		source, custom = bangumi.ArchiveSourceZH, ""
	case bangumi.ArchiveSourceOfficial, bangumi.OfficialArchiveMetaURL:
		source, custom = bangumi.ArchiveSourceOfficial, ""
	}
	// All subscriptions using Bangumi share a single archive.
	st, err := m.bangumi.UpdateArchiveFromSource(ctx, source, custom, force, client)
	if err != nil {
		return false, err
	}
	m.setSyncPhase(runID, "syncing")
	skipped := st.LastCheckResult == "up_to_date"
	state := database.MetadataSourceSyncState{
		Ready: &st.Ready, EntryCount: &st.SubjectCount, LastUpdatedAt: &st.ImportedAt,
	}
	if autoEnable && !skipped {
		enabled := true
		state.Enabled = &enabled
	}
	_, err = m.store.UpdateMetadataSourceSyncState(ctx, src.ID, state)
	if err == nil && !skipped {
		m.InvalidateFallbackCoverCache()
	}
	return skipped, err
}

func (m *Manager) downloadMappings(ctx context.Context, client *http.Client, src database.MetadataSource, staging string) ([]MappingRecord, error) {
	urls := []string{src.DownloadURL}
	if src.ManifestURL != "" {
		var index struct {
			Packs []json.RawMessage `json:"packs"`
		}
		resp, err := fetchMetadataJSON(ctx, client, src.ManifestURL, MaxRepoManifestBytes, &index)
		if err != nil {
			return nil, err
		}
		if len(index.Packs) > 128 {
			return nil, ErrInvalidConfig
		}
		urls = nil
		for _, raw := range index.Packs {
			var path string
			if json.Unmarshal(raw, &path) != nil {
				var pack struct {
					URL         string `json:"url"`
					DownloadURL string `json:"downloadUrl"`
					Path        string `json:"path"`
				}
				if err := json.Unmarshal(raw, &pack); err != nil {
					return nil, err
				}
				path = firstNonEmpty(pack.DownloadURL, pack.URL, pack.Path)
			}
			ref, err := url.Parse(strings.TrimSpace(path))
			if err != nil || path == "" {
				return nil, ErrInvalidConfig
			}
			urls = append(urls, resp.ResolveReference(ref).String())
		}
	}
	var records []MappingRecord
	var size int64
	for _, packURL := range urls {
		path := filepath.Join(staging, "pack")
		out, err := downloadAndDecompressToFileWithLimit(ctx, client, packURL, src.Compression, path, maxMappingsStreamBytes, nil)
		if err != nil {
			return nil, err
		}
		size += out.DecompressedBytes
		if size > maxMappingsStreamBytes {
			return nil, errors.New("combined mapping packs exceed the size limit")
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		part, err := ParseMappingsNDJSON(file)
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		records = append(records, part...)
	}
	return records, nil
}

func fetchMetadataJSON(ctx context.Context, client *http.Client, rawURL string, limit int64, dst any) (*url.URL, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", repoUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPStatusError{StatusCode: resp.StatusCode, Op: "fetch metadata JSON"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("metadata JSON exceeds size limit")
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return nil, err
	}
	if resp.Request != nil {
		return resp.Request.URL, nil
	}
	return req.URL, nil
}
