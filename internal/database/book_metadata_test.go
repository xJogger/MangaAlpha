package database

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/cjk"
)

func TestBookMetadataLocksBindingsSearchAndOrder(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertLibrary(ctx, Library{ID: "catalog", RootCID: "root", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	insertCatalogSeries(t, store, "catalog", "work", "Work", make([]catalogBookTime, 2))
	const firstID, secondID = "work-book-a", "work-book-b"
	if _, ok, err := store.BookMetadataByID(ctx, firstID); ok || err != nil {
		t.Fatalf("missing row found=%t err=%v", ok, err)
	}

	lock := true
	title, summary, number, date, isbn := "独立卷名", "独立卷简介", "", "2026-01-01", "fixture-isbn"
	sortNumber := 0.5
	authors := []Author{{Name: "独立作者", Role: "writer"}}
	tags := []string{"独立卷标签"}
	links := []Link{{Label: "Catalog", URL: "https://example.test/volume"}}
	before, err := store.shelfRevision(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PatchBookMetadata(ctx, secondID, BookMetadataPatch{
		Title: &title, Summary: &summary, Number: &number, NumberSort: &sortNumber,
		ReleaseDate: &date, ISBN: &isbn, Authors: &authors, Tags: &tags, Links: &links,
		TitleLock: &lock, SummaryLock: &lock, NumberLock: &lock, NumberSortLock: &lock,
		ReleaseDateLock: &lock, ISBNLock: &lock, AuthorsLock: &lock, TagsLock: &lock, LinksLock: &lock,
	}); err != nil {
		t.Fatal(err)
	}
	after, err := store.shelfRevision(ctx, false)
	if err != nil || after.catalog <= before.catalog {
		t.Fatalf("revision before=%+v after=%+v: %v", before, after, err)
	}
	if err := store.SetBookCustomCover(ctx, secondID, "custom.jpg", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBookCoverSource(ctx, secondID, "custom"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBookScrapedMetadata(ctx, secondID, BookScrapedMetadata{
		BgmSubjectID: 12, BgmCoverPath: "bangumi.jpg",
	}); err != nil {
		t.Fatal(err)
	}
	otherSort := 3.0
	if err := store.UpsertBookScrapedMetadata(ctx, secondID, BookScrapedMetadata{
		Title: "new title", Summary: "new summary", Number: "3", NumberSort: &otherSort,
		ReleaseDate: "2026-02-01", ISBN: "other", Authors: []Author{}, Tags: []string{}, Links: []Link{},
		ExtSourceID: "offline", ExtEntryID: "7", ExtCoverPath: "external.jpg",
		SeriesGroup: "group", StoryArc: "arc", StoryArcNumber: "2",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBookScrapedMetadata(ctx, secondID, BookScrapedMetadata{BgmSubjectID: 13}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBookComicInfoSentinel(ctx, secondID, "fixture-v1", "skipped_solid"); err != nil {
		t.Fatal(err)
	}

	ids := make([]string, 401)
	for i := range ids {
		ids[i] = "missing-" + strconv.Itoa(i)
	}
	ids[400] = secondID
	metas, err := store.BookMetadataMap(ctx, ids)
	m := metas[secondID]
	if err != nil || len(metas) != 1 || !m.HasNumber() || m.Number != "" ||
		m.Title != title || m.Summary != summary || m.NumberSort == nil || *m.NumberSort != sortNumber ||
		m.ReleaseDate != date || m.ISBN != isbn || len(m.Authors) != 1 || m.Authors[0] != authors[0] ||
		len(m.Tags) != 1 || m.Tags[0] != tags[0] || len(m.Links) != 1 || m.Links[0] != links[0] {
		t.Fatalf("locked metadata = %+v, %v", m, err)
	}
	if m.BgmSubjectID != 13 || m.BgmCoverPath != "" || m.ExtSourceID != "offline" ||
		m.ExtCoverPath != "external.jpg" || m.CustomCoverPath != "custom.jpg" || m.CoverSource != "custom" ||
		m.ComicInfoStatus != "skipped_solid" || m.ComicInfoVersion != "fixture-v1" ||
		m.SeriesGroup != "group" || m.StoryArc != "arc" || m.StoryArcNumber != "2" {
		t.Fatalf("source slots or ComicInfo = %+v", m)
	}

	for _, term := range []string{title, summary, authors[0].Name, tags[0]} {
		for _, seriesID := range []string{"", "work"} {
			items, total, err := store.BooksPage(ctx, BookQuery{SeriesID: seriesID, Search: term, Size: 10})
			if err != nil || total != 1 || len(items) != 1 || items[0].ID != secondID {
				t.Fatalf("search %q in %q = %+v %d %v", term, seriesID, items, total, err)
			}
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for !store.ftsBooksReady.Load() {
		if time.Now().After(deadline) {
			t.Fatal("book FTS did not become ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !store.ensureSearchFTSFresh(ctx, "b") {
		t.Fatal("book FTS could not synchronize")
	}
	for _, term := range []string{title, summary, authors[0].Name, tags[0]} {
		var found int
		if err := store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM books_search_fts f JOIN books b ON b.rowid=f.rowid WHERE f.books_search_fts MATCH ? AND b.id=?`,
			cjk.FTSQueryPhrase(term), secondID).Scan(&found); err != nil || found != 1 {
			t.Fatalf("book FTS %q = %d, %v", term, found, err)
		}
	}
	newSummary := "变更后的卷简介"
	if err := store.PatchBookMetadata(ctx, secondID, BookMetadataPatch{Summary: &newSummary}); err != nil {
		t.Fatal(err)
	}
	for term, expected := range map[string]int64{summary: 0, newSummary: 1} {
		_, total, err := store.BooksPage(ctx, BookQuery{Search: term, Size: 10})
		if err != nil || total != expected {
			t.Fatalf("search after incremental update %q = %d, %v", term, total, err)
		}
	}
	first, err := store.FirstBookInSeries(ctx, "work")
	if err != nil || first.ID != secondID {
		t.Fatalf("effective first = %+v, %v", first, err)
	}
	next, err := store.NextBookInSeries(ctx, first)
	if err != nil || next.ID != firstID {
		t.Fatalf("effective next = %+v, %v", next, err)
	}
	prev, err := store.PreviousBookInSeries(ctx, next)
	if err != nil || prev.ID != secondID {
		t.Fatalf("effective previous = %+v, %v", prev, err)
	}
	if err := store.ClearBookCustomCover(ctx, secondID); err != nil {
		t.Fatal(err)
	}
	if err := store.PatchBookMetadata(ctx, secondID, BookMetadataPatch{ClearNumberSort: true}); err != nil {
		t.Fatal(err)
	}
	first, err = store.FirstBookInSeries(ctx, "work")
	if err != nil || first.ID != firstID {
		t.Fatalf("first after clearing sort = %+v, %v", first, err)
	}
	absent, _, err := store.BookMetadataByID(ctx, firstID)
	if err != nil || absent.HasTitle() || absent.HasSummary() || absent.HasNumber() ||
		absent.HasReleaseDate() || absent.HasISBN() || absent.HasAuthors() || absent.HasTags() || absent.HasLinks() {
		t.Fatalf("missing fields = %+v, %v", absent, err)
	}
	empty := ""
	noAuthors, noTags, noLinks := []Author{}, []string{}, []Link{}
	if err := store.PatchBookMetadata(ctx, firstID, BookMetadataPatch{
		Title: &empty, Summary: &empty, Number: &empty, ReleaseDate: &empty, ISBN: &empty,
		Authors: &noAuthors, Tags: &noTags, Links: &noLinks,
	}); err != nil {
		t.Fatal(err)
	}
	explicit, _, err := store.BookMetadataByID(ctx, firstID)
	if err != nil || !explicit.HasTitle() || !explicit.HasSummary() || !explicit.HasNumber() ||
		!explicit.HasReleaseDate() || !explicit.HasISBN() || !explicit.HasAuthors() || !explicit.HasTags() || !explicit.HasLinks() {
		t.Fatalf("explicit empty fields = %+v, %v", explicit, err)
	}
	if err := store.PatchBookMetadata(ctx, "missing", BookMetadataPatch{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing book patch = %v", err)
	}
}

func TestSeriesOneShotOverrideTracksBookCount(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertLibrary(ctx, Library{ID: "catalog", RootCID: "root", Enabled: true, OneshotsDirectory: "_Oneshots"}); err != nil {
		t.Fatal(err)
	}
	lib, err := store.LibraryBasic(ctx, "catalog")
	if err != nil || lib.OneshotsDirectory != "_Oneshots" {
		t.Fatalf("library mode = %+v, %v", lib, err)
	}
	insertCatalogSeries(t, store, "catalog", "single", "Work", make([]catalogBookTime, 1))
	insertCatalogSeries(t, store, "catalog", "multi", "Many", make([]catalogBookTime, 2))
	insertCatalogSeries(t, store, "catalog", "file", "File", make([]catalogBookTime, 1))
	if _, err := store.DB().ExecContext(ctx, `UPDATE series SET one_shot=1 WHERE id='file'`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"multi", "file"} {
		if err := store.SetSeriesOneShotOverride(ctx, id, true); !errors.Is(err, ErrInvalidOneShotOverride) {
			t.Fatalf("override for %s = %v", id, err)
		}
	}
	if err := store.SetSeriesOneShotOverride(ctx, "single", true); err != nil {
		t.Fatal(err)
	}
	want := true
	check := func(seriesCount, bookCount int64) {
		t.Helper()
		series, total, err := store.SeriesPage(ctx, SeriesQuery{OneShot: &want, Size: 10})
		if err != nil || total != seriesCount || len(series) != int(seriesCount) {
			t.Fatalf("one-shot series = %+v, total=%d, err=%v", series, total, err)
		}
		for _, item := range series {
			if !item.EffectiveOneShot() {
				t.Fatalf("SQL and model disagree: %+v", item)
			}
		}
		books, total, err := store.BooksPage(ctx, BookQuery{OneShot: &want, Size: 10})
		if err != nil || total != bookCount || len(books) != int(bookCount) {
			t.Fatalf("one-shot books = %+v, total=%d, err=%v", books, total, err)
		}
	}
	check(2, 2)
	if _, err := store.DB().ExecContext(ctx, `UPDATE books SET series_id='single' WHERE id='multi-book-a'`); err != nil {
		t.Fatal(err)
	}
	check(1, 1)
	got, err := store.SeriesByID(ctx, "single")
	if err != nil || !got.OneShotOverride || got.EffectiveOneShot() {
		t.Fatalf("override flag after adding volume = %+v, %v", got, err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE books SET series_id='multi' WHERE id='multi-book-a'`); err != nil {
		t.Fatal(err)
	}
	check(2, 2)
}

func TestSeriesScrapePreservesBangumiAndComicInfoOnlyFillsBlanks(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertLibrary(ctx, Library{ID: "catalog", RootCID: "root", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	insertCatalogSeries(t, store, "catalog", "work", "Work", nil)
	series, err := store.SeriesByID(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	bgm, err := store.ApplyScrapedMetadata(ctx, series, ScrapedMetadata{SubjectID: 12, Title: "Bangumi title", CoverURL: "https://example.test/bgm.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSeriesBgmCoverPath(ctx, bgm.SeriesID, 12, "bgm.jpg"); err != nil {
		t.Fatal(err)
	}
	age, count := 16, 5
	locked := true
	if _, err := store.PatchSeriesMetadata(ctx, series, SeriesMetadataPatch{
		TitleLock: &locked, NSFWLock: &locked, PublisherLock: &locked,
	}); err != nil {
		t.Fatal(err)
	}
	external, err := store.ApplyScrapedMetadata(ctx, series, ScrapedMetadata{
		ExtSourceID: "offline", ExtEntryID: "chapter", ExtCoverPath: "external.jpg",
		Title: "external title", TitleSort: "custom sort", Language: "ja", Status: "ENDED",
		TotalBookCount: &count, AgeRating: &age, ReadingDirection: "RIGHT_TO_LEFT",
		Links:         []Link{{URL: "https://example.test/work"}},
		SharingLabels: []string{"family"}, AlternateTitles: []AlternateTitle{{Title: "alternate"}},
	})
	if err != nil || external.BgmSubjectID == nil || *external.BgmSubjectID != 12 ||
		external.BgmCoverPath != "bgm.jpg" || external.ExtCoverPath != "external.jpg" ||
		external.Title != "Bangumi title" || external.TitleSort != "custom sort" || external.Language != "ja" ||
		external.Status != "ENDED" || external.ReadingDirection != "RIGHT_TO_LEFT" ||
		external.TotalBookCount == nil || *external.TotalBookCount != count || external.AgeRating == nil || *external.AgeRating != age ||
		len(external.Links) != 1 || len(external.SharingLabels) != 1 || len(external.AlternateTitles) != 1 {
		t.Fatalf("external scrape = %+v, %v", external, err)
	}
	if err := store.BackfillSeriesMetadataFromComicInfo(ctx, series.ID, SeriesComicInfoBackfill{
		Title: "archive title", TitleSort: "archive sort", Summary: "archive summary", Publisher: "locked publisher",
		ReleaseDate: "2026-02-01", Genres: []string{"fiction"}, IsNSFW: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.BackfillSeriesMetadataFromComicInfo(ctx, series.ID, SeriesComicInfoBackfill{
		Summary: "different archive summary", ReleaseDate: "2027-02-01",
	}); err != nil {
		t.Fatal(err)
	}
	backfilled, err := store.SeriesMetadataByID(ctx, series)
	if err != nil || backfilled.Title != external.Title || backfilled.TitleSort != external.TitleSort ||
		backfilled.Summary != "archive summary" || backfilled.Publisher != "" || backfilled.ReleaseDate != "2026-02-01" ||
		len(backfilled.Genres) != 1 || backfilled.IsNSFW || !backfilled.NSFWLock ||
		backfilled.BgmSubjectID == nil || *backfilled.BgmSubjectID != 12 || backfilled.ExtCoverPath != "external.jpg" {
		t.Fatalf("ComicInfo backfill = %+v, %v", backfilled, err)
	}
}

func TestClearSeriesExternalBindingClearsChildBooksSafely(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "clear_ext_books.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertLibrary(ctx, Library{ID: "catalog", RootCID: "root", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	insertCatalogSeries(t, store, "catalog", "work", "Work", make([]catalogBookTime, 3))
	series, err := store.SeriesByID(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyScrapedMetadata(ctx, series, ScrapedMetadata{
		SubjectID: 42, ExtSourceID: "ext-src-1", ExtEntryID: "series-1",
		ExtCoverURL: "https://ext.example.test/s.jpg", MatchOrigin: "manual",
	}); err != nil {
		t.Fatal(err)
	}

	lock := true
	lockedTitle := "Locked Manual Vol 1"
	if err := store.PatchBookMetadata(ctx, "work-book-a", BookMetadataPatch{
		Title: &lockedTitle, TitleLock: &lock,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBookCustomCover(ctx, "work-book-a", "custom1.jpg", "https://custom.example.test/1.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBookCoverSource(ctx, "work-book-a", "ext"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBookScrapedMetadata(ctx, "work-book-a", BookScrapedMetadata{
		ExtSourceID: "ext-src-1", ExtEntryID: "e1",
		ExtCoverURL: "https://ext.example.test/b1.jpg", ExtCoverPath: "ext1.jpg",
		MatchOrigin: "manual", ComicInfoVersion: "v1", ComicInfoStatus: "parsed",
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.SetBookCustomCover(ctx, "work-book-b", "custom2.jpg", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBookCoverSource(ctx, "work-book-b", "custom"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBookScrapedMetadata(ctx, "work-book-b", BookScrapedMetadata{
		BgmSubjectID: 101, BgmCoverURL: "https://bgm.example.test/101.jpg",
		ExtSourceID: "ext-src-1", ExtEntryID: "e2",
		ExtCoverURL: "https://ext.example.test/b2.jpg", ExtCoverPath: "ext2.jpg",
		MatchOrigin: "manual",
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.SetBookCoverSource(ctx, "work-book-c", "ext"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBookScrapedMetadata(ctx, "work-book-c", BookScrapedMetadata{
		ExtSourceID: "other-src", ExtEntryID: "e3",
		ExtCoverURL: "https://other.example.test/b3.jpg", MatchOrigin: "manual",
	}); err != nil {
		t.Fatal(err)
	}

	cleared, err := store.ClearSeriesExternalBinding(ctx, series)
	if err != nil {
		t.Fatalf("ClearSeriesExternalBinding: %v", err)
	}
	if cleared.ExtSourceID != "" || cleared.ExtEntryID != "" || cleared.BgmSubjectID == nil || *cleared.BgmSubjectID != 42 {
		t.Fatalf("unexpected cleared series metadata: %+v", cleared)
	}

	mA, _, err := store.BookMetadataByID(ctx, "work-book-a")
	if err != nil || mA.ExtSourceID != "" || mA.ExtEntryID != "" || mA.ExtCoverURL != "" || mA.ExtCoverPath != "" ||
		mA.CoverSource != CoverSourceAuto || mA.MatchOrigin != "" ||
		mA.Title != lockedTitle || !mA.TitleLock ||
		mA.CustomCoverPath != "custom1.jpg" || mA.CustomCoverURL != "https://custom.example.test/1.jpg" ||
		mA.ComicInfoVersion != "v1" || mA.ComicInfoStatus != "parsed" {
		t.Fatalf("work-book-a after ClearSeriesExternalBinding = %+v, err=%v", mA, err)
	}
	// Verify auto-scrape is no longer blocked by stale match_origin on work-book-a.
	if err := store.UpsertBookScrapedMetadata(WithAutomaticMetadata(ctx), "work-book-a", BookScrapedMetadata{
		Summary: "Re-scraped summary", MatchOrigin: "auto",
	}); err != nil {
		t.Fatalf("auto-scrape after clear was still blocked: %v", err)
	}

	mB, _, err := store.BookMetadataByID(ctx, "work-book-b")
	if err != nil || mB.ExtSourceID != "" || mB.ExtEntryID != "" || mB.ExtCoverURL != "" || mB.ExtCoverPath != "" ||
		mB.BgmSubjectID != 101 || mB.MatchOrigin != "manual" ||
		mB.CoverSource != "custom" || mB.CustomCoverPath != "custom2.jpg" {
		t.Fatalf("work-book-b after ClearSeriesExternalBinding = %+v, err=%v", mB, err)
	}

	mC, _, err := store.BookMetadataByID(ctx, "work-book-c")
	if err != nil || mC.ExtSourceID != "other-src" || mC.ExtEntryID != "e3" || mC.MatchOrigin != "manual" || mC.CoverSource != "ext" {
		t.Fatalf("unrelated ext_source_id work-book-c modified = %+v, err=%v", mC, err)
	}
}

func TestCommitLibraryScanCarriesPasswordBindingsAndSeriesThumbnails(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "scan_carry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertLibrary(ctx, Library{ID: "lib", Name: "Lib", RootCID: "root", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := nowText()
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO series(id,library_id,cid,name,relative_path,one_shot,created_at,updated_at,seen_scan_id) VALUES
('s_old1','lib','old1','Old 1','Root/Old 1',0,?,?,'run0'),
('s_old2','lib','old2','Old 2','Root/Old 2',0,?,?,'run0'),
('s_new2','lib','new2','New 2','Root/New 2',0,?,?,'run0');
INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,created_at,updated_at,seen_scan_id) VALUES
('b1','s_old1','lib','f1','old1','01.cbz',100,'',1,?,?,'run0'),
('b2','s_old2','lib','f2','old2','02.cbz',100,'',1,?,?,'run0')`,
		now, now, now, now, now, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveArchivePasswordBinding(ctx, Book{ID: "b1", SeriesID: "s_old1", LibraryID: "lib", ParentCID: "old1"}, "secret-pw"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO series_thumbnails(series_id,source_book_id,source_version,path,media_type,width,height,size,generation_duration_ns,created_at,updated_at) VALUES
('s_old1','b1','v1','thumb_old1.jpg','image/jpeg',100,150,2048,11,?,?),
('s_old2','b2','v2','thumb_old2.jpg','image/jpeg',100,150,2048,12,?,?),
('s_new2','b2','v2','existing_new2.jpg','image/jpeg',100,150,2048,13,?,?)`,
		now, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}

	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO scan_runs(id,library_id,status,trigger_type,created_at) VALUES('run1','lib','running','manual',?);
INSERT INTO scan_series_staging(run_id,id,library_id,cid,name,relative_path,one_shot,file_modified_at,created_at,updated_at) VALUES
('run1','s_new1','lib','new1','New 1','Root/New 1',0,?,?,?),
('run1','s_new2','lib','new2','New 2','Root/New 2',0,?,?,?);
INSERT INTO scan_books_staging(run_id,id,series_id,library_id,file_id,parent_cid,name,size,pick_code,sha1,file_created_at,file_modified_at,number_sort,created_at,updated_at) VALUES
('run1','b1','s_new1','lib','f1','new1','01.cbz',100,'','',?,?,1,?,?),
('run1','b2','s_new2','lib','f2','new2','02.cbz',100,'','',?,?,1,?,?)`,
		now, now, now, now, now, now, now, now, now, now, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}

	if err := store.CommitLibraryScan(ctx, "lib", "run1"); err != nil {
		t.Fatalf("CommitLibraryScan: %v", err)
	}

	var apbSeries, apbParent string
	if err := store.DB().QueryRowContext(ctx, `SELECT series_id,parent_cid FROM archive_password_bindings WHERE book_id='b1'`).Scan(&apbSeries, &apbParent); err != nil {
		t.Fatal(err)
	}
	if apbSeries != "s_new1" || apbParent != "new1" {
		t.Fatalf("archive_password_bindings = (%q, %q), want ('s_new1', 'new1')", apbSeries, apbParent)
	}

	var thumb1, thumb2 string
	if err := store.DB().QueryRowContext(ctx, `SELECT path FROM series_thumbnails WHERE series_id='s_new1'`).Scan(&thumb1); err != nil || thumb1 != "thumb_old1.jpg" {
		t.Fatalf("carried s_new1 thumbnail = %q, err=%v", thumb1, err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT path FROM series_thumbnails WHERE series_id='s_new2'`).Scan(&thumb2); err != nil || thumb2 != "existing_new2.jpg" {
		t.Fatalf("existing s_new2 thumbnail = %q, err=%v", thumb2, err)
	}
}

func TestTrackerSnapshotAndReadingActivityEffectiveOneShotAndNumberSort(t *testing.T) {
	ctx := t.Context()
	store, err := Open(filepath.Join(t.TempDir(), "tracker_eff.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertLibrary(ctx, Library{ID: "lib", Name: "Lib", RootCID: "root", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	insertCatalogSeries(t, store, "lib", "s_single", "Single Override", make([]catalogBookTime, 1))
	if err := store.SetSeriesOneShotOverride(ctx, "s_single", true); err != nil {
		t.Fatal(err)
	}
	snapSingle, ok, err := store.BangumiTrackerSeriesSnapshot(ctx, "s_single")
	if err != nil || !ok || !snapSingle.OneShot {
		t.Fatalf("single override tracker snapshot = %+v, ok=%v err=%v", snapSingle, ok, err)
	}

	insertCatalogSeries(t, store, "lib", "s_multi", "Multi Sort", make([]catalogBookTime, 2))
	s1 := 2.0
	s2 := 1.0
	if err := store.PatchBookMetadata(ctx, "s_multi-book-a", BookMetadataPatch{NumberSort: &s1}); err != nil {
		t.Fatal(err)
	}
	if err := store.PatchBookMetadata(ctx, "s_multi-book-b", BookMetadataPatch{NumberSort: &s2}); err != nil {
		t.Fatal(err)
	}
	done := true
	inProg := false
	page := 3
	if err := store.UpdateBookReadProgress(ctx, Book{ID: "s_multi-book-b", SeriesID: "s_multi"}, &done, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateBookReadProgress(ctx, Book{ID: "s_multi-book-a", SeriesID: "s_multi"}, &inProg, &page); err != nil {
		t.Fatal(err)
	}
	snapMulti, ok, err := store.BangumiTrackerSeriesSnapshot(ctx, "s_multi")
	if err != nil || !ok || snapMulti.OneShot || snapMulti.MaxCompletedOrdinal != 1 {
		t.Fatalf("multi tracker snapshot with bm.number_sort = %+v, ok=%v err=%v", snapMulti, ok, err)
	}
	activity, err := store.ReadingActivity(ctx, ReadingActivityQuery{InProgressOnly: true, Size: 10})
	if err != nil || len(activity.Groups) != 1 || len(activity.Groups[0].Books) != 1 ||
		activity.Groups[0].Books[0].Book.ID != "s_multi-book-a" || activity.Groups[0].Books[0].Book.NumberSort != 2.0 {
		t.Fatalf("ReadingActivity effective number_sort = %+v, err=%v", activity, err)
	}
}
