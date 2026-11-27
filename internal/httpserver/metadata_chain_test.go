package httpserver

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/extmeta"
	"github.com/xJogger/fake-komga-115/internal/importedcover"
)

func TestMetadataChainHTTPImportedCoverSurvivesSourceDeletion(t *testing.T) {
	srv, store, bgm := newV27TestServer(t, "")
	ctx := t.Context()
	if _, err := store.ClaimAdmin(ctx, "fixture-owner", "FixturePassword123!"); err != nil {
		t.Fatal(err)
	}
	adminKey, _, err := store.CreateAPIKey(ctx, "fixture admin")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.CreateUser(ctx, database.CreateUserInput{
		Username: "fixture-reader", Password: "FixturePassword123!", AllLibraries: true, AllowNSFW: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	readerKey, _, err := store.CreateAPIKeyForUser(ctx, reader.ID, "fixture reader")
	if err != nil {
		t.Fatal(err)
	}

	manager, err := extmeta.NewManager(store, bgm, nil, srv.runtime.DataDir, nil, extmeta.ManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	store.SetImportedCoverCleanup(manager.RemoveUnusedImportedCovers)
	handler := srv.WithMetadata(manager).Handler()
	request := func(key, method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-API-Key", key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("%s %s: got %d want %d, body %s", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	raw := makeTestPNGBytes(t, 34, 102, 212)
	upstream := filepath.Join(t.TempDir(), "fixture-komga.sqlite")
	db, err := sql.Open("sqlite", upstream)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE SERIES(ID TEXT PRIMARY KEY,NAME TEXT,DELETED_DATE TEXT);
CREATE TABLE SERIES_METADATA(SERIES_ID TEXT,TITLE TEXT,AGE_RATING INT);
CREATE TABLE BOOK(ID TEXT PRIMARY KEY,SERIES_ID TEXT,NAME TEXT,NUMBER INT,DELETED_DATE TEXT);
CREATE TABLE BOOK_METADATA(BOOK_ID TEXT,TITLE TEXT,NUMBER_SORT REAL);
CREATE TABLE THUMBNAIL_SERIES(ID TEXT,SERIES_ID TEXT,SELECTED INT,THUMBNAIL BLOB);
CREATE TABLE THUMBNAIL_BOOK(ID TEXT,BOOK_ID TEXT,SELECTED INT,THUMBNAIL BLOB);
INSERT INTO SERIES VALUES ('s','Fixture Work',NULL);
INSERT INTO SERIES_METADATA VALUES ('s','Fixture Work',18);
INSERT INTO BOOK VALUES ('b','s','Fixture Work Vol.01',1,'');
INSERT INTO BOOK_METADATA VALUES ('b','Fixture Work Vol.01',1.0);
INSERT INTO THUMBNAIL_BOOK VALUES ('cover','b',1,?);
 `, raw); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO libraries(id,name,root_cid,enabled,created_at,updated_at) VALUES ('lib','Catalog','root',1,'','');
INSERT INTO series(id,library_id,cid,name,relative_path,books_count,created_at,updated_at,seen_scan_id)
VALUES ('work','lib','work','Fixture Work','Fixture Work',1,'','','');
INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,created_at,updated_at,seen_scan_id)
VALUES ('volume','work','lib','volume','work','Fixture Work Vol.01.cbz',128,'',1,'','','');
INSERT INTO series_metadata(series_id,is_nsfw,nsfw_lock,updated_at) VALUES ('work',0,1,'');
 `); err != nil {
		t.Fatal(err)
	}
	source := decodeMetadataFixture[extmeta.SourceView](t, request(adminKey, http.MethodPost, "/admin/api/metadata/sources",
		fmt.Sprintf(`{"kind":"offline_sqlite","name":"Fixture SQLite","localPath":%q}`, upstream), http.StatusCreated))
	if source.ImportCovers {
		t.Fatal("source default enables cover import")
	}

	request(adminKey, http.MethodPost, "/admin/api/metadata/sources/"+source.ID+"/sync", `{}`, http.StatusAccepted)
	wait := func(done func() bool) {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for !done() {
			if time.Now().After(deadline) {
				t.Fatal("metadata task did not finish")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait(func() bool { runs := manager.Syncs(); return len(runs) > 0 && runs[0].Status == "success" })
	chainPath := "/admin/api/libraries/lib/metadata-sources"
	chain := decodeMetadataFixture[struct {
		ChainState database.LibraryMetadataChainState `json:"chainState"`
	}](t,
		request(adminKey, http.MethodPut, chainPath, fmt.Sprintf(`{"sourceIds":[%q]}`, source.ID), http.StatusOK))
	if !chain.ChainState.NeedsRematch {
		t.Fatalf("chain state: %+v", chain)
	}

	scrape := func() {
		t.Helper()
		run := decodeMetadataFixture[extmeta.ScrapeRun](t, request(adminKey, http.MethodPost, "/admin/api/metadata/scrape",
			`{"libraryIds":["lib"],"libraryId":"unused","mode":"force"}`, http.StatusAccepted))
		if len(run.LibraryIDs) != 1 || run.LibraryID != "lib" {
			t.Fatalf("scrape selection: %+v", run)
		}
		wait(func() bool {
			latest, err := manager.ScrapeRunByID(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if latest.CompletedAt == "" {
				return false
			}
			if latest.Status != "success" || latest.Matched != 1 {
				t.Fatalf("scrape: %+v", latest)
			}
			return true
		})
	}
	assertBookCoverSummary := func(want bool) {
		t.Helper()
		book := decodeMetadataFixture[struct {
			RawBookMeta struct{ HasExtCover bool }
		}](t, request(readerKey, http.MethodGet, "/admin/api/books/volume/detail", "", http.StatusOK))
		series := decodeMetadataFixture[struct {
			BookExtras map[string]struct{ HasExtCover bool }
		}](t, request(readerKey, http.MethodGet, "/admin/api/series/work/detail", "", http.StatusOK))
		extra, ok := series.BookExtras["volume"]
		if !ok || book.RawBookMeta.HasExtCover != want || extra.HasExtCover != want {
			t.Fatalf("cover summary: book=%+v series=%+v want=%v", book, series, want)
		}
	}
	scrape()
	series, err := store.SeriesMetadataByID(ctx, database.Series{ID: "work", Name: "Fixture Work"})
	if err != nil || series.ExtEntryID != "s" || series.ExtCoverURL != "" {
		t.Fatalf("metadata-only match: %+v %v", series, err)
	}
	if rows, err := store.ListSeriesCoverCandidates(ctx, "work"); err != nil || len(rows) != 0 {
		t.Fatalf("disabled series import: %+v %v", rows, err)
	}
	if rows, err := store.ListBookCoverCandidates(ctx, "volume"); err != nil || len(rows) != 0 {
		t.Fatalf("disabled book import: %+v %v", rows, err)
	}
	assertBookCoverSummary(false)
	if _, err := os.Stat(filepath.Join(srv.runtime.DataDir, "imported-covers")); !os.IsNotExist(err) {
		t.Fatalf("disabled import wrote files: %v", err)
	}

	sourcePath := "/admin/api/metadata/sources/" + source.ID
	updated := decodeMetadataFixture[extmeta.SourceView](t, request(adminKey, http.MethodPut, sourcePath, `{"importCovers":true}`, http.StatusOK))
	if !updated.ImportCovers {
		t.Fatal("cover import setting did not persist")
	}
	overview := func() map[string]database.LibraryMetadataChainState {
		t.Helper()
		body := decodeMetadataFixture[struct {
			ChainStates map[string]database.LibraryMetadataChainState `json:"libraryChainStates"`
			Archive     *bangumi.ArchiveStatus                        `json:"archive"`
			Sources     []extmeta.SourceView                          `json:"sources"`
		}](t, request(adminKey, http.MethodGet, "/admin/api/metadata/overview", "", http.StatusOK))
		if body.Archive == nil {
			t.Fatal("overview omitted archive status")
		}
		return body.ChainStates
	}
	if !overview()["lib"].NeedsRematch {
		t.Fatal("source change did not invalidate chain")
	}
	scrape()
	if overview()["lib"].NeedsRematch {
		t.Fatal("successful force did not apply the chain version")
	}
	ref := fmt.Sprintf("%x.png", sha256.Sum256(raw))
	rows, err := store.ListSeriesCoverCandidates(ctx, "work")
	if err != nil || len(rows) != 1 || rows[0].CoverRef != ref || !rows[0].IsNSFW || !rows[0].NSFWKnown {
		t.Fatalf("series candidates: %+v %v", rows, err)
	}
	bookRows, err := store.ListBookCoverCandidates(ctx, "volume")
	if err != nil || len(bookRows) != 1 || bookRows[0].CoverRef != ref || !bookRows[0].IsNSFW {
		t.Fatalf("book candidates: %+v %v", bookRows, err)
	}
	request(adminKey, http.MethodDelete, sourcePath, "", http.StatusNoContent)
	if err := os.Remove(upstream); err != nil {
		t.Fatal(err)
	}
	after, err := store.ListSeriesCoverCandidates(ctx, "work")
	if err != nil || len(after) != 1 || after[0] != rows[0] {
		t.Fatalf("series cover changed after deletion: %+v %v", after, err)
	}
	afterBooks, err := store.ListBookCoverCandidates(ctx, "volume")
	if err != nil || len(afterBooks) != 1 || afterBooks[0] != bookRows[0] {
		t.Fatalf("book cover changed after deletion: %+v %v", afterBooks, err)
	}
	for _, path := range []string{"/api/v1/series/work/thumbnail", "/api/v1/books/volume/thumbnail"} {
		cover := request(readerKey, http.MethodGet, path, "", http.StatusOK)
		if bytes.Equal(cover.Body.Bytes(), raw) {
			t.Fatalf("restricted user received imported cover at %s", path)
		}
	}
	if _, err := store.SetUserShowNSFW(ctx, reader.ID, true); err != nil {
		t.Fatal(err)
	}
	assertBookCoverSummary(true)
	for _, path := range []string{"/api/v1/series/work/thumbnail", "/api/v1/books/volume/thumbnail"} {
		cover := request(readerKey, http.MethodGet, path, "", http.StatusOK)
		if !bytes.Equal(cover.Body.Bytes(), raw) || cover.Header().Get("Content-Type") != "image/png" ||
			cover.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("imported image unavailable after deleting source at %s, headers %v", path, cover.Header())
		}
	}
	var indexes int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM zip_indexes`).Scan(&indexes); err != nil || indexes != 0 {
		t.Fatalf("matching or thumbnail browsing opened an archive: count=%d err=%v", indexes, err)
	}
	request(adminKey, http.MethodDelete, "/admin/api/libraries/lib", "", http.StatusNoContent)
	if _, _, err := importedcover.Read(filepath.Join(srv.runtime.DataDir, "imported-covers"), ref); !os.IsNotExist(err) {
		t.Fatalf("catalog deletion left an orphaned import: %v", err)
	}
}
