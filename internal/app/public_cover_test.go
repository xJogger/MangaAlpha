package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/bangumi"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type trackingReadCloser struct {
	reader io.Reader
	closed *atomic.Int32
}

func (r *trackingReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *trackingReadCloser) Close() error {
	r.closed.Add(1)
	return nil
}

type errAfterBytesReader struct {
	prefix []byte
	read   bool
	err    error
}

func (r *errAfterBytesReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, r.prefix), nil
	}
	return 0, r.err
}

func TestPublicBangumiCoverFetcherPoolsClientAndRebuildsOnProxyChange(t *testing.T) {
	bangumi.SetAllowLoopbackCoversForTest(true)
	t.Cleanup(func() { bangumi.SetAllowLoopbackCoversForTest(false) })

	var srvRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srvRequests.Add(1)
		if r.Header.Get("User-Agent") != "MangaAlpha" {
			t.Errorf("unexpected User-Agent %q", r.Header.Get("User-Agent"))
		}
		_, _ = w.Write([]byte("pooled-cover"))
	}))
	defer srv.Close()

	var newClients, dialCount atomic.Int32
	var created []*http.Client
	fetcher := &publicBangumiCoverFetcher{
		newClient: func(_ string) *http.Client {
			newClients.Add(1)
			tr := &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					dialCount.Add(1)
					return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
				},
				MaxIdleConns:    4,
				IdleConnTimeout: 30 * time.Second,
			}
			c := &http.Client{Timeout: 5 * time.Second, Transport: tr}
			created = append(created, c)
			return c
		},
		retryWaits: []time.Duration{time.Millisecond, time.Millisecond},
		wait:       func(context.Context, time.Duration) error { return nil },
	}
	defer fetcher.closeIdleConnections()

	ctx := t.Context()
	coverURL := srv.URL + "/pic/cover/c/1.jpg"
	for i := range 3 {
		got, err := fetcher.fetch(ctx, "http://127.0.0.1:18080", coverURL, 100)
		if err != nil || string(got) != "pooled-cover" {
			t.Fatalf("call %d: got=%q err=%v", i, got, err)
		}
	}
	if newClients.Load() != 1 || dialCount.Load() != 1 || srvRequests.Load() != 3 {
		t.Fatalf("expected 1 pooled client & 1 TCP dial for 3 requests, got clients=%d dials=%d reqs=%d",
			newClients.Load(), dialCount.Load(), srvRequests.Load())
	}

	// Switching proxy rebuilds client and closes idle connections on the previous client.
	got, err := fetcher.fetch(ctx, "http://127.0.0.1:18081", coverURL, 100)
	if err != nil || string(got) != "pooled-cover" {
		t.Fatalf("after proxy switch: got=%q err=%v", got, err)
	}
	if newClients.Load() != 2 || dialCount.Load() != 2 || len(created) != 2 {
		t.Fatalf("expected client rebuild on proxy change, got clients=%d dials=%d", newClients.Load(), dialCount.Load())
	}

	// Same new proxy reuses the second pooled client without extra dials.
	got, err = fetcher.fetch(ctx, " http://127.0.0.1:18081 ", coverURL, 100)
	if err != nil || string(got) != "pooled-cover" {
		t.Fatalf("repeat trimmed proxy: got=%q err=%v", got, err)
	}
	if newClients.Load() != 2 || dialCount.Load() != 2 || srvRequests.Load() != 5 {
		t.Fatalf("expected trimmed same proxy reuse, got clients=%d dials=%d reqs=%d",
			newClients.Load(), dialCount.Load(), srvRequests.Load())
	}
}

func TestPublicBangumiCoverFetcherTransientRetriesAndBodyClose(t *testing.T) {
	const coverURL = "https://lain.bgm.tv/pic/cover/c/100.jpg"

	for _, status := range []int{
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		520, 522, 524,
	} {
		t.Run(fmt.Sprintf("http_%d_recovers", status), func(t *testing.T) {
			var attempts, closed atomic.Int32
			var waits []time.Duration
			fetcher := &publicBangumiCoverFetcher{
				newClient: func(string) *http.Client {
					return &http.Client{
						Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
							if r.Header.Get("User-Agent") != "MangaAlpha" {
								t.Errorf("missing User-Agent header")
							}
							n := attempts.Add(1)
							if n <= 2 {
								return &http.Response{
									StatusCode: status,
									Body:       &trackingReadCloser{reader: strings.NewReader("retry"), closed: &closed},
									Request:    r,
								}, nil
							}
							return &http.Response{
								StatusCode: http.StatusOK,
								Body:       &trackingReadCloser{reader: strings.NewReader("ok-cover"), closed: &closed},
								Request:    r,
							}, nil
						}),
					}
				},
				retryWaits: []time.Duration{250 * time.Millisecond, 600 * time.Millisecond},
				wait: func(_ context.Context, d time.Duration) error {
					waits = append(waits, d)
					return nil
				},
			}
			got, err := fetcher.fetch(t.Context(), "", coverURL, 100)
			if err != nil || string(got) != "ok-cover" {
				t.Fatalf("got=%q err=%v", got, err)
			}
			if attempts.Load() != 3 || closed.Load() != 3 || len(waits) != 2 ||
				waits[0] != 250*time.Millisecond || waits[1] != 600*time.Millisecond {
				t.Fatalf("attempts=%d closed=%d waits=%v", attempts.Load(), closed.Load(), waits)
			}
		})
	}

	t.Run("transport_and_body_read_errors_recover", func(t *testing.T) {
		var attempts, closed atomic.Int32
		fetcher := &publicBangumiCoverFetcher{
			newClient: func(string) *http.Client {
				return &http.Client{
					Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
						switch attempts.Add(1) {
						case 1:
							return nil, io.ErrUnexpectedEOF
						case 2:
							return &http.Response{
								StatusCode: http.StatusOK,
								Body:       &trackingReadCloser{reader: &errAfterBytesReader{prefix: []byte("partial"), err: io.ErrUnexpectedEOF}, closed: &closed},
								Request:    r,
							}, nil
						default:
							return &http.Response{
								StatusCode: http.StatusOK,
								Body:       &trackingReadCloser{reader: strings.NewReader("full-bytes"), closed: &closed},
								Request:    r,
							}, nil
						}
					}),
				}
			},
			retryWaits: []time.Duration{time.Millisecond, time.Millisecond},
			wait:       func(context.Context, time.Duration) error { return nil },
		}
		got, err := fetcher.fetch(t.Context(), "", coverURL, 100)
		if err != nil || string(got) != "full-bytes" || attempts.Load() != 3 || closed.Load() != 2 {
			t.Fatalf("got=%q err=%v attempts=%d closed=%d", got, err, attempts.Load(), closed.Load())
		}
	})

	t.Run("exhausted_503_returns_transient_error", func(t *testing.T) {
		var attempts, closed atomic.Int32
		fetcher := &publicBangumiCoverFetcher{
			newClient: func(string) *http.Client {
				return &http.Client{
					Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
						attempts.Add(1)
						return &http.Response{
							StatusCode: http.StatusServiceUnavailable,
							Body:       &trackingReadCloser{reader: strings.NewReader("busy"), closed: &closed},
							Request:    r,
						}, nil
					}),
				}
			},
			retryWaits: []time.Duration{time.Millisecond, time.Millisecond},
			wait:       func(context.Context, time.Duration) error { return nil },
		}
		_, err := fetcher.fetch(t.Context(), "", coverURL, 100)
		if err == nil || errors.Is(err, bangumi.ErrCoverUnavailable) || attempts.Load() != 3 || closed.Load() != 3 {
			t.Fatalf("expected non-permanent error after 3 attempts, got err=%v attempts=%d closed=%d",
				err, attempts.Load(), closed.Load())
		}
	})
}

func TestPublicBangumiCoverFetcherPermanentNoRetryAndCancel(t *testing.T) {
	const coverURL = "https://lain.bgm.tv/pic/cover/c/100.jpg"

	for _, status := range []int{http.StatusNoContent, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone} {
		t.Run(fmt.Sprintf("permanent_%d_no_retry", status), func(t *testing.T) {
			var attempts, closed atomic.Int32
			fetcher := &publicBangumiCoverFetcher{
				newClient: func(string) *http.Client {
					return &http.Client{
						Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
							attempts.Add(1)
							return &http.Response{
								StatusCode: status,
								Body:       &trackingReadCloser{reader: strings.NewReader(""), closed: &closed},
								Request:    r,
							}, nil
						}),
					}
				},
				retryWaits: []time.Duration{time.Millisecond, time.Millisecond},
				wait: func(context.Context, time.Duration) error {
					t.Fatal("permanent status must not wait for retry")
					return nil
				},
			}
			_, err := fetcher.fetch(t.Context(), "", coverURL, 100)
			if !errors.Is(err, bangumi.ErrCoverUnavailable) || attempts.Load() != 1 || closed.Load() != 1 {
				t.Fatalf("err=%v attempts=%d closed=%d", err, attempts.Load(), closed.Load())
			}
		})
	}

	t.Run("redirect_to_no_icon_subject_no_retry", func(t *testing.T) {
		var attempts atomic.Int32
		fetcher := &publicBangumiCoverFetcher{
			newClient: func(string) *http.Client {
				return &http.Client{
					Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
						attempts.Add(1)
						if r.URL.Host == "api.bgm.tv" {
							return &http.Response{
								StatusCode: http.StatusFound,
								Header:     http.Header{"Location": []string{"https://lain.bgm.tv/img/no_icon_subject.png"}},
								Body:       io.NopCloser(strings.NewReader("")),
								Request:    r,
							}, nil
						}
						return &http.Response{
							StatusCode: http.StatusOK,
							Body:       io.NopCloser(strings.NewReader("placeholder")),
							Request:    r,
						}, nil
					}),
				}
			},
			retryWaits: []time.Duration{time.Millisecond, time.Millisecond},
			wait: func(context.Context, time.Duration) error {
				t.Fatal("no_icon_subject must not wait for retry")
				return nil
			},
		}
		_, err := fetcher.fetch(t.Context(), "", "", 100)
		if !errors.Is(err, bangumi.ErrCoverUnavailable) || attempts.Load() != 2 {
			t.Fatalf("err=%v attempts=%d", err, attempts.Load())
		}
	})

	t.Run("redirect_to_disallowed_domain_no_retry", func(t *testing.T) {
		var attempts atomic.Int32
		fetcher := &publicBangumiCoverFetcher{
			newClient: func(string) *http.Client {
				return &http.Client{
					Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
						attempts.Add(1)
						return &http.Response{
							StatusCode: http.StatusFound,
							Header:     http.Header{"Location": []string{"https://evil.example.invalid/cover.jpg"}},
							Body:       io.NopCloser(strings.NewReader("")),
							Request:    r,
						}, nil
					}),
				}
			},
			retryWaits: []time.Duration{time.Millisecond, time.Millisecond},
			wait: func(context.Context, time.Duration) error {
				t.Fatal("disallowed redirect must not wait for retry")
				return nil
			},
		}
		_, err := fetcher.fetch(t.Context(), "", coverURL, 100)
		if !errors.Is(err, bangumi.ErrCoverUnavailable) || attempts.Load() != 1 {
			t.Fatalf("err=%v attempts=%d", err, attempts.Load())
		}
	})

	t.Run("empty_and_oversized_body_no_retry", func(t *testing.T) {
		for _, body := range [][]byte{{}, bytes.Repeat([]byte("a"), (16<<20)+1)} {
			var attempts atomic.Int32
			fetcher := &publicBangumiCoverFetcher{
				newClient: func(string) *http.Client {
					return &http.Client{
						Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
							attempts.Add(1)
							return &http.Response{
								StatusCode: http.StatusOK,
								Body:       io.NopCloser(bytes.NewReader(body)),
								Request:    r,
							}, nil
						}),
					}
				},
				retryWaits: []time.Duration{time.Millisecond, time.Millisecond},
				wait: func(context.Context, time.Duration) error {
					t.Fatal("empty/oversized body must not wait for retry")
					return nil
				},
			}
			_, err := fetcher.fetch(t.Context(), "", coverURL, 100)
			if !errors.Is(err, bangumi.ErrCoverUnavailable) || attempts.Load() != 1 {
				t.Fatalf("len=%d err=%v attempts=%d", len(body), err, attempts.Load())
			}
		}
	})

	t.Run("context_cancelled_during_backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		var attempts atomic.Int32
		fetcher := &publicBangumiCoverFetcher{
			newClient: func(string) *http.Client {
				return &http.Client{
					Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
						attempts.Add(1)
						return &http.Response{
							StatusCode: http.StatusBadGateway,
							Body:       io.NopCloser(strings.NewReader("")),
							Request:    r,
						}, nil
					}),
				}
			},
			retryWaits: []time.Duration{5 * time.Second, 5 * time.Second},
			wait: func(wCtx context.Context, d time.Duration) error {
				cancel()
				return waitPublicCoverRetry(wCtx, d)
			},
		}
		_, err := fetcher.fetch(ctx, "", coverURL, 100)
		if !errors.Is(err, context.Canceled) || attempts.Load() != 1 {
			t.Fatalf("err=%v attempts=%d", err, attempts.Load())
		}
	})
}
