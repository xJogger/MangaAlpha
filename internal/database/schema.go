package database

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 10000;

CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS client_settings (
  user_id TEXT NOT NULL DEFAULT 'admin',
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  allow_unauthenticated INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(user_id, key)
);

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

` + userHiddenLibrariesSchema + `

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
);

CREATE TABLE IF NOT EXISTS auth_admin (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  username TEXT NOT NULL,
  password_hash TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS auth_sessions (
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL DEFAULT 'admin',
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
  user_id TEXT NOT NULL DEFAULT 'admin',
  name TEXT NOT NULL,
  key_prefix TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  last_used_at TEXT
);

CREATE TABLE IF NOT EXISTS provider_accounts (
  provider TEXT PRIMARY KEY,
  access_token TEXT NOT NULL DEFAULT '',
  refresh_token TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);

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

CREATE TABLE IF NOT EXISTS libraries (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  provider_type TEXT NOT NULL DEFAULT '115',
  root_cid TEXT NOT NULL UNIQUE,
  webdav_account_id TEXT NOT NULL DEFAULT '',
  root_path TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  one_shot INTEGER NOT NULL DEFAULT 0,
  oneshots_directory TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  last_scan_started_at TEXT,
  last_scan_completed_at TEXT,
  last_scan_status TEXT NOT NULL DEFAULT 'never',
  last_scan_error TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS scan_runs (
  id TEXT PRIMARY KEY,
  library_id TEXT,
  status TEXT NOT NULL,
  trigger_type TEXT NOT NULL,
  started_at TEXT,
  completed_at TEXT,
  directories_seen INTEGER NOT NULL DEFAULT 0,
  files_seen INTEGER NOT NULL DEFAULT 0,
  series_seen INTEGER NOT NULL DEFAULT 0,
  books_seen INTEGER NOT NULL DEFAULT 0,
  current_path TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  cancel_requested INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  FOREIGN KEY(library_id) REFERENCES libraries(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_scan_runs_created ON scan_runs(created_at DESC);

CREATE TABLE IF NOT EXISTS series (
  id TEXT PRIMARY KEY,
  library_id TEXT NOT NULL,
  cid TEXT NOT NULL,
  name TEXT NOT NULL,
  relative_path TEXT NOT NULL,
  one_shot INTEGER NOT NULL DEFAULT 0,
  one_shot_override INTEGER NOT NULL DEFAULT 0,
  file_modified_at TEXT,
  books_count INTEGER NOT NULL DEFAULT 0,
  latest_book_modified_at TEXT,
  latest_book_created_at TEXT,
  sort_modified_at TEXT NOT NULL DEFAULT '',
  sort_created_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  seen_scan_id TEXT NOT NULL,
  UNIQUE(library_id, cid),
  FOREIGN KEY(library_id) REFERENCES libraries(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_series_library_name ON series(library_id, name);
CREATE INDEX IF NOT EXISTS idx_series_seen_scan ON series(library_id, seen_scan_id);

CREATE TABLE IF NOT EXISTS books (
  id TEXT PRIMARY KEY,
  series_id TEXT NOT NULL,
  library_id TEXT NOT NULL,
  file_id TEXT NOT NULL,
  parent_cid TEXT NOT NULL,
  name TEXT NOT NULL,
  size INTEGER NOT NULL,
  pick_code TEXT NOT NULL,
  sha1 TEXT NOT NULL DEFAULT '',
  file_created_at TEXT,
  file_modified_at TEXT,
  number_sort REAL NOT NULL,
  number_sort_override REAL,
  effective_number_sort REAL GENERATED ALWAYS AS (COALESCE(number_sort_override, number_sort)) VIRTUAL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  seen_scan_id TEXT NOT NULL,
  UNIQUE(library_id, file_id),
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE,
  FOREIGN KEY(library_id) REFERENCES libraries(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_books_series_number ON books(series_id, number_sort);
CREATE INDEX IF NOT EXISTS idx_books_series_modified ON books(series_id, file_modified_at);
CREATE INDEX IF NOT EXISTS idx_books_library_name ON books(library_id, name);
CREATE INDEX IF NOT EXISTS idx_books_library_size ON books(library_id, size);
CREATE INDEX IF NOT EXISTS idx_books_seen_scan ON books(library_id, seen_scan_id);

CREATE TABLE IF NOT EXISTS scan_series_staging (
  run_id TEXT NOT NULL,
  id TEXT NOT NULL,
  library_id TEXT NOT NULL,
  cid TEXT NOT NULL,
  name TEXT NOT NULL,
  relative_path TEXT NOT NULL,
  one_shot INTEGER NOT NULL DEFAULT 0,
  file_modified_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(run_id, id),
  UNIQUE(run_id, library_id, cid),
  FOREIGN KEY(run_id) REFERENCES scan_runs(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS scan_books_staging (
  run_id TEXT NOT NULL,
  id TEXT NOT NULL,
  series_id TEXT NOT NULL,
  library_id TEXT NOT NULL,
  file_id TEXT NOT NULL,
  parent_cid TEXT NOT NULL,
  name TEXT NOT NULL,
  size INTEGER NOT NULL,
  pick_code TEXT NOT NULL,
  sha1 TEXT NOT NULL DEFAULT '',
  file_created_at TEXT,
  file_modified_at TEXT,
  number_sort REAL NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(run_id, id),
  UNIQUE(run_id, library_id, file_id),
  FOREIGN KEY(run_id) REFERENCES scan_runs(id) ON DELETE CASCADE
);

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
  bgm_rank INTEGER,
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
  ext_source_id TEXT NOT NULL DEFAULT '',
  ext_entry_id TEXT NOT NULL DEFAULT '',
  ext_cover_url TEXT NOT NULL DEFAULT '',
  ext_cover_path TEXT NOT NULL DEFAULT '',
  match_origin TEXT NOT NULL DEFAULT '',
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
  library_ids_json TEXT NOT NULL DEFAULT '',
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

CREATE TABLE IF NOT EXISTS zip_indexes (
  book_id TEXT PRIMARY KEY,
  version TEXT NOT NULL,
  page_count INTEGER NOT NULL,
  index_json TEXT NOT NULL,
  index_duration_ns INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(book_id) REFERENCES books(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS book_read_progress (
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
);

CREATE TABLE IF NOT EXISTS book_page_progress (
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
);

CREATE TABLE IF NOT EXISTS series_thumbnails (
  series_id TEXT PRIMARY KEY,
  source_book_id TEXT NOT NULL,
  source_version TEXT NOT NULL,
  path TEXT NOT NULL,
  media_type TEXT NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  size INTEGER NOT NULL,
  generation_duration_ns INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE,
  FOREIGN KEY(source_book_id) REFERENCES books(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_series_thumbnails_source_book ON series_thumbnails(source_book_id);

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

CREATE TABLE IF NOT EXISTS thumbnail_runs (
  id TEXT PRIMARY KEY,
  library_id TEXT NOT NULL,
  mode TEXT NOT NULL,
  requested_limit INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL,
  total_series INTEGER NOT NULL DEFAULT 0,
  processed_series INTEGER NOT NULL DEFAULT 0,
  generated_count INTEGER NOT NULL DEFAULT 0,
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
CREATE INDEX IF NOT EXISTS idx_thumbnail_runs_created ON thumbnail_runs(created_at DESC);

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

CREATE TABLE IF NOT EXISTS downurl_cache (
  cache_key TEXT PRIMARY KEY,
  pick_code TEXT NOT NULL,
  ua_hash TEXT NOT NULL,
  url TEXT NOT NULL,
  user_agent TEXT NOT NULL,
  expire_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_downurl_expire ON downurl_cache(expire_at);

CREATE TABLE IF NOT EXISTS cache_entries (
  cache_key TEXT PRIMARY KEY,
  cache_type TEXT NOT NULL,
  path TEXT NOT NULL,
  size INTEGER NOT NULL,
  last_access_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cache_entries_type_access ON cache_entries(cache_type, last_access_at);

CREATE TABLE IF NOT EXISTS book_read_progress_tombstones (
  user_id TEXT NOT NULL DEFAULT 'admin',
  book_id TEXT NOT NULL,
  series_id TEXT NOT NULL,
  deleted_at TEXT NOT NULL,
  PRIMARY KEY(user_id, book_id),
  FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_progress_tombstones_deleted ON book_read_progress_tombstones(deleted_at);

CREATE TABLE IF NOT EXISTS collections (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL DEFAULT 'admin',
  name TEXT NOT NULL COLLATE NOCASE,
  ordered INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(user_id, name COLLATE NOCASE)
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
  user_id TEXT NOT NULL DEFAULT 'admin',
  name TEXT NOT NULL COLLATE NOCASE,
  summary TEXT NOT NULL DEFAULT '',
  ordered INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(user_id, name COLLATE NOCASE)
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

CREATE TABLE IF NOT EXISTS bangumi_tracker_records (
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
);

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
  import_covers INTEGER NOT NULL DEFAULT 0,
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
` + metadataUnionSchema

const effectiveBookNumberIndexes = `
CREATE INDEX IF NOT EXISTS idx_books_series_eff_number ON books(series_id, effective_number_sort, name COLLATE NOCASE, id);
CREATE INDEX IF NOT EXISTS idx_books_eff_number ON books(effective_number_sort, name COLLATE NOCASE, id);
CREATE INDEX IF NOT EXISTS idx_series_effective_oneshot ON series(library_id, id)
WHERE one_shot = 1 OR (one_shot_override = 1 AND books_count = 1);
`

const legacyCoverRefIndexes = `
CREATE INDEX IF NOT EXISTS idx_series_metadata_legacy_blob ON series_metadata(ext_cover_url)
WHERE ext_cover_url GLOB 'sqlite-blob://*';
CREATE INDEX IF NOT EXISTS idx_book_metadata_legacy_blob ON book_metadata(ext_cover_url)
WHERE ext_cover_url GLOB 'sqlite-blob://*';
`

const bookNumberOverrideTriggers = `
CREATE TRIGGER IF NOT EXISTS trg_book_number_override_insert AFTER INSERT ON book_metadata
WHEN NEW.number_sort IS NOT NULL BEGIN
 UPDATE books SET number_sort_override=NEW.number_sort WHERE id=NEW.book_id;
END;
CREATE TRIGGER IF NOT EXISTS trg_book_number_override_update AFTER UPDATE OF number_sort ON book_metadata
WHEN OLD.number_sort IS NOT NEW.number_sort BEGIN
 UPDATE books SET number_sort_override=NEW.number_sort WHERE id=NEW.book_id;
END;
CREATE TRIGGER IF NOT EXISTS trg_book_number_override_delete AFTER DELETE ON book_metadata
WHEN OLD.number_sort IS NOT NULL BEGIN
 UPDATE books SET number_sort_override=NULL WHERE id=OLD.book_id;
END;
`

const bookMetadataSchema = `
CREATE TABLE IF NOT EXISTS book_metadata (
  book_id TEXT PRIMARY KEY REFERENCES books(id) ON DELETE CASCADE,
  title TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  number TEXT NOT NULL DEFAULT '',
  number_sort REAL,
  release_date TEXT NOT NULL DEFAULT '',
  isbn TEXT NOT NULL DEFAULT '',
  authors_json TEXT NOT NULL DEFAULT '[]',
  tags_json TEXT NOT NULL DEFAULT '[]',
  links_json TEXT NOT NULL DEFAULT '[]',
  title_lock INTEGER NOT NULL DEFAULT 0,
  summary_lock INTEGER NOT NULL DEFAULT 0,
  number_lock INTEGER NOT NULL DEFAULT 0,
  number_sort_lock INTEGER NOT NULL DEFAULT 0,
  release_date_lock INTEGER NOT NULL DEFAULT 0,
  authors_lock INTEGER NOT NULL DEFAULT 0,
  tags_lock INTEGER NOT NULL DEFAULT 0,
  isbn_lock INTEGER NOT NULL DEFAULT 0,
  links_lock INTEGER NOT NULL DEFAULT 0,
  bgm_subject_id INTEGER NOT NULL DEFAULT 0,
  bgm_cover_url TEXT NOT NULL DEFAULT '',
  bgm_cover_path TEXT NOT NULL DEFAULT '',
  bgm_is_nsfw INTEGER NOT NULL DEFAULT 0,
  ext_source_id TEXT NOT NULL DEFAULT '',
  ext_entry_id TEXT NOT NULL DEFAULT '',
  ext_cover_url TEXT NOT NULL DEFAULT '',
  ext_cover_path TEXT NOT NULL DEFAULT '',
  custom_cover_path TEXT NOT NULL DEFAULT '',
  custom_cover_url TEXT NOT NULL DEFAULT '',
  cover_source TEXT NOT NULL DEFAULT 'auto'
    CHECK (cover_source IN ('auto','custom','ext','bangumi','book')),
  series_group TEXT NOT NULL DEFAULT '',
  story_arc TEXT NOT NULL DEFAULT '',
  story_arc_number TEXT NOT NULL DEFAULT '',
  comicinfo_version TEXT NOT NULL DEFAULT '',
  comicinfo_status TEXT NOT NULL DEFAULT ''
    CHECK (comicinfo_status IN ('','parsed','absent','skipped_solid','skipped_encrypted','skipped_unsupported')),
  match_origin TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT '',
  explicit_empty INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_book_metadata_bgm ON book_metadata(bgm_subject_id);
CREATE INDEX IF NOT EXISTS idx_book_metadata_ext ON book_metadata(ext_source_id,ext_entry_id);

CREATE TABLE IF NOT EXISTS book_thumbnails (
  book_id TEXT PRIMARY KEY REFERENCES books(id) ON DELETE CASCADE,
  source_version TEXT NOT NULL,
  path TEXT NOT NULL,
  media_type TEXT NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  size INTEGER NOT NULL,
  generation_duration_ns INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
`

const metadataUnionSchema = `
CREATE TABLE IF NOT EXISTS series_source_bindings (
  series_id TEXT NOT NULL REFERENCES series(id) ON DELETE CASCADE,
  source_id TEXT NOT NULL,
  entry_id TEXT NOT NULL,
  match_origin TEXT NOT NULL DEFAULT 'auto',
  confidence REAL NOT NULL DEFAULT 0,
  source_generation TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(series_id, source_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_series_source_bindings_src_gen ON series_source_bindings(source_id, source_generation);

CREATE TABLE IF NOT EXISTS book_source_bindings (
  book_id TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE,
  source_id TEXT NOT NULL,
  series_entry_id TEXT NOT NULL DEFAULT '',
  entry_id TEXT NOT NULL,
  match_origin TEXT NOT NULL DEFAULT 'auto',
  confidence REAL NOT NULL DEFAULT 0,
  source_generation TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(book_id, source_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_book_source_bindings_src_gen ON book_source_bindings(source_id, source_generation);

CREATE TABLE IF NOT EXISTS series_cover_candidates (
  series_id TEXT NOT NULL REFERENCES series(id) ON DELETE CASCADE,
  source_id TEXT NOT NULL,
  entry_id TEXT NOT NULL DEFAULT '',
  cover_kind TEXT NOT NULL,
  cover_ref TEXT NOT NULL,
  is_nsfw INTEGER NOT NULL DEFAULT 0,
  nsfw_known INTEGER NOT NULL DEFAULT 1,
  rank_order INTEGER NOT NULL DEFAULT 0,
  source_generation TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(series_id, source_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_series_cover_candidates_src_gen ON series_cover_candidates(source_id, source_generation);
CREATE INDEX IF NOT EXISTS idx_series_cover_candidates_ref ON series_cover_candidates(cover_kind, cover_ref);

CREATE TABLE IF NOT EXISTS book_cover_candidates (
  book_id TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE,
  source_id TEXT NOT NULL,
  series_entry_id TEXT NOT NULL DEFAULT '',
  entry_id TEXT NOT NULL DEFAULT '',
  cover_kind TEXT NOT NULL,
  cover_ref TEXT NOT NULL,
  is_nsfw INTEGER NOT NULL DEFAULT 0,
  nsfw_known INTEGER NOT NULL DEFAULT 1,
  rank_order INTEGER NOT NULL DEFAULT 0,
  source_generation TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(book_id, source_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_book_cover_candidates_src_gen ON book_cover_candidates(source_id, source_generation);
CREATE INDEX IF NOT EXISTS idx_book_cover_candidates_ref ON book_cover_candidates(cover_kind, cover_ref);

CREATE TABLE IF NOT EXISTS library_metadata_chain_state (
  library_id TEXT PRIMARY KEY NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
  chain_version TEXT NOT NULL DEFAULT '',
  applied_chain_version TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);
`

const unionBookMetadataDirtySchema = `
CREATE TABLE IF NOT EXISTS union_book_metadata_dirty (
 library_id TEXT NOT NULL,
 book_id TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE,
 previous_source TEXT,
 previous_size INTEGER,
 PRIMARY KEY(library_id, book_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_union_book_metadata_dirty_book ON union_book_metadata_dirty(book_id);

CREATE TRIGGER IF NOT EXISTS trg_union_book_metadata_mapping_insert AFTER INSERT ON union_book_sources
WHEN EXISTS (SELECT 1 FROM book_metadata WHERE book_id=NEW.source_book_id) BEGIN
 INSERT INTO union_book_metadata_dirty(library_id,book_id)
 SELECT library_id,id FROM books WHERE id=NEW.union_book_id ON CONFLICT DO NOTHING;
END;

CREATE TRIGGER IF NOT EXISTS trg_union_book_metadata_mapping_delete AFTER DELETE ON union_book_sources
WHEN NOT EXISTS (
 SELECT 1 FROM union_book_sources WHERE union_book_id=OLD.union_book_id AND priority<OLD.priority
) BEGIN
 INSERT INTO union_book_metadata_dirty(library_id,book_id,previous_source,previous_size)
 SELECT library_id,id,OLD.source_book_id,OLD.source_size FROM books WHERE id=OLD.union_book_id
 ON CONFLICT(library_id,book_id) DO UPDATE SET
  previous_size=excluded.previous_size,previous_source=excluded.previous_source
 WHERE union_book_metadata_dirty.previous_source IS NULL;
END;
`
