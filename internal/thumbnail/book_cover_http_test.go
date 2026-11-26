package thumbnail_test

import (
	"bytes"
	"image"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/httpserver"
	"github.com/xJogger/fake-komga-115/internal/thumbnail"
)

func TestBookThumbnailAndMetadataHTTPPermissions(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ClaimAdmin(ctx, "fixture-admin", "fixture-password"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertLibrary(ctx, database.Library{ID: "library", Name: "Local", ProviderType: database.ProviderLocal, RootPath: dir, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`
INSERT INTO series(id,library_id,cid,name,relative_path,books_count,created_at,updated_at,seen_scan_id)
VALUES('series','library','series','Series','',1,'','','scan');
INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,created_at,updated_at,seen_scan_id)
VALUES('book','series','library','book.cbz','','Book.cbz',1,'',1,'','','scan')`); err != nil {
		t.Fatal(err)
	}
	reader, err := store.CreateUser(ctx, database.CreateUserInput{
		Username: "fixture-reader", Password: "fixture-password", AllowedLibraryIDs: []string{"library"},
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := store.CreateUser(ctx, database.CreateUserInput{Username: "fixture-blocked", Password: "fixture-password"})
	if err != nil {
		t.Fatal(err)
	}
	apiKey := func(id string) string {
		key, _, err := store.CreateAPIKeyForUser(ctx, id, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	adminKey, readerKey, blockedKey := apiKey(database.PrimaryAdminUserID), apiKey(reader.ID), apiKey(blocked.ID)
	thumbs, err := thumbnail.New(store, filepath.Join(dir, "series"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	handler := httpserver.New(store, nil, nil, nil, nil, thumbs, nil, nil, httpserver.RuntimeInfo{}, nil).Handler()
	request := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	const thumbPath = "/api/v1/books/book/thumbnail"
	const patchPath = "/api/v1/books/book/metadata"
	const patch = `{"title":"Edited book","titleLock":true}`
	if rec := request(http.MethodGet, thumbPath, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous thumbnail status=%d", rec.Code)
	}
	if rec := request(http.MethodGet, thumbPath, blockedKey, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("ungranted thumbnail status=%d", rec.Code)
	}
	if rec := request(http.MethodPatch, patchPath, readerKey, patch); rec.Code != http.StatusForbidden {
		t.Fatalf("reader patch status=%d", rec.Code)
	}
	if rec := request(http.MethodPatch, patchPath, adminKey, "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed patch status=%d", rec.Code)
	}
	if rec := request(http.MethodPatch, patchPath, adminKey, patch); rec.Code != http.StatusNoContent {
		t.Fatalf("admin patch status=%d", rec.Code)
	}
	if rec := request(http.MethodGet, "/api/v1/books/book", readerKey, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Edited book") {
		t.Fatalf("book DTO did not reflect patch: %d %s", rec.Code, rec.Body.String())
	}
	placeholder := request(http.MethodGet, thumbPath, readerKey, "")
	if placeholder.Code != http.StatusOK || placeholder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("placeholder status=%d cache=%q", placeholder.Code, placeholder.Header().Get("Cache-Control"))
	}
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 52, 79))); err != nil {
		t.Fatal(err)
	}
	if _, err := thumbs.SaveBookCustomCover(ctx, "book", img.Bytes()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{thumbPath, "/opds/v1.2/books/book/thumbnail", "/opds/v1.2/books/book/thumbnail/small"} {
		rec := request(http.MethodGet, path, readerKey, "")
		image, format, err := image.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" ||
			rec.Header().Get("Cache-Control") != "private, no-store" || err != nil || format != "jpeg" || image.Width != 52 || image.Height != 79 {
			t.Fatalf("thumbnail %s status=%d headers=%v size=%dx%d err=%v", path, rec.Code, rec.Header(), image.Width, image.Height, err)
		}
	}
	if _, err := store.DB().Exec(`UPDATE libraries SET enabled=0 WHERE id='library'`); err != nil {
		t.Fatal(err)
	}
	if rec := request(http.MethodGet, thumbPath, readerKey, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("hidden thumbnail status=%d", rec.Code)
	}
	if rec := request(http.MethodPatch, patchPath, adminKey, patch); rec.Code != http.StatusNotFound {
		t.Fatalf("hidden book patch status=%d", rec.Code)
	}
}
