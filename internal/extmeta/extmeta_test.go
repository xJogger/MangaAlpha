package extmeta

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/diagnostics"
	_ "modernc.org/sqlite"
)

type stubIPResolver struct {
	addrs []net.IPAddr
	err   error
}

func (s stubIPResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return s.addrs, s.err
}

type recordingDialer struct {
	addresses []string
	err       error
}

func (d *recordingDialer) DialContext(_ context.Context, _, addr string) (net.Conn, error) {
	d.addresses = append(d.addresses, addr)
	return nil, d.err
}

func TestSafeDialTransparentProxyDNS(t *testing.T) {
	t.Parallel()
	dialError := errors.New("fixture dial ended")
	for _, tc := range []struct {
		name string
		dns  []string
		want []string
	}{
		{"fake IPv4", []string{"198.18.0.42"}, []string{"198.18.0.42"}},
		{"fake IPv6", []string{"fc00::1234"}, []string{"fc00::1234"}},
		{"dual stack", []string{"fc00::1234", "198.18.0.42"}, []string{"198.18.0.42", "fc00::1234"}},
		{"stable dual stack", []string{"fc00::1234", "198.18.0.42", "fc00::1235", "198.18.0.41"},
			[]string{"198.18.0.42", "198.18.0.41", "fc00::1234", "fc00::1235"}},
		{"unspecified AAAA", []string{"93.184.216.34", "::"}, []string{"93.184.216.34"}},
		{"unspecified A", []string{"198.18.0.42", "0.0.0.0"}, []string{"198.18.0.42"}},
		{"mapped IPv4", []string{"::ffff:198.18.0.42", "::ffff:0.0.0.0"}, []string{"198.18.0.42"}},
		{"fake mixed with public", []string{"198.18.0.42", "93.184.216.34"}, []string{"198.18.0.42", "93.184.216.34"}},
		{"fake IPv6 mixed with public IPv4", []string{"fc00::1234", "93.184.216.34"}, []string{"93.184.216.34", "fc00::1234"}},
		{"only unspecified", []string{"0.0.0.0", "::"}, nil},
		{"private", []string{"198.18.0.42", "10.0.0.5"}, nil},
		{"loopback", []string{"198.18.0.42", "127.0.0.1"}, nil},
		{"ULA", []string{"198.18.0.42", "fd00::1"}, nil},
		{"outside Fake-IP", []string{"198.18.0.42", "fc00:4000::1"}, nil},
		{"CGNAT", []string{"198.18.0.42", "100.64.0.1"}, nil},
		{"link local", []string{"198.18.0.42", "169.254.169.254"}, nil},
		{"scoped IPv6", []string{"198.18.0.42", "fc00::1234%fixture"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resolver stubIPResolver
			for _, raw := range tc.dns {
				addr := netip.MustParseAddr(raw)
				resolver.addrs = append(resolver.addrs, net.IPAddr{IP: addr.AsSlice(), Zone: addr.Zone()})
			}
			dialer := &recordingDialer{err: dialError}
			dial := makeSafeDialContext(dialer, resolver, false)
			_, err := dial(t.Context(), "tcp", "dual.example.test:443")
			wantErr := dialError
			if len(tc.want) == 0 {
				wantErr = ErrSSRFBlocked
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("dial error = %v, want %v", err, wantErr)
			}
			var wantAddresses []string
			for _, addr := range tc.want {
				wantAddresses = append(wantAddresses, net.JoinHostPort(addr, "443"))
			}
			if !slices.Equal(dialer.addresses, wantAddresses) {
				t.Errorf("dial addresses = %v, want %v", dialer.addresses, wantAddresses)
			}
		})
	}
}

func TestSafeDialBlocksInternalHostnamesAndLiteralIPs(t *testing.T) {
	t.Parallel()
	dialer := &recordingDialer{err: errors.New("unexpected dial")}
	dial := makeSafeDialContext(dialer, stubIPResolver{err: errors.New("unexpected DNS lookup")}, false)
	for _, host := range []string{
		"intranet", "localhost", "sub.localhost", "NaS.LoCaL.", "host.docker.internal",
		"printer.lan", "server.home.arpa", "host.localdomain", "1.in-addr.arpa", "1.ip6.arpa",
		"198.18.0.42", "::ffff:198.18.0.42", "fc00::1234", "0.0.0.0", "::",
	} {
		if _, err := dial(t.Context(), "tcp", net.JoinHostPort(host, "443")); !errors.Is(err, ErrSSRFBlocked) {
			t.Errorf("host %q: got %v, want ErrSSRFBlocked", host, err)
		}
	}
	if len(dialer.addresses) != 0 {
		t.Errorf("unexpected dial addresses %v", dialer.addresses)
	}
}

func TestSSRFClientBlocksNonPublicTargetsAndRedirects(t *testing.T) {
	t.Parallel()

	client := NewSafeHTTPClient("", 5*time.Second)

	disallowedURLs := []string{
		"http://127.0.0.1:8080/test",
		"http://[::1]:8080/test",
		"http://localhost:8080/test",
		"http://sub.localhost/test",
		"http://10.20.30.40/test",
		"http://172.16.5.4/test",
		"http://169.254.169.254/latest/meta-data/",
		"http://100.64.0.1/test",
		"http://0.0.0.0/test",
		"http://[::]/test",
		"http://198.18.0.42/test",
		"http://224.0.0.1/test",
		"http://[fe80::1]/test",
		"http://[fc00::1]/test",
		"http://[::ffff:127.0.0.1]/test",
		"http://[::ffff:10.0.0.1]/test",
		"http://[64:ff9b::127.0.0.1]/test",
		"file:///etc/passwd",
		"ftp://example.invalid/file",
	}
	for _, raw := range disallowedURLs {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatalf("NewRequest(%q): %v", raw, err)
		}
		_, doErr := client.Do(req)
		if doErr == nil {
			t.Fatalf("expected request to %q to be blocked", raw)
		}
		if !errors.Is(doErr, ErrSSRFBlocked) {
			t.Fatalf("expected ErrSSRFBlocked for %q, got %v", raw, doErr)
		}
	}

	// Verify DNS rebinding / mixed-A-record defense: if any resolved IP is private, dial fails.
	dialer := &net.Dialer{Timeout: time.Second}
	mixedDial := makeSafeDialContext(dialer, stubIPResolver{
		addrs: []net.IPAddr{
			{IP: net.ParseIP("93.184.216.34")},
			{IP: net.ParseIP("10.0.0.5")},
		},
	}, false)
	if _, err := mixedDial(context.Background(), "tcp", "dual.example.test:443"); !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("expected mixed public+private DNS resolution to be blocked, got %v", err)
	}

	if !isPublicAddr(netip.MustParseAddr("93.184.216.34"), false) {
		t.Fatalf("expected public IPv4 93.184.216.34 to be allowed")
	}
	if !isPublicAddr(netip.MustParseAddr("2606:4700:4700::1111"), false) {
		t.Fatalf("expected public IPv6 to be allowed")
	}

	// Verify proxy URL validation & target host checking in proxy mode.
	badProxyClient := NewSafeHTTPClient("ftp://127.0.0.1:8080", 5*time.Second)
	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid/repo.json", nil)
	if _, err := badProxyClient.Do(req); !errors.Is(err, ErrInvalidProxyURL) {
		t.Fatalf("expected ErrInvalidProxyURL, got %v", err)
	}

	proxyClient := NewSafeHTTPClient("http://127.0.0.1:8888", 5*time.Second)
	privReq, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest", nil)
	if _, err := proxyClient.Do(privReq); !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("expected link-local target to be blocked even with proxy, got %v", err)
	}
}

func TestSSRFClientRedirectsAndHeaderStripping(t *testing.T) {
	t.Parallel()

	var targetAuth atomic.Value
	var targetCookie atomic.Value
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetAuth.Store(r.Header.Get("Authorization"))
		targetCookie.Store(r.Header.Get("Cookie"))
		_, _ = w.Write([]byte("target-ok"))
	}))
	defer targetSrv.Close()

	var sameHostAuth atomic.Value
	var sameHostCookie atomic.Value
	var mux http.ServeMux
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) {
		step := r.URL.Query().Get("n")
		switch step {
		case "0", "1", "2", "3", "4":
			var n int
			_, _ = fmt.Sscanf(step, "%d", &n)
			http.Redirect(w, r, fmt.Sprintf("/hop?n=%d", n+1), http.StatusFound)
		case "5":
			sameHostAuth.Store(r.Header.Get("Authorization"))
			sameHostCookie.Store(r.Header.Get("Cookie"))
			_, _ = w.Write([]byte("five-hops-ok"))
		default:
			http.Redirect(w, r, "/hop?n=next", http.StatusFound)
		}
	})
	mux.HandleFunc("/hop-loop", func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, _ = fmt.Sscanf(r.URL.Query().Get("n"), "%d", &n)
		if n >= 6 {
			_, _ = w.Write([]byte("should-not-reach"))
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop-loop?n=%d", n+1), http.StatusFound)
	})
	mux.HandleFunc("/cross-host", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetSrv.URL+"/dest", http.StatusFound)
	})
	mux.HandleFunc("/to-private", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/internal", http.StatusFound)
	})
	mux.HandleFunc("/to-file", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})

	mainSrv := httptest.NewServer(&mux)
	defer mainSrv.Close()

	client := newSafeHTTPClient("", 5*time.Second, safeClientOptions{allowLoopbackForTest: true})

	// Up to 5 redirects on the same host should succeed and retain auth/cookie.
	req5, _ := http.NewRequest(http.MethodGet, mainSrv.URL+"/hop?n=0", nil)
	req5.Header.Set("Authorization", "Bearer secret-token")
	req5.Header.Set("Cookie", "session=abc")
	resp5, err := client.Do(req5)
	if err != nil {
		t.Fatalf("5 redirects failed: %v", err)
	}
	body5, _ := io.ReadAll(resp5.Body)
	resp5.Body.Close()
	if string(body5) != "five-hops-ok" {
		t.Fatalf("unexpected body %q", string(body5))
	}
	if sameHostAuth.Load() != "Bearer secret-token" || sameHostCookie.Load() != "session=abc" {
		t.Fatalf("expected same-host redirect to retain auth/cookie, got auth=%v cookie=%v", sameHostAuth.Load(), sameHostCookie.Load())
	}

	// 6th redirect must fail.
	req6, _ := http.NewRequest(http.MethodGet, mainSrv.URL+"/hop-loop?n=0", nil)
	if _, err := client.Do(req6); !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("expected ErrTooManyRedirects on 6th hop, got %v", err)
	}

	// Cross-host redirect (different port) must strip Authorization & Cookie.
	reqCross, _ := http.NewRequest(http.MethodGet, mainSrv.URL+"/cross-host", nil)
	reqCross.Header.Set("Authorization", "Bearer top-secret")
	reqCross.Header.Set("Cookie", "sid=xyz")
	respCross, err := client.Do(reqCross)
	if err != nil {
		t.Fatalf("cross-host redirect failed: %v", err)
	}
	respCross.Body.Close()
	if targetAuth.Load() != "" || targetCookie.Load() != "" {
		t.Fatalf("expected cross-host redirect to strip auth/cookie, got auth=%q cookie=%q", targetAuth.Load(), targetCookie.Load())
	}

	// Redirect to RFC1918 IP or file:// must be blocked even in allowLoopbackForTest mode.
	for _, path := range []string{"/to-private", "/to-file"} {
		r, _ := http.NewRequest(http.MethodGet, mainSrv.URL+path, nil)
		if _, err := client.Do(r); !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("expected ErrSSRFBlocked for redirect %s, got %v", path, err)
		}
	}
}

func TestSSRFProxyValidatesTargetsWithoutResolvingThem(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Host != "public.example.test" {
			t.Errorf("proxy received target %q", r.URL.Host)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()
	client := NewSafeHTTPClient(proxy.URL, 5*time.Second)
	defer client.CloseIdleConnections()

	for _, host := range []string{
		"metadata", "redis", "postgres", "intranet", "NaS.LoCaL.", "host.docker.internal",
		"printer.lan", "server.home.arpa", "host.localdomain", "1.in-addr.arpa", "1.ip6.arpa",
		"169.254.169.254", "198.18.0.42", "[fc00::1234]", "0.0.0.0", "[::]",
	} {
		resp, err := client.Get("http://" + host + "/")
		if resp != nil {
			resp.Body.Close()
		}
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Errorf("target %q: got %v, want ErrSSRFBlocked", host, err)
		}
	}
	resp, err := client.Get("http://public.example.test/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || hits.Load() != 1 {
		t.Fatalf("proxy status = %d, requests = %d", resp.StatusCode, hits.Load())
	}
}

func TestCoverSSRFAndCircuitDiagnostics(t *testing.T) {
	rec := testMetadataDiagnostics(t)
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/blocked":
			return nil, fmt.Errorf("%w: disallowed IP fc00::1234, fixture-secret", ErrSSRFBlocked)
		case "/forbidden":
			return managerResponse(r, http.StatusForbidden, strings.NewReader("")), nil
		case "/limit":
			return managerResponse(r, http.StatusTooManyRequests, strings.NewReader("")), nil
		case "/unavailable":
			return managerResponse(r, http.StatusServiceUnavailable, strings.NewReader("")), nil
		default:
			return managerResponse(r, http.StatusNotFound, strings.NewReader("")), nil
		}
	})}
	svc, err := NewCoverService(CoverConfig{DataDir: t.TempDir(), Client: client, Burst: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want error
	}{
		{"/blocked", ErrSSRFBlocked},
		{"/forbidden", ErrCircuitOpen},
		{"/limit", ErrCircuitOpen},
		{"/unavailable", ErrCircuitOpen},
		{"/missing", nil},
	} {
		_, err := svc.GetOrFetch(t.Context(), tc.path, "fixture", "https://repo.example.test"+tc.path)
		if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("cover fetch %s returned %v, want failure %v", tc.path, err, tc.want)
		}
	}
	events := rec.Recent(10)
	want := []struct {
		code       string
		httpStatus any
	}{
		{"SSRF_BLOCKED", nil},
		{"CIRCUIT_OPEN", float64(http.StatusForbidden)},
		{"CIRCUIT_OPEN", float64(http.StatusTooManyRequests)},
		{"CIRCUIT_OPEN", float64(http.StatusServiceUnavailable)},
	}
	if len(events) != len(want) {
		t.Fatalf("cover diagnostics = %+v, want %d events", events, len(want))
	}
	for i, ev := range events {
		if ev.Component != "extmeta" || ev.Stage != "extmeta_cover" || ev.Action != "fetch" ||
			ev.Level != diagnostics.LevelWarn || ev.Code != want[i].code || ev.Meta["httpStatus"] != want[i].httpStatus || ev.Detail == "" {
			t.Errorf("unexpected cover diagnostic: %+v", ev)
		}
	}
	assertSafeMetadataDiagnostics(t, rec.ExportText(10))
}

func TestRemoteUpdateAndDownloadPreserveHTTPStatus(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		status := http.StatusNotFound
		if r.Method == http.MethodHead {
			status = http.StatusMethodNotAllowed
		}
		return managerResponse(r, status, strings.NewReader("")), nil
	})}
	const upstream = "https://repo.example.test/source.sqlite"
	_, releaseErr := CheckRemoteUpdate(t.Context(), client, "", upstream, "", "", "", 0, "")
	_, rangeErr := CheckRemoteUpdate(t.Context(), client, upstream, "", "", "", "", 0, "")
	_, downloadErr := DownloadAndDecompressToFile(t.Context(), client, upstream, "", filepath.Join(t.TempDir(), "source.sqlite"), nil)
	for _, err := range []error{releaseErr, rangeErr, downloadErr} {
		var httpErr *HTTPStatusError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
			t.Errorf("remote HTTP error = %v, want typed HTTP 404", err)
		}
	}
}

func TestRepoManifestParseFetchAndValidation(t *testing.T) {
	t.Parallel()

	if RecommendedRepoURL != "https://raw.githubusercontent.com/BlackMuseCode/mangaalpha-metadata-repo/main/repo.json" {
		t.Fatalf("unexpected RecommendedRepoURL: %q", RecommendedRepoURL)
	}

	validJSON := []byte(`{
		"schema": "mangaalpha-repo-v1",
		"name": "Community Metadata Repo",
		"description": "Open metadata packs",
		"homepage": "https://example.test/repo",
		"sources": [
			{
				"id": "bgm-zh",
				"kind": "bangumi_archive",
				"name": "Bangumi Chinese Archive",
				"manifestUrl": "https://example.test/bgm/latest.json",
				"autoUpdateDefault": true,
				"default": true
			},
			{
				"id": "open-meta-1",
				"kind": "offline_sqlite",
				"schema": "mangaalpha_meta_v1",
				"name": "Open Comic Index",
				"downloadUrl": "https://example.test/meta.sqlite.zst",
				"releaseApiUrl": "https://api.example.test/releases/latest",
				"assetName": "meta.sqlite.zst",
				"compression": "zst",
				"sizeHintBytes": 1048576,
				"defaultTagDictId": "zh-tags-1",
				"nsfwRule": "non_h_exempt",
				"homepage": "https://example.test/meta",
				"contributeUrl": "https://example.test/meta/issues"
			},
			{
				"id": "zh-tags-1",
				"kind": "tag_dictionary",
				"schema": "tag_translation_text_v1",
				"name": "Tag Translations",
				"downloadUrl": "https://example.test/db.text.json.gz",
				"compression": "gz"
			},
			{
				"id": "community-map-1",
				"kind": "mapping_pack",
				"schema": "mangaalpha_mapping_v1",
				"name": "Community Mappings",
				"downloadUrl": "https://example.test/mappings.ndjson.gz",
				"compression": "gz"
			}
		]
	}`)

	manifest, err := ParseRepoManifest(validJSON)
	if err != nil {
		t.Fatalf("ParseRepoManifest valid failed: %v", err)
	}
	if len(manifest.Sources) != 4 || manifest.Sources[1].DefaultTagDictID != "zh-tags-1" || !manifest.Sources[0].Default {
		t.Fatalf("unexpected parsed manifest: %+v", manifest)
	}

	invalidCases := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"invalid_json", []byte(`<html>`)},
		{"wrong_schema", []byte(`{"schema":"other-v1","sources":[]}`)},
		{"duplicate_id", []byte(`{"schema":"mangaalpha-repo-v1","sources":[{"id":"dup","kind":"mapping_pack","name":"A"},{"id":"dup","kind":"tag_dictionary","name":"B"}]}`)},
		{"bad_id_slash", []byte(`{"schema":"mangaalpha-repo-v1","sources":[{"id":"a/b","kind":"mapping_pack","name":"A"}]}`)},
		{"bad_id_dotdot", []byte(`{"schema":"mangaalpha-repo-v1","sources":[{"id":"..","kind":"mapping_pack","name":"A"}]}`)},
		{"bad_kind", []byte(`{"schema":"mangaalpha-repo-v1","sources":[{"id":"a1","kind":"unknown_kind","name":"A"}]}`)},
		{"bad_url_scheme", []byte(`{"schema":"mangaalpha-repo-v1","sources":[{"id":"a1","kind":"mapping_pack","name":"A","downloadUrl":"file:///tmp/x"}]}`)},
		{"oversize", bytes.Repeat([]byte("a"), MaxRepoManifestBytes+1)},
	}
	for _, tc := range invalidCases {
		if _, err := ParseRepoManifest(tc.body); !errors.Is(err, ErrInvalidManifest) {
			t.Fatalf("ParseRepoManifest(%s) = %v, want ErrInvalidManifest", tc.name, err)
		}
	}

	// Test FetchRepoManifest over httptest.Server (including > 4 MiB rejection).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.json":
			_, _ = w.Write(validJSON)
		case "/too-big.json":
			_, _ = w.Write(bytes.Repeat([]byte(" "), MaxRepoManifestBytes+128))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := newSafeHTTPClient("", 5*time.Second, safeClientOptions{allowLoopbackForTest: true})
	fetched, err := FetchRepoManifest(context.Background(), client, srv.URL+"/ok.json")
	if err != nil {
		t.Fatalf("FetchRepoManifest failed: %v", err)
	}
	if len(fetched.Sources) != 4 {
		t.Fatalf("expected 4 fetched sources, got %d", len(fetched.Sources))
	}
	if _, err := FetchRepoManifest(context.Background(), client, srv.URL+"/too-big.json"); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("oversized remote repo manifest = %v, want ErrInvalidManifest", err)
	}
}

func TestRepoManifestAutoUpdatePreferences(t *testing.T) {
	const body = `{
		"schema": "mangaalpha-repo-v1",
		"name": "Community Metadata",
		"version": "2026.11.25.1",
		"updatedAt": "2026-11-25T00:00:00Z",
		"sources": [
			{"id":"archive","kind":"bangumi_archive","name":"Archive","manifestUrl":"zh_cn","autoUpdateDefault":"weekly"},
			{"id":"catalog","kind":"offline_sqlite","name":"Catalog","downloadUrl":"https://example.test/catalog.sqlite","autoUpdateDefault":"manual"},
			{"id":"tags","kind":"tag_dictionary","name":"Tags","autoUpdateDefault":" Monthly "},
			{"id":"legacy-on","kind":"mapping_pack","name":"Legacy on","autoUpdateDefault":true},
			{"id":"legacy-off","kind":"mapping_pack","name":"Legacy off","autoUpdateDefault":false}
		]
	}`
	manifest, err := ParseRepoManifest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "2026.11.25.1" {
		t.Fatalf("version = %q", manifest.Version)
	}
	want := []AutoUpdatePreference{"weekly", "manual", "monthly", "weekly", "manual"}
	for i, source := range manifest.Sources {
		if source.AutoUpdateDefault != want[i] {
			t.Errorf("%s: schedule = %q, want %q", source.ID, source.AutoUpdateDefault, want[i])
		}
		data, err := json.Marshal(source.AutoUpdateDefault)
		if err != nil || string(data) != fmt.Sprintf("%q", want[i]) {
			t.Errorf("%s: marshaled schedule = %s, err = %v", source.ID, data, err)
		}
	}
	for _, invalid := range []string{`"daily"`, `""`, "null", "1", "[]", "{}"} {
		var preference AutoUpdatePreference
		if err := json.Unmarshal([]byte(invalid), &preference); err == nil {
			t.Errorf("accepted invalid schedule %s", invalid)
		}
	}
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		return managerResponse(r, http.StatusOK, strings.NewReader(body)), nil
	})}
	store, manager, _ := newTestManager(t, ManagerOptions{HTTPClient: client})
	ctx := t.Context()
	repo, err := manager.SubscribeRepo(ctx, "https://example.test/repo.json")
	if err != nil || repo.RepoVersion != manifest.Version {
		t.Fatalf("subscribed version = %q, err = %v", repo.RepoVersion, err)
	}
	sources, err := store.ListMetadataSources(ctx, "")
	if err != nil || len(sources) != len(want) {
		t.Fatalf("subscribed source count = %d, err = %v", len(sources), err)
	}
	for i, declared := range manifest.Sources {
		for _, source := range sources {
			if source.SourceKey == declared.ID && source.AutoUpdate != string(want[i]) {
				t.Errorf("%s: saved schedule = %q, want %q", source.SourceKey, source.AutoUpdate, want[i])
			}
		}
	}
}

func TestDecompressAndRemoteUpdateCheck(t *testing.T) {
	t.Parallel()

	samplePayload := bytes.Repeat([]byte("SQLite format 3\x00test-metadata-block-0123456789abcdef\n"), 4000)
	sum := sha256.Sum256(samplePayload)
	wantSHA := hex.EncodeToString(sum[:])

	// Prepare zstd, gzip, and zip payloads.
	var zstBuf bytes.Buffer
	zw, err := zstd.NewWriter(&zstBuf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	_, _ = zw.Write(samplePayload)
	_ = zw.Close()

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write(samplePayload)
	_ = gw.Close()

	var zipBuf bytes.Buffer
	zipW := zip.NewWriter(&zipBuf)
	readmeEntry, _ := zipW.Create("README.txt")
	_, _ = readmeEntry.Write([]byte("ignore me"))
	macEntry, _ := zipW.Create("__MACOSX/._db.sqlite")
	_, _ = macEntry.Write([]byte("ignore mac resource"))
	dbEntry, _ := zipW.Create("nested/archive.sqlite")
	_, _ = dbEntry.Write(samplePayload)
	_ = zipW.Close()

	var relUpdatedAt atomic.Value
	relUpdatedAt.Store("2026-11-24T00:00:00Z")
	var assetSize atomic.Int64
	assetSize.Store(1024)
	var etagVal atomic.Value
	etagVal.Store(`"v1"`)

	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/release":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "nightly",
				"assets": []map[string]any{
					{
						"name":                 "meta.sqlite.zst",
						"size":                 assetSize.Load(),
						"updated_at":           relUpdatedAt.Load(),
						"browser_download_url": srvURL + "/meta.sqlite.zst",
					},
				},
			})
		case "/meta.sqlite.zst":
			w.Header().Set("ETag", etagVal.Load().(string))
			w.Header().Set("Last-Modified", "Tue, 24 Nov 2026 00:00:00 GMT")
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", zstBuf.Len()))
				return
			}
			_, _ = w.Write(zstBuf.Bytes())
		case "/meta.sqlite.gz":
			_, _ = w.Write(gzBuf.Bytes())
		case "/meta.zip":
			_, _ = w.Write(zipBuf.Bytes())
		case "/raw.db":
			_, _ = w.Write(samplePayload)
		case "/no-head.db":
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("ETag", `"range-v1"`)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(samplePayload)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(samplePayload[:1])
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	client := newSafeHTTPClient("", 10*time.Second, safeClientOptions{allowLoopbackForTest: true})
	ctx := context.Background()

	// CheckRemoteUpdate with release API: unchanged -> HasUpdate=false; updated_at changed (nightly reuse) -> HasUpdate=true.
	up1, err := CheckRemoteUpdate(ctx, client, "", srv.URL+"/release", "meta.sqlite.zst", "", "", 1024, "2026-11-24T00:00:00Z")
	if err != nil {
		t.Fatalf("CheckRemoteUpdate release unchanged: %v", err)
	}
	if up1.HasUpdate {
		t.Fatalf("expected HasUpdate=false for matching updated_at & size, got %+v", up1)
	}

	relUpdatedAt.Store("2026-11-25T00:00:00Z")
	up2, err := CheckRemoteUpdate(ctx, client, "", srv.URL+"/release", "meta.sqlite.zst", "", "", 1024, "2026-11-24T00:00:00Z")
	if err != nil {
		t.Fatalf("CheckRemoteUpdate release nightly changed: %v", err)
	}
	if !up2.HasUpdate || up2.DownloadURL != srv.URL+"/meta.sqlite.zst" {
		t.Fatalf("expected HasUpdate=true with resolved DownloadURL, got %+v", up2)
	}

	// CheckRemoteUpdate via HEAD ETag and 405 Range fallback.
	headSame, err := CheckRemoteUpdate(ctx, client, srv.URL+"/meta.sqlite.zst", "", "", `"v1"`, "", int64(zstBuf.Len()), "")
	if err != nil || headSame.HasUpdate {
		t.Fatalf("expected HEAD ETag match up-to-date, got %+v err=%v", headSame, err)
	}
	etagVal.Store(`"v2"`)
	headDiff, err := CheckRemoteUpdate(ctx, client, srv.URL+"/meta.sqlite.zst", "", "", `"v1"`, "", int64(zstBuf.Len()), "")
	if err != nil || !headDiff.HasUpdate {
		t.Fatalf("expected HEAD ETag change to report update, got %+v err=%v", headDiff, err)
	}
	head405, err := CheckRemoteUpdate(ctx, client, srv.URL+"/no-head.db", "", "", `"range-v1"`, "", int64(len(samplePayload)), "")
	if err != nil || head405.HasUpdate {
		t.Fatalf("expected 405 HEAD range fallback match, got %+v err=%v", head405, err)
	}

	// Stream decompress zst, gz, zip, and none.
	tmpDir := t.TempDir()
	cases := []struct {
		path string
		comp string
	}{
		{"/meta.sqlite.zst", ""},
		{"/meta.sqlite.gz", "gzip"},
		{"/meta.zip", "zip"},
		{"/raw.db", "none"},
	}
	for i, tc := range cases {
		dest := filepath.Join(tmpDir, fmt.Sprintf("out_%d.sqlite", i))
		var progressCalls atomic.Int32
		res, err := DownloadAndDecompressToFile(ctx, client, srv.URL+tc.path, tc.comp, dest, func(_, _, _ int64) {
			progressCalls.Add(1)
		})
		if err != nil {
			t.Fatalf("DownloadAndDecompressToFile(%s): %v", tc.path, err)
		}
		if res.SHA256 != wantSHA || res.DecompressedBytes != int64(len(samplePayload)) {
			t.Fatalf("mismatch for %s: sha=%s want=%s bytes=%d want=%d", tc.path, res.SHA256, wantSHA, res.DecompressedBytes, len(samplePayload))
		}
		if progressCalls.Load() == 0 {
			t.Fatalf("expected progress callback for %s", tc.path)
		}
		diskBytes, err := os.ReadFile(dest)
		if err != nil || !bytes.Equal(diskBytes, samplePayload) {
			t.Fatalf("disk content mismatch for %s: err=%v", tc.path, err)
		}
	}

	// Verify zip bomb / decompressed size ceiling preserves existing destination file.
	protectedDest := filepath.Join(tmpDir, "protected.sqlite")
	if err := os.WriteFile(protectedDest, []byte("keep-old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadAndDecompressToFileWithLimit(ctx, client, srv.URL+"/meta.zip", "zip", protectedDest, 1024, nil); err == nil {
		t.Fatalf("expected zip exceeding decompressed byte limit to fail")
	}
	if got, _ := os.ReadFile(protectedDest); string(got) != "keep-old" {
		t.Fatalf("expected existing file to remain untouched on failed decompress, got %q", string(got))
	}
}

func TestSchemasSidecarFTS5AndThumbSHA1(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	ctx := context.Background()

	// 1. Test gallery_v1 schema + Non-H NSFW rule + artist/group extraction + thumb SHA1 + FTS5 CJK.
	galDBPath := filepath.Join(tmpDir, "gallery.sqlite")
	galDB, err := sql.Open("sqlite", galDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := galDB.Exec(`
CREATE TABLE gallery (
  gid INTEGER PRIMARY KEY,
  token TEXT,
  title TEXT,
  title_jpn TEXT,
  category TEXT,
  thumb TEXT,
  uploader TEXT,
  posted INTEGER,
  filecount INTEGER,
  filesize INTEGER,
  expunged INTEGER,
  removed INTEGER,
  replaced INTEGER,
  rating REAL,
  root_gid INTEGER
);
CREATE TABLE tag (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL
);
CREATE TABLE gid_tid (
  gid INTEGER NOT NULL,
  tid INTEGER NOT NULL,
  PRIMARY KEY (gid, tid)
);
CREATE TABLE gallery_meta (
  key TEXT PRIMARY KEY,
  value TEXT
);
INSERT INTO gallery VALUES
  (1001, 'tok1', '[AuthorA] 魔法少女の物語', '魔法少女の物語', 'Non-H', 'https://cdn.example.test/t/4c/6a/4c6a1234567890abcdef1234567890abcdef1234-123-800-1200-jpg_250.jpg', 'up1', 1700000100, 32, 10485760, 0, 0, 0, 4.8, 0),
  (1002, 'tok2', '灌籃高手 特別編', 'スラムダンク', 'Doujinshi', 'https://cdn.example.test/t/aa/bb/aabb1234567890abcdef1234567890abcdef5678-456-700-1000-jpg_250.jpg', 'up2', 1700000200, 24, 8388608, 0, 0, 0, 4.5, 0),
  (1003, 'tok3', '黒の契約者 外伝', '', 'Manga', 'https://cdn.example.test/t/cc/dd/ccdd1234567890abcdef1234567890abcdef9012.jpg', 'up3', 1700000300, 18, 5242880, 0, 0, 0, 4.1, 0);
INSERT INTO tag VALUES
  (1, 'artist:Artist One'),
  (2, 'group:Circle Alpha'),
  (3, 'female:glasses'),
  (4, 'parody:original'),
  (5, 'language:chinese');
INSERT INTO gid_tid VALUES
  (1001, 1), (1001, 2), (1001, 3), (1001, 5),
  (1002, 4), (1002, 5);
`); err != nil {
		t.Fatal(err)
	}
	if err := galDB.Close(); err != nil {
		t.Fatal(err)
	}

	// Make upstream DB read-only to assert BuildSidecarIndex & SidecarStore never write to it.
	if err := os.Chmod(galDBPath, 0o444); err != nil {
		t.Fatal(err)
	}
	galSidecar := filepath.Join(tmpDir, "gallery.sidecar.sqlite")
	var indexedTotal int64
	if err := BuildSidecarIndex(ctx, galDBPath, galSidecar, SchemaAuto, func(n int64) {
		indexedTotal = n
	}); err != nil {
		t.Fatalf("BuildSidecarIndex(gallery_v1): %v", err)
	}
	if indexedTotal != 3 {
		t.Fatalf("expected 3 indexed gallery rows, got %d", indexedTotal)
	}

	store, err := OpenSidecarStore("gal-src", galDBPath, galSidecar, SchemaAuto)
	if err != nil {
		t.Fatalf("OpenSidecarStore(gallery_v1): %v", err)
	}
	defer store.Close()
	if store.Schema() != SchemaGalleryV1 {
		t.Fatalf("expected auto-detected schema %q, got %q", SchemaGalleryV1, store.Schema())
	}

	// Search Simplified Chinese for Traditional Chinese entry ("灌籃高手 特別編").
	hits, err := store.SearchByTitle(ctx, "灌篮高手", 10)
	if err != nil {
		t.Fatalf("SearchByTitle(灌篮高手): %v", err)
	}
	if len(hits) == 0 || hits[0].EntryID != "1002" || !hits[0].IsNSFW {
		t.Fatalf("unexpected SearchByTitle(灌篮高手) hits: %+v", hits)
	}

	// Search Japanese Shinjitai folding ("黑の契约者" vs "黒の契約者").
	hitsJP, err := store.SearchByTitle(ctx, "黑の契约者 外传", 10)
	if err != nil {
		t.Fatalf("SearchByTitle(黑の契约者): %v", err)
	}
	if len(hitsJP) == 0 || hitsJP[0].EntryID != "1003" {
		t.Fatalf("expected entry 1003 via CJK variant folding, got %+v", hitsJP)
	}

	// Lookup by cover thumb SHA-1.
	shaHits, err := store.LookupByThumbSHA1(ctx, "4C6A1234567890ABCDEF1234567890ABCDEF1234")
	if err != nil {
		t.Fatalf("LookupByThumbSHA1: %v", err)
	}
	if len(shaHits) != 1 || shaHits[0].EntryID != "1001" || shaHits[0].IsNSFW {
		t.Fatalf("expected Non-H entry 1001 (IsNSFW=false), got %+v", shaHits)
	}

	// Full entry lookup on Non-H entry: verify Authors, Tags, and IsNSFW=false.
	entry1001, err := store.GetEntry(ctx, "1001")
	if err != nil {
		t.Fatalf("GetEntry(1001): %v", err)
	}
	if entry1001.IsNSFW {
		t.Fatalf("expected Non-H gallery 1001 IsNSFW=false")
	}
	if len(entry1001.Authors) != 2 || entry1001.Authors[0] != "Artist One" || entry1001.Authors[1] != "Circle Alpha" {
		t.Fatalf("unexpected Authors: %+v", entry1001.Authors)
	}
	if len(entry1001.Tags) != 2 || entry1001.Tags[0] != "female:glasses" || entry1001.Tags[1] != "language:chinese" {
		t.Fatalf("unexpected Tags: %+v", entry1001.Tags)
	}

	entry1002, err := store.GetEntry(ctx, "1002")
	if err != nil || !entry1002.IsNSFW {
		t.Fatalf("expected Doujinshi entry 1002 IsNSFW=true, got %+v err=%v", entry1002, err)
	}
	if _, err := store.GetEntry(ctx, "999999"); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("expected ErrEntryNotFound, got %v", err)
	}

	// 2. Test mangaalpha_meta_v1 schema.
	metaDBPath := filepath.Join(tmpDir, "meta_v1.sqlite")
	metaDB, err := sql.Open("sqlite", metaDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := metaDB.Exec(`
CREATE TABLE entries (
  id TEXT PRIMARY KEY,
  title TEXT,
  title_alt TEXT,
  authors TEXT,
  summary TEXT,
  tags TEXT,
  category TEXT,
  cover_url TEXT,
  rating REAL,
  is_nsfw INTEGER,
  updated_at INTEGER,
  extra_json TEXT
);
INSERT INTO entries VALUES (
  'm-1',
  '水星領航員',
  'ARIA',
  '["天野梢"]',
  'Neo Venezia slice of life manga',
  '["治愈","科幻"]',
  'Manga',
  'https://cdn.example.test/covers/0123456789abcdef0123456789abcdef01234567.jpg',
  9.2,
  0,
  1700001000,
  '{"source_url":"https://example.test/item/m-1","volumes":12}'
);
`); err != nil {
		t.Fatal(err)
	}
	if err := metaDB.Close(); err != nil {
		t.Fatal(err)
	}

	metaSidecar := filepath.Join(tmpDir, "meta_v1.sidecar.sqlite")
	if err := BuildSidecarIndex(ctx, metaDBPath, metaSidecar, SchemaMangaAlphaMetaV1, nil); err != nil {
		t.Fatalf("BuildSidecarIndex(mangaalpha_meta_v1): %v", err)
	}
	metaStore, err := OpenSidecarStore("meta-src", metaDBPath, metaSidecar, SchemaAuto)
	if err != nil {
		t.Fatalf("OpenSidecarStore(meta_v1): %v", err)
	}
	defer metaStore.Close()
	if metaStore.Schema() != SchemaMangaAlphaMetaV1 {
		t.Fatalf("expected %q, got %q", SchemaMangaAlphaMetaV1, metaStore.Schema())
	}

	mHits, err := metaStore.SearchByTitle(ctx, "水星领航员", 5)
	if err != nil || len(mHits) != 1 || mHits[0].EntryID != "m-1" || mHits[0].Score != 1.0 {
		t.Fatalf("unexpected meta_v1 search hits: %+v err=%v", mHits, err)
	}
	mEntry, err := metaStore.GetEntry(ctx, "m-1")
	if err != nil {
		t.Fatalf("GetEntry(m-1): %v", err)
	}
	if mEntry.IsNSFW || mEntry.SourceURL != "https://example.test/item/m-1" || len(mEntry.Authors) != 1 || mEntry.Authors[0] != "天野梢" {
		t.Fatalf("unexpected m-1 entry: %+v", mEntry)
	}
}

func TestTagDictionaryModesAndConcurrency(t *testing.T) {
	t.Parallel()

	rawJSON := []byte(`{
		"data": [
			{
				"namespace": "female",
				"data": {
					"glasses": {"name": "眼镜"},
					"schoolgirl uniform": {"name": "水手服"}
				}
			},
			{
				"namespace": "other",
				"data": {
					"full color": {"name": "全彩"}
				}
			}
		]
	}`)

	dictPlain, err := ParseTagDictionary(bytes.NewReader(rawJSON))
	if err != nil {
		t.Fatalf("ParseTagDictionary plain: %v", err)
	}

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write(rawJSON)
	_ = gw.Close()
	dictGZ, err := ParseTagDictionary(bytes.NewReader(gzBuf.Bytes()))
	if err != nil {
		t.Fatalf("ParseTagDictionary gzip: %v", err)
	}
	if dictPlain.Len() != 3 || dictGZ.Len() != 3 {
		t.Fatalf("expected 3 entries, got plain=%d gz=%d", dictPlain.Len(), dictGZ.Len())
	}

	if got := dictGZ.Translate("female:glasses", ModeZH); got != "眼镜" {
		t.Fatalf("ModeZH translated=%q, want 眼镜", got)
	}
	if got := dictGZ.Translate("female:unknown", ModeZH); got != "female:unknown" {
		t.Fatalf("ModeZH fallback=%q, want female:unknown", got)
	}
	if got := dictGZ.Translate("female:glasses", ModeBilingual); got != "眼镜 (female:glasses)" {
		t.Fatalf("ModeBilingual=%q", got)
	}
	if got := dictGZ.Translate("female:glasses", ModeRaw); got != "female:glasses" {
		t.Fatalf("ModeRaw=%q", got)
	}

	list := dictGZ.TranslateList([]string{"female:glasses", "female:glasses", "other:full color", "untranslated"}, ModeZH)
	if len(list) != 3 || list[0] != "眼镜" || list[1] != "全彩" || list[2] != "untranslated" {
		t.Fatalf("unexpected TranslateList: %+v", list)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = dictGZ.Translate("female:glasses", ModeBilingual)
				_ = dictGZ.TranslateList([]string{"glasses", "other:full color"}, ModeZH)
			}
		}()
	}
	wg.Wait()
}

func TestMappingsParseLookupAndSanitizedExport(t *testing.T) {
	t.Parallel()

	ndjson := strings.Join([]string{
		"",
		`{not valid json`,
		`{"schema":"wrong","key":"Bad","targetSource":"bangumi","targetId":"1"}`,
		`{"schema":"mangaalpha_mapping_v1","key":"Private Path","rawKey":"/home/alice/manga/test","targetSource":"bangumi","targetId":"2"}`,
		`{"schema":"mangaalpha_mapping_v1","key":"Private IP","rawKey":"10.20.30.40","targetSource":"bangumi","targetId":"3"}`,
		`{"schema":"mangaalpha_mapping_v1","key":"Private URL","rawKey":"https://private.example.test/secret","targetSource":"bangumi","targetId":"4"}`,
		`{"schema":"mangaalpha_mapping_v1","key":"Leak Token","author":"fk115_secret_key","targetSource":"bangumi","targetId":"5"}`,
		`{"schema":"mangaalpha_mapping_v1","key":"魔法少女","rawKey":"[作者A] 魔法少女 Vol.01","author":"作者A","targetSource":"bangumi","targetId":"101","origin":"manual"}`,
		`{"schema":"mangaalpha_mapping_v1","key":"魔法少女","rawKey":"[作者A] 魔法少女 Vol.01","author":"作者A","targetSource":"bangumi","targetId":"999","origin":"duplicate"}`,
		`{"schema":"mangaalpha_mapping_v1","key":"魔法少女","rawKey":"[作者B] 魔法少女","author":"作者B","targetSource":"bangumi","targetId":"102","origin":"agent"}`,
		`{"schema":"mangaalpha_mapping_v1","key":"日常物語","rawKey":"日常物语 raw","author":"","targetSource":"bangumi","targetId":"201","origin":"manual"}`,
		`{"schema":"mangaalpha_mapping_v1","key":"成人作品","rawKey":"adult raw","author":"作者C","targetSource":"gallery_v1","targetId":"301","isNsfw":true,"origin":"manual"}`,
	}, "\n")

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write([]byte(ndjson))
	_ = gw.Close()

	records, err := ParseMappingsNDJSON(bytes.NewReader(gzBuf.Bytes()))
	if err != nil {
		t.Fatalf("ParseMappingsNDJSON failed: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("expected 4 valid deduplicated records, got %d: %+v", len(records), records)
	}

	idx := NewMappingIndex(records)

	// Priority 1: normKey + normAuthor
	if rec, ok := idx.Lookup("魔法少女", "", "作者B", TargetSourceBangumi); !ok || rec.TargetID != "102" {
		t.Fatalf("expected 魔法少女+作者B -> 102, got ok=%v rec=%+v", ok, rec)
	}

	// Author conflict guard: 魔法少女 has AuthorA and AuthorB -> querying with unknown AuthorC or empty author must NOT guess.
	if _, ok := idx.Lookup("魔法少女", "", "作者C", TargetSourceBangumi); ok {
		t.Fatalf("expected author conflict for 魔法少女+作者C")
	}
	if _, ok := idx.Lookup("魔法少女", "", "", TargetSourceBangumi); ok {
		t.Fatalf("expected ambiguity rejection for 魔法少女 without author")
	}

	// Priority 2: normKey without author conflict (authorless record matches with or without caller author).
	if rec, ok := idx.Lookup("日常物语", "", "任意作者", TargetSourceBangumi); !ok || rec.TargetID != "201" {
		t.Fatalf("expected 日常物语 authorless match -> 201, got ok=%v rec=%+v", ok, rec)
	}

	// Priority 3 & 4: rawKey fallback (with author & without author).
	if rec, ok := idx.Lookup("不匹配的主键", "[作者A] 魔法少女 Vol.01", "作者A", TargetSourceBangumi); !ok || rec.TargetID != "101" {
		t.Fatalf("expected rawKey+author fallback -> 101, got ok=%v rec=%+v", ok, rec)
	}
	if rec, ok := idx.Lookup("不匹配的主键", "日常物语 raw", "", TargetSourceBangumi); !ok || rec.TargetID != "201" {
		t.Fatalf("expected rawKey fallback -> 201, got ok=%v rec=%+v", ok, rec)
	}

	// SanitizeAndExportMappings: default excludes NSFW, enforces origin filter & strips private paths.
	exportRecords := append([]MappingRecord{}, records...)
	exportRecords = append(exportRecords, MappingRecord{
		Schema:       MappingSchemaV1,
		Key:          "BadWinPath",
		RawKey:       `C:\Users\demo\comics\secret`,
		TargetSource: TargetSourceBangumi,
		TargetID:     "555",
		Origin:       "manual",
	})

	exportedDefault, err := SanitizeAndExportMappings(exportRecords, ExportMappingsOptions{
		AllowedOrigins: []string{"manual"},
	})
	if err != nil {
		t.Fatalf("SanitizeAndExportMappings default: %v", err)
	}
	parsedDefault, err := ParseMappingsNDJSON(bytes.NewReader(exportedDefault))
	if err != nil {
		t.Fatalf("reparse exported default: %v", err)
	}
	// Only 魔法少女(manual) and 日常物語(manual) should remain; agent(102), nsfw(301), and BadWinPath(555) excluded.
	if len(parsedDefault) != 2 {
		t.Fatalf("expected 2 sanitized manual non-NSFW records, got %d: %+v", len(parsedDefault), parsedDefault)
	}

	exportedWithNSFW, err := SanitizeAndExportMappings(exportRecords, ExportMappingsOptions{
		IncludeNSFW:    true,
		AllowedOrigins: []string{"manual"},
	})
	if err != nil {
		t.Fatalf("SanitizeAndExportMappings includeNSFW: %v", err)
	}
	parsedNSFW, _ := ParseMappingsNDJSON(bytes.NewReader(exportedWithNSFW))
	if len(parsedNSFW) != 3 {
		t.Fatalf("expected 3 records with IncludeNSFW=true, got %d", len(parsedNSFW))
	}
}

func TestMappingIPv6Literals(t *testing.T) {
	for _, input := range []string{"fe80::1", "Before [2001:0db8:85a3:0000:0000:8a2e:0370:7334] After"} {
		if !containsSensitiveOrPrivateValue(input) {
			t.Errorf("IPv6 literal was allowed in %q", input)
		}
	}
	for _, input := range []string{"Re:Zero: Starting Life", "12:34:56"} {
		if containsSensitiveOrPrivateValue(input) {
			t.Errorf("ordinary title was rejected: %q", input)
		}
	}
}

func TestArchiveEmbeddedMetadataParsers(t *testing.T) {
	t.Parallel()

	comicXML := `<?xml version="1.0" encoding="utf-8"?>
<ComicInfo>
  <Title>Aria Vol. 01</Title>
  <Series>Aria</Series>
  <Writer>Kozue Amano</Writer>
  <Penciller>Kozue Amano, Assistant One</Penciller>
  <Summary>Gondola training on Aqua.</Summary>
  <Genre>Slice of Life, Sci-Fi</Genre>
  <Tags>healing, venice, healing</Tags>
  <Web>https://bgm.tv/subject/539</Web>
  <AgeRating>Teen</AgeRating>
</ComicInfo>`

	metaXML, err := ParseComicInfoXML(strings.NewReader(comicXML))
	if err != nil {
		t.Fatalf("ParseComicInfoXML: %v", err)
	}
	if metaXML.Title != "Aria Vol. 01" || metaXML.Series != "Aria" || metaXML.ExternalID != "539" || metaXML.IsNSFW {
		t.Fatalf("unexpected ComicInfo meta: %+v", metaXML)
	}
	if len(metaXML.Authors) != 2 || len(metaXML.Genre) != 2 || len(metaXML.Tags) != 2 {
		t.Fatalf("unexpected deduplicated ComicInfo lists: %+v", metaXML)
	}

	galleryTXT := `Title:       Test Work Title
Category:    Non-H
Tags:        artist:Artist A, group:Circle B, female:glasses, language:chinese
URL:         https://gallery.example.test/g/123456/abcdef0123/

Uploader Comment:
Line one of summary.
Line two of summary.`

	metaTXT, err := ParseGalleryInfoTXT(strings.NewReader(galleryTXT))
	if err != nil {
		t.Fatalf("ParseGalleryInfoTXT: %v", err)
	}
	if metaTXT.Title != "Test Work Title" || metaTXT.IsNSFW || metaTXT.ExternalID != "123456" {
		t.Fatalf("unexpected galleryinfo meta: %+v", metaTXT)
	}
	if len(metaTXT.Authors) != 2 || len(metaTXT.Tags) != 2 || !strings.Contains(metaTXT.Summary, "Line two") {
		t.Fatalf("unexpected galleryinfo parsed fields: %+v", metaTXT)
	}

	// Verify >512 KiB size limit protection.
	oversize := bytes.Repeat([]byte("A"), int(maxEmbeddedMetaBytes)+10)
	if _, err := ParseComicInfoXML(bytes.NewReader(oversize)); err == nil {
		t.Fatalf("expected oversized ComicInfo.xml to fail")
	}
	if _, err := ParseGalleryInfoTXT(bytes.NewReader(oversize)); err == nil {
		t.Fatalf("expected oversized galleryinfo.txt to fail")
	}
}

func TestCoverServiceCacheLimiterNegativeAndCircuitBreaker(t *testing.T) {
	t.Parallel()

	validPNG := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n', 0, 0, 0, 1}
	validJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 16, 'J', 'F', 'I', 'F'}
	validWebP := []byte{'R', 'I', 'F', 'F', 12, 0, 0, 0, 'W', 'E', 'B', 'P', 'V', 'P', '8', ' '}
	validGIF := []byte("GIF89a\x01\x00\x01\x00")

	for _, sample := range [][]byte{validPNG, validJPEG, validWebP, validGIF} {
		if _, _, ok := DetectImageFormat(sample); !ok {
			t.Fatalf("expected valid image format for %x", sample[:4])
		}
	}
	if _, _, ok := DetectImageFormat([]byte("<html>not an image</html>")); ok {
		t.Fatalf("expected HTML payload to fail image magic validation")
	}

	var mu sync.Mutex
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	var statusCode atomic.Int32
	statusCode.Store(http.StatusOK)
	var hits atomic.Int32
	var activeConns atomic.Int32
	var maxObservedConns atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/slow.png":
			cur := activeConns.Add(1)
			for {
				prev := maxObservedConns.Load()
				if cur <= prev || maxObservedConns.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(35 * time.Millisecond)
			activeConns.Add(-1)
			_, _ = w.Write(validPNG)
		case "/updated.png":
			_, _ = w.Write(validJPEG)
		case "/bad-magic":
			_, _ = w.Write([]byte("<html>blocked page</html>"))
		case "/huge.png":
			_, _ = w.Write(validPNG)
			_, _ = w.Write(bytes.Repeat([]byte{0}, 4096))
		case "/flaky.png":
			code := int(statusCode.Load())
			if code != http.StatusOK {
				w.WriteHeader(code)
				return
			}
			_, _ = w.Write(validPNG)
		}
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	client := newSafeHTTPClient("", 5*time.Second, safeClientOptions{allowLoopbackForTest: true})
	svc, err := NewCoverService(CoverConfig{
		DataDir:          dataDir,
		Client:           client,
		RequestsPerSec:   100,
		Burst:            10,
		MaxConcurrent:    2,
		MaxImageBytes:    1024,
		NegativeCacheTTL: 5 * time.Minute,
		Now:              nowFn,
	})
	if err != nil {
		t.Fatalf("NewCoverService: %v", err)
	}

	// Verify path traversal sanitization keeps cache files strictly inside thumbnails/extmeta.
	safePath, err := svc.CoverCachePath("../evil", "../../passwd", "png")
	if err != nil {
		t.Fatalf("CoverCachePath traversal check: %v", err)
	}
	if !strings.HasPrefix(safePath, filepath.Join(dataDir, "thumbnails", "extmeta")+string(filepath.Separator)) {
		t.Fatalf("cache path escaped extmeta root: %s", safePath)
	}

	// Verify concurrency semaphore <= 2.
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, gErr := svc.GetOrFetch(context.Background(), "src-conc", fmt.Sprintf("e-%d", idx), srv.URL+"/slow.png")
			if gErr != nil {
				t.Errorf("concurrent fetch %d failed: %v", idx, gErr)
			}
		}(i)
	}
	wg.Wait()
	if maxObservedConns.Load() > 2 {
		t.Fatalf("expected max concurrency <= 2, observed %d", maxObservedConns.Load())
	}

	// Verify disk cache hit skips network.
	hitsBefore := hits.Load()
	cached, err := svc.GetOrFetch(context.Background(), "src-conc", "e-0", srv.URL+"/slow.png")
	if err != nil || !cached.FromCache || hits.Load() != hitsBefore {
		t.Fatalf("expected disk cache hit without network call, got fromCache=%v err=%v", cached.FromCache, err)
	}
	newURL := srv.URL + "/updated.png"
	updated, err := svc.GetOrFetch(t.Context(), "src-conc", "e-0", newURL)
	if err != nil || updated.FromCache || !bytes.Equal(updated.Bytes, validJPEG) || updated.CachePath == cached.CachePath {
		t.Fatalf("changed cover URL: got %+v, err = %v", updated, err)
	}
	reused, err := svc.GetOrFetch(t.Context(), "src-conc", "e-0", " "+newURL+" ")
	if err != nil || !reused.FromCache || !bytes.Equal(reused.Bytes, validJPEG) || hits.Load() != hitsBefore+1 {
		t.Fatalf("changed cover URL cache: got %+v, err = %v, requests = %d", reused, err, hits.Load()-hitsBefore)
	}

	// Verify invalid magic & oversized payload rejection + 5m negative cache.
	if _, err := svc.GetOrFetch(context.Background(), "src-neg", "bad1", srv.URL+"/bad-magic"); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("expected ErrInvalidImage for HTML payload, got %v", err)
	}
	if _, err := svc.GetOrFetch(context.Background(), "src-neg", "bad1", srv.URL+"/bad-magic"); !errors.Is(err, ErrNegativeCached) {
		t.Fatalf("expected ErrNegativeCached on immediate retry, got %v", err)
	}
	if _, err := svc.GetOrFetch(context.Background(), "src-neg", "huge1", srv.URL+"/huge.png"); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("expected ErrInvalidImage for oversized payload, got %v", err)
	}

	// Verify 404 negative cache TTL expires after 5m.
	statusCode.Store(http.StatusNotFound)
	if _, err := svc.GetOrFetch(context.Background(), "src-404", "item1", srv.URL+"/flaky.png"); err == nil {
		t.Fatalf("expected 404 fetch error")
	}
	hits404 := hits.Load()
	if _, err := svc.GetOrFetch(context.Background(), "src-404", "item1", srv.URL+"/flaky.png"); !errors.Is(err, ErrNegativeCached) {
		t.Fatalf("expected ErrNegativeCached within 5m TTL, got %v", err)
	}
	if hits.Load() != hits404 {
		t.Fatalf("expected negative cache to prevent upstream hit")
	}
	advance(5*time.Minute + time.Second)
	statusCode.Store(http.StatusOK)
	recovered404, err := svc.GetOrFetch(context.Background(), "src-404", "item1", srv.URL+"/flaky.png")
	if err != nil || len(recovered404.Bytes) == 0 {
		t.Fatalf("expected recovery after 5m negative TTL, got err=%v", err)
	}

	// Verify circuit breaker exponential backoff: 30s -> 60s -> 180s -> 10m (capped), then recovery.
	tripCodes := []struct {
		code     int32
		cooldown time.Duration
	}{
		{http.StatusTooManyRequests, 30 * time.Second},
		{http.StatusServiceUnavailable, 60 * time.Second},
		{http.StatusForbidden, 180 * time.Second},
		{http.StatusTooManyRequests, 10 * time.Minute},
		{http.StatusTooManyRequests, 10 * time.Minute},
	}
	for i, step := range tripCodes {
		statusCode.Store(step.code)
		_, err := svc.GetOrFetch(context.Background(), "src-cb", "cb-item", srv.URL+"/flaky.png")
		if !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("trip %d: expected ErrCircuitOpen, got %v", i+1, err)
		}
		hitsDuringOpen := hits.Load()
		advance(step.cooldown - time.Second)
		if _, err := svc.GetOrFetch(context.Background(), "src-cb", "cb-item", srv.URL+"/flaky.png"); !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("trip %d before cooldown expiry: expected ErrCircuitOpen, got %v", i+1, err)
		}
		if hits.Load() != hitsDuringOpen {
			t.Fatalf("trip %d: circuit open should not hit upstream", i+1)
		}
		advance(time.Second)
	}

	statusCode.Store(http.StatusOK)
	res, err := svc.GetOrFetch(context.Background(), "src-cb", "cb-item", srv.URL+"/flaky.png")
	if err != nil || res.Extension != "png" {
		t.Fatalf("expected circuit breaker recovery on 200 OK, got err=%v", err)
	}
}

func TestManagerCoverChecksSidecarEvenWhenSeriesNSFWIsLockedOff(t *testing.T) {
	ctx := t.Context()
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	var requests int
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		return managerResponse(r, http.StatusOK, bytes.NewReader(png)), nil
	})}
	store, m, root := newTestManager(t, ManagerOptions{})
	covers, err := NewCoverService(CoverConfig{DataDir: root, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	m.covers = covers
	seedMetadataLibrary(t, store, "main", true)
	seedMetadataSeries(t, store, "series", "main", "Blue River")
	upstream := filepath.Join(root, "upstream.sqlite")
	createManagerGallery(t, upstream, "Blue River")
	src := addManagerSource(t, m, database.MetadataSource{Name: "Catalog", Kind: KindOfflineSQLite, LocalPath: upstream})
	syncManagerFixture(t, m, src, false, "success")

	const coverURL = "https://cover.example.test/cover.png"
	if err := store.SaveSeriesMetadata(ctx, database.SeriesMetadata{
		SeriesID: "series", ExtSourceID: src.ID, ExtEntryID: "102", ExtCoverURL: coverURL, NSFWLock: true,
	}); err != nil {
		t.Fatal(err)
	}
	meta := readMetadata(t, store, "series")
	if meta.IsNSFW || !meta.NSFWLock {
		t.Fatalf("series fixture did not lock off the NSFW flag: %+v", meta)
	}
	allowed := database.WithUserScope(ctx, database.UserScope{UserID: "allowed", AllowNSFW: true, ShowNSFW: true})
	restricted := database.WithUserScope(ctx, database.UserScope{UserID: "restricted"})
	raw, mediaType, err := m.FetchCover(allowed, meta.ExtSourceID, meta.ExtEntryID, meta.ExtCoverURL)
	if err != nil || mediaType != "image/png" || !bytes.Equal(raw, png) {
		t.Fatalf("allowed cover: media type = %q, err = %v", mediaType, err)
	}
	for _, entryID := range []string{meta.ExtEntryID, "999"} {
		raw, mediaType, err := m.FetchCover(restricted, meta.ExtSourceID, entryID, meta.ExtCoverURL)
		if !errors.Is(err, ErrUnavailable) || raw != nil || mediaType != "" || requests != 1 {
			t.Fatalf("restricted cover %q: bytes = %x, media type = %q, requests = %d, err = %v", entryID, raw, mediaType, requests, err)
		}
	}
	if _, _, err := m.FetchCover(restricted, src.ID, "101", coverURL); err != nil || requests != 2 {
		t.Fatalf("safe cover: requests = %d, err = %v", requests, err)
	}
}

func TestForceScrapePreservesProtectedBindings(t *testing.T) {
	ctx := t.Context()
	store, m, root := newTestManager(t, ManagerOptions{})
	bgm, err := bangumi.NewService(store, filepath.Join(root, "bangumi"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		m.Close()
		bgm.Close()
	})
	var contents bytes.Buffer
	archive := zip.NewWriter(&contents)
	subjects, err := archive.Create("subject.jsonlines")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(subjects, `{"id":100,"type":1,"platform":1001,"name":"Blue River","series":true}`); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	dumpPath := filepath.Join(root, "fixture.zip")
	if err := os.WriteFile(dumpPath, contents.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bgm.ImportArchiveZip(ctx, dumpPath, "fixture.zip", "2026-11-25T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	seedMetadataLibrary(t, store, "main", true)
	upstream := filepath.Join(root, "upstream.sqlite")
	createManagerGallery(t, upstream, "Blue River")
	src := addManagerSource(t, m, database.MetadataSource{Name: "Catalog", Kind: KindOfflineSQLite, LocalPath: upstream})
	syncManagerFixture(t, m, src, false, "success")
	if _, err := m.SetLibrarySources(ctx, "main", []string{src.ID}); err != nil {
		t.Fatal(err)
	}
	legacyID := int64(42)
	protected := []database.SeriesMetadata{
		{SeriesID: "legacy-bangumi", BgmSubjectID: &legacyID},
		{SeriesID: "legacy-external", ExtSourceID: src.ID, ExtEntryID: "102"},
		{SeriesID: "manual", BgmSubjectID: &legacyID, MatchOrigin: "manual"},
		{SeriesID: "agent", ExtSourceID: src.ID, ExtEntryID: "102", MatchOrigin: "agent"},
	}
	assertProtected := func() {
		t.Helper()
		for _, want := range protected {
			got := readMetadata(t, store, want.SeriesID)
			if got.ExtSourceID != want.ExtSourceID || got.ExtEntryID != want.ExtEntryID || got.MatchOrigin != want.MatchOrigin ||
				got.ScrapeStatus != database.ScrapeStatusMatched || (want.BgmSubjectID != nil && (got.BgmSubjectID == nil || *got.BgmSubjectID != legacyID)) {
				t.Fatalf("protected binding changed: %+v", got)
			}
		}
	}
	for _, meta := range append(slices.Clone(protected), database.SeriesMetadata{SeriesID: "automatic", BgmSubjectID: &legacyID, MatchOrigin: "auto"}) {
		seedMetadataSeries(t, store, meta.SeriesID, "main", "Blue River")
		meta.ScrapeStatus = database.ScrapeStatusMatched
		if err := store.SaveSeriesMetadata(ctx, meta); err != nil {
			t.Fatal(err)
		}
	}

	extRun, err := m.StartMultiScrape(ctx, "main", bangumi.ScrapeModeForce, false)
	if err != nil {
		t.Fatal(err)
	}
	waitForManager(t, func() bool { return m.ScrapeRuns()[0].CompletedAt != "" })
	if done := m.ScrapeRuns()[0]; done.ID != extRun.ID || done.Status != "success" || done.TotalSeries != 1 ||
		done.Matched != 1 || done.LibraryName != "main" || done.CurrentSeries != "Blue River" {
		t.Fatalf("external force scrape = %+v", done)
	}
	assertProtected()
	if meta := readMetadata(t, store, "automatic"); meta.ExtSourceID != src.ID || meta.ExtEntryID != "101" || meta.MatchOrigin != "auto" {
		t.Fatalf("external force scrape did not update automatic binding: %+v", meta)
	}
	bgmRun, err := bgm.StartBatchWithMode(ctx, "main", bangumi.ScrapeModeForce)
	if err != nil {
		t.Fatal(err)
	}
	waitForManager(t, func() bool {
		bgmRun, err = bgm.Run(ctx, bgmRun.ID)
		if err != nil {
			t.Fatal(err)
		}
		return bgmRun.CompletedAt != nil
	})
	if bgmRun.Status != "success" || bgmRun.SkippedCount != len(protected) || bgmRun.MatchedCount != 1 || bgmRun.FailedCount != 0 {
		t.Fatalf("Bangumi force scrape = %+v", bgmRun)
	}
	assertProtected()
	if meta := readMetadata(t, store, "automatic"); meta.BgmSubjectID == nil || *meta.BgmSubjectID != 100 || meta.MatchOrigin != "auto" {
		t.Fatalf("Bangumi force scrape did not update automatic binding: %+v", meta)
	}
	cleared, err := m.ClearSeriesExternal(ctx, "automatic")
	if err != nil || cleared.BgmSubjectID == nil || *cleared.BgmSubjectID != 100 || cleared.ScrapeStatus != database.ScrapeStatusMatched {
		t.Fatalf("external unbind changed Bangumi metadata: %+v, err = %v", cleared, err)
	}
}

func TestManagerCandidateSourceAndTagDisplay(t *testing.T) {
	ctx := t.Context()
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		return managerResponse(r, http.StatusOK, strings.NewReader(
			`{"data":[{"namespace":"topic","data":{"glasses":{"name":"眼镜"}}}]}`)), nil
	})}
	store, m, root := newTestManager(t, ManagerOptions{HTTPClient: client})
	seedMetadataLibrary(t, store, "main", true)
	seedMetadataSeries(t, store, "series", "main", "Blue River")
	upstream := filepath.Join(root, "upstream.sqlite")
	createManagerGallery(t, upstream, "Blue River")
	dict := addManagerSource(t, m, database.MetadataSource{Name: "Dictionary", Kind: KindTagDictionary, DownloadURL: "https://example.test/tags.json"})
	src := addManagerSource(t, m, database.MetadataSource{Name: "Catalog", Kind: KindOfflineSQLite, LocalPath: upstream, TagDictSourceID: dict.ID})
	syncManagerFixture(t, m, dict, false, "success")
	syncManagerFixture(t, m, src, false, "success")
	if run := m.Syncs()[0]; run.Phase != "syncing" || run.Indexed != 2 || run.IndexedEntries != run.Indexed {
		t.Fatalf("sync progress = %+v", run)
	}
	for _, tc := range []struct{ query, mode, tag string }{
		{"101", "zh", "眼镜"},
		{"Blue River", "bilingual", "眼镜 (topic:glasses)"},
		{"101", "raw", "topic:glasses"},
	} {
		if _, err := m.UpdateSourceConfig(ctx, src.ID, SourceConfig{TagDisplayMode: &tc.mode}); err != nil {
			t.Fatal(err)
		}
		found, err := m.SearchCandidates(ctx, "series", src.ID, tc.query)
		if err != nil || found.SourceID != src.ID {
			t.Fatalf("candidate search = %+v, err = %v", found, err)
		}
		hits := found.Candidates.([]*Entry)
		if len(hits) != 1 || hits[0].SourceID != src.ID || !slices.Equal(hits[0].Tags, []string{tc.tag}) {
			t.Fatalf("candidates for %s = %+v", tc.mode, hits)
		}
	}
}

func TestManagerOnlineEnrichReportsOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		body    string
		matched int
		failed  int
	}{
		{"matched", http.StatusOK, `{"entryId":"1","title":"Blue River"}`, 1, 0},
		{"unmatched", http.StatusNoContent, "", 0, 0},
		{"failed", http.StatusBadGateway, "", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
				return managerResponse(r, tc.code, strings.NewReader(tc.body)), nil
			})}
			store, m, _ := newTestManager(t, ManagerOptions{HTTPClient: client})
			seedMetadataLibrary(t, store, "main", true)
			seedMetadataSeries(t, store, "series", "main", "Blue River")
			addManagerSource(t, m, database.MetadataSource{Name: "Online", Kind: KindOfflineSQLite, OnlineEnabled: true, OnlineAPIURL: "https://source.example.test/suggest"})
			run, err := m.StartMultiScrape(t.Context(), "main", "force", true)
			if err != nil {
				t.Fatal(err)
			}
			waitForManager(t, func() bool { return m.ScrapeRuns()[0].CompletedAt != "" })
			status := m.OnlineEnrichStatus()
			if status.RunID != run.ID || status.Running || status.CircuitOpen || status.Processed != 1 ||
				status.Matched != tc.matched || status.Failed != tc.failed || (status.LastError != "") != (tc.failed != 0) {
				t.Fatalf("online status = %+v", status)
			}
		})
	}
}

func TestSidecarSupportsMangaAlphaMetaV2AliasAndExtendedColumns(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "open_v2.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO meta VALUES ('schema', 'mangaalpha_meta_v2');
CREATE TABLE entries (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  title_alt TEXT DEFAULT '',
  authors TEXT DEFAULT '[]',
  summary TEXT DEFAULT '',
  tags TEXT DEFAULT '[]',
  category TEXT DEFAULT '',
  cover_url TEXT DEFAULT '',
  rating REAL DEFAULT 0,
  is_nsfw INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER DEFAULT 0,
  extra_json TEXT DEFAULT '{}',
  status TEXT DEFAULT '',
  publisher TEXT DEFAULT '',
  reading_direction TEXT DEFAULT '',
  age_rating INTEGER
);
INSERT INTO entries VALUES (
  'v2-1', '鋼の錬金術師', 'Fullmetal Alchemist', '["荒川弘"]', 'Equivalent exchange',
  '["奇幻"]', 'Manga', '', 9.7, 0, 1700002000, '{}', 'ENDED', 'Square Enix', 'RIGHT_TO_LEFT', 13
);
`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	sidecarPath := filepath.Join(tmpDir, "open_v2.sidecar.sqlite")
	if err := BuildSidecarIndex(ctx, dbPath, sidecarPath, FormatOpenV2, nil); err != nil {
		t.Fatalf("BuildSidecarIndex(mangaalpha_meta_v2): %v", err)
	}
	store, err := OpenSidecarStore("open-v2-src", dbPath, sidecarPath, FormatOpenV2)
	if err != nil {
		t.Fatalf("OpenSidecarStore(mangaalpha_meta_v2): %v", err)
	}
	defer store.Close()

	hits, err := store.SearchByTitle(ctx, "钢の炼金术师", 5)
	if err != nil || len(hits) != 1 || hits[0].EntryID != "v2-1" {
		t.Fatalf("unexpected v2 sidecar search hits: %+v err=%v", hits, err)
	}
	entry, err := store.GetEntry(ctx, "v2-1")
	if err != nil {
		t.Fatalf("GetEntry(v2-1): %v", err)
	}
	if entry.Status != "ENDED" || entry.Publisher != "Square Enix" || entry.ReadingDirection != "RIGHT_TO_LEFT" ||
		entry.AgeRating == nil || *entry.AgeRating != 13 {
		t.Fatalf("unexpected v2 entry extended fields: %+v", entry)
	}
}
