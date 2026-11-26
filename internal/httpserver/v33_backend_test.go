package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/xJogger/fake-komga-115/internal/archive"
	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/cache"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/oneonefive"
	"github.com/xJogger/fake-komga-115/internal/thumbnail"
)

const v33TestPassword = "test-password"

type v33HTTPFixture struct {
	store   *database.Store
	handler http.Handler
	dir     string
}

func newV33HTTPFixture(t *testing.T, withArchives, withBangumi bool) v33HTTPFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := database.Open(filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ClaimAdmin(ctx, "owner", v33TestPassword); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := oneonefive.New(store, logger)
	var archives *archive.Service
	var thumbs *thumbnail.Service
	if withArchives {
		cacheMgr, err := cache.New(store, filepath.Join(dir, "cache"))
		if err != nil {
			t.Fatal(err)
		}
		archives = archive.NewService(store, client, cacheMgr, logger)
		t.Cleanup(func() { archives.Close() })
		thumbs, err = thumbnail.New(store, filepath.Join(dir, "covers"), logger)
		if err != nil {
			t.Fatal(err)
		}
	}
	srv := New(store, client, nil, nil, archives, thumbs, nil, nil, RuntimeInfo{}, logger)
	if withBangumi {
		bgm, err := bangumi.NewService(store, filepath.Join(dir, "bangumi"), logger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { bgm.Close() })
		srv.WithServices(nil, bgm)
	}
	return v33HTTPFixture{store: store, handler: srv.Handler(), dir: dir}
}

func (f v33HTTPFixture) request(t *testing.T, user, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if user != "" {
		req.SetBasicAuth(user, v33TestPassword)
	}
	return f.serve(t, req, status)
}

func (f v33HTTPFixture) serve(t *testing.T, req *http.Request, status int) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("%s %s: expected %d, got %d: %s", req.Method, req.URL, status, rec.Code, rec.Body.String())
	}
	return rec
}

func decodeV33[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	return result
}

func (f v33HTTPFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.store.DB().Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f v33HTTPFixture) library(t *testing.T, id, name string, enabled bool) {
	t.Helper()
	root := filepath.Join(f.dir, id)
	if err := f.store.UpsertLibrary(context.Background(), database.Library{
		ID: id, Name: name, RootCID: "local:" + root, RootPath: root, ProviderType: database.ProviderLocal, Enabled: enabled,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f v33HTTPFixture) book(t *testing.T, lib, series, book string, writeArchive bool) {
	t.Helper()
	const now = "2026-01-01T00:00:00Z"
	f.exec(t, `INSERT OR IGNORE INTO series(id,library_id,cid,name,relative_path,created_at,updated_at,seen_scan_id)
VALUES(?,?,?,?,?,?,?,'scan')`, series, lib, series, series, series, now, now)
	name := book + ".cbz"
	fileID := series + "/" + name
	size := int64(100)
	if writeArchive {
		target := filepath.Join(f.dir, lib, series, name)
		writeTestCBZ(t, target, 3)
		stat, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		size = stat.Size()
	}
	f.exec(t, `INSERT INTO books(id,series_id,library_id,file_id,parent_cid,pick_code,name,size,number_sort,created_at,updated_at,seen_scan_id)
VALUES(?,?,?,?,?,'',?,?,1,?,?,'scan')`, book, series, lib, fileID, series, name, size, now, now)
}

func (f v33HTTPFixture) user(t *testing.T, name string, allowNSFW bool, libs ...string) database.User {
	t.Helper()
	user, err := f.store.CreateUser(context.Background(), database.CreateUserInput{
		Username: name, Password: v33TestPassword, AllowedLibraryIDs: libs, AllowNSFW: allowNSFW,
	})
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func (f v33HTTPFixture) progress(t *testing.T, userID, bookID, date string, done bool) {
	t.Helper()
	f.exec(t, `INSERT INTO book_read_progress(user_id,book_id,series_id,completed,page,updated_at)
SELECT ?,id,series_id,?,1,? FROM books WHERE id=?`, userID, done, date, bookID)
}

func TestV33PersonalHiddenLibrariesAcrossClients(t *testing.T) {
	f := newV33HTTPFixture(t, true, false)
	ctx := context.Background()
	for _, lib := range []string{"hidden", "visible", "denied", "disabled"} {
		f.library(t, lib, "Catalog "+lib, lib != "disabled")
		f.book(t, lib, "series-"+lib, "read-"+lib, lib == "hidden")
		f.book(t, lib, "series-"+lib, "next-"+lib, false)
	}
	f.book(t, "hidden", "activity-hidden", "current-hidden", false)
	f.book(t, "visible", "activity-visible", "current-visible", false)
	f.book(t, "hidden", "adult-hidden", "adult-hidden", false)
	f.exec(t, "UPDATE books SET number_sort=2 WHERE id LIKE 'next-%'")
	for _, lib := range []string{"visible", "hidden"} {
		series := database.Series{ID: "series-" + lib, LibraryID: lib}
		if _, err := f.store.ApplyScrapedMetadata(ctx, series, database.ScrapedMetadata{
			SubjectID: 44, Title: "Edition " + lib, Publisher: "Publisher " + lib,
			Genres: []string{"Genre " + lib}, Tags: []string{"Tag " + lib},
			Authors: []database.Author{{Name: "Author " + lib, Role: "writer"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(t, `INSERT INTO series_metadata(series_id,is_nsfw,updated_at) VALUES('adult-hidden',1,'2026-01-01T00:00:00Z')`)
	reader := f.user(t, "reader", true, "hidden", "visible", "disabled")
	other := f.user(t, "other", false, "hidden", "visible")
	readerCtx := database.WithUserScope(ctx, reader.Scope())
	for _, lib := range []string{"visible", "hidden"} {
		f.progress(t, reader.ID, "read-"+lib, "2026-01-02T10:00:00Z", true)
		f.progress(t, reader.ID, "current-"+lib, "2026-01-02T11:00:00Z", false)
	}
	collection, err := f.store.CreateCollection(readerCtx, "Favorites", true, []string{"series-hidden", "series-visible"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.store.CreateReadList(readerCtx, "Queue", "", true, []string{"read-hidden", "read-visible"})
	if err != nil {
		t.Fatal(err)
	}

	get := func(path string) *httptest.ResponseRecorder { return f.request(t, "reader", "GET", path, "", 200) }
	assertBody := func(path, include, exclude string) {
		t.Helper()
		body := get(path).Body.String()
		if !strings.Contains(body, include) || strings.Contains(body, exclude) {
			t.Fatalf("%s: expected %q and no %q: %s", path, include, exclude, body)
		}
	}
	assertBody("/admin/api/bookshelf/series", "series-hidden", "series-denied")
	assertBody("/api/v1/books?read_status=IN_PROGRESS&size=8&sort=readProgress.readDate,desc", "current-hidden", "current-denied")
	rawSession, _, err := f.store.CreateSessionForUser(ctx, reader.ID, reader.Username, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PUT", "/admin/api/me/preferences",
		strings.NewReader(`{"hiddenLibraryIds":["hidden","denied","hidden","missing"]}`))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: rawSession})
	req.Header.Set("X-FK115-User-Id", reader.ID)
	prefs := decodeV33[database.UserPreferences](t, f.serve(t, req, 200))
	if !prefs.AllowNSFW || prefs.ShowNSFW || !slices.Equal(prefs.HiddenLibraryIDs, []string{"hidden"}) {
		t.Fatalf("hidden preference: %+v", prefs)
	}

	account := decodeV33[struct {
		HiddenLibraryIDs []string `json:"hiddenLibraryIds"`
		Authorized       []struct {
			ID, Name, ProviderType string
			Hidden                 bool
		} `json:"authorizedLibraries"`
		Visible []struct{ ID string } `json:"visibleLibraries"`
	}](t, get("/admin/api/me"))
	if !slices.Equal(account.HiddenLibraryIDs, []string{"hidden"}) || len(account.Authorized) != 2 ||
		account.Authorized[0].ID != "hidden" || account.Authorized[0].Name != "Catalog hidden" ||
		account.Authorized[0].ProviderType != "local" || !account.Authorized[0].Hidden ||
		account.Authorized[1].Hidden || len(account.Visible) != 1 || account.Visible[0].ID != "visible" {
		t.Fatalf("account catalog projection: %+v", account)
	}

	for _, tc := range []struct{ path, include, exclude string }{
		{"/api/v1/libraries", "Catalog visible", "Catalog hidden"},
		{"/api/v1/series?search=Edition", "series-visible", "series-hidden"},
		{"/api/v1/books?search=next", "next-visible", "next-hidden"},
		{"/api/v1/books?read_status=IN_PROGRESS&size=8&sort=readProgress.readDate,desc", "current-visible", "current-hidden"},
		{"/api/v1/genres", "Genre visible", "Genre hidden"},
		{"/api/v1/tags", "Tag visible", "Tag hidden"},
		{"/api/v1/publishers", "Publisher visible", "Publisher hidden"},
		{"/api/v1/authors", "Author visible", "Author hidden"},
		{"/api/v2/authors", "Author visible", "Author hidden"},
		{"/admin/api/bookshelf/series", "series-visible", "series-hidden"},
		{"/admin/api/bookshelf/continue-reading", "current-visible", "current-hidden"},
		{"/admin/api/bookshelf/history", "read-visible", "read-hidden"},
		{"/admin/api/bookshelf/series/series-visible/versions", "series-visible", "series-hidden"},
		{"/api/v1/collections?unpaged=true", "series-visible", "series-hidden"},
		{"/api/v1/readlists?unpaged=true", "read-visible", "read-hidden"},
		{"/api/v1/collections/" + collection.ID + "/series", "series-visible", "series-hidden"},
		{"/api/v1/readlists/" + list.ID + "/books", "read-visible", "read-hidden"},
		{"/opds/v1.2/libraries", "Catalog visible", "Catalog hidden"},
		{"/opds/v1.2/series?search=Edition", "Edition visible", "Edition hidden"},
		{"/opds/v1.2/series/latest", "Edition visible", "Edition hidden"},
		{"/opds/v1.2/books/latest", "next-visible", "next-hidden"},
		{"/opds/v1.2/on-deck", "next-visible", "next-hidden"},
		{"/api/opds/v1.2/library/sources", "Catalog visible", "Catalog hidden"},
		{"/api/opds/v1.2/library/series?query=Edition", "Edition visible", "Edition hidden"},
		{"/api/opds/v1.2/library-updates", "current-visible", "current-hidden"},
		{"/api/opds/v1.2/history", "current-visible", "current-hidden"},
	} {
		assertBody(tc.path, tc.include, tc.exclude)
	}
	for _, path := range []string{"/api/v1/collections/" + collection.ID, "/api/v1/readlists/" + list.ID} {
		assertBody(path, `"filtered":true`, "hidden")
	}
	if id, err := f.store.CollectionFirstVisibleSeriesID(database.WithUserScope(ctx, database.User{
		ID: reader.ID, AllowedLibraryIDs: reader.AllowedLibraryIDs, HiddenLibraryIDs: prefs.HiddenLibraryIDs,
	}.Scope()), collection.ID); err != nil || id != "series-visible" {
		t.Fatalf("collection first visible series: %q, %v", id, err)
	}
	currentReader, err := f.store.UserByID(ctx, reader.ID)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := f.store.ReadListFirstVisibleSeriesID(database.WithUserScope(ctx, currentReader.Scope()), list.ID); err != nil || id != "series-visible" {
		t.Fatalf("read list first visible series: %q, %v", id, err)
	}

	seriesRow, err := f.store.SeriesRowIDs(ctx, []string{"series-hidden"})
	if err != nil {
		t.Fatal(err)
	}
	bookRow, err := f.store.BookRowIDs(ctx, []string{"read-hidden"})
	if err != nil {
		t.Fatal(err)
	}
	libraryRow, err := f.store.LibraryRowIDs(ctx, []string{"hidden"})
	if err != nil {
		t.Fatal(err)
	}
	suwayomiSeries := "/api/opds/v1.2/series/" + strconv.FormatInt(seriesRow["series-hidden"], 10)
	suwayomiBook := suwayomiSeries + "/chapter/" + strconv.FormatInt(bookRow["read-hidden"], 10)
	for _, path := range []string{
		"/libraries/hidden", "/series/series-hidden", "/book/read-hidden", "/books/read-hidden",
		"/api/v1/series/series-hidden", "/api/v1/books/read-hidden", "/api/v1/books/read-hidden/pages",
		"/opds/v1.2/books/read-hidden", suwayomiBook + "/metadata",
	} {
		get(path)
	}
	for _, path := range []string{
		"/api/v1/books/read-hidden/pages/1/raw", "/api/v1/books/read-hidden/thumbnail", "/api/v1/series/series-hidden/thumbnail",
		"/opds/v1.2/books/read-hidden/thumbnail", "/opds/v1.2/books/read-hidden/pages/1", suwayomiBook + "/page/1", suwayomiSeries + "/thumbnail",
	} {
		if cacheControl := get(path).Header().Get("Cache-Control"); cacheControl != "private, no-store" {
			t.Fatalf("%s unexpected Cache-Control=%q", path, cacheControl)
		}
	}
	assertBody("/api/v1/series/series-hidden/books", "read-hidden", "read-visible")
	assertBody("/api/v1/series?library_id=hidden&search=Edition", "series-hidden", "series-visible")
	assertBody("/admin/api/bookshelf/series?library_id=hidden", "series-hidden", "series-visible")
	assertBody("/api/v1/books?library_id=hidden", "read-hidden", "read-visible")
	for _, path := range []string{
		"/libraries/denied", "/libraries/disabled", "/series/series-denied", "/books/read-disabled", "/api/v1/books/adult-hidden",
		"/opds/v1.2/libraries/hidden", "/opds/v1.2/series/series-hidden", suwayomiSeries + "/chapters",
		"/api/opds/v1.2/source/" + strconv.FormatInt(libraryRow["hidden"], 10),
	} {
		f.request(t, "reader", "GET", path, "", 404)
	}
	f.request(t, "reader", "PATCH", "/api/v1/books/read-hidden/read-progress", `{"page":2}`, 204)
	f.request(t, "reader", "DELETE", "/api/v1/books/read-hidden/read-progress", "", 204)

	apiKey, _, err := f.store.CreateAPIKeyForUser(ctx, reader.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/api/v1/libraries", nil)
	request.Header.Set("X-API-Key", apiKey)
	if body := f.serve(t, request, 200).Body.String(); strings.Contains(body, "Catalog hidden") {
		t.Fatalf("API key did not apply personal preferences: %s", body)
	}
	if body := f.request(t, other.Username, "GET", "/api/v1/libraries", "", 200).Body.String(); !strings.Contains(body, "Catalog hidden") {
		t.Fatalf("reader preferences leaked to another user: %s", body)
	}

	prefs = decodeV33[database.UserPreferences](t, f.request(t, "reader", "PUT", "/admin/api/me/preferences",
		`{"showNsfw":true,"hiddenLibraryIds":["visible","hidden"]}`, 200))
	if !prefs.ShowNSFW || !slices.Equal(prefs.HiddenLibraryIDs, []string{"hidden", "visible"}) {
		t.Fatalf("combined preferences: %+v", prefs)
	}
	f.request(t, "reader", "GET", "/api/v1/books/adult-hidden", "", 200)
	status := decodeV33[map[string]any](t, get("/admin/api/auth/status"))
	if status["authorizedLibrariesCount"] != float64(2) || status["hasHiddenLibraries"] != true {
		t.Fatalf("all-hidden auth status: %+v", status)
	}
	if libs := decodeV33[[]any](t, get("/api/v1/libraries")); libs == nil || len(libs) != 0 {
		t.Fatalf("catalog when every authorized library is hidden: %+v", libs)
	}
	f.user(t, "empty", false)
	empty := decodeV33[map[string]any](t, f.request(t, "empty", "GET", "/admin/api/auth/status", "", 200))
	if empty["authorizedLibrariesCount"] != float64(0) || empty["hasHiddenLibraries"] != false {
		t.Fatalf("no-authorization status: %+v", empty)
	}
	anon := decodeV33[map[string]any](t, f.request(t, "", "GET", "/admin/api/auth/status", "", 200))
	if anon["authorizedLibrariesCount"] != nil || anon["hasHiddenLibraries"] != nil {
		t.Fatalf("anonymous status leaked preferences: %+v", anon)
	}
	prefs = decodeV33[database.UserPreferences](t, f.request(t, "reader", "PUT", "/admin/api/me/preferences",
		`{"showNsfw":false}`, 200))
	if prefs.ShowNSFW || len(prefs.HiddenLibraryIDs) != 2 {
		t.Fatalf("NSFW-only update changed hidden books: %+v", prefs)
	}
	prefs = decodeV33[database.UserPreferences](t, f.request(t, "reader", "PUT", "/admin/api/me/preferences",
		`{"hiddenLibraryIds":[]}`, 200))
	if prefs.HiddenLibraryIDs == nil || len(prefs.HiddenLibraryIDs) != 0 {
		t.Fatalf("clear hidden libraries: %+v", prefs)
	}
	assertBody("/api/v1/collections/"+collection.ID, "series-hidden", `"filtered":true`)

	f.request(t, "owner", "PUT", "/admin/api/me/preferences", `{"hiddenLibraryIds":["hidden"]}`, 200)
	if body := f.request(t, "owner", "GET", "/api/v1/libraries", "", 200).Body.String(); strings.Contains(body, "Catalog hidden") {
		t.Fatalf("admin personal catalog did not filter: %s", body)
	}
	adminStatus := decodeV33[map[string]any](t, f.request(t, "owner", "GET", "/admin/api/status", "", 200))
	if adminStatus["libraries"] != float64(3) {
		t.Fatalf("admin global stats were affected by personal preferences: %+v", adminStatus)
	}
	if body := f.request(t, "owner", "GET", "/api/agent/v1/libraries?include_hidden=false", "", 200).Body.String(); !strings.Contains(body, "Catalog hidden") {
		t.Fatalf("admin global library management did not include personal hidden library: %s", body)
	}
}

func TestV33NSFWCompatibilityEndpoints(t *testing.T) {
	f := newV33HTTPFixture(t, false, true)
	ctx := context.Background()
	f.library(t, "main", "Main", true)
	f.user(t, "reader", true, "main")
	f.user(t, "restricted", false, "main")
	check := func(want bool) {
		t.Helper()
		config := decodeV33[map[string]any](t, f.request(t, "owner", "GET", "/admin/api/auth/status", "", 200))
		if config["showNsfw"] != want || f.store.BoolSetting(ctx, "show_nsfw", !want) != want {
			t.Fatalf("showNsfw: auth=%v setting=%v want=%v", config["showNsfw"], f.store.ShowNSFW(ctx), want)
		}
	}
	const settingsURL = "/admin/api/settings"
	const agentSettingsURL = "/api/agent/v1/settings"
	const configURL = "/admin/api/bangumi/config"

	f.request(t, "owner", "PUT", settingsURL, `{"show_nsfw":"true"}`, 200)
	check(true)
	f.request(t, "owner", "PUT", settingsURL, `{"showNsfw":false}`, 200)
	check(false)
	f.request(t, "owner", "PATCH", agentSettingsURL, `{"showNsfw":true}`, 200)
	check(true)
	f.request(t, "owner", "PATCH", agentSettingsURL, `{"show_nsfw":"false"}`, 200)
	check(false)

	for _, tc := range []struct {
		payload string
		want    bool
	}{
		{`{"showNsfw":true}`, true},
		{`{"show_nsfw":false}`, false},
		{`{"show_nsfw":true}`, true},
		{`{}`, true},
	} {
		config := decodeV33[map[string]any](t, f.request(t, "owner", "PUT", configURL, tc.payload, 200))
		if config["showNsfw"] != tc.want {
			t.Fatalf("%s response showNsfw=%v want=%v", tc.payload, config["showNsfw"], tc.want)
		}
		check(tc.want)
	}
	f.request(t, "owner", "PUT", settingsURL, `{"page_prefetch_count":"4"}`, 200)
	f.request(t, "owner", "PATCH", agentSettingsURL, `{"page_prefetch_count":5}`, 200)
	check(true)
	for _, tc := range []struct{ method, path string }{
		{"PUT", settingsURL}, {"PUT", configURL}, {"PATCH", agentSettingsURL},
	} {
		f.request(t, "reader", tc.method, tc.path, `{"showNsfw":false}`, 403)
	}
	f.request(t, "reader", "PUT", "/admin/api/me/preferences", `{"showNsfw":false}`, 200)
	check(true)
	f.request(t, "restricted", "PUT", "/admin/api/me/preferences", `{"showNsfw":true,"hiddenLibraryIds":["main"]}`, 403)
	prefs := decodeV33[database.UserPreferences](t, f.request(t, "restricted", "PUT", "/admin/api/me/preferences", `{}`, 200))
	if prefs.AllowNSFW || prefs.ShowNSFW || prefs.HiddenLibraryIDs == nil || len(prefs.HiddenLibraryIDs) != 0 {
		t.Fatalf("rejected combined update changed preferences: %+v", prefs)
	}
}

func TestV33BookshelfReadingActivityHTTP(t *testing.T) {
	f := newV33HTTPFixture(t, false, false)
	for _, id := range []string{"first", "second", "hidden"} {
		f.library(t, id, id, true)
	}
	f.book(t, "first", "first-edition", "yesterday", false)
	f.book(t, "first", "first-edition", "first-volume", false)
	f.book(t, "second", "second-edition", "second-volume", false)
	f.book(t, "second", "second-edition", "finished", false)
	f.book(t, "first", "oneshot", "oneshot-volume", false)
	f.book(t, "hidden", "hidden-edition", "hidden-volume", false)
	f.exec(t, `UPDATE series SET one_shot=1 WHERE id='oneshot'`)
	f.exec(t, `INSERT INTO series_metadata(series_id,bgm_subject_id,title,updated_at)
SELECT id,765,'Title for ' || id,'2026-01-01T00:00:00Z' FROM series`)
	reader := f.user(t, "reader", false, "first", "second", "hidden")
	other := f.user(t, "other", false, "first", "second", "hidden")
	f.request(t, "reader", "PUT", "/admin/api/me/preferences", `{"hiddenLibraryIds":["hidden"]}`, 200)
	f.progress(t, reader.ID, "yesterday", "2026-02-01T15:59:59.999999999Z", false)
	f.progress(t, reader.ID, "first-volume", "2026-02-01T16:00:00Z", false)
	f.progress(t, reader.ID, "second-volume", "2026-02-01T16:10:00Z", false)
	f.progress(t, reader.ID, "finished", "2026-02-01T16:20:00Z", true)
	f.progress(t, reader.ID, "oneshot-volume", "2026-02-01T16:05:00Z", false)
	f.progress(t, reader.ID, "hidden-volume", "2026-02-01T16:30:00Z", false)
	f.progress(t, other.ID, "second-volume", "2026-03-15T10:00:00Z", false)

	const historyURL = "/admin/api/bookshelf/history"
	const continueURL = "/admin/api/bookshelf/continue-reading"
	f.request(t, "", "GET", historyURL, "", 401)
	f.request(t, "", "GET", continueURL, "", 401)

	type book struct {
		ID           string
		BgmSubjectID int64
		SeriesTitle  string
		ReadProgress struct {
			Page      int
			Completed bool
		}
	}
	type activityPage struct {
		Content []struct {
			DayKey, GroupKey, SeriesTitle string
			MergedByBangumi               bool
			BgmSubjectID                  int64
			OtherInProgressCount          int
			Primary                       book
			Others                        []book
		}
		Page, Size, TotalElements, TotalBooks, TotalPages int
	}
	get := func(user, path string) activityPage {
		t.Helper()
		return decodeV33[activityPage](t, f.request(t, user, "GET", path, "", 200))
	}
	continued := get("reader", continueURL+"?merge_bangumi=1&size=0")
	if continued.TotalElements != 2 || len(continued.Content) != 1 {
		t.Fatalf("continue-reading does not count groups or clamp size: %+v", continued)
	}
	if g := continued.Content[0]; g.GroupKey != "bgm:765" || !g.MergedByBangumi || g.BgmSubjectID != 765 ||
		g.OtherInProgressCount != 2 || g.Primary.ID != "second-volume" || g.Primary.BgmSubjectID != 765 ||
		g.Primary.ReadProgress.Completed || g.Primary.ReadProgress.Page != 1 ||
		len(g.Others) != 2 || g.Others[0].ID != "first-volume" || g.Others[1].ID != "yesterday" {
		t.Fatalf("continue-reading grouped DTO: %+v", g)
	}
	bySeries := get("reader", continueURL)
	if bySeries.TotalElements != 3 || bySeries.Content[0].GroupKey != "series:second-edition" ||
		bySeries.Content[1].GroupKey != "series:oneshot" || bySeries.Content[1].MergedByBangumi ||
		bySeries.Content[1].Others == nil || len(bySeries.Content[1].Others) != 0 {
		t.Fatalf("continue-reading per-series DTO: %+v", bySeries)
	}

	historyQuery := historyURL + "?merge_bangumi=true&tz_offset=480&size=1"
	history := get("reader", historyQuery)
	if history.TotalElements != 3 || history.TotalBooks != 5 || history.TotalPages != 3 || history.Page != 0 ||
		history.Size != 1 || len(history.Content) != 1 {
		t.Fatalf("history pagination: %+v", history)
	}
	if g := history.Content[0]; g.DayKey != "2026-02-02" || g.GroupKey != "bgm:765" ||
		g.SeriesTitle != "Title for second-edition" || g.Primary.SeriesTitle != g.SeriesTitle ||
		g.Primary.ID != "finished" || !g.Primary.ReadProgress.Completed || len(g.Others) != 2 ||
		g.Others[0].ID != "second-volume" || g.Others[1].ID != "first-volume" {
		t.Fatalf("history grouped DTO: %+v", g)
	}
	p2 := get("reader", historyQuery+"&page=2")
	if len(p2.Content) != 1 || p2.Content[0].DayKey != "2026-02-01" || p2.Content[0].Primary.ID != "yesterday" {
		t.Fatalf("local midnight boundary: %+v", p2)
	}
	beyond := get("reader", historyQuery+"&page=999")
	if beyond.Content == nil || len(beyond.Content) != 0 || beyond.TotalElements != 3 || beyond.TotalBooks != 5 {
		t.Fatalf("out-of-range history result: %+v", beyond)
	}
	search := get("reader", historyURL+"?search=first-volume&size=999&tz_offset=-9999&page=-1")
	if search.Size != 100 || search.Page != 0 || search.TotalElements != 1 || len(search.Content) != 1 ||
		search.Content[0].DayKey != "2026-02-01" || search.Content[0].Primary.ID != "first-volume" {
		t.Fatalf("history search and input bounds: %+v", search)
	}
	utc := get("reader", historyURL+"?merge_bangumi=true")
	if utc.Size != 30 || utc.TotalElements != 2 || utc.Content[0].DayKey != "2026-02-01" {
		t.Fatalf("history UTC defaults: %+v", utc)
	}
	isolated := get("other", historyURL)
	if isolated.TotalBooks != 1 || isolated.TotalElements != 1 ||
		isolated.Content[0].DayKey != "2026-03-15" || isolated.Content[0].Primary.ID != "second-volume" {
		t.Fatalf("another user's activity was included: %+v", isolated)
	}
}

func TestV33KomgaPatchReadProgressHTTP(t *testing.T) {
	// A nil archive service ensures the PATCH can only use SQLite metadata.
	f := newV33HTTPFixture(t, false, false)
	ctx := context.Background()
	f.library(t, "member", "Member", true)
	f.library(t, "main", "Main", true)
	f.book(t, "main", "series", "indexed", false)
	f.book(t, "main", "series", "unindexed", false)
	f.book(t, "member", "source", "replica", false)
	f.exec(t, `UPDATE libraries SET provider_type='union' WHERE id='main'`)
	f.exec(t, `INSERT INTO union_layers(union_library_id,member_library_id,priority,created_at) VALUES('main','member',0,'2026-01-01T00:00:00Z')`)
	f.exec(t, `INSERT INTO union_book_sources(union_book_id,priority,source_book_id,source_library_id,source_size)
VALUES('indexed',0,'replica','member',100)`)
	if err := f.store.RecordArchiveIndexBuild(ctx, "indexed", "test", "[]", 20, 0); err != nil {
		t.Fatal(err)
	}
	reader := f.user(t, "reader", false, "main")
	scope := database.WithUserScope(ctx, reader.Scope())
	check := func(id string, done bool, page int) {
		t.Helper()
		got, found, err := f.store.BookReadProgress(scope, id)
		if err != nil || !found || got.Completed != done || (got.CompletedAt != nil) != done ||
			((page == 0 && got.Page != nil) || (page != 0 && (got.Page == nil || *got.Page != page))) {
			t.Fatalf("%s unexpected progress %+v, found=%v err=%v", id, got, found, err)
		}
	}
	for _, tc := range []struct {
		input string
		done  bool
		page  int
	}{
		{`{"page":19}`, false, 19}, {`{"page":20}`, true, 20}, {`{"page":200}`, true, 20},
		{`{"page":4}`, false, 4}, {`{"completed":true}`, true, 20},
		{`{"completed":false,"page":200}`, false, 20}, {`{"completed":true,"page":4}`, true, 4},
		{`{"completed":false}`, false, 4},
	} {
		f.request(t, "reader", "PATCH", "/api/v1/books/indexed/read-progress", tc.input, 204)
		check("indexed", tc.done, tc.page)
		check("replica", tc.done, tc.page)
	}
	for _, body := range []string{`{}`, `{"page":0}`, `{"page":-2}`} {
		f.request(t, "reader", "PATCH", "/api/v1/books/indexed/read-progress", body, 400)
	}
	check("indexed", false, 4)

	for _, tc := range []struct {
		input string
		done  bool
		page  int
	}{
		{`{"completed":true}`, true, 0},
		{`{"page":150}`, false, 150},
		{`{"completed":true}`, true, 150},
		{`{"completed":false}`, false, 150},
		{`{"page":2,"completed":true}`, true, 2},
		{`{"page":2}`, false, 2},
	} {
		f.request(t, "reader", "PATCH", "/api/v1/books/unindexed/read-progress", tc.input, 204)
		check("unindexed", tc.done, tc.page)
		if tc.page == 0 {
			dto := decodeV33[map[string]any](t, f.request(t, "reader", "GET", "/api/v1/books/unindexed", "", 200))
			if dto["readProgress"].(map[string]any)["page"] != float64(1) {
				t.Fatalf("unindexed completion page fallback: %+v", dto["readProgress"])
			}
		}
	}
	if _, found, err := f.store.ArchiveIndexRaw(ctx, "unindexed"); err != nil || found {
		t.Fatalf("PATCH created an index: found=%v err=%v", found, err)
	}
	if _, found, err := f.store.BookReadProgress(ctx, "indexed"); err != nil || found {
		t.Fatalf("PATCH wrote to the wrong user: found=%v err=%v", found, err)
	}
}
