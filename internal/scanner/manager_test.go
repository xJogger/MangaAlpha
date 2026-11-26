package scanner

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/id"
	"github.com/xJogger/fake-komga-115/internal/oneonefive"
)

func TestIsComicArchive(t *testing.T) {
	tests := map[string]bool{
		"book.cbz":        true,
		"book.zip":        true,
		"book.cbr":        true,
		"book.RAR":        true,
		"book.part1.rar":  true,
		"book.part01.rar": true,
		"book.part2.rar":  false,
		"book.part10.rar": false,
		"book.r00":        false,
		"book.7z":         false,
	}
	for name, want := range tests {
		if got := isComic(name); got != want {
			t.Fatalf("isComic(%q)=%v want %v", name, got, want)
		}
	}
}

func TestSaveOneShotCreatesOneSeriesPerFile(t *testing.T) {
	store, library, item := scannerTestStore(t, true)
	manager := &Manager{store: store}
	now := time.Now().UTC()
	file := oneonefive.File{
		ID: "file-1", Name: "My Comic.cbz", Size: 42, PickCode: "pick", SHA1: "sha",
		CreatedAt: now.Add(-time.Hour), ModifiedAt: now,
	}
	second := file
	second.ID = "file-2"
	second.PickCode = "pick-2"
	if err := manager.saveOneShots(context.Background(), item, queueDir{
		CID: "parent", Name: "Folder", RelativePath: "Root/Folder", ModifiedAt: now,
	}, []oneonefive.File{file, second}); err != nil {
		t.Fatal(err)
	}

	if err := store.CommitLibraryScan(context.Background(), library.ID, item.runID); err != nil {
		t.Fatal(err)
	}

	series, err := store.SeriesByID(context.Background(), id.OneShotSeries(library.ID, file.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !series.OneShot || series.Name != "My Comic" ||
		series.RelativePath != "Root/Folder/My Comic.cbz" || series.BooksCount != 1 {
		t.Fatalf("unexpected series: %+v", series)
	}
	book, err := store.BookByID(context.Background(), id.Book(library.ID, file.ID))
	if err != nil {
		t.Fatal(err)
	}
	if book.Name != file.Name || book.ParentCID != "parent" || book.SeriesID != series.ID {
		t.Fatalf("unexpected book: %+v", book)
	}
	secondSeries, err := store.SeriesByID(context.Background(), id.OneShotSeries(library.ID, second.ID))
	if err != nil || secondSeries.Name != series.Name || secondSeries.ID == series.ID {
		t.Fatalf("same-name file was not kept as a distinct series: %+v, %v", secondSeries, err)
	}
}

func TestModeConversionIsStagedUntilSuccessfulCommit(t *testing.T) {
	store, library, item := scannerTestStore(t, true)
	manager := &Manager{store: store}
	ctx := context.Background()
	now := time.Now().UTC()
	oldSeriesID := id.Series(library.ID, "old-folder")
	oldBookID := id.Book(library.ID, "file-1")
	nowText := now.Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO series(id,library_id,cid,name,relative_path,one_shot,created_at,updated_at,seen_scan_id)
VALUES(?,?,?,?,?,0,?,?,?)`,
		oldSeriesID, library.ID, "old-folder", "Old", "Root/Old", nowText, nowText, "old-scan"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO books(
 id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,
 created_at,updated_at,seen_scan_id
) VALUES(?,?,?,?,?,?,?,?,1,?,?,?)`,
		oldBookID, oldSeriesID, library.ID, "file-1", "old-folder", "My Comic.cbz", 42, "pick",
		nowText, nowText, "old-scan"); err != nil {
		t.Fatal(err)
	}
	collection, err := store.CreateCollection(ctx, "Fixture collection", true, []string{oldSeriesID})
	if err != nil {
		t.Fatal(err)
	}
	readList, err := store.CreateReadList(ctx, "Fixture reading list", "", true, []string{oldBookID})
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO series_metadata(series_id,title,title_lock,updated_at) VALUES(?,'Catalog title',1,?)`, []any{oldSeriesID, nowText}},
		{`INSERT INTO series_tags(series_id,tag) VALUES(?,'fixture')`, []any{oldSeriesID}},
		{`INSERT INTO book_read_progress(user_id,book_id,series_id,page,updated_at)
			VALUES('reader-a',?,?,3,?),('reader-b',?,?,8,?)`, []any{oldBookID, oldSeriesID, nowText, oldBookID, oldSeriesID, nowText}},
		{`INSERT INTO book_read_progress_tombstones(user_id,book_id,series_id,deleted_at)
			VALUES('reader-c',?,?,?)`, []any{oldBookID, oldSeriesID, nowText}},
		{`INSERT INTO book_page_progress(user_id,book_id,series_id,last_loaded_page,max_loaded_page,page_count,updated_at)
			VALUES('reader-a',?,?,3,3,12,?)`, []any{oldBookID, oldSeriesID, nowText}},
		{`INSERT INTO zip_indexes(book_id,version,page_count,index_json,created_at,updated_at)
			VALUES(?,'fixture',12,'[]',?,?)`, []any{oldBookID, nowText, nowText}},
		{`INSERT INTO book_metadata(book_id,title) VALUES(?,'Volume title')`, []any{oldBookID}},
		{`INSERT INTO book_thumbnails(book_id,source_version,path,media_type,width,height,size,created_at,updated_at)
			VALUES(?,'fixture','first.jpg','image/jpeg',10,20,30,?,?)`, []any{oldBookID, nowText, nowText}},
	} {
		if _, err := store.DB().ExecContext(ctx, fixture.sql, fixture.args...); err != nil {
			t.Fatalf("seed scan state: %s: %v", fixture.sql, err)
		}
	}
	file := oneonefive.File{
		ID: "file-1", Name: "My Comic.cbz", Size: 42, PickCode: "pick",
		CreatedAt: now, ModifiedAt: now,
	}
	dir := queueDir{CID: "old-folder", RelativePath: "Root/Old", ModifiedAt: now}
	if err := manager.saveOneShots(ctx, item, dir, []oneonefive.File{file}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SeriesByID(ctx, oldSeriesID); err != nil {
		t.Fatalf("old series changed before commit: %v", err)
	}
	if book, err := store.BookByID(ctx, oldBookID); err != nil || book.SeriesID != oldSeriesID {
		t.Fatalf("old book changed before commit: %+v, %v", book, err)
	}

	manager.discardStaging(item)
	if _, err := store.SeriesByID(ctx, oldSeriesID); err != nil {
		t.Fatalf("discard removed old series: %v", err)
	}
	if err := manager.saveOneShots(ctx, item, dir, []oneonefive.File{file}); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitLibraryScan(ctx, library.ID, item.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SeriesByID(ctx, oldSeriesID); err != sql.ErrNoRows {
		t.Fatalf("old series remains after conversion: %v", err)
	}
	newSeries, err := store.SeriesByID(ctx, id.OneShotSeries(library.ID, file.ID))
	if err != nil || !newSeries.OneShot {
		t.Fatalf("new one-shot series: %+v, %v", newSeries, err)
	}
	book, err := store.BookByID(ctx, oldBookID)
	if err != nil || book.SeriesID != newSeries.ID || !book.CreatedAt.Equal(now) || book.PageCount != 12 {
		t.Fatalf("converted book = %+v, %v", book, err)
	}
	meta, ok, err := store.BookMetadataByID(ctx, oldBookID)
	if err != nil || !ok || meta.Title != "Volume title" {
		t.Fatalf("converted book metadata = %+v, %t, %v", meta, ok, err)
	}
	seriesMeta, err := store.SeriesMetadataByID(ctx, newSeries)
	if err != nil || seriesMeta.Title != "Catalog title" || !seriesMeta.TitleLock {
		t.Fatalf("converted series metadata = %+v, %v", seriesMeta, err)
	}
	gotCollection, err := store.CollectionByID(ctx, collection.ID)
	if err != nil || len(gotCollection.SeriesIDs) != 1 || gotCollection.SeriesIDs[0] != newSeries.ID {
		t.Fatalf("converted collection = %+v, %v", gotCollection, err)
	}
	gotReadList, err := store.ReadListByID(ctx, readList.ID)
	if err != nil || len(gotReadList.BookIDs) != 1 || gotReadList.BookIDs[0] != oldBookID {
		t.Fatalf("converted reading list = %+v, %v", gotReadList, err)
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM book_read_progress WHERE series_id=? AND page IN (3,8)`:                 2,
		`SELECT count(*) FROM book_read_progress_tombstones WHERE series_id=? AND user_id='reader-c'`: 1,
		`SELECT count(*) FROM book_page_progress WHERE series_id=? AND max_loaded_page=3`:             1,
		`SELECT count(*) FROM series_tags WHERE series_id=? AND tag='fixture'`:                        1,
	} {
		var count int
		if err := store.DB().QueryRowContext(ctx, query, newSeries.ID).Scan(&count); err != nil || count != want {
			t.Fatalf("%s returned %d, want %d, err=%v", query, count, want, err)
		}
	}
	var cover string
	if err := store.DB().QueryRowContext(ctx, `SELECT path FROM book_thumbnails WHERE book_id=?`, oldBookID).Scan(&cover); err != nil || cover != "first.jpg" {
		t.Fatalf("converted thumbnail = %q, %v", cover, err)
	}
}

func scannerTestStore(t *testing.T, oneShot bool) (*database.Store, database.Library, job) {
	t.Helper()
	store, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	library := database.Library{
		ID: id.Library("root"), Name: "Library", RootCID: "root", Enabled: true, OneShot: oneShot,
	}
	if err := store.UpsertLibrary(context.Background(), library); err != nil {
		t.Fatal(err)
	}
	runID := "scan-run"
	if _, err := store.DB().ExecContext(context.Background(), `
INSERT INTO scan_runs(id,library_id,status,trigger_type,created_at)
VALUES(?,?,'running','test',?)`, runID, library.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	return store, library, job{runID: runID, library: library}
}

func TestMatchesOneshotsDirectoryAndStagesMixedCatalog(t *testing.T) {
	for _, tt := range []struct {
		dir, configured string
		want            bool
	}{
		{"_oneshots", "_oneshots", true},
		{"manga/_ONESHOTS/作者/合集", "_oneshots", true},
		{"manga\\_Oneshots\\nested", "_oneshots", true},
		{"manga/my_oneshots/作者", "_oneshots", false},
		{"manga/_oneshots_extra", "_oneshots", false},
		{"海贼王", "_oneshots", false},
		{"_oneshots", "", false},
		{"", "_oneshots", false},
	} {
		if got := matchesOneshotsDirectory(tt.dir, tt.configured); got != tt.want {
			t.Errorf("matchesOneshotsDirectory(%q,%q)=%t, want %t", tt.dir, tt.configured, got, tt.want)
		}
	}
	store, lib, item := scannerTestStore(t, false)
	manager := &Manager{store: store}
	ctx := t.Context()
	regular := queueDir{CID: "regular", Name: "Regular", RelativePath: "Root/Regular"}
	mixed := queueDir{CID: "mixed", Name: "Author", RelativePath: "Root/_ONESHOTS/Author"}
	scan := func() {
		t.Helper()
		var count counters
		for _, dir := range []queueDir{regular, mixed} {
			files := []oneonefive.File{{ID: dir.CID + "-1", Name: "A.cbz"}, {ID: dir.CID + "-2", Name: "B.cbz"}}
			if err := manager.persistDirComics(ctx, item, dir, files, &count); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.CommitLibraryScan(ctx, lib.ID, item.runID); err != nil {
			t.Fatal(err)
		}
	}
	scan()
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO series_metadata(series_id,title,is_nsfw,updated_at)
VALUES(?,'Old series title',1,?)`, id.Series(lib.ID, mixed.CID), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	item.library.OneshotsDirectory = "_oneshots"
	scan()
	want := map[string]int{
		id.Series(lib.ID, regular.CID):           2,
		id.OneShotSeries(lib.ID, mixed.CID+"-1"): 1,
		id.OneShotSeries(lib.ID, mixed.CID+"-2"): 1,
	}
	for seriesID, volumes := range want {
		series, err := store.SeriesByIDAny(ctx, seriesID, true)
		if err != nil || series.BooksCount != volumes || series.EffectiveOneShot() != (volumes == 1) {
			t.Fatalf("mixed series = %+v, %v", series, err)
		}
		meta, err := store.SeriesMetadataByID(ctx, series)
		if err != nil || meta.IsNSFW != (volumes == 1) || meta.Title != series.Name {
			t.Errorf("mixed series metadata = %+v, %v", meta, err)
		}
	}
}
