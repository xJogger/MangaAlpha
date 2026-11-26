package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/extmeta"
)

func TestMetadataSubscribeErrorResponses(t *testing.T) {
	srv, store, bgm, _ := setupV27TestServer(t, "")
	ctx := t.Context()
	if _, err := store.ClaimAdmin(ctx, "fixture-owner", "FixturePassword123!"); err != nil {
		t.Fatal(err)
	}
	key, _, err := store.CreateAPIKey(ctx, "fixture admin")
	if err != nil {
		t.Fatal(err)
	}
	safeClient := extmeta.NewSafeHTTPClient("", time.Second)
	t.Cleanup(safeClient.CloseIdleConnections)
	client := &http.Client{Transport: bangumiTestTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		switch r.URL.Path {
		case "/blocked":
			return safeClient.Transport.RoundTrip(r)
		case "/invalid-proxy":
			return nil, extmeta.ErrInvalidProxyURL
		case "/invalid-manifest":
			_, _ = io.WriteString(w, `{"schema":"fixture-secret"}`)
		case "/timeout":
			return nil, &net.DNSError{Err: "fixture-secret", IsTimeout: true}
		case "/deadline":
			return nil, fmt.Errorf("upstream: %w", context.DeadlineExceeded)
		case "/dns":
			return nil, &net.DNSError{Err: "fixture-secret", Name: r.URL.Hostname()}
		case "/tls":
			return nil, fmt.Errorf("tls: fixture-secret")
		case "/reset":
			return nil, fmt.Errorf("connection reset by peer: fixture-secret")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
		return w.Result(), nil
	})}
	manager, err := extmeta.NewManager(store, bgm, nil, srv.runtime.DataDir, nil, extmeta.ManagerOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	srv.WithMetadata(manager)
	handler := srv.Handler()
	for _, tc := range []struct {
		path, code, message string
		status              int
	}{
		{"/blocked", "METADATA_SSRF_BLOCKED", "配置显式代理", http.StatusBadRequest},
		{"/invalid-proxy", "INVALID_PROXY_URL", "代理 URL 无效", http.StatusBadRequest},
		{"/invalid-manifest", "INVALID_METADATA_MANIFEST", "repo.json", http.StatusBadRequest},
		{"/missing", "METADATA_UPSTREAM_FAILED", "HTTP 404", http.StatusBadGateway},
		{"/timeout", "REQUEST_TIMEOUT", "超时", http.StatusGatewayTimeout},
		{"/deadline", "REQUEST_TIMEOUT", "超时", http.StatusGatewayTimeout},
		{"/dns", "METADATA_UPSTREAM_FAILED", "DNS", http.StatusBadGateway},
		{"/tls", "METADATA_UPSTREAM_FAILED", "TLS", http.StatusBadGateway},
		{"/reset", "METADATA_UPSTREAM_FAILED", "连接中断", http.StatusBadGateway},
	} {
		t.Run(tc.path, func(t *testing.T) {
			host := "repo.example.test"
			if tc.path == "/blocked" {
				host = "198.18.0.42"
			}
			payload := fmt.Sprintf(`{"url":"http://%s%s?secret=fixture-secret"}`, host, tc.path)
			r := httptest.NewRequest(http.MethodPost, "/admin/api/metadata/repos", strings.NewReader(payload))
			r.Header.Set("X-API-Key", key)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			body := decodeMetadataFixture[struct {
				Code    string `json:"error"`
				Message string `json:"message"`
			}](t, w)
			if w.Code != tc.status || body.Code != tc.code || !strings.Contains(body.Message, tc.message) {
				t.Errorf("HTTP %d, body %s; want HTTP %d with %s and %q", w.Code, w.Body, tc.status, tc.code, tc.message)
			}
			if strings.Contains(body.Message, host) || strings.Contains(body.Message, "fixture-secret") {
				t.Errorf("response leaked upstream information: %q", body.Message)
			}
		})
	}
}

func TestMetadataScrapeRejectsEmptyMappingSource(t *testing.T) {
	srv, store, bgm, _ := setupV27TestServer(t, "")
	ctx := t.Context()
	if _, err := store.ClaimAdmin(ctx, "fixture-owner", "FixturePassword123!"); err != nil {
		t.Fatal(err)
	}
	key, _, err := store.CreateAPIKey(ctx, "fixture admin")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: bangumiTestTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		_, _ = io.WriteString(w, `{"packs":[]}`)
		resp := w.Result()
		resp.Request = r
		return resp, nil
	})}
	manager, err := extmeta.NewManager(store, bgm, nil, srv.runtime.DataDir, nil, extmeta.ManagerOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	srv.WithMetadata(manager)
	src, err := manager.AddCustomSource(ctx, database.MetadataSource{
		Name: "Community", Kind: extmeta.KindMappingPack, Enabled: true, ManifestURL: "https://upstream.example.test/index.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartSourceSync(ctx, src.ID, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		run := manager.Syncs()[0]
		if run.CompletedAt != "" {
			if run.Status != "success" {
				t.Fatalf("mapping sync = %+v", run)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mapping sync did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/api/metadata/scrape", strings.NewReader("{}"))
	r.Header.Set("X-API-Key", key)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	body := decodeMetadataFixture[struct {
		Code    string `json:"error"`
		Message string `json:"message"`
	}](t, w)
	if w.Code != http.StatusBadRequest || body.Code != "NO_READY_METADATA_SOURCES" || body.Message != extmeta.ErrNoReadySources.Error() {
		t.Fatalf("scrape with empty mappings = HTTP %d, %s", w.Code, w.Body.String())
	}
}

func TestMetadataAdminRoutesBindingExportAndCovers(t *testing.T) {
	srv, store, bgm, _ := setupV27TestServer(t, "")
	ctx := context.Background()
	root := srv.runtime.DataDir
	if _, err := store.ClaimAdmin(ctx, "fixture-owner", "FixturePassword123!"); err != nil {
		t.Fatal(err)
	}
	adminKey, _, err := store.CreateAPIKey(ctx, "fixture admin")
	if err != nil {
		t.Fatal(err)
	}
	library := database.Library{ID: "catalog", Name: "Catalog", ProviderType: database.ProviderLocal,
		RootCID: "local:fixture", RootPath: filepath.Join(root, "fixture"), Enabled: true}
	if err := store.UpsertLibrary(ctx, library); err != nil {
		t.Fatal(err)
	}
	reader, err := store.CreateUser(ctx, database.CreateUserInput{
		Username: "fixture-reader", Password: "FixturePassword123!", AllowedLibraryIDs: []string{library.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	readerKey, _, err := store.CreateAPIKeyForUser(ctx, reader.ID, "fixture reader")
	if err != nil {
		t.Fatal(err)
	}
	unscoped, err := store.CreateUser(ctx, database.CreateUserInput{
		Username: "fixture-unscoped", Password: "FixturePassword123!",
	})
	if err != nil {
		t.Fatal(err)
	}
	unscopedKey, _, err := store.CreateAPIKeyForUser(ctx, unscoped.ID, "fixture reader")
	if err != nil {
		t.Fatal(err)
	}

	png := testCoverPNG(t)
	var revision atomic.Int32
	var coverCalls atomic.Int32
	client := &http.Client{Transport: bangumiTestTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		switch r.URL.Path {
		case "/good.png":
			coverCalls.Add(1)
			_, _ = w.Write(png)
		case "/blocked.png", "/other.png":
			coverCalls.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
		resp := w.Result()
		resp.Request = r
		return resp, nil
	})}
	// Serve two versions of the manifest, allowing the refresh to change market
	// description and URLs while preserving the source's local settings.
	client.Transport = metadataHTTPFixture(client.Transport, &revision)
	covers, err := extmeta.NewCoverService(extmeta.CoverConfig{DataDir: root, Client: client, RequestsPerSec: 100})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := extmeta.NewManager(store, bgm, covers, root, slog.New(slog.NewTextHandler(io.Discard, nil)), extmeta.ManagerOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	srv.WithMetadata(manager)
	srv.thumbs.SetExternalCoverFetcher(manager.FetchCover)
	srv.thumbs.SetLocalFirstPageFetcher(func(ctx context.Context, book database.Book) ([]byte, error) {
		page, err := srv.archive.ReadPage(ctx, book, 1)
		return page.Data, err
	})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`
INSERT INTO series(id,library_id,cid,name,relative_path,created_at,updated_at,seen_scan_id)
VALUES('work','catalog','fixture','Blue River','Blue River',?,?,'scan')`, now, now); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(library.RootPath, "first.cbz")
	writePlainCBZForV30Test(t, archivePath)
	stat, err := os.Stat(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`
INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,created_at,updated_at,seen_scan_id)
VALUES('first','work','catalog','first.cbz','fixture','first.cbz',?,'',1,?,?,'scan')`, stat.Size(), now, now); err != nil {
		t.Fatal(err)
	}

	call := func(method, path, key string, value any, want int) *httptest.ResponseRecorder {
		t.Helper()
		var body io.Reader
		if value != nil {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(data)
		}
		r := httptest.NewRequest(method, path, body)
		r.Header.Set("X-API-Key", key)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s = HTTP %d, want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w
	}
	admin := func(method, path string, value any, want int) *httptest.ResponseRecorder {
		t.Helper()
		return call(method, "/admin/api"+path, adminKey, value, want)
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/metadata/overview"}, {"POST", "/metadata/repos"}, {"POST", "/metadata/repos/repo/refresh"},
		{"DELETE", "/metadata/repos/repo"}, {"POST", "/metadata/sources"}, {"PUT", "/metadata/sources/source"},
		{"DELETE", "/metadata/sources/source"}, {"POST", "/metadata/sources/source/sync"}, {"GET", "/metadata/syncs"},
		{"PUT", "/libraries/catalog/metadata-sources"}, {"POST", "/metadata/scrape"}, {"GET", "/metadata/scrape-runs"},
		{"POST", "/metadata/scrape-runs/run/cancel"}, {"GET", "/series/work/metadata/candidates"},
		{"POST", "/series/work/metadata/bind"}, {"DELETE", "/series/work/metadata/bind"},
		{"GET", "/metadata/sources/source/entries/1/cover"}, {"POST", "/metadata/mappings/export"},
	} {
		call(route.method, "/admin/api"+route.path, readerKey, nil, http.StatusForbidden)
	}

	repo := decodeMetadataFixture[database.MetadataRepo](t, admin("POST", "/metadata/repos",
		map[string]string{"url": "https://upstream.example.test/repo.json"}, http.StatusCreated))
	sources, err := store.ListMetadataSources(ctx, "")
	if err != nil || len(sources) != 1 || sources[0].RepoID != repo.ID {
		t.Fatalf("market sources = %+v, %v", sources, err)
	}
	source := sources[0]
	sourcePath := "/metadata/sources/" + source.ID
	sqlitePath := filepath.Join(root, "fixture-source.sqlite")
	createMetadataAPIFixture(t, sqlitePath)
	admin("PUT", sourcePath, map[string]any{"localPath": sqlitePath, "tagDisplayMode": "raw"}, http.StatusOK)
	admin("POST", sourcePath+"/sync", nil, http.StatusAccepted)
	deadline := time.Now().Add(8 * time.Second)
	for {
		syncs := manager.Syncs()
		if len(syncs) > 0 && syncs[0].CompletedAt != "" {
			if syncs[0].Status != "success" {
				t.Fatalf("metadata sync = %+v", syncs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("metadata sync did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	revision.Store(1)
	admin("POST", "/metadata/repos/"+repo.ID+"/refresh", nil, http.StatusOK)
	refreshed, err := store.GetMetadataSource(ctx, source.ID)
	if err != nil || refreshed.Name != "Updated catalog" || !refreshed.Ready || refreshed.LocalPath != sqlitePath ||
		refreshed.TagDisplayMode != "raw" || refreshed.DownloadURL != "https://upstream.example.test/new.sqlite" {
		t.Fatalf("refreshed source lost configuration or state: %+v %v", refreshed, err)
	}
	chain := decodeMetadataFixture[struct {
		Sources []database.LibraryMetadataSource `json:"sources"`
	}](t, admin("PUT", "/libraries/catalog/metadata-sources", map[string]any{"sourceIds": []string{source.ID, "bangumi"}}, http.StatusOK))
	if len(chain.Sources) != 2 || chain.Sources[0].SourceID != source.ID || chain.Sources[1].SourceID != "bangumi" {
		t.Fatalf("chain = %+v", chain.Sources)
	}

	query := url.Values{"sourceId": {source.ID}, "q": {"101"}}
	result := decodeMetadataFixture[struct {
		Candidates []extmeta.Entry `json:"candidates"`
	}](t, admin("GET", "/series/work/metadata/candidates?"+query.Encode(), nil, http.StatusOK))
	if len(result.Candidates) != 1 || result.Candidates[0].EntryID != "101" {
		t.Fatalf("candidates = %+v", result.Candidates)
	}
	bind := func(entryID string) database.SeriesMetadata {
		return decodeMetadataFixture[database.SeriesMetadata](t, admin("POST", "/series/work/metadata/bind",
			map[string]string{"sourceId": source.ID, "entryId": entryID}, http.StatusOK))
	}
	if meta := bind("101"); meta.MatchOrigin != "manual" || meta.ExtEntryID != "101" {
		t.Fatalf("binding = %+v", meta)
	}
	preview := decodeMetadataFixture[extmeta.MappingExport](t, admin("POST", "/metadata/mappings/export", map[string]bool{"preview": true}, http.StatusOK))
	if preview.Count != 1 || len(preview.Sample) != 1 || preview.Sample[0].Key != "Blue River" {
		t.Fatalf("export preview = %+v", preview)
	}
	file := admin("POST", "/metadata/mappings/export", map[string]bool{"preview": false}, http.StatusOK)
	if file.Header().Get("Content-Type") != "application/x-ndjson" || !strings.Contains(file.Header().Get("Content-Disposition"), "attachment") ||
		!strings.Contains(file.Body.String(), `"targetId":"101"`) {
		t.Fatalf("mapping download: %v %s", file.Header(), file.Body)
	}
	const thumbnailPath = "/api/v1/series/work/thumbnail"
	call("GET", thumbnailPath, unscopedKey, nil, http.StatusNotFound)
	if got := coverCalls.Load(); got != 0 {
		t.Fatalf("out-of-scope thumbnail attempted external fetch: %d", got)
	}
	for _, cover := range []*httptest.ResponseRecorder{
		admin("GET", sourcePath+"/entries/101/cover", nil, http.StatusOK),
		call("GET", thumbnailPath, readerKey, nil, http.StatusOK),
	} {
		if cover.Header().Get("Cache-Control") != "private, no-store" || !bytes.Equal(png, cover.Body.Bytes()) {
			t.Fatalf("external cover response: %v", cover.Header())
		}
	}
	if got := coverCalls.Load(); got != 1 {
		t.Fatalf("external cover cache missed: %d requests", got)
	}
	bind("102")
	admin("GET", sourcePath+"/entries/102/cover", nil, http.StatusServiceUnavailable)
	bind("103")
	fallback := call("GET", thumbnailPath, readerKey, nil, http.StatusOK)
	if fallback.Header().Get("Content-Type") != "image/png" || fallback.Header().Get("Cache-Control") != "private, no-store" ||
		!bytes.HasPrefix(fallback.Body.Bytes(), []byte{0x89, 'P', 'N', 'G'}) || coverCalls.Load() != 2 {
		t.Fatalf("thumbnail circuit should fall back to placeholder without opening local archive: %v, requests = %d", fallback.Header(), coverCalls.Load())
	}
	var indexedBooks int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM zip_indexes`).Scan(&indexedBooks); err != nil || indexedBooks != 0 {
		t.Fatalf("thumbnail fallback implicitly opened archive: zip_indexes=%d err=%v", indexedBooks, err)
	}
	bind("104")
	call("GET", thumbnailPath, readerKey, nil, http.StatusNotFound)
	if coverCalls.Load() != 2 {
		t.Fatal("NSFW series attempted an external fetch")
	}
	clear := decodeMetadataFixture[database.SeriesMetadata](t, admin("DELETE", "/series/work/metadata/bind", nil, http.StatusOK))
	if clear.ExtEntryID != "" || clear.ExtSourceID != "" || clear.MatchOrigin != "" {
		t.Fatalf("cleared binding = %+v", clear)
	}
	admin("GET", "/metadata/overview", nil, http.StatusOK)
	admin("GET", "/metadata/syncs", nil, http.StatusOK)
	admin("GET", "/metadata/scrape-runs", nil, http.StatusOK)
	admin("DELETE", "/metadata/repos/"+repo.ID, nil, http.StatusNoContent)
	if _, err := os.Stat(sqlitePath); err != nil {
		t.Fatal("repo deletion removed local user data")
	}
}

func metadataHTTPFixture(next http.RoundTripper, revision *atomic.Int32) http.RoundTripper {
	return bangumiTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/repo.json" {
			return next.RoundTrip(r)
		}
		name, asset := "Initial catalog", "old.sqlite"
		if revision.Load() > 0 {
			name, asset = "Updated catalog", "new.sqlite"
		}
		w := httptest.NewRecorder()
		_ = json.NewEncoder(w).Encode(extmeta.RepoManifest{
			Schema: extmeta.RepoSchemaV1, Name: "Test market",
			Sources: []extmeta.RepoSource{{
				ID: "catalog", Kind: extmeta.KindOfflineSQLite, Name: name, Default: true,
				DownloadURL: "https://upstream.example.test/" + asset,
			}},
		})
		resp := w.Result()
		resp.Request = r
		return resp, nil
	})
}

func decodeMetadataFixture[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var data T
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func createMetadataAPIFixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
CREATE TABLE entries (
 id TEXT PRIMARY KEY, title TEXT, title_alt TEXT, authors TEXT, summary TEXT, tags TEXT,
 category TEXT, cover_url TEXT, rating REAL, is_nsfw INTEGER, updated_at INTEGER, extra_json TEXT
);
INSERT INTO entries(id,title,cover_url,is_nsfw) VALUES
 ('101','Blue River','https://upstream.example.test/good.png',0),
 ('102','Second Entry','https://upstream.example.test/blocked.png',0),
 ('103','Third Entry','https://upstream.example.test/other.png',0),
 ('104','Restricted Entry','https://upstream.example.test/other.png',1);
`); err != nil {
		t.Fatal(err)
	}
}
