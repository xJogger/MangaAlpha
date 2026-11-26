package httpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateClientSettingWebUIDisplayRanges(t *testing.T) {
	value := func(s string) *string { return &s }
	cases := []struct {
		name       string
		key        string
		value      *string
		wantErr    bool
		errContain string
	}{
		{"page size min", "webui.shelf.pageSize", value("12"), false, ""},
		{"page size max", "webui.shelf.pageSize", value("120"), false, ""},
		{"page size default", "webui.shelf.pageSize", value("36"), false, ""},
		{"page size below range", "webui.shelf.pageSize", value("11"), true, ""},
		{"page size above range", "webui.shelf.pageSize", value("121"), true, ""},
		{"page size not a number", "webui.shelf.pageSize", value("abc"), true, ""},
		{"page size deletion", "webui.shelf.pageSize", nil, false, ""},
		{"home updated in range", "webui.home.updatedSize", value("18"), false, ""},
		{"home added above range", "webui.home.addedSize", value("37"), true, ""},
		{"home continue below range", "webui.home.continueSize", value("3"), true, ""},
		{"infinite scroll enabled", "webui.shelf.infiniteScroll", value("1"), false, ""},
		{"infinite scroll disabled", "webui.shelf.infiniteScroll", value("0"), false, ""},
		{"infinite scroll boolean text", "webui.shelf.infiniteScroll", value("true"), true, `"0" or "1"`},
		{"infinite scroll out of range", "webui.shelf.infiniteScroll", value("2"), true, `"0" or "1"`},
		{"infinite scroll empty value", "webui.shelf.infiniteScroll", value(""), true, `"0" or "1"`},
		{"infinite scroll deletion", "webui.shelf.infiniteScroll", nil, false, ""},
		{"booksView list", "webui.series.booksView", value("list"), false, ""},
		{"booksView cover", "webui.series.booksView", value("cover"), false, ""},
		{"booksView invalid grid", "webui.series.booksView", value("grid"), true, `"cover" or "list"`},
		{"booksView empty", "webui.series.booksView", value(""), true, `"cover" or "list"`},
		{"booksView deletion", "webui.series.booksView", nil, false, ""},
		{"unrelated webui key untouched", "webui.reader.mode", value("rtl"), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateClientSetting(tc.key, tc.value)
			display := "<nil>"
			if tc.value != nil {
				display = *tc.value
			}
			if tc.wantErr && err == nil {
				t.Fatalf("validateClientSetting(%q) = nil, want error", display)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateClientSetting(%q) = %v, want nil", display, err)
			}
			if tc.errContain != "" && (err == nil || !strings.Contains(err.Error(), tc.errContain)) {
				t.Fatalf("validateClientSetting(%q) error = %v, want substring %q", display, err, tc.errContain)
			}
		})
	}
}

func TestClientSettingsBooksViewHTTPAndMultiUserIsolation(t *testing.T) {
	srv, store, _, _ := setupV27TestServer(t, "")
	ctx := t.Context()
	if _, err := store.ClaimAdmin(ctx, "owner-admin", "AdminPass123!"); err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()
	doJSON := func(method, path, user, pass string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var rBody *bytes.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			rBody = bytes.NewReader(raw)
		} else {
			rBody = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rBody)
		req.SetBasicAuth(user, pass)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	for _, u := range []string{"reader-alpha", "reader-beta"} {
		w := doJSON(http.MethodPost, "/admin/api/users", "owner-admin", "AdminPass123!", map[string]any{
			"username":     u,
			"password":     "ReaderPass123!",
			"allLibraries": true,
			"allowNsfw":    false,
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("create user %s: status=%d body=%s", u, w.Code, w.Body.String())
		}
	}

	// Admin sets "list"; reader-alpha sets "cover"; invalid value returns 400 with enum list.
	if w := doJSON(http.MethodPatch, "/api/v1/client-settings/user", "owner-admin", "AdminPass123!", map[string]any{
		"webui.series.booksView": map[string]any{"value": "list"},
	}); w.Code != http.StatusNoContent {
		t.Fatalf("admin patch valid list: status=%d body=%s", w.Code, w.Body.String())
	}
	if w := doJSON(http.MethodPatch, "/api/v1/client-settings/user", "reader-alpha", "ReaderPass123!", map[string]any{
		"webui.series.booksView": map[string]any{"value": " cover "},
	}); w.Code != http.StatusNoContent {
		t.Fatalf("alpha patch valid cover: status=%d body=%s", w.Code, w.Body.String())
	}
	bad := doJSON(http.MethodPatch, "/api/v1/client-settings/user", "reader-alpha", "ReaderPass123!", map[string]any{
		"webui.series.booksView": map[string]any{"value": "grid"},
	})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid booksView status=%d, want 400 body=%s", bad.Code, bad.Body.String())
	}
	if !strings.Contains(bad.Body.String(), "cover") || !strings.Contains(bad.Body.String(), "list") {
		t.Fatalf("expected error listing allowed values cover/list, got %s", bad.Body.String())
	}

	readBooksView := func(user, pass string) (string, bool) {
		t.Helper()
		w := doJSON(http.MethodGet, "/api/v1/client-settings/user/list", user, pass, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("get settings as %s: status=%d body=%s", user, w.Code, w.Body.String())
		}
		var out map[string]struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		item, ok := out["webui.series.booksView"]
		return item.Value, ok
	}

	if got, ok := readBooksView("owner-admin", "AdminPass123!"); !ok || got != "list" {
		t.Fatalf("admin booksView = (%q, %v), want (\"list\", true)", got, ok)
	}
	if got, ok := readBooksView("reader-alpha", "ReaderPass123!"); !ok || got != "cover" {
		t.Fatalf("alpha booksView = (%q, %v), want (\"cover\", true)", got, ok)
	}
	if got, ok := readBooksView("reader-beta", "ReaderPass123!"); ok {
		t.Fatalf("beta leaked booksView = %q, want absent", got)
	}

	// Deleting admin's setting does not affect reader-alpha.
	if w := doJSON(http.MethodPatch, "/api/v1/client-settings/user", "owner-admin", "AdminPass123!", map[string]any{
		"webui.series.booksView": map[string]any{"value": nil},
	}); w.Code != http.StatusNoContent {
		t.Fatalf("admin delete booksView: status=%d body=%s", w.Code, w.Body.String())
	}
	if _, ok := readBooksView("owner-admin", "AdminPass123!"); ok {
		t.Fatalf("admin booksView should be deleted")
	}
	if got, ok := readBooksView("reader-alpha", "ReaderPass123!"); !ok || got != "cover" {
		t.Fatalf("alpha booksView changed after admin delete: (%q, %v)", got, ok)
	}
}
