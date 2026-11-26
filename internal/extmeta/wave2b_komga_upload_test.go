package extmeta

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/xJogger/fake-komga-115/internal/database"
	_ "modernc.org/sqlite"
)

func encodeTestJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 220, G: 110, B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeTestPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 30, G: 160, B: 230, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func createWave2BKomgaDB(t *testing.T, path string, jpgBytes, pngBytes []byte) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

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
CREATE TABLE SERIES_METADATA_GENRE (
  SERIES_ID TEXT NOT NULL,
  GENRE TEXT NOT NULL
);
CREATE TABLE SERIES_METADATA_TAG (
  SERIES_ID TEXT NOT NULL,
  TAG TEXT NOT NULL
);
CREATE TABLE SERIES_METADATA_LINK (
  SERIES_ID TEXT NOT NULL,
  LABEL TEXT NOT NULL,
  URL TEXT NOT NULL
);
CREATE TABLE SERIES_METADATA_SHARING (
  SERIES_ID TEXT NOT NULL,
  LABEL TEXT NOT NULL
);
CREATE TABLE SERIES_METADATA_ALTERNATE_TITLE (
  SERIES_ID TEXT NOT NULL,
  LABEL TEXT NOT NULL,
  TITLE TEXT NOT NULL
);
CREATE TABLE BOOK (
  ID TEXT PRIMARY KEY,
  SERIES_ID TEXT NOT NULL,
  NAME TEXT NOT NULL,
  URL TEXT NOT NULL,
  NUMBER INTEGER NOT NULL DEFAULT 0
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
CREATE TABLE BOOK_METADATA_AUTHOR (
  BOOK_ID TEXT NOT NULL,
  NAME TEXT NOT NULL,
  ROLE TEXT NOT NULL
);
CREATE TABLE BOOK_METADATA_TAG (
  BOOK_ID TEXT NOT NULL,
  TAG TEXT NOT NULL
);
CREATE TABLE BOOK_METADATA_LINK (
  BOOK_ID TEXT NOT NULL,
  LABEL TEXT NOT NULL,
  URL TEXT NOT NULL
);
CREATE TABLE thumbnail_series (
  ID TEXT PRIMARY KEY,
  SERIES_ID TEXT NOT NULL,
  SELECTED INTEGER NOT NULL DEFAULT 0,
  THUMBNAIL BLOB
);
CREATE TABLE thumbnail_book (
  ID TEXT PRIMARY KEY,
  BOOK_ID TEXT NOT NULL,
  SELECTED INTEGER NOT NULL DEFAULT 0,
  THUMBNAIL BLOB
);
`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`
INSERT INTO SERIES(ID,NAME,URL,ONESHOT,DELETED) VALUES
  ('k-s1','aria-folder','file:///library/Aria%20Official',0,0),
  ('k-s2','secret-anthology','/library/Secret%20Anthology',1,0);
INSERT INTO SERIES_METADATA(SERIES_ID,STATUS,TITLE,TITLE_SORT,SUMMARY,READING_DIRECTION,PUBLISHER,AGE_RATING,LANGUAGE,TOTAL_BOOK_COUNT) VALUES
  ('k-s1','ENDED','水星领航员','Aria','Neo Venezia slice of life','RIGHT_TO_LEFT','マッグガーデン',13,'ja',12),
  ('k-s2','ONGOING','Secret Anthology','Secret Anthology','Adults only anthology','LEFT_TO_RIGHT','Press X',18,'en',1);
INSERT INTO SERIES_METADATA_GENRE VALUES ('k-s1','治愈'),('k-s1','科幻'),('k-s2','Adult');
INSERT INTO SERIES_METADATA_TAG VALUES ('k-s1','classic'),('k-s1','slice of life'),('k-s2','mature');
INSERT INTO SERIES_METADATA_LINK VALUES ('k-s1','Official','https://aria.example.test/series');
INSERT INTO SERIES_METADATA_SHARING VALUES ('k-s1','推荐'),('k-s2','R18');
INSERT INTO SERIES_METADATA_ALTERNATE_TITLE VALUES ('k-s1','EN','ARIA The Masterpiece'),('k-s1','JP','アリア');
INSERT INTO BOOK(ID,SERIES_ID,NAME,URL,NUMBER) VALUES
  ('kb1','k-s1','[GroupA] ARIA Vol.01','/library/Aria%20Official/%5BGroupA%5D%20ARIA%20Vol.01.cbz',1),
  ('kb2','k-s1','ARIA Second Edition','/library/Aria%20Official/ARIA%20Second%20Edition.cbz',2),
  ('kb3','k-s1','[Scan] Aqua Prologue Special [Raw]','/library/Aria%20Official/%5BScan%5D%20Aqua%20Prologue%20Special%20%5BRaw%5D.cbz',3),
  ('kb4','k-s1','Bonus Artbook','/library/Aria%20Official/Bonus%20Artbook.cbz',4),
  ('kb-sec','k-s2','Secret OneShot','/library/Secret%20Anthology/Secret%20OneShot.cbz',1);
INSERT INTO BOOK_METADATA(BOOK_ID,TITLE,SUMMARY,NUMBER,NUMBER_SORT,RELEASE_DATE,ISBN) VALUES
  ('kb1','ARIA 01','First volume summary','1',1.0,'2002-10-10','978-4901926126'),
  ('kb2','ARIA 02','Second volume summary','2',2.0,'2003-03-10','978-4901926386'),
  ('kb3','Aqua Prologue Special','Locked manual test','3',3.0,'2003-06-10','978-4901926500'),
  ('kb4','Evening Gondola Guide','Matched by title similarity','4',4.0,'2004-01-10','978-4901926999'),
  ('kb-sec','Secret OneShot','Restricted summary','1',1.0,'2020-01-01','978-0000000001');
INSERT INTO BOOK_METADATA_AUTHOR VALUES
  ('kb1','天野梢','writer'),
  ('kb1','天野梢','penciller'),
  ('kb2','天野梢','writer'),
  ('kb4','天野梢','writer'),
  ('kb-sec','Restricted Artist','writer');
INSERT INTO BOOK_METADATA_TAG VALUES ('kb1','debut'),('kb2','aqua');
INSERT INTO BOOK_METADATA_LINK VALUES ('kb1','Bangumi','https://bgm.tv/subject/1001');
`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO thumbnail_series(ID,SERIES_ID,SELECTED,THUMBNAIL) VALUES
  ('ts-1','k-s1',1,?),
  ('ts-2','k-s2',1,?)`, jpgBytes, jpgBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO thumbnail_book(ID,BOOK_ID,SELECTED,THUMBNAIL) VALUES
  ('tb-1','kb1',1,?),
  ('tb-2','kb2',1,?),
  ('tb-sec','kb-sec',1,?)`, pngBytes, jpgBytes, pngBytes); err != nil {
		t.Fatal(err)
	}
}

func createWave2BOpenV2DB(t *testing.T, path string, jpgBytes, pngBytes []byte) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
INSERT INTO meta VALUES ('schema', 'mangaalpha_meta_v2');
CREATE TABLE entries (
  id TEXT PRIMARY KEY,
  title TEXT,
  title_alt TEXT,
  authors TEXT,
  summary TEXT,
  tags TEXT,
  category TEXT,
  cover_url TEXT,
  rating REAL,
  is_nsfw INTEGER,
  updated_at INTEGER,
  extra_json TEXT,
  status TEXT,
  title_sort TEXT,
  publisher TEXT,
  language TEXT,
  release_date TEXT,
  total_book_count INTEGER,
  age_rating INTEGER,
  reading_direction TEXT,
  links_json TEXT,
  sharing_labels_json TEXT,
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

	if _, err := db.Exec(`
INSERT INTO entries VALUES(
  's1','银河铁道','Galaxy Express','["松本零士"]','Space train journey','["sci-fi","classic"]','Manga','',9.2,0,1700000000,'{}',
  'ENDED','Ginga Tetsudou','少年画報社','ja','1977-01-01',18,12,'RIGHT_TO_LEFT',
  '[{"label":"Info","url":"https://example.test/galaxy"}]','["masterpiece"]', ?
), (
  's2','Another Work','','["Someone Else"]','Other','[]','Non-H','',8.0,0,1700000000,'{}',
  'ONGOING','Another Work','','ja','2020-01-01',1,10,'LEFT_TO_RIGHT','[]','[]', ?
)`, jpgBytes, pngBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO books VALUES
  ('s2','vol-1','Other Vol.01.cbz','1',1.0,'Collision Key Guard','Summary s2','2020-01-01','[]','[]','978-2222222222','[]','', ?),
  ('s1','vol-1','Galaxy Vol.01.cbz','1',1.0,'Departing Earth','Summary vol 1','1978-01-01','[{"name":"松本零士","role":"writer"}]','["scifi"]','978-1111111111','[{"label":"Ref","url":"https://example.test/v1"}]','', ?)`, jpgBytes, pngBytes); err != nil {
		t.Fatal(err)
	}
}

func seedWave2BBook(t *testing.T, store *database.Store, id, seriesID, libID, name string, numberSort float64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	execManagerFixture(t, store.DB(), `
INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,sha1,number_sort,created_at,updated_at,seen_scan_id)
VALUES(?,?,?,?,?,?,1024,'','sha1fixture',?,?,?,'scan1')`,
		id, seriesID, libID, seriesID+"/"+name, seriesID, name, numberSort, now, now)
}

func TestKomgaSQLiteSyncIndexScrapeBooksAndBlobCoverCache(t *testing.T) {
	ctx := context.Background()
	store, m, root := newTestManager(t, ManagerOptions{})
	var err error
	m.covers, err = NewCoverService(CoverConfig{DataDir: root, RequestsPerSec: 100, Burst: 10})
	if err != nil {
		t.Fatal(err)
	}

	jpgBytes := encodeTestJPEG(t)
	pngBytes := encodeTestPNG(t)
	komgaPath := filepath.Join(root, "komga.database.sqlite")
	createWave2BKomgaDB(t, komgaPath, jpgBytes, pngBytes)

	// Place a non-empty -wal side file to exercise DetectSQLiteWALWarning.
	if err := os.WriteFile(komgaPath+"-wal", []byte("pending-wal-frames"), 0o600); err != nil {
		t.Fatal(err)
	}

	src := addManagerSource(t, m, database.MetadataSource{
		Name:      "Local Komga",
		Kind:      KindOfflineSQLite,
		LocalPath: komgaPath,
	})
	if src.SchemaType != FormatKomgaSQLite {
		t.Fatalf("AddCustomSource auto SchemaType = %q, want %q", src.SchemaType, FormatKomgaSQLite)
	}

	view := m.SourceView(ctx, src)
	if view.SchemaFormat != FormatKomgaSQLite || !view.WALWarning || view.WALWarningBytes <= 0 || view.WALWarningMessage == "" {
		t.Fatalf("unexpected WAL source view: %+v", view)
	}

	syncManagerFixture(t, m, src, false, "success")

	// Zero pre-extraction: syncing must NOT write thumbnail blobs to extmeta cover cache dir.
	extThumbDir := filepath.Join(root, "thumbnails", "extmeta")
	if entries, err := os.ReadDir(extThumbDir); err == nil && len(entries) > 0 {
		t.Fatalf("expected zero pre-extracted covers during sync, found %v", entries)
	}

	// Sidecar lookup must recall all Komga aliases including decoded URL tail ("Aria Official").
	m.mu.RLock()
	sidecar := m.sidecars[src.ID]
	m.mu.RUnlock()
	for _, q := range []string{"水星领航员", "ARIA The Masterpiece", "aria-folder", "Aria Official"} {
		hits, err := sidecar.SearchWithHints(ctx, []string{q}, []string{"天野梢"}, 5)
		if err != nil || len(hits) == 0 || hits[0].EntryID != "k-s1" {
			t.Fatalf("SearchWithHints(%q) = %+v, err=%v", q, hits, err)
		}
		if !strings.HasPrefix(hits[0].CoverURL, "sqlite-blob://"+src.ID+"/komga-series/") {
			t.Fatalf("expected virtual blob cover ref in SearchMatch, got %q", hits[0].CoverURL)
		}
	}

	// Seed MangaAlpha library + two series:
	// 1) 'aria': already bound to Bangumi 539 -> external Komga match MUST preserve BgmSubjectID=539.
	// 2) 'unrecognized-dir': matched purely via cached DB book_metadata (zero archive reads).
	seedMetadataLibrary(t, store, "lib-main", true)
	seedMetadataSeries(t, store, "aria", "lib-main", "Aria Official")
	seedMetadataSeries(t, store, "unrecognized-dir", "lib-main", "Completely Unknown Folder 999")
	execManagerFixture(t, store.DB(), `
INSERT INTO series_metadata(series_id,bgm_subject_id,bgm_cover_url,match_origin,scrape_status,updated_at)
VALUES('aria',539,'https://bgm.example.test/539.jpg','auto','none','2026-01-01T00:00:00Z')`)

	// Seed 4 books in 'aria' exercising all waterfall tiers + locks + manual protection.
	seedWave2BBook(t, store, "mb1", "aria", "lib-main", "Volume 1.cbz", 1.0)                      // Tier 2 vol=1 -> kb1
	seedWave2BBook(t, store, "mb2", "aria", "lib-main", "[OtherScan] ARIA Special [Raw].zip", 99) // Note: unmatched or Tier 1? Let's test Tier 1 + lock + Tier 2!
	// Wait: let's match mb2 to kb3 via Tier 1 (`[Other] Aqua Prologue Special.cbz`), and kb2 via Tier 2 (`Renamed Vol.02.cbz`)!
	execManagerFixture(t, store.DB(), `DELETE FROM books WHERE id='mb2'`)
	seedWave2BBook(t, store, "mb2", "aria", "lib-main", "Renamed Vol.02.cbz", 2.0)                  // Tier 2 vol=2 -> kb2 (locked fields)
	seedWave2BBook(t, store, "mb3", "aria", "lib-main", "[OtherScan] Aqua Prologue Special.cbz", 5) // Tier 1 normalized filename -> kb3 (manual protect)
	seedWave2BBook(t, store, "mb4", "aria", "lib-main", "Evening Gondola Guide.cbz", 8)             // Tier 3 title match -> kb4
	seedWave2BBook(t, store, "u-b1", "unrecognized-dir", "lib-main", "RandomFile.cbz", 1)

	locked := true
	lockedTitle := "My Locked Vol 2"
	lockedISBN := "LOCKED-ISBN"
	if err := store.PatchBookMetadata(ctx, "mb2", database.BookMetadataPatch{
		Title:     &lockedTitle,
		TitleLock: &locked,
		ISBN:      &lockedISBN,
		ISBNLock:  &locked,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBookScrapedMetadata(ctx, "mb3", database.BookScrapedMetadata{
		Title:       "Manual Book Must Stay",
		MatchOrigin: "manual",
	}); err != nil {
		t.Fatal(err)
	}
	// Seed cached ComicInfo hint in DB for unrecognized-dir (no archive file exists on disk!).
	if err := store.UpsertBookScrapedMetadata(ctx, "u-b1", database.BookScrapedMetadata{
		Title:       "ARIA The Masterpiece",
		Authors:     []database.Author{{Name: "天野梢", Role: "writer"}},
		MatchOrigin: "auto",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.SetLibrarySources(ctx, "lib-main", []string{src.ID}); err != nil {
		t.Fatal(err)
	}
	run, err := m.StartMultiScrape(ctx, "lib-main", "incremental", false)
	if err != nil {
		t.Fatalf("StartMultiScrape: %v", err)
	}
	waitForManager(t, func() bool {
		for _, r := range m.ScrapeRuns() {
			if r.ID == run.ID && r.CompletedAt != "" {
				if r.Status != "success" {
					t.Fatalf("scrape status = %+v", r)
				}
				return true
			}
		}
		return false
	})

	// Check series full Komga fields & BgmSubjectID preservation.
	ariaSeries := database.Series{ID: "aria", LibraryID: "lib-main", Name: "Aria Official"}
	sm, err := store.SeriesMetadataByID(ctx, ariaSeries)
	if err != nil {
		t.Fatal(err)
	}
	if sm.BgmSubjectID == nil || *sm.BgmSubjectID != 539 {
		t.Fatalf("BgmSubjectID not preserved across Komga scrape: %+v", sm.BgmSubjectID)
	}
	if sm.ExtSourceID != src.ID || sm.ExtEntryID != "k-s1" ||
		sm.Status != "ENDED" || sm.TitleSort != "Aria" || sm.Publisher != "マッグガーデン" ||
		sm.Language != "ja" || sm.ReadingDirection != "RIGHT_TO_LEFT" ||
		sm.ReleaseDate != "2002-10-10" || sm.TotalBookCount == nil || *sm.TotalBookCount != 12 ||
		sm.AgeRating == nil || *sm.AgeRating != 13 || len(sm.Links) != 1 || len(sm.SharingLabels) != 1 ||
		len(sm.Genres) != 2 || len(sm.Authors) != 2 ||
		sm.ExtCoverURL != "sqlite-blob://"+src.ID+"/komga-series/ts-1" {
		t.Fatalf("incomplete Komga series metadata: %+v", sm)
	}

	// Check book alignment + locks + manual guard.
	bm1, ok1, err := store.BookMetadataByID(ctx, "mb1")
	if err != nil || !ok1 || bm1.ExtEntryID != "kb1" || bm1.ISBN != "978-4901926126" ||
		bm1.ExtCoverURL != "sqlite-blob://"+src.ID+"/komga-book/tb-1" || len(bm1.Authors) != 2 || len(bm1.Links) != 1 {
		t.Fatalf("unexpected mb1 metadata: ok=%v err=%v %+v", ok1, err, bm1)
	}
	bm2, ok2, err := store.BookMetadataByID(ctx, "mb2")
	if err != nil || !ok2 || bm2.ExtEntryID != "kb2" || bm2.Title != lockedTitle || bm2.ISBN != lockedISBN ||
		bm2.Summary != "Second volume summary" || bm2.ExtCoverURL != "sqlite-blob://"+src.ID+"/komga-book/tb-2" {
		t.Fatalf("mb2 did not align unlocked fields or clobbered locks: ok=%v err=%v %+v", ok2, err, bm2)
	}
	bm3, ok3, err := store.BookMetadataByID(ctx, "mb3")
	if err != nil || !ok3 || bm3.Title != "Manual Book Must Stay" || bm3.MatchOrigin != "manual" || bm3.ExtEntryID != "" {
		t.Fatalf("mb3 manual book was overwritten: %+v", bm3)
	}
	bm4, ok4, err := store.BookMetadataByID(ctx, "mb4")
	if err != nil || !ok4 || bm4.ExtEntryID != "kb4" || bm4.Title != "Evening Gondola Guide" {
		t.Fatalf("mb4 Tier-3 title similarity failed: %+v", bm4)
	}

	// Check cached book_metadata hint match without ArchiveMetaReader.
	uMeta, err := store.SeriesMetadataByID(ctx, database.Series{ID: "unrecognized-dir", LibraryID: "lib-main", Name: "Completely Unknown Folder 999"})
	if err != nil || uMeta.ExtEntryID != "k-s1" {
		t.Fatalf("cached book_metadata fallback failed: %+v, err=%v", uMeta, err)
	}

	// Lazy BLOB cover loading & disk cache hit verification.
	adminCtx := database.WithUserScope(ctx, database.UserScope{UserID: "admin", IsAdmin: true, AllowNSFW: true, ShowNSFW: true})
	sfBytes, sfMime, err := m.FetchCover(adminCtx, src.ID, "k-s1", sm.ExtCoverURL)
	if err != nil || sfMime != "image/jpeg" || !bytes.Equal(sfBytes, jpgBytes) {
		t.Fatalf("FetchCover komga-series failed: mime=%q err=%v", sfMime, err)
	}
	cachedData, hit := m.covers.readCachedFromDisk(src.ID, "k-s1", sm.ExtCoverURL)
	if !hit || !cachedData.FromCache || !bytes.Equal(cachedData.Bytes, jpgBytes) {
		t.Fatalf("expected disk cache hit after lazy BLOB fetch, hit=%v", hit)
	}
	bkBytes, bkMime, err := m.FetchCover(adminCtx, src.ID, "kb1", bm1.ExtCoverURL)
	if err != nil || bkMime != "image/png" || !bytes.Equal(bkBytes, pngBytes) {
		t.Fatalf("FetchCover komga-book failed: mime=%q err=%v", bkMime, err)
	}

	// NSFW user restriction must block Komga NSFW series & book BLOB covers.
	safeCtx := database.WithUserScope(ctx, database.UserScope{UserID: "reader", IsAdmin: false, AllowNSFW: false, ShowNSFW: false})
	nsfwSeriesCover := EncodeSQLiteBlobCoverRef(src.ID, SQLiteBlobKindKomgaSeries, "ts-2")
	nsfwBookCover := EncodeSQLiteBlobCoverRef(src.ID, SQLiteBlobKindKomgaBook, "tb-sec")
	if _, _, err := m.FetchCover(safeCtx, src.ID, "k-s2", nsfwSeriesCover); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable for NSFW series cover, got %v", err)
	}
	if _, _, err := m.FetchCover(safeCtx, src.ID, "kb-sec", nsfwBookCover); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable for NSFW book cover, got %v", err)
	}
	if gotBytes, gotMime, err := m.FetchCover(adminCtx, src.ID, "kb-sec", nsfwBookCover); err != nil || gotMime != "image/png" || !bytes.Equal(gotBytes, pngBytes) {
		t.Fatalf("expected admin NSFW book cover fetch to succeed, mime=%q err=%v", gotMime, err)
	}
}

func TestMangaAlphaMetaV2ExtendedColsBooksAndBlobCache(t *testing.T) {
	ctx := context.Background()
	store, m, root := newTestManager(t, ManagerOptions{})
	var err error
	m.covers, err = NewCoverService(CoverConfig{DataDir: root, RequestsPerSec: 100, Burst: 10})
	if err != nil {
		t.Fatal(err)
	}

	jpgBytes := encodeTestJPEG(t)
	pngBytes := encodeTestPNG(t)
	v2Path := filepath.Join(root, "open-v2.sqlite")
	createWave2BOpenV2DB(t, v2Path, jpgBytes, pngBytes)

	src := addManagerSource(t, m, database.MetadataSource{
		Name:      "Open Meta V2",
		Kind:      KindOfflineSQLite,
		LocalPath: v2Path,
	})
	if src.SchemaType != FormatOpenV2 {
		t.Fatalf("detected SchemaType = %q, want %q", src.SchemaType, FormatOpenV2)
	}
	syncManagerFixture(t, m, src, false, "success")

	seedMetadataLibrary(t, store, "open-lib", true)
	seedMetadataSeries(t, store, "galaxy-series", "open-lib", "Galaxy Folder")
	// Single unmatched book (One-shot fallback Tier 3a) aligns cleanly to s1:vol-1.
	seedWave2BBook(t, store, "gb1", "galaxy-series", "open-lib", "Single Archive.cbz", 1)

	saved, err := m.BindSeriesExternal(ctx, "galaxy-series", src.ID, "s1", "manual")
	if err != nil {
		t.Fatalf("BindSeriesExternal(open-v2): %v", err)
	}
	if saved.Status != "ENDED" || saved.TitleSort != "Ginga Tetsudou" || saved.Publisher != "少年画報社" ||
		saved.TotalBookCount == nil || *saved.TotalBookCount != 18 || saved.AgeRating == nil || *saved.AgeRating != 12 ||
		len(saved.Links) != 1 || len(saved.SharingLabels) != 1 ||
		saved.ExtCoverURL != "sqlite-blob://"+src.ID+"/open-entry/s1" {
		t.Fatalf("unexpected open-v2 series metadata: %+v", saved)
	}

	bm, ok, err := store.BookMetadataByID(ctx, "gb1")
	if err != nil || !ok || bm.Title != "Departing Earth" || bm.ISBN != "978-1111111111" ||
		bm.ExtCoverURL != EncodeSQLiteBlobCoverRef(src.ID, SQLiteBlobKindOpenBook, "s1:vol-1") {
		t.Fatalf("unexpected open-v2 aligned book metadata: ok=%v err=%v %+v", ok, err, bm)
	}

	// Cover BLOB fetch for open-entry and open-book must return exact series' BLOB (not s2's colliding vol-1).
	eb, em, err := m.FetchCover(ctx, src.ID, "s1", saved.ExtCoverURL)
	if err != nil || em != "image/jpeg" || !bytes.Equal(eb, jpgBytes) {
		t.Fatalf("FetchCover open-entry failed: mime=%q err=%v", em, err)
	}
	bb, bmime, err := m.FetchCover(ctx, src.ID, "vol-1", bm.ExtCoverURL)
	if err != nil || bmime != "image/png" || !bytes.Equal(bb, pngBytes) {
		t.Fatalf("FetchCover open-book failed (or collided with s2): mime=%q err=%v", bmime, err)
	}
}

func TestImportUploadedSQLiteFormatsAndValidation(t *testing.T) {
	ctx := context.Background()
	_, m, root := newTestManager(t, ManagerOptions{})
	jpgBytes := encodeTestJPEG(t)
	pngBytes := encodeTestPNG(t)

	komgaPath := filepath.Join(root, "komga.sqlite")
	createWave2BKomgaDB(t, komgaPath, jpgBytes, pngBytes)
	komgaRaw, err := os.ReadFile(komgaPath)
	if err != nil {
		t.Fatal(err)
	}

	v2Path := filepath.Join(root, "open-v2.sqlite")
	createWave2BOpenV2DB(t, v2Path, jpgBytes, pngBytes)
	v2Raw, err := os.ReadFile(v2Path)
	if err != nil {
		t.Fatal(err)
	}

	src, err := m.AddCustomSource(ctx, database.MetadataSource{
		Name:    "Upload Target",
		Kind:    KindOfflineSQLite,
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Raw .sqlite upload
	imported, err := m.ImportUploadedSQLite(ctx, src.ID, "komga.sqlite", bytes.NewReader(komgaRaw), nil)
	if err != nil || imported.SchemaType != FormatKomgaSQLite || !imported.Ready || imported.EntryCount != 2 {
		t.Fatalf("raw sqlite import failed: %+v err=%v", imported, err)
	}

	// 2. .sqlite.gz upload (replaces existing source cleanly)
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write(v2Raw)
	_ = gw.Close()
	imported, err = m.ImportUploadedSQLite(ctx, src.ID, "open-v2.sqlite.gz", &gzBuf, nil)
	if err != nil || imported.SchemaType != FormatOpenV2 || imported.EntryCount != 2 {
		t.Fatalf("gz import failed: %+v err=%v", imported, err)
	}

	// 3. .zip upload (skips __MACOSX junk)
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	junk, _ := zw.Create("__MACOSX/._komga.sqlite")
	_, _ = junk.Write([]byte("appledouble"))
	realEntry, _ := zw.Create("export/komga.sqlite")
	_, _ = realEntry.Write(komgaRaw)
	_ = zw.Close()
	imported, err = m.ImportUploadedSQLite(ctx, src.ID, "bundle.zip", &zipBuf, nil)
	if err != nil || imported.SchemaType != FormatKomgaSQLite {
		t.Fatalf("zip import failed: %+v err=%v", imported, err)
	}

	// 4. .sqlite.zst upload
	var zstBuf bytes.Buffer
	zwEnc, err := zstd.NewWriter(&zstBuf)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = zwEnc.Write(v2Raw)
	_ = zwEnc.Close()
	imported, err = m.ImportUploadedSQLite(ctx, src.ID, "open-v2.sqlite.zst", &zstBuf, nil)
	if err != nil || imported.SchemaType != FormatOpenV2 {
		t.Fatalf("zst import failed: %+v err=%v", imported, err)
	}

	// 5. Re-sync an uploaded source without DownloadURL must rebuild sidecar in-place.
	syncManagerFixture(t, m, imported, false, "success")

	// 6. Invalid header upload must fail with ErrUnsupportedSchema and keep existing sidecar working.
	if _, err := m.ImportUploadedSQLite(ctx, src.ID, "bad.sqlite", strings.NewReader("not-sqlite-header"), nil); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("expected ErrUnsupportedSchema for invalid header, got %v", err)
	}
	// 7. Valid SQLite file lacking any recognized metadata tables must also fail cleanly.
	unknownPath := filepath.Join(root, "unknown-schema.sqlite")
	udb, err := sql.Open("sqlite", unknownPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = udb.Exec(`CREATE TABLE unrelated(x INT); INSERT INTO unrelated VALUES (1);`)
	_ = udb.Close()
	unknownRaw, err := os.ReadFile(unknownPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ImportUploadedSQLite(ctx, src.ID, "unknown.sqlite", bytes.NewReader(unknownRaw), nil); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("expected ErrUnsupportedSchema for unknown SQLite tables, got %v", err)
	}
	hits, err := m.sidecars[src.ID].SearchByTitle(ctx, "银河铁道", 5)
	if err != nil || len(hits) == 0 || !slices.ContainsFunc(hits, func(h SearchMatch) bool { return h.EntryID == "s1" }) {
		t.Fatalf("existing sidecar damaged after failed upload: hits=%+v err=%v", hits, err)
	}
}
