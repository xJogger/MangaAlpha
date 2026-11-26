package extmeta

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xJogger/fake-komga-115/internal/diagnostics"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

const (
	defaultCoverRatePerSec    = 1.0
	defaultCoverBurst         = 2
	defaultCoverConcurrency   = 2
	defaultMaxCoverImageBytes = int64(10 << 20) // 10 MiB
	defaultNegativeCacheTTL   = 5 * time.Minute
	maxCircuitCooldown        = 10 * time.Minute
)

var (
	// ErrCircuitOpen indicates the source's cover circuit breaker is currently open.
	ErrCircuitOpen = errors.New("extmeta cover circuit breaker is open")
	// ErrNegativeCached indicates the cover URL failed recently and is within its negative cache TTL.
	ErrNegativeCached = errors.New("extmeta cover recently failed (negative cached)")
	// ErrInvalidImage indicates the fetched payload failed magic-byte image validation or size bounds.
	ErrInvalidImage = errors.New("invalid or unsupported cover image")
)

var circuitBreakerBackoffs = []time.Duration{
	30 * time.Second,
	60 * time.Second,
	180 * time.Second,
	maxCircuitCooldown,
}

// CoverConfig configures CoverService disk caching, rate limiting, concurrency, and circuit breaking.
type CoverConfig struct {
	DataDir          string
	Client           *http.Client
	ProxyURLProvider func(context.Context) string
	RequestsPerSec   float64
	Burst            int
	MaxConcurrent    int
	MaxImageBytes    int64
	NegativeCacheTTL time.Duration
	Now              func() time.Time
}

// CoverData holds a fetched or disk-cached external cover image.
type CoverData struct {
	Bytes     []byte
	MediaType string
	Extension string
	CachePath string
	FromCache bool
}

type circuitState struct {
	openUntil        time.Time
	consecutiveTrips int
}

type negativeEntry struct {
	expiresAt time.Time
	errMsg    string
}

// CoverService fetches, validates, and caches external source cover images with
// rate limiting, bounded concurrency (max 2), short-lived negative caching (5m),
// and exponential-backoff circuit breaking on 403/429/503 responses.
type CoverService struct {
	baseDir          string
	client           *http.Client
	proxyURLProvider func(context.Context) string
	limiter          *rate.Limiter
	sem              chan struct{}
	maxImageBytes    int64
	negativeCacheTTL time.Duration
	nowFn            func() time.Time

	flight singleflight.Group

	mu       sync.Mutex
	circuits map[string]*circuitState
	negCache map[string]negativeEntry
}

// NewCoverService creates a CoverService storing thumbnails under
// <dataDir>/thumbnails/extmeta/<safeSourceID>/<safeEntryID>_<urlHash>.<ext>.
func NewCoverService(cfg CoverConfig) (*CoverService, error) {
	dataDir := strings.TrimSpace(cfg.DataDir)
	if dataDir == "" {
		return nil, errors.New("CoverConfig.DataDir must not be empty")
	}
	baseDir := filepath.Join(dataDir, "thumbnails", "extmeta")
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return nil, err
	}

	rps := cfg.RequestsPerSec
	if rps <= 0 {
		rps = defaultCoverRatePerSec
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = defaultCoverBurst
	}
	maxConc := cfg.MaxConcurrent
	if maxConc <= 0 {
		maxConc = defaultCoverConcurrency
	}
	maxBytes := cfg.MaxImageBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxCoverImageBytes
	}
	negTTL := cfg.NegativeCacheTTL
	if negTTL <= 0 {
		negTTL = defaultNegativeCacheTTL
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	client := cfg.Client
	if client == nil {
		client = NewSafeHTTPClient("", 20*time.Second)
	}

	return &CoverService{
		baseDir:          baseDir,
		client:           client,
		proxyURLProvider: cfg.ProxyURLProvider,
		limiter:          rate.NewLimiter(rate.Limit(rps), burst),
		sem:              make(chan struct{}, maxConc),
		maxImageBytes:    maxBytes,
		negativeCacheTTL: negTTL,
		nowFn:            nowFn,
		circuits:         make(map[string]*circuitState),
		negCache:         make(map[string]negativeEntry),
	}, nil
}

// SafePathSegment normalizes source/entry identifiers into safe single-component filenames
// preventing path traversal while preserving readability for normal ASCII IDs.
func SafePathSegment(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if isCleanSafeSegment(trimmed) {
		return trimmed
	}

	var b strings.Builder
	b.Grow(len(trimmed))
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	prefix := strings.Trim(b.String(), "_")
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	sum := sha256.Sum256([]byte(raw))
	hashSuffix := hex.EncodeToString(sum[:6])
	if prefix == "" {
		return "id_" + hashSuffix
	}
	return prefix + "_" + hashSuffix
}

func isCleanSafeSegment(s string) bool {
	if s == "" || len(s) > 120 || s == "." || s == ".." || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isValid := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
		if !isValid {
			return false
		}
	}
	return s[0] != '.'
}

// CoverCachePath returns the resolved safe disk path for (sourceID, entryID, ext)
// guaranteed to stay inside <dataDir>/thumbnails/extmeta.
func (c *CoverService) CoverCachePath(sourceID, entryID, ext string) (string, error) {
	return c.coverCachePath(sourceID, SafePathSegment(entryID), ext)
}

func (c *CoverService) versionedCoverCachePath(sourceID, entryID, coverURL, ext string) (string, error) {
	sum := sha256.Sum256([]byte(strings.TrimSpace(coverURL)))
	return c.coverCachePath(sourceID, SafePathSegment(entryID)+"_"+hex.EncodeToString(sum[:6]), ext)
}

func (c *CoverService) coverCachePath(sourceID, safeEntry, ext string) (string, error) {
	safeSource := SafePathSegment(sourceID)
	cleanExt := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), ".")
	switch cleanExt {
	case "jpg", "jpeg":
		cleanExt = "jpg"
	case "png", "webp", "gif":
	default:
		return "", fmt.Errorf("unsupported cover extension %q", ext)
	}

	target := filepath.Join(c.baseDir, safeSource, safeEntry+"."+cleanExt)
	rel, err := filepath.Rel(c.baseDir, target)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", errors.New("resolved cover cache path escaped base directory")
	}
	return target, nil
}

// GetOrLoadLocal returns a cached cover from disk or resolves it via loader (used
// for offline SQLite thumbnail BLOBs) with singleflight deduplication, format
// validation, and atomic disk persistence without HTTP SSRF/circuit-breaker overhead.
func (c *CoverService) GetOrLoadLocal(
	ctx context.Context,
	sourceID, entryID, cacheKey string,
	maxBytes int64,
	loader func(context.Context) ([]byte, string, error),
) (CoverData, error) {
	if c == nil {
		return CoverData{}, errors.New("cover service is nil")
	}
	cacheKey = strings.TrimSpace(cacheKey)
	if cacheKey == "" || loader == nil {
		return CoverData{}, ErrInvalidConfig
	}
	if data, ok := c.readCachedFromDisk(sourceID, entryID, cacheKey); ok {
		return data, nil
	}
	limit := maxBytes
	if limit <= 0 {
		limit = max(c.maxImageBytes, maxSQLiteBlobCoverBytes)
	}
	flightKey := "local\x00" + sourceID + "\x00" + entryID + "\x00" + cacheKey
	val, err, _ := c.flight.Do(flightKey, func() (any, error) {
		if data, ok := c.readCachedFromDisk(sourceID, entryID, cacheKey); ok {
			return data, nil
		}
		raw, _, err := loader(ctx)
		if err != nil {
			return nil, err
		}
		if int64(len(raw)) > limit {
			return nil, fmt.Errorf("%w: cover blob size %d exceeds %d bytes limit", ErrInvalidImage, len(raw), limit)
		}
		ext, mimeType, ok := DetectImageFormat(raw)
		if !ok {
			return nil, fmt.Errorf("%w: unrecognized image magic bytes", ErrInvalidImage)
		}
		cachePath, err := c.versionedCoverCachePath(sourceID, entryID, cacheKey, ext)
		if err != nil {
			return nil, err
		}
		if err := writeCoverAtomic(cachePath, raw); err != nil {
			return nil, err
		}
		return CoverData{
			Bytes:     raw,
			MediaType: mimeType,
			Extension: ext,
			CachePath: cachePath,
			FromCache: false,
		}, nil
	})
	if err != nil {
		return CoverData{}, err
	}
	return val.(CoverData), nil
}

// GetOrFetch returns the cached cover from disk if present, otherwise fetches it remotely
// subject to circuit breaker, 5m negative cache, rate limiter, concurrency=2 semaphore,
// 10 MiB size cap, and JPEG/PNG/WebP/GIF magic-byte validation.
func (c *CoverService) GetOrFetch(ctx context.Context, sourceID, entryID, coverURL string) (CoverData, error) {
	if c == nil {
		return CoverData{}, errors.New("cover service is nil")
	}
	coverURL = strings.TrimSpace(coverURL)
	if _, err := validateHTTPURL(coverURL); err != nil {
		return CoverData{}, err
	}
	if data, ok := c.readCachedFromDisk(sourceID, entryID, coverURL); ok {
		return data, nil
	}

	circuitKey := c.circuitKey(sourceID, coverURL)
	if err := c.checkCircuitOpen(circuitKey); err != nil {
		return CoverData{}, err
	}

	negKey := c.negativeKey(sourceID, entryID, coverURL)
	if msg, cached := c.checkNegativeCache(negKey); cached {
		recordCoverFailure("NEGATIVE_CACHE_HIT", "A recent cover request failed.", 0)
		return CoverData{}, fmt.Errorf("%w: %s", ErrNegativeCached, msg)
	}

	flightKey := sourceID + "\x00" + entryID + "\x00" + coverURL
	val, err, _ := c.flight.Do(flightKey, func() (any, error) {
		if data, ok := c.readCachedFromDisk(sourceID, entryID, coverURL); ok {
			return data, nil
		}
		if err := c.checkCircuitOpen(circuitKey); err != nil {
			return nil, err
		}
		if msg, cached := c.checkNegativeCache(negKey); cached {
			recordCoverFailure("NEGATIVE_CACHE_HIT", "A recent cover request failed.", 0)
			return nil, fmt.Errorf("%w: %s", ErrNegativeCached, msg)
		}

		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}

		return c.fetchValidateAndCache(ctx, sourceID, entryID, coverURL, circuitKey, negKey)
	})
	if err != nil {
		return CoverData{}, err
	}
	return val.(CoverData), nil
}

func (c *CoverService) fetchValidateAndCache(
	ctx context.Context,
	sourceID, entryID, coverURL, circuitKey, negKey string,
) (CoverData, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, coverURL, nil)
	if err != nil {
		c.setNegativeCache(negKey, err.Error())
		return CoverData{}, err
	}
	req.Header.Set("User-Agent", repoUserAgent)
	req.Header.Set("Accept", "image/webp,image/png,image/jpeg,image/gif,*/*;q=0.8")

	client := c.client
	var proxy string
	if c.proxyURLProvider != nil {
		proxy = c.proxyURLProvider(ctx)
		client = NewSafeHTTPClient(proxy, 20*time.Second)
		defer client.CloseIdleConnections()
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrSSRFBlocked) {
			diagnostics.Default().Record(metadataDiagnostic("extmeta_cover", "fetch", diagnostics.LevelWarn, err, proxy))
		} else if !errors.Is(err, context.Canceled) {
			recordCoverFailure("TRANSPORT_ERROR", DescribeError(err), 0)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return CoverData{}, err
		}
		c.setNegativeCache(negKey, DescribeError(err))
		return CoverData{}, err
	}
	defer resp.Body.Close()

	if isCircuitBreakerStatus(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		c.tripCircuit(circuitKey)
		err := fmt.Errorf("%w: %w", ErrCircuitOpen, &HTTPStatusError{StatusCode: resp.StatusCode, Op: "fetch metadata cover"})
		event := metadataDiagnostic("extmeta_cover", "fetch", diagnostics.LevelWarn, err, proxy)
		event.Code = "CIRCUIT_OPEN"
		diagnostics.Default().Record(event)
		return CoverData{}, err
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		errMsg := fmt.Sprintf("cover HTTP %d", resp.StatusCode)
		c.setNegativeCache(negKey, errMsg)
		recordCoverFailure("HTTP_STATUS", "The cover server returned a non-success status.", resp.StatusCode)
		return CoverData{}, errors.New(errMsg)
	}

	if resp.ContentLength > c.maxImageBytes {
		errMsg := fmt.Sprintf("cover content-length (%d) exceeds %d bytes limit", resp.ContentLength, c.maxImageBytes)
		c.setNegativeCache(negKey, errMsg)
		recordCoverFailure("SIZE_LIMIT", "The cover exceeds the size limit.", 0)
		return CoverData{}, fmt.Errorf("%w: %s", ErrInvalidImage, errMsg)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxImageBytes+1))
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			recordCoverFailure("TRANSPORT_ERROR", DescribeError(err), 0)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return CoverData{}, err
		}
		c.setNegativeCache(negKey, DescribeError(err))
		return CoverData{}, err
	}
	if int64(len(raw)) > c.maxImageBytes {
		errMsg := fmt.Sprintf("cover size exceeds %d bytes limit", c.maxImageBytes)
		c.setNegativeCache(negKey, errMsg)
		recordCoverFailure("SIZE_LIMIT", "The cover exceeds the size limit.", 0)
		return CoverData{}, fmt.Errorf("%w: %s", ErrInvalidImage, errMsg)
	}

	ext, mimeType, ok := DetectImageFormat(raw)
	if !ok {
		c.setNegativeCache(negKey, "unrecognized image magic bytes")
		recordCoverFailure("INVALID_IMAGE", "The cover has unrecognized image magic bytes.", 0)
		return CoverData{}, fmt.Errorf("%w: unrecognized magic bytes", ErrInvalidImage)
	}

	cachePath, err := c.versionedCoverCachePath(sourceID, entryID, coverURL, ext)
	if err != nil {
		return CoverData{}, err
	}
	if err := writeCoverAtomic(cachePath, raw); err != nil {
		return CoverData{}, err
	}

	c.resetCircuit(circuitKey)
	return CoverData{
		Bytes:     raw,
		MediaType: mimeType,
		Extension: ext,
		CachePath: cachePath,
		FromCache: false,
	}, nil
}

func recordCoverFailure(code, detail string, status int) {
	event := diagnostics.Event{
		Component: "extmeta", Stage: "extmeta_cover", Action: "fetch",
		Code: code, Detail: detail,
	}
	if status != 0 {
		event.Meta = map[string]any{"httpStatus": status}
	}
	diagnostics.Default().RecordVerbose(event)
}

// DetectImageFormat inspects leading magic bytes for JPEG, PNG, WebP, and GIF images.
func DetectImageFormat(data []byte) (ext string, mimeType string, ok bool) {
	if len(data) < 6 {
		return "", "", false
	}
	if bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}) {
		return "jpg", "image/jpeg", true
	}
	if len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}) {
		return "png", "image/png", true
	}
	if bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")) {
		return "gif", "image/gif", true
	}
	if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")) {
		return "webp", "image/webp", true
	}
	return "", "", false
}

func (c *CoverService) readCachedFromDisk(sourceID, entryID, coverURL string) (CoverData, bool) {
	for _, ext := range []string{"jpg", "webp", "png", "gif"} {
		path, err := c.versionedCoverCachePath(sourceID, entryID, coverURL, ext)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			continue
		}
		detExt, mimeType, ok := DetectImageFormat(raw)
		if !ok {
			continue
		}
		return CoverData{
			Bytes:     raw,
			MediaType: mimeType,
			Extension: detExt,
			CachePath: path,
			FromCache: true,
		}, true
	}
	return CoverData{}, false
}

func (c *CoverService) circuitKey(sourceID, coverURL string) string {
	if s := strings.TrimSpace(sourceID); s != "" && s != "book-custom" {
		return "src:" + s
	}
	if u, err := url.Parse(coverURL); err == nil && u.Hostname() != "" {
		return "host:" + strings.ToLower(u.Hostname())
	}
	return "default"
}

func (c *CoverService) checkCircuitOpen(key string) error {
	now := c.nowFn()
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.circuits[key]
	if !ok {
		return nil
	}
	if now.Before(st.openUntil) {
		return fmt.Errorf("%w: cooldown until %s", ErrCircuitOpen, st.openUntil.UTC().Format(time.RFC3339))
	}
	return nil
}

func (c *CoverService) tripCircuit(key string) {
	now := c.nowFn()
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.circuits[key]
	if !ok {
		st = &circuitState{}
		c.circuits[key] = st
	}
	st.consecutiveTrips++
	idx := st.consecutiveTrips - 1
	if idx >= len(circuitBreakerBackoffs) {
		idx = len(circuitBreakerBackoffs) - 1
	}
	cooldown := circuitBreakerBackoffs[idx]
	if cooldown > maxCircuitCooldown {
		cooldown = maxCircuitCooldown
	}
	st.openUntil = now.Add(cooldown)
}

func (c *CoverService) resetCircuit(key string) {
	c.mu.Lock()
	delete(c.circuits, key)
	c.mu.Unlock()
}

func (c *CoverService) negativeKey(sourceID, entryID, coverURL string) string {
	return sourceID + "\x00" + entryID + "\x00" + coverURL
}

func (c *CoverService) checkNegativeCache(key string) (string, bool) {
	now := c.nowFn()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.negCache[key]
	if !ok {
		return "", false
	}
	if !now.Before(entry.expiresAt) {
		delete(c.negCache, key)
		return "", false
	}
	return entry.errMsg, true
}

func (c *CoverService) setNegativeCache(key, msg string) {
	now := c.nowFn()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.negCache) >= 4096 {
		for k, v := range c.negCache {
			if !now.Before(v.expiresAt) {
				delete(c.negCache, k)
			}
		}
	}
	c.negCache[key] = negativeEntry{
		expiresAt: now.Add(c.negativeCacheTTL),
		errMsg:    msg,
	}
}

func isCircuitBreakerStatus(code int) bool {
	return code == http.StatusForbidden || code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable
}

func writeCoverAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".extmeta-cover-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
