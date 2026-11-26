package extmeta

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSQLiteBlobCoverRefEncodeDecode(t *testing.T) {
	t.Parallel()

	ref := EncodeSQLiteBlobCoverRef("src-1", SQLiteBlobKindKomgaSeries, "0ABC/123")
	if ref != "sqlite-blob://src-1/komga-series/0ABC%2F123" {
		t.Fatalf("unexpected encoded ref: %q", ref)
	}
	sid, kind, rid, ok := ParseSQLiteBlobCoverRef(ref)
	if !ok || sid != "src-1" || kind != SQLiteBlobKindKomgaSeries || rid != "0ABC/123" {
		t.Fatalf("ParseSQLiteBlobCoverRef failed: ok=%v sid=%q kind=%q rid=%q", ok, sid, kind, rid)
	}

	for _, bad := range []string{
		"",
		"https://example.test/cover.jpg",
		"sqlite-blob://src/invalid-kind/123",
		"sqlite-blob://../komga-series/123",
		"sqlite-blob://src/komga-book/",
		"sqlite-blob://src/komga-book",
	} {
		if _, _, _, ok := ParseSQLiteBlobCoverRef(bad); ok {
			t.Errorf("expected invalid blob ref %q to fail", bad)
		}
	}
}

func TestKomgaSQLiteReadOnlyExtractionWALAndThumbnails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "komga.database.sqlite")

	validJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
	validPNG := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n', 0, 0, 0, 1}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE SERIES (
  ID TEXT PRIMARY KEY,
  NAME TEXT NOT NULL,
  URL TEXT NOT NULL,
  ONESHOT INTEGER NOT NULL DEFAULT 0,
  DELETED INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE SERIES_METADATA (
  SERIES_ID TEXT PRIMARY KEY,
  STATUS TEXT NOT NULL DEFAULT 'ONGOING',
  TITLE TEXT NOT NULL DEFAULT '',
  TITLE_SORT TEXT NOT NULL DEFAULT '',
  SUMMARY TEXT NOT NULL DEFAULT '',
  READING_DIRECTION TEXT,
  PUBLISHER TEXT NOT NULL DEFAULT '',
  AGE_RATING INTEGER,
  LANGUAGE TEXT NOT NULL DEFAULT '',
  TOTAL_BOOK_COUNT INTEGER
);
CREATE TABLE SERIES_METADATA_GENRE (SERIES_ID TEXT NOT NULL, GENRE TEXT NOT NULL);
CREATE TABLE SERIES_METADATA_TAG (SERIES_ID TEXT NOT NULL, TAG TEXT NOT NULL);
CREATE TABLE SERIES_METADATA_LINK (SERIES_ID TEXT NOT NULL, LABEL TEXT NOT NULL, URL TEXT NOT NULL);
CREATE TABLE SERIES_METADATA_SHARING (SERIES_ID TEXT NOT NULL, LABEL TEXT NOT NULL);
CREATE TABLE SERIES_METADATA_ALTERNATE_TITLE (SERIES_ID TEXT NOT NULL, LABEL TEXT NOT NULL, TITLE TEXT NOT NULL);
CREATE TABLE BOOK (
  ID TEXT PRIMARY KEY,
  NAME TEXT NOT NULL,
  URL TEXT NOT NULL,
  SERIES_ID TEXT NOT NULL,
  DELETED INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE BOOK_METADATA (
  BOOK_ID TEXT PRIMARY KEY,
  TITLE TEXT NOT NULL DEFAULT '',
  SUMMARY TEXT NOT NULL DEFAULT '',
  NUMBER TEXT NOT NULL DEFAULT '',
  NUMBER_SORT REAL NOT NULL DEFAULT 0,
  RELEASE_DATE TEXT,
  ISBN TEXT NOT NULL DEFAULT ''
);
CREATE TABLE BOOK_METADATA_AUTHOR (BOOK_ID TEXT NOT NULL, NAME TEXT NOT NULL, ROLE TEXT NOT NULL);
CREATE TABLE BOOK_METADATA_TAG (BOOK_ID TEXT NOT NULL, TAG TEXT NOT NULL);
CREATE TABLE BOOK_METADATA_LINK (BOOK_ID TEXT NOT NULL, LABEL TEXT NOT NULL, URL TEXT NOT NULL);
CREATE TABLE THUMBNAIL_SERIES (
  ID TEXT PRIMARY KEY,
  SERIES_ID TEXT NOT NULL,
  SELECTED INTEGER NOT NULL DEFAULT 0,
  TYPE TEXT NOT NULL DEFAULT 'GENERATED',
  MEDIA_TYPE TEXT NOT NULL DEFAULT '',
  THUMBNAIL BLOB
);
CREATE TABLE THUMBNAIL_BOOK (
  ID TEXT PRIMARY KEY,
  BOOK_ID TEXT NOT NULL,
  SELECTED INTEGER NOT NULL DEFAULT 0,
  TYPE TEXT NOT NULL DEFAULT 'GENERATED',
  MEDIA_TYPE TEXT NOT NULL DEFAULT '',
  THUMBNAIL BLOB
);

INSERT INTO SERIES VALUES
  ('ser-1', 'Frieren', '/comics/Frieren', 0, 0),
  ('ser-nsfw-age', 'Adult Work', '/comics/AdultWork', 0, 0),
  ('ser-nsfw-share', 'Shared Adult', '/comics/NormalFolder', 1, 0),
  ('ser-deleted', 'Deleted Series', '/comics/Deleted', 0, 1);

INSERT INTO SERIES_METADATA VALUES
  ('ser-1', 'ONGOING', '葬送的芙莉莲', 'Frieren', 'Magic journey', 'RIGHT_TO_LEFT', '小学馆', 13, 'zh', 13),
  ('ser-nsfw-age', 'ENDED', 'R18 Work', 'R18 Work', '', 'RIGHT_TO_LEFT', '', 18, 'ja', NULL),
  ('ser-nsfw-share', 'ENDED', 'Shared Work', 'Shared Work', '', NULL, '', NULL, 'ja', 1);

INSERT INTO SERIES_METADATA_GENRE VALUES ('ser-1', 'Fantasy'), ('ser-1', 'Adventure');
INSERT INTO SERIES_METADATA_TAG VALUES ('ser-1', 'elf'), ('ser-1', 'magic');
INSERT INTO SERIES_METADATA_LINK VALUES ('ser-1', 'Bangumi', 'https://bgm.tv/subject/305025');
INSERT INTO SERIES_METADATA_ALTERNATE_TITLE VALUES ('ser-1', '日文原名', '葬送のフリーレン');
INSERT INTO SERIES_METADATA_SHARING VALUES ('ser-nsfw-share', 'nsfw');

INSERT INTO BOOK VALUES
  ('bk-2', 'Frieren Vol.02.cbz', '/comics/Frieren/Frieren Vol.02.cbz', 'ser-1', 0),
  ('bk-1', 'Frieren Vol.01.cbz', '/comics/Frieren/Frieren Vol.01.cbz', 'ser-1', 0),
  ('bk-nsfw', 'OneShot.cbz', '/comics/NormalFolder/OneShot.cbz', 'ser-nsfw-share', 0);

INSERT INTO BOOK_METADATA VALUES
  ('bk-2', '葬送的芙莉莲 02', 'Vol 2 summary', '2', 2.0, '2020-10-16', '978-4098501816'),
  ('bk-1', '葬送的芙莉莲 01', 'Vol 1 summary', '1', 1.0, '2020-08-18', '978-4098501809'),
  ('bk-nsfw', 'OneShot', '', '1', 1.0, '2022-01-01', '');

INSERT INTO BOOK_METADATA_AUTHOR VALUES
  ('bk-1', '山田钟人', 'writer'),
  ('bk-1', '阿部司', 'penciller'),
  ('bk-2', '山田钟人', 'writer');
INSERT INTO BOOK_METADATA_TAG VALUES ('bk-1', 'first-volume');
INSERT INTO BOOK_METADATA_LINK VALUES ('bk-1', 'Bangumi', 'https://bgm.tv/subject/311001');
`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO THUMBNAIL_SERIES VALUES
  ('ts-unselected', 'ser-1', 0, 'GENERATED', 'image/png', ?),
  ('ts-selected', 'ser-1', 1, 'USER_UPLOADED', 'image/jpeg', ?)`, validPNG, validJPEG); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO THUMBNAIL_BOOK VALUES
  ('tb-1', 'bk-1', 1, 'GENERATED', 'image/png', ?)`, validPNG); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Clean DB has no WAL warning.
	if hasWAL, walSize := DetectSQLiteWALWarning(dbPath); hasWAL || walSize != 0 {
		t.Fatalf("expected no WAL warning on checkpointed DB, got hasWAL=%v size=%d", hasWAL, walSize)
	}

	// ProbeSQLiteFile auto-detects komga_sqlite_v1.
	probe, err := ProbeSQLiteFile(ctx, dbPath)
	if err != nil {
		t.Fatalf("ProbeSQLiteFile(komga): %v", err)
	}
	if probe.Format != FormatKomgaSQLite || probe.EntryCount != 4 || probe.BookCount != 3 || !probe.HasCoverBlobs || probe.WALWarning {
		t.Fatalf("unexpected Komga probe result: %+v", probe)
	}

	// Read-only permissions check: opening & reading must never modify the file.
	if err := os.Chmod(dbPath, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dbPath, 0o600) })

	roDB, err := openKomgaReadOnlySQLite(dbPath)
	if err != nil {
		t.Fatalf("openKomgaReadOnlySQLite: %v", err)
	}
	defer roDB.Close()

	ser1, err := ReadKomgaSeries(ctx, roDB, "ser-1")
	if err != nil {
		t.Fatalf("ReadKomgaSeries(ser-1): %v", err)
	}
	if ser1.Title != "葬送的芙莉莲" || ser1.ReadingDirection != "RIGHT_TO_LEFT" || ser1.Publisher != "小学馆" ||
		ser1.AgeRating == nil || *ser1.AgeRating != 13 || ser1.TotalBookCount == nil || *ser1.TotalBookCount != 13 ||
		ser1.ReleaseDate != "2020-08-18" || ser1.NSFW || ser1.SelectedThumbnailID != "ts-selected" {
		t.Fatalf("unexpected ser-1 fields: %+v", ser1)
	}
	if len(ser1.Authors) != 2 || len(ser1.Books) != 2 || ser1.Books[0].KomgaBookID != "bk-1" || ser1.Books[0].ISBN != "978-4098501809" || ser1.Books[0].SelectedThumbnailID != "tb-1" {
		t.Fatalf("unexpected ser-1 authors/books: authors=%+v books=%+v", ser1.Authors, ser1.Books)
	}

	allSeries, err := ReadKomgaAllSeries(ctx, roDB)
	if err != nil {
		t.Fatalf("ReadKomgaAllSeries: %v", err)
	}
	if len(allSeries) != 3 { // ser-deleted is filtered out
		t.Fatalf("expected 3 non-deleted series, got %d", len(allSeries))
	}
	for _, s := range allSeries {
		switch s.KomgaSeriesID {
		case "ser-nsfw-age", "ser-nsfw-share":
			if !s.NSFW {
				t.Errorf("expected %s to be conservatively marked NSFW=true", s.KomgaSeriesID)
			}
		}
	}

	// Lazy BLOB extraction for both series and book thumbnails.
	sBlob, sMIME, err := ReadKomgaThumbnailBLOB(ctx, dbPath, SQLiteBlobKindKomgaSeries, "ts-selected")
	if err != nil || sMIME != "image/jpeg" || !bytes.Equal(sBlob, validJPEG) {
		t.Fatalf("ReadKomgaThumbnailBLOB series failed: mime=%q len=%d err=%v", sMIME, len(sBlob), err)
	}
	bBlob, bMIME, err := ReadKomgaThumbnailBLOB(ctx, dbPath, SQLiteBlobKindKomgaBook, "tb-1")
	if err != nil || bMIME != "image/png" || !bytes.Equal(bBlob, validPNG) {
		t.Fatalf("ReadKomgaThumbnailBLOB book failed: mime=%q len=%d err=%v", bMIME, len(bBlob), err)
	}
	if _, _, err := ReadKomgaThumbnailBLOB(ctx, dbPath, SQLiteBlobKindKomgaSeries, "missing"); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("expected ErrEntryNotFound for missing thumb, got %v", err)
	}

	// Verify >16 MiB BLOB OOM guard rejects oversized thumbnail without OOM.
	oversizePath := filepath.Join(tmpDir, "oversize.sqlite")
	odb, err := sql.Open("sqlite", oversizePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := odb.Exec(`
CREATE TABLE THUMBNAIL_SERIES (ID TEXT PRIMARY KEY, SERIES_ID TEXT, MEDIA_TYPE TEXT, THUMBNAIL BLOB);
INSERT INTO THUMBNAIL_SERIES VALUES ('huge', 's1', 'image/jpeg', zeroblob(16*1024*1024 + 1));
`); err != nil {
		t.Fatal(err)
	}
	_ = odb.Close()
	if _, _, err := ReadKomgaThumbnailBLOB(ctx, oversizePath, SQLiteBlobKindKomgaSeries, "huge"); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("expected ErrInvalidImage for >16 MiB thumbnail BLOB, got %v", err)
	}

	// Non-empty -wal file triggers DetectSQLiteWALWarning + ProbeSQLiteFile WALWarning.
	walPath := dbPath + "-wal"
	if err := os.WriteFile(walPath, []byte("uncheckpointed-wal-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	hasWAL, walSize := DetectSQLiteWALWarning(dbPath)
	if !hasWAL || walSize <= 0 {
		t.Fatalf("expected WAL warning when non-empty -wal exists, got hasWAL=%v size=%d", hasWAL, walSize)
	}
	walProbe, err := ProbeSQLiteFile(ctx, dbPath)
	if err != nil || !walProbe.WALWarning || walProbe.WALSizeBytes != walSize {
		t.Fatalf("expected ProbeSQLiteFile to surface WAL warning, got %+v err=%v", walProbe, err)
	}
	// Ensure no -shm side effect file was created when opening with orphan -wal.
	if _, err := os.Stat(dbPath + "-shm"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected zero disk writes (no -shm created) for orphan -wal, stat err=%v", err)
	}
}

func TestOpenMetaV1V2CompatibilityBooksAndProbeFormats(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tmpDir := t.TempDir()

	validJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
	validPNG := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n', 0, 0, 0, 1}

	// 1. Legacy v1 DB (strictly original 12 columns, no books table) must load seamlessly.
	v1Path := filepath.Join(tmpDir, "legacy_v1.sqlite")
	v1DB, err := sql.Open("sqlite", v1Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v1DB.Exec(`
CREATE TABLE entries (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  title_alt TEXT DEFAULT '',
  authors TEXT DEFAULT '[]',
  summary TEXT DEFAULT '',
  tags TEXT DEFAULT '[]',
  category TEXT DEFAULT '',
  cover_url TEXT DEFAULT '',
  rating REAL DEFAULT 0,
  is_nsfw INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER DEFAULT 0,
  extra_json TEXT DEFAULT '{}'
);
INSERT INTO entries VALUES('e1','Old Work','Old Alt','["Author 1"]','Sum','["tag1"]','Manga','https://example.test/c.jpg',8.5,0,1700000000,'{}');
`); err != nil {
		t.Fatal(err)
	}
	_ = v1DB.Close()

	p1, err := ProbeSQLiteFile(ctx, v1Path)
	if err != nil {
		t.Fatalf("ProbeSQLiteFile(legacy_v1): %v", err)
	}
	if p1.Format != SchemaMangaAlphaMetaV1 || p1.HasBooksTable || p1.HasExtendedCols || p1.EntryCount != 1 {
		t.Fatalf("unexpected legacy v1 probe: %+v", p1)
	}
	roV1, err := openReadOnlySQLite(v1Path)
	if err != nil {
		t.Fatal(err)
	}
	defer roV1.Close()
	if schema, err := DetectSchema(ctx, roV1, SchemaMangaAlphaMetaV2); err != nil || schema != SchemaMangaAlphaMetaV1 {
		t.Fatalf("expected DetectSchema with mangaalpha_meta_v2 alias to succeed, got schema=%q err=%v", schema, err)
	}
	e1, err := getEntryFromUpstream(ctx, roV1, "src-v1", SchemaMangaAlphaMetaV1, "e1")
	if err != nil || e1.Title != "Old Work" || e1.HasCoverBlob {
		t.Fatalf("unexpected legacy v1 entry: %+v err=%v", e1, err)
	}
	if books, err := ReadOpenOfflineBooks(ctx, roV1, "e1"); err != nil || len(books) != 0 {
		t.Fatalf("expected nil books on legacy v1 without error, got %+v err=%v", books, err)
	}

	// 2. Extended v1/v2 DB with optional columns, books table, and cover BLOBs.
	v2Path := filepath.Join(tmpDir, "extended_v2.sqlite")
	v2DB, err := sql.Open("sqlite", v2Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v2DB.Exec(`
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO meta VALUES ('schema', 'mangaalpha_meta_v2');
CREATE TABLE entries (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  title_alt TEXT DEFAULT '',
  authors TEXT DEFAULT '[]',
  summary TEXT DEFAULT '',
  tags TEXT DEFAULT '[]',
  category TEXT DEFAULT '',
  cover_url TEXT DEFAULT '',
  rating REAL DEFAULT 0,
  is_nsfw INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER DEFAULT 0,
  extra_json TEXT DEFAULT '{}',
  status TEXT DEFAULT '',
  title_sort TEXT DEFAULT '',
  publisher TEXT DEFAULT '',
  language TEXT DEFAULT '',
  release_date TEXT DEFAULT '',
  total_book_count INTEGER,
  age_rating INTEGER,
  reading_direction TEXT DEFAULT '',
  links_json TEXT DEFAULT '[]',
  sharing_labels_json TEXT DEFAULT '[]',
  cover_blob BLOB
);
CREATE TABLE books (
  series_entry_id TEXT NOT NULL,
  book_key TEXT NOT NULL,
  file_name TEXT DEFAULT '',
  number TEXT DEFAULT '',
  number_sort REAL,
  title TEXT DEFAULT '',
  summary TEXT DEFAULT '',
  release_date TEXT DEFAULT '',
  authors_json TEXT DEFAULT '[]',
  tags_json TEXT DEFAULT '[]',
  isbn TEXT DEFAULT '',
  links_json TEXT DEFAULT '[]',
  cover_url TEXT DEFAULT '',
  cover_blob BLOB,
  PRIMARY KEY (series_entry_id, book_key)
);
`); err != nil {
		t.Fatal(err)
	}
	if _, err := v2DB.Exec(`
INSERT INTO entries VALUES(
  's1','水星领航员','ARIA','["天野梢"]','Neo Venezia','["治愈"]','Manga','',9.5,0,1700000000,'{}',
  'ENDED','Aria','マッグガーデン','ja','2002-10-10',12,13,'RIGHT_TO_LEFT',
  '[{"label":"Bangumi","url":"https://bgm.tv/subject/539"}]','["classic"]', ?
)`, validJPEG); err != nil {
		t.Fatal(err)
	}
	if _, err := v2DB.Exec(`
INSERT INTO books VALUES(
  's1','vol-1','ARIA Vol.01.cbz','1',1.0,'ARIA 第1卷','First volume','2002-10-10',
  '[{"name":"天野梢","role":"writer"},{"name":"天野梢","role":"penciller"}]',
  '["debut"]','978-4901926126','[{"label":"Bangumi","url":"https://bgm.tv/subject/1001"}]','', ?
)`, validPNG); err != nil {
		t.Fatal(err)
	}
	_ = v2DB.Close()

	p2, err := ProbeSQLiteFile(ctx, v2Path)
	if err != nil {
		t.Fatalf("ProbeSQLiteFile(extended_v2): %v", err)
	}
	if p2.Format != FormatOpenV2 || !p2.HasBooksTable || !p2.HasExtendedCols || !p2.HasCoverBlobs || p2.EntryCount != 1 || p2.BookCount != 1 {
		t.Fatalf("unexpected extended v2 probe: %+v", p2)
	}
	roV2, err := openReadOnlySQLite(v2Path)
	if err != nil {
		t.Fatal(err)
	}
	defer roV2.Close()

	e2, err := getEntryFromUpstream(ctx, roV2, "src-v2", FormatOpenV2, "s1")
	if err != nil {
		t.Fatalf("getEntryFromUpstream(v2): %v", err)
	}
	if e2.Status != "ENDED" || e2.TitleSort != "Aria" || e2.Publisher != "マッグガーデン" ||
		e2.Language != "ja" || e2.ReleaseDate != "2002-10-10" || e2.ReadingDirection != "RIGHT_TO_LEFT" ||
		e2.TotalBookCount == nil || *e2.TotalBookCount != 12 || e2.AgeRating == nil || *e2.AgeRating != 13 ||
		len(e2.Links) != 1 || len(e2.SharingLabels) != 1 || !e2.HasCoverBlob ||
		e2.CoverURL != "sqlite-blob://src-v2/open-entry/s1" {
		t.Fatalf("unexpected extended v2 entry: %+v", e2)
	}

	books, err := ReadOpenOfflineBooks(ctx, roV2, "s1")
	if err != nil || len(books) != 1 {
		t.Fatalf("ReadOpenOfflineBooks failed: books=%+v err=%v", books, err)
	}
	if books[0].BookKey != "vol-1" || books[0].ISBN != "978-4901926126" || !books[0].HasCoverBlob ||
		len(books[0].Authors) != 2 || books[0].NumberSort == nil || *books[0].NumberSort != 1.0 {
		t.Fatalf("unexpected open book entry: %+v", books[0])
	}

	eb, eMIME, err := ReadOpenCoverBLOB(ctx, v2Path, SQLiteBlobKindOpenEntry, "s1")
	if err != nil || eMIME != "image/jpeg" || !bytes.Equal(eb, validJPEG) {
		t.Fatalf("ReadOpenCoverBLOB entry failed: mime=%q err=%v", eMIME, err)
	}
	bb, bMIME, err := ReadOpenCoverBLOB(ctx, v2Path, SQLiteBlobKindOpenBook, "vol-1")
	if err != nil || bMIME != "image/png" || !bytes.Equal(bb, validPNG) {
		t.Fatalf("ReadOpenCoverBLOB book failed: mime=%q err=%v", bMIME, err)
	}

	// 3. ProbeSQLiteFile detects tag_translation_text_v1 (.json.gz) and rejects unknown payloads.
	dictPath := filepath.Join(tmpDir, "db.text.json.gz")
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write([]byte(`{"data":[{"namespace":"female","data":{"glasses":{"name":"眼镜"}}}]}`))
	_ = gw.Close()
	if err := os.WriteFile(dictPath, gzBuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	pDict, err := ProbeSQLiteFile(ctx, dictPath)
	if err != nil || pDict.Format != SchemaTagTranslationTextV1 || pDict.TagCount != 1 {
		t.Fatalf("ProbeSQLiteFile(tagdict) = %+v, err=%v", pDict, err)
	}

	badPath := filepath.Join(tmpDir, "unknown.bin")
	_ = os.WriteFile(badPath, []byte(`{"schema":"mangaalpha-repo-v1","sources":[]}`), 0o600)
	if _, err := ProbeSQLiteFile(ctx, badPath); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("expected ErrUnsupportedSchema for repo JSON, got %v", err)
	}
}

func TestReadOnlySQLiteRealWAL(t *testing.T) {
	ctx := t.Context()
	liveDir := t.TempDir()
	livePath := filepath.Join(liveDir, "catalog.sqlite")
	writer, err := sql.Open("sqlite", livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err := writer.ExecContext(ctx, `
PRAGMA journal_mode=WAL;
PRAGMA wal_autocheckpoint=0;
CREATE TABLE checkpointed(id INTEGER PRIMARY KEY);
PRAGMA wal_checkpoint(TRUNCATE);
CREATE TABLE wal_only(id INTEGER PRIMARY KEY);
INSERT INTO wal_only VALUES(42);`); err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		bytes, err := os.ReadFile(livePath + suffix)
		if err != nil {
			t.Fatal(err)
		}
		files[suffix] = bytes
	}
	// Use an independent inode for the reader, as in a separate Komga
	// process. The SQLite VFS shares SHM mappings for in-process connections.
	readDir := t.TempDir()
	readPath := filepath.Join(readDir, "catalog.sqlite")
	for suffix, content := range files {
		if err := os.WriteFile(readPath+suffix, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(readDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(readDir, 0o700)
	readOnly, err := openKomgaReadOnlySQLite(readPath)
	if err != nil {
		t.Fatal(err)
	}
	var id int
	err = readOnly.QueryRowContext(ctx, "SELECT id FROM wal_only").Scan(&id)
	_ = readOnly.Close()
	if err != nil || id != 42 {
		t.Fatalf("WAL-only schema and row unavailable: id=%d err=%v", id, err)
	}
	for suffix, before := range files {
		if after, err := os.ReadFile(readPath + suffix); err != nil || !bytes.Equal(before, after) {
			t.Fatalf("read-only connection changed %q: %v", suffix, err)
		}
	}

	// A copied WAL with no SHM must fall back to the checkpointed main file
	// without trying to create an index next to it.
	copyDir := t.TempDir()
	copyPath := filepath.Join(copyDir, "catalog.sqlite")
	for _, suffix := range []string{"", "-wal"} {
		if err := os.WriteFile(copyPath+suffix, files[suffix], 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(copyDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(copyDir, 0o700)
	fallback, checkpointedOnly, err := openKomgaSQLiteSnapshot(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !checkpointedOnly {
		t.Error("checkpointed fallback did not report unread pending WAL")
	}
	tables, err := listSQLiteTables(ctx, fallback)
	_ = fallback.Close()
	if err != nil || !tables["checkpointed"] || tables["wal_only"] {
		t.Fatalf("wrong checkpointed snapshot: tables=%v err=%v", tables, err)
	}
	if _, err := os.Stat(copyPath + "-shm"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readonly reader created -shm: %v", err)
	}
	if warned, size := DetectSQLiteWALWarning(copyPath); !warned || size != int64(len(files["-wal"])) {
		t.Fatalf("missing WAL checkpoint warning: warned=%v size=%d", warned, size)
	}
}

func TestOpenBookCoverKeysWithDelimiters(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "open.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	execManagerFixture(t, db, `
CREATE TABLE entries(id TEXT PRIMARY KEY);
CREATE TABLE books(series_entry_id TEXT, book_key TEXT, cover_blob BLOB, PRIMARY KEY(series_entry_id, book_key));
INSERT INTO entries VALUES ('a:b'),('a'),('a/b'),('literal');
INSERT INTO books VALUES ('a:b','c',?),('a','b:c',?),('a/b','v/01',?),('literal','plain:key',?);`,
		encodeTestPNG(t), encodeTestJPEG(t), encodeTestPNG(t), encodeTestJPEG(t))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, mediaType string }{
		{openBookCoverKey("a:b", "c"), "image/png"},
		{openBookCoverKey("a", "b:c"), "image/jpeg"},
		{openBookCoverKey("a/b", "v/01"), "image/png"},
		{"plain:key", "image/jpeg"},
	} {
		_, mediaType, err := ReadOpenCoverBLOB(t.Context(), dbPath, SQLiteBlobKindOpenBook, tc.key)
		if err != nil || mediaType != tc.mediaType {
			t.Fatalf("cover key %q resolved to %q: %v", tc.key, mediaType, err)
		}
	}
	if _, _, err := ReadOpenCoverBLOB(t.Context(), dbPath, SQLiteBlobKindOpenBook, "a:b:c"); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("ambiguous old compound key accepted: %v", err)
	}
}
