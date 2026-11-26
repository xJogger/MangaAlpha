package httpserver

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/archive"
	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/buildinfo"
	"github.com/xJogger/fake-komga-115/internal/cache"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/maintenance"
	"github.com/xJogger/fake-komga-115/internal/oneonefive"
	"github.com/xJogger/fake-komga-115/internal/scanner"
	"github.com/xJogger/fake-komga-115/internal/thumbnail"
	"github.com/xJogger/fake-komga-115/internal/webdav"
)

func writePlainCBZForV30Test(t *testing.T, dest string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dest, err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("001.png")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	_, _ = w.Write(makeTestPNGBytes(t, 40, 120, 210))
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	if err := os.WriteFile(dest, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", dest, err)
	}
}

func TestV30MultiUserRBACIsolationAndCodename(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := database.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	defer store.Close()

	// Create local directories with normal CBZ and encrypted CBZ
	lib1Dir := filepath.Join(tmpDir, "lib1")
	lib2Dir := filepath.Join(tmpDir, "lib2")
	encPath := filepath.Join(lib1Dir, "SeriesOne", "Vol01.cbz")
	nsfwPath := filepath.Join(lib1Dir, "SeriesNSFW", "N01.cbz")
	secPath := filepath.Join(lib2Dir, "SecretSeries", "S01.cbz")
	writeEncryptedCBZForHTTPTest(t, encPath, "open-sesame", 2, true)
	writePlainCBZForV30Test(t, nsfwPath)
	writePlainCBZForV30Test(t, secPath)

	cacheMgr, err := cache.New(store, filepath.Join(tmpDir, "cache"))
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	client := oneonefive.New(store, logger)
	wdMgr := webdav.NewManager()
	scanMgr := scanner.NewWithWebDAV(store, client, wdMgr, logger)
	defer scanMgr.Close()
	archSvc := archive.NewServiceWithWebDAV(store, client, wdMgr, cacheMgr, logger)
	defer archSvc.Close()
	thumbSvc, err := thumbnail.New(store, filepath.Join(tmpDir, "thumbs"), logger)
	if err != nil {
		t.Fatalf("thumbnail.New: %v", err)
	}
	coverMgr := thumbnail.NewBatchManager(store, archSvc, thumbSvc, logger)
	defer coverMgr.Close()
	maintMgr := maintenance.New(store, archSvc, thumbSvc, logger)
	defer maintMgr.Close()
	bgmSvc, err := bangumi.NewService(store, tmpDir, logger)
	if err != nil {
		t.Fatalf("bangumi.NewService: %v", err)
	}
	defer bgmSvc.Close()

	srv := New(store, client, scanMgr, cacheMgr, archSvc, thumbSvc, coverMgr, maintMgr, RuntimeInfo{DataDir: tmpDir, Host: "127.0.0.1", Port: 25600}, logger).
		WithServices(wdMgr, bgmSvc)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 0. Embedded codename font is publicly served as font/woff2
	fontResp, err := http.Get(ts.URL + "/ui/codename-subset.woff2")
	if err != nil {
		t.Fatalf("GET font: %v", err)
	}
	fontResp.Body.Close()
	if fontResp.StatusCode != http.StatusOK || !strings.Contains(fontResp.Header.Get("Content-Type"), "font/woff2") {
		t.Fatalf("unexpected codename font response: status=%d ct=%q", fontResp.StatusCode, fontResp.Header.Get("Content-Type"))
	}

	// Claim primary admin
	if _, err := store.ClaimAdmin(ctx, "adminuser", "AdminPass123!"); err != nil {
		t.Fatalf("ClaimAdmin: %v", err)
	}

	// Unauthenticated /admin/api/auth/status must NOT leak version or codename
	anonResp, err := http.Get(ts.URL + "/admin/api/auth/status")
	if err != nil {
		t.Fatalf("GET anon auth/status: %v", err)
	}
	var anonStatus map[string]any
	_ = json.NewDecoder(anonResp.Body).Decode(&anonStatus)
	anonResp.Body.Close()
	if _, hasVer := anonStatus["version"]; hasVer {
		t.Fatalf("unauthenticated auth/status must not leak version: %+v", anonStatus)
	}
	if _, hasCode := anonStatus["codename"]; hasCode {
		t.Fatalf("unauthenticated auth/status must not leak codename: %+v", anonStatus)
	}

	// Seed libraries & books
	nowStr := time.Now().UTC().Format(time.RFC3339)
	for _, lib := range []database.Library{
		{ID: "lib1", Name: "PublicLib", ProviderType: database.ProviderLocal, RootCID: "local:" + lib1Dir, RootPath: lib1Dir, Enabled: true},
		{ID: "lib2", Name: "SecretLib", ProviderType: database.ProviderLocal, RootCID: "local:" + lib2Dir, RootPath: lib2Dir, Enabled: true},
	} {
		if err := store.UpsertLibrary(ctx, lib); err != nil {
			t.Fatalf("UpsertLibrary: %v", err)
		}
	}
	encStat, _ := os.Stat(encPath)
	nsfwStat, _ := os.Stat(nsfwPath)
	secStat, _ := os.Stat(secPath)
	seedExec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	seedExec(`INSERT INTO series(id,library_id,cid,name,relative_path,books_count,one_shot,created_at,updated_at,seen_scan_id) VALUES('s1','lib1','c1','SeriesOne','SeriesOne',1,0,?,?,'scan1')`, nowStr, nowStr)
	seedExec(`INSERT INTO series(id,library_id,cid,name,relative_path,books_count,one_shot,created_at,updated_at,seen_scan_id) VALUES('s1_nsfw','lib1','cn','SeriesNSFW','SeriesNSFW',1,0,?,?,'scan1')`, nowStr, nowStr)
	seedExec(`INSERT INTO series(id,library_id,cid,name,relative_path,books_count,one_shot,created_at,updated_at,seen_scan_id) VALUES('s2','lib2','c2','SecretSeries','SecretSeries',1,0,?,?,'scan1')`, nowStr, nowStr)
	seedExec(`INSERT INTO books(id,series_id,library_id,file_id,parent_cid,pick_code,sha1,name,size,number_sort,created_at,updated_at,seen_scan_id) VALUES('b1_enc','s1','lib1','SeriesOne/Vol01.cbz','c1','SeriesOne/Vol01.cbz','enc1','Vol01.cbz',?,1,?,?,'scan1')`, encStat.Size(), nowStr, nowStr)
	seedExec(`INSERT INTO books(id,series_id,library_id,file_id,parent_cid,pick_code,sha1,name,size,number_sort,created_at,updated_at,seen_scan_id) VALUES('b1_nsfw','s1_nsfw','lib1','SeriesNSFW/N01.cbz','cn','SeriesNSFW/N01.cbz','nsfw1','N01.cbz',?,1,?,?,'scan1')`, nsfwStat.Size(), nowStr, nowStr)
	seedExec(`INSERT INTO books(id,series_id,library_id,file_id,parent_cid,pick_code,sha1,name,size,number_sort,created_at,updated_at,seen_scan_id) VALUES('b2_sec','s2','lib2','SecretSeries/S01.cbz','c2','SecretSeries/S01.cbz','sec1','S01.cbz',?,1,?,?,'scan1')`, secStat.Size(), nowStr, nowStr)
	isNSFW := true
	if _, err := store.PatchSeriesMetadata(ctx, database.Series{ID: "s1_nsfw", LibraryID: "lib1", Name: "SeriesNSFW"}, database.SeriesMetadataPatch{IsNSFW: &isNSFW}); err != nil {
		t.Fatalf("PatchSeriesMetadata(nsfw): %v", err)
	}
	// Link s1 and s2 to same bgm_subject_id=777 to verify bookshelfVersions respects library allowlist!
	seedExec(`INSERT OR REPLACE INTO series_metadata(series_id,title,bgm_subject_id,is_nsfw,updated_at) VALUES('s1','SharedWork',777,0,?)`, nowStr)
	seedExec(`INSERT OR REPLACE INTO series_metadata(series_id,title,bgm_subject_id,is_nsfw,updated_at) VALUES('s2','SharedWork',777,0,?)`, nowStr)

	doReq := func(method, path string, body any, user, pass, apiKey string) *http.Response {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req, err := http.NewRequest(method, ts.URL+path, &buf)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if apiKey != "" {
			req.Header.Set("X-API-Key", apiKey)
		} else if user != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do %s %s: %v", method, path, err)
		}
		return resp
	}

	// 1. Authenticated /admin/api/auth/status exposes codename + quote
	stResp := doReq("GET", "/admin/api/auth/status", nil, "adminuser", "AdminPass123!", "")
	var adminSt map[string]any
	_ = json.NewDecoder(stResp.Body).Decode(&adminSt)
	stResp.Body.Close()
	if adminSt["codename"] != buildinfo.Codename || adminSt["codenameQuote"] != buildinfo.CodenameQuote || adminSt["isAdmin"] != true {
		t.Fatalf("unexpected admin auth/status: %+v", adminSt)
	}

	// 2. Admin creates User A (only lib1, allowNsfw=false) and User B (all libs, allowNsfw=true)
	createA := doReq("POST", "/admin/api/users", map[string]any{
		"username":          "alice",
		"password":          "AlicePass123!",
		"allLibraries":      false,
		"allowedLibraryIds": []string{"lib1"},
		"allowNsfw":         false,
	}, "adminuser", "AdminPass123!", "")
	if createA.StatusCode != http.StatusCreated {
		t.Fatalf("create alice status=%d", createA.StatusCode)
	}
	var userA database.User
	_ = json.NewDecoder(createA.Body).Decode(&userA)
	createA.Body.Close()

	createB := doReq("POST", "/admin/api/users", map[string]any{
		"username":     "bob",
		"password":     "BobPass123!",
		"allLibraries": true,
		"allowNsfw":    true,
	}, "adminuser", "AdminPass123!", "")
	if createB.StatusCode != http.StatusCreated {
		t.Fatalf("create bob status=%d", createB.StatusCode)
	}
	var userB database.User
	_ = json.NewDecoder(createB.Body).Decode(&userB)
	createB.Body.Close()

	// 2b. Shelf display preferences (including the infinite-scroll toggle) stay
	// per-account: Alice's choice must never show up for Bob.
	prefA := doReq("PATCH", "/api/v1/client-settings/user", map[string]any{
		"webui.shelf.infiniteScroll": map[string]any{"value": "1"},
	}, "alice", "AlicePass123!", "")
	prefA.Body.Close()
	if prefA.StatusCode != http.StatusNoContent {
		t.Fatalf("alice infinite scroll PATCH status=%d", prefA.StatusCode)
	}

	var aliceSettings map[string]map[string]any
	settingsA := doReq("GET", "/api/v1/client-settings/user/list", nil, "alice", "AlicePass123!", "")
	if settingsA.StatusCode != http.StatusOK {
		t.Fatalf("alice client settings status=%d", settingsA.StatusCode)
	}
	_ = json.NewDecoder(settingsA.Body).Decode(&aliceSettings)
	settingsA.Body.Close()
	if aliceSettings["webui.shelf.infiniteScroll"]["value"] != "1" {
		t.Fatalf("alice infinite scroll preference missing: %#v", aliceSettings)
	}

	var bobSettings map[string]map[string]any
	settingsB := doReq("GET", "/api/v1/client-settings/user/list", nil, "bob", "BobPass123!", "")
	if settingsB.StatusCode != http.StatusOK {
		t.Fatalf("bob client settings status=%d", settingsB.StatusCode)
	}
	_ = json.NewDecoder(settingsB.Body).Decode(&bobSettings)
	settingsB.Body.Close()
	if _, leaked := bobSettings["webui.shelf.infiniteScroll"]; leaked {
		t.Fatalf("bob must not inherit alice's shelf preference: %#v", bobSettings)
	}

	// Only the canonical "0"/"1" encodings are accepted for the boolean toggle.
	badPrefA := doReq("PATCH", "/api/v1/client-settings/user", map[string]any{
		"webui.shelf.infiniteScroll": map[string]any{"value": "yes"},
	}, "alice", "AlicePass123!", "")
	badPrefA.Body.Close()
	if badPrefA.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid infinite scroll value status=%d", badPrefA.StatusCode)
	}

	// 3. RBAC: Alice MUST get 403 on all admin-only routes, metadata PATCH, and Agent API!
	forbiddenChecks := []struct {
		method string
		path   string
		body   any
	}{
		{"GET", "/admin/api/users", nil},
		{"GET", "/admin/api/status", nil},
		{"GET", "/admin/api/settings", nil},
		{"GET", "/admin/api/libraries", nil},
		{"GET", "/admin/api/archive-passwords", nil},
		{"GET", "/admin/api/update", nil},
		{"GET", "/admin/api/diagnostics", nil},
		{"PATCH", "/api/v1/series/s1/metadata", map[string]any{"title": "Hacked"}},
		{"GET", "/api/agent/v1/manifest", nil},
		{"GET", "/api/agent/v1/overview", nil},
		{"PUT", "/admin/api/bangumi/tracker/config", map[string]any{"customOAuthClientId": "evil"}},
	}
	for _, tc := range forbiddenChecks {
		resp := doReq(tc.method, tc.path, tc.body, "alice", "AlicePass123!", "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("RBAC bypass! alice %s %s expected 403, got %d", tc.method, tc.path, resp.StatusCode)
		}
	}

	// Alice cannot rename herself (403)
	renameResp := doReq("PUT", "/admin/api/auth/credentials", map[string]any{
		"currentPassword": "AlicePass123!",
		"newUsername":     "alice_renamed",
	}, "alice", "AlicePass123!", "")
	renameResp.Body.Close()
	if renameResp.StatusCode != http.StatusForbidden {
		t.Fatalf("alice self-rename must be 403, got %d", renameResp.StatusCode)
	}

	// 4. Komga /api/v2/users/me reflects real user role & library scope
	meA := doReq("GET", "/api/v2/users/me", nil, "alice", "AlicePass123!", "")
	var meABody map[string]any
	_ = json.NewDecoder(meA.Body).Decode(&meABody)
	meA.Body.Close()
	if meABody["id"] != userA.ID || meABody["email"] != "alice" || meABody["sharedAllLibraries"] != false {
		t.Fatalf("unexpected komgaMe for alice: %+v", meABody)
	}
	for _, r := range meABody["roles"].([]any) {
		if r == "ADMIN" {
			t.Fatalf("alice must not have ADMIN role in komgaMe: %+v", meABody["roles"])
		}
	}

	// 5. Library allowlist isolation across Komga API, OPDS, Web SPA, and bookshelfVersions
	libsResp := doReq("GET", "/api/v1/libraries", nil, "alice", "AlicePass123!", "")
	var libsList []map[string]any
	_ = json.NewDecoder(libsResp.Body).Decode(&libsList)
	libsResp.Body.Close()
	if len(libsList) != 1 || libsList[0]["id"] != "lib1" {
		t.Fatalf("alice /api/v1/libraries mismatch: %+v", libsList)
	}
	for _, hiddenPath := range []string{
		"/api/v1/series/s2",
		"/api/v1/books/b2_sec",
		"/api/v1/books/b2_sec/pages/1",
		"/opds/v1.2/libraries/lib2",
		"/opds/v1.2/series/s2",
		"/libraries/lib2",
		"/series/s2",
	} {
		r := doReq("GET", hiddenPath, nil, "alice", "AlicePass123!", "")
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("alice must get 404 on %s, got %d", hiddenPath, r.StatusCode)
		}
	}
	verResp := doReq("GET", "/admin/api/bookshelf/versions/s1", nil, "alice", "AlicePass123!", "")
	var verBody struct {
		Versions []map[string]any `json:"versions"`
	}
	_ = json.NewDecoder(verResp.Body).Decode(&verBody)
	verResp.Body.Close()
	if len(verBody.Versions) != 1 || verBody.Versions[0]["id"] != "s1" {
		t.Fatalf("alice bookshelfVersions leaked lib2 s2: %+v", verBody.Versions)
	}

	// 6. NSFW permission grant & per-user toggle isolation
	nsfwDeny := doReq("PUT", "/admin/api/me/preferences", map[string]any{"showNsfw": true}, "alice", "AlicePass123!", "")
	nsfwDeny.Body.Close()
	if nsfwDeny.StatusCode != http.StatusForbidden {
		t.Fatalf("alice without allowNsfw must get 403, got %d", nsfwDeny.StatusCode)
	}
	bobBefore := doReq("GET", "/api/v1/series/s1_nsfw", nil, "bob", "BobPass123!", "")
	bobBefore.Body.Close()
	if bobBefore.StatusCode != http.StatusNotFound {
		t.Fatalf("bob should get 404 on nsfw before enabling toggle, got %d", bobBefore.StatusCode)
	}
	bobToggle := doReq("PUT", "/admin/api/me/preferences", map[string]any{"showNsfw": true}, "bob", "BobPass123!", "")
	bobToggle.Body.Close()
	if bobToggle.StatusCode != http.StatusOK {
		t.Fatalf("bob enable nsfw toggle failed: %d", bobToggle.StatusCode)
	}
	bobAfter := doReq("GET", "/api/v1/series/s1_nsfw", nil, "bob", "BobPass123!", "")
	bobAfter.Body.Close()
	if bobAfter.StatusCode != http.StatusOK {
		t.Fatalf("bob should see s1_nsfw after toggle, got %d", bobAfter.StatusCode)
	}
	adminStillHidden := doReq("GET", "/api/v1/series/s1_nsfw", nil, "adminuser", "AdminPass123!", "")
	adminStillHidden.Body.Close()
	if adminStillHidden.StatusCode != http.StatusNotFound {
		t.Fatalf("admin should remain nsfw-hidden independently of bob, got %d", adminStillHidden.StatusCode)
	}

	// 7. Encrypted archive unlock: Alice CAN unlock her accessible book, CANNOT list plaintext passwords!
	unlockResp := doReq("POST", "/admin/api/books/b1_enc/unlock", map[string]any{"password": "open-sesame"}, "alice", "AlicePass123!", "")
	unlockResp.Body.Close()
	if unlockResp.StatusCode != http.StatusOK {
		t.Fatalf("alice should be allowed to unlock accessible encrypted book, got %d", unlockResp.StatusCode)
	}
	pageResp := doReq("GET", "/api/v1/books/b1_enc/pages/1", nil, "alice", "AlicePass123!", "")
	pageResp.Body.Close()
	if pageResp.StatusCode != http.StatusOK {
		t.Fatalf("alice should read unlocked page, got %d", pageResp.StatusCode)
	}
	// Admin CAN see the saved plaintext password
	apwAdmin := doReq("GET", "/admin/api/archive-passwords", nil, "adminuser", "AdminPass123!", "")
	if apwAdmin.StatusCode != http.StatusOK {
		t.Fatalf("admin archive-passwords status=%d", apwAdmin.StatusCode)
	}
	apwAdmin.Body.Close()

	// 8. Per-user API Key isolation (Alice cannot delete Bob's key)
	akCreateB := doReq("POST", "/admin/api/auth/api-keys", map[string]any{"name": "BobPhone"}, "bob", "BobPass123!", "")
	var akB struct {
		Key    string          `json:"key"`
		Record database.APIKey `json:"record"`
	}
	_ = json.NewDecoder(akCreateB.Body).Decode(&akB)
	akCreateB.Body.Close()
	if akB.Key == "" || akB.Record.ID == "" {
		t.Fatalf("failed to create bob API key")
	}
	crossDel := doReq("DELETE", "/admin/api/auth/api-keys/"+akB.Record.ID, nil, "alice", "AlicePass123!", "")
	crossDel.Body.Close()
	if crossDel.StatusCode != http.StatusNotFound {
		t.Fatalf("alice must not delete bob API key, got %d", crossDel.StatusCode)
	}
	// Bob's API key works for Bob and gets 403 on admin endpoint
	bobKeyMe := doReq("GET", "/api/v2/users/me", nil, "", "", akB.Key)
	var bobKeyMeBody map[string]any
	_ = json.NewDecoder(bobKeyMe.Body).Decode(&bobKeyMeBody)
	bobKeyMe.Body.Close()
	if bobKeyMeBody["id"] != userB.ID {
		t.Fatalf("bob API key authenticated wrong user: %+v", bobKeyMeBody)
	}
	bobKeyAdmin := doReq("GET", "/admin/api/users", nil, "", "", akB.Key)
	bobKeyAdmin.Body.Close()
	if bobKeyAdmin.StatusCode != http.StatusForbidden {
		t.Fatalf("bob API key must get 403 on admin endpoint, got %d", bobKeyAdmin.StatusCode)
	}

	// 9. Per-user SSE event isolation: Alice's read progress MUST NOT be delivered to Bob's SSE stream!
	sseCtxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	sseReqA, _ := http.NewRequestWithContext(sseCtxA, "GET", ts.URL+"/api/v1/sse/v1/events", nil)
	sseReqA.SetBasicAuth("alice", "AlicePass123!")
	sseRespA, err := http.DefaultClient.Do(sseReqA)
	if err != nil {
		t.Fatalf("open SSE A: %v", err)
	}
	defer sseRespA.Body.Close()

	sseCtxB, cancelB := context.WithCancel(ctx)
	defer cancelB()
	sseReqB, _ := http.NewRequestWithContext(sseCtxB, "GET", ts.URL+"/api/v1/sse/v1/events", nil)
	sseReqB.SetBasicAuth("bob", "BobPass123!")
	sseRespB, err := http.DefaultClient.Do(sseReqB)
	if err != nil {
		t.Fatalf("open SSE B: %v", err)
	}
	defer sseRespB.Body.Close()

	eventsA := make(chan string, 4)
	eventsB := make(chan string, 4)
	readSSE := func(r *http.Response, out chan<- string) {
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				out <- strings.TrimPrefix(line, "event: ")
			}
		}
	}
	go readSSE(sseRespA, eventsA)
	go readSSE(sseRespB, eventsB)
	time.Sleep(60 * time.Millisecond)

	progA := doReq("PATCH", "/api/v1/books/b1_enc/read-progress", map[string]any{"page": 1, "completed": true}, "alice", "AlicePass123!", "")
	progA.Body.Close()
	if progA.StatusCode != http.StatusNoContent {
		t.Fatalf("alice progress patch status=%d", progA.StatusCode)
	}

	select {
	case ev := <-eventsA:
		if ev != "ReadProgressChanged" {
			t.Fatalf("unexpected event for alice: %q", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("alice did not receive SSE ReadProgressChanged")
	}
	select {
	case ev := <-eventsB:
		t.Fatalf("SSE LEAK: bob received alice's event %q!", ev)
	case <-time.After(200 * time.Millisecond):
		// Expected: bob receives nothing
	}

	// 10. Disable Bob immediately blocks Bob's API Key & Basic Auth; Delete Bob cleans up cleanly
	disB := doReq("PUT", "/admin/api/users/"+userB.ID, map[string]any{"enabled": false}, "adminuser", "AdminPass123!", "")
	disB.Body.Close()
	if disB.StatusCode != http.StatusOK {
		t.Fatalf("disable bob status=%d", disB.StatusCode)
	}
	bobKeyBlocked := doReq("GET", "/api/v2/users/me", nil, "", "", akB.Key)
	bobKeyBlocked.Body.Close()
	if bobKeyBlocked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled bob API key must get 401, got %d", bobKeyBlocked.StatusCode)
	}
	bobBasicBlocked := doReq("GET", "/api/v2/users/me", nil, "bob", "BobPass123!", "")
	bobBasicBlocked.Body.Close()
	if bobBasicBlocked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled bob Basic Auth must get 401, got %d", bobBasicBlocked.StatusCode)
	}
	delB := doReq("DELETE", "/admin/api/users/"+userB.ID, nil, "adminuser", "AdminPass123!", "")
	delB.Body.Close()
	if delB.StatusCode != http.StatusNoContent {
		t.Fatalf("delete bob status=%d", delB.StatusCode)
	}
	// Primary admin cannot be deleted (400)
	delAdmin := doReq("DELETE", "/admin/api/users/admin", nil, "adminuser", "AdminPass123!", "")
	delAdmin.Body.Close()
	if delAdmin.StatusCode != http.StatusBadRequest {
		t.Fatalf("deleting primary admin must return 400, got %d", delAdmin.StatusCode)
	}

	// 11. Codex cross-review hardening regressions:
	// 11a. Creating user with allowedLibraryIds and omitted allLibraries must NOT default to allLibraries=true
	noAllResp := doReq("POST", "/admin/api/users", map[string]any{
		"username":          "zero_or_scoped",
		"password":          "ZeroPass123!",
		"allowedLibraryIds": []string{},
	}, "adminuser", "AdminPass123!", "")
	if noAllResp.StatusCode != http.StatusCreated {
		t.Fatalf("create zero-lib user status=%d", noAllResp.StatusCode)
	}
	var zeroUser database.User
	_ = json.NewDecoder(noAllResp.Body).Decode(&zeroUser)
	noAllResp.Body.Close()
	if zeroUser.AllLibraries {
		t.Fatalf("omitted allLibraries must default false, got true")
	}
	// 11b. Zero-library user passing explicit ?library_id=lib1 must fail CLOSED (0 tags/genres/publishers/authors/series)
	for _, path := range []string{
		"/api/v1/series?library_id=lib1",
		"/api/v1/genres?library_id=lib1",
		"/api/v1/tags?library_id=lib1",
		"/api/v1/publishers?library_id=lib1",
	} {
		rZero := doReq("GET", path, nil, "zero_or_scoped", "ZeroPass123!", "")
		bodyBytes, _ := io.ReadAll(rZero.Body)
		rZero.Body.Close()
		if rZero.StatusCode != http.StatusOK {
			t.Fatalf("zero-lib GET %s status=%d", path, rZero.StatusCode)
		}
		if strings.Contains(string(bodyBytes), "SeriesOne") || strings.Contains(string(bodyBytes), "SharedWork") {
			t.Fatalf("zero-lib user leaked data on %s: %s", path, string(bodyBytes))
		}
	}

	// 11c. ResetUserPassword rejects primary admin (400) AND revokes target user's API keys
	rstAdmin := doReq("POST", "/admin/api/users/admin/reset-password", map[string]any{"password": "NewAdmin999!"}, "adminuser", "AdminPass123!", "")
	rstAdmin.Body.Close()
	if rstAdmin.StatusCode != http.StatusBadRequest {
		t.Fatalf("reset primary admin password must return 400, got %d", rstAdmin.StatusCode)
	}
	akZeroResp := doReq("POST", "/admin/api/auth/api-keys", map[string]any{"name": "zero-key"}, "zero_or_scoped", "ZeroPass123!", "")
	var akZero struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(akZeroResp.Body).Decode(&akZero)
	akZeroResp.Body.Close()
	rstZero := doReq("POST", "/admin/api/users/"+zeroUser.ID+"/reset-password", map[string]any{"password": "ZeroPass999!"}, "adminuser", "AdminPass123!", "")
	rstZero.Body.Close()
	if rstZero.StatusCode != http.StatusOK {
		t.Fatalf("reset zero user password status=%d", rstZero.StatusCode)
	}
	akZeroAfter := doReq("GET", "/api/v2/users/me", nil, "", "", akZero.Key)
	akZeroAfter.Body.Close()
	if akZeroAfter.StatusCode != http.StatusUnauthorized {
		t.Fatalf("API key must be revoked after password reset, got %d", akZeroAfter.StatusCode)
	}

	// 11d. Cross-origin CSRF guard applies to browser HTTP Basic Auth requests
	csrfReq, _ := http.NewRequest("POST", ts.URL+"/admin/api/users/"+zeroUser.ID+"/kick", nil)
	csrfReq.SetBasicAuth("adminuser", "AdminPass123!")
	csrfReq.Header.Set("Origin", "https://evil.example")
	csrfResp, err := http.DefaultClient.Do(csrfReq)
	if err != nil {
		t.Fatalf("csrf basic req: %v", err)
	}
	csrfResp.Body.Close()
	if csrfResp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin Basic Auth POST must return 403, got %d", csrfResp.StatusCode)
	}

	// 11e. X-FK115-User-Id header mismatch rejects stale reader write with 409
	mmReq, _ := http.NewRequest("PATCH", ts.URL+"/api/v1/books/b1_nsfw/read-progress", strings.NewReader(`{"page":1,"completed":false}`))
	mmReq.SetBasicAuth("adminuser", "AdminPass123!")
	mmReq.Header.Set("Content-Type", "application/json")
	mmReq.Header.Set("X-FK115-User-Id", "u_other_account")
	mmResp, err := http.DefaultClient.Do(mmReq)
	if err != nil {
		t.Fatalf("mismatch req: %v", err)
	}
	mmResp.Body.Close()
	if mmResp.StatusCode != http.StatusConflict {
		t.Fatalf("X-FK115-User-Id mismatch must return 409, got %d", mmResp.StatusCode)
	}

	// 11f. Unencrypted comic page must send Cache-Control: private, no-store (no cross-account browser cache reuse)
	plainPageResp := doReq("GET", "/api/v1/books/b2_sec/pages/1", nil, "adminuser", "AdminPass123!", "")
	plainPageResp.Body.Close()
	if plainPageResp.StatusCode != http.StatusOK {
		t.Fatalf("admin GET b2_sec page 1 status=%d", plainPageResp.StatusCode)
	}
	if cc := plainPageResp.Header.Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("expected private, no-store on protected comic page, got %q", cc)
	}

	// 11g. Public Agent guide does not leak server build version
	guideResp, gErr := http.Get(ts.URL + "/api/agent/v1/guide?format=json")
	if gErr != nil {
		t.Fatalf("GET agent guide: %v", gErr)
	}
	guideBytes, _ := io.ReadAll(guideResp.Body)
	guideResp.Body.Close()
	if strings.Contains(string(guideBytes), buildinfo.Version) {
		t.Fatalf("public agent guide must not leak build version %q: %s", buildinfo.Version, string(guideBytes))
	}

	// 11h. Non-3.x build version must NOT emit codename
	prevVer := buildinfo.Version
	buildinfo.Version = "v2.9.0"
	if cn, _ := buildinfo.CodenameForVersion(buildinfo.Version); cn != "" {
		t.Fatalf("v2.9.0 must not emit codename, got %q", cn)
	}
	buildinfo.Version = "v3.0.0"
	if cn, _ := buildinfo.CodenameForVersion(buildinfo.Version); cn != buildinfo.Codename {
		t.Fatalf("v3.0.0 must emit codename %q, got %q", buildinfo.Codename, cn)
	}
	buildinfo.Version = prevVer
}
