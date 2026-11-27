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
		{ID: "offline-disabled", Kind: "bangumi_archive", Name: "Disabled archive", Enabled: false, Ready: true},
	} {
		if _, err := store.UpsertMetadataSource(ctx, src); err != nil {
			t.Fatal(err)
		}
	}
	for library, sources := range map[string][]string{
		"mixed": {"komga", "bangumi"}, "excluded": {"komga"}, "disabled": {"offline-disabled"},
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
 ('manual-binding','bangumi','100','909','manual','');
INSERT INTO book_cover_candidates(book_id,source_id,entry_id,cover_kind,cover_ref,updated_at)
VALUES ('priority-binding','komga','komga-vol','http_url','https://covers.example/fixture','');
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
		bindings, err := store.ListBookSourceBindings(ctx, want.id)
		if err != nil || len(bindings) == 0 || bindings[0].SourceID != "bangumi" || bindings[0].SeriesEntryID != "100" ||
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
		if err != nil || len(covers) != 1 || covers[0].CoverKind != database.CoverKindBangumi || covers[0].RankOrder != want.rank ||
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
	want := archiveDumpSignature(svc.ArchiveStatus())
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
