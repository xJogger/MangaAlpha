package bangumi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/database"
	"modernc.org/sqlite"
)

func TestTankobonBackfillOnImportAndDiffOnly(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := database.Open(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	execTankobonFixture(t, store, `
INSERT INTO libraries(id,name,root_cid,enabled,created_at,updated_at) VALUES
 ('default','Catalog','root-default',1,'',''), ('mixed','Mixed','root-mixed',1,'',''),
 ('excluded','Excluded','root-excluded',1,'',''), ('disabled','Disabled','root-disabled',1,'',''),
 ('hidden','Hidden','root-hidden',0,'',''), ('member','Member','root-member',1,'','');
INSERT INTO libraries(id,name,root_cid,enabled,created_at,updated_at,provider_type)
VALUES ('union','Union','root-union',1,'','','union');
INSERT INTO union_layers(union_library_id,member_library_id,priority,created_at) VALUES ('union','member',1,'');
`)
	for _, src := range []database.MetadataSource{
		{ID: "komga", Kind: "offline_sqlite", Name: "Komga", Enabled: true, Ready: true},
		{ID: "custom-archive", Kind: "bangumi_archive", Name: "Custom archive", Enabled: true, Ready: true},
		{ID: "offline-disabled", Kind: "bangumi_archive", Name: "Disabled archive", Enabled: false, Ready: true},
	} {
		if _, err := store.UpsertMetadataSource(ctx, src); err != nil {
			t.Fatal(err)
		}
	}
	for library, sources := range map[string][]string{
		"mixed": {"komga", "custom-archive"}, "excluded": {"komga"}, "disabled": {"offline-disabled"},
	} {
		if err := store.SetLibraryMetadataSources(ctx, library, sources); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct{ id, library, origin string }{
		{"new", "default", "manual"}, {"legacy", "default", ""}, {"locked", "default", "agent"},
		{"manual", "default", "auto"}, {"agent", "default", "auto"}, {"manual-binding", "default", ""},
		{"old-binding", "default", ""}, {"automatic", "default", "auto"},
		{"priority-binding", "mixed", "auto"}, {"priority-field", "mixed", "auto"}, {"no-priority-binding", "mixed", "auto"},
		{"custom-manual-binding", "mixed", "auto"}, {"custom-agent-binding", "mixed", "auto"},
		{"excluded", "excluded", "auto"}, {"disabled", "disabled", "auto"},
		{"hidden", "hidden", "auto"}, {"member", "member", "auto"}, {"union", "union", "auto"},
	} {
		seedTankobonFixture(t, store, item.id, item.library, item.origin)
	}
	execTankobonFixture(t, store, `
INSERT INTO book_metadata(book_id,title,summary,release_date,isbn,ext_source_id,match_origin) VALUES
 ('legacy','Legacy title','Legacy summary','','','',''),
 ('locked','Locked title','','1980-01-01','locked-isbn','','auto'),
 ('manual','Manual title','','','','','manual'),
 ('agent','Agent title','','','','','agent'),
 ('old-binding','Old binding','','','','',''),
 ('automatic','Old auto title','Old auto summary','','','','auto'),
 ('priority-binding','Komga binding title','','2001-02-03','','','auto'),
 ('priority-field','Komga projection title','Komga summary','','','komga','auto'),
 ('no-priority-binding','Auto title','','','','','auto');
UPDATE book_metadata SET title_lock=1,summary_lock=1,release_date_lock=1,isbn_lock=1,
 number='1',number_lock=1,authors_json='[{"name":"Fixture author","role":"writer"}]',authors_lock=1 WHERE book_id='locked';
UPDATE book_metadata SET bgm_subject_id=909 WHERE book_id='old-binding';
INSERT INTO book_source_bindings(book_id,source_id,series_entry_id,entry_id,match_origin,updated_at) VALUES
 ('priority-binding','komga','komga-parent','komga-vol','auto',''),
 ('manual-binding','bangumi','100','909','manual',''),
 ('custom-manual-binding','custom-archive','100','909','manual',''),
 ('custom-agent-binding','custom-archive','100','909','agent',''),
 ('priority-field','custom-archive','100','909','auto','');
INSERT INTO book_cover_candidates(book_id,source_id,entry_id,cover_kind,cover_ref,updated_at)
VALUES ('priority-binding','komga','komga-vol','http_url','https://covers.example/fixture',''),
 ('priority-field','custom-archive','909','bangumi','https://covers.example/old','');
`)

	svc, err := NewService(store, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	dumpPath := filepath.Join(dir, "fixture.zip")
	writeSyntheticBangumiZip(t, dumpPath)
	if err := svc.ImportArchiveZip(ctx, dumpPath, "fixture.zip", "2025-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	waitTankobonCompletion(t, svc)

	for _, want := range []struct {
		id, title, summary, date, isbn string
		rank                           int
	}{
		{"new", "灌篮高手 (1)", "第一卷", "1991-02-08", "978-4088511283", 0},
		{"legacy", "Legacy title", "Legacy summary", "1991-02-08", "978-4088511283", 0},
		{"locked", "Locked title", "", "1980-01-01", "locked-isbn", 0},
		{"automatic", "灌篮高手 (1)", "第一卷", "1991-02-08", "978-4088511283", 0},
		{"priority-binding", "Komga binding title", "第一卷", "2001-02-03", "978-4088511283", 1},
		{"priority-field", "Komga projection title", "Komga summary", "1991-02-08", "978-4088511283", 1},
		{"no-priority-binding", "灌篮高手 (1)", "第一卷", "1991-02-08", "978-4088511283", 1},
		{"union", "灌篮高手 (1)", "第一卷", "1991-02-08", "978-4088511283", 0},
	} {
		meta, ok, err := store.BookMetadataByID(ctx, want.id)
		if err != nil || !ok || meta.Title != want.title || meta.Summary != want.summary || meta.ReleaseDate != want.date || meta.ISBN != want.isbn ||
			meta.BgmSubjectID != 101 || meta.BgmCoverURL != CoverEndpointURL(101) || meta.BgmIsNSFW {
			t.Errorf("%s metadata = %+v, exists=%t err=%v", want.id, meta, ok, err)
		}
		sourceID, bindingCount := "bangumi", 1
		if want.rank == 1 {
			sourceID = "custom-archive"
		}
		if want.id == "priority-binding" {
			bindingCount++
		}
		bindings, err := store.ListBookSourceBindings(ctx, want.id)
		if err != nil || len(bindings) != bindingCount || bindings[0].SourceID != sourceID || bindings[0].SeriesEntryID != "100" ||
			bindings[0].EntryID != "101" || bindings[0].MatchOrigin != "auto" {
			t.Errorf("%s bindings = %+v, err=%v", want.id, bindings, err)
		}
		covers, err := store.ListBookCoverCandidates(ctx, want.id)
		if want.id == "priority-binding" {
			if len(covers) != 2 || covers[0].SourceID != "komga" || covers[0].CoverRef != "https://covers.example/fixture" {
				t.Fatalf("Komga cover preference changed: %+v", covers)
			}
			covers = covers[1:]
		}
		if err != nil || len(covers) != 1 || covers[0].SourceID != sourceID || covers[0].CoverKind != database.CoverKindBangumi || covers[0].RankOrder != want.rank ||
			covers[0].EntryID != "101" || !covers[0].NSFWKnown || covers[0].IsNSFW || covers[0].CoverRef != CoverEndpointURL(101) {
			t.Errorf("%s candidates = %+v, err=%v", want.id, covers, err)
		}
	}
	locked, _, err := store.BookMetadataByID(ctx, "locked")
	if err != nil || !locked.TitleLock || !locked.SummaryLock || !locked.ReleaseDateLock || !locked.ISBNLock ||
		!locked.NumberLock || locked.Number != "1" || !locked.AuthorsLock || len(locked.Authors) != 1 {
		t.Fatalf("locked fields changed: %+v, %v", locked, err)
	}
	for _, skip := range []struct {
		id, title string
		subjectID int64
		exists    bool
	}{
		{"manual", "Manual title", 0, true}, {"agent", "Agent title", 0, true},
		{"old-binding", "Old binding", 909, true}, {"manual-binding", "", 0, false},
		{"custom-manual-binding", "", 0, false}, {"custom-agent-binding", "", 0, false},
		{"excluded", "", 0, false}, {"disabled", "", 0, false}, {"hidden", "", 0, false}, {"member", "", 0, false},
	} {
		book, ok, err := store.BookMetadataByID(ctx, skip.id)
		if err != nil || ok != skip.exists || book.Title != skip.title || book.BgmSubjectID != skip.subjectID {
			t.Errorf("protected %s = %+v, exists=%t err=%v", skip.id, book, ok, err)
		}
		candidates, err := store.ListBookCoverCandidates(ctx, skip.id)
		if err != nil || len(candidates) != 0 {
			t.Errorf("protected %s candidates = %+v, %v", skip.id, candidates, err)
		}
	}
	for bookID, origin := range map[string]string{"custom-manual-binding": "manual", "custom-agent-binding": "agent"} {
		bindings, err := store.ListBookSourceBindings(ctx, bookID)
		if err != nil || len(bindings) != 1 || bindings[0].SourceID != "custom-archive" || bindings[0].EntryID != "909" || bindings[0].MatchOrigin != origin {
			t.Errorf("protected %s bindings = %+v, %v", bookID, bindings, err)
		}
	}

	// Block any writes even if an attempted upsert would leave values identical.
	for _, table := range []string{"book_metadata", "book_source_bindings", "book_cover_candidates", "shelf_revision", "settings"} {
		for _, action := range []string{"INSERT", "UPDATE", "DELETE"} {
			execTankobonFixture(t, store, fmt.Sprintf(
				"CREATE TRIGGER diff_%s_%s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT,'unexpected repeat write'); END",
				table, action, action, table))
		}
	}
	if n, err := svc.syncOfflineTankobonBooks(ctx, true); err != nil || n != 0 {
		t.Fatalf("forced repeat = %d, %v", n, err)
	}
}

var cancelBackfillFixture atomic.Pointer[context.CancelFunc]
var changeBackfillChainFixture atomic.Pointer[func() error]

func init() {
	err := sqlite.RegisterScalarFunction("cancel_tankobon_backfill_fixture", 0,
		func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
			if cancel := cancelBackfillFixture.Load(); cancel != nil {
				(*cancel)()
			}
			return nil, nil
		})
	if err != nil {
		panic(err)
	}
	err = sqlite.RegisterScalarFunction("change_tankobon_chain_fixture", 0,
		func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
			if change := changeBackfillChainFixture.Load(); change != nil {
				return int64(0), (*change)()
			}
			return int64(0), nil
		})
	if err != nil {
		panic(err)
	}
}

func TestTankobonBackfillResetsChainRetriesAfterSuccessfulBatch(t *testing.T) {
	ctx := t.Context()
	store, err := database.Open(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	execTankobonFixture(t, store, `
INSERT INTO libraries(id,name,root_cid,enabled,created_at,updated_at) VALUES ('default','Catalog','root',1,'','');
`)
	if _, err := store.UpsertMetadataSource(ctx, database.MetadataSource{
		ID: "komga", Kind: "offline_sqlite", Name: "Komga", Enabled: true, Ready: true,
	}); err != nil {
		t.Fatal(err)
	}
	for i := range tankobonSeriesBatchSize + 1 {
		seedTankobonFixture(t, store, fmt.Sprintf("issue-%03d", i), "default", "auto")
	}

	archiveDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archiveDB.Close()
	archiveDB.SetMaxOpenConns(1)
	if _, err := archiveDB.ExecContext(ctx, `
CREATE TABLE bgm_subject_relations(subject_id,related_subject_id,relation_type,sort_order);
CREATE VIEW bgm_subjects AS
 SELECT 101 AS id, 'Fixture Vol.1' AS name, '' AS name_cn, '' AS release_date, '' AS summary,
 change_tankobon_chain_fixture() AS is_nsfw;
INSERT INTO bgm_subject_relations VALUES (100,101,?,1);
`, relationTypeTankobon); err != nil {
		t.Fatal(err)
	}
	// Each archive lookup runs after the plan is read and before its batch is applied.
	// Force three conflicts in the first batch and two in the second.
	calls := 0
	change := func() error {
		calls++
		switch calls {
		case 1, 3, 6:
			return store.SetLibraryMetadataSources(ctx, "default", []string{"komga", "bangumi"})
		case 2, 5:
			return store.SetLibraryMetadataSources(ctx, "default", []string{"bangumi"})
		default:
			return nil
		}
	}
	changeBackfillChainFixture.Store(&change)
	defer changeBackfillChainFixture.Store(nil)
	svc := &Service{store: store, archive: &ArchiveIndex{db: archiveDB, status: ArchiveStatus{Ready: true}}}
	if n, err := svc.syncOfflineTankobonBooks(ctx, false); n != tankobonSeriesBatchSize+1 || err != nil || calls != 7 {
		t.Fatalf("backfill updated %d books in %d attempts: %v", n, calls, err)
	}
}

func TestTankobonBackfillResumesOnStartupAfterCancellation(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := database.Open(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	execTankobonFixture(t, store, `
INSERT INTO libraries(id,name,root_cid,enabled,created_at,updated_at) VALUES ('default','Catalog','root',1,'','');
`)
	for i := range tankobonSeriesBatchSize + 1 {
		seedTankobonFixture(t, store, fmt.Sprintf("issue-%03d", i), "default", "manual")
	}
	index, err := NewArchiveIndex(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	dumpPath := filepath.Join(dir, "fixture.zip")
	writeSyntheticBangumiZip(t, dumpPath)
	if err := index.ImportFromZipFile(ctx, dumpPath, "fixture.zip", "2025-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSetting(ctx, settingArchiveSyncedDump, archiveDumpSignature(index.Status())); err != nil {
		t.Fatal(err)
	}
	execTankobonFixture(t, store, `
CREATE TRIGGER interrupt_backfill AFTER INSERT ON book_metadata WHEN NEW.book_id='issue-050'
BEGIN SELECT cancel_tankobon_backfill_fixture(); END;
`)
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cancelBackfillFixture.Store(&cancel)
	defer cancelBackfillFixture.Store(nil)
	svc := &Service{store: store, archive: index}
	n, err := svc.syncOfflineTankobonBooks(cancelCtx, false)
	if n != tankobonSeriesBatchSize || !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted backfill = %d, %v", n, err)
	}
	if _, err := store.GetSetting(ctx, settingArchiveSyncedBooksDump); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("interrupted backfill marked complete: %v", err)
	}
	if _, ok, err := store.BookMetadataByID(ctx, "issue-050"); ok || err != nil {
		t.Fatalf("interrupted batch committed book metadata: exists=%t err=%v", ok, err)
	}
	first, ok, err := store.BookMetadataByID(ctx, "issue-000")
	if !ok || err != nil || first.BgmSubjectID != 101 {
		t.Fatalf("initial batch did not commit: %+v %v", first, err)
	}

	execTankobonFixture(t, store, "DROP TRIGGER interrupt_backfill")
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewService(store, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	waitTankobonCompletion(t, restarted)
	last, ok, err := store.BookMetadataByID(ctx, "issue-050")
	if !ok || err != nil || last.BgmSubjectID != 101 {
		t.Fatalf("resumed book = %+v, exists=%t err=%v", last, ok, err)
	}
	again, _, err := store.BookMetadataByID(ctx, "issue-000")
	if err != nil || !again.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("completed batch was rewritten on restart: %v -> %v, err=%v", first.UpdatedAt, again.UpdatedAt, err)
	}
}

func seedTankobonFixture(t *testing.T, store *database.Store, id, library, origin string) {
	t.Helper()
	execTankobonFixture(t, store, `
INSERT INTO series(id,library_id,cid,name,relative_path,created_at,updated_at,seen_scan_id) VALUES (?,?,?,'Fixture','Fixture','','','')`, id, library, id)
	execTankobonFixture(t, store, `
INSERT INTO series_metadata(series_id,bgm_subject_id,match_origin,updated_at) VALUES (?,100,?,'')`, id, origin)
	// The archives intentionally don't exist: backfill can work from names and
	// SQLite metadata alone, including for remote and union catalogs.
	execTankobonFixture(t, store, `
INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,created_at,updated_at,seen_scan_id)
VALUES (?,?,?,?,?,'Fixture Vol.1.cbz',100,'',1,'','','')`, id, id, library, id, id)
}

func execTankobonFixture(t *testing.T, store *database.Store, statement string, args ...any) {
	t.Helper()
	if _, err := store.DB().ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatal(err)
	}
}

func waitTankobonCompletion(t *testing.T, svc *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	want := tankobonBackfillSignature(svc.ArchiveStatus())
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if got, _ := svc.store.GetSetting(ctx, settingArchiveSyncedBooksDump); got == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("tankobon backfill did not finish: %+v", svc.ArchiveStatus())
		case <-ticker.C:
		}
	}
}

func TestTankobonBackfillOneshotFallbackAndAlgoUpgrade(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := database.Open(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	execTankobonFixture(t, store, `
INSERT INTO libraries(id,name,root_cid,enabled,created_at,updated_at) VALUES ('default','Catalog','root',1,'','');
`)
	seedNamed := func(seriesID, bookID, bookName string, subjectID int64, seriesNSFW bool) {
		execTankobonFixture(t, store, `
INSERT INTO series(id,library_id,cid,name,relative_path,created_at,updated_at,seen_scan_id) VALUES (?,'default',?,?,?,?,?,'')`,
			seriesID, seriesID, seriesID, seriesID, "", "")
		execTankobonFixture(t, store, `
INSERT INTO series_metadata(series_id,bgm_subject_id,is_nsfw,match_origin,updated_at) VALUES (?,?,?,'auto','')`,
			seriesID, subjectID, seriesNSFW)
		execTankobonFixture(t, store, `
INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,created_at,updated_at,seen_scan_id)
VALUES (?,?,'default',?,?,?,100,'',1,'','','')`, bookID, seriesID, bookID, seriesID, bookName)
	}

	// 1. Single-book series whose parent (100 in synthetic zip? Note: writeSyntheticBangumiZip creates 100 & 101 with relation 100->101!)
	// Use a dedicated parent subject without tankobon relations: 200 (safe), 201 (parent NSFW=1), 202 (series NSFW=1), 999 (missing from archive).
	seedNamed("s-safe", "b-safe", "Short Story.cbz", 200, false)
	seedNamed("s-parent-nsfw", "b-parent-nsfw", "Adult Short.cbz", 201, false)
	seedNamed("s-series-nsfw", "b-series-nsfw", "Local Flag Short.cbz", 200, true)
	seedNamed("s-missing", "b-missing", "Missing Parent.cbz", 999, false)
	seedNamed("s-chapter", "b-chapter", "第1话.cbz", 200, false)

	// Multi-book series with 0 tankobon relations must NOT get oneshot_fallback.
	seedNamed("s-multi", "b-multi-1", "Vol.01.cbz", 200, false)
	execTankobonFixture(t, store, `
INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,created_at,updated_at,seen_scan_id)
VALUES ('b-multi-2','s-multi','default','b-multi-2','s-multi','Vol.02.cbz',100,'',2,'','','')`)

	archiveDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archiveDB.Close()
	archiveDB.SetMaxOpenConns(1)
	if _, err := archiveDB.ExecContext(ctx, `
CREATE TABLE bgm_subject_relations(subject_id,related_subject_id,relation_type,sort_order);
CREATE TABLE bgm_subjects(id INTEGER PRIMARY KEY, name TEXT, name_cn TEXT, release_date TEXT, summary TEXT, is_nsfw INTEGER);
INSERT INTO bgm_subjects(id,name,name_cn,release_date,summary,is_nsfw) VALUES
 (200,'Safe Parent','安全单篇','','',0),
 (201,'NSFW Parent','成人单篇','','',1);
`); err != nil {
		t.Fatal(err)
	}

	status := ArchiveStatus{Ready: true, DumpName: "dump-test.zip", DumpCreatedAt: "2026-01-01", DumpDigest: "sha256:abc"}
	svc := &Service{store: store, archive: &ArchiveIndex{db: archiveDB, status: status}}

	// Simulate an older v1/pre-algo completion signature stored before upgrade.
	if err := store.SetSetting(ctx, settingArchiveSyncedBooksDump, archiveDumpSignature(status)); err != nil {
		t.Fatal(err)
	}
	n, err := svc.syncOfflineTankobonBooks(ctx, false)
	if err != nil || n != 3 {
		t.Fatalf("expected 3 oneshot_fallback books backfilled on algo upgrade, got n=%d err=%v", n, err)
	}
	sig, err := store.GetSetting(ctx, settingArchiveSyncedBooksDump)
	if err != nil || sig != tankobonBackfillSignature(status) {
		t.Fatalf("expected upgraded signature %q, got %q err=%v", tankobonBackfillSignature(status), sig, err)
	}

	for _, tc := range []struct {
		bookID    string
		subjectID int64
		wantNSFW  bool
	}{
		{"b-safe", 200, false},
		{"b-parent-nsfw", 201, true},
		{"b-series-nsfw", 200, true},
	} {
		bm, ok, err := store.BookMetadataByID(ctx, tc.bookID)
		if err != nil || !ok || bm.BgmSubjectID != tc.subjectID || bm.BgmIsNSFW != tc.wantNSFW ||
			bm.BgmCoverURL != CoverEndpointURL(tc.subjectID) {
			t.Fatalf("%s metadata = %+v, ok=%v err=%v", tc.bookID, bm, ok, err)
		}
		bindings, err := store.ListBookSourceBindings(ctx, tc.bookID)
		if err != nil || len(bindings) != 1 || bindings[0].EntryID != fmt.Sprint(tc.subjectID) {
			t.Fatalf("%s bindings = %+v, err=%v", tc.bookID, bindings, err)
		}
		covers, err := store.ListBookCoverCandidates(ctx, tc.bookID)
		if err != nil || len(covers) != 1 || !covers[0].NSFWKnown || covers[0].IsNSFW != tc.wantNSFW {
			t.Fatalf("%s covers = %+v, err=%v", tc.bookID, covers, err)
		}
	}

	for _, skipped := range []string{"b-missing", "b-chapter", "b-multi-1", "b-multi-2"} {
		if bm, ok, err := store.BookMetadataByID(ctx, skipped); err != nil || ok {
			t.Fatalf("%s should not be backfilled, got %+v ok=%v err=%v", skipped, bm, ok, err)
		}
	}

	// Second pass with same algo signature is a no-op, and forced pass writes 0 rows.
	if n2, err := svc.syncOfflineTankobonBooks(ctx, false); err != nil || n2 != 0 {
		t.Fatalf("expected idempotent 0 on repeat, got %d, %v", n2, err)
	}
	if n3, err := svc.syncOfflineTankobonBooks(ctx, true); err != nil || n3 != 0 {
		t.Fatalf("expected 0 diff writes on forced repeat, got %d, %v", n3, err)
	}
}
