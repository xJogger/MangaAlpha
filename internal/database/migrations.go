package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const createSeriesMetadataExtIndex = `CREATE INDEX IF NOT EXISTS idx_series_metadata_ext ON series_metadata(ext_source_id, ext_entry_id) WHERE ext_source_id <> ''`

const userHiddenLibrariesSchema = `
CREATE TABLE IF NOT EXISTS user_hidden_libraries (
  user_id TEXT NOT NULL,
  library_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(user_id, library_id),
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
  FOREIGN KEY(library_id) REFERENCES libraries(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_user_hidden_libraries_lib ON user_hidden_libraries(library_id);
`

func ensureUserHiddenLibraries(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(userHiddenLibrariesSchema); err != nil {
		return err
	}
	return tx.Commit()
}

func applyMigrations(db *sql.DB) error {
	if err := ensureClientSettingsTable(db); err != nil {
		return err
	}
	if err := ensureAuthTables(db); err != nil {
		return err
	}
	if err := ensureStorageProviderTables(db); err != nil {
		return err
	}
	if err := ensureProgressTables(db); err != nil {
		return err
	}
	if err := ensureOperationStats(db); err != nil {
		return err
	}
	if err := ensureMaintenanceTables(db); err != nil {
		return err
	}
	if err := ensureMetadataAndScrapeTables(db); err != nil {
		return err
	}
	if err := ensureOrganizationTables(db); err != nil {
		return err
	}
	if err := ensureUnionAndScheduleTables(db); err != nil {
		return err
	}
	if err := ensureBangumiTrackerTable(db); err != nil {
		return err
	}
	if err := ensureArchivePasswordTables(db); err != nil {
		return err
	}
	if err := ensureExternalMetadataTables(db); err != nil {
		return err
	}
	if err := ensureMultiUserMigration(db); err != nil {
		return err
	}
	if err := ensureUserHiddenLibraries(db); err != nil {
		return err
	}
	migrations := []struct {
		table      string
		column     string
		definition string
	}{
		{"libraries", "one_shot", "INTEGER NOT NULL DEFAULT 0"},
		{"libraries", "provider_type", "TEXT NOT NULL DEFAULT '115'"},
		{"libraries", "webdav_account_id", "TEXT NOT NULL DEFAULT ''"},
		{"libraries", "root_path", "TEXT NOT NULL DEFAULT ''"},
		{"series", "one_shot", "INTEGER NOT NULL DEFAULT 0"},
		{"series", "books_count", "INTEGER NOT NULL DEFAULT 0"},
		{"series", "latest_book_modified_at", "TEXT"},
		{"series", "latest_book_created_at", "TEXT"},
		{"series", "sort_modified_at", "TEXT NOT NULL DEFAULT ''"},
		{"series", "sort_created_at", "TEXT NOT NULL DEFAULT ''"},
		{"series_metadata", "is_nsfw", "INTEGER NOT NULL DEFAULT 0"},
		{"series_metadata", "bgm_rank", "INTEGER"},
		{"series_metadata", "nsfw_lock", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, migration := range migrations {
		if err := addColumnIfMissing(db, migration.table, migration.column, migration.definition); err != nil {
			return err
		}
	}
	if err := ensureSeriesMetadataExtColumns(db); err != nil {
		return err
	}
	if err := backfillLegacyMatchOrigin(db); err != nil {
		return err
	}
	if err := ensurePerformanceIndexes(db); err != nil {
		return err
	}
	if err := ensureShelfRevisionTable(db); err != nil {
		return err
	}
	if err := ensureSeriesRollups(db); err != nil {
		return err
	}
	if err := ensureSearchFTSTables(db); err != nil {
		return err
	}
	return createShelfRevisions(db)
}

func ensureAuthTables(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS auth_admin (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  username TEXT NOT NULL,
  password_hash TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS auth_sessions (
  token_hash TEXT PRIMARY KEY,
  username TEXT NOT NULL,
  remember INTEGER NOT NULL DEFAULT 0,
  user_agent TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_auth_sessions_expires ON auth_sessions(expires_at);

CREATE TABLE IF NOT EXISTS auth_api_keys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  key_prefix TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  last_used_at TEXT
);
`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
UPDATE auth_sessions SET remember=1,expires_at=? WHERE expires_at>? AND expires_at<>?`,
		PermanentSessionExpiresAt, nowText(), PermanentSessionExpiresAt)
	return err
}

func ensureStorageProviderTables(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS webdav_accounts (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  base_url TEXT NOT NULL,
  username TEXT NOT NULL DEFAULT '',
  password TEXT NOT NULL DEFAULT '',
  insecure_tls INTEGER NOT NULL DEFAULT 0,
  rate_per_second REAL NOT NULL DEFAULT 8,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
`)
	return err
}

func ensurePerformanceIndexes(db *sql.DB) error {
	if exists, err := tableExists(db, "series"); err != nil {
		return err
	} else if exists {
		for _, stmt := range []string{
			`CREATE INDEX IF NOT EXISTS idx_series_lib_sort_mod ON series(library_id, sort_modified_at DESC, name COLLATE NOCASE, id)`,
			`CREATE INDEX IF NOT EXISTS idx_series_lib_sort_cre ON series(library_id, sort_created_at DESC, name COLLATE NOCASE, id)`,
			`CREATE INDEX IF NOT EXISTS idx_series_sort_mod ON series(sort_modified_at DESC, name COLLATE NOCASE, id, library_id)`,
			`CREATE INDEX IF NOT EXISTS idx_series_sort_cre ON series(sort_created_at DESC, name COLLATE NOCASE, id, library_id)`,
		} {
			if _, err := db.Exec(stmt); err != nil {
				return err
			}
		}
	}
	if exists, err := tableExists(db, "books"); err != nil {
		return err
	} else if exists {
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_books_library_size ON books(library_id, size)`); err != nil {
			return err
		}
	}
	if exists, err := tableExists(db, "series_thumbnails"); err != nil {
		return err
	} else if exists {
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_series_thumbnails_source_book ON series_thumbnails(source_book_id)`); err != nil {
			return err
		}
	}
	if exists, err := tableExists(db, "book_read_progress"); err != nil {
		return err
	} else if exists {
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_book_read_progress_updated ON book_read_progress(user_id, completed, updated_at DESC)`); err != nil {
			return err
		}
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_book_read_progress_updated_all ON book_read_progress(user_id, updated_at DESC, book_id)`); err != nil {
			return err
		}
	}
	if exists, err := tableExists(db, "series_metadata"); err != nil {
		return err
	} else if exists {
		for _, stmt := range []string{
			`CREATE INDEX IF NOT EXISTS idx_series_metadata_nsfw ON series_metadata(is_nsfw)`,
			`CREATE INDEX IF NOT EXISTS idx_series_metadata_nsfw_series ON series_metadata(is_nsfw, series_id)`,
			`CREATE INDEX IF NOT EXISTS idx_series_metadata_fast ON series_metadata(series_id, is_nsfw, scrape_status, bgm_subject_id, bgm_rank, bgm_score)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_series_metadata_search ON series_metadata(series_id, title, title_sort, alternate_titles_json)`,
			`CREATE INDEX IF NOT EXISTS idx_series_metadata_bgm_rank ON series_metadata(bgm_rank ASC, series_id, is_nsfw) WHERE bgm_rank > 0`,
			`CREATE INDEX IF NOT EXISTS idx_series_metadata_bgm_sub ON series_metadata(bgm_subject_id, series_id, bgm_rank, bgm_score) WHERE bgm_subject_id > 0`,
			`CREATE INDEX IF NOT EXISTS idx_series_metadata_pub_series ON series_metadata(publisher COLLATE NOCASE, series_id)`,
			createSeriesMetadataExtIndex,
		} {
			if _, err := db.Exec(stmt); err != nil {
				return err
			}
		}
	}
	return nil
}

func ensureOrganizationTables(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS book_read_progress_tombstones (
  book_id TEXT PRIMARY KEY,
  series_id TEXT NOT NULL,
  deleted_at TEXT NOT NULL,
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_progress_tombstones_series ON book_read_progress_tombstones(series_id);
CREATE INDEX IF NOT EXISTS idx_progress_tombstones_deleted ON book_read_progress_tombstones(deleted_at);

CREATE TABLE IF NOT EXISTS collections (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE COLLATE NOCASE,
  ordered INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS collection_series (
  collection_id TEXT NOT NULL,
  series_id TEXT NOT NULL,
  position INTEGER NOT NULL,
  PRIMARY KEY(collection_id, series_id),
  FOREIGN KEY(collection_id) REFERENCES collections(id) ON DELETE CASCADE,
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_collection_series_pos ON collection_series(collection_id, position);
CREATE INDEX IF NOT EXISTS idx_collection_series_series ON collection_series(series_id, collection_id);

CREATE TABLE IF NOT EXISTS readlists (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE COLLATE NOCASE,
  summary TEXT NOT NULL DEFAULT '',
  ordered INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS readlist_books (
  readlist_id TEXT NOT NULL,
  book_id TEXT NOT NULL,
  position INTEGER NOT NULL,
  PRIMARY KEY(readlist_id, book_id),
  FOREIGN KEY(readlist_id) REFERENCES readlists(id) ON DELETE CASCADE,
  FOREIGN KEY(book_id) REFERENCES books(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_readlist_books_pos ON readlist_books(readlist_id, position);
CREATE INDEX IF NOT EXISTS idx_readlist_books_book ON readlist_books(book_id, readlist_id);
`)
	return err
}

func ensureUnionAndScheduleTables(db *sql.DB) error {
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS union_layers (
  union_library_id TEXT NOT NULL,
  member_library_id TEXT NOT NULL UNIQUE,
  priority INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(union_library_id, member_library_id),
  FOREIGN KEY(union_library_id) REFERENCES libraries(id) ON DELETE CASCADE,
  FOREIGN KEY(member_library_id) REFERENCES libraries(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_union_layers_union_prio ON union_layers(union_library_id, priority);

CREATE TABLE IF NOT EXISTS union_series_sources (
  union_series_id TEXT NOT NULL,
  priority INTEGER NOT NULL,
  source_series_id TEXT NOT NULL,
  source_library_id TEXT NOT NULL,
  PRIMARY KEY(union_series_id, priority),
  FOREIGN KEY(union_series_id) REFERENCES series(id) ON DELETE CASCADE,
  FOREIGN KEY(source_series_id) REFERENCES series(id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_union_series_sources_src ON union_series_sources(source_series_id);

CREATE TABLE IF NOT EXISTS union_book_sources (
  union_book_id TEXT NOT NULL,
  priority INTEGER NOT NULL,
  source_book_id TEXT NOT NULL,
  source_library_id TEXT NOT NULL,
  source_size INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(union_book_id, priority),
  FOREIGN KEY(union_book_id) REFERENCES books(id) ON DELETE CASCADE,
  FOREIGN KEY(source_book_id) REFERENCES books(id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_union_book_sources_src ON union_book_sources(source_book_id);

CREATE TABLE IF NOT EXISTS library_schedules (
  library_id TEXT PRIMARY KEY,
  enabled INTEGER NOT NULL DEFAULT 0,
  frequency TEXT NOT NULL DEFAULT 'daily',
  weekday INTEGER NOT NULL DEFAULT 0,
  time_of_day TEXT NOT NULL DEFAULT '03:00',
  scan_member_ids_json TEXT NOT NULL DEFAULT '[]',
  auto_bangumi INTEGER NOT NULL DEFAULT 1,
  auto_cover_limit INTEGER NOT NULL DEFAULT 0,
  last_run_at TEXT,
  last_slot_key TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  FOREIGN KEY(library_id) REFERENCES libraries(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS pipeline_runs (
  id TEXT PRIMARY KEY,
  library_id TEXT NOT NULL,
  trigger_type TEXT NOT NULL DEFAULT 'manual',
  status TEXT NOT NULL,
  stage TEXT NOT NULL DEFAULT 'queued',
  scan_run_ids_json TEXT NOT NULL DEFAULT '[]',
  bangumi_run_id TEXT NOT NULL DEFAULT '',
  cover_run_id TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  cancel_requested INTEGER NOT NULL DEFAULT 0,
  started_at TEXT,
  completed_at TEXT,
  created_at TEXT NOT NULL,
  FOREIGN KEY(library_id) REFERENCES libraries(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_pipeline_runs_created ON pipeline_runs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_pipeline_runs_lib ON pipeline_runs(library_id, created_at DESC);
`); err != nil {
		return err
	}
	// Clean up removed legacy global auto-scan settings.
	_, _ = db.Exec(`DELETE FROM settings WHERE key IN ('auto_scan_enabled','auto_scan_interval_minutes','scan_on_startup')`)
	return nil
}

func ensureBangumiTrackerTable(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS bangumi_tracker_records (
  bgm_subject_id INTEGER PRIMARY KEY,
  series_id TEXT NOT NULL DEFAULT '',
  series_title TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  remote_type INTEGER NOT NULL DEFAULT 0,
  remote_vol_status INTEGER NOT NULL DEFAULT 0,
  remote_private INTEGER NOT NULL DEFAULT 0,
  last_local_vol INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  last_synced_at TEXT,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bgm_tracker_updated ON bangumi_tracker_records(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_bgm_tracker_series ON bangumi_tracker_records(series_id);
`)
	return err
}

func ensureArchivePasswordTables(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS archive_passwords (
  id TEXT PRIMARY KEY,
  password TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_archive_passwords_updated ON archive_passwords(updated_at DESC);

CREATE TABLE IF NOT EXISTS archive_password_bindings (
  book_id TEXT PRIMARY KEY,
  password_id TEXT NOT NULL,
  series_id TEXT NOT NULL DEFAULT '',
  library_id TEXT NOT NULL DEFAULT '',
  parent_cid TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(password_id) REFERENCES archive_passwords(id) ON DELETE CASCADE,
  FOREIGN KEY(book_id) REFERENCES books(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_apb_password ON archive_password_bindings(password_id);
CREATE INDEX IF NOT EXISTS idx_apb_series ON archive_password_bindings(series_id);
CREATE INDEX IF NOT EXISTS idx_apb_folder ON archive_password_bindings(library_id, parent_cid);
`)
	return err
}

func ensureExternalMetadataTables(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS metadata_repos (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  url TEXT NOT NULL UNIQUE,
  schema_version TEXT NOT NULL DEFAULT 'mangaalpha-repo-v1',
  repo_version TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  homepage TEXT NOT NULL DEFAULT '',
  raw_json TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  last_synced_at TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS metadata_sources (
  id TEXT PRIMARY KEY,
  repo_id TEXT NOT NULL DEFAULT '',
  source_key TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  schema_type TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  download_url TEXT NOT NULL DEFAULT '',
  manifest_url TEXT NOT NULL DEFAULT '',
  release_api_url TEXT NOT NULL DEFAULT '',
  asset_name TEXT NOT NULL DEFAULT '',
  compression TEXT NOT NULL DEFAULT '',
  local_path TEXT NOT NULL DEFAULT '',
  sidecar_path TEXT NOT NULL DEFAULT '',
  tag_dict_source_id TEXT NOT NULL DEFAULT '',
  tag_display_mode TEXT NOT NULL DEFAULT 'zh',
  nsfw_rule TEXT NOT NULL DEFAULT 'non_h_safe',
  auto_update TEXT NOT NULL DEFAULT 'manual',
  online_api_url TEXT NOT NULL DEFAULT '',
  online_enabled INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 0,
  ready INTEGER NOT NULL DEFAULT 0,
  entry_count INTEGER NOT NULL DEFAULT 0,
  file_size INTEGER NOT NULL DEFAULT 0,
  remote_etag TEXT NOT NULL DEFAULT '',
  remote_last_mod TEXT NOT NULL DEFAULT '',
  remote_updated_at TEXT NOT NULL DEFAULT '',
  local_sha256 TEXT NOT NULL DEFAULT '',
  last_checked_at TEXT NOT NULL DEFAULT '',
  last_updated_at TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_metadata_sources_repo ON metadata_sources(repo_id);
CREATE INDEX IF NOT EXISTS idx_metadata_sources_kind ON metadata_sources(kind);

CREATE TABLE IF NOT EXISTS library_metadata_sources (
  library_id TEXT NOT NULL,
  source_id TEXT NOT NULL,
  priority INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY(library_id, source_id)
);
CREATE INDEX IF NOT EXISTS idx_lib_meta_sources_prio ON library_metadata_sources(library_id, priority);
CREATE INDEX IF NOT EXISTS idx_lib_meta_sources_source ON library_metadata_sources(source_id);
`); err != nil {
		return err
	}
	return tx.Commit()
}

func ensureSeriesMetadataExtColumns(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	cols := []struct {
		column     string
		definition string
	}{
		{"ext_source_id", "TEXT NOT NULL DEFAULT ''"},
		{"ext_entry_id", "TEXT NOT NULL DEFAULT ''"},
		{"ext_cover_url", "TEXT NOT NULL DEFAULT ''"},
		{"ext_cover_path", "TEXT NOT NULL DEFAULT ''"},
		{"match_origin", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, col := range cols {
		if err := addColumnIfMissingTx(tx, "series_metadata", col.column, col.definition); err != nil {
			return err
		}
	}
	// Older databases have a full index under the same name.
	var partial bool
	switch err := tx.QueryRow(`SELECT "partial" FROM pragma_index_list('series_metadata') WHERE name='idx_series_metadata_ext'`).Scan(&partial); err {
	case nil:
		if !partial {
			if _, err := tx.Exec(`DROP INDEX idx_series_metadata_ext`); err != nil {
				return err
			}
		}
	case sql.ErrNoRows:
	default:
		return err
	}
	if _, err := tx.Exec(createSeriesMetadataExtIndex); err != nil {
		return err
	}
	return tx.Commit()
}

func backfillLegacyMatchOrigin(db *sql.DB) error {
	_, err := db.Exec(`
UPDATE series_metadata
SET match_origin = CASE
  WHEN coalesce(scrape_confidence, 0) >= 0.999
    OR status_lock = 1 OR title_lock = 1 OR title_sort_lock = 1 OR summary_lock = 1
    OR reading_direction_lock = 1 OR publisher_lock = 1 OR age_rating_lock = 1
    OR language_lock = 1 OR total_book_count_lock = 1 OR release_date_lock = 1
    OR genres_lock = 1 OR tags_lock = 1 OR authors_lock = 1 OR alternate_titles_lock = 1
    OR links_lock = 1 OR sharing_labels_lock = 1 OR nsfw_lock = 1
  THEN 'manual'
  ELSE 'auto'
END
WHERE coalesce(match_origin, '') = ''
  AND bgm_subject_id > 0
  AND coalesce(ext_entry_id, '') = ''`)
	return err
}

func columnExistsTx(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func addColumnIfMissingTx(tx *sql.Tx, table, column, definition string) error {
	exists, err := columnExistsTx(tx, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

func triggerHasBulkGuard(db *sql.DB, name string) bool {
	var def string
	if err := db.QueryRow(`SELECT COALESCE(sql,'') FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&def); err != nil {
		return false
	}
	return strings.Contains(def, "bulk_mode")
}

func ensureSeriesRollups(db *sql.DB) error {
	if !triggerHasBulkGuard(db, "fk115_books_rollup_upd") {
		for _, name := range []string{
			"fk115_series_rollup_ins",
			"fk115_series_rollup_upd",
			"fk115_books_rollup_ins",
			"fk115_books_rollup_del",
			"fk115_books_rollup_upd",
		} {
			if _, err := db.Exec("DROP TRIGGER IF EXISTS " + name); err != nil {
				return err
			}
		}
	}
	triggers := []string{
		`CREATE TRIGGER IF NOT EXISTS fk115_series_rollup_ins
AFTER INSERT ON series
WHEN (NEW.sort_modified_at='' OR NEW.sort_created_at='') AND (SELECT bulk_mode FROM shelf_revision WHERE id=1)=0
BEGIN
  UPDATE series
  SET sort_modified_at=COALESCE(NULLIF(latest_book_modified_at,''), NULLIF(file_modified_at,''), updated_at),
      sort_created_at=COALESCE(NULLIF(latest_book_created_at,''), created_at)
  WHERE id=NEW.id;
END`,
		`CREATE TRIGGER IF NOT EXISTS fk115_series_rollup_upd
AFTER UPDATE OF file_modified_at, updated_at, created_at ON series
WHEN (OLD.file_modified_at IS NOT NEW.file_modified_at OR OLD.updated_at IS NOT NEW.updated_at OR OLD.created_at IS NOT NEW.created_at)
 AND (SELECT bulk_mode FROM shelf_revision WHERE id=1)=0
BEGIN
  UPDATE series
  SET sort_modified_at=COALESCE(NULLIF(latest_book_modified_at,''), NULLIF(NEW.file_modified_at,''), NEW.updated_at),
      sort_created_at=COALESCE(NULLIF(latest_book_created_at,''), NEW.created_at)
  WHERE id=NEW.id;
END`,
		`CREATE TRIGGER IF NOT EXISTS fk115_books_rollup_ins
AFTER INSERT ON books
WHEN (SELECT bulk_mode FROM shelf_revision WHERE id=1)=0
BEGIN
  UPDATE series
  SET books_count=books_count+1,
      latest_book_modified_at=CASE
        WHEN NEW.file_modified_at IS NULL OR NEW.file_modified_at='' THEN latest_book_modified_at
        WHEN latest_book_modified_at IS NULL OR latest_book_modified_at='' OR NEW.file_modified_at>latest_book_modified_at THEN NEW.file_modified_at
        ELSE latest_book_modified_at
      END,
      latest_book_created_at=CASE
        WHEN NEW.file_created_at IS NULL OR NEW.file_created_at='' THEN latest_book_created_at
        WHEN latest_book_created_at IS NULL OR latest_book_created_at='' OR NEW.file_created_at>latest_book_created_at THEN NEW.file_created_at
        ELSE latest_book_created_at
      END,
      sort_modified_at=COALESCE(
        NULLIF(CASE
          WHEN NEW.file_modified_at IS NULL OR NEW.file_modified_at='' THEN latest_book_modified_at
          WHEN latest_book_modified_at IS NULL OR latest_book_modified_at='' OR NEW.file_modified_at>latest_book_modified_at THEN NEW.file_modified_at
          ELSE latest_book_modified_at
        END,''),
        NULLIF(file_modified_at,''),
        updated_at
      ),
      sort_created_at=COALESCE(
        NULLIF(CASE
          WHEN NEW.file_created_at IS NULL OR NEW.file_created_at='' THEN latest_book_created_at
          WHEN latest_book_created_at IS NULL OR latest_book_created_at='' OR NEW.file_created_at>latest_book_created_at THEN NEW.file_created_at
          ELSE latest_book_created_at
        END,''),
        created_at
      )
  WHERE id=NEW.series_id;
END`,
		`CREATE TRIGGER IF NOT EXISTS fk115_books_rollup_del
AFTER DELETE ON books
WHEN (SELECT bulk_mode FROM shelf_revision WHERE id=1)=0 AND EXISTS(SELECT 1 FROM series WHERE id=OLD.series_id)
BEGIN
  UPDATE series
  SET books_count=(SELECT count(*) FROM books b WHERE b.series_id=OLD.series_id),
      latest_book_modified_at=(SELECT MAX(NULLIF(b.file_modified_at,'')) FROM books b WHERE b.series_id=OLD.series_id),
      latest_book_created_at=(SELECT MAX(NULLIF(b.file_created_at,'')) FROM books b WHERE b.series_id=OLD.series_id),
      sort_modified_at=COALESCE((SELECT MAX(NULLIF(b.file_modified_at,'')) FROM books b WHERE b.series_id=OLD.series_id), NULLIF(file_modified_at,''), updated_at),
      sort_created_at=COALESCE((SELECT MAX(NULLIF(b.file_created_at,'')) FROM books b WHERE b.series_id=OLD.series_id), created_at)
  WHERE id=OLD.series_id;
END`,
		`CREATE TRIGGER IF NOT EXISTS fk115_books_rollup_upd
AFTER UPDATE OF series_id, file_modified_at, file_created_at ON books
WHEN (OLD.series_id IS NOT NEW.series_id OR OLD.file_modified_at IS NOT NEW.file_modified_at OR OLD.file_created_at IS NOT NEW.file_created_at)
 AND (SELECT bulk_mode FROM shelf_revision WHERE id=1)=0
BEGIN
  UPDATE series
  SET books_count=(SELECT count(*) FROM books b WHERE b.series_id=NEW.series_id),
      latest_book_modified_at=(SELECT MAX(NULLIF(b.file_modified_at,'')) FROM books b WHERE b.series_id=NEW.series_id),
      latest_book_created_at=(SELECT MAX(NULLIF(b.file_created_at,'')) FROM books b WHERE b.series_id=NEW.series_id),
      sort_modified_at=COALESCE((SELECT MAX(NULLIF(b.file_modified_at,'')) FROM books b WHERE b.series_id=NEW.series_id), NULLIF(file_modified_at,''), updated_at),
      sort_created_at=COALESCE((SELECT MAX(NULLIF(b.file_created_at,'')) FROM books b WHERE b.series_id=NEW.series_id), created_at)
  WHERE id=NEW.series_id;
  UPDATE series
  SET books_count=(SELECT count(*) FROM books b WHERE b.series_id=OLD.series_id),
      latest_book_modified_at=(SELECT MAX(NULLIF(b.file_modified_at,'')) FROM books b WHERE b.series_id=OLD.series_id),
      latest_book_created_at=(SELECT MAX(NULLIF(b.file_created_at,'')) FROM books b WHERE b.series_id=OLD.series_id),
      sort_modified_at=COALESCE((SELECT MAX(NULLIF(b.file_modified_at,'')) FROM books b WHERE b.series_id=OLD.series_id), NULLIF(file_modified_at,''), updated_at),
      sort_created_at=COALESCE((SELECT MAX(NULLIF(b.file_created_at,'')) FROM books b WHERE b.series_id=OLD.series_id), created_at)
  WHERE OLD.series_id<>NEW.series_id AND id=OLD.series_id;
END`,
	}
	for _, stmt := range triggers {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}

	var done string
	err := db.QueryRow(`SELECT value FROM settings WHERE key='series_rollups_v1'`).Scan(&done)
	if err == nil && done == "1" {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS temp._fk115_series_agg`,
		`CREATE TEMP TABLE _fk115_series_agg (
  series_id TEXT PRIMARY KEY,
  cnt INTEGER NOT NULL,
  max_mod TEXT,
  max_cre TEXT
)`,
		`INSERT INTO _fk115_series_agg(series_id, cnt, max_mod, max_cre)
SELECT series_id, count(*), MAX(NULLIF(file_modified_at,'')), MAX(NULLIF(file_created_at,''))
FROM books
GROUP BY series_id`,
		`UPDATE series
SET books_count=0,
    latest_book_modified_at=NULL,
    latest_book_created_at=NULL,
    sort_modified_at=COALESCE(NULLIF(file_modified_at,''), updated_at),
    sort_created_at=created_at`,
		`UPDATE series
SET books_count=a.cnt,
    latest_book_modified_at=a.max_mod,
    latest_book_created_at=a.max_cre,
    sort_modified_at=COALESCE(a.max_mod, NULLIF(series.file_modified_at,''), series.updated_at),
    sort_created_at=COALESCE(a.max_cre, series.created_at)
FROM _fk115_series_agg a
WHERE a.series_id=series.id`,
		`DROP TABLE IF EXISTS temp._fk115_series_agg`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`
INSERT INTO settings(key,value,updated_at) VALUES('series_rollups_v1','1',?)
ON CONFLICT(key) DO UPDATE SET value='1',updated_at=excluded.updated_at`, nowText()); err != nil {
		return err
	}
	return tx.Commit()
}

func ensureMetadataAndScrapeTables(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS series_metadata (
  series_id TEXT PRIMARY KEY,
  bgm_subject_id INTEGER,
  scrape_status TEXT NOT NULL DEFAULT 'none',
  scrape_confidence REAL NOT NULL DEFAULT 0,
  scrape_synced_at TEXT,
  cover_source TEXT NOT NULL DEFAULT 'auto',
  bgm_cover_url TEXT NOT NULL DEFAULT '',
  bgm_cover_path TEXT NOT NULL DEFAULT '',
  bgm_score REAL,
  status TEXT NOT NULL DEFAULT 'ONGOING',
  status_lock INTEGER NOT NULL DEFAULT 0,
  title TEXT NOT NULL DEFAULT '',
  title_lock INTEGER NOT NULL DEFAULT 0,
  title_sort TEXT NOT NULL DEFAULT '',
  title_sort_lock INTEGER NOT NULL DEFAULT 0,
  summary TEXT NOT NULL DEFAULT '',
  summary_lock INTEGER NOT NULL DEFAULT 0,
  reading_direction TEXT NOT NULL DEFAULT 'LEFT_TO_RIGHT',
  reading_direction_lock INTEGER NOT NULL DEFAULT 0,
  publisher TEXT NOT NULL DEFAULT '',
  publisher_lock INTEGER NOT NULL DEFAULT 0,
  age_rating INTEGER,
  age_rating_lock INTEGER NOT NULL DEFAULT 0,
  language TEXT NOT NULL DEFAULT '',
  language_lock INTEGER NOT NULL DEFAULT 0,
  total_book_count INTEGER,
  total_book_count_lock INTEGER NOT NULL DEFAULT 0,
  release_date TEXT NOT NULL DEFAULT '',
  release_date_lock INTEGER NOT NULL DEFAULT 0,
  genres_json TEXT NOT NULL DEFAULT '[]',
  genres_lock INTEGER NOT NULL DEFAULT 0,
  tags_json TEXT NOT NULL DEFAULT '[]',
  tags_lock INTEGER NOT NULL DEFAULT 0,
  authors_json TEXT NOT NULL DEFAULT '[]',
  authors_lock INTEGER NOT NULL DEFAULT 0,
  alternate_titles_json TEXT NOT NULL DEFAULT '[]',
  alternate_titles_lock INTEGER NOT NULL DEFAULT 0,
  links_json TEXT NOT NULL DEFAULT '[]',
  links_lock INTEGER NOT NULL DEFAULT 0,
  sharing_labels_json TEXT NOT NULL DEFAULT '[]',
  sharing_labels_lock INTEGER NOT NULL DEFAULT 0,
  is_nsfw INTEGER NOT NULL DEFAULT 0,
  nsfw_lock INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_series_metadata_title ON series_metadata(title COLLATE NOCASE);
CREATE INDEX IF NOT EXISTS idx_series_metadata_publisher ON series_metadata(publisher COLLATE NOCASE);
CREATE INDEX IF NOT EXISTS idx_series_metadata_scrape_status ON series_metadata(scrape_status);
CREATE INDEX IF NOT EXISTS idx_series_metadata_bgm_id ON series_metadata(bgm_subject_id);

CREATE TABLE IF NOT EXISTS series_genres (
  series_id TEXT NOT NULL,
  genre TEXT NOT NULL,
  PRIMARY KEY(series_id, genre),
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_series_genres_genre ON series_genres(genre COLLATE NOCASE, series_id);

CREATE TABLE IF NOT EXISTS series_tags (
  series_id TEXT NOT NULL,
  tag TEXT NOT NULL,
  PRIMARY KEY(series_id, tag),
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_series_tags_tag ON series_tags(tag COLLATE NOCASE, series_id);

CREATE TABLE IF NOT EXISTS series_authors (
  series_id TEXT NOT NULL,
  name TEXT NOT NULL,
  role TEXT NOT NULL,
  PRIMARY KEY(series_id, name, role),
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_series_authors_name ON series_authors(name COLLATE NOCASE, role, series_id);

CREATE TABLE IF NOT EXISTS bangumi_scrape_runs (
  id TEXT PRIMARY KEY,
  library_id TEXT,
  mode TEXT NOT NULL,
  force INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL,
  total_series INTEGER NOT NULL DEFAULT 0,
  processed_series INTEGER NOT NULL DEFAULT 0,
  matched_count INTEGER NOT NULL DEFAULT 0,
  unmatched_count INTEGER NOT NULL DEFAULT 0,
  skipped_count INTEGER NOT NULL DEFAULT 0,
  failed_count INTEGER NOT NULL DEFAULT 0,
  current_series TEXT NOT NULL DEFAULT '',
  errors_json TEXT NOT NULL DEFAULT '[]',
  cancel_requested INTEGER NOT NULL DEFAULT 0,
  started_at TEXT,
  completed_at TEXT,
  created_at TEXT NOT NULL,
  FOREIGN KEY(library_id) REFERENCES libraries(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_bangumi_scrape_runs_created ON bangumi_scrape_runs(created_at DESC);
`)
	return err
}

func ensureClientSettingsTable(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS client_settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  allow_unauthenticated INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL
);
`)
	return err
}

func ensureOperationStats(db *sql.DB) error {
	if err := addColumnIfMissing(db, "zip_indexes", "index_duration_ns",
		"INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "series_thumbnails", "generation_duration_ns",
		"INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS book_download_stats (
  book_id TEXT PRIMARY KEY,
  series_id TEXT NOT NULL,
  bytes INTEGER NOT NULL DEFAULT 0,
  duration_ns INTEGER NOT NULL DEFAULT 0,
  samples INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(book_id) REFERENCES books(id) ON DELETE CASCADE,
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_book_download_stats_series ON book_download_stats(series_id);
`)
	return err
}

func ensureMaintenanceTables(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS maintenance_runs (
  id TEXT PRIMARY KEY,
  operation TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_id TEXT NOT NULL,
  target_name TEXT NOT NULL DEFAULT '',
  series_id TEXT,
  book_id TEXT,
  force INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL,
  total_items INTEGER NOT NULL DEFAULT 0,
  processed_items INTEGER NOT NULL DEFAULT 0,
  generated_count INTEGER NOT NULL DEFAULT 0,
  skipped_count INTEGER NOT NULL DEFAULT 0,
  failed_count INTEGER NOT NULL DEFAULT 0,
  current_item TEXT NOT NULL DEFAULT '',
  errors_json TEXT NOT NULL DEFAULT '[]',
  cancel_requested INTEGER NOT NULL DEFAULT 0,
  started_at TEXT,
  completed_at TEXT,
  created_at TEXT NOT NULL,
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE,
  FOREIGN KEY(book_id) REFERENCES books(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_maintenance_runs_created ON maintenance_runs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_maintenance_runs_target ON maintenance_runs(target_type,target_id,created_at DESC);
CREATE INDEX IF NOT EXISTS idx_maintenance_runs_series ON maintenance_runs(series_id) WHERE series_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_maintenance_runs_book ON maintenance_runs(book_id) WHERE book_id IS NOT NULL;
`)
	return err
}

func ensureProgressTables(db *sql.DB) error {
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS book_read_progress (
  book_id TEXT PRIMARY KEY,
  series_id TEXT NOT NULL,
  completed INTEGER NOT NULL DEFAULT 1,
  page INTEGER,
  read_date TEXT,
  completed_at TEXT,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(book_id) REFERENCES books(id) ON DELETE CASCADE,
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_book_read_progress_series ON book_read_progress(series_id, completed);

CREATE TABLE IF NOT EXISTS book_page_progress (
  book_id TEXT PRIMARY KEY,
  series_id TEXT NOT NULL,
  last_loaded_page INTEGER NOT NULL,
  max_loaded_page INTEGER NOT NULL,
  page_count INTEGER NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(book_id) REFERENCES books(id) ON DELETE CASCADE,
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_book_page_progress_series_updated ON book_page_progress(series_id, updated_at DESC);
`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "book_read_progress", "page", "INTEGER"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "book_read_progress", "read_date", "TEXT"); err != nil {
		return err
	}
	return nil
}

func tableExists(db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func addColumnIfMissing(db *sql.DB, table, column, definition string) error {
	exists, err := columnExists(db, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	query := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)
	_, err = db.Exec(query)
	return err
}

func ensureMultiUserMigration(db *sql.DB) error {
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY,
  username TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'user',
  enabled INTEGER NOT NULL DEFAULT 1,
  all_libraries INTEGER NOT NULL DEFAULT 1,
  allow_nsfw INTEGER NOT NULL DEFAULT 0,
  show_nsfw INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_users_role ON users(role);

CREATE TABLE IF NOT EXISTS user_library_access (
  user_id TEXT NOT NULL,
  library_id TEXT NOT NULL,
  PRIMARY KEY(user_id, library_id),
  FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
  FOREIGN KEY(library_id) REFERENCES libraries(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_ula_library ON user_library_access(library_id);

CREATE TABLE IF NOT EXISTS user_bangumi_trackers (
  user_id TEXT PRIMARY KEY,
  enabled INTEGER NOT NULL DEFAULT 0,
  auth_mode TEXT NOT NULL DEFAULT '',
  access_token TEXT NOT NULL DEFAULT '',
  refresh_token TEXT NOT NULL DEFAULT '',
  expires_at TEXT NOT NULL DEFAULT '',
  last_refresh_at TEXT NOT NULL DEFAULT '',
  last_refresh_error TEXT NOT NULL DEFAULT '',
  oauth_redirect_uri TEXT NOT NULL DEFAULT '',
  bgm_user_id TEXT NOT NULL DEFAULT '',
  bgm_username TEXT NOT NULL DEFAULT '',
  bgm_nickname TEXT NOT NULL DEFAULT '',
  nsfw_mode TEXT NOT NULL DEFAULT 'skip',
  private_default INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL
);`); err != nil {
		return err
	}

	// 1. Seed primary admin row in users from legacy auth_admin (id=1) + settings['show_nsfw'].
	var adminCount int
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE id=?`, PrimaryAdminUserID).Scan(&adminCount); err != nil {
		return err
	}
	if adminCount == 0 {
		var u, h, created, updated string
		err := db.QueryRow(`SELECT username,password_hash,created_at,updated_at FROM auth_admin WHERE id=1`).
			Scan(&u, &h, &created, &updated)
		if err == nil && u != "" {
			showNSFW := 0
			var rawShow string
			if db.QueryRow(`SELECT value FROM settings WHERE key='show_nsfw'`).Scan(&rawShow) == nil &&
				strings.EqualFold(strings.TrimSpace(rawShow), "true") {
				showNSFW = 1
			}
			if _, err := db.Exec(`
INSERT OR IGNORE INTO users(id,username,password_hash,role,enabled,all_libraries,allow_nsfw,show_nsfw,created_at,updated_at)
VALUES(?,?,?,'admin',1,1,1,?,?,?)`, PrimaryAdminUserID, u, h, showNSFW, created, updated); err != nil {
				return err
			}
		}
	}

	// 2. Migrate legacy global Bangumi tracker settings into user_bangumi_trackers for 'admin'.
	var trackerCount int
	if err := db.QueryRow(`SELECT count(*) FROM user_bangumi_trackers WHERE user_id=?`, PrimaryAdminUserID).Scan(&trackerCount); err != nil {
		return err
	}
	if trackerCount == 0 {
		readSetting := func(k string) string {
			var v string
			_ = db.QueryRow(`SELECT value FROM settings WHERE key=?`, k).Scan(&v)
			return strings.TrimSpace(v)
		}
		tok := readSetting("bangumi_tracker_token")
		refTok := readSetting("bangumi_tracker_refresh_token")
		enabledStr := readSetting("bangumi_tracker_enabled")
		if tok != "" || refTok != "" || strings.EqualFold(enabledStr, "true") {
			authMode := readSetting("bangumi_tracker_auth_mode")
			if authMode == "" {
				if refTok != "" {
					authMode = "oauth"
				} else if tok != "" {
					authMode = "pat"
				}
			}
			nsfwMode := readSetting("bangumi_tracker_nsfw_mode")
			if nsfwMode == "" {
				nsfwMode = "skip"
			}
			priv := 0
			if strings.EqualFold(readSetting("bangumi_tracker_private_default"), "true") {
				priv = 1
			}
			en := 0
			if strings.EqualFold(enabledStr, "true") {
				en = 1
			}
			if _, err := db.Exec(`
INSERT OR IGNORE INTO user_bangumi_trackers(
 user_id,enabled,auth_mode,access_token,refresh_token,expires_at,last_refresh_at,last_refresh_error,
 oauth_redirect_uri,bgm_user_id,bgm_username,bgm_nickname,nsfw_mode,private_default,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				PrimaryAdminUserID, en, authMode, tok, refTok,
				readSetting("bangumi_tracker_expires_at"),
				readSetting("bangumi_tracker_last_refresh_at"),
				readSetting("bangumi_tracker_last_refresh_error"),
				readSetting("bangumi_tracker_oauth_redirect_uri"),
				readSetting("bangumi_tracker_user_id"),
				readSetting("bangumi_tracker_username"),
				readSetting("bangumi_tracker_nickname"),
				nsfwMode, priv, nowText(),
			); err != nil {
				return err
			}
		}
	}

	// 3. Add user_id to auth_sessions & auth_api_keys.
	if err := addColumnIfMissing(db, "auth_sessions", "user_id", "TEXT NOT NULL DEFAULT 'admin'"); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_auth_sessions_user ON auth_sessions(user_id, last_seen_at DESC)`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "auth_api_keys", "user_id", "TEXT NOT NULL DEFAULT 'admin'"); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_auth_api_keys_user ON auth_api_keys(user_id, created_at DESC)`); err != nil {
		return err
	}

	// 4. Rebuild composite-PK / composite-UNIQUE tables if migrating from pre-v3.0 schema.
	// Use a dedicated connection with PRAGMA foreign_keys = OFF outside the transaction so
	// dropping collections/readlists during table swap NEVER cascades collection_series/readlist_books!
	type tableRebuild struct {
		name string
		sql  []string
	}
	rebuilds := []tableRebuild{
		{
			name: "client_settings",
			sql: []string{
				`CREATE TABLE client_settings_v3 (
  user_id TEXT NOT NULL DEFAULT 'admin',
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  allow_unauthenticated INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(user_id, key)
)`,
				`INSERT INTO client_settings_v3(user_id,key,value,allow_unauthenticated,updated_at)
SELECT 'admin',key,value,allow_unauthenticated,updated_at FROM client_settings`,
				`DROP TABLE client_settings`,
				`ALTER TABLE client_settings_v3 RENAME TO client_settings`,
			},
		},
		{
			name: "book_read_progress",
			sql: []string{
				`DROP TRIGGER IF EXISTS fk115_shelf_book_read_progress_insert`,
				`DROP TRIGGER IF EXISTS fk115_shelf_book_read_progress_update`,
				`DROP TRIGGER IF EXISTS fk115_shelf_book_read_progress_delete`,
				`CREATE TABLE book_read_progress_v3 (
  user_id TEXT NOT NULL DEFAULT 'admin',
  book_id TEXT NOT NULL,
  series_id TEXT NOT NULL,
  completed INTEGER NOT NULL DEFAULT 1,
  page INTEGER,
  read_date TEXT,
  completed_at TEXT,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(user_id, book_id),
  FOREIGN KEY(book_id) REFERENCES books(id) ON DELETE CASCADE,
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE
)`,
				`INSERT INTO book_read_progress_v3(user_id,book_id,series_id,completed,page,read_date,completed_at,updated_at)
SELECT 'admin',book_id,series_id,completed,page,read_date,completed_at,updated_at FROM book_read_progress`,
				`DROP TABLE book_read_progress`,
				`ALTER TABLE book_read_progress_v3 RENAME TO book_read_progress`,
				`CREATE INDEX IF NOT EXISTS idx_book_read_progress_series ON book_read_progress(user_id, series_id, completed)`,
				`CREATE INDEX IF NOT EXISTS idx_book_read_progress_updated ON book_read_progress(user_id, completed, updated_at DESC)`,
				`CREATE INDEX IF NOT EXISTS idx_book_read_progress_updated_all ON book_read_progress(user_id, updated_at DESC, book_id)`,
			},
		},
		{
			name: "book_page_progress",
			sql: []string{
				`CREATE TABLE book_page_progress_v3 (
  user_id TEXT NOT NULL DEFAULT 'admin',
  book_id TEXT NOT NULL,
  series_id TEXT NOT NULL,
  last_loaded_page INTEGER NOT NULL,
  max_loaded_page INTEGER NOT NULL,
  page_count INTEGER NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(user_id, book_id),
  FOREIGN KEY(book_id) REFERENCES books(id) ON DELETE CASCADE,
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE
)`,
				`INSERT INTO book_page_progress_v3(user_id,book_id,series_id,last_loaded_page,max_loaded_page,page_count,updated_at)
SELECT 'admin',book_id,series_id,last_loaded_page,max_loaded_page,page_count,updated_at FROM book_page_progress`,
				`DROP TABLE book_page_progress`,
				`ALTER TABLE book_page_progress_v3 RENAME TO book_page_progress`,
				`CREATE INDEX IF NOT EXISTS idx_book_page_progress_series_updated ON book_page_progress(user_id, series_id, updated_at DESC)`,
			},
		},
		{
			name: "book_read_progress_tombstones",
			sql: []string{
				`DROP TRIGGER IF EXISTS fk115_shelf_book_read_progress_tombstones_insert`,
				`DROP TRIGGER IF EXISTS fk115_shelf_book_read_progress_tombstones_update`,
				`DROP TRIGGER IF EXISTS fk115_shelf_book_read_progress_tombstones_delete`,
				`CREATE TABLE book_read_progress_tombstones_v3 (
  user_id TEXT NOT NULL DEFAULT 'admin',
  book_id TEXT NOT NULL,
  series_id TEXT NOT NULL,
  deleted_at TEXT NOT NULL,
  PRIMARY KEY(user_id, book_id),
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE
)`,
				`INSERT INTO book_read_progress_tombstones_v3(user_id,book_id,series_id,deleted_at)
SELECT 'admin',book_id,series_id,deleted_at FROM book_read_progress_tombstones`,
				`DROP TABLE book_read_progress_tombstones`,
				`ALTER TABLE book_read_progress_tombstones_v3 RENAME TO book_read_progress_tombstones`,
				`CREATE INDEX IF NOT EXISTS idx_progress_tombstones_series ON book_read_progress_tombstones(user_id, series_id)`,
				`CREATE INDEX IF NOT EXISTS idx_progress_tombstones_deleted ON book_read_progress_tombstones(deleted_at)`,
			},
		},
		{
			name: "collections",
			sql: []string{
				`CREATE TABLE collections_v3 (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL DEFAULT 'admin',
  name TEXT NOT NULL COLLATE NOCASE,
  ordered INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(user_id, name COLLATE NOCASE)
)`,
				`INSERT INTO collections_v3(rowid,id,user_id,name,ordered,created_at,updated_at)
SELECT rowid,id,'admin',name,ordered,created_at,updated_at FROM collections`,
				`DROP TABLE collections`,
				`ALTER TABLE collections_v3 RENAME TO collections`,
				`CREATE INDEX IF NOT EXISTS idx_collections_user ON collections(user_id, name COLLATE NOCASE)`,
			},
		},
		{
			name: "readlists",
			sql: []string{
				`CREATE TABLE readlists_v3 (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL DEFAULT 'admin',
  name TEXT NOT NULL COLLATE NOCASE,
  summary TEXT NOT NULL DEFAULT '',
  ordered INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(user_id, name COLLATE NOCASE)
)`,
				`INSERT INTO readlists_v3(id,user_id,name,summary,ordered,created_at,updated_at)
SELECT id,'admin',name,summary,ordered,created_at,updated_at FROM readlists`,
				`DROP TABLE readlists`,
				`ALTER TABLE readlists_v3 RENAME TO readlists`,
				`CREATE INDEX IF NOT EXISTS idx_readlists_user ON readlists(user_id, name COLLATE NOCASE)`,
			},
		},
		{
			name: "bangumi_tracker_records",
			sql: []string{
				`CREATE TABLE bangumi_tracker_records_v3 (
  user_id TEXT NOT NULL DEFAULT 'admin',
  bgm_subject_id INTEGER NOT NULL,
  series_id TEXT NOT NULL DEFAULT '',
  series_title TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  remote_type INTEGER NOT NULL DEFAULT 0,
  remote_vol_status INTEGER NOT NULL DEFAULT 0,
  remote_private INTEGER NOT NULL DEFAULT 0,
  last_local_vol INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  last_synced_at TEXT,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(user_id, bgm_subject_id)
)`,
				`INSERT INTO bangumi_tracker_records_v3(
 user_id,bgm_subject_id,series_id,series_title,status,remote_type,remote_vol_status,
 remote_private,last_local_vol,last_error,last_synced_at,updated_at
)
SELECT 'admin',bgm_subject_id,series_id,series_title,status,remote_type,remote_vol_status,
 remote_private,last_local_vol,last_error,last_synced_at,updated_at FROM bangumi_tracker_records`,
				`DROP TABLE bangumi_tracker_records`,
				`ALTER TABLE bangumi_tracker_records_v3 RENAME TO bangumi_tracker_records`,
				`CREATE INDEX IF NOT EXISTS idx_bgm_tracker_updated ON bangumi_tracker_records(user_id, updated_at DESC)`,
				`CREATE INDEX IF NOT EXISTS idx_bgm_tracker_series ON bangumi_tracker_records(user_id, series_id)`,
			},
		},
	}

	// Ensure optional legacy columns exist before table rebuild SELECTs.
	_ = addColumnIfMissing(db, "book_read_progress", "completed_at", "TEXT")
	_ = addColumnIfMissing(db, "bangumi_tracker_records", "last_local_vol", "INTEGER NOT NULL DEFAULT 0")

	var pending []tableRebuild
	for _, rb := range rebuilds {
		hasCol, err := columnExists(db, rb.name, "user_id")
		if err != nil {
			return err
		}
		if !hasCol {
			pending = append(pending, rb)
		}
	}
	if len(pending) > 0 {
		ctx := context.Background()
		conn, err := db.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()

		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			return err
		}
		defer conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)

		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		for _, rb := range pending {
			for _, stmt := range rb.sql {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("migrate %s to multi-user: %w", rb.name, err)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	// Ensure all multi-user indexes exist on both fresh installs and upgraded databases.
	for _, idxSQL := range []string{
		`CREATE INDEX IF NOT EXISTS idx_auth_sessions_user ON auth_sessions(user_id, last_seen_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_auth_api_keys_user ON auth_api_keys(user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_book_read_progress_series ON book_read_progress(user_id, series_id, completed)`,
		`CREATE INDEX IF NOT EXISTS idx_book_read_progress_updated ON book_read_progress(user_id, completed, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_book_read_progress_updated_all ON book_read_progress(user_id, updated_at DESC, book_id)`,
		`CREATE INDEX IF NOT EXISTS idx_book_page_progress_series_updated ON book_page_progress(user_id, series_id, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_progress_tombstones_series ON book_read_progress_tombstones(user_id, series_id)`,
		`CREATE INDEX IF NOT EXISTS idx_collections_user ON collections(user_id, name COLLATE NOCASE)`,
		`CREATE INDEX IF NOT EXISTS idx_readlists_user ON readlists(user_id, name COLLATE NOCASE)`,
		`CREATE INDEX IF NOT EXISTS idx_bgm_tracker_updated ON bangumi_tracker_records(user_id, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_bgm_tracker_series ON bangumi_tracker_records(user_id, series_id)`,
	} {
		if _, err := db.Exec(idxSQL); err != nil {
			return err
		}
	}
	return nil
}
