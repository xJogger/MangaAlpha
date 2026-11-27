package extmeta

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/thumbnail"
)

func TestOfflineHTTPCoversWithBlobImportDisabled(t *testing.T) {
	ctx := t.Context()
	store, m, root := newTestManager(t, ManagerOptions{})
	upstream := filepath.Join(root, "open-v2.sqlite")
	createWave2BOpenV2DB(t, upstream, encodeTestJPEG(t), encodeTestPNG(t))

	const seriesURL = "http://covers.example.test/galaxy.jpg"
	const bookURL = "https://covers.example.test/galaxy-vol-1.png"
	db, err := sql.Open("sqlite", upstream)
	if err != nil {
		t.Fatal(err)
	}
	execManagerFixture(t, db, `UPDATE entries SET cover_url=? WHERE id='s1'`, seriesURL)
	execManagerFixture(t, db, `UPDATE books SET cover_url=? WHERE series_entry_id='s1'`, bookURL)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	source := addManagerSource(t, m, database.MetadataSource{Name: "Open Meta", Kind: KindOfflineSQLite, LocalPath: upstream})
	syncManagerFixture(t, m, source, false, "success")
	seedMetadataLibrary(t, store, "lib", true)
	if _, err := m.SetLibrarySources(ctx, "lib", []string{source.ID}); err != nil {
		t.Fatal(err)
	}
	seedMetadataSeries(t, store, "manual", "lib", "Galaxy Folder")
	seedMetadataSeries(t, store, "auto", "lib", "银河铁道")
	seedMetadataSeries(t, store, "blob-only", "lib", "Another Work")
	seedWave2BBook(t, store, "manual-vol", "manual", "lib", "Galaxy Vol.01.cbz", 1)
	seedWave2BBook(t, store, "auto-vol", "auto", "lib", "Galaxy Vol.01.cbz", 1)
	seedWave2BBook(t, store, "blob-only-vol", "blob-only", "lib", "Other Vol.01.cbz", 1)

	if _, err := m.BindSeriesExternal(ctx, "manual", source.ID, "s1", "manual"); err != nil {
		t.Fatal(err)
	}
	if run := runChainFixture(t, m, "lib", "force", nil); run.Status != "success" || run.Matched != 2 {
		t.Fatalf("scrape: %+v", run)
	}
	for _, id := range []string{"manual", "auto"} {
		meta := readMetadata(t, store, id)
		if meta.ExtEntryID != "s1" || meta.ExtCoverURL != seriesURL {
			t.Fatalf("%s series projection: %+v", id, meta)
		}
		covers, err := store.ListSeriesCoverCandidates(ctx, id)
		if err != nil || len(covers) != 1 || covers[0].CoverKind != database.CoverKindHTTPURL || covers[0].CoverRef != seriesURL {
			t.Fatalf("%s series covers: %+v, %v", id, covers, err)
		}
		book, ok, err := store.BookMetadataByID(ctx, id+"-vol")
		if err != nil || !ok || book.ExtEntryID != "vol-1" || book.ExtCoverURL != bookURL {
			t.Fatalf("%s book projection: %+v, %v", id, book, err)
		}
		bookCovers, err := store.ListBookCoverCandidates(ctx, id+"-vol")
		if err != nil || len(bookCovers) != 1 || bookCovers[0].CoverKind != database.CoverKindHTTPURL || bookCovers[0].CoverRef != bookURL {
			t.Fatalf("%s book covers: %+v, %v", id, bookCovers, err)
		}
	}
	if meta := readMetadata(t, store, "blob-only"); meta.ExtEntryID != "s2" || meta.ExtCoverURL != "" {
		t.Fatalf("BLOB-only series projection: %+v", meta)
	}
	if book, ok, err := store.BookMetadataByID(ctx, "blob-only-vol"); err != nil || !ok || book.ExtEntryID != "vol-1" || book.ExtCoverURL != "" {
		t.Fatalf("BLOB-only book projection: %+v, %v", book, err)
	}
	if covers, err := store.ListSeriesCoverCandidates(ctx, "blob-only"); err != nil || len(covers) != 0 {
		t.Fatalf("BLOB-only series covers: %+v, %v", covers, err)
	}
	if covers, err := store.ListBookCoverCandidates(ctx, "blob-only-vol"); err != nil || len(covers) != 0 {
		t.Fatalf("BLOB-only book covers: %+v, %v", covers, err)
	}
	if _, err := os.Stat(m.importedCovers); !os.IsNotExist(err) {
		t.Fatalf("disabled BLOB import wrote files: %v", err)
	}
}

func TestForceMergesCompleteMetadataChain(t *testing.T) {
	ctx := t.Context()
	store, m, root := newTestManager(t, ManagerOptions{})
	bgm, err := bangumi.NewService(store, filepath.Join(root, "bangumi"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(); bgm.Close() })
	m.bangumi = bgm
	importChainBangumi(t, bgm, root)
	thumbs, err := thumbnail.NewWithDataDir(store, filepath.Join(root, "thumbs"), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	rawBGM := encodeTestJPEG(t)
	thumbs.SetBangumiCoverFetcher(func(context.Context, string, int64) ([]byte, error) { return rawBGM, nil })

	rawKomga := encodeTestPNG(t)
	upstream := filepath.Join(root, "komga.sqlite")
	createWave2BKomgaDB(t, upstream, encodeTestJPEG(t), rawKomga)
	db, err := sql.Open("sqlite", upstream)
	if err != nil {
		t.Fatal(err)
	}
	execManagerFixture(t, db, `
ALTER TABLE SERIES ADD COLUMN DELETED_DATE TEXT;
ALTER TABLE BOOK ADD COLUMN DELETED_DATE TEXT;
ALTER TABLE SERIES_METADATA ADD COLUMN TITLE_LOCK INTEGER DEFAULT 0;
ALTER TABLE SERIES_METADATA ADD COLUMN STATUS_LOCK INTEGER DEFAULT 0;
ALTER TABLE BOOK_METADATA ADD COLUMN TITLE_LOCK INTEGER DEFAULT 0;
UPDATE SERIES SET DELETED_DATE='2025-01-01' WHERE ID='k-s2';
UPDATE BOOK SET DELETED_DATE='2025-01-01' WHERE ID='kb2';
UPDATE BOOK_METADATA SET NUMBER_SORT=0 WHERE BOOK_ID='kb2';
UPDATE SERIES_METADATA SET TITLE=' ARIA-FOLDER ',TITLE_SORT='aria-folder',STATUS='ONGOING',SUMMARY='',AGE_RATING=18 WHERE SERIES_ID='k-s1';
UPDATE BOOK_METADATA SET TITLE='[GroupA] ARIA Vol.01',SUMMARY='' WHERE BOOK_ID='kb1';
DELETE FROM THUMBNAIL_SERIES;`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	source := addManagerSource(t, m, database.MetadataSource{Name: "Komga", Kind: KindOfflineSQLite, LocalPath: upstream, ImportCovers: true})
	syncManagerFixture(t, m, source, false, "success")
	if _, err := m.entry(ctx, source.ID, "k-s2"); err != ErrEntryNotFound {
		t.Fatalf("deleted series loaded: %v", err)
	}
	if _, _, err := ReadKomgaThumbnailBLOB(ctx, upstream, SQLiteBlobKindKomgaBook, "tb-2"); err != ErrEntryNotFound {
		t.Fatalf("deleted book cover loaded: %v", err)
	}

	seedMetadataLibrary(t, store, "lib", true)
	for _, id := range []string{"auto", "legacy", "manual", "agent"} {
		seedMetadataSeries(t, store, id, "lib", "Aria Official")
		seedWave2BBook(t, store, id+"-vol", id, "lib", "Aria Official Vol.01.cbz", 1)
	}
	execManagerFixture(t, store.DB(), `
INSERT INTO series_metadata(series_id,bgm_subject_id,match_origin,title,summary,summary_lock,updated_at)
VALUES ('auto',100,'auto','','',0,''),('legacy',100,'','Legacy title','Legacy summary',0,''),
 ('manual',100,'manual','Manual title','Manual summary',0,''),('agent',100,'agent','Agent title','Agent summary',0,'')`)
	if _, err := m.SetLibrarySources(ctx, "lib", []string{source.ID, TargetSourceBangumi}); err != nil {
		t.Fatal(err)
	}
	run := runChainFixture(t, m, "lib", "force", nil)
	if run.Status != "success" || run.TotalSeries != 2 || run.Matched != 2 {
		t.Fatalf("run: %+v", run)
	}

	got := readMetadata(t, store, "auto")
	if got.BgmSubjectID == nil || *got.BgmSubjectID != 100 || got.ExtSourceID != source.ID || got.ExtEntryID != "k-s1" ||
		got.Title != "Official Edition" || got.Summary != "Bangumi description" || got.BgmScore == nil || *got.BgmScore != 8.6 ||
		got.Status != "ENDED" || !got.IsNSFW || !slices.Equal(got.Tags, []string{"classic", "slice of life", "Exclusive"}) {
		t.Fatalf("merged fields: %+v", got)
	}
	legacy := readMetadata(t, store, "legacy")
	if legacy.BgmSubjectID == nil || *legacy.BgmSubjectID != 100 || legacy.ExtEntryID != "k-s1" || legacy.MatchOrigin != "" ||
		legacy.Title != "Legacy title" || legacy.Summary != "Legacy summary" || legacy.BgmScore == nil {
		t.Fatalf("legacy fields: %+v", legacy)
	}
	for _, id := range []string{"manual", "agent"} {
		meta := readMetadata(t, store, id)
		if meta.MatchOrigin != id || meta.ExtEntryID != "" {
			t.Fatalf("protected binding: %+v", meta)
		}
	}
	book, _, err := store.BookMetadataByID(ctx, "auto-vol")
	if err != nil || book.BgmSubjectID != 101 || book.ExtEntryID != "kb1" || book.Title != "Official Edition Volume One" ||
		book.Summary != "Bangumi volume description" || book.ISBN != "978-4901926126" {
		t.Fatalf("merged book: %+v, %v", book, err)
	}
	seriesBindings, err := store.ListSeriesSourceBindings(ctx, "auto")
	if err != nil || len(seriesBindings) != 2 {
		t.Fatalf("series bindings: %+v %v", seriesBindings, err)
	}
	bookBindings, err := store.ListBookSourceBindings(ctx, "auto-vol")
	if err != nil || len(bookBindings) != 2 {
		t.Fatalf("book bindings: %+v %v", bookBindings, err)
	}
	candidates, err := store.ListSeriesCoverCandidates(ctx, "auto")
	wantRef := fmt.Sprintf("%x.png", sha256.Sum256(rawKomga))
	if err != nil || len(candidates) != 2 || candidates[0].CoverRef != wantRef || candidates[0].RankOrder != 0 ||
		!candidates[0].NSFWKnown || !candidates[0].IsNSFW || candidates[1].CoverKind != database.CoverKindBangumi {
		t.Fatalf("cover chain: %+v %v", candidates, err)
	}
	bookCovers, err := store.ListBookCoverCandidates(ctx, "auto-vol")
	if err != nil || len(bookCovers) != 2 || bookCovers[0].CoverRef != wantRef {
		t.Fatalf("book covers: %+v %v", bookCovers, err)
	}
	allowed := database.WithUserScope(ctx, database.UserScope{IsAdmin: true, AllowNSFW: true, ShowNSFW: true})
	assertCover := func(want []byte) {
		t.Helper()
		series, ok, err := thumbs.Get(allowed, "auto")
		if err != nil || !ok || !bytes.Equal(series.Bytes, want) {
			t.Fatalf("selected series cover: %v, %v", ok, err)
		}
	}
	assertCover(rawKomga)
	if _, err := m.SetLibrarySources(ctx, "lib", []string{TargetSourceBangumi, source.ID}); err != nil {
		t.Fatal(err)
	}
	title := "Title lock"
	locked := true
	series := database.Series{ID: "auto", LibraryID: "lib", Name: "Aria Official"}
	if _, err := store.PatchSeriesMetadata(ctx, series, database.SeriesMetadataPatch{Title: &title, TitleLock: &locked}); err != nil {
		t.Fatal(err)
	}
	run = runChainFixture(t, m, "lib", "force", nil)
	got = readMetadata(t, store, "auto")
	if run.Status != "success" || got.Title != title || got.ExtEntryID != "k-s1" || !got.IsNSFW {
		t.Fatalf("reordered: %+v %+v", run, got)
	}
	seriesCover, ok, err := thumbs.Get(allowed, "auto")
	if err != nil || !ok || seriesCover.MediaType != "image/jpeg" {
		t.Fatalf("Bangumi-first cover: %+v %v", seriesCover, err)
	}
	if err := m.DeleteSource(ctx, source.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(upstream); err != nil {
		t.Fatal(err)
	}
	candidates, err = store.ListSeriesCoverCandidates(ctx, "auto")
	if err != nil || len(candidates) != 2 || candidates[1].CoverRef != wantRef || candidates[1].RankOrder != 1 || !candidates[1].IsNSFW {
		t.Fatalf("deletion changed imported candidate: %+v %v", candidates, err)
	}
	if _, err := store.PatchSeriesMetadata(ctx, series, database.SeriesMetadataPatch{CoverSource: new("external")}); err != nil {
		t.Fatal(err)
	}
	assertCover(rawKomga)
}

func importChainBangumi(t *testing.T, bgm *bangumi.Service, root string) {
	t.Helper()
	var raw bytes.Buffer
	writer := zip.NewWriter(&raw)
	fixtures := []struct{ name, body string }{
		{"subject.jsonlines", `{"id":100,"type":1,"platform":1001,"name":"Away from this directory","name_cn":"Official Edition","series":true,"summary":"Bangumi description","score":8.6,"rank":24,"nsfw":false,"infobox":"{{Infobox animanga/Manga\n|结束=2025-01-01\n}}","tags":[{"name":" CLASSIC ","count":5},{"name":"Exclusive","count":3}]}
{"id":101,"type":1,"platform":1001,"name":"Official Edition Vol.01","name_cn":"Official Edition Volume One","summary":"Bangumi volume description","nsfw":false}
{"id":999,"type":1,"platform":1001,"name":"Aria Official","series":true,"summary":"Different work","nsfw":false}`},
		{"subject-relations.jsonlines", `{"subject_id":100,"related_subject_id":101,"relation_type":1003,"order":1}`},
	}
	for _, f := range fixtures {
		file, err := writer.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(file, f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "bangumi-fixture.zip")
	if err := os.WriteFile(path, raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := bgm.ImportArchiveZip(t.Context(), path, "fixture.zip", "2025-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
}

func runChainFixture(t *testing.T, m *Manager, legacy, mode string, ids []string) ScrapeRun {
	t.Helper()
	run, err := m.StartMultiScrape(t.Context(), legacy, mode, false, ids)
	if err != nil {
		t.Fatal(err)
	}
	waitForManager(t, func() bool {
		run, err = m.ScrapeRunByID(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return run.CompletedAt != ""
	})
	return run
}

func TestScrapeLibrarySelectionPersistsAndAppliesVersions(t *testing.T) {
	store, m, root := newTestManager(t, ManagerOptions{})
	ctx := t.Context()
	path := filepath.Join(root, "gallery.sqlite")
	createManagerGallery(t, path, "Blue River")
	source := addManagerSource(t, m, database.MetadataSource{Name: "Catalog", Kind: KindOfflineSQLite, LocalPath: path})
	syncManagerFixture(t, m, source, false, "success")
	for _, id := range []string{"a", "b", "c", "hidden"} {
		seedMetadataLibrary(t, store, id, id != "hidden")
		seedMetadataSeries(t, store, "series-"+id, id, "Blue River")
		if _, err := m.SetLibrarySources(ctx, id, []string{source.ID}); err != nil {
			t.Fatal(err)
		}
	}
	state := func(id string) database.LibraryMetadataChainState {
		t.Helper()
		state, err := store.GetLibraryMetadataChainState(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	verify := func(run ScrapeRun, single, jsonIDs string, n int) {
		t.Helper()
		var id sql.NullString
		var raw string
		err := store.DB().QueryRowContext(ctx, `SELECT library_id,library_ids_json FROM bangumi_scrape_runs WHERE id=?`, run.ID).Scan(&id, &raw)
		if err != nil || id.String != single || id.Valid != (single != "") || raw != jsonIDs || run.Status != "success" || run.TotalSeries != n {
			t.Fatalf("selection: %+v id=%+v json=%q err=%v", run, id, raw, err)
		}
	}
	partial := runChainFixture(t, m, "a", "incremental", []string{"b", "a", "b"})
	verify(partial, "", `["b","a"]`, 2)
	if partial.LibraryName != "b、a" || !state("a").NeedsRematch || !state("b").NeedsRematch {
		t.Fatalf("incremental applied versions: %+v", partial)
	}
	single := runChainFixture(t, m, "a", "force", nil)
	verify(single, "a", `["a"]`, 1)
	if state("a").NeedsRematch || !state("b").NeedsRematch {
		t.Fatal("force applied the wrong libraries")
	}
	execManagerFixture(t, store.DB(), `UPDATE libraries SET name='Renamed' WHERE id='b'`)
	prior, err := m.ScrapeRunByID(ctx, partial.ID)
	if err != nil || prior.LibraryName != "Renamed、a" {
		t.Fatalf("historical names: %+v %v", prior, err)
	}
	all := runChainFixture(t, m, "", "force", []string{"c", "a", "b"})
	verify(all, "", "", 3)
	for _, id := range []string{"a", "b", "c"} {
		if state(id).NeedsRematch {
			t.Fatalf("force did not apply %s", id)
		}
	}
	if !state("hidden").NeedsRematch {
		t.Fatal("hidden library version was applied")
	}
	if _, err := m.StartMultiScrape(ctx, "", "force", false, []string{"hidden"}); err != ErrUnavailable {
		t.Fatalf("hidden selection: %v", err)
	}
	if _, err := m.StartMultiScrape(ctx, "", "force", false, []string{"missing"}); err != sql.ErrNoRows {
		t.Fatalf("unknown selection: %v", err)
	}
}

func TestCancelSelectedLibraryKeepsForceReminder(t *testing.T) {
	started := make(chan struct{})
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	store, m, _ := newTestManager(t, ManagerOptions{HTTPClient: client})
	ctx := t.Context()
	for _, id := range []string{"a", "b", "c"} {
		seedMetadataLibrary(t, store, id, true)
	}
	seedMetadataSeries(t, store, "work", "a", "Uncatalogued Work")
	source := addManagerSource(t, m, database.MetadataSource{
		Name: "Online fixture", Kind: KindOfflineSQLite, OnlineEnabled: true, OnlineAPIURL: "https://source.example.test/suggest",
	})
	for _, id := range []string{"a", "b"} {
		if _, err := m.SetLibrarySources(ctx, id, []string{source.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.StartMultiScrape(ctx, "", "force", true, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(6 * time.Second):
		t.Fatal("force did not start")
	}
	m.CancelLibrary("c")
	if run := m.ScrapeRuns()[0]; run.Status != "running" {
		t.Fatalf("unselected library canceled task: %+v", run)
	}
	m.CancelLibrary("b")
	waitForManager(t, func() bool { return m.ScrapeRuns()[0].Status == "canceled" })
	for _, id := range []string{"a", "b"} {
		state, err := store.GetLibraryMetadataChainState(ctx, id)
		if err != nil || !state.NeedsRematch {
			t.Fatalf("canceled force applied %s: %+v %v", id, state, err)
		}
	}
}

func TestKomgaSidecarDoesNotRequireBookColumns(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "upstream.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	execManagerFixture(t, db, `
CREATE TABLE SERIES(ID TEXT, NAME TEXT, DELETED_DATE TEXT);
CREATE TABLE SERIES_METADATA(SERIES_ID TEXT, TITLE TEXT);
CREATE TABLE BOOK(incompatible_schema TEXT);
CREATE TABLE BOOK_METADATA(incompatible_schema TEXT);
CREATE TABLE THUMBNAIL_BOOK(incompatible_schema TEXT);
INSERT INTO SERIES VALUES('good','Visible','  '),('removed','Removed','2025-01-01');
INSERT INTO BOOK VALUES('unused');`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	probe, err := ProbeSQLiteFile(t.Context(), path)
	if err != nil || probe.BookCount != 0 {
		t.Fatalf("probe touched book counts: %+v %v", probe, err)
	}
	index := filepath.Join(root, "sidecar.sqlite")
	if err := BuildSidecarIndex(t.Context(), path, index, SchemaKomgaSQLiteV1, nil); err != nil {
		t.Fatal(err)
	}
	sidecar, err := OpenSidecarStore("source", path, index, SchemaKomgaSQLiteV1)
	if err != nil {
		t.Fatal(err)
	}
	defer sidecar.Close()
	hits, err := sidecar.SearchByTitle(t.Context(), "Visible", 5)
	if err != nil || len(hits) != 1 || hits[0].EntryID != "good" {
		t.Fatalf("index: %+v %v", hits, err)
	}
	if _, err := sidecar.GetEntry(t.Context(), "removed"); err != ErrEntryNotFound {
		t.Fatalf("deleted series: %v", err)
	}
}

func TestImportedCoverRejectsDamagedData(t *testing.T) {
	root := t.TempDir()
	png := encodeTestPNG(t)
	if _, err := writeImportedCover(root, png[:len(png)/2]); err != ErrInvalidImage {
		t.Fatalf("truncated image: %v", err)
	}
	if _, err := writeImportedCover(root, []byte(strings.Repeat("x", 64))); err != ErrInvalidImage {
		t.Fatalf("invalid image: %v", err)
	}
}
