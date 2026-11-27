package httpserver

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/archive"
	"github.com/xJogger/fake-komga-115/internal/cache"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/extmeta"
	"github.com/xJogger/fake-komga-115/internal/maintenance"
	"github.com/xJogger/fake-komga-115/internal/oneonefive"
	"github.com/xJogger/fake-komga-115/internal/thumbnail"
)

type adminV34Fixture struct {
	v33HTTPFixture
	archives *archive.Service
	thumbs   *thumbnail.Service
	tasks    *maintenance.Manager
}

func newAdminV34Fixture(t *testing.T) adminV34Fixture {
	t.Helper()
	f := newV33HTTPFixture(t, false, false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := oneonefive.New(f.store, logger)
	cacheMgr, err := cache.New(f.store, filepath.Join(f.dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	archives := archive.NewService(f.store, client, cacheMgr, logger)
	t.Cleanup(archives.Close)
	archives.SetComicInfoParser(parseAdminV34ComicInfo)
	thumbs, err := thumbnail.New(f.store, filepath.Join(f.dir, "covers"), logger)
	if err != nil {
		t.Fatal(err)
	}
	tasks := maintenance.New(f.store, archives, thumbs, logger)
	t.Cleanup(tasks.Close)
	f.handler = New(f.store, client, nil, cacheMgr, archives, thumbs, nil, tasks, RuntimeInfo{}, logger).Handler()
	return adminV34Fixture{f, archives, thumbs, tasks}
}

func parseAdminV34ComicInfo(reader io.Reader) (database.BookScrapedMetadata, database.SeriesComicInfoBackfill, error) {
	meta, err := extmeta.ParseComicInfoXML(reader)
	if err != nil {
		return database.BookScrapedMetadata{}, database.SeriesComicInfoBackfill{}, err
	}
	return database.BookScrapedMetadata{
		Title: meta.Title, Number: meta.Number, NumberSort: meta.NumberSort,
		SeriesGroup: meta.SeriesGroup, StoryArc: meta.StoryArc, StoryArcNumber: meta.StoryArcNumber,
	}, database.SeriesComicInfoBackfill{Title: meta.Series, Summary: meta.Summary}, nil
}

func (f adminV34Fixture) comicArchive(t *testing.T, library, series, book string) {
	t.Helper()
	f.book(t, library, series, book, false)
	var raw bytes.Buffer
	zw := zip.NewWriter(&raw)
	for _, file := range []struct {
		name string
		data []byte
	}{
		{"1.png", makeTestPNGBytes(t, 20, 40, 190)},
		{"ComicInfo.xml", []byte(`<ComicInfo><Title>Embedded volume</Title><Series>Embedded series</Series><Number>2.5</Number><SeriesGroup>Group</SeriesGroup><StoryArc>Arc</StoryArc><StoryArcNumber>3</StoryArcNumber></ComicInfo>`)},
	} {
		entry, err := zw.Create(file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.dir, library, series)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, book+".cbz"), raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	f.exec(t, "UPDATE books SET size=? WHERE id=?", raw.Len(), book)
}

func (f adminV34Fixture) jsonRequest(t *testing.T, user, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if user != "" {
		req.SetBasicAuth(user, v33TestPassword)
	}
	return f.serve(t, req, status)
}

func (f adminV34Fixture) coverUpload(t *testing.T, user, book, field string, raw []byte, status int) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("description", "Cover upload"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile(field, "cover.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/books/"+book+"/cover", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.SetBasicAuth(user, v33TestPassword)
	return f.serve(t, req, status)
}

func assertAdminV34Fields(t *testing.T, got, want map[string]any) {
	t.Helper()
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %#v, want %#v", key, got[key], value)
		}
	}
}

func (f adminV34Fixture) assertCoverPathsPrivate(t *testing.T, rec *httptest.ResponseRecorder, paths ...string) {
	t.Helper()
	body := rec.Body.String()
	for _, value := range append(paths, "customCoverPath", "extCoverPath", "bgmCoverPath", f.dir) {
		if value != "" && strings.Contains(body, value) {
			t.Fatal("cover response contains a local path or path field")
		}
	}
}

func TestAdminV34SeriesOneShot(t *testing.T) {
	f := newAdminV34Fixture(t)
	f.library(t, "catalog", "Catalog", true)
	f.book(t, "catalog", "single", "single-book", false)
	f.book(t, "catalog", "multi", "multi-one", false)
	f.book(t, "catalog", "multi", "multi-two", false)
	f.book(t, "catalog", "file-backed", "file-book", false)
	f.exec(t, "UPDATE series SET one_shot=1 WHERE id='file-backed'")
	f.user(t, "reader", false, "catalog")

	const single = "/admin/api/series/single/"
	const enable = `{"oneShotOverride":true}`
	const disable = `{"oneShotOverride":false}`
	f.jsonRequest(t, "", http.MethodPatch, single+"oneshot", enable, http.StatusUnauthorized)
	f.jsonRequest(t, "reader", http.MethodPatch, single+"oneshot", enable, http.StatusForbidden)
	f.jsonRequest(t, "owner", http.MethodPatch, single+"oneshot", `{}`, http.StatusBadRequest)
	f.jsonRequest(t, "owner", http.MethodPatch, "/admin/api/series/multi/oneshot", enable, http.StatusBadRequest)
	f.jsonRequest(t, "owner", http.MethodPatch, "/admin/api/series/file-backed/oneshot", enable, http.StatusBadRequest)
	f.jsonRequest(t, "owner", http.MethodPatch, "/admin/api/series/missing/oneshot", enable, http.StatusNotFound)
	for _, tc := range []struct {
		body string
		want bool
	}{{enable, true}, {disable, false}} {
		result := decodeV33[map[string]any](t, f.jsonRequest(t, "owner", http.MethodPatch, single+"oneshot", tc.body, http.StatusOK))
		assertAdminV34Fields(t, result, map[string]any{
			"seriesId": "single", "oneShot": false, "oneShotOverride": tc.want,
			"effectiveOneShot": tc.want, "canToggleOneShot": true,
		})
		detail := decodeV33[map[string]any](t, f.request(t, "reader", http.MethodGet, single+"detail", "", http.StatusOK))
		assertAdminV34Fields(t, detail, map[string]any{
			"fileBackedOneShot": false, "oneShotOverride": tc.want,
			"effectiveOneShot": tc.want, "canToggleOneShot": true,
		})
		dto := decodeV33[map[string]any](t, f.request(t, "reader", http.MethodGet, "/api/v1/series/single", "", http.StatusOK))
		assertAdminV34Fields(t, dto, map[string]any{"oneshot": tc.want})
	}

	session, _, err := f.store.CreateSessionForUser(t.Context(), database.PrimaryAdminUserID, "owner", false, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		origin, userID, code string
		status               int
	}{
		{"https://cross-origin.example.invalid", database.PrimaryAdminUserID, "CSRF_REJECTED", http.StatusForbidden},
		{"http://example.com", "different-user", "USER_CONTEXT_MISMATCH", http.StatusConflict},
	} {
		req := httptest.NewRequest(http.MethodPatch, single+"oneshot", strings.NewReader(enable))
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("X-FK115-User-Id", tc.userID)
		rec := f.serve(t, req, tc.status)
		assertAdminV34Fields(t, decodeV33[map[string]any](t, rec), map[string]any{"error": tc.code})
	}
}

func TestAdminV34BookCustomCover(t *testing.T) {
	f := newAdminV34Fixture(t)
	f.library(t, "catalog", "Catalog", true)
	f.book(t, "catalog", "series", "book", false)
	f.user(t, "reader", false, "catalog")
	f.user(t, "unassigned", false)
	const cover = "/admin/api/books/book/cover"
	const detail = "/admin/api/books/book/detail"
	const series = "/admin/api/series/series/detail"
	const thumbnailPath = "/api/v1/books/book/thumbnail"
	const coverURL = "https://covers.example.invalid/volume.png"
	const urlBody = `{"url":"` + coverURL + `"}`

	book, err := f.store.BookByID(t.Context(), "book")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.thumbs.GenerateBookThumbnail(t.Context(), book, makeTestPNGBytes(t, 10, 40, 210)); err != nil {
		t.Fatal(err)
	}
	fallback := f.request(t, "reader", http.MethodGet, thumbnailPath, "", http.StatusOK).Body.Bytes()
	upload := makeTestPNGBytes(t, 220, 20, 10)
	f.coverUpload(t, "reader", "book", "file", upload, http.StatusForbidden)
	f.coverUpload(t, "owner", "book", "wrong-field", upload, http.StatusBadRequest)
	f.coverUpload(t, "owner", "book", "file", []byte("not an image"), http.StatusBadRequest)
	f.coverUpload(t, "owner", "book", "file", make([]byte, (10<<20)+1), http.StatusRequestEntityTooLarge)
	f.jsonRequest(t, "reader", http.MethodPost, cover, urlBody, http.StatusForbidden)
	f.jsonRequest(t, "owner", http.MethodPost, "/admin/api/books/missing/cover", urlBody, http.StatusNotFound)
	for _, badURL := range []string{`""`, `"file:///cover.png"`, `"https:///cover.png"`, `"/cover.png"`, `"https://user@example.invalid/cover.png"`} {
		f.jsonRequest(t, "owner", http.MethodPost, cover, `{"url":`+badURL+`}`, http.StatusBadRequest)
	}
	exactLimit := make([]byte, 10<<20)
	copy(exactLimit, upload)
	rec := f.coverUpload(t, "owner", "book", "file", exactLimit, http.StatusOK)
	uploadedMeta, found, err := f.store.BookMetadataByID(t.Context(), book.ID)
	if err != nil || !found || uploadedMeta.CustomCoverPath == "" {
		t.Fatalf("uploaded cover missing from database: found=%v, err=%v", found, err)
	}
	f.assertCoverPathsPrivate(t, rec, uploadedMeta.CustomCoverPath)
	assertAdminV34Fields(t, decodeV33[map[string]any](t, rec), map[string]any{
		"bookId": "book", "coverSource": "auto", "hasCustomCover": true, "customCoverUrl": "",
	})
	f.assertCoverPathsPrivate(t, f.request(t, "owner", http.MethodGet, detail, "", http.StatusOK), uploadedMeta.CustomCoverPath)
	thumb := f.request(t, "reader", http.MethodGet, thumbnailPath, "", http.StatusOK)
	if thumb.Header().Get("Cache-Control") != "private, no-store" || thumb.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("unexpected cover response headers: %v", thumb.Header())
	}
	img, _, err := image.Decode(bytes.NewReader(thumb.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	r, _, b, _ := img.At(0, 0).RGBA()
	if r <= b || bytes.Equal(thumb.Body.Bytes(), fallback) {
		t.Fatal("thumbnail did not contain the uploaded cover")
	}
	f.request(t, "unassigned", http.MethodGet, detail, "", http.StatusNotFound)
	f.request(t, "unassigned", http.MethodGet, series, "", http.StatusNotFound)
	f.request(t, "unassigned", http.MethodGet, thumbnailPath, "", http.StatusNotFound)
	f.request(t, "reader", http.MethodDelete, cover, "", http.StatusForbidden)
	f.request(t, "owner", http.MethodDelete, "/admin/api/books/missing/cover", "", http.StatusNotFound)

	f.jsonRequest(t, "owner", http.MethodPatch, cover+"-source", `{"coverSource":"custom"}`, http.StatusOK)
	assertRestored := func() {
		t.Helper()
		if rec := f.request(t, "owner", http.MethodDelete, cover, "", http.StatusNoContent); rec.Body.Len() != 0 {
			t.Fatal("cover deletion returned a response body")
		}
		if thumb := f.request(t, "reader", http.MethodGet, thumbnailPath, "", http.StatusOK); !bytes.Equal(fallback, thumb.Body.Bytes()) {
			t.Fatal("deleting the custom cover did not restore the cached page fallback")
		}
	}
	assertRestored()
	rec = f.jsonRequest(t, "owner", http.MethodPost, cover, urlBody, http.StatusOK)
	f.assertCoverPathsPrivate(t, rec)
	assertAdminV34Fields(t, decodeV33[map[string]any](t, rec), map[string]any{
		"bookId": "book", "coverSource": "auto", "hasCustomCover": true, "customCoverUrl": coverURL,
	})
	extPath := filepath.Join(f.dir, "external-cover.jpg")
	bgmPath := filepath.Join(f.dir, "bangumi-cover.jpg")
	if err := f.store.UpsertBookScrapedMetadata(t.Context(), book.ID, database.BookScrapedMetadata{
		BgmSubjectID: 42, BgmCoverPath: bgmPath, ExtCoverPath: extPath,
		ExtSourceID: "fixture-source", ExtEntryID: "fixture-entry", MatchOrigin: "manual",
	}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"owner", "reader"} {
		rec := f.request(t, user, http.MethodGet, detail, "", http.StatusOK)
		f.assertCoverPathsPrivate(t, rec, extPath, bgmPath)
		rawMeta := decodeV33[struct {
			RawBookMeta map[string]any
		}](t, rec).RawBookMeta
		assertAdminV34Fields(t, rawMeta, map[string]any{
			"coverSource": "auto", "hasCustomCover": true, "hasExtCover": true, "bgmSubjectId": float64(42),
		})
		if user == "owner" {
			assertAdminV34Fields(t, rawMeta, map[string]any{"customCoverUrl": coverURL, "extSourceId": "fixture-source", "extEntryId": "fixture-entry", "matchOrigin": "manual"})
		} else {
			assertAdminV34Fields(t, rawMeta, map[string]any{"customCoverUrl": "", "extSourceId": "", "extEntryId": "", "matchOrigin": ""})
			for _, private := range []string{coverURL, "fixture-source", "fixture-entry"} {
				if strings.Contains(rec.Body.String(), private) {
					t.Fatal("reader received administrator cover metadata")
				}
			}
		}
	}
	// Remove the seeded test binding before checking the page fallback again.
	f.exec(t, "UPDATE book_metadata SET bgm_subject_id=0, bgm_cover_path='', ext_cover_path='' WHERE book_id=?", book.ID)
	assertRestored()
}

func TestAdminV34BookCoverSource(t *testing.T) {
	f := newAdminV34Fixture(t)
	f.library(t, "catalog", "Catalog", true)
	f.book(t, "catalog", "series", "book", false)
	f.user(t, "reader", false, "catalog")
	const path = "/admin/api/books/book/cover-source"
	const valid = `{"coverSource":"auto"}`
	f.jsonRequest(t, "reader", http.MethodPatch, path, valid, http.StatusForbidden)
	f.jsonRequest(t, "owner", http.MethodPatch, "/admin/api/books/missing/cover-source", valid, http.StatusNotFound)
	f.jsonRequest(t, "owner", http.MethodPatch, path, `{"coverSource":"unknown"}`, http.StatusBadRequest)
	for _, source := range []string{"auto", "custom", "ext", "bangumi", "book"} {
		rec := f.jsonRequest(t, "owner", http.MethodPatch, path, `{"coverSource":"`+source+`"}`, http.StatusOK)
		assertAdminV34Fields(t, decodeV33[map[string]any](t, rec), map[string]any{"bookId": "book", "coverSource": source})
		bm, _, err := f.store.BookMetadataByID(t.Context(), "book")
		if err != nil || bm.CoverSource != source {
			t.Fatalf("cover source not persisted: %q, err=%v", bm.CoverSource, err)
		}
	}
}

func TestAdminV34BookCustomCoverActivatesFromExternal(t *testing.T) {
	f := newAdminV34Fixture(t)
	f.library(t, "catalog", "Catalog", true)
	f.book(t, "catalog", "series", "book", false)
	f.user(t, "reader", false, "catalog")
	const cover = "/admin/api/books/book/cover"
	const thumbnailPath = "/api/v1/books/book/thumbnail"
	const extURL = "https://covers.example.invalid/external.png"
	const customURL = "https://covers.example.invalid/custom.png"
	external := makeTestPNGBytes(t, 10, 20, 220)
	remoteCustom := makeTestPNGBytes(t, 20, 220, 10)
	f.thumbs.SetBookExternalCoverFetcher(func(_ context.Context, source, entry, _, url string) ([]byte, string, error) {
		switch {
		case source == "fixture-source" && entry == "fixture-entry" && url == extURL:
			return external, "image/png", nil
		case source == "" && entry == "book" && url == customURL:
			return remoteCustom, "image/png", nil
		default:
			return nil, "", os.ErrNotExist
		}
	})
	if err := f.store.UpsertBookScrapedMetadata(t.Context(), "book", database.BookScrapedMetadata{
		ExtSourceID: "fixture-source", ExtEntryID: "fixture-entry", ExtCoverURL: extURL,
	}); err != nil {
		t.Fatal(err)
	}
	pickExternal := func(t *testing.T) {
		t.Helper()
		f.jsonRequest(t, "owner", http.MethodPatch, cover+"-source", `{"coverSource":"ext"}`, http.StatusOK)
		thumb := f.request(t, "reader", http.MethodGet, thumbnailPath, "", http.StatusOK)
		if !bytes.Equal(thumb.Body.Bytes(), external) {
			t.Fatal("external cover was not active")
		}
	}
	pickExternal(t)
	upload := makeTestPNGBytes(t, 220, 20, 10)
	rec := f.coverUpload(t, "owner", "book", "file", upload, http.StatusOK)
	assertAdminV34Fields(t, decodeV33[map[string]any](t, rec), map[string]any{
		"coverSource": "custom", "hasCustomCover": true,
	})
	thumb := f.request(t, "reader", http.MethodGet, thumbnailPath, "", http.StatusOK)
	img, _, err := image.Decode(bytes.NewReader(thumb.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	r, _, b, _ := img.At(0, 0).RGBA()
	if r <= b {
		t.Fatal("thumbnail did not switch to the uploaded cover")
	}
	f.jsonRequest(t, "owner", http.MethodPatch, cover+"-source", `{"coverSource":"ext"}`, http.StatusOK)
	thumb = f.request(t, "reader", http.MethodGet, thumbnailPath, "", http.StatusOK)
	if bytes.Equal(thumb.Body.Bytes(), external) {
		t.Fatal("explicit external cover displaced the custom upload")
	}
	rec = f.jsonRequest(t, "owner", http.MethodPost, cover, `{"url":"`+customURL+`"}`, http.StatusOK)
	assertAdminV34Fields(t, decodeV33[map[string]any](t, rec), map[string]any{
		"coverSource": "custom", "hasCustomCover": true, "customCoverUrl": customURL,
	})
	thumb = f.request(t, "reader", http.MethodGet, thumbnailPath, "", http.StatusOK)
	if !bytes.Equal(thumb.Body.Bytes(), remoteCustom) || thumb.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("thumbnail did not switch to the URL cover with private caching")
	}
}

func TestAdminV34BookComicInfoRefreshAndRichDetail(t *testing.T) {
	f := newAdminV34Fixture(t)
	f.library(t, "catalog", "Catalog", true)
	f.comicArchive(t, "catalog", "series", "book")
	f.book(t, "catalog", "series", "other", false)
	f.user(t, "reader", false, "catalog")
	const bookDetail = "/admin/api/books/book/detail"
	const seriesDetail = "/admin/api/series/series/detail"
	const refresh = "/admin/api/books/book/comicinfo/refresh"
	getBook := func() struct {
		RawBookMeta map[string]any
		Book        struct{ Metadata map[string]any }
	} {
		t.Helper()
		return decodeV33[struct {
			RawBookMeta map[string]any
			Book        struct{ Metadata map[string]any }
		}](t, f.request(t, "owner", http.MethodGet, bookDetail, "", http.StatusOK))
	}
	assertAdminV34Fields(t, getBook().RawBookMeta, map[string]any{
		"coverSource": "auto", "hasCustomCover": false, "customCoverUrl": "", "bgmSubjectId": float64(0),
		"extSourceId": "", "extEntryId": "", "hasExtCover": false, "seriesGroup": "", "storyArc": "",
		"storyArcNumber": "", "comicInfoStatus": "", "matchOrigin": "", "numberSortExplicit": false, "updatedAt": "",
	})
	f.request(t, "reader", http.MethodGet, seriesDetail, "", http.StatusOK)
	if _, found, err := f.store.BookMetadataByID(t.Context(), "book"); err != nil || found {
		t.Fatalf("detail reads cached ComicInfo implicitly: found=%v, err=%v", found, err)
	}
	f.jsonRequest(t, "reader", http.MethodPost, refresh, "", http.StatusForbidden)
	f.jsonRequest(t, "owner", http.MethodPost, "/admin/api/books/missing/comicinfo/refresh", "", http.StatusNotFound)
	f.jsonRequest(t, "owner", http.MethodPost, refresh, `{"force":"true"}`, http.StatusBadRequest)
	book, err := f.store.BookByID(t.Context(), "book")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetBookComicInfoSentinel(t.Context(), book.ID, archive.BookVersion(book), archive.ComicInfoAbsent); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ body, status string }{
		{`{"force":false}`, archive.ComicInfoAbsent},
		{"", archive.ComicInfoParsed},
	} {
		rec := f.jsonRequest(t, "owner", http.MethodPost, refresh, tc.body, http.StatusOK)
		assertAdminV34Fields(t, decodeV33[map[string]any](t, rec), map[string]any{"bookId": book.ID, "status": tc.status})
	}
	detail := getBook()
	assertAdminV34Fields(t, detail.Book.Metadata, map[string]any{"title": "Embedded volume", "numberSort": 2.5})
	assertAdminV34Fields(t, detail.RawBookMeta, map[string]any{
		"comicInfoStatus": archive.ComicInfoParsed, "seriesGroup": "Group",
		"storyArc": "Arc", "storyArcNumber": "3", "numberSortExplicit": true,
	})
	if _, err := time.Parse(time.RFC3339, detail.RawBookMeta["updatedAt"].(string)); err != nil {
		t.Fatal(err)
	}
	rec := f.request(t, "reader", http.MethodGet, seriesDetail, "", http.StatusOK)
	series := decodeV33[struct {
		Series     struct{ Metadata map[string]any }
		BookExtras map[string]map[string]any
	}](t, rec)
	assertAdminV34Fields(t, series.Series.Metadata, map[string]any{"title": "Embedded series"})
	assertAdminV34Fields(t, series.BookExtras["book"], map[string]any{"comicInfoStatus": archive.ComicInfoParsed, "numberSortExplicit": true})
	assertAdminV34Fields(t, series.BookExtras["other"], map[string]any{
		"coverSource": "auto", "hasCustomCover": false, "hasExtCover": false,
		"bgmSubjectId": float64(0), "comicInfoStatus": "", "numberSortExplicit": false,
	})
}

const adminV34MaintenancePath = "/admin/api/maintenance-jobs"

func (f adminV34Fixture) waitMaintenance(t *testing.T, id string) maintenance.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		run, err := f.tasks.Run(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != "queued" && run.Status != "running" {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("maintenance job did not finish: %+v", run)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAdminV34BatchMaintenance(t *testing.T) {
	f := newAdminV34Fixture(t)
	f.library(t, "hidden", "Hidden", false)
	f.user(t, "reader", false, "hidden")
	for _, tc := range []struct {
		operation, target, idField, option string
		count                              int
	}{
		{"batch_book_covers", "series", "targetId", "fallbackPage1", 1},
		{"batch_book_covers", "library", "libraryId", "fallbackPage1", 2},
		{"batch_comicinfo", "series", "seriesId", "extractPage1", 1},
		{"batch_comicinfo", "library", "targetId", "extractPage1", 2},
	} {
		t.Run(tc.operation+"/"+tc.target, func(t *testing.T) {
			library := tc.operation + "-" + tc.target
			targetID := library
			if tc.target == "series" {
				targetID = library + "-one"
			}
			f.library(t, library, "Catalog", true)
			books := []string{library + "-one"}
			if tc.count == 2 {
				books = append(books, library+"-two")
			}
			for _, id := range books {
				f.comicArchive(t, library, id, id)
			}
			f.user(t, library, false, library)
			extractsComicInfo := tc.operation == "batch_comicinfo"
			force := extractsComicInfo && tc.target == "library"
			if force {
				for _, id := range books {
					book, err := f.store.BookByID(t.Context(), id)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.store.SetBookComicInfoSentinel(t.Context(), id, archive.BookVersion(book), archive.ComicInfoAbsent); err != nil {
						t.Fatal(err)
					}
				}
			}
			body, err := json.Marshal(map[string]any{
				"operation": tc.operation, "targetType": tc.target, tc.idField: targetID, tc.option: true, "force": force,
			})
			if err != nil {
				t.Fatal(err)
			}
			f.jsonRequest(t, library, http.MethodPost, adminV34MaintenancePath, string(body), http.StatusForbidden)
			rec := f.jsonRequest(t, "owner", http.MethodPost, adminV34MaintenancePath, string(body), http.StatusAccepted)
			queued := decodeV33[maintenance.Run](t, rec)
			if queued.Status != "queued" || queued.Operation != tc.operation || queued.TargetType != tc.target ||
				queued.TargetID != targetID || queued.TotalItems != tc.count || queued.Force != force {
				t.Fatalf("unexpected queued job: %+v", queued)
			}
			run := f.waitMaintenance(t, queued.ID)
			if run.Status != "success" || run.ProcessedItems != tc.count || run.GeneratedCount != tc.count {
				t.Fatalf("maintenance job did not generate the expected results: %+v", run)
			}
			for _, id := range books {
				book, err := f.store.BookByID(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				if ok, err := f.thumbs.HasBookThumbnail(t.Context(), book); err != nil || !ok {
					t.Fatalf("page-one thumbnail missing for %s: %v", id, err)
				}
				meta, present, err := f.store.BookMetadataByID(t.Context(), id)
				if err != nil || (extractsComicInfo && (!present || meta.ComicInfoStatus != archive.ComicInfoParsed)) ||
					(!extractsComicInfo && meta.ComicInfoStatus != "") {
					t.Fatalf("unexpected ComicInfo status for %s: %q, found=%v, err=%v", id, meta.ComicInfoStatus, present, err)
				}
			}
		})
	}
	for _, tc := range []struct {
		body, code string
		status     int
	}{
		{`{"operation":"unknown","targetType":"series","targetId":"missing"}`, "INVALID_OPERATION", http.StatusBadRequest},
		{`{"operation":"batch_comicinfo","targetType":"book","targetId":"missing"}`, "INVALID_TARGET", http.StatusBadRequest},
		{`{"operation":"batch_comicinfo","targetType":"series"}`, "INVALID_TARGET", http.StatusBadRequest},
		{`{"operation":"batch_comicinfo","targetType":"series","seriesId":"missing"}`, "NOT_FOUND", http.StatusNotFound},
		{`{"operation":"batch_book_covers","targetType":"library","libraryId":"hidden"}`, "NOT_FOUND", http.StatusNotFound},
	} {
		rec := f.jsonRequest(t, "owner", http.MethodPost, adminV34MaintenancePath, tc.body, tc.status)
		assertAdminV34Fields(t, decodeV33[map[string]any](t, rec), map[string]any{"error": tc.code})
	}
}

func TestAdminV34MaintenanceDuplicateAndLegacy(t *testing.T) {
	f := newAdminV34Fixture(t)
	f.library(t, "catalog", "Catalog", true)
	f.comicArchive(t, "catalog", "series", "book")
	entered, release := make(chan struct{}), make(chan struct{})
	f.archives.SetComicInfoParser(func(reader io.Reader) (database.BookScrapedMetadata, database.SeriesComicInfoBackfill, error) {
		close(entered)
		<-release
		return parseAdminV34ComicInfo(reader)
	})
	defer close(release)
	const batch = `{"operation":"batch_comicinfo","targetType":"series","seriesId":"series"}`
	f.jsonRequest(t, "owner", http.MethodPost, adminV34MaintenancePath, batch, http.StatusAccepted)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("batch did not reach ComicInfo parser")
	}
	rec := f.jsonRequest(t, "owner", http.MethodPost, adminV34MaintenancePath, batch, http.StatusConflict)
	assertAdminV34Fields(t, decodeV33[map[string]any](t, rec), map[string]any{"error": "JOB_ALREADY_RUNNING"})

	for _, tc := range []struct {
		body, operation, targetType, targetID string
	}{
		{`{"operation":"book_index","bookId":"book","force":true}`, "book_index", "book", "book"},
		{`{"operation":"series_index","targetType":"series","seriesId":"series","force":true}`, "series_index", "series", "series"},
		{`{"operation":"series_thumbnail","seriesId":"series","force":true}`, "series_thumbnail", "series", "series"},
	} {
		rec := f.jsonRequest(t, "owner", http.MethodPost, adminV34MaintenancePath, tc.body, http.StatusAccepted)
		run := decodeV33[maintenance.Run](t, rec)
		if run.Status != "queued" || run.Operation != tc.operation || run.TargetType != tc.targetType || run.TargetID != tc.targetID || !run.Force {
			t.Fatalf("legacy maintenance request queued unexpected job: %+v", run)
		}
	}
}
