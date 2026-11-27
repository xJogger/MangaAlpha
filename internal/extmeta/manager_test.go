package extmeta

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/diagnostics"
)

type managerTransport func(*http.Request) (*http.Response, error)

func (f managerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func managerResponse(r *http.Request, code int, body io.Reader) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(body), Request: r}
}

func newTestManager(t *testing.T, opts ManagerOptions) (*database.Store, *Manager, string) {
	t.Helper()
	root := t.TempDir()
	store, err := database.Open(filepath.Join(root, "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager, err := NewManager(store, nil, nil, root, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return store, manager, root
}

func testMetadataDiagnostics(t *testing.T) *diagnostics.Recorder {
	t.Helper()
	rec, err := diagnostics.New(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	previous := diagnostics.Default()
	diagnostics.SetDefault(rec)
	t.Cleanup(func() { diagnostics.SetDefault(previous) })
	return rec
}

func TestManagerRepoErrorsAndDiagnostics(t *testing.T) {
	rec := testMetadataDiagnostics(t)
	const proxy = "socks5://fixture-user:fixture-secret@proxy.example.test:1080"
	const privateDetail = proxy + " fixture-secret [fc00::1234] 127.0.0.1 /fixture/private.sqlite"
	timeout := &net.DNSError{Err: privateDetail, IsTimeout: true}
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/blocked":
			return nil, fmt.Errorf("%w: %s", ErrSSRFBlocked, privateDetail)
		case "/invalid-proxy":
			return nil, ErrInvalidProxyURL
		case "/canceled":
			return nil, context.Canceled
		case "/deadline":
			return nil, context.DeadlineExceeded
		case "/timeout":
			return nil, timeout
		case "/invalid-manifest":
			return managerResponse(r, http.StatusOK, strings.NewReader(`{"schema":"fixture-secret"}`)), nil
		case "/repo.json":
			return managerResponse(r, http.StatusOK, strings.NewReader(`{"schema":"mangaalpha-repo-v1"}`)), nil
		default:
			return managerResponse(r, http.StatusNotFound, strings.NewReader(privateDetail)), nil
		}
	})}
	_, m, _ := newTestManager(t, ManagerOptions{
		HTTPClient: client, ProxyURLProvider: func(context.Context) string { return proxy },
	})
	var log bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&log, nil))
	for _, tc := range []struct {
		path   string
		want   error
		code   string
		detail string
	}{
		{"/blocked", ErrSSRFBlocked, "SSRF_BLOCKED", "网络安全策略"},
		{"/invalid-proxy", ErrInvalidProxyURL, "INVALID_PROXY_URL", "代理 URL 无效"},
		{"/invalid-manifest", ErrInvalidManifest, "INVALID_METADATA_MANIFEST", "repo.json"},
		{"/canceled", context.Canceled, "CANCELED", "已取消"},
		{"/deadline", context.DeadlineExceeded, "TIMEOUT", "超时"},
		{"/timeout", ErrRemoteMetadata, "TIMEOUT", "超时"},
		{"/missing", ErrRemoteMetadata, "NOT_FOUND", "HTTP 404"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, err := m.SubscribeRepo(t.Context(), "https://repo.example.test"+tc.path+"?secret=fixture-secret")
			if !errors.Is(err, tc.want) || errors.Is(err, ErrRemoteMetadata) != (tc.want == ErrRemoteMetadata) {
				t.Fatalf("subscribe error = %v, want %v", err, tc.want)
			}
			switch tc.path {
			case "/missing":
				var statusErr *HTTPStatusError
				if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound {
					t.Fatalf("subscribe error did not preserve HTTP 404: %v", err)
				}
			case "/timeout":
				var dnsErr *net.DNSError
				if !errors.As(err, &dnsErr) || dnsErr != timeout {
					t.Fatalf("subscribe error did not preserve the timeout cause: %v", err)
				}
			}
			events := rec.Recent(1)
			if len(events) == 0 {
				t.Fatal("subscribe diagnostics are missing")
			}
			event := events[0]
			if event.Component != "extmeta" || event.Stage != "extmeta_repo" || event.Action != "subscribe" ||
				event.Level != diagnostics.LevelWarn || event.Code != tc.code || !strings.Contains(event.Detail, tc.detail) {
				t.Errorf("unexpected subscribe event: %+v", event)
			}
			if tc.path == "/missing" && event.Meta["httpStatus"] != float64(http.StatusNotFound) {
				t.Errorf("subscribe event missing HTTP status: %+v", event)
			}
		})
	}
	repo, err := m.SubscribeRepo(t.Context(), "https://repo.example.test/repo.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RefreshRepo(t.Context(), repo.ID); err != nil {
		t.Fatal(err)
	}
	events := rec.Recent(2)
	if len(events) != 2 || events[0].Action != "subscribe" || events[1].Action != "refresh" ||
		events[0].Level != diagnostics.LevelInfo || events[1].Level != diagnostics.LevelInfo {
		t.Errorf("expected concise subscription and refresh success events, got %+v", events)
	}
	assertSafeMetadataDiagnostics(t, log.String(), rec.ExportText(30))
}

func TestManagerSyncFailureDetailsAndDiagnostics(t *testing.T) {
	rec := testMetadataDiagnostics(t)
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/blocked" {
			return nil, fmt.Errorf("%w: disallowed IP [fc00::1234] at 127.0.0.1 using /fixture/private.sqlite (fixture-secret)", ErrSSRFBlocked)
		}
		return managerResponse(r, http.StatusNotFound, strings.NewReader("fixture-secret")), nil
	})}
	store, m, _ := newTestManager(t, ManagerOptions{HTTPClient: client})
	var log bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&log, nil))
	for _, tc := range []struct {
		path, code, detail string
		httpStatus         any
	}{
		{"/blocked", "SSRF_BLOCKED", "网络安全策略", nil},
		{"/missing", "NOT_FOUND", "HTTP 404", float64(http.StatusNotFound)},
	} {
		src, err := m.AddCustomSource(t.Context(), database.MetadataSource{
			Name: "Mapping index", Kind: KindMappingPack, ManifestURL: "https://repo.example.test" + tc.path,
		})
		if err != nil {
			t.Fatal(err)
		}
		syncManagerFixture(t, m, src, false, "failed")
		run := m.Syncs()[0]
		state, err := store.GetMetadataSource(t.Context(), src.ID)
		if err != nil || state.Enabled || state.Ready || state.LastError != run.Error || !strings.Contains(run.Error, tc.detail) {
			t.Fatalf("sync messages: state=%q run=%q error=%v", state.LastError, run.Error, err)
		}
		events := rec.Recent(1)
		if len(events) == 0 {
			t.Fatal("sync diagnostics are missing")
		}
		event := events[0]
		if event.Component != "extmeta" || event.Stage != "extmeta_sync" || event.Action != "sync" ||
			event.Level != diagnostics.LevelError || event.Code != tc.code || event.Detail != run.Error ||
			event.Meta["httpStatus"] != tc.httpStatus || event.Meta["kind"] != KindMappingPack ||
			event.Meta["phase"] != "downloading" || event.Meta["skipped"] != false {
			t.Errorf("unexpected sync event: %+v", event)
		}
		if !strings.Contains(log.String(), tc.detail) || !strings.Contains(log.String(), "sourceId="+src.ID) {
			t.Errorf("sync failure warning is missing error detail or source ID: %s", log.String())
		}
		assertSafeMetadataDiagnostics(t, state.LastError, run.Error, log.String(), rec.ExportText(30))
	}
}

func assertSafeMetadataDiagnostics(t *testing.T, texts ...string) {
	t.Helper()
	for _, output := range texts {
		for _, secret := range []string{
			"repo.example.test", "proxy.example.test", "fixture-user", "fixture-secret", "fc00::1234",
			"127.0.0.1", "/fixture/private.sqlite",
		} {
			if strings.Contains(output, secret) {
				t.Errorf("metadata diagnostics leaked %q in %s", secret, output)
			}
		}
	}
}

func execManagerFixture(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func seedMetadataLibrary(t *testing.T, store *database.Store, id string, enabled bool) {
	t.Helper()
	err := store.UpsertLibrary(context.Background(), database.Library{ID: id, Name: id, RootCID: id, Enabled: enabled})
	if err != nil {
		t.Fatal(err)
	}
}

func seedMetadataSeries(t *testing.T, store *database.Store, id, libraryID, title string) {
	t.Helper()
	execManagerFixture(t, store.DB(), `
INSERT INTO series(id,library_id,cid,name,relative_path,created_at,updated_at,seen_scan_id)
VALUES(?,?,?,?,?,?,?,'scan')`, id, libraryID, id, title, title, timestamp(), timestamp())
}

func readMetadata(t *testing.T, store *database.Store, id string) database.SeriesMetadata {
	t.Helper()
	series, err := store.SeriesByIDAny(context.Background(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.SeriesMetadataByID(context.Background(), series)
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func createManagerGallery(t *testing.T, file, title string) {
	t.Helper()
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	execManagerFixture(t, db, `
CREATE TABLE gallery (
 gid INTEGER PRIMARY KEY, token TEXT, title TEXT, title_jpn TEXT, category TEXT, thumb TEXT,
 uploader TEXT, posted INTEGER, filecount INTEGER, filesize INTEGER, expunged INTEGER,
 removed INTEGER, replaced INTEGER, rating REAL, root_gid INTEGER
);
CREATE TABLE tag (id INTEGER PRIMARY KEY, name TEXT);
CREATE TABLE gid_tid (gid INTEGER, tid INTEGER);
INSERT INTO tag VALUES(1,'topic:glasses');
INSERT INTO gid_tid VALUES(101,1);
`)
	execManagerFixture(t, db, "INSERT INTO gallery(gid,token,title,category) VALUES(101,'fixture',?,'Non-H'),(102,'second','Another Entry','Manga')", title)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitForManager(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("metadata job did not finish in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func syncManagerFixture(t *testing.T, m *Manager, src database.MetadataSource, force bool, want string) {
	t.Helper()
	run, err := m.StartSourceSync(context.Background(), src.ID, force)
	if err != nil {
		t.Fatal(err)
	}
	var finished SyncRun
	waitForManager(t, func() bool {
		for _, item := range m.Syncs() {
			if item.ID == run.ID && item.CompletedAt != "" {
				finished = item
				return true
			}
		}
		return false
	})
	if finished.Status != want {
		t.Fatalf("source sync status = %#v, want %s", finished, want)
	}
}

func addManagerSource(t *testing.T, m *Manager, source database.MetadataSource) database.MetadataSource {
	t.Helper()
	source.Enabled = true
	src, err := m.AddCustomSource(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestManagerEmptyMappingPacksSyncAndReload(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "empty manifest", body: `{"packs":[]}`},
		{name: "empty NDJSON", body: `{"packs":["pack.ndjson"]}`},
		{name: "no valid records", body: `{"packs":["invalid.ndjson"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/index.json":
					return managerResponse(r, http.StatusOK, strings.NewReader(tc.body)), nil
				case "/pack.ndjson":
					return managerResponse(r, http.StatusOK, strings.NewReader("")), nil
				case "/invalid.ndjson":
					return managerResponse(r, http.StatusOK, strings.NewReader("not a mapping\n")), nil
				default:
					return nil, fmt.Errorf("unexpected request")
				}
			})}
			opts := ManagerOptions{HTTPClient: client}
			store, m, root := newTestManager(t, opts)
			seedMetadataLibrary(t, store, "main", true)
			src := addManagerSource(t, m, database.MetadataSource{
				Name: "Community", Kind: KindMappingPack, ManifestURL: "https://source.example.test/index.json",
			})
			previousError := "previous sync failed"
			if _, err := store.UpdateMetadataSourceSyncState(ctx, src.ID, database.MetadataSourceSyncState{LastError: &previousError}); err != nil {
				t.Fatal(err)
			}
			syncManagerFixture(t, m, src, false, "success")

			assertEmpty := func(manager *Manager) {
				t.Helper()
				src, err := store.GetMetadataSource(ctx, src.ID)
				if err != nil || !src.Ready || src.EntryCount != 0 || src.FileSize != 0 || src.LastError != "" {
					t.Fatalf("empty mapping source = %+v, err = %v", src, err)
				}
				info, err := os.Stat(manager.mappingPath(src.ID))
				if err != nil || info.Size() != 0 {
					t.Fatalf("empty mapping file = %v, err = %v", info, err)
				}
				for _, libraryID := range []string{"main", ""} {
					run, err := manager.StartMultiScrape(ctx, libraryID, "force", false)
					if !errors.Is(err, ErrNoReadySources) || run.ID != "" {
						t.Fatalf("scrape with only empty mapping = %+v, err = %v", run, err)
					}
				}
				if runs := manager.ScrapeRuns(); len(runs) != 0 {
					t.Fatalf("unexpected scrape history: %+v", runs)
				}
			}
			assertEmpty(m)
			m.Close()
			reopened, err := NewManager(store, nil, nil, root, nil, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			assertEmpty(reopened)
		})
	}
}

func TestManagerOfflineReadinessUsesVisibleLibraryChains(t *testing.T) {
	ctx := t.Context()
	store, m, root := newTestManager(t, ManagerOptions{})
	path := filepath.Join(root, "catalog-source.sqlite")
	createManagerGallery(t, path, "Blue River")
	src := addManagerSource(t, m, database.MetadataSource{Name: "Catalog", Kind: KindOfflineSQLite, LocalPath: path})
	syncManagerFixture(t, m, src, false, "success")
	assertReady := func(id string, want bool) {
		t.Helper()
		if ready, err := m.HasReadySourcesForLibrary(ctx, id, false); err != nil || ready != want {
			t.Fatalf("readiness for %q = %v, err = %v, want %v", id, ready, err, want)
		}
	}
	assertReady("", true)
	seedMetadataLibrary(t, store, "main", true)
	seedMetadataLibrary(t, store, "hidden", false)
	seedMetadataLibrary(t, store, "member", true)
	seedMetadataLibrary(t, store, "other-member", true)
	if _, _, err := store.CreateUnionLibrary(ctx, "Combined", true, []string{"member", "other-member"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"hidden", "member"} {
		if _, err := m.SetLibrarySources(ctx, id, []string{src.ID}); err != nil {
			t.Fatal(err)
		}
	}
	assertReady("", false)
	assertReady("main", false)
	if _, err := m.SetLibrarySources(ctx, "main", []string{src.ID}); err != nil {
		t.Fatal(err)
	}
	assertReady("", true)
	assertReady("main", true)
	execManagerFixture(t, store.DB(), "UPDATE library_metadata_sources SET enabled=0 WHERE library_id='main'")
	assertReady("main", false)
	assertReady("", false)
}

func TestManagerOfflineChainMappingAndManualProtection(t *testing.T) {
	rec := testMetadataDiagnostics(t)
	ctx := context.Background()
	mappings := `{"schema":"mangaalpha_mapping_v1","key":"Blue River","targetSource":"gallery_v1","targetId":"102"}
{"schema":"mangaalpha_mapping_v1","key":"Uncatalogued Work","targetSource":"gallery_v1","targetId":"102"}
`
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/index.json":
			return managerResponse(r, http.StatusOK, strings.NewReader(`{"packs":[{"url":"pack.ndjson"}]}`)), nil
		case "/pack.ndjson":
			return managerResponse(r, http.StatusOK, strings.NewReader(mappings)), nil
		default:
			return nil, fmt.Errorf("unexpected request")
		}
	})}
	store, m, root := newTestManager(t, ManagerOptions{HTTPClient: client})
	seedMetadataLibrary(t, store, "main", true)
	seedMetadataLibrary(t, store, "hidden", false)
	seedMetadataSeries(t, store, "offline", "main", "Blue River")
	seedMetadataSeries(t, store, "mapping", "main", "Uncatalogued Work")
	seedMetadataSeries(t, store, "protected", "main", "Blue River")
	seedMetadataSeries(t, store, "hidden-series", "hidden", "Blue River")
	p1, p2 := filepath.Join(root, "first.sqlite"), filepath.Join(root, "second.sqlite")
	createManagerGallery(t, p1, "Green Valley")
	createManagerGallery(t, p2, "Blue River")
	db, err := sql.Open("sqlite", p1)
	if err != nil {
		t.Fatal(err)
	}
	execManagerFixture(t, db, "DELETE FROM gallery WHERE gid=102")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	first := addManagerSource(t, m, database.MetadataSource{Name: "First", Kind: KindOfflineSQLite, LocalPath: p1})
	second := addManagerSource(t, m, database.MetadataSource{Name: "Second", Kind: KindOfflineSQLite, LocalPath: p2})
	pack := addManagerSource(t, m, database.MetadataSource{Name: "Mapping", Kind: KindMappingPack, ManifestURL: "https://source.example.test/index.json"})
	for _, src := range []database.MetadataSource{first, second, pack} {
		syncManagerFixture(t, m, src, false, "success")
	}
	if _, err := m.SetLibrarySources(ctx, "main", []string{first.ID, second.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BindSeriesExternal(ctx, "protected", second.ID, "102", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartMultiScrape(ctx, "hidden", "force", false); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("hidden scrape = %v", err)
	}
	// Reopen to exercise loading the persisted sidecars and mapping pack.
	m.Close()
	m, err = NewManager(store, nil, nil, root, nil, ManagerOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	run, err := m.StartMultiScrape(ctx, "main", "force", true)
	if err != nil {
		t.Fatal(err)
	}
	waitForManager(t, func() bool { return m.ScrapeRuns()[0].CompletedAt != "" })
	if done := m.ScrapeRuns()[0]; done.ID != run.ID || done.Status != "success" || done.Matched != 2 || done.Mapped != 1 {
		t.Fatalf("scrape = %+v", done)
	}
	events := rec.Recent(1)
	if len(events) != 1 {
		t.Fatal("missing scrape completion diagnostic")
	}
	event := events[0]
	if event.Component != "extmeta" || event.Stage != "extmeta_scrape" || event.Action != "finish" || event.Detail != "" ||
		!reflect.DeepEqual(event.Meta, map[string]any{
			"mode": "force", "status": "success", "allowOnline": true, "total": float64(2), "matched": float64(2),
			"mapped": float64(1), "unmatched": float64(0), "skipped": float64(0), "failed": float64(0),
		}) {
		t.Errorf("scrape completion diagnostic = %+v", event)
	}
	if live, err := m.ScrapeRunByID(ctx, run.ID); err != nil || live.Mapped != 1 {
		t.Fatalf("in-process scrape = %+v, err = %v", live, err)
	}
	m.Close()
	reopened, err := NewManager(store, nil, nil, root, nil, ManagerOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	runs := reopened.ScrapeRuns()
	if len(runs) != 1 {
		t.Fatalf("scrape history = %+v", runs)
	}
	persisted := runs[0]
	if !strings.HasPrefix(persisted.ID, "ext_") || persisted.ID != run.ID || persisted.LibraryID != "main" ||
		persisted.LibraryName != "main" || persisted.Mode != bangumi.ScrapeModeForce || !persisted.AllowOnline ||
		persisted.Matched != 2 || persisted.TotalSeries != 2 || persisted.Processed != 2 ||
		persisted.Status != "success" || persisted.StartedAt != run.StartedAt || persisted.CompletedAt == "" {
		t.Fatalf("persisted scrape = %+v", persisted)
	}
	if byID, err := reopened.ScrapeRunByID(ctx, run.ID); err != nil || !reflect.DeepEqual(byID, persisted) {
		t.Fatalf("scrape by ID = %+v, err = %v", byID, err)
	}
	var mode, createdAt string
	var force bool
	if err := store.DB().QueryRowContext(ctx, "SELECT mode,force,created_at FROM bangumi_scrape_runs WHERE id=?", run.ID).
		Scan(&mode, &force, &createdAt); err != nil || mode != "force+online" || !force || createdAt != run.StartedAt {
		t.Fatalf("stored scrape mode = %q, force = %v, createdAt = %q, err = %v", mode, force, createdAt, err)
	}
	for _, tc := range []struct {
		id, entry, origin string
		nsfw              bool
	}{{"offline", "101", "auto", false}, {"mapping", "102", "mapping", true}, {"protected", "102", "agent", true}} {
		got := readMetadata(t, store, tc.id)
		if got.ExtSourceID != second.ID || got.ExtEntryID != tc.entry || got.MatchOrigin != tc.origin || got.IsNSFW != tc.nsfw {
			t.Fatalf("series %s = %+v", tc.id, got)
		}
	}
	if got := readMetadata(t, store, "hidden-series"); got.MatchOrigin != "" {
		t.Fatalf("hidden series changed: %+v", got)
	}
}

func TestManagerMappingRequiresReadyTarget(t *testing.T) {
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"schema":"mangaalpha_mapping_v1","key":"Blue River","targetSource":"bangumi","targetId":"42"}`
		return managerResponse(r, http.StatusOK, strings.NewReader(body)), nil
	})}
	store, m, _ := newTestManager(t, ManagerOptions{HTTPClient: client})
	seedMetadataLibrary(t, store, "main", true)
	src, err := m.AddCustomSource(t.Context(), database.MetadataSource{
		Name: "Community", Kind: KindMappingPack, DownloadURL: "https://source.example.test/pack.ndjson",
	})
	if err != nil {
		t.Fatal(err)
	}
	syncManagerFixture(t, m, src, false, "success")
	if _, err := m.StartMultiScrape(t.Context(), "", "", false); !errors.Is(err, ErrNoReadySources) {
		t.Fatalf("mapping-only scrape: %v", err)
	}
}

func TestManagerBindingModesIDsAndExport(t *testing.T) {
	ctx := context.Background()
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		return managerResponse(r, http.StatusOK, strings.NewReader(
			`{"data":[{"namespace":"topic","data":{"glasses":{"name":"眼镜"}}}]}`)), nil
	})}
	store, m, root := newTestManager(t, ManagerOptions{HTTPClient: client})
	seedMetadataLibrary(t, store, "main", true)
	seedMetadataSeries(t, store, "safe", "main", "Blue River Vol 2")
	seedMetadataSeries(t, store, "adult", "main", "Another Entry")
	seedMetadataSeries(t, store, "private", "main", "/private/Fixture Folder")
	path := filepath.Join(root, "upstream.sqlite")
	createManagerGallery(t, path, "Blue River")
	dict := addManagerSource(t, m, database.MetadataSource{Name: "Dictionary", Kind: KindTagDictionary, DownloadURL: "https://source.example.test/tags.json"})
	src := addManagerSource(t, m, database.MetadataSource{Name: "Catalog", Kind: KindOfflineSQLite, LocalPath: path, TagDictSourceID: dict.ID})
	syncManagerFixture(t, m, dict, false, "success")
	syncManagerFixture(t, m, src, false, "success")
	for _, query := range []string{"101", "101/fixture", "https://series.example.test/g/101/fixture/?view=full"} {
		found, err := m.SearchCandidates(ctx, "safe", src.ID, query)
		if err != nil || len(found.Candidates.([]*Entry)) != 1 || found.Candidates.([]*Entry)[0].EntryID != "101" {
			t.Fatalf("candidate query %q: %+v %v", query, found, err)
		}
	}
	result, err := m.SearchCandidates(ctx, "safe", src.ID, "101/wrong")
	if err != nil || len(result.Candidates.([]*Entry)) != 0 {
		t.Fatalf("wrong token returned candidates: %+v %v", result, err)
	}
	series, err := store.SeriesByIDAny(ctx, "safe", false)
	if err != nil {
		t.Fatal(err)
	}
	locked, title := true, "Locked"
	if _, err := store.PatchSeriesMetadata(ctx, series, database.SeriesMetadataPatch{Title: &title, TitleLock: &locked}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ mode, tag string }{{"zh", "眼镜"}, {"bilingual", "眼镜 (topic:glasses)"}, {"raw", "topic:glasses"}} {
		if _, err := m.UpdateSourceConfig(ctx, src.ID, SourceConfig{TagDisplayMode: &tc.mode}); err != nil {
			t.Fatal(err)
		}
		meta, err := m.BindSeriesExternal(ctx, "safe", src.ID, "101", "")
		if err != nil || len(meta.Tags) != 1 || meta.Tags[0] != tc.tag || meta.Title != "Locked" || meta.IsNSFW || meta.MatchOrigin != "manual" {
			t.Fatalf("bind mode %s: %+v %v", tc.mode, meta, err)
		}
	}
	for id, entry := range map[string]string{"adult": "102", "private": "101"} {
		if _, err := m.BindSeriesExternal(ctx, id, src.ID, entry, "manual"); err != nil {
			t.Fatal(err)
		}
	}
	exported, err := m.ExportCommunityMappings(ctx, database.MappingExportFilter{}, false)
	if err != nil || exported.Count != 1 || exported.ExcludedNSFW != 1 || exported.SkippedPrivacy != 1 ||
		len(exported.Sample) != 1 || exported.Sample[0].Key != "Blue River" ||
		strings.Contains(string(exported.NDJSON), "private") || strings.Contains(string(exported.NDJSON), "Locked") {
		t.Fatalf("export = %+v, %s, %v", exported, exported.NDJSON, err)
	}
	if err := m.DeleteSource(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("source deletion touched the user file: %v", err)
	}
	if _, err := os.Stat(m.indexPath(src.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source deletion retained index: %v", err)
	}
}

func TestManagerRemoteSyncValidatorsAndFailurePreservesIndex(t *testing.T) {
	ctx := context.Background()
	var downloads atomic.Int32
	var bad atomic.Bool
	var blob []byte
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		body := string(blob)
		if bad.Load() {
			body = "invalid sqlite"
		}
		resp := managerResponse(r, http.StatusOK, strings.NewReader(body))
		resp.Header.Set("ETag", `"first"`)
		if bad.Load() {
			resp.Header.Set("ETag", `"broken"`)
		}
		if r.Method == http.MethodGet {
			downloads.Add(1)
		}
		return resp, nil
	})}
	store, m, root := newTestManager(t, ManagerOptions{HTTPClient: client})
	fixture := filepath.Join(root, "source.sqlite")
	createManagerGallery(t, fixture, "Blue River")
	var err error
	blob, err = os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	src, err := m.AddCustomSource(ctx, database.MetadataSource{
		Name: "Remote", Kind: KindOfflineSQLite, DownloadURL: "https://source.example.test/source.sqlite",
	})
	if err != nil {
		t.Fatal(err)
	}
	if src.Enabled || src.Ready {
		t.Fatal("source fixture must start disabled and unready")
	}
	if _, err := m.startSourceSync(ctx, src.ID, false, true); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("scheduled initial sync for disabled source: %v", err)
	}
	syncManagerFixture(t, m, src, false, "success")
	state, err := store.GetMetadataSource(ctx, src.ID)
	if err != nil || !state.Enabled || !state.Ready {
		t.Fatalf("first manual sync did not enable source: %+v, %v", state, err)
	}
	disabled := false
	if _, err := m.UpdateSourceConfig(ctx, src.ID, SourceConfig{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	syncManagerFixture(t, m, src, false, "skipped")
	syncManagerFixture(t, m, src, true, "success")
	bad.Store(true)
	syncManagerFixture(t, m, src, true, "failed")
	state, err = store.GetMetadataSource(ctx, src.ID)
	if err != nil || state.Enabled || !state.Ready || state.EntryCount != 2 || state.RemoteETag != `"first"` || downloads.Load() != 3 {
		t.Fatalf("remote sync state = %+v, downloads = %d, error = %v", state, downloads.Load(), err)
	}
	m.mu.RLock()
	entry, err := m.sidecars[src.ID].GetEntry(ctx, "101")
	m.mu.RUnlock()
	if err != nil || entry.Title != "Blue River" {
		t.Fatalf("previous sidecar no longer readable: %+v %v", entry, err)
	}
}

func TestManagerOnlineCircuitKeepsQueueCheckpoint(t *testing.T) {
	var hits atomic.Int32
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		if r.URL.Query().Get("q") != "Uncatalogued Work" {
			t.Error("unrecognized online query")
		}
		return managerResponse(r, http.StatusTooManyRequests, strings.NewReader("")), nil
	})}
	store, m, _ := newTestManager(t, ManagerOptions{HTTPClient: client})
	seedMetadataLibrary(t, store, "main", true)
	seedMetadataSeries(t, store, "series", "main", "Uncatalogued Work")
	addManagerSource(t, m, database.MetadataSource{Name: "Online", Kind: KindOfflineSQLite, OnlineEnabled: true, OnlineAPIURL: "https://source.example.test/suggest"})
	run, err := m.StartMultiScrape(context.Background(), "main", "force", true)
	if err != nil {
		t.Fatal(err)
	}
	waitForManager(t, func() bool { return m.OnlineEnrichStatus().Status == "cooldown" })
	status := m.OnlineEnrichStatus()
	if status.Processed != 0 || status.Total != 1 || status.HTTPStatus != 429 || status.CooldownUntil == nil || hits.Load() != 1 {
		t.Fatalf("online checkpoint = %+v, requests = %d", status, hits.Load())
	}
	waitForManager(t, func() bool {
		var total int
		var current string
		if err := store.DB().QueryRow("SELECT total_series,current_series FROM bangumi_scrape_runs WHERE id=?", run.ID).
			Scan(&total, &current); err != nil {
			t.Fatal(err)
		}
		return total == 1 && current == "Uncatalogued Work"
	})
	if err := m.CancelScrape(run.ID); err != nil {
		t.Fatal(err)
	}
	waitForManager(t, func() bool { return m.ScrapeRuns()[0].Status == "canceled" })
	if status := m.OnlineEnrichStatus(); status.Status != "canceled" || status.Processed != 0 {
		t.Fatalf("online cancellation = %+v", status)
	}
	var storedStatus string
	var completedAt sql.NullString
	if err := store.DB().QueryRow("SELECT status,completed_at FROM bangumi_scrape_runs WHERE id=?", run.ID).
		Scan(&storedStatus, &completedAt); err != nil || storedStatus != "canceled" || !completedAt.Valid {
		t.Fatalf("stored cancellation = %q, completedAt = %+v, err = %v", storedStatus, completedAt, err)
	}
}

func TestManagerSchedulerHonorsSourceCadence(t *testing.T) {
	ctx := context.Background()
	var requests atomic.Int32
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.URL.Path != "/weekly.json" {
			t.Errorf("scheduler requested an ineligible source")
		}
		return managerResponse(r, http.StatusOK, strings.NewReader(`{"data":[{"namespace":"topic","data":{"glasses":{"name":"眼镜"}}}]}`)), nil
	})}
	store, m, _ := newTestManager(t, ManagerOptions{HTTPClient: client})
	add := func(name, cadence string, enabled bool) database.MetadataSource {
		t.Helper()
		src, err := m.AddCustomSource(ctx, database.MetadataSource{
			Name: name, Kind: KindTagDictionary, Enabled: enabled, AutoUpdate: cadence,
			DownloadURL: "https://source.example.test/" + name + ".json",
		})
		if err != nil {
			t.Fatal(err)
		}
		return src
	}
	add("manual", "manual", true)
	add("disabled", "weekly", false)
	monthly := add("monthly", "monthly", true)
	weekly := add("weekly", "weekly", true)
	checked := time.Now().AddDate(0, 0, -8).Format(time.RFC3339Nano)
	if _, err := store.UpdateMetadataSourceSyncState(ctx, monthly.ID, database.MetadataSourceSyncState{LastCheckedAt: &checked}); err != nil {
		t.Fatal(err)
	}
	m.StartScheduler()
	waitForManager(t, func() bool {
		runs := m.Syncs()
		return len(runs) > 0 && runs[0].CompletedAt != ""
	})
	if runs := m.Syncs(); len(runs) != 1 || runs[0].SourceID != weekly.ID || runs[0].Status != "success" || requests.Load() != 1 {
		t.Fatalf("scheduled runs = %+v, requests = %d", runs, requests.Load())
	}
}

func TestDisabledSourceStaysDisabledAcrossURLChangeRebuild(t *testing.T) {
	ctx := context.Background()
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		return managerResponse(r, http.StatusOK, strings.NewReader(`{"data":[{"namespace":"topic","data":{"glasses":{"name":"眼镜"}}}]}`)), nil
	})}
	store, m, _ := newTestManager(t, ManagerOptions{HTTPClient: client})
	src, err := m.AddCustomSource(ctx, database.MetadataSource{
		Name: "Rebuild Dict", Kind: KindTagDictionary, Enabled: false, AutoUpdate: "manual",
		DownloadURL: "https://source.example.test/v1.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	// First-ever manual sync auto-enables the newly added source.
	syncManagerFixture(t, m, src, false, "success")
	got, err := store.GetMetadataSource(ctx, src.ID)
	if err != nil || !got.Enabled || !got.Ready {
		t.Fatalf("after first sync: %+v, err=%v", got, err)
	}

	// Admin explicitly disables the source, then changes its DownloadURL (which clears Ready).
	disabled := false
	nextURL := "https://source.example.test/v2.json"
	if _, err := m.UpdateSourceConfig(ctx, src.ID, SourceConfig{Enabled: &disabled, DownloadURL: &nextURL}); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetMetadataSource(ctx, src.ID)
	if err != nil || got.Enabled || got.Ready {
		t.Fatalf("after disabling and changing URL: %+v, err=%v", got, err)
	}

	// Rebuilding the explicitly disabled source must preserve Enabled=false.
	syncManagerFixture(t, m, got, false, "success")
	got, err = store.GetMetadataSource(ctx, src.ID)
	if err != nil || got.Enabled || !got.Ready {
		t.Fatalf("rebuilt disabled source unexpectedly changed enabled=%v ready=%v, err=%v", got.Enabled, got.Ready, err)
	}
}
