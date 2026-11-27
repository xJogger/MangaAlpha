package thumbnail

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/database"
)

func TestBangumiCoverFailureExpiryAndDiskReuse(t *testing.T) {
	store, first, _ := thumbnailTestStore(t)
	root := filepath.Join(t.TempDir(), "thumbnails", "series")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := New(store, root, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx := database.WithUserScope(t.Context(), database.UserScope{
		UserID: "fixture-reader", AllowNSFW: true, ShowNSFW: true, AllowedLibraryIDs: []string{first.LibraryID},
	})
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO series_metadata(series_id,bgm_subject_id,is_nsfw,cover_source,updated_at)
VALUES(?,42,1,'bangumi',?)`, first.SeriesID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	imageBytes := testPNG(t, 42, 24)
	service.SetBangumiCoverFetcher(func(got context.Context, _ string, _ int64) ([]byte, error) {
		scope, _ := database.UserScopeFromContext(got)
		if scope.UserID != "fixture-reader" || !scope.EffectiveShowNSFW() || !scope.AllowsLibrary(first.LibraryID) {
			t.Error("cover worker lost caller scope")
		}
		switch calls.Add(1) {
		case 1:
			return nil, errors.New("fixture upstream HTTP 403")
		case 2:
			return []byte("fixture missing image"), nil
		default:
			return imageBytes, nil
		}
	})
	miss := func(wantCalls int32) {
		t.Helper()
		if _, ok, err := service.Get(ctx, first.SeriesID); err != nil || ok || calls.Load() != wantCalls {
			t.Fatalf("cover miss returned ok=%v, err=%v, requests=%d want %d", ok, err, calls.Load(), wantCalls)
		}
	}
	miss(1)
	miss(1)
	service.bgmMu.Lock()
	if len(service.bgmFailures) != 1 {
		t.Errorf("negative entries = %d, want 1", len(service.bgmFailures))
	}
	for key, until := range service.bgmFailures {
		if remaining := time.Until(until); remaining <= 0 || remaining > bgmFailureTTL {
			t.Errorf("negative cache expiry = %v", remaining)
		}
		service.bgmFailures[key] = time.Now().Add(-time.Second)
	}
	service.bgmMu.Unlock()
	miss(2)
	miss(2)
	service.InvalidateBangumiCoverFailures()
	data, ok, err := service.Get(ctx, first.SeriesID)
	if err != nil || !ok || calls.Load() != 3 {
		t.Fatalf("cover after invalidation = %v, %v, requests=%d", ok, err, calls.Load())
	}
	file, err := os.ReadFile(filepath.Join(filepath.Dir(root), "bangumi", "bgm_42.jpg"))
	if err != nil || !bytes.Equal(file, data.Bytes) {
		t.Fatalf("canonical disk cover differs from response: %v", err)
	}
	reopened, err := New(store, root, logger)
	if err != nil {
		t.Fatal(err)
	}
	data, ok, err = reopened.Get(ctx, first.SeriesID)
	if err != nil || !ok || !bytes.Equal(file, data.Bytes) {
		t.Fatalf("cover after service restart = %v, %v", ok, err)
	}
}

func TestGenerateFirstBookSeriesThumbnail(t *testing.T) {
	store, first, second := thumbnailTestStore(t)
	service, err := New(
		store, t.TempDir()+"/thumbnails",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	page := testPNG(t, 600, 900)
	if err := service.Generate(context.Background(), second, page); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := service.Get(context.Background(), first.SeriesID); err != nil || ok {
		t.Fatalf("second book generated thumbnail: ok=%v err=%v", ok, err)
	}
	if err := service.Generate(context.Background(), first, page); err != nil {
		t.Fatal(err)
	}
	item, ok, err := service.Get(context.Background(), first.SeriesID)
	if err != nil || !ok {
		t.Fatalf("thumbnail missing: ok=%v err=%v", ok, err)
	}
	if item.MediaType != "image/jpeg" || item.Width != 200 || item.Height != 300 {
		t.Fatalf("unexpected thumbnail metadata: %+v", item)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(item.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	if config.Width != 200 || config.Height != 300 {
		t.Fatalf("unexpected JPEG dimensions: %+v", config)
	}
	stats, err := service.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 1 || stats.Bytes != int64(len(item.Bytes)) {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	if _, err := store.DB().Exec(`UPDATE books SET size=size+1 WHERE id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := service.Get(context.Background(), first.SeriesID); err != nil || ok {
		t.Fatalf("stale thumbnail returned: ok=%v err=%v", ok, err)
	}
	stats, err = service.Stats(context.Background())
	if err != nil || stats.Files != 0 {
		t.Fatalf("stale metadata remains: stats=%+v err=%v", stats, err)
	}
}

func TestClearSeriesThumbnails(t *testing.T) {
	store, first, _ := thumbnailTestStore(t)
	root := t.TempDir() + "/thumbnails"
	service, err := New(store, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Generate(context.Background(), first, testPNG(t, 100, 150)); err != nil {
		t.Fatal(err)
	}
	if err := service.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("thumbnail files remain: %v", entries)
	}
	stats, err := service.Stats(context.Background())
	if err != nil || stats.Files != 0 || stats.Bytes != 0 {
		t.Fatalf("unexpected stats after clear: %+v err=%v", stats, err)
	}
}

func TestMaybeGenerateAfterReadingFirstPage(t *testing.T) {
	store, first, _ := thumbnailTestStore(t)
	service, err := New(
		store, t.TempDir()+"/thumbnails",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	page := testPNG(t, 400, 600)
	service.MaybeGenerate(first, 2, page)
	if _, ok, err := service.Get(context.Background(), first.SeriesID); err != nil || ok {
		t.Fatalf("non-first page generated thumbnail: ok=%v err=%v", ok, err)
	}
	service.MaybeGenerate(first, 1, page)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok, err := service.Get(context.Background(), first.SeriesID); err != nil {
			t.Fatal(err)
		} else if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("thumbnail was not generated asynchronously")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func thumbnailTestStore(t *testing.T) (*database.Store, database.Book, database.Book) {
	t.Helper()
	store, err := database.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.UpsertLibrary(ctx, database.Library{
		ID: "lib", Name: "Library", RootCID: "root", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`
INSERT INTO series(id,library_id,cid,name,relative_path,created_at,updated_at,seen_scan_id)
VALUES('series','lib','cid','Series','Series',?,?,'scan')`, now, now); err != nil {
		t.Fatal(err)
	}
	for _, book := range []struct {
		id, fileID, name string
		number           int
	}{
		{"first", "file-1", "001.cbz", 1},
		{"second", "file-2", "002.cbz", 2},
	} {
		if _, err := store.DB().Exec(`
INSERT INTO books(
 id,series_id,library_id,file_id,parent_cid,name,size,pick_code,sha1,
 file_created_at,file_modified_at,number_sort,created_at,updated_at,seen_scan_id
) VALUES(?,'series','lib',?,'cid',?,123,'pick','sha',?,?,?, ?,?,'scan')`,
			book.id, book.fileID, book.name, now, now, book.number, now, now); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.BookByID(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.BookByID(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	return store, first, second
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestScaledSize(t *testing.T) {
	tests := []struct {
		width, height int
		wantWidth     int
		wantHeight    int
	}{
		{100, 150, 100, 150},
		{600, 900, 200, 300},
		{1200, 600, 300, 150},
	}
	for _, test := range tests {
		width, height := scaledSize(test.width, test.height, 300)
		if width != test.wantWidth || height != test.wantHeight {
			t.Fatalf(
				"scaledSize(%d,%d)=(%d,%d), want (%d,%d)",
				test.width, test.height, width, height, test.wantWidth, test.wantHeight,
			)
		}
	}
}

func TestNSFWFallbackPreferredCoverModes(t *testing.T) {
	store, first, _ := thumbnailTestStore(t)
	service, err := New(store, filepath.Join(t.TempDir(), "thumbnails", "series"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	scope := database.UserScope{UserID: "reader", AllowNSFW: true, ShowNSFW: true, AllowedLibraryIDs: []string{first.LibraryID}}
	ctx := database.WithUserScope(t.Context(), scope)
	if _, err := store.DB().Exec(`
INSERT INTO series_metadata(series_id,bgm_subject_id,is_nsfw,ext_source_id,ext_entry_id,updated_at)
VALUES(?,42,1,'existing-source','existing-entry','')`, first.SeriesID); err != nil {
		t.Fatal(err)
	}
	if err := service.Generate(ctx, first, testPNG(t, 9, 9)); err != nil {
		t.Fatal(err)
	}
	fallbackPNG := testPNG(t, 18, 12)
	var upstreamCalls, fallbackCalls int
	service.SetBangumiCoverFetcher(func(context.Context, string, int64) ([]byte, error) {
		upstreamCalls++
		return nil, bangumi.ErrCoverUnavailable
	})
	service.SetNSFWFallbackCoverFetcher(func(ctx context.Context, id string) ([]byte, string, error) {
		caller, _ := database.UserScopeFromContext(ctx)
		if id != first.SeriesID || caller.UserID != scope.UserID || !caller.EffectiveShowNSFW() {
			t.Fatal("fallback lost the caller scope")
		}
		fallbackCalls++
		return fallbackPNG, "image/png", nil
	}, nil)

	for _, mode := range []string{database.CoverSourceAuto, database.CoverSourceBangumi, database.CoverSourceBook} {
		if _, err := store.DB().Exec("UPDATE series_metadata SET cover_source=? WHERE series_id=?", mode, first.SeriesID); err != nil {
			t.Fatal(err)
		}
		series, err := store.SeriesByID(ctx, first.SeriesID)
		if err != nil {
			t.Fatal(err)
		}
		before, err := store.SeriesMetadataByID(ctx, series)
		if err != nil {
			t.Fatal(err)
		}
		data, ok, err := service.Get(ctx, first.SeriesID)
		if err != nil || !ok {
			t.Fatalf("mode %s: cover present=%v, err=%v", mode, ok, err)
		}
		if mode == database.CoverSourceBook {
			if data.MediaType != "image/jpeg" || bytes.Equal(data.Bytes, fallbackPNG) {
				t.Fatalf("book mode returned %s", data.MediaType)
			}
		} else if data.MediaType != "image/png" || !bytes.Equal(data.Bytes, fallbackPNG) {
			t.Fatalf("mode %s did not serve the fallback cover", mode)
		}
		after, err := store.SeriesMetadataByID(ctx, series)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("mode %s changed series metadata: %v", mode, err)
		}
	}
	if upstreamCalls != 1 || fallbackCalls != 2 {
		t.Fatalf("upstream requests=%d, fallback requests=%d", upstreamCalls, fallbackCalls)
	}
	if _, err := store.DB().Exec("UPDATE series_metadata SET cover_source='auto' WHERE series_id=?", first.SeriesID); err != nil {
		t.Fatal(err)
	}
	for _, denied := range []database.UserScope{
		{UserID: scope.UserID, AllowNSFW: true, AllowedLibraryIDs: scope.AllowedLibraryIDs},
		{UserID: scope.UserID, ShowNSFW: true, AllowedLibraryIDs: scope.AllowedLibraryIDs},
	} {
		if _, ok, err := service.Get(database.WithUserScope(ctx, denied), first.SeriesID); ok || !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("denied NSFW cover: ok=%v, err=%v", ok, err)
		}
	}
	if upstreamCalls != 1 || fallbackCalls != 2 {
		t.Fatal("denied NSFW cover reached an upstream fetcher")
	}

	service.SetNSFWFallbackCoverFetcher(func(context.Context, string) ([]byte, string, error) {
		return []byte("invalid image fixture"), "image/png", nil
	}, nil)
	if data, ok, err := service.Get(ctx, first.SeriesID); err != nil || !ok || data.MediaType != "image/jpeg" {
		t.Fatalf("auto mode with invalid fallback did not reuse book cover: %v, %v", ok, err)
	}
	if _, err := store.DB().Exec("UPDATE series_metadata SET cover_source='bangumi' WHERE series_id=?", first.SeriesID); err != nil {
		t.Fatal(err)
	}
	if data, ok, err := service.Get(ctx, first.SeriesID); err != nil || !ok || data.MediaType != "image/jpeg" {
		t.Fatalf("bangumi mode with invalid fallback: %v, %v", ok, err)
	}
}

func TestBangumiNSFWCooldownIsPerUserAndResettable(t *testing.T) {
	store, first, _ := thumbnailTestStore(t)
	service, err := New(store, filepath.Join(t.TempDir(), "thumbnails", "series"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	scope := func(user string) context.Context {
		return database.WithUserScope(t.Context(), database.UserScope{
			UserID: user, AllowNSFW: true, ShowNSFW: true, AllowedLibraryIDs: []string{first.LibraryID},
		})
	}
	ctx := scope("reader-a")
	for subject := 1; subject <= 7; subject++ {
		id := fmt.Sprintf("fixture-series-%d", subject)
		if _, err := store.DB().Exec(`
INSERT INTO series(id,library_id,cid,name,relative_path,created_at,updated_at,seen_scan_id)
VALUES(?,?,?,?,?,'','','fixture-scan')`, id, first.LibraryID, id, id, id); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`
INSERT INTO series_metadata(series_id,bgm_subject_id,is_nsfw,cover_source,updated_at)
VALUES(?,?,1,'bangumi','')`, id, subject); err != nil {
			t.Fatal(err)
		}
	}
	png := testPNG(t, 18, 12)
	var fetched, invalidations int
	service.SetBangumiCoverFetcher(func(_ context.Context, _ string, subject int64) ([]byte, error) {
		fetched++
		switch subject {
		case 3:
			return png, nil
		case 6:
			return nil, errors.New("temporary upstream fixture error")
		default:
			return nil, fmt.Errorf("fixture response: %w", bangumi.ErrCoverUnavailable)
		}
	})
	service.SetNSFWFallbackCoverFetcher(func(context.Context, string) ([]byte, string, error) {
		return png, "image/png", nil
	}, func() { invalidations++ })
	get := func(ctx context.Context, subject, wantCalls int, wantType string) {
		t.Helper()
		data, ok, err := service.Get(ctx, fmt.Sprintf("fixture-series-%d", subject))
		if err != nil || !ok || data.MediaType != wantType || fetched != wantCalls {
			t.Fatalf("subject %d: type=%s, cover=%v, err=%v, requests=%d want %d", subject, data.MediaType, ok, err, fetched, wantCalls)
		}
	}
	get(ctx, 1, 1, "image/png")
	get(ctx, 1, 1, "image/png") // a cache hit does not count toward the cooldown
	get(ctx, 3, 2, "image/jpeg")
	get(ctx, 2, 3, "image/png")
	get(ctx, 4, 4, "image/png") // the successful fetch reset the first failure
	get(ctx, 5, 4, "image/png")
	get(ctx, 3, 4, "image/jpeg") // cached Bangumi covers still win during cooldown
	get(scope("reader-b"), 5, 5, "image/png")
	service.bgmMu.Lock()
	if cooldown := time.Until(service.bgmNSFWFailures["reader-a"].until); cooldown < 29*time.Minute || cooldown > bgmNSFWCooldown {
		t.Errorf("reader cooldown is %s", cooldown)
	}
	expired := service.bgmNSFWFailures["reader-a"]
	expired.until = time.Now().Add(-time.Second)
	service.bgmNSFWFailures["reader-a"] = expired
	service.bgmMu.Unlock()
	get(ctx, 5, 6, "image/png") // expired cooldown starts a new streak
	get(ctx, 6, 7, "image/png") // unrelated failure breaks the streak
	get(ctx, 7, 8, "image/png")

	service.InvalidateBangumiCoverFailures()
	get(ctx, 1, 9, "image/png")
	get(ctx, 2, 10, "image/png")
	get(ctx, 4, 10, "image/png")
	if err := service.ClearBangumiCovers(ctx); err != nil {
		t.Fatal(err)
	}
	get(ctx, 4, 11, "image/png")
	get(ctx, 3, 12, "image/jpeg") // both the cooldown and disk image were cleared
	if invalidations != 2 {
		t.Fatalf("fallback cache invalidations=%d, want 2", invalidations)
	}

	// Per-user invalidation resets only the targeted user's cooldown and failure cache.
	get(ctx, 1, 13, "image/png")
	get(ctx, 2, 14, "image/png") // reader-a now cooling down
	get(scope("reader-b"), 1, 15, "image/png")
	get(scope("reader-b"), 2, 16, "image/png") // reader-b now cooling down
	service.InvalidateUserBangumiCoverFailures("reader-a")
	get(scope("reader-b"), 4, 16, "image/png") // reader-b still cooling down
	get(ctx, 4, 17, "image/png")               // reader-a retries Bangumi after per-user reset
}

func TestExternalCoverFailureOnlyUsesCachedFirstPage(t *testing.T) {
	store, first, _ := thumbnailTestStore(t)
	ctx := t.Context()
	if _, err := store.DB().ExecContext(ctx, `UPDATE libraries SET provider_type='local' WHERE id=?`, first.LibraryID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO series_metadata(series_id,ext_source_id,ext_entry_id,ext_cover_url,updated_at)
VALUES(?,'offline','entry','https://example.test/cover',?)`, first.SeriesID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	service, err := New(store, filepath.Join(t.TempDir(), "thumbnails", "series"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var externalCalls, pageCalls int
	service.SetExternalCoverFetcher(func(context.Context, string, string, string) ([]byte, string, error) {
		externalCalls++
		return nil, "", errors.New("fixture missing cover")
	})
	service.SetLocalFirstPageFetcher(func(context.Context, database.Book) ([]byte, error) {
		pageCalls++
		return testPNG(t, 24, 24), nil
	})
	if _, ok, err := service.Get(ctx, first.SeriesID); err != nil || ok || externalCalls != 1 || pageCalls != 0 {
		t.Fatalf("uncached fallback ok=%t err=%v external=%d archive=%d", ok, err, externalCalls, pageCalls)
	}
	if err := service.Generate(ctx, first, testPNG(t, 24, 24)); err != nil {
		t.Fatal(err)
	}
	data, ok, err := service.Get(ctx, first.SeriesID)
	if err != nil || !ok || data.MediaType != "image/jpeg" || externalCalls != 2 || pageCalls != 0 {
		t.Fatalf("cached fallback ok=%t err=%v external=%d archive=%d", ok, err, externalCalls, pageCalls)
	}
}
