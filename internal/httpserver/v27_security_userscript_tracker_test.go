package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/archive"
	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/cache"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/maintenance"
	"github.com/xJogger/fake-komga-115/internal/oneonefive"
	"github.com/xJogger/fake-komga-115/internal/scanner"
	"github.com/xJogger/fake-komga-115/internal/thumbnail"
	"github.com/xJogger/fake-komga-115/internal/webdav"
)

func setupV27TestServer(t *testing.T, trustedProxies string) (*Server, *database.Store, *bangumi.Service, *httptest.Server) {
	t.Helper()
	srv, store, bgmSvc := newV27TestServer(t, trustedProxies)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, store, bgmSvc, ts
}

func newV27TestServer(t *testing.T, trustedProxies string) (*Server, *database.Store, *bangumi.Service) {
	t.Helper()
	tempDir := t.TempDir()
	store, err := database.Open(filepath.Join(tempDir, "v27.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := oneonefive.New(store, logger)
	webdavMgr := webdav.NewManager()
	scanManager := scanner.NewWithWebDAV(store, client, webdavMgr, logger)
	t.Cleanup(scanManager.Close)
	cacheManager, err := cache.New(store, filepath.Join(tempDir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	bgmSvc, err := bangumi.NewService(store, filepath.Join(tempDir, "bangumi"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bgmSvc.Close() })
	thumbs, err := thumbnail.New(store, filepath.Join(tempDir, "thumbs"), logger)
	if err != nil {
		t.Fatal(err)
	}
	archSvc := archive.NewServiceWithWebDAV(store, client, webdavMgr, cacheManager, logger)
	t.Cleanup(archSvc.Close)
	coverMgr := thumbnail.NewBatchManager(store, archSvc, thumbs, logger)
	t.Cleanup(coverMgr.Close)
	maintMgr := maintenance.New(store, archSvc, thumbs, logger)
	t.Cleanup(maintMgr.Close)

	srv := New(
		store, client, scanManager, cacheManager, archSvc, thumbs, coverMgr, maintMgr,
		RuntimeInfo{
			DataDir:        tempDir,
			Host:           "127.0.0.1",
			Port:           25600,
			TrustedProxies: trustedProxies,
		}, logger,
	).WithServices(webdavMgr, bgmSvc)

	return srv, store, bgmSvc
}

func TestV27PublicSecurityHardeningAndLANCompatibility(t *testing.T) {
	_, store, _, ts := setupV27TestServer(t, "")
	ctx := context.Background()

	if _, err := store.ClaimAdmin(ctx, "owner", "SuperStrongPass123!"); err != nil {
		t.Fatal(err)
	}

	// Seed a visible series + book to verify /reader auth oracle is closed.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = store.DB().ExecContext(ctx, `INSERT INTO libraries(id,name,provider_type,root_cid,enabled,created_at,updated_at) VALUES('l1','主库','local','local:/tmp',1,?,?)`, now, now)
	_, _ = store.DB().ExecContext(ctx, `INSERT INTO series(id,library_id,cid,name,relative_path,books_count,created_at,updated_at,seen_scan_id) VALUES('s1','l1','c1','Test','Test',1,?,?,'sc')`, now, now)
	_, _ = store.DB().ExecContext(ctx, `INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,created_at,updated_at,seen_scan_id) VALUES('b1','s1','l1','f1','c1','01.cbz',100,'p',1,?,?,'sc')`, now, now)

	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// 1. Global security headers present; NO HSTS on HTTP (LAN HTTP safe)
	resp, err := noRedirect.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		resp.Header.Get("X-Frame-Options") != "SAMEORIGIN" ||
		resp.Header.Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("missing security headers: %+v", resp.Header)
	}
	if resp.Header.Get("Strict-Transport-Security") != "" {
		t.Fatalf("HSTS must NOT be forced by Go server (breaks plain LAN HTTP): %q", resp.Header.Get("Strict-Transport-Security"))
	}

	// 2. /reader/* auth bypass oracle closed: /reader/book/b1/x.png and /reader/series/s1/x.png require login
	for _, probe := range []string{"/reader/book/b1/x.png", "/reader/series/s1/x.png", "/reader/series/s1/book/b1/evil.svg"} {
		r, err := noRedirect.Get(ts.URL + probe)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusTemporaryRedirect || !strings.HasPrefix(r.Header.Get("Location"), "/login?redirect=") {
			t.Fatalf("probe %s bypassed auth: status=%d loc=%q", probe, r.StatusCode, r.Header.Get("Location"))
		}
	}
	// Known root static file & assets under /reader/ remain public
	for _, pub := range []string{"/reader/site.webmanifest", "/reader/favicon.ico"} {
		if !isStaticReaderAssetPath(pub) {
			t.Fatalf("expected %s to remain allowed static reader path", pub)
		}
	}

	// 3. Open redirect backslash & control-char sanitization
	for _, bad := range []string{"/\\evil.com", "//evil.com", "https://evil.com", "/foo\r\nbar", "/login?x=1"} {
		if got := sanitizeRedirectTarget(bad); got != "/" {
			t.Fatalf("sanitizeRedirectTarget(%q) = %q, want /", bad, got)
		}
	}
	if got := sanitizeRedirectTarget("/series/s1?versions=1"); got != "/series/s1?versions=1" {
		t.Fatalf("valid relative redirect rejected: %q", got)
	}

	// 4. Production Bangumi cover SSRF protection: loopback blocked by default
	if bangumi.IsAllowedBangumiCoverURL("http://127.0.0.1:8080/internal") ||
		bangumi.IsAllowedBangumiCoverURL("http://localhost:25600/health") {
		t.Fatal("loopback cover URLs must be rejected by default in production")
	}

	// 5. Login with session cookie (plain LAN HTTP -> Secure=false)
	jar, _ := cookiejar.New(nil)
	sessionClient := &http.Client{Jar: jar}
	loginResp := requestJSONWithClient(t, sessionClient, http.MethodPost, ts.URL+"/admin/api/auth/login", map[string]any{
		"username": "owner",
		"password": "SuperStrongPass123!",
	})
	var sessionSecure bool
	for _, ck := range loginResp.Cookies() {
		if ck.Name == sessionCookieName {
			sessionSecure = ck.Secure
		}
	}
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d", loginResp.StatusCode)
	}
	if sessionSecure {
		t.Fatal("plain HTTP login must NOT set Secure cookie flag (would break LAN HTTP)")
	}

	// Trusted proxy sending X-Forwarded-Proto: https -> Secure=true
	httpsReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/auth/login",
		bytes.NewBufferString(`{"username":"owner","password":"SuperStrongPass123!"}`))
	httpsReq.Header.Set("Content-Type", "application/json")
	httpsReq.Header.Set("X-Forwarded-Proto", "https")
	httpsResp, err := noRedirect.Do(httpsReq)
	if err != nil {
		t.Fatal(err)
	}
	var forwardedSecure bool
	for _, ck := range httpsResp.Cookies() {
		if ck.Name == sessionCookieName {
			forwardedSecure = ck.Secure
		}
	}
	httpsResp.Body.Close()
	if !forwardedSecure {
		t.Fatal("trusted proxy with X-Forwarded-Proto: https must set Secure cookie flag")
	}

	// 6. Cookie CSRF guard: blocks cross-origin & cross-site, allows LAN HTTP same-origin & native cookie client
	// 6a. Cross-origin Origin -> 403
	csrfReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/auth/api-keys", bytes.NewBufferString(`{"name":"evil"}`))
	csrfReq.Header.Set("Content-Type", "application/json")
	csrfReq.Header.Set("Origin", "https://evil.example.com")
	csrfResp, err := sessionClient.Do(csrfReq)
	if err != nil {
		t.Fatal(err)
	}
	csrfResp.Body.Close()
	if csrfResp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin cookie POST must be 403, got %d", csrfResp.StatusCode)
	}

	// 6b. Sec-Fetch-Site: cross-site -> 403
	csrfReq2, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/auth/api-keys", bytes.NewBufferString(`{"name":"evil2"}`))
	csrfReq2.Header.Set("Content-Type", "application/json")
	csrfReq2.Header.Set("Sec-Fetch-Site", "cross-site")
	csrfResp2, err := sessionClient.Do(csrfReq2)
	if err != nil {
		t.Fatal(err)
	}
	csrfResp2.Body.Close()
	if csrfResp2.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site Sec-Fetch-Site cookie POST must be 403, got %d", csrfResp2.StatusCode)
	}

	// 6c. Plain LAN HTTP same-origin (matching Origin, NO Sec-Fetch-Site) -> 201 OK
	lanReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/auth/api-keys", bytes.NewBufferString(`{"name":"lan-ok"}`))
	lanReq.Header.Set("Content-Type", "application/json")
	lanReq.Header.Set("Origin", ts.URL)
	lanResp, err := sessionClient.Do(lanReq)
	if err != nil {
		t.Fatal(err)
	}
	lanResp.Body.Close()
	if lanResp.StatusCode != http.StatusCreated {
		t.Fatalf("same-origin LAN HTTP POST without Sec-Fetch-Site must succeed, got %d", lanResp.StatusCode)
	}

	// 6d. Native Komga client with cookie and NO Origin/Sec-Fetch-Site/Referer -> 200 OK
	nativeReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/series/list", bytes.NewBufferString(`{}`))
	nativeReq.Header.Set("Content-Type", "application/json")
	nativeResp, err := sessionClient.Do(nativeReq)
	if err != nil {
		t.Fatal(err)
	}
	nativeResp.Body.Close()
	if nativeResp.StatusCode != http.StatusOK {
		t.Fatalf("native client cookie POST without Origin must succeed, got %d", nativeResp.StatusCode)
	}

	// 7. WebDAV test endpoint must NOT leak saved password to a different host
	savedWD, err := store.UpsertWebDAVAccount(ctx, database.WebDAVAccountSecret{
		WebDAVAccount: database.WebDAVAccount{
			Name:     "MyNAS",
			BaseURL:  "http://127.0.0.1:19999/dav",
			Username: "nasuser",
		},
		Password: "UltraSecretWebDAVPassword",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	var attackerHits atomic.Int32
	attackerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer attackerSrv.Close()

	exfilResp := requestJSONWithClient(t, sessionClient, http.MethodPost, ts.URL+"/admin/api/webdav-accounts/test", map[string]any{
		"id":       savedWD.ID,
		"baseUrl":  attackerSrv.URL,
		"username": "nasuser",
		"password": "",
	})
	exfilResp.Body.Close()
	if exfilResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("WebDAV test changing host with empty password must fail 400, got %d", exfilResp.StatusCode)
	}
	if attackerHits.Load() != 0 {
		t.Fatalf("attacker WebDAV host must receive 0 requests, got %d", attackerHits.Load())
	}

	// 8. Brute-force login rate limiting (10 failures -> 11th returns 429 + Retry-After)
	for i := 0; i < authFailThreshold; i++ {
		badReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/auth/login",
			bytes.NewBufferString(`{"username":"owner","password":"wrong-password"}`))
		badReq.Header.Set("Content-Type", "application/json")
		badReq.Header.Set("X-Forwarded-For", "198.51.100.77")
		badResp, err := noRedirect.Do(badReq)
		if err != nil {
			t.Fatal(err)
		}
		badResp.Body.Close()
		if badResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d expected 401, got %d", i+1, badResp.StatusCode)
		}
	}
	blockedReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/auth/login",
		bytes.NewBufferString(`{"username":"owner","password":"SuperStrongPass123!"}`))
	blockedReq.Header.Set("Content-Type", "application/json")
	blockedReq.Header.Set("X-Forwarded-For", "198.51.100.77")
	blockedResp, err := noRedirect.Do(blockedReq)
	if err != nil {
		t.Fatal(err)
	}
	blockedResp.Body.Close()
	if blockedResp.StatusCode != http.StatusTooManyRequests || blockedResp.Header.Get("Retry-After") == "" {
		t.Fatalf("expected 429 + Retry-After after %d failures, got %d Retry-After=%q",
			authFailThreshold, blockedResp.StatusCode, blockedResp.Header.Get("Retry-After"))
	}
}

func TestV27TrustedProxiesDisabledIgnoresSpoofedHeaders(t *testing.T) {
	_, store, _, ts := setupV27TestServer(t, "none")
	if _, err := store.ClaimAdmin(context.Background(), "owner", "SuperStrongPass123!"); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/auth/login",
		bytes.NewBufferString(`{"username":"owner","password":"SuperStrongPass123!"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var gotSecure bool
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookieName {
			gotSecure = ck.Secure
		}
	}
	resp.Body.Close()
	if gotSecure {
		t.Fatal("when FK115_TRUSTED_PROXIES=none, spoofed X-Forwarded-Proto must be ignored")
	}
}

func TestV27UserscriptKeylessLatestAndPairKeyRotation(t *testing.T) {
	_, store, _, ts := setupV27TestServer(t, "")
	ctx := context.Background()
	if _, err := store.ClaimAdmin(ctx, "owner", "SuperStrongPass123!"); err != nil {
		t.Fatal(err)
	}

	jar, _ := cookiejar.New(nil)
	sessionClient := &http.Client{Jar: jar}
	loginResp := requestJSONWithClient(t, sessionClient, http.MethodPost, ts.URL+"/admin/api/auth/login", map[string]any{
		"username": "owner",
		"password": "SuperStrongPass123!",
	})
	loginResp.Body.Close()

	// 1. Public permanent keyless endpoint /bgm/userscript/latest.user.js works WITHOUT auth cookie and never expires!
	for i := 0; i < 3; i++ {
		r, err := http.Get(ts.URL + userscriptLatestPath)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("latest.user.js read %d expected 200, got %d", i+1, r.StatusCode)
		}
		body := string(raw)
		if r.Header.Get("Cache-Control") != "no-cache" || !strings.Contains(r.Header.Get("Content-Disposition"), "inline") {
			t.Fatalf("unexpected headers: Cache-Control=%q Content-Disposition=%q",
				r.Header.Get("Cache-Control"), r.Header.Get("Content-Disposition"))
		}
		for _, want := range []string{
			"// @connect      127.0.0.1",
			"// @match        *://127.0.0.1/bgm/userscript/pair*",
			"// @updateURL    " + ts.URL + userscriptLatestPath,
			"// @downloadURL  " + ts.URL + userscriptLatestPath,
			"#panelInterestWrapper",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("latest.user.js missing %q", want)
			}
		}
		if strings.Contains(body, "DEFAULT_API_KEY") || strings.Contains(body, "fk115_") {
			t.Fatalf("public latest.user.js must be strictly keyless")
		}
	}

	// 2. /bgm/userscript/pair requires admin session (unauthenticated redirects to /login)
	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	unauthPair, err := noRedirect.Get(ts.URL + userscriptPairPath)
	if err != nil {
		t.Fatal(err)
	}
	unauthPair.Body.Close()
	if unauthPair.StatusCode != http.StatusTemporaryRedirect || !strings.Contains(unauthPair.Header.Get("Location"), "/login") {
		t.Fatalf("unauthenticated /bgm/userscript/pair expected redirect to /login, got %d loc=%q",
			unauthPair.StatusCode, unauthPair.Header.Get("Location"))
	}

	// Authenticated admin receives pairing page
	authPair, err := sessionClient.Get(ts.URL + userscriptPairPath)
	if err != nil {
		t.Fatal(err)
	}
	authPairBytes, _ := io.ReadAll(authPair.Body)
	authPair.Body.Close()
	if authPair.StatusCode != http.StatusOK || !strings.Contains(string(authPairBytes), "ma-pair-status") {
		t.Fatalf("authenticated pair page unexpected status=%d", authPair.StatusCode)
	}

	// 3. First pair-key generates a working key; second pair-key rotates and revokes key1
	probeKey := func(k string) int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/bangumi/lookup?subject_id=1", nil)
		req.Header.Set("X-API-Key", k)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	p1Resp := requestJSONWithClient(t, sessionClient, http.MethodPost, ts.URL+"/admin/api/bangumi/userscript/pair-key", nil)
	var p1 struct {
		Key            string `json:"key"`
		KeyPrefix      string `json:"keyPrefix"`
		RotatedOldKeys int64  `json:"rotatedOldKeys"`
		ConnectHost    string `json:"connectHost"`
	}
	if err := json.NewDecoder(p1Resp.Body).Decode(&p1); err != nil {
		p1Resp.Body.Close()
		t.Fatal(err)
	}
	p1Resp.Body.Close()
	if p1Resp.StatusCode != http.StatusCreated || !strings.HasPrefix(p1.Key, "fk115_") || p1.RotatedOldKeys != 0 || p1.ConnectHost != "127.0.0.1" {
		t.Fatalf("unexpected pair-key 1 response: status=%d body=%+v", p1Resp.StatusCode, p1)
	}
	if code := probeKey(p1.Key); code != http.StatusOK {
		t.Fatalf("key1 expected 200, got %d", code)
	}

	p2Resp := requestJSONWithClient(t, sessionClient, http.MethodPost, ts.URL+"/admin/api/bangumi/userscript/pair-key", nil)
	var p2 struct {
		Key            string `json:"key"`
		RotatedOldKeys int64  `json:"rotatedOldKeys"`
	}
	if err := json.NewDecoder(p2Resp.Body).Decode(&p2); err != nil {
		p2Resp.Body.Close()
		t.Fatal(err)
	}
	p2Resp.Body.Close()
	if p2Resp.StatusCode != http.StatusCreated || p2.RotatedOldKeys != 1 || p2.Key == "" || p2.Key == p1.Key {
		t.Fatalf("unexpected pair-key 2 response: status=%d body=%+v", p2Resp.StatusCode, p2)
	}
	if code := probeKey(p1.Key); code != http.StatusUnauthorized {
		t.Fatalf("revoked key1 must return 401, got %d", code)
	}
	if code := probeKey(p2.Key); code != http.StatusOK {
		t.Fatalf("new key2 must return 200, got %d", code)
	}
}

func TestV27BangumiTrackerHTTPIntegration(t *testing.T) {
	_, store, bgmSvc, ts := setupV27TestServer(t, "")
	ctx := context.Background()
	if _, err := store.ClaimAdmin(ctx, "owner", "SuperStrongPass123!"); err != nil {
		t.Fatal(err)
	}

	const pat = "pat-e2e-secret"
	var pushed atomic.Int32
	mockBgm := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+pat {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/me":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"username":"reader1","nickname":"书虫"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v0/users/reader1/collections/54321":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v0/users/-/collections/54321":
			pushed.Add(1)
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	})
	bgmSvc.SetAPITransportForTest(bangumiTestTransport(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		mockBgm.ServeHTTP(rec, r)
		return rec.Result(), nil
	}))
	bgmSvc.SetTrackerDebounceForTest(25 * time.Millisecond)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = store.DB().ExecContext(ctx, `INSERT INTO libraries(id,name,provider_type,root_cid,enabled,created_at,updated_at) VALUES('l1','主库','local','local:/tmp',1,?,?)`, now, now)
	_, _ = store.DB().ExecContext(ctx, `INSERT INTO series(id,library_id,cid,name,relative_path,books_count,created_at,updated_at,seen_scan_id) VALUES('s1','l1','c1','Test','Test',2,?,?,'sc')`, now, now)
	_, _ = store.DB().ExecContext(ctx, `INSERT INTO series_metadata(series_id,bgm_subject_id,scrape_status,status,title,total_book_count,is_nsfw,updated_at) VALUES('s1',54321,'matched','ENDED','测试漫画',2,0,?)`, now)
	_, _ = store.DB().ExecContext(ctx, `INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,created_at,updated_at,seen_scan_id) VALUES('b1','s1','l1','f1','c1','01.cbz',100,'p',1,?,?,'sc')`, now, now)
	_, _ = store.DB().ExecContext(ctx, `INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,created_at,updated_at,seen_scan_id) VALUES('b2','s1','l1','f2','c1','02.cbz',100,'p',2,?,?,'sc')`, now, now)

	jar, _ := cookiejar.New(nil)
	sessionClient := &http.Client{Jar: jar}
	loginResp := requestJSONWithClient(t, sessionClient, http.MethodPost, ts.URL+"/admin/api/auth/login", map[string]any{
		"username": "owner",
		"password": "SuperStrongPass123!",
	})
	loginResp.Body.Close()

	cfgResp := requestJSONWithClient(t, sessionClient, http.MethodPut, ts.URL+"/admin/api/bangumi/tracker/config", map[string]any{
		"enabled":     true,
		"accessToken": pat,
		"nsfwMode":    "skip",
	})
	var cfgBody map[string]any
	decodeResponseJSON(t, cfgResp, &cfgBody)
	if cfgResp.StatusCode != http.StatusOK || cfgBody["username"] != "reader1" {
		t.Fatalf("tracker config save failed: status=%d body=%#v", cfgResp.StatusCode, cfgBody)
	}

	// Ensure /admin/api/settings NEVER exposes bangumi_tracker_token
	var settingsBody map[string]string
	getJSONWithClient(t, sessionClient, ts.URL+"/admin/api/settings", &settingsBody)
	if _, leaked := settingsBody[bangumi.SettingTrackerToken]; leaked {
		t.Fatal("/admin/api/settings leaked bangumi_tracker_token")
	}

	// Mark book 1 read via standard Komga endpoint -> triggers async tracker push
	progResp := requestJSONWithClient(t, sessionClient, http.MethodPatch, ts.URL+"/api/v1/books/b1/read-progress", map[string]any{
		"completed": true,
	})
	progResp.Body.Close()
	if progResp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch progress status=%d", progResp.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pushed.Load() >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if pushed.Load() == 0 {
		t.Fatal("expected Komga progress update to trigger Bangumi tracker POST")
	}

	// Verify series detail exposes read-only tracker badge summary
	var detail struct {
		Tracker *bangumi.SeriesTrackerSummary `json:"tracker"`
	}
	getJSONWithClient(t, sessionClient, ts.URL+"/admin/api/series/s1/detail", &detail)
	if detail.Tracker == nil || detail.Tracker.Status != "synced" || detail.Tracker.RemoteVolStatus != 1 {
		t.Fatalf("expected synced tracker summary in series detail, got %+v", detail.Tracker)
	}
}
