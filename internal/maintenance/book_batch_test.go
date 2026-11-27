package maintenance

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/archive"
	"github.com/xJogger/fake-komga-115/internal/cache"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/extmeta"
	"github.com/xJogger/fake-komga-115/internal/thumbnail"
)

func TestBatchBookCoversOnlyReadsPageWhenRequested(t *testing.T) {
	store, thumbs, manager, page := bookBatchFixture(t)
	ctx := t.Context()
	if _, err := store.DB().Exec(`INSERT INTO series_metadata(series_id,bgm_subject_id,updated_at) VALUES('series',40,'')`); err != nil {
		t.Fatal(err)
	}
	var matchCalls atomic.Int32
	thumbs.SetTankobonMatcher(func(_ context.Context, _ database.Series, _ database.SeriesMetadata, books []thumbnail.VolumeMatchBook, live bool) ([]thumbnail.VolumeMatchResult, error) {
		matchCalls.Add(1)
		if !live || len(books) != 2 {
			t.Errorf("batch matcher received live=%v books=%v", live, books)
		}
		return []thumbnail.VolumeMatchResult{{BookID: "book-1", SubjectID: 41, NSFWKnown: true}}, nil
	})
	thumbs.SetBangumiCoverFetcher(func(context.Context, string, int64) ([]byte, error) { return page, nil })

	run := startBookBatch(t, manager, OperationBatchBookCovers, TargetSeries, "series", JobOptions{})
	if run.GeneratedCount != 1 || run.SkippedCount != 1 || matchCalls.Load() != 1 {
		t.Fatalf("metadata-only batch: %+v, match calls=%d", run, matchCalls.Load())
	}
	assertBatchTableCount(t, store, "zip_indexes", 0)
	assertBatchTableCount(t, store, "book_thumbnails", 0)
	if err := store.UpsertBookCoverCandidates(ctx, []database.BookCoverCandidate{{
		BookID: "book-1", SourceID: "fixture", CoverKind: database.CoverKindImportedFile, CoverRef: strings.Repeat("a", 64) + ".png",
	}}); err != nil {
		t.Fatal(err)
	}
	run = startBookBatch(t, manager, OperationBatchBookCovers, TargetLibrary, "library", JobOptions{FallbackPage1: true})
	if run.GeneratedCount != 1 || run.SkippedCount != 1 || matchCalls.Load() != 1 {
		t.Fatalf("page fallback batch: %+v, match calls=%d", run, matchCalls.Load())
	}
	assertBatchTableCount(t, store, "zip_indexes", 1)
	assertBatchTableCount(t, store, "book_thumbnails", 1)
	meta, found, err := store.BookMetadataByID(ctx, "book-1")
	if err != nil || !found || meta.BgmSubjectID != 41 {
		t.Fatalf("Bangumi binding was not stored: %+v %v", meta, err)
	}
}

func TestBatchComicInfoSeriesAndLibraryResume(t *testing.T) {
	store, _, manager, _ := bookBatchFixture(t)
	run := startBookBatch(t, manager, OperationBatchComicInfo, TargetSeries, "series", JobOptions{})
	if run.TotalItems != 2 || run.GeneratedCount != 2 {
		t.Fatalf("series ComicInfo: %+v", run)
	}
	assertBatchTableCount(t, store, "zip_indexes", 0)
	assertBatchTableCount(t, store, "book_thumbnails", 0)
	run = startBookBatch(t, manager, OperationBatchComicInfo, TargetLibrary, "library", JobOptions{})
	if run.TotalItems != 2 || run.SkippedCount != 2 {
		t.Fatalf("library ComicInfo resume: %+v", run)
	}
	run = startBookBatch(t, manager, OperationBatchComicInfo, TargetLibrary, "library", JobOptions{ExtractPage1: true})
	if run.GeneratedCount != 2 {
		t.Fatalf("library ComicInfo and page thumbnails: %+v", run)
	}
	assertBatchTableCount(t, store, "book_thumbnails", 2)
	run = startBookBatch(t, manager, OperationBatchComicInfo, TargetLibrary, "library", JobOptions{Force: true, ExtractPage1: true})
	if run.GeneratedCount != 2 {
		t.Fatalf("library ComicInfo force: %+v", run)
	}
	assertBatchTableCount(t, store, "book_thumbnails", 2)
}

func TestCancelLibraryStopsActiveAndQueuedLibraryBatches(t *testing.T) {
	store, thumbs, manager, _ := bookBatchFixture(t)
	ctx := t.Context()
	if _, err := store.DB().Exec(`INSERT INTO series_metadata(series_id,bgm_subject_id,updated_at) VALUES('series',40,'')`); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	thumbs.SetTankobonMatcher(func(ctx context.Context, _ database.Series, _ database.SeriesMetadata, _ []thumbnail.VolumeMatchBook, _ bool) ([]thumbnail.VolumeMatchResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	active, err := manager.StartJob(ctx, OperationBatchBookCovers, TargetLibrary, "library", JobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("cover batch did not start")
	}
	queued, err := manager.Enqueue(ctx, OperationBatchComicInfo, TargetLibrary, "library", JobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager.CancelLibrary(ctx, "library")
	for _, id := range []string{active.ID, queued.ID} {
		run := waitRun(t, manager, id)
		if run.Status != "canceled" || run.ProcessedItems != 0 {
			t.Fatalf("library batch did not cancel: %+v", run)
		}
	}
	assertBatchTableCount(t, store, "zip_indexes", 0)
}

func bookBatchFixture(t *testing.T) (*database.Store, *thumbnail.Service, *Manager, []byte) {
	t.Helper()
	dir := t.TempDir()
	store, err := database.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	insertMaintenanceCatalog(t, store)
	var page, raw bytes.Buffer
	if err := png.Encode(&page, image.NewRGBA(image.Rect(0, 0, 64, 80))); err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(&raw)
	for name, data := range map[string][]byte{
		"0001.png":          page.Bytes(),
		"sub/ComicInfo.xml": []byte("<ComicInfo><Title>Embedded title</Title></ComicInfo>"),
	} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file-1", "file-2"} {
		if err := os.WriteFile(filepath.Join(dir, name), raw.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`UPDATE libraries SET provider_type='local',root_path=? WHERE id='library'`, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE books SET size=?; UPDATE series SET books_count=2`, raw.Len()); err != nil {
		t.Fatal(err)
	}
	cm, err := cache.New(store, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.Default()
	archives := archive.NewService(store, nil, cm, logger)
	archives.SetComicInfoParser(func(reader io.Reader) (database.BookScrapedMetadata, database.SeriesComicInfoBackfill, error) {
		meta, err := extmeta.ParseComicInfoXML(reader)
		if err != nil {
			return database.BookScrapedMetadata{}, database.SeriesComicInfoBackfill{}, err
		}
		return database.BookScrapedMetadata{Title: meta.Title}, database.SeriesComicInfoBackfill{}, nil
	})
	t.Cleanup(archives.Close)
	thumbs, err := thumbnail.New(store, filepath.Join(dir, "series"), logger)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(store, archives, thumbs, logger)
	t.Cleanup(manager.Close)
	return store, thumbs, manager, page.Bytes()
}

func startBookBatch(t *testing.T, manager *Manager, op, target, id string, options JobOptions) Run {
	t.Helper()
	run, err := manager.StartJob(t.Context(), op, target, id, options)
	if err != nil {
		t.Fatal(err)
	}
	run = waitRun(t, manager, run.ID)
	if run.Status != "success" || run.ProcessedItems != 2 {
		t.Fatalf("batch failed: %+v", run)
	}
	return run
}

func assertBatchTableCount(t *testing.T, store *database.Store, table string, want int) {
	t.Helper()
	var got int
	if err := store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != want {
		t.Fatalf("%s count=%d want=%d err=%v", table, got, want, err)
	}
}

func TestBookBatchUnknownTankobonKeepsConfirmedBinding(t *testing.T) {
	store, thumbs, manager, raw := bookBatchFixture(t)
	ctx := t.Context()
	if err := store.UpsertBookScrapedMetadata(ctx, "book-1", database.BookScrapedMetadata{
		BgmSubjectID: 41, MatchOrigin: "auto",
	}); err != nil {
		t.Fatal(err)
	}
	thumbs.SetPublicBangumiCoverFetcher(func(_ context.Context, _ string, id int64) ([]byte, error) {
		if id != 41 {
			t.Errorf("unknown subject passed to the unchecked public fetcher: %d", id)
		}
		return raw, nil
	})
	book, err := store.BookByID(ctx, "book-1")
	if err != nil {
		t.Fatal(err)
	}
	meta, _, err := store.BookMetadataByID(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	unknown := thumbnail.VolumeMatchResult{BookID: book.ID, SubjectID: 42}
	if generated, err := manager.batchBookCover(ctx, book, meta, &unknown, nil, JobOptions{Force: true}); err != nil || !generated {
		t.Fatalf("existing binding unavailable: generated=%v err=%v", generated, err)
	}
	if saved, _, err := store.BookMetadataByID(ctx, book.ID); err != nil || saved.BgmSubjectID != 41 {
		t.Fatalf("confirmed binding was replaced: %+v err=%v", saved, err)
	}
}

func TestBatchComicInfoSkipsPage1OnIOError(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := database.Open(filepath.Join(dir, "batch_io_err.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer srv.Close()

	acct, err := store.UpsertWebDAVAccount(ctx, database.WebDAVAccountSecret{
		WebDAVAccount: database.WebDAVAccount{
			Name:          "TestWD",
			BaseURL:       srv.URL,
			RatePerSecond: 100,
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO libraries(id,name,provider_type,webdav_account_id,root_cid,root_path,enabled,one_shot,created_at,updated_at)
VALUES('lib_wd','WD','webdav',?,'','/',1,0,?,?);
INSERT INTO series(id,library_id,cid,name,relative_path,one_shot,books_count,created_at,updated_at,seen_scan_id)
VALUES('ser_wd','lib_wd','S','S','S',0,1,?,?,'sc1');
INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,sha1,number_sort,created_at,updated_at,seen_scan_id)
VALUES('book_wd','ser_wd','lib_wd','S/bad.cbz','S','bad.cbz',1024,'','',1,?,?,'sc1')`,
		acct.ID, now, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}

	cm, err := cache.New(store, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	archives := archive.NewService(store, nil, cm, logger)
	archives.SetComicInfoParser(func(reader io.Reader) (database.BookScrapedMetadata, database.SeriesComicInfoBackfill, error) {
		return database.BookScrapedMetadata{}, database.SeriesComicInfoBackfill{}, nil
	})
	t.Cleanup(archives.Close)
	thumbs, err := thumbnail.New(store, filepath.Join(dir, "thumbs"), logger)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(store, archives, thumbs, logger)
	t.Cleanup(manager.Close)

	baseRun, err := manager.StartJob(ctx, OperationBatchComicInfo, TargetSeries, "ser_wd", JobOptions{ExtractPage1: false})
	if err != nil {
		t.Fatal(err)
	}
	baseRun = waitRun(t, manager, baseRun.ID)
	if baseRun.Status != "failed" || baseRun.FailedCount != 1 {
		t.Fatalf("unexpected baseline run result: %+v", baseRun)
	}
	baseline := reqCount.Swap(0)

	run, err := manager.StartJob(ctx, OperationBatchComicInfo, TargetSeries, "ser_wd", JobOptions{ExtractPage1: true})
	if err != nil {
		t.Fatal(err)
	}
	run = waitRun(t, manager, run.ID)
	if run.Status != "failed" || run.FailedCount != 1 || run.GeneratedCount != 0 {
		t.Fatalf("unexpected run result: %+v", run)
	}
	if got := reqCount.Load(); baseline == 0 || got != baseline {
		t.Fatalf("ExtractPage1 retried failed archive: baseline=%d withExtractPage1=%d", baseline, got)
	}
	assertBatchTableCount(t, store, "book_thumbnails", 0)
}
