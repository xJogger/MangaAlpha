package thumbnail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/archive"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/importedcover"
)

func TestBookCoverPriorityAndSourceOverride(t *testing.T) {
	store, first, second := thumbnailTestStore(t)
	ctx := t.Context()
	dir := t.TempDir()
	s, err := NewWithDataDir(store, filepath.Join(dir, "series"), dir, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	page, bgm, ext, custom := testPNG(t, 80, 120), testPNG(t, 33, 55), testPNG(t, 24, 27), testPNG(t, 45, 60)
	assertCover := func(id string, width, height int) []byte {
		t.Helper()
		data, ok, err := s.GetBookCover(ctx, id)
		if err != nil || ok != (width > 0) {
			t.Fatalf("cover %s found=%v err=%v", id, ok, err)
		}
		if ok {
			image, _, err := image.DecodeConfig(bytes.NewReader(data.Bytes))
			if err != nil || image.Width != width || image.Height != height {
				t.Fatalf("cover %s size=%dx%d err=%v, want %dx%d", id, image.Width, image.Height, err, width, height)
			}
		}
		return data.Bytes
	}
	assertCover(second.ID, 0, 0)
	var indexCount int
	if err := store.DB().QueryRow(`SELECT count(*) FROM zip_indexes`).Scan(&indexCount); err != nil || indexCount != 0 {
		t.Fatalf("cover lookup indexed archive: count=%d err=%v", indexCount, err)
	}
	if err := s.GenerateBookThumbnail(ctx, second, page); err != nil {
		t.Fatal(err)
	}
	assertCover(second.ID, 80, 120)
	s.SetBangumiCoverFetcher(func(context.Context, string, int64) ([]byte, error) { return bgm, nil })
	s.SetBookExternalCoverFetcher(func(_ context.Context, source, entry, _, ref string) ([]byte, string, error) {
		if source != "catalog" || entry != "external-book" || ref != "https://example.invalid/cover.png" {
			t.Errorf("unexpected external reference %q %q %q", source, entry, ref)
		}
		return ext, "image/png", nil
	})
	if err := store.UpsertBookScrapedMetadata(ctx, second.ID, database.BookScrapedMetadata{BgmSubjectID: 42}); err != nil {
		t.Fatal(err)
	}
	assertCover(second.ID, 33, 55)
	if _, err := os.Stat(filepath.Join(dir, "bangumi", "bgm_42.jpg.public")); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBookScrapedMetadata(ctx, second.ID, database.BookScrapedMetadata{
		ExtSourceID: "catalog", ExtEntryID: "external-book", ExtCoverURL: "https://example.invalid/cover.png",
	}); err != nil {
		t.Fatal(err)
	}
	assertCover(second.ID, 24, 27)
	for _, source := range []database.MetadataSource{
		{ID: "catalog", Kind: "offline_sqlite", Name: "Catalog", Enabled: true, Ready: true},
		{ID: "custom-archive", Kind: "bangumi_archive", Name: "Custom archive", Enabled: true, Ready: true},
	} {
		if _, err := store.UpsertMetadataSource(ctx, source); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetLibraryMetadataSources(ctx, second.LibraryID, []string{"catalog", "custom-archive"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBookCoverCandidates(ctx, []database.BookCoverCandidate{
		{BookID: second.ID, SourceID: "custom-archive", EntryID: "42", RankOrder: 1,
			CoverKind: database.CoverKindBangumi, CoverRef: "https://bgm.tv/subject/42", NSFWKnown: true},
		{BookID: second.ID, SourceID: "third", EntryID: "third-book", RankOrder: 2,
			CoverKind: database.CoverKindHTTPURL, CoverRef: "https://example.invalid/third.png", NSFWKnown: true, IsNSFW: true},
	}); err != nil {
		t.Fatal(err)
	}
	assertCover(second.ID, 24, 27)
	if err := store.SetBookCoverSource(ctx, second.ID, "bangumi"); err != nil {
		t.Fatal(err)
	}
	assertCover(second.ID, 33, 55)
	if err := store.SetBookCoverSource(ctx, second.ID, "auto"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveBookCustomCover(ctx, second.ID, custom); err != nil {
		t.Fatal(err)
	}
	assertCover(second.ID, 45, 60)
	for _, tc := range []struct {
		mode string
		w, h int
	}{
		{"ext", 45, 60}, {"bangumi", 45, 60}, {"book", 45, 60}, {"custom", 45, 60}, {"auto", 45, 60},
	} {
		if err := store.SetBookCoverSource(ctx, second.ID, tc.mode); err != nil {
			t.Fatal(err)
		}
		assertCover(second.ID, tc.w, tc.h)
	}
	if err := s.DeleteBookCustomCover(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	assertCover(second.ID, 24, 27)
	if err := store.SetLibraryMetadataSources(ctx, second.LibraryID, []string{"bangumi", "catalog", "custom-archive"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
UPDATE book_cover_candidates SET rank_order=rank_order+1 WHERE book_id=?`, second.ID); err != nil {
		t.Fatal(err)
	}
	assertCover(second.ID, 33, 55)
	if err := store.SetBookCoverSource(ctx, second.ID, "book"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE books SET size=size+1 WHERE id=?`, second.ID); err != nil {
		t.Fatal(err)
	}
	assertCover(second.ID, 0, 0)
	assertCover(first.ID, 0, 0)
	blocked := database.WithUserScope(ctx, database.UserScope{UserID: "reader"})
	if _, _, err := s.GetBookCover(blocked, second.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty library whitelist allowed cover: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE libraries SET enabled=0 WHERE id=?`, second.LibraryID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetBookCover(ctx, second.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("hidden library allowed cover: %v", err)
	}
}

func TestUnionBookCoverPrefersHigherMemberCandidate(t *testing.T) {
	store, member, _ := thumbnailTestStore(t)
	ctx := t.Context()
	dir := t.TempDir()
	s, err := NewWithDataDir(store, filepath.Join(dir, "series"), dir, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO libraries(id,name,root_cid,enabled,created_at,updated_at,provider_type)
VALUES ('union','Union','union-root',1,'','','union');
INSERT INTO union_layers(union_library_id,member_library_id,priority,created_at) VALUES ('union','lib',1,'');
INSERT INTO series(id,library_id,cid,name,relative_path,created_at,updated_at,seen_scan_id)
VALUES ('union-series','union','union-cid','Series','Series','','','scan');
INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,created_at,updated_at,seen_scan_id)
VALUES ('union-book','union-series','union','union-file','union-cid','001.cbz',123,'',1,'','','scan');
INSERT INTO union_book_sources(union_book_id,source_book_id,source_library_id,priority)
VALUES ('union-book','first','lib',1);
`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertMetadataSource(ctx, database.MetadataSource{
		ID: "komga", Kind: "offline_sqlite", Name: "Komga", Enabled: true, Ready: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, library := range []string{member.LibraryID, "union"} {
		if err := store.SetLibraryMetadataSources(ctx, library, []string{"komga", "bangumi"}); err != nil {
			t.Fatal(err)
		}
	}
	imported, bgmPNG, external := testPNG(t, 24, 27), testPNG(t, 33, 55), testPNG(t, 36, 49)
	bgm, _, _, err := compressWithMaxEdge(bgmPNG, 420)
	if err != nil {
		t.Fatal(err)
	}
	ref := fmt.Sprintf("%x.png", sha256.Sum256(imported))
	path, _ := importedcover.Path(s.importedCoversDir, ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, imported, 0o600); err != nil {
		t.Fatal(err)
	}
	memberCover := database.BookCoverCandidate{
		BookID: member.ID, SourceID: "komga", EntryID: "imported", RankOrder: 0,
		CoverKind: database.CoverKindImportedFile, CoverRef: ref, NSFWKnown: true,
	}
	if err := store.UpsertBookCoverCandidates(ctx, []database.BookCoverCandidate{
		memberCover,
		{BookID: "union-book", SourceID: "bangumi", EntryID: "42", RankOrder: 1,
			CoverKind: database.CoverKindBangumi, CoverRef: "https://bgm.tv/subject/42", NSFWKnown: true},
	}); err != nil {
		t.Fatal(err)
	}
	bgmFetches := 0
	s.SetBangumiCoverFetcher(func(context.Context, string, int64) ([]byte, error) {
		bgmFetches++
		return bgmPNG, nil
	})
	s.SetBookExternalCoverFetcher(func(context.Context, string, string, string, string) ([]byte, string, error) {
		return external, "image/png", nil
	})
	assertCover := func(ctx context.Context, want []byte) {
		t.Helper()
		data, ok, err := s.GetBookCover(ctx, "union-book")
		if err != nil || !ok || !bytes.Equal(data.Bytes, want) {
			t.Fatalf("union book cover found=%t, matched=%t, err=%v", ok, bytes.Equal(data.Bytes, want), err)
		}
	}
	allowed := database.WithUserScope(ctx, database.UserScope{
		UserID: "reader", AllowedLibraryIDs: []string{"union", member.LibraryID},
	})
	unionOnly := database.WithUserScope(ctx, database.UserScope{
		UserID: "reader", AllowedLibraryIDs: []string{"union"},
	})
	assertCover(allowed, imported)
	if bgmFetches != 0 {
		t.Fatal("Bangumi cover was fetched before the higher-priority member cover")
	}
	assertCover(unionOnly, bgm)
	if err := store.SetBookCoverSource(ctx, "union-book", database.CoverSourceBangumi); err != nil {
		t.Fatal(err)
	}
	assertCover(allowed, bgm)
	if err := store.SetBookCoverSource(ctx, "union-book", database.CoverSourceAuto); err != nil {
		t.Fatal(err)
	}
	memberCover.IsNSFW = true
	if err := store.UpsertBookCoverCandidates(ctx, []database.BookCoverCandidate{memberCover}); err != nil {
		t.Fatal(err)
	}
	assertCover(allowed, bgm)

	// The union's chain also identifies a higher-priority source when the
	// member's recorded rank is stale.
	memberCover.IsNSFW = false
	memberCover.RankOrder = 2
	memberCover.CoverKind = database.CoverKindHTTPURL
	memberCover.CoverRef = "https://example.invalid/vol.png"
	if err := store.UpsertBookCoverCandidates(ctx, []database.BookCoverCandidate{memberCover}); err != nil {
		t.Fatal(err)
	}
	assertCover(allowed, external)
	if _, err := store.DB().ExecContext(ctx, `
DELETE FROM book_cover_candidates WHERE book_id=? AND source_id=?`, member.ID, memberCover.SourceID); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBookScrapedMetadata(ctx, member.ID, database.BookScrapedMetadata{
		ExtSourceID: memberCover.SourceID, ExtEntryID: memberCover.EntryID, ExtCoverURL: memberCover.CoverRef,
	}); err != nil {
		t.Fatal(err)
	}
	assertCover(allowed, external)
	assertCover(unionOnly, bgm)
	customName, err := s.SaveBookCustomCover(ctx, "union-book", testPNG(t, 45, 60))
	if err != nil {
		t.Fatal(err)
	}
	custom, err := os.ReadFile(filepath.Join(s.bookCoverDir, customName))
	if err != nil {
		t.Fatal(err)
	}
	assertCover(allowed, custom)
}

func TestReadingSecondVolumeCachesThumbnailBelowCustom(t *testing.T) {
	store, first, second := thumbnailTestStore(t)
	ctx := t.Context()
	s, err := New(store, filepath.Join(t.TempDir(), "series"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	page, custom := testPNG(t, 450, 600), testPNG(t, 120, 160)
	if _, err := s.SaveBookCustomCover(ctx, second.ID, custom); err != nil {
		t.Fatal(err)
	}
	before, _, err := s.GetBookCover(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.MaybeGenerate(second, 2, page)
	if _, ok, err := s.readBookThumbnail(ctx, second); err != nil || ok {
		t.Fatalf("page 2 generated a cover: %v %v", ok, err)
	}
	s.MaybeGenerate(second, 1, page)
	s.MaybeGenerate(second, 1, page)
	for deadline := time.Now().Add(3 * time.Second); ; {
		s.inflightMu.Lock()
		count := len(s.inflight)
		s.inflightMu.Unlock()
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cover generation did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	cover, ok, err := s.readBookThumbnail(ctx, second)
	if err != nil || !ok || cover.Width != 225 || cover.Height != 300 {
		t.Fatalf("thumbnail cover=%dx%d found=%v err=%v", cover.Width, cover.Height, ok, err)
	}
	var version string
	if err := store.DB().QueryRow(`SELECT source_version FROM book_thumbnails WHERE book_id=?`, second.ID).Scan(&version); err != nil || version != archive.BookVersion(second) {
		t.Fatalf("thumbnail version=%q err=%v", version, err)
	}
	if _, found, err := s.Get(ctx, first.SeriesID); err != nil || found {
		t.Fatalf("second volume became series cover: found=%v err=%v", found, err)
	}
	after, _, err := s.GetBookCover(ctx, second.ID)
	if err != nil || !bytes.Equal(before.Bytes, after.Bytes) {
		t.Fatalf("reading changed custom cover: %v", err)
	}
}

func TestBookCoverOfflineMatchAndNSFWCacheIsolation(t *testing.T) {
	store, _, book := thumbnailTestStore(t)
	ctx := t.Context()
	s, err := New(store, filepath.Join(t.TempDir(), "series"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO series_metadata(series_id,bgm_subject_id,updated_at) VALUES(?,40,'')`, book.SeriesID); err != nil {
		t.Fatal(err)
	}
	matchCalls, fetchCalls := 0, 0
	s.SetTankobonMatcher(func(_ context.Context, _ database.Series, _ database.SeriesMetadata, books []VolumeMatchBook, allowLive bool) ([]VolumeMatchResult, error) {
		matchCalls++
		if allowLive || len(books) != 2 {
			t.Fatalf("reader matcher received live=%v books=%v", allowLive, books)
		}
		return []VolumeMatchResult{{BookID: book.ID, SubjectID: 42, NSFW: true, NSFWKnown: true}}, nil
	})
	raw := testPNG(t, 30, 40)
	s.SetBangumiCoverFetcher(func(context.Context, string, int64) ([]byte, error) {
		fetchCalls++
		return raw, nil
	})
	scope := database.UserScope{UserID: "reader", AllowedLibraryIDs: []string{book.LibraryID}, AllowNSFW: true}
	restricted := database.WithUserScope(ctx, scope)
	if _, ok, err := s.GetBookCover(restricted, book.ID); err != nil || ok || fetchCalls != 0 {
		t.Fatalf("restricted requester fetched NSFW image: %v %v", ok, err)
	}
	scope.ShowNSFW = true
	if _, ok, err := s.GetBookCover(database.WithUserScope(ctx, scope), book.ID); err != nil || !ok || fetchCalls != 1 {
		t.Fatalf("authorized requester could not fetch image: %v %v", ok, err)
	}
	if _, ok, err := s.GetBookCover(restricted, book.ID); err != nil || ok || matchCalls != 2 || fetchCalls != 1 {
		t.Fatalf("private image served from disk after switching scopes: %v %v", ok, err)
	}
}

func TestUnknownTankobonCoverRequiresPublicVerification(t *testing.T) {
	store, book, _ := thumbnailTestStore(t)
	dir := t.TempDir()
	s, err := NewWithDataDir(store, filepath.Join(dir, "series"), dir, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	raw := testPNG(t, 15, 20)
	publicFetcherCalls, verifiedFetcherCalls := 0, 0
	verificationDenied := errors.New("subject rating could not be verified")
	s.SetPublicBangumiCoverFetcher(func(context.Context, string, int64) ([]byte, error) {
		publicFetcherCalls++
		return raw, nil
	})
	s.SetBangumiCoverFetcher(func(ctx context.Context, _ string, _ int64) ([]byte, error) {
		verifiedFetcherCalls++
		scope, _ := database.UserScopeFromContext(ctx)
		if !scope.EffectiveShowNSFW() {
			return nil, verificationDenied
		}
		return raw, nil
	})
	scope := database.UserScope{UserID: "reader", AllowNSFW: true, ShowNSFW: true}
	match := VolumeMatchResult{BookID: book.ID, SubjectID: 42}
	meta := database.BookMetadata{}
	if cover, _, err := s.CacheBookBangumiCover(database.WithUserScope(t.Context(), scope), book.ID, meta, &match, false); err != nil || len(cover.Bytes) == 0 {
		t.Fatalf("authorized fetch failed: %v", err)
	}
	scope.ShowNSFW = false
	if cover, _, err := s.CacheBookBangumiCover(database.WithUserScope(t.Context(), scope), book.ID, meta, &match, false); err != nil || len(cover.Bytes) != 0 {
		t.Fatalf("unknown cover bypassed public verification: len=%d err=%v", len(cover.Bytes), err)
	}
	if publicFetcherCalls != 0 || verifiedFetcherCalls != 1 {
		t.Fatalf("unchecked fetches=%d checked fetches=%d", publicFetcherCalls, verifiedFetcherCalls)
	}
	if saved, _, err := store.BookMetadataByID(t.Context(), book.ID); err != nil || saved.BgmSubjectID != 0 {
		t.Fatalf("unknown rating was persisted: %+v err=%v", saved, err)
	}
	if _, err := os.Stat(filepath.Join(s.bookBgmDir, "bgm_42_unverified.jpg.public")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unverified cover marked public: %v", err)
	}
}
