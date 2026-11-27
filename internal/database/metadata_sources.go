package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const (
	defaultRepoSchemaVersion = "mangaalpha-repo-v1"
	defaultTagDisplayMode    = "zh"
	defaultNSFWRule          = "non_h_safe"
	defaultAutoUpdate        = "manual"
)

// MetadataRepo describes a declarative external metadata repository persisted in metadata_repos.
type MetadataRepo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	URL           string `json:"url"`
	SchemaVersion string `json:"schemaVersion"`
	RepoVersion   string `json:"repoVersion"`
	Description   string `json:"description"`
	Homepage      string `json:"homepage"`
	RawJSON       string `json:"rawJson,omitempty"`
	Enabled       bool   `json:"enabled"`
	LastSyncedAt  string `json:"lastSyncedAt"`
	LastError     string `json:"lastError"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

// MetadataSource describes an external metadata source, tag dictionary, or mapping pack in metadata_sources.
type MetadataSource struct {
	ID              string `json:"id"`
	RepoID          string `json:"repoId"`
	SourceKey       string `json:"sourceKey"`
	Kind            string `json:"kind"`
	SchemaType      string `json:"schemaType"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	DownloadURL     string `json:"downloadUrl"`
	ManifestURL     string `json:"manifestUrl"`
	ReleaseAPIURL   string `json:"releaseApiUrl"`
	AssetName       string `json:"assetName"`
	Compression     string `json:"compression"`
	LocalPath       string `json:"localPath"`
	SidecarPath     string `json:"sidecarPath"`
	TagDictSourceID string `json:"tagDictSourceId"`
	TagDisplayMode  string `json:"tagDisplayMode"`
	NSFWRule        string `json:"nsfwRule"`
	AutoUpdate      string `json:"autoUpdate"`
	OnlineAPIURL    string `json:"onlineApiUrl"`
	OnlineEnabled   bool   `json:"onlineEnabled"`
	ImportCovers    bool   `json:"importCovers"`
	Enabled         bool   `json:"enabled"`
	Ready           bool   `json:"ready"`
	EntryCount      int64  `json:"entryCount"`
	FileSize        int64  `json:"fileSize"`
	RemoteETag      string `json:"remoteEtag"`
	RemoteLastMod   string `json:"remoteLastMod"`
	RemoteUpdatedAt string `json:"remoteUpdatedAt"`
	LocalSHA256     string `json:"localSha256"`
	LastCheckedAt   string `json:"lastCheckedAt"`
	LastUpdatedAt   string `json:"lastUpdatedAt"`
	LastError       string `json:"lastError"`
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
}

// MetadataSourceSyncState contains optional patch fields updated during metadata source sync/build.
type MetadataSourceSyncState struct {
	LocalPath       *string `json:"localPath,omitempty"`
	SidecarPath     *string `json:"sidecarPath,omitempty"`
	Ready           *bool   `json:"ready,omitempty"`
	Enabled         *bool   `json:"enabled,omitempty"`
	EntryCount      *int64  `json:"entryCount,omitempty"`
	FileSize        *int64  `json:"fileSize,omitempty"`
	RemoteETag      *string `json:"remoteEtag,omitempty"`
	RemoteLastMod   *string `json:"remoteLastMod,omitempty"`
	RemoteUpdatedAt *string `json:"remoteUpdatedAt,omitempty"`
	LocalSHA256     *string `json:"localSha256,omitempty"`
	LastCheckedAt   *string `json:"lastCheckedAt,omitempty"`
	LastUpdatedAt   *string `json:"lastUpdatedAt,omitempty"`
	LastError       *string `json:"lastError,omitempty"`
}

// LibraryMetadataSource represents a single priority entry in a library's metadata fallback chain.
type LibraryMetadataSource struct {
	LibraryID     string `json:"libraryId"`
	SourceID      string `json:"sourceId"`
	Priority      int    `json:"priority"`
	Enabled       bool   `json:"enabled"`
	SourceName    string `json:"sourceName,omitempty"`
	SourceKind    string `json:"sourceKind,omitempty"`
	SourceEnabled bool   `json:"sourceEnabled,omitempty"`
	SourceReady   bool   `json:"sourceReady,omitempty"`
}

// MappingExportFilter filters matched series returned for community mapping pack export.
type MappingExportFilter struct {
	LibraryIDs  []string `json:"libraryIds,omitempty"`
	Origins     []string `json:"origins,omitempty"`
	IncludeNSFW bool     `json:"includeNsfw,omitempty"`
	Limit       int      `json:"limit,omitempty"`
}

// MappingExportRow carries sanitized series binding fields for mapping pack export.
type MappingExportRow struct {
	SeriesID     string   `json:"seriesId"`
	LibraryID    string   `json:"libraryId"`
	FolderName   string   `json:"folderName"`
	Title        string   `json:"title"`
	Authors      []Author `json:"authors"`
	BgmSubjectID *int64   `json:"bgmSubjectId,omitempty"`
	ExtSourceID  string   `json:"extSourceId,omitempty"`
	ExtEntryID   string   `json:"extEntryId,omitempty"`
	IsNSFW       bool     `json:"isNsfw"`
	MatchOrigin  string   `json:"matchOrigin,omitempty"`
}

const metadataRepoColumns = `
id,name,url,schema_version,repo_version,description,homepage,raw_json,
enabled,last_synced_at,last_error,created_at,updated_at`

const metadataSourceColumns = `
id,repo_id,source_key,kind,schema_type,name,description,
download_url,manifest_url,release_api_url,asset_name,compression,
local_path,sidecar_path,tag_dict_source_id,tag_display_mode,nsfw_rule,auto_update,
online_api_url,online_enabled,enabled,ready,entry_count,file_size,
remote_etag,remote_last_mod,remote_updated_at,local_sha256,
last_checked_at,last_updated_at,last_error,created_at,updated_at,import_covers`

func scanMetadataRepoRow(scanner interface{ Scan(...any) error }) (MetadataRepo, error) {
	var (
		item       MetadataRepo
		enabledInt int
	)
	if err := scanner.Scan(
		&item.ID, &item.Name, &item.URL, &item.SchemaVersion, &item.RepoVersion,
		&item.Description, &item.Homepage, &item.RawJSON,
		&enabledInt, &item.LastSyncedAt, &item.LastError, &item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return MetadataRepo{}, err
	}
	item.Enabled = enabledInt != 0
	return item, nil
}

func scanMetadataSourceRow(scanner interface{ Scan(...any) error }) (MetadataSource, error) {
	var (
		item             MetadataSource
		onlineEnabledInt int
		enabledInt       int
		readyInt         int
	)
	if err := scanner.Scan(
		&item.ID, &item.RepoID, &item.SourceKey, &item.Kind, &item.SchemaType, &item.Name, &item.Description,
		&item.DownloadURL, &item.ManifestURL, &item.ReleaseAPIURL, &item.AssetName, &item.Compression,
		&item.LocalPath, &item.SidecarPath, &item.TagDictSourceID, &item.TagDisplayMode, &item.NSFWRule, &item.AutoUpdate,
		&item.OnlineAPIURL, &onlineEnabledInt, &enabledInt, &readyInt, &item.EntryCount, &item.FileSize,
		&item.RemoteETag, &item.RemoteLastMod, &item.RemoteUpdatedAt, &item.LocalSHA256,
		&item.LastCheckedAt, &item.LastUpdatedAt, &item.LastError, &item.CreatedAt, &item.UpdatedAt, &item.ImportCovers,
	); err != nil {
		return MetadataSource{}, err
	}
	item.OnlineEnabled = onlineEnabledInt != 0
	item.Enabled = enabledInt != 0
	item.Ready = readyInt != 0
	return item, nil
}

// ListMetadataRepos returns all configured metadata repositories ordered by name and ID.
func (s *Store) ListMetadataRepos(ctx context.Context) ([]MetadataRepo, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT `+metadataRepoColumns+`
FROM metadata_repos
ORDER BY name COLLATE NOCASE ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MetadataRepo, 0)
	for rows.Next() {
		item, err := scanMetadataRepoRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// GetMetadataRepo returns a single metadata repository by ID.
func (s *Store) GetMetadataRepo(ctx context.Context, id string) (MetadataRepo, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT `+metadataRepoColumns+`
FROM metadata_repos WHERE id=?`, strings.TrimSpace(id))
	return scanMetadataRepoRow(row)
}

// UpsertMetadataRepo creates or updates a metadata repository record.
func (s *Store) UpsertMetadataRepo(ctx context.Context, repo MetadataRepo) (MetadataRepo, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MetadataRepo{}, err
	}
	defer tx.Rollback()
	saved, err := upsertMetadataRepo(ctx, tx, repo)
	if err != nil {
		return MetadataRepo{}, err
	}
	return saved, tx.Commit()
}

func upsertMetadataRepo(ctx context.Context, tx *sql.Tx, repo MetadataRepo) (MetadataRepo, error) {
	url := strings.TrimSpace(repo.URL)
	if url == "" {
		return MetadataRepo{}, errors.New("metadata repo url is required")
	}
	name := strings.TrimSpace(repo.Name)
	if name == "" {
		name = url
	}
	schemaVersion := strings.TrimSpace(repo.SchemaVersion)
	if schemaVersion == "" {
		schemaVersion = defaultRepoSchemaVersion
	}

	now := nowText()
	createdAt := strings.TrimSpace(repo.CreatedAt)
	if createdAt == "" {
		createdAt = now
	}

	id := strings.TrimSpace(repo.ID)
	if id == "" {
		var existingID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM metadata_repos WHERE url=?`, url).Scan(&existingID)
		if err == nil && existingID != "" {
			id = existingID
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return MetadataRepo{}, err
		} else {
			gen, gErr := randomHexID(12)
			if gErr != nil {
				return MetadataRepo{}, gErr
			}
			id = "mrepo_" + gen
		}
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO metadata_repos(
 `+metadataRepoColumns+`
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
 name=excluded.name,
 url=excluded.url,
 schema_version=excluded.schema_version,
 repo_version=excluded.repo_version,
 description=excluded.description,
 homepage=excluded.homepage,
 raw_json=excluded.raw_json,
 enabled=excluded.enabled,
 last_synced_at=excluded.last_synced_at,
 last_error=excluded.last_error,
 updated_at=excluded.updated_at`,
		id, name, url, schemaVersion,
		strings.TrimSpace(repo.RepoVersion), strings.TrimSpace(repo.Description),
		strings.TrimSpace(repo.Homepage), repo.RawJSON,
		boolInt(repo.Enabled), strings.TrimSpace(repo.LastSyncedAt),
		strings.TrimSpace(repo.LastError), createdAt, now,
	); err != nil {
		return MetadataRepo{}, err
	}

	return scanMetadataRepoRow(tx.QueryRowContext(ctx, `
SELECT `+metadataRepoColumns+` FROM metadata_repos WHERE id=?`, id))
}

// MetadataRepoSourceUpdate carries source metadata and an optional manifest-local
// dictionary key. The key can refer to a dictionary staged earlier in the refresh.
type MetadataRepoSourceUpdate struct {
	Source           MetadataSource
	TagDictionaryKey string
}

// UpsertMetadataRepoWithSources commits a repository refresh as one transaction.
func (s *Store) UpsertMetadataRepoWithSources(ctx context.Context, repo MetadataRepo, sources []MetadataRepoSourceUpdate) (MetadataRepo, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MetadataRepo{}, err
	}
	defer tx.Rollback()
	repo, err = upsertMetadataRepo(ctx, tx, repo)
	if err != nil {
		return MetadataRepo{}, err
	}
	for _, item := range sources {
		src := item.Source
		src.RepoID = repo.ID
		if item.TagDictionaryKey != "" {
			err := tx.QueryRowContext(ctx, `
SELECT id FROM metadata_sources WHERE repo_id=? AND source_key=? AND kind='tag_dictionary'`,
				repo.ID, item.TagDictionaryKey).Scan(&src.TagDictSourceID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return MetadataRepo{}, err
			}
		}
		if _, err := upsertMetadataSource(ctx, tx, src); err != nil {
			return MetadataRepo{}, err
		}
	}
	return repo, tx.Commit()
}

// DeleteMetadataRepo deletes a metadata repository and cascades deletion to its sources
// and any library_metadata_sources references inside a single transaction.
func (s *Store) DeleteMetadataRepo(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return sql.ErrNoRows
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	const repoSources = "IN (SELECT id FROM metadata_sources WHERE repo_id=?)"
	var hasSources bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM metadata_sources WHERE repo_id=?)", id).Scan(&hasSources); err != nil {
		return err
	}
	if hasSources {
		if err := markLibrariesUsingSourceChainChanged(ctx, tx, repoSources, id, "delete"); err != nil {
			return err
		}
		if err := clearMetadataSourceBindings(ctx, tx, repoSources, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM library_metadata_sources
WHERE source_id IN (SELECT id FROM metadata_sources WHERE repo_id=?)`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM metadata_sources WHERE repo_id=?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM metadata_repos WHERE id=?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// ListMetadataSources lists metadata sources, optionally filtered by kind.
func (s *Store) ListMetadataSources(ctx context.Context, kindFilter string) ([]MetadataSource, error) {
	kind := strings.TrimSpace(kindFilter)
	query := `SELECT ` + metadataSourceColumns + ` FROM metadata_sources`
	var args []any
	if kind != "" {
		query += ` WHERE kind=?`
		args = append(args, kind)
	}
	query += ` ORDER BY enabled DESC, kind ASC, name COLLATE NOCASE ASC, id ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MetadataSource, 0)
	for rows.Next() {
		item, err := scanMetadataSourceRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// GetMetadataSource returns a single metadata source by ID.
func (s *Store) GetMetadataSource(ctx context.Context, id string) (MetadataSource, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT `+metadataSourceColumns+`
FROM metadata_sources WHERE id=?`, strings.TrimSpace(id))
	return scanMetadataSourceRow(row)
}

// CreateMetadataSource stores a source configuration, allocating an ID when needed.
func (s *Store) CreateMetadataSource(ctx context.Context, src MetadataSource) (MetadataSource, error) {
	return s.UpsertMetadataSource(ctx, src)
}

// UpdateMetadataSource saves the complete source configuration under its existing ID.
func (s *Store) UpdateMetadataSource(ctx context.Context, src MetadataSource) (MetadataSource, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MetadataSource{}, err
	}
	defer tx.Rollback()
	src.ID = strings.TrimSpace(src.ID)
	var id string
	if err := tx.QueryRowContext(ctx, "SELECT id FROM metadata_sources WHERE id=?", src.ID).Scan(&id); err != nil {
		return MetadataSource{}, err
	}
	saved, err := upsertMetadataSource(ctx, tx, src)
	if err != nil {
		return MetadataSource{}, err
	}
	return saved, tx.Commit()
}

// UpsertMetadataSource creates or updates a metadata source record.
func (s *Store) UpsertMetadataSource(ctx context.Context, src MetadataSource) (MetadataSource, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MetadataSource{}, err
	}
	defer tx.Rollback()
	saved, err := upsertMetadataSource(ctx, tx, src)
	if err != nil {
		return MetadataSource{}, err
	}
	return saved, tx.Commit()
}

func upsertMetadataSource(ctx context.Context, tx *sql.Tx, src MetadataSource) (MetadataSource, error) {
	kind := strings.TrimSpace(src.Kind)
	if kind == "" {
		return MetadataSource{}, errors.New("metadata source kind is required")
	}
	name := strings.TrimSpace(src.Name)
	if name == "" {
		return MetadataSource{}, errors.New("metadata source name is required")
	}
	repoID := strings.TrimSpace(src.RepoID)
	sourceKey := strings.TrimSpace(src.SourceKey)

	tagDisplayMode, err := normalizeTagDisplayMode(src.TagDisplayMode)
	if err != nil {
		return MetadataSource{}, err
	}
	nsfwRule := strings.TrimSpace(src.NSFWRule)
	if nsfwRule == "" {
		nsfwRule = defaultNSFWRule
	}
	autoUpdate := strings.ToLower(strings.TrimSpace(src.AutoUpdate))
	if autoUpdate == "" {
		autoUpdate = defaultAutoUpdate
	}

	now := nowText()
	createdAt := strings.TrimSpace(src.CreatedAt)
	if createdAt == "" {
		createdAt = now
	}

	id := strings.TrimSpace(src.ID)
	if id == "" {
		if repoID != "" && sourceKey != "" {
			var existingID string
			qErr := tx.QueryRowContext(ctx,
				`SELECT id FROM metadata_sources WHERE repo_id=? AND source_key=?`, repoID, sourceKey,
			).Scan(&existingID)
			if qErr == nil && existingID != "" {
				id = existingID
			} else if qErr != nil && !errors.Is(qErr, sql.ErrNoRows) {
				return MetadataSource{}, qErr
			}
		}
		if id == "" {
			gen, gErr := randomHexID(12)
			if gErr != nil {
				return MetadataSource{}, gErr
			}
			id = "msrc_" + gen
		}
	}

	previous, err := metadataSourceChainVersion(ctx, tx, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return MetadataSource{}, err
	}
	missing := errors.Is(err, sql.ErrNoRows)

	if _, err := tx.ExecContext(ctx, `
INSERT INTO metadata_sources(
 `+metadataSourceColumns+`
) VALUES(
 ?,?,?,?,?,?,?,
 ?,?,?,?,?,
 ?,?,?,?,?,?,
 ?,?,?,?,?,?,
 ?,?,?,?,
 ?,?,?,?,?,?
)
ON CONFLICT(id) DO UPDATE SET
 repo_id=excluded.repo_id,
 source_key=excluded.source_key,
 kind=excluded.kind,
 schema_type=excluded.schema_type,
 name=excluded.name,
 description=excluded.description,
 download_url=excluded.download_url,
 manifest_url=excluded.manifest_url,
 release_api_url=excluded.release_api_url,
 asset_name=excluded.asset_name,
 compression=excluded.compression,
 local_path=excluded.local_path,
 sidecar_path=excluded.sidecar_path,
 tag_dict_source_id=excluded.tag_dict_source_id,
 tag_display_mode=excluded.tag_display_mode,
 nsfw_rule=excluded.nsfw_rule,
 auto_update=excluded.auto_update,
 online_api_url=excluded.online_api_url,
 online_enabled=excluded.online_enabled,
 enabled=excluded.enabled,
 import_covers=excluded.import_covers,
 ready=excluded.ready,
 entry_count=excluded.entry_count,
 file_size=excluded.file_size,
 remote_etag=excluded.remote_etag,
 remote_last_mod=excluded.remote_last_mod,
 remote_updated_at=excluded.remote_updated_at,
 local_sha256=excluded.local_sha256,
 last_checked_at=excluded.last_checked_at,
 last_updated_at=excluded.last_updated_at,
 last_error=excluded.last_error,
 updated_at=excluded.updated_at`,
		id, repoID, sourceKey, kind, strings.TrimSpace(src.SchemaType), name, strings.TrimSpace(src.Description),
		strings.TrimSpace(src.DownloadURL), strings.TrimSpace(src.ManifestURL), strings.TrimSpace(src.ReleaseAPIURL),
		strings.TrimSpace(src.AssetName), strings.ToLower(strings.TrimSpace(src.Compression)),
		strings.TrimSpace(src.LocalPath), strings.TrimSpace(src.SidecarPath),
		strings.TrimSpace(src.TagDictSourceID), tagDisplayMode, nsfwRule, autoUpdate,
		strings.TrimSpace(src.OnlineAPIURL), boolInt(src.OnlineEnabled), boolInt(src.Enabled), boolInt(src.Ready),
		src.EntryCount, src.FileSize,
		strings.TrimSpace(src.RemoteETag), strings.TrimSpace(src.RemoteLastMod),
		strings.TrimSpace(src.RemoteUpdatedAt), strings.TrimSpace(src.LocalSHA256),
		strings.TrimSpace(src.LastCheckedAt), strings.TrimSpace(src.LastUpdatedAt),
		strings.TrimSpace(src.LastError), createdAt, now, src.ImportCovers,
	); err != nil {
		return MetadataSource{}, err
	}

	if err := markMetadataSourceVersionChange(ctx, tx, id, previous, missing); err != nil {
		return MetadataSource{}, err
	}
	return scanMetadataSourceRow(tx.QueryRowContext(ctx, `
SELECT `+metadataSourceColumns+` FROM metadata_sources WHERE id=?`, id))
}

// UpdateMetadataSourceConfig updates user-configurable fields of an existing metadata source.
func (s *Store) UpdateMetadataSourceConfig(
	ctx context.Context,
	id string,
	enabled bool,
	tagDictSourceID, tagDisplayMode, autoUpdate, localPath, downloadURL, onlineAPIURL string,
	onlineEnabled bool,
) (MetadataSource, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return MetadataSource{}, sql.ErrNoRows
	}
	normMode, err := normalizeTagDisplayMode(tagDisplayMode)
	if err != nil {
		return MetadataSource{}, err
	}
	normAuto := strings.ToLower(strings.TrimSpace(autoUpdate))
	if normAuto == "" {
		normAuto = defaultAutoUpdate
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MetadataSource{}, err
	}
	defer tx.Rollback()
	previous, err := metadataSourceChainVersion(ctx, tx, id)
	if err != nil {
		return MetadataSource{}, err
	}
	res, err := tx.ExecContext(ctx, `
UPDATE metadata_sources
SET enabled=?,
    tag_dict_source_id=?,
    tag_display_mode=?,
    auto_update=?,
    local_path=?,
    download_url=?,
    online_api_url=?,
    online_enabled=?,
    updated_at=?
WHERE id=?`,
		boolInt(enabled),
		strings.TrimSpace(tagDictSourceID),
		normMode,
		normAuto,
		strings.TrimSpace(localPath),
		strings.TrimSpace(downloadURL),
		strings.TrimSpace(onlineAPIURL),
		boolInt(onlineEnabled),
		nowText(),
		id,
	)
	if err != nil {
		return MetadataSource{}, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return MetadataSource{}, sql.ErrNoRows
	}
	if err := markMetadataSourceVersionChange(ctx, tx, id, previous, false); err != nil {
		return MetadataSource{}, err
	}
	src, err := scanMetadataSourceRow(tx.QueryRowContext(ctx, "SELECT "+metadataSourceColumns+" FROM metadata_sources WHERE id=?", id))
	if err != nil {
		return MetadataSource{}, err
	}
	return src, tx.Commit()
}

// UpdateMetadataSourceSyncState patches synchronization/index status fields on an existing metadata source.
func (s *Store) UpdateMetadataSourceSyncState(
	ctx context.Context,
	id string,
	state MetadataSourceSyncState,
) (MetadataSource, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return MetadataSource{}, sql.ErrNoRows
	}

	sets := make([]string, 0, 13)
	args := make([]any, 0, 14)

	if state.LocalPath != nil {
		sets = append(sets, "local_path=?")
		args = append(args, strings.TrimSpace(*state.LocalPath))
	}
	if state.SidecarPath != nil {
		sets = append(sets, "sidecar_path=?")
		args = append(args, strings.TrimSpace(*state.SidecarPath))
	}
	if state.Ready != nil {
		sets = append(sets, "ready=?")
		args = append(args, boolInt(*state.Ready))
	}
	if state.Enabled != nil {
		sets = append(sets, "enabled=?")
		args = append(args, boolInt(*state.Enabled))
	}
	if state.EntryCount != nil {
		sets = append(sets, "entry_count=?")
		args = append(args, *state.EntryCount)
	}
	if state.FileSize != nil {
		sets = append(sets, "file_size=?")
		args = append(args, *state.FileSize)
	}
	if state.RemoteETag != nil {
		sets = append(sets, "remote_etag=?")
		args = append(args, strings.TrimSpace(*state.RemoteETag))
	}
	if state.RemoteLastMod != nil {
		sets = append(sets, "remote_last_mod=?")
		args = append(args, strings.TrimSpace(*state.RemoteLastMod))
	}
	if state.RemoteUpdatedAt != nil {
		sets = append(sets, "remote_updated_at=?")
		args = append(args, strings.TrimSpace(*state.RemoteUpdatedAt))
	}
	if state.LocalSHA256 != nil {
		sets = append(sets, "local_sha256=?")
		args = append(args, strings.TrimSpace(*state.LocalSHA256))
	}
	if state.LastCheckedAt != nil {
		sets = append(sets, "last_checked_at=?")
		args = append(args, strings.TrimSpace(*state.LastCheckedAt))
	}
	if state.LastUpdatedAt != nil {
		sets = append(sets, "last_updated_at=?")
		args = append(args, strings.TrimSpace(*state.LastUpdatedAt))
	}
	if state.LastError != nil {
		sets = append(sets, "last_error=?")
		args = append(args, strings.TrimSpace(*state.LastError))
	}
	sets = append(sets, "updated_at=?")
	args = append(args, nowText(), id)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MetadataSource{}, err
	}
	defer tx.Rollback()
	previous, err := metadataSourceChainVersion(ctx, tx, id)
	if err != nil {
		return MetadataSource{}, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE metadata_sources SET `+strings.Join(sets, ",")+` WHERE id=?`, args...)
	if err != nil {
		return MetadataSource{}, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return MetadataSource{}, sql.ErrNoRows
	}
	if err := markMetadataSourceVersionChange(ctx, tx, id, previous, false); err != nil {
		return MetadataSource{}, err
	}
	src, err := scanMetadataSourceRow(tx.QueryRowContext(ctx, "SELECT "+metadataSourceColumns+" FROM metadata_sources WHERE id=?", id))
	if err != nil {
		return MetadataSource{}, err
	}
	return src, tx.Commit()
}

// DeleteMetadataSource removes a metadata source and cleans up library_metadata_sources references atomically.
func (s *Store) DeleteMetadataSource(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return sql.ErrNoRows
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := markLibrariesUsingSourceChainChanged(ctx, tx, "= ?", id, "delete"); err != nil {
		return err
	}
	if err := clearMetadataSourceBindings(ctx, tx, "= ?", id); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM library_metadata_sources WHERE source_id=?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM metadata_sources WHERE id=?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// ListLibraryMetadataSources returns the ordered metadata fallback chain configured for a library.
func (s *Store) ListLibraryMetadataSources(ctx context.Context, libraryID string) ([]LibraryMetadataSource, error) {
	libraryID = strings.TrimSpace(libraryID)
	rows, err := s.db.QueryContext(ctx, `
SELECT lms.library_id, lms.source_id, lms.priority, lms.enabled,
       coalesce(ms.name, ''), coalesce(ms.kind, ''), coalesce(ms.enabled, 0), coalesce(ms.ready, 0)
FROM library_metadata_sources lms
LEFT JOIN metadata_sources ms ON ms.id=lms.source_id
WHERE lms.library_id=?
ORDER BY lms.priority ASC, lms.source_id ASC`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LibraryMetadataSource, 0)
	for rows.Next() {
		item, err := scanLibraryMetadataSourceRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// SetLibraryMetadataSources atomically replaces a library's metadata fallback chain,
// deduplicating sourceIDs while preserving order and assigning priorities 1..N.
func (s *Store) SetLibraryMetadataSources(ctx context.Context, libraryID string, sourceIDs []string) error {
	libraryID = strings.TrimSpace(libraryID)
	if libraryID == "" {
		return errors.New("libraryId is required")
	}
	cleanIDs := cleanStringSliceCaseSensitive(sourceIDs)
	cleanIDsJSON, _ := json.Marshal(cleanIDs)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var unchanged bool
	if err := tx.QueryRowContext(ctx, `
SELECT NOT EXISTS (
 SELECT source_id,priority,enabled FROM library_metadata_sources WHERE library_id=?
 EXCEPT SELECT value,key+1,1 FROM json_each(?)
) AND NOT EXISTS (
 SELECT value,key+1,1 FROM json_each(?)
 EXCEPT SELECT source_id,priority,enabled FROM library_metadata_sources WHERE library_id=?
)`, libraryID, cleanIDsJSON, cleanIDsJSON, libraryID).Scan(&unchanged); err != nil {
		return err
	}
	if unchanged {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM library_metadata_sources WHERE library_id=?`, libraryID); err != nil {
		return err
	}
	if len(cleanIDs) > 0 {
		stmt, err := tx.PrepareContext(ctx, `
INSERT INTO library_metadata_sources(library_id,source_id,priority,enabled)
VALUES(?,?,?,1)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for idx, sourceID := range cleanIDs {
			if _, err := stmt.ExecContext(ctx, libraryID, sourceID, idx+1); err != nil {
				return err
			}
		}
	}
	version, err := randomHexID(16)
	if err != nil {
		return err
	}
	if err := markLibraryMetadataChainChanged(ctx, tx, libraryID, version); err != nil {
		return err
	}
	return tx.Commit()
}

// ListAllLibraryMetadataChains loads all per-library metadata fallback chains in a single query.
func (s *Store) ListAllLibraryMetadataChains(ctx context.Context) (map[string][]LibraryMetadataSource, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT lms.library_id, lms.source_id, lms.priority, lms.enabled,
       coalesce(ms.name, ''), coalesce(ms.kind, ''), coalesce(ms.enabled, 0), coalesce(ms.ready, 0)
FROM library_metadata_sources lms
LEFT JOIN metadata_sources ms ON ms.id=lms.source_id
ORDER BY lms.library_id ASC, lms.priority ASC, lms.source_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]LibraryMetadataSource)
	for rows.Next() {
		item, err := scanLibraryMetadataSourceRow(rows)
		if err != nil {
			return nil, err
		}
		out[item.LibraryID] = append(out[item.LibraryID], item)
	}
	return out, rows.Err()
}

func scanLibraryMetadataSourceRow(scanner interface{ Scan(...any) error }) (LibraryMetadataSource, error) {
	var (
		item             LibraryMetadataSource
		enabledInt       int
		sourceEnabledInt int
		sourceReadyInt   int
	)
	if err := scanner.Scan(
		&item.LibraryID, &item.SourceID, &item.Priority, &enabledInt,
		&item.SourceName, &item.SourceKind, &sourceEnabledInt, &sourceReadyInt,
	); err != nil {
		return LibraryMetadataSource{}, err
	}
	item.Enabled = enabledInt != 0
	item.SourceEnabled = sourceEnabledInt != 0
	item.SourceReady = sourceReadyInt != 0
	return item, nil
}

// ListSeriesForMappingExport queries matched series rows for community mapping pack export.
// By default (IncludeNSFW=false), NSFW entries are strictly excluded.
func (s *Store) ListSeriesForMappingExport(ctx context.Context, filter MappingExportFilter) ([]MappingExportRow, error) {
	includeNSFW := filter.IncludeNSFW
	libIDs := cleanStringSliceCaseSensitive(filter.LibraryIDs)
	defaultLibraries := len(libIDs) == 0

	if scope, hasScope := UserScopeFromContext(ctx); hasScope && !scope.IsAdmin {
		if !scope.EffectiveShowNSFW() {
			includeNSFW = false
		}
		if !scope.AllLibraries {
			scopedLibs, empty := intersectLibraryIDs(libIDs, scope.AllowedLibraryIDs)
			if empty {
				return []MappingExportRow{}, nil
			}
			libIDs = scopedLibs
		}
	}

	clauses := []string{
		`((sm.bgm_subject_id IS NOT NULL AND sm.bgm_subject_id > 0) OR (sm.ext_source_id <> '' AND sm.ext_entry_id <> ''))`,
	}
	var args []any

	if !includeNSFW {
		clauses = append(clauses, `sm.is_nsfw = 0`)
	}
	if defaultLibraries {
		clauses = append(clauses, `l.enabled=1 AND NOT EXISTS (SELECT 1 FROM union_layers ul WHERE ul.member_library_id=l.id)`)
	}
	if len(libIDs) > 0 {
		clauses = append(clauses, `s.library_id IN `+Placeholders(len(libIDs)))
		for _, id := range libIDs {
			args = append(args, id)
		}
	}
	origins := cleanStringSlice(filter.Origins)
	if len(origins) > 0 {
		clauses = append(clauses, `lower(sm.match_origin) IN `+Placeholders(len(origins)))
		for _, orig := range origins {
			args = append(args, strings.ToLower(orig))
		}
	}

	query := `
SELECT s.id, s.library_id, s.name, COALESCE(NULLIF(sm.title,''), s.name),
       sm.authors_json, sm.bgm_subject_id, sm.ext_source_id, sm.ext_entry_id,
       sm.is_nsfw, sm.match_origin
FROM series s
JOIN libraries l ON l.id=s.library_id
JOIN series_metadata sm ON sm.series_id=s.id
WHERE ` + strings.Join(clauses, " AND ") + `
ORDER BY s.library_id ASC, s.name COLLATE NOCASE ASC, s.id ASC`

	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]MappingExportRow, 0)
	for rows.Next() {
		var (
			row         MappingExportRow
			authorsJSON string
			bgmID       sql.NullInt64
			isNSFWInt   int
		)
		if err := rows.Scan(
			&row.SeriesID, &row.LibraryID, &row.FolderName, &row.Title,
			&authorsJSON, &bgmID, &row.ExtSourceID, &row.ExtEntryID,
			&isNSFWInt, &row.MatchOrigin,
		); err != nil {
			return nil, err
		}
		if bgmID.Valid && bgmID.Int64 > 0 {
			v := bgmID.Int64
			row.BgmSubjectID = &v
		}
		row.IsNSFW = isNSFWInt != 0
		_ = json.Unmarshal([]byte(authorsJSON), &row.Authors)
		if row.Authors == nil {
			row.Authors = []Author{}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func normalizeTagDisplayMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		return defaultTagDisplayMode, nil
	}
	switch mode {
	case "zh", "bilingual", "raw":
		return mode, nil
	default:
		return "", errors.New("tagDisplayMode must be 'zh', 'bilingual', or 'raw'")
	}
}
