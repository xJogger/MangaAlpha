package database

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestOpenBackfillsLegacyMatchOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-origins.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := store.UpsertLibrary(ctx, Library{ID: "fixture", RootCID: "fixture-root", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	type testRow struct {
		id, lock, extEntry, origin, want string
		subject                          any
		confidence                       float64
	}
	rows := []testRow{
		{id: "manual", subject: 1, confidence: 1, want: "manual"},
		{id: "threshold", subject: 2, confidence: 0.999, want: "manual"},
		{id: "below-threshold", subject: 3, confidence: 0.9989, want: "auto"},
		{id: "automatic", subject: 4, confidence: 0.99, want: "auto"},
		{id: "zero-confidence", subject: 5, want: "auto"},
		{id: "external", subject: 6, confidence: 1, extEntry: "fixture-entry"},
		{id: "unbound", confidence: 1},
		{id: "zero-subject", subject: 0, confidence: 1},
		{id: "negative-subject", subject: -1, confidence: 1},
		{id: "agent", subject: 7, origin: "agent", want: "agent"},
		{id: "mapped", subject: 8, origin: "mapping", want: "mapping"},
		{id: "already-auto", subject: 9, confidence: 1, origin: "auto", want: "auto"},
	}
	for _, column := range []string{
		"status_lock", "title_lock", "title_sort_lock", "summary_lock", "reading_direction_lock",
		"publisher_lock", "age_rating_lock", "language_lock", "total_book_count_lock", "release_date_lock",
		"genres_lock", "tags_lock", "authors_lock", "alternate_titles_lock", "links_lock", "sharing_labels_lock", "nsfw_lock",
	} {
		rows = append(rows, testRow{id: column, lock: column, subject: 10, confidence: 0.99, want: "manual"})
	}
	for _, row := range rows {
		_, err := store.DB().Exec(`
INSERT INTO series(id,library_id,cid,name,relative_path,created_at,updated_at,seen_scan_id)
VALUES(?,'fixture',?,?,?,'','','fixture-scan')`, row.id, row.id, row.id, row.id)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.DB().Exec(`
INSERT INTO series_metadata(series_id,bgm_subject_id,scrape_confidence,ext_entry_id,match_origin,updated_at)
VALUES(?,?,?,?,?,'')`, row.id, row.subject, row.confidence, row.extEntry, row.origin)
		if err != nil {
			t.Fatal(err)
		}
		if row.lock != "" {
			if _, err := store.DB().Exec(fmt.Sprintf("UPDATE series_metadata SET %s=1 WHERE series_id=?", row.lock), row.id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 2; pass++ {
		reopened, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			var origin string
			err := reopened.DB().QueryRow("SELECT match_origin FROM series_metadata WHERE series_id=?", row.id).Scan(&origin)
			if err != nil || origin != row.want {
				t.Errorf("pass %d, %s: match_origin = %q, want %q, err = %v", pass, row.id, origin, row.want, err)
			}
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenMigratesLegacyToV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE libraries (
 id TEXT PRIMARY KEY,
 name TEXT NOT NULL,
 root_cid TEXT NOT NULL UNIQUE,
 enabled INTEGER NOT NULL DEFAULT 1,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 last_scan_started_at TEXT,
 last_scan_completed_at TEXT,
 last_scan_status TEXT NOT NULL DEFAULT 'never',
 last_scan_error TEXT NOT NULL DEFAULT ''
);
INSERT INTO libraries(id,name,root_cid,enabled,created_at,updated_at)
VALUES('l_MTIzNDU','Legacy115','12345',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
CREATE TABLE series (
 id TEXT PRIMARY KEY,
 library_id TEXT NOT NULL,
 cid TEXT NOT NULL,
 name TEXT NOT NULL,
 relative_path TEXT NOT NULL,
 file_modified_at TEXT,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 seen_scan_id TEXT NOT NULL,
 UNIQUE(library_id, cid)
);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, item := range []struct {
		table  string
		column string
	}{
		{"libraries", "one_shot"},
		{"libraries", "provider_type"},
		{"libraries", "webdav_account_id"},
		{"libraries", "root_path"},
		{"series", "one_shot"},
		{"auth_admin", "password_hash"},
		{"auth_sessions", "token_hash"},
		{"auth_api_keys", "key_hash"},
		{"webdav_accounts", "base_url"},
		{"series_metadata", "bgm_subject_id"},
		{"series_metadata", "is_nsfw"},
		{"series_metadata", "nsfw_lock"},
		{"series_genres", "genre"},
		{"series_tags", "tag"},
		{"series_authors", "name"},
		{"bangumi_scrape_runs", "matched_count"},
		{"book_read_progress", "book_id"},
		{"book_read_progress", "page"},
		{"book_read_progress", "read_date"},
		{"book_page_progress", "last_loaded_page"},
		{"client_settings", "allow_unauthenticated"},
		{"zip_indexes", "index_duration_ns"},
		{"series_thumbnails", "generation_duration_ns"},
		{"book_download_stats", "duration_ns"},
		{"maintenance_runs", "operation"},
	} {
		exists, err := columnExists(store.DB(), item.table, item.column)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("%s.%s was not migrated", item.table, item.column)
		}
	}

	lib, err := store.Library(t.Context(), "l_MTIzNDU")
	if err != nil {
		t.Fatalf("read migrated legacy library: %v", err)
	}
	if lib.ProviderType != Provider115 || lib.RootCID != "12345" {
		t.Fatalf("unexpected migrated library: %+v", lib)
	}
}

func TestOpenMigratesEarlyV2SeriesMetadataWithoutNSFWColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "early-v2.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE libraries (
 id TEXT PRIMARY KEY,
 name TEXT NOT NULL,
 provider_type TEXT NOT NULL DEFAULT '115',
 root_cid TEXT NOT NULL UNIQUE,
 webdav_account_id TEXT NOT NULL DEFAULT '',
 root_path TEXT NOT NULL DEFAULT '',
 enabled INTEGER NOT NULL DEFAULT 1,
 one_shot INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 last_scan_started_at TEXT,
 last_scan_completed_at TEXT,
 last_scan_status TEXT NOT NULL DEFAULT 'never',
 last_scan_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE series (
 id TEXT PRIMARY KEY,
 library_id TEXT NOT NULL,
 cid TEXT NOT NULL,
 name TEXT NOT NULL,
 relative_path TEXT NOT NULL,
 one_shot INTEGER NOT NULL DEFAULT 0,
 file_modified_at TEXT,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 seen_scan_id TEXT NOT NULL,
 UNIQUE(library_id, cid)
);
CREATE TABLE series_metadata (
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
 updated_at TEXT NOT NULL
);`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(early-v2.db) failed: %v", err)
	}
	defer store.Close()

	for _, col := range []string{"is_nsfw", "nsfw_lock"} {
		ok, err := columnExists(store.DB(), "series_metadata", col)
		if err != nil || !ok {
			t.Fatalf("expected series_metadata.%s after migration, ok=%v err=%v", col, ok, err)
		}
	}
}

func TestOpenMigratesSparseBookMetadataAndOneShots(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "legacy-books.db")
	old, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.UpsertLibrary(ctx, Library{ID: "legacy", Name: "Legacy", RootCID: "root", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	insertCatalogSeries(t, old, "legacy", "work", "Work", make([]catalogBookTime, 1))
	collection, err := old.CreateCollection(ctx, "Legacy collection", false, []string{"work"})
	if err != nil {
		t.Fatal(err)
	}
	var collectionRowID int64
	if err := old.DB().QueryRowContext(ctx, `SELECT rowid FROM collections WHERE id=?`, collection.ID).Scan(&collectionRowID); err != nil {
		t.Fatal(err)
	}
	if _, err := old.DB().ExecContext(ctx, `
DROP INDEX idx_series_effective_oneshot;
DROP TRIGGER trg_union_book_metadata_mapping_insert;
DROP TABLE book_metadata;
DROP TABLE book_thumbnails;
ALTER TABLE libraries DROP COLUMN oneshots_directory;
ALTER TABLE series DROP COLUMN one_shot_override;
`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		store, err := Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		lib, err := store.LibraryBasic(ctx, "legacy")
		if err != nil || lib.OneshotsDirectory != "" {
			t.Fatalf("legacy library = %+v, %v", lib, err)
		}
		series, err := store.SeriesByID(ctx, "work")
		if err != nil || series.OneShotOverride || series.BooksCount != 1 {
			t.Fatalf("legacy series = %+v, %v", series, err)
		}
		if _, ok, err := store.BookMetadataByID(ctx, "work-book-a"); err != nil || ok {
			t.Fatalf("migration backfilled book metadata: found=%t, err=%v", ok, err)
		}
		var thumbnails, indexes int
		if err := store.DB().QueryRowContext(ctx, `
SELECT (SELECT count(*) FROM book_thumbnails), (SELECT count(*) FROM sqlite_master
 WHERE type='index' AND name IN ('idx_book_metadata_bgm','idx_book_metadata_ext'))`).Scan(&thumbnails, &indexes); err != nil || thumbnails != 0 || indexes != 2 {
			t.Fatalf("migration thumbnails=%d indexes=%d err=%v", thumbnails, indexes, err)
		}
		migrated, err := store.CollectionByRowID(ctx, collectionRowID)
		if err != nil || migrated.ID != collection.ID || len(migrated.SeriesIDs) != 1 || migrated.SeriesIDs[0] != "work" {
			t.Fatalf("legacy collection = %+v, %v", migrated, err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
