package bangumi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/database"
	"golang.org/x/time/rate"
)

func TestBangumiBearerOriginAndRedirects(t *testing.T) {
	for _, tc := range []struct {
		url     string
		allowed bool
	}{
		{"https://api.bgm.tv/v0/subjects/1", true},
		{"https://api.bgm.tv:443/v0/subjects/1", true},
		{"http://api.bgm.tv/v0/subjects/1", false},
		{"https://api.bgm.tv:1443/v0/subjects/1", false},
		{"https://api.bgm.tv.evil.example/v0/subjects/1", false},
		{"https://img.api.bgm.tv/v0/subjects/1", false},
		{"https://lain.bgm.tv/image.jpg", false},
		{"https://userinfo@api.bgm.tv/v0/subjects/1", false},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.url, nil)
		setBangumiBearer(req, "fixture-token")
		if got := req.Header.Get("Authorization"); (got == "Bearer fixture-token") != tc.allowed {
			t.Errorf("unexpected bearer policy for %s", tc.url)
		}
	}
	for _, tc := range []struct {
		name, location, start string
		code                  int
		wantRequests          int
	}{
		{"cdn", "https://lain.bgm.tv/cover.jpg", "https://api.bgm.tv/image", 302, 2},
		{"subdomain", "https://img.api.bgm.tv/cover.jpg", "https://api.bgm.tv/image", 302, 2},
		{"downgrade", "http://api.bgm.tv/cover.jpg", "https://api.bgm.tv/image", 302, 2},
		{"oauth form", "https://lain.bgm.tv/token", "https://bgm.tv/oauth/access_token", 307, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := newLiveHTTPClient(matchingTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				w := httptest.NewRecorder()
				if calls == 1 {
					w.Header().Set("Location", tc.location)
					w.WriteHeader(tc.code)
				} else if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("redirect carried credentials")
				}
				return w.Result(), nil
			}))
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, tc.start, strings.NewReader("fixture-form"))
			if err != nil {
				t.Fatal(err)
			}
			setBangumiBearer(req, "fixture-token")
			req.Header.Set("Cookie", "session=fixture-cookie")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if calls != tc.wantRequests {
				t.Fatalf("round trips = %d, want %d", calls, tc.wantRequests)
			}
		})
	}
}

func TestBangumiUnavailableCoverResponses(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNoContent, http.StatusFound, http.StatusOK} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			ctx := database.WithUserScope(t.Context(), database.UserScope{AllowNSFW: true, ShowNSFW: true})
			client := NewLiveClient()
			client.readToken = func(context.Context) string { return "fixture-token" }
			client.http = newLiveHTTPClient(matchingTransport(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				if r.URL.Host == "api.bgm.tv" {
					if r.Header.Get("Authorization") != "Bearer fixture-token" {
						t.Error("cover request did not authenticate")
					}
					if code == http.StatusFound {
						w.Header().Set("Location", "https://lain.bgm.tv/img/no_icon_subject.png")
					}
					w.WriteHeader(code)
				} else {
					if r.Header.Get("Authorization") != "" {
						t.Error("image host received API credentials")
					}
					_, _ = io.WriteString(w, "fixture-placeholder")
				}
				if code >= 400 {
					_, _ = io.WriteString(w, "fixture-token fixture-admin-profile")
				}
				resp := w.Result()
				resp.Request = r
				return resp, nil
			}))
			if _, err := client.FetchCoverBytes(ctx, "", 303); !errors.Is(err, ErrCoverUnavailable) ||
				strings.Contains(err.Error(), "fixture-") {
				t.Fatalf("unexpected or unsanitized cover failure: %v", err)
			}
		})
	}
}

type bangumiErrReader struct {
	err error
}

func (r bangumiErrReader) Read([]byte) (int, error) {
	return 0, r.err
}

func TestLiveClientFetchCoverRetriesAndPermanentFailures(t *testing.T) {
	ctx := database.WithUserScope(t.Context(), database.UserScope{AllowNSFW: true, ShowNSFW: true})

	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 522} {
		t.Run("transient_recover_"+http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			var waits []time.Duration
			client := NewLiveClient()
			client.coverLimiter = rate.NewLimiter(rate.Inf, 1)
			client.coverRetryWait = func(_ context.Context, d time.Duration) error {
				waits = append(waits, d)
				return nil
			}
			client.http = newLiveHTTPClient(matchingTransport(func(r *http.Request) (*http.Response, error) {
				if calls.Add(1) <= 2 {
					return &http.Response{
						StatusCode: status,
						Body:       io.NopCloser(strings.NewReader("retry")),
						Request:    r,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("ok-cover")),
					Request:    r,
				}, nil
			}))
			got, err := client.FetchCoverBytes(ctx, "https://lain.bgm.tv/pic/cover/c/fixture.jpg", 303)
			if err != nil || string(got) != "ok-cover" || calls.Load() != 3 || len(waits) != 2 {
				t.Fatalf("got=%q err=%v calls=%d waits=%v", got, err, calls.Load(), waits)
			}
		})
	}

	t.Run("transient_transport_and_read_error_recover", func(t *testing.T) {
		var calls atomic.Int32
		client := NewLiveClient()
		client.coverLimiter = rate.NewLimiter(rate.Inf, 1)
		client.coverRetryWait = func(context.Context, time.Duration) error { return nil }
		client.http = newLiveHTTPClient(matchingTransport(func(r *http.Request) (*http.Response, error) {
			switch calls.Add(1) {
			case 1:
				return nil, io.ErrUnexpectedEOF
			case 2:
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bangumiErrReader{err: io.ErrUnexpectedEOF}),
					Request:    r,
				}, nil
			default:
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("recovered-cover")),
					Request:    r,
				}, nil
			}
		}))
		got, err := client.FetchCoverBytes(ctx, "https://lain.bgm.tv/pic/cover/c/fixture.jpg", 303)
		if err != nil || string(got) != "recovered-cover" || calls.Load() != 3 {
			t.Fatalf("got=%q err=%v calls=%d", got, err, calls.Load())
		}
	})

	t.Run("transient_exhausted_returns_non_permanent_error", func(t *testing.T) {
		var calls atomic.Int32
		client := NewLiveClient()
		client.coverLimiter = rate.NewLimiter(rate.Inf, 1)
		client.coverRetryWait = func(context.Context, time.Duration) error { return nil }
		client.http = newLiveHTTPClient(matchingTransport(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader("busy")),
				Request:    r,
			}, nil
		}))
		_, err := client.FetchCoverBytes(ctx, "https://lain.bgm.tv/pic/cover/c/fixture.jpg", 303)
		if err == nil || errors.Is(err, ErrCoverUnavailable) || calls.Load() != 3 {
			t.Fatalf("expected non-ErrCoverUnavailable after 3 attempts, got err=%v calls=%d", err, calls.Load())
		}
	})

	for _, status := range []int{http.StatusNoContent, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone} {
		t.Run("permanent_no_retry_"+http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			client := NewLiveClient()
			client.coverLimiter = rate.NewLimiter(rate.Inf, 1)
			client.coverRetryWait = func(context.Context, time.Duration) error {
				t.Fatal("permanent status must not wait for retry")
				return nil
			}
			client.http = newLiveHTTPClient(matchingTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{
					StatusCode: status,
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    r,
				}, nil
			}))
			_, err := client.FetchCoverBytes(ctx, "https://lain.bgm.tv/pic/cover/c/fixture.jpg", 303)
			if !errors.Is(err, ErrCoverUnavailable) || calls.Load() != 1 {
				t.Fatalf("got err=%v calls=%d", err, calls.Load())
			}
		})
	}

	t.Run("cancel_during_retry_wait", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(ctx)
		var calls atomic.Int32
		client := NewLiveClient()
		client.coverLimiter = rate.NewLimiter(rate.Inf, 1)
		client.coverRetryWait = func(waitCtx context.Context, _ time.Duration) error {
			cancel()
			return waitCtx.Err()
		}
		client.http = newLiveHTTPClient(matchingTransport(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    r,
			}, nil
		}))
		_, err := client.FetchCoverBytes(cancelCtx, "https://lain.bgm.tv/pic/cover/c/fixture.jpg", 303)
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("got err=%v calls=%d", err, calls.Load())
		}
	})
}

func TestCatalogAuthUsesCredentialOwnerAndSeriesScope(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := database.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := NewService(store, filepath.Join(dir, "bangumi"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.live.limiter = rate.NewLimiter(rate.Inf, 1)
	if _, err := store.ClaimAdmin(ctx, "catalogowner", "FixturePassword123!"); err != nil {
		t.Fatal(err)
	}
	lib := database.Library{ID: "lib", RootCID: "root", Name: "Catalog", Enabled: true}
	if err := store.UpsertLibrary(ctx, lib); err != nil {
		t.Fatal(err)
	}
	reader, err := store.CreateUser(ctx, database.CreateUserInput{
		Username: "catalogreader", Password: "FixturePassword123!",
		AllowNSFW: true, AllowedLibraryIDs: []string{"lib"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = store.DB().ExecContext(ctx, `
INSERT INTO series(id,library_id,cid,name,relative_path,created_at,updated_at,seen_scan_id)
VALUES('private','lib','private','Private title','Private title',?,?,'scan'),
      ('public','lib','public','Public title','Public title',?,?,'scan')`, now, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO series_metadata(series_id,is_nsfw,updated_at) VALUES('private',1,?)`, now); err != nil {
		t.Fatal(err)
	}
	privateSeries, err := store.SeriesByIDAny(ctx, "private", false)
	if err != nil {
		t.Fatal(err)
	}
	publicSeries, err := store.SeriesByID(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}
	save := func(row database.UserBangumiTracker) {
		t.Helper()
		if err := store.SaveUserBangumiTrackerConfig(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	save(database.UserBangumiTracker{
		UserID: database.PrimaryAdminUserID, AuthMode: TrackerAuthModeOAuth,
		AccessToken: "fixture-admin-old", RefreshToken: "fixture-refresh-old",
		ExpiresAt:   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		BgmUsername: "fixture-admin-profile",
	})
	var refreshed, calls, invalidated atomic.Int32
	svc.SetCoverCacheInvalidator(func() { invalidated.Add(1) })
	const private = `{"id":201,"type":1,"name":"Private title","nsfw":true}`
	const public = `{"id":202,"type":1,"name":"Public title","nsfw":false}`
	var authHeaders []string
	svc.SetAPITransportForTest(matchingTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		w := httptest.NewRecorder()
		switch r.URL.Path {
		case "/oauth/access_token":
			refreshed.Add(1)
			if r.Host != "bgm.tv" || r.FormValue("refresh_token") != "fixture-refresh-old" {
				t.Error("OAuth refresh owner or destination mismatch")
			}
			_, _ = io.WriteString(w, `{"access_token":"fixture-admin-new","refresh_token":"fixture-refresh-new","expires_in":604800}`)
		case "/v0/me":
			if r.Header.Get("Authorization") == "Bearer fixture-own" {
				_, _ = io.WriteString(w, `{"id":2,"username":"fixture-reader-profile"}`)
			} else {
				_, _ = io.WriteString(w, `{"id":1,"username":"fixture-admin-profile"}`)
			}
		case "/oauth/token_status":
			w.WriteHeader(http.StatusNotFound)
		case "/v0/search/subjects", "/v0/subjects/201", "/v0/subjects/201/persons":
			authHeaders = append(authHeaders, r.Header.Get("Authorization"))
			switch r.URL.Path {
			case "/v0/search/subjects":
				var payload struct {
					Filter map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error("could not decode search filter")
				}
				want := any(false)
				if r.Header.Get("Authorization") != "" {
					want = nil
				}
				if value, ok := payload.Filter["nsfw"]; !ok || value != want {
					t.Errorf("unexpected upstream NSFW filter: %v", value)
				}
				_, _ = io.WriteString(w, `{"data":[`+private+`,`+public+`]}`)
			case "/v0/subjects/201":
				_, _ = io.WriteString(w, private)
			default:
				_, _ = io.WriteString(w, "[]")
			}
		default:
			t.Errorf("unexpected upstream request path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
		return w.Result(), nil
	}))
	scope := reader.Scope()
	scope.ShowNSFW = true
	readerCtx := database.WithUserScope(ctx, scope)
	candidates, _, err := svc.SearchCandidates(readerCtx, privateSeries, "201")
	if err != nil || len(candidates) != 1 || candidates[0].ID != 201 || !candidates[0].NSFW {
		t.Fatalf("authorized direct lookup = %+v, %v", candidates, err)
	}
	if refreshed.Load() != 1 || invalidated.Load() != 1 || strings.Join(authHeaders, ",") != "Bearer fixture-admin-new,Bearer fixture-admin-new" {
		t.Fatalf("direct lookup did not use the refreshed credential for both API operations")
	}
	adminRow, err := store.UserBangumiTrackerConfig(ctx, database.PrimaryAdminUserID)
	if err != nil || adminRow.AccessToken != "fixture-admin-new" || adminRow.RefreshToken != "fixture-refresh-new" {
		t.Fatal("refreshed credential was not persisted to its owner")
	}
	borrowerRow, err := store.UserBangumiTrackerConfig(ctx, reader.ID)
	if err != nil || borrowerRow.AccessToken != "" || borrowerRow.RefreshToken != "" || borrowerRow.BgmUsername != "" {
		t.Fatal("borrowed profile or credential appeared in the borrower's tracker")
	}
	ownPAT := "fixture-own"
	if _, err := svc.UpdateTrackerConfig(readerCtx, TrackerConfigUpdate{AccessToken: &ownPAT}); err != nil || invalidated.Load() != 2 {
		t.Fatalf("PAT binding did not invalidate failed cover entries: %v", err)
	}
	authHeaders = nil
	candidates, _, err = svc.SearchCandidates(readerCtx, privateSeries, "Private title")
	if err != nil || len(candidates) != 2 || strings.Join(authHeaders, ",") != "Bearer fixture-own" {
		t.Fatalf("bound reader search did not use its own PAT: %v, results=%d", err, len(candidates))
	}
	scope.ShowNSFW = false
	restricted := database.WithUserScope(ctx, scope)
	authHeaders = nil
	candidates, _, err = svc.SearchCandidates(restricted, publicSeries, "Public title")
	if err != nil || len(candidates) != 1 || candidates[0].ID != 202 || len(authHeaders) != 1 || authHeaders[0] != "" {
		t.Fatalf("restricted search = %+v, %v", candidates, err)
	}
	noLibrary := database.WithUserScope(ctx, database.UserScope{UserID: reader.ID, AllowNSFW: true, ShowNSFW: true})
	before := calls.Load()
	for _, denied := range []context.Context{restricted, noLibrary} {
		if _, _, err := svc.SearchCandidates(denied, privateSeries, "201"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("denied series search = %v", err)
		}
	}
	lib.Enabled = false
	if err := store.UpsertLibrary(ctx, lib); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SearchCandidates(readerCtx, privateSeries, "201"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("hidden library search = %v", err)
	}
	if calls.Load() != before {
		t.Fatal("denied series triggered an upstream request")
	}
}
