package extmeta

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestCoverFailureDiagnosticsAreVerboseAndSanitized(t *testing.T) {
	rec := testMetadataDiagnostics(t)
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/missing":
			return managerResponse(r, http.StatusNotFound, strings.NewReader("fixture-secret")), nil
		case "/transport":
			return nil, errors.New("request failed to repo.example.test with fixture-secret")
		case "/length":
			resp := managerResponse(r, http.StatusOK, strings.NewReader("fixture-secret"))
			resp.ContentLength = 100
			return resp, nil
		case "/size":
			return managerResponse(r, http.StatusOK, strings.NewReader(strings.Repeat("a", 20))), nil
		default:
			return managerResponse(r, http.StatusOK, strings.NewReader("invalid")), nil
		}
	})}
	covers, err := NewCoverService(CoverConfig{
		DataDir: t.TempDir(), Client: client, Burst: 12, MaxImageBytes: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(path string) error {
		t.Helper()
		_, err := covers.GetOrFetch(t.Context(), "fixture", "cover", "https://repo.example.test"+path+"?token=fixture-secret")
		if err == nil {
			t.Fatalf("%s was expected to fail", path)
		}
		return err
	}
	_ = fetch("/concise")
	if len(rec.Recent(10)) != 0 {
		t.Fatal("cover fetch diagnostic was recorded in concise mode")
	}
	rec.SetVerbose(true)
	for _, tc := range []struct {
		path, code string
		status     any
	}{
		{"/missing", "HTTP_STATUS", float64(http.StatusNotFound)},
		{"/transport", "TRANSPORT_ERROR", nil},
		{"/length", "SIZE_LIMIT", nil},
		{"/size", "SIZE_LIMIT", nil},
		{"/invalid", "INVALID_IMAGE", nil},
	} {
		_ = fetch(tc.path)
		events := rec.Recent(1)
		if len(events) != 1 {
			t.Fatal("missing cover diagnostic event")
		}
		event := events[0]
		if event.Component != "extmeta" || event.Stage != "extmeta_cover" || event.Action != "fetch" ||
			event.Level != "debug" || event.Code != tc.code || event.Detail == "" || event.Meta["httpStatus"] != tc.status {
			t.Errorf("%s diagnostic = %+v", tc.path, event)
		}
	}
	if err := fetch("/missing"); !errors.Is(err, ErrNegativeCached) {
		t.Fatalf("repeated request = %v", err)
	}
	if events := rec.Recent(1); len(events) != 1 || events[0].Code != "NEGATIVE_CACHE_HIT" {
		t.Fatalf("negative cache diagnostic = %+v", events)
	}
	assertSafeMetadataDiagnostics(t, rec.ExportText(20))
}

func TestCoverCircuitBreakerIsolatesBookCustomByHost(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}
	var badHits, goodHits int
	client := &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "bad.example.test":
			badHits++
			return managerResponse(r, http.StatusTooManyRequests, strings.NewReader("rate limited")), nil
		case "good.example.test":
			goodHits++
			return managerResponse(r, http.StatusOK, bytes.NewReader(png)), nil
		default:
			return managerResponse(r, http.StatusNotFound, strings.NewReader("missing")), nil
		}
	})}
	covers, err := NewCoverService(CoverConfig{
		DataDir: t.TempDir(), Client: client, RequestsPerSec: 100, Burst: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	if _, err := covers.GetOrFetch(ctx, "book-custom", "b1", "https://bad.example.test/1.png"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen on bad host, got %v", err)
	}
	if _, err := covers.GetOrFetch(ctx, "book-custom", "b2", "https://bad.example.test/2.png"); !errors.Is(err, ErrCircuitOpen) || badHits != 1 {
		t.Fatalf("expected cached circuit open for same host without extra hit (hits=%d), got %v", badHits, err)
	}
	data, err := covers.GetOrFetch(ctx, "book-custom", "b3", "https://good.example.test/1.png")
	if err != nil || !bytes.Equal(data.Bytes, png) || goodHits != 1 {
		t.Fatalf("healthy domain should not be affected by bad domain circuit breaker: data=%+v goodHits=%d err=%v", data, goodHits, err)
	}
}
