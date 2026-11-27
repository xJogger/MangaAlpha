package database

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func openR1AStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func r1aExec(t *testing.T, store *Store, query string, args ...any) {
	t.Helper()
	if _, err := store.DB().ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func r1aLibrary(t *testing.T, store *Store, id string, enabled bool) {
	t.Helper()
	if err := store.UpsertLibrary(t.Context(), Library{ID: id, Name: id, RootCID: id, Enabled: enabled}); err != nil {
		t.Fatal(err)
	}
}

func TestClearSeriesBindingsSeparatesBangumiAndExternalRows(t *testing.T) {
	ctx := t.Context()
	store := openR1AStore(t)
	r1aLibrary(t, store, "lib", true)
	insertCatalogSeries(t, store, "lib", "work", "Work", make([]catalogBookTime, 2))
	if _, err := store.CreateMetadataSource(ctx, MetadataSource{ID: "archive", Kind: "bangumi_archive", Name: "Archive"}); err != nil {
		t.Fatal(err)
	}
	subject := int64(42)
	if err := store.SaveSeriesMetadata(ctx, SeriesMetadata{
		SeriesID: "work", BgmSubjectID: &subject, ExtSourceID: "external", ExtEntryID: "entry", MatchOrigin: "auto",
	}); err != nil {
		t.Fatal(err)
	}
	r1aExec(t, store, `
INSERT INTO book_metadata(book_id,ext_source_id,ext_entry_id,match_origin)
VALUES('work-book-a','external','auto-volume','auto'),('work-book-b','external','manual-volume','manual')`)
	var seriesBindings []SeriesSourceBinding
	var bookBindings []BookSourceBinding
	var seriesCovers []SeriesCoverCandidate
	var bookCovers []BookCoverCandidate
	importedRef := strings.Repeat("a", 64) + ".png"
	protectedRef := strings.Repeat("b", 64) + ".png"
	for _, src := range []string{"bangumi", "archive", "external", "agent-source"} {
		kind, ref, origin := CoverKindBangumi, "https://covers.example.test/image.jpg", "auto"
		switch src {
		case "external":
			kind, ref = CoverKindImportedFile, importedRef
		case "agent-source":
			kind, ref, origin = CoverKindImportedFile, protectedRef, "agent"
		}
		bookBindings = append(bookBindings, BookSourceBinding{BookID: "work-book-a", SourceID: src, EntryID: "42", MatchOrigin: origin})
		bookCovers = append(bookCovers, BookCoverCandidate{BookID: "work-book-a", SourceID: src, EntryID: "42", CoverKind: kind, CoverRef: ref})
		if src != "agent-source" {
			seriesBindings = append(seriesBindings, SeriesSourceBinding{SeriesID: "work", SourceID: src, EntryID: "42"})
			seriesCovers = append(seriesCovers, SeriesCoverCandidate{SeriesID: "work", SourceID: src, EntryID: "42", CoverKind: kind, CoverRef: ref})
		}
	}
	bookBindings = append(bookBindings, BookSourceBinding{BookID: "work-book-b", SourceID: "external", EntryID: "42"})
	bookCovers = append(bookCovers, BookCoverCandidate{BookID: "work-book-b", SourceID: "external", CoverKind: CoverKindImportedFile, CoverRef: protectedRef})
	for _, err := range []error{
		store.UpsertSeriesSourceBindings(ctx, seriesBindings), store.UpsertSeriesCoverCandidates(ctx, seriesCovers),
		store.UpsertBookSourceBindings(ctx, bookBindings), store.UpsertBookCoverCandidates(ctx, bookCovers),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertSources := func(table, column, id, want string) {
		t.Helper()
		var got string
		query := "SELECT coalesce(group_concat(source_id ORDER BY source_id),'') FROM " + table + " WHERE " + column + "=?"
		if err := store.DB().QueryRowContext(ctx, query, id).Scan(&got); err != nil || got != want {
			t.Fatalf("%s %s sources = %q, want %q, err %v", table, id, got, want, err)
		}
	}
	series := Series{ID: "work", Name: "Work"}
	meta, refs, err := store.ClearSeriesExternalBindingWithCoverRefs(ctx, series)
	if err != nil || !slices.Equal(refs, []string{importedRef}) || meta.ExtEntryID != "" ||
		meta.BgmSubjectID == nil || *meta.BgmSubjectID != subject {
		t.Fatalf("clear external: %+v, %q, %v", meta, refs, err)
	}
	for _, table := range []string{"series_source_bindings", "series_cover_candidates"} {
		assertSources(table, "series_id", "work", "archive,bangumi")
	}
	for _, table := range []string{"book_source_bindings", "book_cover_candidates"} {
		assertSources(table, "book_id", "work-book-a", "agent-source,archive,bangumi")
		assertSources(table, "book_id", "work-book-b", "external")
	}
	for id, want := range map[string]string{"work-book-a": "", "work-book-b": "manual-volume"} {
		meta, _, err := store.BookMetadataByID(ctx, id)
		if err != nil || meta.ExtEntryID != want {
			t.Fatalf("%s: %+v, %v", id, meta, err)
		}
	}
	if meta, err = store.ClearSeriesBangumiBinding(ctx, series); err != nil || meta.BgmSubjectID != nil {
		t.Fatalf("clear Bangumi: %+v, %v", meta, err)
	}
	for _, table := range []string{"series_source_bindings", "series_cover_candidates"} {
		assertSources(table, "series_id", "work", "")
	}
	for _, table := range []string{"book_source_bindings", "book_cover_candidates"} {
		assertSources(table, "book_id", "work-book-a", "agent-source")
		assertSources(table, "book_id", "work-book-b", "external")
	}
}

func explainR1A(t *testing.T, store *Store, query string, args ...any) string {
	t.Helper()
	rows, err := store.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "\n")
}

func requireR1APlan(t *testing.T, plan string, details ...string) {
	t.Helper()
	t.Logf("query plan:\n%s", plan)
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Fatal("query used a temporary B-tree")
	}
	for _, detail := range details {
		if !strings.Contains(plan, detail) {
			t.Errorf("missing plan detail %q", detail)
		}
	}
}

func TestR1AVolumeOrderQueryPlansAndTies(t *testing.T) {
	ctx := t.Context()
	store := openR1AStore(t)
	r1aLibrary(t, store, "visible", true)
	r1aLibrary(t, store, "hidden", false)
	insertCatalogSeries(t, store, "visible", "work", "Work", make([]catalogBookTime, 4))
	insertCatalogSeries(t, store, "visible", "single", "Single", make([]catalogBookTime, 1))
	insertCatalogSeries(t, store, "hidden", "invisible", "Invisible", make([]catalogBookTime, 1))
	r1aExec(t, store, `UPDATE series SET one_shot=1 WHERE id='invisible'`)
	if err := store.SetSeriesOneShotOverride(ctx, "single", true); err != nil {
		t.Fatal(err)
	}
	r1aExec(t, store, `UPDATE books SET name=CASE id
WHEN 'work-book-a' THEN 'alpha.cbz'
WHEN 'work-book-b' THEN 'ALPHA.cbz'
WHEN 'work-book-c' THEN 'Beta.cbz'
ELSE 'aardvark.cbz' END WHERE series_id='work'`)
	sort := 1.25
	for _, id := range []string{"work-book-a", "work-book-b", "work-book-c"} {
		if err := store.PatchBookMetadata(ctx, id, BookMetadataPatch{NumberSort: &sort}); err != nil {
			t.Fatal(err)
		}
	}
	sort = 8
	if err := store.UpsertBookScrapedMetadata(ctx, "work-book-d", BookScrapedMetadata{NumberSort: &sort}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []BookQuery{{SeriesID: "work"}, {}} {
		where, args, empty := buildBookFilters(q, []string{"visible", "second-visible"}, true, "admin", false, false)
		if empty {
			t.Fatal("book filters were empty")
		}
		index := "idx_books_eff_number"
		if q.SeriesID != "" {
			index = "idx_books_series_eff_number"
		}
		for _, order := range []string{bookNumberOrder, reverseBookNumberOrder} {
			requireR1APlan(t, explainR1A(t, store, bookPageSelect(" FROM books b", where, order, q.SeriesID != "")+" LIMIT 20 OFFSET 0", args...), index)
		}
	}
	requireR1APlan(t, explainR1A(t, store, firstBookIDQuery, "work"), "idx_books_series_eff_number")
	requireR1APlan(t, explainR1A(t, store, siblingBookIDQuery(">", bookNumberOrder), "work-book-b", "work"), "idx_books_series_eff_number", "effective_number_sort>?", "name>?", "id>?")
	requireR1APlan(t, explainR1A(t, store, siblingBookIDQuery("<", reverseBookNumberOrder), "work-book-b", "work"), "idx_books_series_eff_number", "effective_number_sort<?", "name<?", "id<?")

	first, err := store.FirstBookInSeries(ctx, "work")
	if err != nil || first.ID != "work-book-a" || first.NumberSort != 1.25 {
		t.Fatalf("first book = %+v, %v", first, err)
	}
	want := []string{"work-book-a", "work-book-b", "work-book-c", "work-book-d"}
	for i := range want {
		// The lookup uses the current database sort key, even with stale fields in Book.
		current := Book{ID: want[i], SeriesID: "work", NumberSort: 999, Name: "stale"}
		next, nextErr := store.NextBookInSeries(ctx, current)
		prev, prevErr := store.PreviousBookInSeries(ctx, current)
		if (i == 0 && !errors.Is(prevErr, sql.ErrNoRows)) ||
			(i > 0 && (prevErr != nil || prev.ID != want[i-1])) ||
			(i == len(want)-1 && !errors.Is(nextErr, sql.ErrNoRows)) ||
			(i < len(want)-1 && (nextErr != nil || next.ID != want[i+1])) {
			t.Fatalf("neighbors of %s: previous %s (%v), next %s (%v)", want[i], prev.ID, prevErr, next.ID, nextErr)
		}
	}
	if _, err := store.NextBookInSeries(ctx, Book{ID: "single-book-a", SeriesID: "single"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("one-shot neighbor: %v", err)
	}
	oneShot := true
	where, args, _ := buildBookFilters(BookQuery{OneShot: &oneShot}, []string{"visible"}, false, "admin", false, false)
	requireR1APlan(t, explainR1A(t, store, bookPageSelect(" FROM books b", where, bookNumberOrder, false)+" LIMIT 20", args...),
		"idx_series_effective_oneshot", "library_id=?")
	where, args, _ = buildSeriesFilters(SeriesQuery{OneShot: &oneShot, NSFWMode: "all"}, []string{"visible"}, "admin", false)
	requireR1APlan(t, explainR1A(t, store, "SELECT s.id FROM series s"+where, args...), "idx_series_effective_oneshot")
	books, total, err := store.BooksPage(ctx, BookQuery{OneShot: &oneShot, Size: 20})
	if err != nil || total != 1 || len(books) != 1 || books[0].SeriesID != "single" {
		t.Fatalf("public one-shot books = %+v, %d, %v", books, total, err)
	}
}

func TestR1ALegacyMigrationAndScanOrder(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r1aLibrary(t, store, "lib", true)
	insertCatalogSeries(t, store, "lib", "work", "Work", make([]catalogBookTime, 2))
	r1aExec(t, store, `INSERT INTO book_metadata(book_id,number_sort,ext_source_id,ext_entry_id)
VALUES('work-book-a',7.5,'legacy','legacy-entry'),('work-book-b',NULL,'','')`)
	r1aExec(t, store, `INSERT INTO metadata_sources(id,kind,name,created_at,updated_at)
VALUES('legacy','offline_sqlite','Legacy source','','');
INSERT INTO bangumi_scrape_runs(id,library_id,mode,status,created_at) VALUES('legacy-run','lib','force','done','');
DROP TRIGGER fk115_shelf_books_update;
DROP TRIGGER trg_book_number_override_insert;
DROP TRIGGER trg_book_number_override_update;
DROP TRIGGER trg_book_number_override_delete;
DROP INDEX idx_books_series_eff_number;
DROP INDEX idx_books_eff_number;
DROP INDEX idx_series_effective_oneshot;
ALTER TABLE books DROP COLUMN effective_number_sort;
ALTER TABLE books DROP COLUMN number_sort_override;
ALTER TABLE metadata_sources DROP COLUMN import_covers;
ALTER TABLE bangumi_scrape_runs DROP COLUMN library_ids_json;
DROP TABLE series_source_bindings;
DROP TABLE book_source_bindings;
DROP TABLE series_cover_candidates;
DROP TABLE book_cover_candidates;
DROP TABLE library_metadata_chain_state;`)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	for iteration := 0; iteration < 2; iteration++ {
		store, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		books, _, err := store.BooksPage(ctx, BookQuery{SeriesID: "work"})
		if err != nil || len(books) != 2 || books[0].ID != "work-book-b" || books[0].NumberSort != 2 || books[1].NumberSort != 7.5 {
			t.Fatalf("migrated books = %+v, %v", books, err)
		}
		src, err := store.GetMetadataSource(ctx, "legacy")
		if err != nil || src.ImportCovers {
			t.Fatalf("legacy import covers = %v, %v", src.ImportCovers, err)
		}
		bindings, err := store.ListBookSourceBindings(ctx, "work-book-a")
		if err != nil || len(bindings) != 0 {
			t.Fatalf("migration eagerly populated bindings: %+v, %v", bindings, err)
		}
		var libraryIDsJSON string
		if err := store.DB().QueryRowContext(ctx, "SELECT library_ids_json FROM bangumi_scrape_runs WHERE id='legacy-run'").Scan(&libraryIDsJSON); err != nil || libraryIDsJSON != "" {
			t.Fatalf("legacy run library IDs = %q, %v", libraryIDsJSON, err)
		}
		if iteration == 0 {
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	defer store.Close()
	r1aExec(t, store, `
INSERT INTO scan_runs(id,library_id,trigger_type,status,created_at) VALUES('rescan','lib','manual','running','');
INSERT INTO scan_series_staging(run_id,id,library_id,cid,name,relative_path,one_shot,file_modified_at,created_at,updated_at)
SELECT 'rescan',id,library_id,cid,name,relative_path,one_shot,file_modified_at,created_at,updated_at FROM series;
INSERT INTO scan_books_staging(run_id,id,series_id,library_id,file_id,parent_cid,name,size,pick_code,sha1,file_created_at,file_modified_at,number_sort,created_at,updated_at)
SELECT 'rescan',id,series_id,library_id,file_id,parent_cid,name,size,pick_code,sha1,file_created_at,file_modified_at,number_sort+10,created_at,updated_at FROM books;
INSERT INTO bangumi_scrape_runs(id,library_id,library_ids_json,mode,status,created_at)
VALUES('multi',NULL,'["lib","other"]','force','done','');`)
	if err := store.CommitLibraryScan(ctx, "lib", "rescan"); err != nil {
		t.Fatal(err)
	}
	assertNumber := func(want float64) {
		t.Helper()
		book, err := store.BookByID(ctx, "work-book-a")
		if err != nil || book.NumberSort != want {
			t.Fatalf("effective number = %v, want %v, err %v", book.NumberSort, want, err)
		}
	}
	assertNumber(7.5)
	if err := store.PatchBookMetadata(ctx, "work-book-a", BookMetadataPatch{ClearNumberSort: true}); err != nil {
		t.Fatal(err)
	}
	assertNumber(11)
	value := 0.75
	if err := store.UpsertBookScrapedMetadata(ctx, "work-book-a", BookScrapedMetadata{NumberSort: &value}); err != nil {
		t.Fatal(err)
	}
	assertNumber(value)
	r1aExec(t, store, "DELETE FROM book_metadata WHERE book_id='work-book-a'")
	assertNumber(11)
	r1aExec(t, store, "DELETE FROM libraries WHERE id='lib'")
	var subset string
	if err := store.DB().QueryRowContext(ctx, "SELECT library_ids_json FROM bangumi_scrape_runs WHERE id='multi' AND library_id IS NULL").Scan(&subset); err != nil || subset != `["lib","other"]` {
		t.Fatalf("multi-library run = %q, %v", subset, err)
	}
	var oldRuns int
	if err := store.DB().QueryRowContext(ctx, "SELECT count(*) FROM bangumi_scrape_runs WHERE id='legacy-run'").Scan(&oldRuns); err != nil || oldRuns != 0 {
		t.Fatalf("old single-library FK behavior = %d, %v", oldRuns, err)
	}
}

func TestR1ASparseBindingsAndCoverSourceDeletion(t *testing.T) {
	ctx := t.Context()
	store := openR1AStore(t)
	r1aLibrary(t, store, "lib", true)
	insertCatalogSeries(t, store, "lib", "work", "Work", make([]catalogBookTime, 2))
	insertCatalogSeries(t, store, "lib", "other", "Other", make([]catalogBookTime, 1))
	repo, err := store.UpsertMetadataRepo(ctx, MetadataRepo{ID: "repo", Name: "Repo", URL: "https://example.invalid/repo"})
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []MetadataSource{
		{ID: "primary", Kind: "offline_sqlite", Name: "Primary", Ready: true, Enabled: true, ImportCovers: true},
		{ID: "next", RepoID: repo.ID, Kind: "offline_sqlite", Name: "Next", Ready: true, Enabled: true},
		{ID: "alphabetical", Kind: "offline_sqlite", Name: "Unlisted", Ready: true, Enabled: true},
	} {
		if _, err := store.CreateMetadataSource(ctx, src); err != nil {
			t.Fatal(err)
		}
	}
	src, err := store.GetMetadataSource(ctx, "primary")
	if err != nil || !src.ImportCovers {
		t.Fatalf("persisted importCovers = %v, %v", src.ImportCovers, err)
	}
	src.ImportCovers = false
	if _, err := store.UpdateMetadataSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	sourceRows, err := store.ListMetadataSources(ctx, "offline_sqlite")
	if err != nil || len(sourceRows) != 3 {
		t.Fatalf("source list = %+v, %v", sourceRows, err)
	}
	for _, row := range sourceRows {
		if row.ImportCovers {
			t.Fatalf("importCovers update was lost: %+v", row)
		}
	}
	if err := store.SetLibraryMetadataSources(ctx, "lib", []string{"primary", "next"}); err != nil {
		t.Fatal(err)
	}
	prior, err := store.GetLibraryMetadataChainState(ctx, "lib")
	if err != nil {
		t.Fatal(err)
	}
	subject := int64(42)
	if err := store.SaveSeriesMetadata(ctx, SeriesMetadata{
		SeriesID: "work", BgmSubjectID: &subject, ExtSourceID: "primary", ExtEntryID: "primary-series",
		ExtCoverURL: "sqlite-blob://primary/open-entry/primary-series", ExtCoverPath: "legacy.png", MatchOrigin: "manual",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBookScrapedMetadata(ctx, "work-book-a", BookScrapedMetadata{
		ExtSourceID: "primary", ExtEntryID: "primary-book", ExtCoverURL: "sqlite-blob://primary/open-book/primary-book",
	}); err != nil {
		t.Fatal(err)
	}
	r1aExec(t, store, `INSERT INTO book_metadata(book_id,ext_cover_url)
VALUES('work-book-b','sqlite-blob://primary/open-book/stale')`)
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := store.ReplaceSeriesSourceBindingsTx(ctx, tx, "work", []SeriesSourceBinding{
		{SourceID: "primary", EntryID: "primary-series"},
		{SourceID: "next", EntryID: "next-series", MatchOrigin: "agent", Confidence: 0.9, SourceGeneration: "g2"},
		{SourceID: "alphabetical", EntryID: "unlisted-series"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBookSourceBindings(ctx, []BookSourceBinding{
		{BookID: "work-book-a", SourceID: "primary", EntryID: "primary-book"},
		{BookID: "work-book-a", SourceID: "next", SeriesEntryID: "next-series", EntryID: "next-book", Confidence: 0.8},
		{BookID: "work-book-b", SourceID: "primary", EntryID: "stale"},
	}); err != nil {
		t.Fatal(err)
	}
	importedSeries := SeriesCoverCandidate{
		SeriesID: "work", SourceID: "primary", EntryID: "primary-series", CoverKind: CoverKindImportedFile,
		CoverRef: strings.Repeat("b", 64) + ".png", RankOrder: 11, IsNSFW: true, NSFWKnown: false, SourceGeneration: "g1", UpdatedAt: "initial",
	}
	importedBook := BookCoverCandidate{
		BookID: "work-book-a", SourceID: "next", SeriesEntryID: "next-series", EntryID: "next-book", CoverKind: CoverKindImportedFile,
		CoverRef: strings.Repeat("a", 64) + ".png", RankOrder: 7, IsNSFW: false, NSFWKnown: true, SourceGeneration: "g2", UpdatedAt: "initial",
	}
	nextHTTP := "https://example.invalid/next.png"
	if err := store.UpsertSeriesCoverCandidates(ctx, []SeriesCoverCandidate{
		importedSeries,
		{SeriesID: "work", SourceID: "next", EntryID: "next-series", CoverKind: CoverKindHTTPURL, CoverRef: nextHTTP, RankOrder: 2},
		{SeriesID: "work", SourceID: CoverCandidateSourceManual, CoverKind: CoverKindImportedFile, CoverRef: strings.Repeat("c", 64) + ".png", RankOrder: 2},
		{SeriesID: "other", SourceID: "primary", CoverKind: CoverKindImportedFile, CoverRef: importedSeries.CoverRef},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBookCoverCandidates(ctx, []BookCoverCandidate{
		importedBook, {BookID: "work-book-a", SourceID: "primary", CoverKind: CoverKindHTTPURL, CoverRef: "https://example.invalid/primary.png"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteMetadataSource(ctx, "primary"); err != nil {
		t.Fatal(err)
	}
	updated, err := store.GetLibraryMetadataChainState(ctx, "lib")
	if err != nil || updated.ChainVersion == prior.ChainVersion || !updated.NeedsRematch {
		t.Fatalf("delete state = %+v, previous %+v, err %v", updated, prior, err)
	}
	series := Series{ID: "work", Name: "Work"}
	meta, err := store.SeriesMetadataByID(ctx, series)
	if err != nil || meta.ExtSourceID != "next" || meta.ExtEntryID != "next-series" || meta.ExtCoverURL != nextHTTP ||
		meta.ExtCoverPath != "" || meta.BgmSubjectID == nil || *meta.BgmSubjectID != subject || meta.MatchOrigin != "manual" {
		t.Fatalf("fallback projection = %+v, %v", meta, err)
	}
	bookMeta, _, err := store.BookMetadataByID(ctx, "work-book-a")
	if err != nil || bookMeta.ExtSourceID != "next" || bookMeta.ExtEntryID != "next-book" || bookMeta.ExtCoverURL != "" {
		t.Fatalf("book fallback projection = %+v, %v", bookMeta, err)
	}
	bookMeta, _, err = store.BookMetadataByID(ctx, "work-book-b")
	if err != nil || bookMeta.ExtCoverURL != "" {
		t.Fatalf("legacy blob without ext_source_id survived: %+v, %v", bookMeta, err)
	}
	if rows, err := store.ListSeriesCoverCandidates(ctx, "work"); err != nil || len(rows) != 3 || rows[2] != importedSeries ||
		rows[0].SourceID != CoverCandidateSourceManual || rows[1].SourceID != "next" {
		t.Fatalf("ordered series candidates = %+v, %v", rows, err)
	}
	if rows, err := store.ListBookCoverCandidatesBySeries(ctx, "work"); err != nil || !slices.Equal(rows["work-book-a"], []BookCoverCandidate{importedBook}) {
		t.Fatalf("book candidates by series = %+v, %v", rows, err)
	}
	if rows, err := store.ListBookSourceBindingsBySeries(ctx, "work"); err != nil || len(rows) != 1 || len(rows["work-book-a"]) != 1 || rows["work-book-a"][0].SourceID != "next" {
		t.Fatalf("book bindings by series = %+v, %v", rows, err)
	}

	if err := store.DeleteMetadataRepo(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	meta, err = store.SeriesMetadataByID(ctx, series)
	if err != nil || meta.ExtSourceID != "alphabetical" || meta.ExtCoverURL != "" || meta.ExtCoverPath != "" {
		t.Fatalf("projection after deleting repository = %+v, %v", meta, err)
	}
	if rows, err := store.ListBookCoverCandidates(ctx, "work-book-a"); err != nil || !slices.Equal(rows, []BookCoverCandidate{importedBook}) {
		t.Fatalf("imported book candidate after deleting repository = %+v, %v", rows, err)
	}
	bookMeta, _, err = store.BookMetadataByID(ctx, "work-book-a")
	if err != nil || bookMeta.ExtSourceID != "" || bookMeta.ExtEntryID != "" {
		t.Fatalf("stale book binding after repo delete = %+v, %v", bookMeta, err)
	}

	_, refs, err := store.ClearSeriesExternalBindingWithCoverRefs(ctx, series)
	if err != nil || !slices.Equal(refs, []string{importedBook.CoverRef, importedSeries.CoverRef}) {
		t.Fatalf("potential orphan cover refs = %q, %v", refs, err)
	}
	if ref, err := store.IsImportedCoverRefReferenced(ctx, importedSeries.CoverRef); err != nil || !ref {
		t.Fatalf("shared cover reference = %v, %v", ref, err)
	}
	if ref, err := store.IsImportedCoverRefReferenced(ctx, importedBook.CoverRef); err != nil || ref {
		t.Fatalf("removed book cover reference = %v, %v", ref, err)
	}
	if rows, err := store.ListSeriesCoverCandidates(ctx, "work"); err != nil || len(rows) != 1 || rows[0].SourceID != CoverCandidateSourceManual {
		t.Fatalf("manual cover after clear = %+v, %v", rows, err)
	}
	if rows, err := store.ListSeriesSourceBindings(ctx, "work"); err != nil || len(rows) != 0 {
		t.Fatalf("series bindings after clear = %+v, %v", rows, err)
	}
}

func TestR1AMetadataChainVersionStateMachine(t *testing.T) {
	ctx := t.Context()
	store := openR1AStore(t)
	for _, id := range []string{"configured", "default", "unrelated", "member"} {
		r1aLibrary(t, store, id, true)
	}
	union, _, err := store.CreateUnionLibrary(ctx, "Union", true, []string{"member", "default"})
	if err != nil {
		t.Fatal(err)
	}
	src, err := store.CreateMetadataSource(ctx, MetadataSource{
		ID: "source", Name: "Source", Kind: "offline_sqlite", Enabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetLibraryMetadataSources(ctx, "configured", []string{src.ID, "other"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetLibraryMetadataSources(ctx, "unrelated", []string{"other"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetLibraryMetadataSources(ctx, "member", []string{src.ID}); err != nil {
		t.Fatal(err)
	}
	state := func(id string) LibraryMetadataChainState {
		t.Helper()
		st, err := store.GetLibraryMetadataChainState(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	initial := state("configured")
	if !initial.NeedsRematch || initial.AppliedChainVersion != "" {
		t.Fatalf("initial configured state = %+v", initial)
	}
	if err := store.SetLibraryMetadataSources(ctx, "configured", []string{src.ID, "other", src.ID}); err != nil {
		t.Fatal(err)
	}
	if current := state("configured"); current != initial {
		t.Fatalf("unchanged configuration marked dirty: %+v", current)
	}
	unrelated := state("unrelated")
	member := state("member")
	if err := store.MarkLibraryMetadataChainApplied(ctx, "configured", initial.ChainVersion); err != nil {
		t.Fatal(err)
	}
	if state("configured").NeedsRematch {
		t.Fatal("applying the current version left a reminder")
	}
	if _, err := store.UpdateMetadataSourceConfig(ctx, src.ID, true, "", "zh", "manual", "", "", "", false); err != nil {
		t.Fatal(err)
	}
	enabled := state("configured")
	if enabled.ChainVersion == initial.ChainVersion || !enabled.NeedsRematch ||
		!state(union.ID).NeedsRematch || state("member") != member || state("default").NeedsRematch || state("unrelated") != unrelated {
		t.Fatalf("source enable marked the wrong libraries: %+v", enabled)
	}
	if err := store.MarkLibraryMetadataChainApplied(ctx, "configured", initial.ChainVersion); err != nil {
		t.Fatal(err)
	}
	if current := state("configured"); current != enabled {
		t.Fatalf("stale application cleared the reminder: %+v", current)
	}
	digest := "new-generation"
	if _, err := store.UpdateMetadataSourceSyncState(ctx, src.ID, MetadataSourceSyncState{LocalSHA256: &digest}); err != nil {
		t.Fatal(err)
	}
	published := state("configured")
	if published.ChainVersion == enabled.ChainVersion || !published.NeedsRematch {
		t.Fatalf("source publication failed to dirty chain: %+v", published)
	}
	if _, err := store.UpdateMetadataSourceSyncState(ctx, src.ID, MetadataSourceSyncState{LocalSHA256: &digest}); err != nil {
		t.Fatal(err)
	}
	if state("configured") != published {
		t.Fatal("identical source publication marked the chain again")
	}
	if err := store.MarkLibraryMetadataChainApplied(ctx, "configured", published.ChainVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkLibrariesUsingSourceChainChanged(ctx, src.ID, "generation-three"); err != nil {
		t.Fatal(err)
	}
	states, err := store.ListAllLibraryMetadataChainStates(ctx)
	if err != nil || !states["configured"].NeedsRematch || states["configured"].ChainVersion == published.ChainVersion {
		t.Fatalf("listed chain states = %+v, %v", states, err)
	}
	libs, err := store.Libraries(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, lib := range libs {
		if lib.MetadataChainNeedsRematch != states[lib.ID].NeedsRematch {
			t.Fatalf("library summary needsRematch = %v for %s", lib.MetadataChainNeedsRematch, lib.ID)
		}
	}
	if err := store.MarkLibraryMetadataChainChanged(ctx, "configured", "explicit-version"); err != nil {
		t.Fatal(err)
	}
	if st := state("configured"); st.ChainVersion != "explicit-version" || !st.NeedsRematch {
		t.Fatalf("explicit chain version = %+v", st)
	}
	if err := store.DeleteLibrary(ctx, "configured"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetLibraryMetadataChainState(ctx, "configured"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted library state error = %v", err)
	}
	states, err = store.ListAllLibraryMetadataChainStates(ctx)
	if _, found := states["configured"]; err != nil || found {
		t.Fatalf("deleted library retained state: %+v, %v", states, err)
	}
}

func TestR1AUnionMetadataQueueIsScopedAndTracksRemovedWinner(t *testing.T) {
	ctx := t.Context()
	store := openR1AStore(t)
	for _, id := range []string{"upper", "lower", "other-source", "unrelated"} {
		r1aLibrary(t, store, id, true)
	}
	insertCatalogSeries(t, store, "upper", "upper-work", "Work", make([]catalogBookTime, 1))
	insertCatalogSeries(t, store, "lower", "lower-work", "Work", make([]catalogBookTime, 1))
	insertCatalogSeries(t, store, "upper", "upper-secondary", "Secondary", make([]catalogBookTime, 1))
	insertCatalogSeries(t, store, "other-source", "other-work", "Other", make([]catalogBookTime, 1))
	insertCatalogSeries(t, store, "unrelated", "unrelated-work", "Unrelated", make([]catalogBookTime, 1))

	first, _, err := store.CreateUnionLibrary(ctx, "First", true, []string{"upper", "lower"})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.CreateUnionLibrary(ctx, "Second", true, []string{"other-source"})
	if err != nil {
		t.Fatal(err)
	}
	var workBook string
	if err := store.DB().QueryRowContext(ctx, `
SELECT union_book_id FROM union_book_sources WHERE source_book_id='upper-work-book-a'`).Scan(&workBook); err != nil {
		t.Fatal(err)
	}
	queued := func(library string) int {
		t.Helper()
		var count int
		if err := store.DB().QueryRowContext(ctx, "SELECT count(*) FROM union_book_metadata_dirty WHERE library_id=?", library).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if queued(first.ID) != 0 || queued(second.ID) != 0 {
		t.Fatal("new union refresh left queued rows")
	}
	if err := store.SetBookComicInfoSentinel(ctx, "upper-work-book-a", "upper-version", "parsed"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"other-work-book-a", "unrelated-work-book-a"} {
		if err := store.UpsertBookScrapedMetadata(ctx, id, BookScrapedMetadata{Title: "Changed"}); err != nil {
			t.Fatal(err)
		}
	}
	requireR1APlan(t, explainR1A(t, store, "SELECT d.book_id,sbm.title "+unionBookMetadataDirtySources, first.ID),
		"SEARCH d USING PRIMARY KEY (library_id=?)", "SEARCH ubs USING PRIMARY KEY (union_book_id=?)", "SEARCH sbm")
	if queued(first.ID) != 1 || queued(second.ID) != 1 || queued("unrelated") != 0 {
		t.Fatal("metadata update queued books outside its affected mappings")
	}
	revisionBefore, err := store.shelfRevision(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefreshUnionLibrary(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	revisionAfter, err := store.shelfRevision(ctx, true)
	if err != nil || revisionBefore != revisionAfter {
		t.Fatalf("ComicInfo sentinel alone advanced catalog: %+v -> %+v, %v", revisionBefore, revisionAfter, err)
	}
	meta, _, err := store.BookMetadataByID(ctx, workBook)
	if err != nil || meta.ComicInfoVersion != "upper-version" || queued(first.ID) != 0 || queued(second.ID) != 1 {
		t.Fatalf("scoped union refresh = %+v, %v", meta, err)
	}
	r1aExec(t, store, "DELETE FROM books WHERE id='upper-work-book-a'")
	if _, err := store.RefreshUnionLibrary(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	meta, _, err = store.BookMetadataByID(ctx, workBook)
	if err != nil || meta.ComicInfoVersion != "" || meta.ComicInfoStatus != "" || queued(second.ID) != 1 {
		t.Fatalf("removed primary metadata marker survived = %+v, %v", meta, err)
	}

	// A pre-upgrade union has mappings but may have no pending row yet.
	if err := store.UpsertBookScrapedMetadata(ctx, "lower-work-book-a", BookScrapedMetadata{ISBN: "new-isbn"}); err != nil {
		t.Fatal(err)
	}
	r1aExec(t, store, "DELETE FROM union_book_metadata_dirty WHERE library_id=?", first.ID)
	r1aExec(t, store, "UPDATE libraries SET union_book_metadata_initialized=0 WHERE id=?", first.ID)
	if _, err := store.RefreshUnionLibrary(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	meta, _, err = store.BookMetadataByID(ctx, workBook)
	if err != nil || meta.ISBN != "new-isbn" || queued(first.ID) != 0 {
		t.Fatalf("legacy scoped inheritance = %+v, %v", meta, err)
	}
}

func TestR1AMetadataWritesAreDifferential(t *testing.T) {
	ctx := t.Context()
	store := openR1AStore(t)
	r1aLibrary(t, store, "lib", true)
	insertCatalogSeries(t, store, "lib", "work", "Work", make([]catalogBookTime, 1))
	series, err := store.SeriesByID(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	incoming := ScrapedMetadata{
		SubjectID: 1, Title: "Scraped",
		Genres:  []string{"Fantasy", "Drama"},
		Tags:    []string{"Adventure", "Travel"},
		Authors: []Author{{Name: "One", Role: "writer"}, {Name: "Two", Role: "artist"}},
	}
	firstSeries, err := store.UpsertSeriesScrapedMetadata(ctx, series, incoming)
	if err != nil {
		t.Fatal(err)
	}
	bookID := "work-book-a"
	incomingBook := BookScrapedMetadata{Title: "Old title", Tags: incoming.Tags, Authors: incoming.Authors}
	if err := store.UpsertBookScrapedMetadata(ctx, bookID, incomingBook); err != nil {
		t.Fatal(err)
	}
	firstBook, _, err := store.BookMetadataByID(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.shelfRevision(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	r1aExec(t, store, `
CREATE TABLE metadata_write_audit (table_name TEXT NOT NULL);
CREATE TRIGGER audit_genres_delete AFTER DELETE ON series_genres BEGIN
 INSERT INTO metadata_write_audit VALUES('genres');
END;
CREATE TRIGGER audit_genres_insert AFTER INSERT ON series_genres BEGIN
 INSERT INTO metadata_write_audit VALUES('genres');
END;
CREATE TRIGGER audit_tags_delete AFTER DELETE ON series_tags BEGIN
 INSERT INTO metadata_write_audit VALUES('tags');
END;
CREATE TRIGGER audit_tags_insert AFTER INSERT ON series_tags BEGIN
 INSERT INTO metadata_write_audit VALUES('tags');
END;
CREATE TRIGGER audit_authors_delete AFTER DELETE ON series_authors BEGIN
 INSERT INTO metadata_write_audit VALUES('authors');
END;
CREATE TRIGGER audit_authors_insert AFTER INSERT ON series_authors BEGIN
 INSERT INTO metadata_write_audit VALUES('authors');
END;`)
	incoming.Genres = []string{" Drama ", "Fantasy", "drama"}
	incoming.Tags = []string{"Travel", " Adventure "}
	incoming.Authors = []Author{{Name: " Two ", Role: "ARTIST"}, {Name: "One"}}
	incomingBook.Tags, incomingBook.Authors = incoming.Tags, incoming.Authors
	secondSeries, err := store.UpsertSeriesScrapedMetadata(ctx, series, incoming)
	if err != nil || !secondSeries.UpdatedAt.Equal(firstSeries.UpdatedAt) || !slices.Equal(secondSeries.Genres, firstSeries.Genres) {
		t.Fatalf("no-op changed series metadata = %+v, %v", secondSeries, err)
	}
	if err := store.UpsertBookScrapedMetadata(ctx, bookID, incomingBook); err != nil {
		t.Fatal(err)
	}
	secondBook, _, err := store.BookMetadataByID(ctx, bookID)
	if err != nil || !secondBook.UpdatedAt.Equal(firstBook.UpdatedAt) || !slices.Equal(secondBook.Tags, firstBook.Tags) {
		t.Fatalf("no-op changed book metadata = %+v, %v", secondBook, err)
	}
	if err := store.SetBookComicInfoSentinel(ctx, bookID, "new-sentinel", "parsed"); err != nil {
		t.Fatal(err)
	}
	after, err := store.shelfRevision(ctx, false)
	if err != nil || before != after {
		t.Fatalf("no-op and sentinel changed catalog revision: %+v -> %+v, %v", before, after, err)
	}
	incoming.Genres = []string{"Fantasy", "Mystery"}
	if _, err := store.UpsertSeriesScrapedMetadata(ctx, series, incoming); err != nil {
		t.Fatal(err)
	}
	var touched, nonGenre int
	if err := store.DB().QueryRowContext(ctx, "SELECT count(*),sum(table_name<>'genres') FROM metadata_write_audit").Scan(&touched, &nonGenre); err != nil || touched != 2 || nonGenre != 0 {
		t.Fatalf("changing one genre caused %d writes, %d outside genres, %v", touched, nonGenre, err)
	}
	after, err = store.shelfRevision(ctx, false)
	if err != nil || after == before {
		t.Fatalf("visible change failed to advance catalog revision: %+v -> %+v, %v", before, after, err)
	}
	wakeup := &Store{ftsWake: make(chan struct{}, 1)}
	wakeup.NotifyBookSearchDirty()
	wakeup.NotifyBookSearchDirty()
	if len(wakeup.ftsWake) != 1 {
		t.Fatal("non-blocking FTS worker signal was missing")
	}
}
