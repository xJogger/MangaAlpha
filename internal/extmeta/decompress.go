package extmeta

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	defaultMaxDecompressedBytes int64 = 8 << 30  // 8 GiB safety ceiling
	defaultMaxUploadStreamBytes int64 = 10 << 30 // 10 GiB upload stream safety ceiling
	defaultMaxReleaseJSONBytes  int64 = 4 << 20  // 4 MiB
	streamBufferSize                  = 256 << 10
	progressEmitThreshold       int64 = 64 << 10
	sqliteFileMagic                   = "SQLite format 3\x00"
)

// RemoteUpdateInfo describes the outcome of checking a remote asset or release for changes.
type RemoteUpdateInfo struct {
	HasUpdate   bool   `json:"hasUpdate"`
	DownloadURL string `json:"downloadUrl,omitempty"`
	AssetName   string `json:"assetName,omitempty"`
	ETag        string `json:"etag,omitempty"`
	LastMod     string `json:"lastModified,omitempty"`
	Size        int64  `json:"size,omitempty"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
	Digest      string `json:"digest,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// DownloadResult contains byte counts, checksums, and HTTP cache validators for a completed download.
type DownloadResult struct {
	DownloadedBytes   int64  `json:"downloadedBytes"`
	DecompressedBytes int64  `json:"decompressedBytes"`
	SHA256            string `json:"sha256"`
	DownloadSHA256    string `json:"downloadSha256,omitempty"`
	ETag              string `json:"etag,omitempty"`
	LastModified      string `json:"lastModified,omitempty"`
}

// DownloadProgressFunc receives cumulative downloaded bytes, expected total bytes (-1 if unknown),
// and cumulative decompressed bytes written to disk.
type DownloadProgressFunc func(downloadedBytes, totalBytes, writtenBytes int64)

type githubReleaseAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	UpdatedAt          string `json:"updated_at"`
	CreatedAt          string `json:"created_at"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

type githubReleasePayload struct {
	TagName     string               `json:"tag_name"`
	PublishedAt string               `json:"published_at"`
	Name        string               `json:"name"`
	Size        int64                `json:"size"`
	UpdatedAt   string               `json:"updated_at"`
	CreatedAt   string               `json:"created_at"`
	DownloadURL string               `json:"browser_download_url"`
	Digest      string               `json:"digest"`
	Assets      []githubReleaseAsset `json:"assets"`
}

// CheckRemoteUpdate checks whether a remote metadata asset has changed compared to
// previously recorded validators. When releaseAPIURL is set, it inspects the release JSON
// for a matching assetName (comparing updated_at and size so rolling nightly tags work);
// otherwise it issues an HTTP HEAD (with Range fallback on 405/501) on downloadURL.
func CheckRemoteUpdate(
	ctx context.Context,
	client *http.Client,
	downloadURL, releaseAPIURL, assetName, prevETag, prevLastMod string,
	prevSize int64,
	prevUpdatedAt string,
) (RemoteUpdateInfo, error) {
	if client == nil {
		client = NewSafeHTTPClient("", 30*time.Second)
	}

	releaseAPIURL = strings.TrimSpace(releaseAPIURL)
	downloadURL = strings.TrimSpace(downloadURL)
	assetName = strings.TrimSpace(assetName)
	prevETag = strings.TrimSpace(prevETag)
	prevLastMod = strings.TrimSpace(prevLastMod)
	prevUpdatedAt = strings.TrimSpace(prevUpdatedAt)

	if releaseAPIURL != "" {
		info, err := checkReleaseAPIUpdate(ctx, client, releaseAPIURL, downloadURL, assetName, prevSize, prevUpdatedAt)
		if err != nil {
			return RemoteUpdateInfo{}, err
		}
		// If release payload lacks updated_at and size, fall back to HEAD on resolved downloadURL.
		if info.UpdatedAt == "" && info.Size <= 0 && info.DownloadURL != "" && (prevETag != "" || prevLastMod != "") {
			return checkHeadUpdate(ctx, client, info.DownloadURL, prevETag, prevLastMod, prevSize)
		}
		return info, nil
	}

	if downloadURL == "" {
		return RemoteUpdateInfo{}, fmt.Errorf("%w: either downloadURL or releaseAPIURL must be provided", ErrInvalidConfig)
	}
	return checkHeadUpdate(ctx, client, downloadURL, prevETag, prevLastMod, prevSize)
}

func checkReleaseAPIUpdate(
	ctx context.Context,
	client *http.Client,
	releaseAPIURL, fallbackDownloadURL, assetName string,
	prevSize int64,
	prevUpdatedAt string,
) (RemoteUpdateInfo, error) {
	if _, err := validateHTTPURL(releaseAPIURL); err != nil {
		return RemoteUpdateInfo{}, fmt.Errorf("invalid releaseApiUrl: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseAPIURL, nil)
	if err != nil {
		return RemoteUpdateInfo{}, err
	}
	req.Header.Set("User-Agent", repoUserAgent)
	req.Header.Set("Accept", "application/vnd.github+json, application/json")

	resp, err := client.Do(req)
	if err != nil {
		return RemoteUpdateInfo{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return RemoteUpdateInfo{}, &HTTPStatusError{StatusCode: resp.StatusCode, Op: "fetch release API"}
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, defaultMaxReleaseJSONBytes+1))
	if err != nil {
		return RemoteUpdateInfo{}, fmt.Errorf("read release API response: %w", err)
	}
	if int64(len(raw)) > defaultMaxReleaseJSONBytes {
		return RemoteUpdateInfo{}, errors.New("release API response exceeds size limit")
	}

	var rel githubReleasePayload
	if err := json.Unmarshal(raw, &rel); err != nil {
		return RemoteUpdateInfo{}, fmt.Errorf("decode release API JSON: %w", err)
	}

	var matched *githubReleaseAsset
	if len(rel.Assets) > 0 {
		for i := range rel.Assets {
			a := &rel.Assets[i]
			if assetName != "" && a.Name == assetName {
				matched = a
				break
			}
		}
		if matched == nil && assetName != "" {
			for i := range rel.Assets {
				a := &rel.Assets[i]
				if strings.EqualFold(a.Name, assetName) {
					matched = a
					break
				}
			}
		}
		if matched == nil && assetName == "" && len(rel.Assets) == 1 {
			matched = &rel.Assets[0]
		}
		if matched == nil {
			return RemoteUpdateInfo{}, fmt.Errorf("release asset %q not found", assetName)
		}
	} else if rel.DownloadURL != "" {
		matched = &githubReleaseAsset{
			Name:               rel.Name,
			Size:               rel.Size,
			UpdatedAt:          rel.UpdatedAt,
			CreatedAt:          rel.CreatedAt,
			BrowserDownloadURL: rel.DownloadURL,
			Digest:             rel.Digest,
		}
	} else {
		return RemoteUpdateInfo{}, errors.New("release payload contains no assets")
	}

	dlURL := strings.TrimSpace(matched.BrowserDownloadURL)
	if dlURL == "" {
		dlURL = fallbackDownloadURL
	}
	if dlURL != "" {
		if _, err := validateHTTPURL(dlURL); err != nil {
			return RemoteUpdateInfo{}, fmt.Errorf("invalid release asset download URL: %w", err)
		}
	}

	updatedAt := strings.TrimSpace(matched.UpdatedAt)
	if updatedAt == "" {
		updatedAt = strings.TrimSpace(matched.CreatedAt)
	}
	if updatedAt == "" {
		updatedAt = strings.TrimSpace(rel.PublishedAt)
	}

	info := RemoteUpdateInfo{
		DownloadURL: dlURL,
		AssetName:   matched.Name,
		Size:        matched.Size,
		UpdatedAt:   updatedAt,
		Digest:      strings.TrimSpace(matched.Digest),
	}

	switch {
	case prevUpdatedAt == "" && prevSize <= 0:
		info.HasUpdate = true
		info.Reason = "no_previous_state"
	case updatedAt != "" && prevUpdatedAt != "" && updatedAt != prevUpdatedAt:
		info.HasUpdate = true
		info.Reason = "updated_at_changed"
	case prevSize > 0 && matched.Size > 0 && matched.Size != prevSize:
		info.HasUpdate = true
		info.Reason = "size_changed"
	case updatedAt != "" && prevUpdatedAt != "" && updatedAt == prevUpdatedAt:
		info.HasUpdate = false
		info.Reason = "up_to_date"
	case prevSize > 0 && matched.Size > 0 && matched.Size == prevSize:
		info.HasUpdate = false
		info.Reason = "up_to_date"
	default:
		info.HasUpdate = true
		info.Reason = "missing_validators"
	}

	return info, nil
}

func checkHeadUpdate(
	ctx context.Context,
	client *http.Client,
	downloadURL, prevETag, prevLastMod string,
	prevSize int64,
) (RemoteUpdateInfo, error) {
	if _, err := validateHTTPURL(downloadURL); err != nil {
		return RemoteUpdateInfo{}, fmt.Errorf("invalid downloadUrl: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, downloadURL, nil)
	if err != nil {
		return RemoteUpdateInfo{}, err
	}
	req.Header.Set("User-Agent", repoUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return RemoteUpdateInfo{}, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	size := resp.ContentLength
	op := "check download headers"
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		rangeReq, rErr := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
		if rErr != nil {
			return RemoteUpdateInfo{}, rErr
		}
		rangeReq.Header.Set("User-Agent", repoUserAgent)
		rangeReq.Header.Set("Range", "bytes=0-0")
		resp, err = client.Do(rangeReq)
		if err != nil {
			return RemoteUpdateInfo{}, err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		size = parseTotalSizeFromHeaders(resp)
		op = "check download range"
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return RemoteUpdateInfo{}, &HTTPStatusError{StatusCode: resp.StatusCode, Op: op}
	}

	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	lastMod := strings.TrimSpace(resp.Header.Get("Last-Modified"))

	info := RemoteUpdateInfo{
		DownloadURL: downloadURL,
		ETag:        etag,
		LastMod:     lastMod,
		Size:        size,
	}

	if prevETag == "" && prevLastMod == "" && prevSize <= 0 {
		info.HasUpdate = true
		info.Reason = "no_previous_state"
		return info, nil
	}
	if prevETag != "" && etag != "" {
		if etag != prevETag {
			info.HasUpdate = true
			info.Reason = "etag_changed"
			return info, nil
		}
		if prevSize > 0 && size > 0 && size != prevSize {
			info.HasUpdate = true
			info.Reason = "size_changed"
			return info, nil
		}
		info.HasUpdate = false
		info.Reason = "up_to_date"
		return info, nil
	}
	if prevLastMod != "" && lastMod != "" {
		if lastMod != prevLastMod {
			info.HasUpdate = true
			info.Reason = "last_modified_changed"
			return info, nil
		}
		if prevSize > 0 && size > 0 && size != prevSize {
			info.HasUpdate = true
			info.Reason = "size_changed"
			return info, nil
		}
		info.HasUpdate = false
		info.Reason = "up_to_date"
		return info, nil
	}
	if prevSize > 0 && size > 0 {
		info.HasUpdate = size != prevSize
		if info.HasUpdate {
			info.Reason = "size_changed"
		} else {
			info.Reason = "up_to_date"
		}
		return info, nil
	}

	info.HasUpdate = true
	info.Reason = "missing_validators"
	return info, nil
}

func parseTotalSizeFromHeaders(resp *http.Response) int64 {
	if cr := strings.TrimSpace(resp.Header.Get("Content-Range")); cr != "" {
		if idx := strings.LastIndexByte(cr, '/'); idx >= 0 {
			totalStr := strings.TrimSpace(cr[idx+1:])
			if totalStr != "*" {
				if n, err := strconv.ParseInt(totalStr, 10, 64); err == nil && n >= 0 {
					return n
				}
			}
		}
	}
	return resp.ContentLength
}

// DownloadAndDecompressToFile streams a remote archive to a temporary file, decompresses it
// on the fly according to compression (or filename suffix), and atomically renames the result
// to destPath. It never buffers whole archives in memory.
func DownloadAndDecompressToFile(
	ctx context.Context,
	client *http.Client,
	downloadURL, compression, destPath string,
	onProgress DownloadProgressFunc,
) (DownloadResult, error) {
	return downloadAndDecompressToFileWithLimit(
		ctx, client, downloadURL, compression, destPath, defaultMaxDecompressedBytes, onProgress,
	)
}

func downloadAndDecompressToFileWithLimit(
	ctx context.Context,
	client *http.Client,
	downloadURL, compression, destPath string,
	maxOutputBytes int64,
	onProgress DownloadProgressFunc,
) (DownloadResult, error) {
	if _, err := validateHTTPURL(downloadURL); err != nil {
		return DownloadResult{}, fmt.Errorf("invalid downloadUrl: %w", err)
	}
	if strings.TrimSpace(destPath) == "" {
		return DownloadResult{}, errors.New("destPath must not be empty")
	}
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxDecompressedBytes
	}
	format, err := resolveCompressionFormat(compression, downloadURL)
	if err != nil {
		return DownloadResult{}, err
	}
	if client == nil {
		client = NewSafeHTTPClient("", 60*time.Second)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(downloadURL), nil)
	if err != nil {
		return DownloadResult{}, err
	}
	req.Header.Set("User-Agent", repoUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return DownloadResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return DownloadResult{}, &HTTPStatusError{StatusCode: resp.StatusCode, Op: "download metadata"}
	}

	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return DownloadResult{}, err
	}

	dlHasher := sha256.New()
	tracker := &progressStreamReader{
		r:          io.TeeReader(resp.Body, dlHasher),
		totalBytes: resp.ContentLength,
		onProgress: onProgress,
	}

	outTmp, err := os.CreateTemp(destDir, ".extmeta-out-*")
	if err != nil {
		return DownloadResult{}, err
	}
	outTmpPath := outTmp.Name()
	cleanupOut := true
	defer func() {
		if cleanupOut {
			_ = outTmp.Close()
			_ = os.Remove(outTmpPath)
		}
	}()
	if err := outTmp.Chmod(0o600); err != nil {
		return DownloadResult{}, err
	}

	outHasher := sha256.New()
	var writtenBytes int64

	switch format {
	case "none":
		writtenBytes, err = streamBoundedCopy(ctx, outTmp, outHasher, tracker, maxOutputBytes, tracker, onProgress)
	case "gzip":
		gz, gErr := gzip.NewReader(tracker)
		if gErr != nil {
			return DownloadResult{}, fmt.Errorf("open gzip stream: %w", gErr)
		}
		writtenBytes, err = streamBoundedCopy(ctx, outTmp, outHasher, gz, maxOutputBytes, tracker, onProgress)
		closeErr := gz.Close()
		if err == nil {
			err = closeErr
		}
	case "zstd":
		dec, zErr := zstd.NewReader(
			tracker,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxWindow(128<<20),
		)
		if zErr != nil {
			return DownloadResult{}, fmt.Errorf("open zstd stream: %w", zErr)
		}
		writtenBytes, err = streamBoundedCopy(ctx, outTmp, outHasher, dec, maxOutputBytes, tracker, onProgress)
		dec.Close()
	case "zip":
		writtenBytes, err = extractFirstSQLiteFromZipStream(ctx, destDir, outTmp, outHasher, tracker, maxOutputBytes, onProgress)
	default:
		return DownloadResult{}, fmt.Errorf("unsupported compression format %q", format)
	}
	if err != nil {
		return DownloadResult{}, err
	}

	if err := outTmp.Sync(); err != nil {
		return DownloadResult{}, err
	}
	if err := outTmp.Close(); err != nil {
		return DownloadResult{}, err
	}
	if err := os.Rename(outTmpPath, destPath); err != nil {
		return DownloadResult{}, err
	}
	cleanupOut = false

	if onProgress != nil {
		onProgress(tracker.downloaded, tracker.totalBytes, writtenBytes)
	}

	return DownloadResult{
		DownloadedBytes:   tracker.downloaded,
		DecompressedBytes: writtenBytes,
		SHA256:            hex.EncodeToString(outHasher.Sum(nil)),
		DownloadSHA256:    hex.EncodeToString(dlHasher.Sum(nil)),
		ETag:              strings.TrimSpace(resp.Header.Get("ETag")),
		LastModified:      strings.TrimSpace(resp.Header.Get("Last-Modified")),
	}, nil
}

func resolveCompressionFormat(compression, downloadURL string) (string, error) {
	c := strings.ToLower(strings.TrimSpace(compression))
	if c == "" || c == "auto" {
		if u, err := url.Parse(strings.TrimSpace(downloadURL)); err == nil {
			ext := strings.ToLower(path.Ext(u.Path))
			switch ext {
			case ".zst", ".zstd":
				return "zstd", nil
			case ".gz", ".gzip":
				return "gzip", nil
			case ".zip":
				return "zip", nil
			case ".sqlite", ".sqlite3", ".db":
				return "none", nil
			}
		}
		return "none", nil
	}
	switch c {
	case "zst", "zstd":
		return "zstd", nil
	case "gz", "gzip":
		return "gzip", nil
	case "zip":
		return "zip", nil
	case "none", "sqlite", "sqlite3", "db", "raw":
		return "none", nil
	default:
		return "", fmt.Errorf("unsupported compression %q", compression)
	}
}

type progressStreamReader struct {
	r           io.Reader
	downloaded  int64
	totalBytes  int64
	written     int64
	lastEmitted int64
	onProgress  DownloadProgressFunc
}

func (p *progressStreamReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	if n > 0 {
		p.downloaded += int64(n)
		if p.onProgress != nil && p.downloaded-p.lastEmitted >= progressEmitThreshold {
			p.lastEmitted = p.downloaded
			p.onProgress(p.downloaded, p.totalBytes, p.written)
		}
	}
	return n, err
}

func streamBoundedCopy(
	ctx context.Context,
	dst io.Writer,
	hasher io.Writer,
	src io.Reader,
	maxBytes int64,
	tracker *progressStreamReader,
	onProgress DownloadProgressFunc,
) (int64, error) {
	mw := io.MultiWriter(dst, hasher)
	buf := make([]byte, streamBufferSize)
	var written int64
	var lastEmitted int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, rErr := src.Read(buf)
		if n > 0 {
			if written+int64(n) > maxBytes {
				return written, fmt.Errorf("decompressed output exceeds %d bytes limit", maxBytes)
			}
			if _, wErr := mw.Write(buf[:n]); wErr != nil {
				return written, wErr
			}
			written += int64(n)
			if tracker != nil {
				tracker.written = written
			}
			if onProgress != nil && written-lastEmitted >= progressEmitThreshold {
				lastEmitted = written
				dl := written
				tot := int64(-1)
				if tracker != nil {
					dl = tracker.downloaded
					tot = tracker.totalBytes
				}
				onProgress(dl, tot, written)
			}
		}
		if rErr == io.EOF {
			return written, nil
		}
		if rErr != nil {
			return written, rErr
		}
	}
}

func extractFirstSQLiteFromZipStream(
	ctx context.Context,
	tempDir string,
	outTmp io.Writer,
	outHasher io.Writer,
	tracker *progressStreamReader,
	maxOutputBytes int64,
	onProgress DownloadProgressFunc,
) (int64, error) {
	zipTmp, err := os.CreateTemp(tempDir, ".extmeta-zip-*")
	if err != nil {
		return 0, err
	}
	zipTmpPath := zipTmp.Name()
	defer func() {
		_ = zipTmp.Close()
		_ = os.Remove(zipTmpPath)
	}()
	if err := zipTmp.Chmod(0o600); err != nil {
		return 0, err
	}

	buf := make([]byte, streamBufferSize)
	maxCompressedZipBytes := maxOutputBytes + (16 << 20)
	if maxCompressedZipBytes < maxOutputBytes {
		maxCompressedZipBytes = maxOutputBytes
	}
	var zipBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, rErr := tracker.Read(buf)
		if n > 0 {
			if zipBytes+int64(n) > maxCompressedZipBytes {
				return 0, fmt.Errorf("downloaded zip exceeds %d bytes limit", maxCompressedZipBytes)
			}
			if _, wErr := zipTmp.Write(buf[:n]); wErr != nil {
				return 0, wErr
			}
			zipBytes += int64(n)
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			return 0, rErr
		}
	}
	if err := zipTmp.Close(); err != nil {
		return 0, err
	}

	zr, err := zip.OpenReader(zipTmpPath)
	if err != nil {
		return 0, fmt.Errorf("open downloaded zip: %w", err)
	}
	defer zr.Close()

	var target *zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := path.Base(strings.ReplaceAll(f.Name, "\\", "/"))
		if strings.HasPrefix(f.Name, "__MACOSX/") || strings.HasPrefix(base, "._") {
			continue
		}
		ext := strings.ToLower(path.Ext(base))
		if ext == ".sqlite" || ext == ".sqlite3" || ext == ".db" {
			target = f
			break
		}
	}
	if target == nil {
		return 0, errors.New("no .sqlite/.sqlite3/.db entry found in zip archive")
	}
	if target.UncompressedSize64 > uint64(maxOutputBytes) {
		return 0, fmt.Errorf("zip entry %q uncompressed size (%d) exceeds %d bytes limit", target.Name, target.UncompressedSize64, maxOutputBytes)
	}

	rc, err := target.Open()
	if err != nil {
		return 0, fmt.Errorf("open zip entry %q: %w", target.Name, err)
	}
	defer rc.Close()

	return streamBoundedCopy(ctx, outTmp, outHasher, rc, maxOutputBytes, tracker, onProgress)
}

// StreamUploadResult describes byte counts, SHA-256 checksum, and detected compression
// for a streaming SQLite upload written atomically to disk.
type StreamUploadResult struct {
	UploadedBytes     int64
	DecompressedBytes int64
	SHA256            string
	Compression       string
}

// StreamDecompressSQLiteToFile streams an uploaded raw or compressed (.sqlite/.sqlite3/.db/.gz/.zst/.zip)
// SQLite stream directly to a bounded temporary file, validates the SQLite3 file header ("SQLite format 3\x00"),
// and atomically places the resulting database at destPath without buffering in memory.
func StreamDecompressSQLiteToFile(
	ctx context.Context,
	filename string,
	src io.Reader,
	destPath string,
	maxUploadBytes int64,
	onProgress func(written int64),
) (StreamUploadResult, error) {
	if src == nil {
		return StreamUploadResult{}, errors.New("upload reader must not be nil")
	}
	if strings.TrimSpace(destPath) == "" {
		return StreamUploadResult{}, errors.New("destPath must not be empty")
	}
	if maxUploadBytes <= 0 {
		maxUploadBytes = defaultMaxUploadStreamBytes
	}

	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return StreamUploadResult{}, err
	}

	// Sniff leading bytes before selecting compression when extension is ambiguous.
	limited := io.LimitReader(src, maxUploadBytes+1)
	headBuf := make([]byte, 16)
	hn, hErr := io.ReadFull(limited, headBuf)
	if hErr != nil && !errors.Is(hErr, io.EOF) && !errors.Is(hErr, io.ErrUnexpectedEOF) {
		return StreamUploadResult{}, hErr
	}
	head := headBuf[:hn]
	if len(head) == 0 {
		return StreamUploadResult{}, fmt.Errorf("%w: uploaded file is empty", ErrUnsupportedSchema)
	}

	format := detectUploadCompressionFormat(filename, head)
	combined := io.MultiReader(bytes.NewReader(head), limited)
	var dlCb DownloadProgressFunc
	if onProgress != nil {
		dlCb = func(_, _, written int64) { onProgress(written) }
	}
	tracker := &progressStreamReader{
		r:          combined,
		totalBytes: -1,
		onProgress: dlCb,
	}

	outTmp, err := os.CreateTemp(destDir, ".extmeta-upload-*")
	if err != nil {
		return StreamUploadResult{}, err
	}
	outTmpPath := outTmp.Name()
	cleanupOut := true
	defer func() {
		if cleanupOut {
			_ = outTmp.Close()
			_ = os.Remove(outTmpPath)
		}
	}()
	if err := outTmp.Chmod(0o600); err != nil {
		return StreamUploadResult{}, err
	}

	outHasher := sha256.New()
	var writtenBytes int64
	switch format {
	case "none":
		writtenBytes, err = streamBoundedCopy(ctx, outTmp, outHasher, tracker, maxUploadBytes, tracker, dlCb)
	case "gzip":
		gz, gErr := gzip.NewReader(tracker)
		if gErr != nil {
			return StreamUploadResult{}, fmt.Errorf("%w: open gzip upload stream: %v", ErrUnsupportedSchema, gErr)
		}
		writtenBytes, err = streamBoundedCopy(ctx, outTmp, outHasher, gz, maxUploadBytes, tracker, dlCb)
		closeErr := gz.Close()
		if err == nil {
			err = closeErr
		}
	case "zstd":
		dec, zErr := zstd.NewReader(
			tracker,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxWindow(128<<20),
		)
		if zErr != nil {
			return StreamUploadResult{}, fmt.Errorf("%w: open zstd upload stream: %v", ErrUnsupportedSchema, zErr)
		}
		writtenBytes, err = streamBoundedCopy(ctx, outTmp, outHasher, dec, maxUploadBytes, tracker, dlCb)
		dec.Close()
	case "zip":
		writtenBytes, err = extractFirstSQLiteFromZipStream(ctx, destDir, outTmp, outHasher, tracker, maxUploadBytes, dlCb)
	default:
		return StreamUploadResult{}, fmt.Errorf("%w: unsupported upload format %q", ErrUnsupportedSchema, format)
	}
	if err != nil {
		return StreamUploadResult{}, err
	}
	if tracker.downloaded > maxUploadBytes {
		return StreamUploadResult{}, fmt.Errorf("upload stream exceeds %d bytes limit", maxUploadBytes)
	}

	// Verify SQLite3 file magic header before promoting temporary file.
	if writtenBytes < int64(len(sqliteFileMagic)) {
		return StreamUploadResult{}, fmt.Errorf("%w: uploaded file is not a valid SQLite database", ErrUnsupportedSchema)
	}
	if _, err := outTmp.Seek(0, io.SeekStart); err != nil {
		return StreamUploadResult{}, err
	}
	magic := make([]byte, len(sqliteFileMagic))
	if _, err := io.ReadFull(outTmp, magic); err != nil {
		return StreamUploadResult{}, fmt.Errorf("%w: read SQLite header: %v", ErrUnsupportedSchema, err)
	}
	if string(magic) != sqliteFileMagic {
		return StreamUploadResult{}, fmt.Errorf("%w: invalid SQLite header", ErrUnsupportedSchema)
	}

	if err := outTmp.Sync(); err != nil {
		return StreamUploadResult{}, err
	}
	if err := outTmp.Close(); err != nil {
		return StreamUploadResult{}, err
	}
	if err := os.Rename(outTmpPath, destPath); err != nil {
		return StreamUploadResult{}, err
	}
	cleanupOut = false

	if onProgress != nil {
		onProgress(writtenBytes)
	}
	return StreamUploadResult{
		UploadedBytes:     tracker.downloaded,
		DecompressedBytes: writtenBytes,
		SHA256:            hex.EncodeToString(outHasher.Sum(nil)),
		Compression:       format,
	}, nil
}

func detectUploadCompressionFormat(filename string, head []byte) string {
	if len(head) >= len(sqliteFileMagic) && string(head[:len(sqliteFileMagic)]) == sqliteFileMagic {
		return "none"
	}
	if len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b {
		return "gzip"
	}
	if len(head) >= 4 && head[0] == 0x28 && head[1] == 0xb5 && head[2] == 0x2f && head[3] == 0xfd {
		return "zstd"
	}
	if len(head) >= 4 && head[0] == 'P' && head[1] == 'K' && head[2] == 0x03 && head[3] == 0x04 {
		return "zip"
	}
	switch strings.ToLower(path.Ext(strings.TrimSpace(filename))) {
	case ".gz", ".gzip":
		return "gzip"
	case ".zst", ".zstd":
		return "zstd"
	case ".zip":
		return "zip"
	default:
		return "none"
	}
}
