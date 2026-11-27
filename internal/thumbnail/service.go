package thumbnail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
	"golang.org/x/sync/singleflight"

	"github.com/xJogger/fake-komga-115/internal/archive"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/importedcover"
)

type ExternalCoverFetcher func(ctx context.Context, sourceID, entryID, coverURL string) ([]byte, string, error)

type NSFWFallbackCoverFetcher func(ctx context.Context, seriesID string) ([]byte, string, error)

type CoverFetcher func(ctx context.Context, coverURL string, subjectID int64) ([]byte, error)

const (
	defaultMaxEdge         = 300
	jpegQuality            = 75
	maxImagePixels         = 100_000_000
	bgmFailureTTL          = 5 * time.Minute
	bgmTransientFailureTTL = 15 * time.Second
	maxBgmFailures         = 2048
	bgmNSFWCooldown        = 30 * time.Minute
)

var errBangumiCoverUnavailable = errors.New("Bangumi cover unavailable")

type bgmNSFWFailure struct {
	consecutive int
	until       time.Time
}

type Data struct {
	Bytes     []byte
	MediaType string
	Width     int
	Height    int
}

type Stats struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

type SeriesStats struct {
	SourceBookID       string
	SourceVersion      string
	MediaType          string
	Width              int
	Height             int
	Size               int
	GenerationDuration time.Duration
	UpdatedAt          time.Time
}

type Service struct {
	store  *database.Store
	root   string
	bgmDir string
	logger *slog.Logger

	bgmFetcher       CoverFetcher
	publicBgmFetcher CoverFetcher
	bgmFlight        singleflight.Group
	bgmSem           chan struct{}
	bgmMu            sync.RWMutex
	bgmFailures      map[string]time.Time
	bgmNSFWFailures  map[string]bgmNSFWFailure
	bgmUserEpoch     map[string]uint64
	bgmEpoch         uint64

	externalMu             sync.RWMutex
	externalFetcher        ExternalCoverFetcher
	bookExternalFetcher    BookExternalCoverFetcher
	tankobonMatcher        TankobonMatcher
	nsfwFallbackFetcher    NSFWFallbackCoverFetcher
	invalidateNSFWFallback func()

	importedCoversDir string
	bookThumbnailDir  string
	bookCoverDir      string
	bookBgmDir        string
	bookCoverMu       sync.RWMutex
	bookThumbnailMu   sync.RWMutex

	operationMu sync.RWMutex
	inflightMu  sync.Mutex
	inflight    map[string]struct{}
}

func New(store *database.Store, root string, logger *slog.Logger) (*Service, error) {
	return NewWithDataDir(store, root, filepath.Dir(root), logger)
}

func NewWithDataDir(store *database.Store, root, dataDir string, logger *slog.Logger) (*Service, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	bgmDir := filepath.Join(filepath.Dir(root), "bangumi")
	if err := os.MkdirAll(bgmDir, 0o700); err != nil {
		return nil, err
	}
	service := &Service{
		store: store, root: root, bgmDir: bgmDir, logger: logger,
		importedCoversDir: filepath.Join(dataDir, "imported-covers"),
		bookThumbnailDir:  filepath.Join(dataDir, "book-thumbnails"),
		bookCoverDir:      filepath.Join(dataDir, "book-covers"),
		bookBgmDir:        filepath.Join(dataDir, "bangumi"),
		bgmSem:            make(chan struct{}, 6),
		bgmFailures:       make(map[string]time.Time),
		bgmNSFWFailures:   make(map[string]bgmNSFWFailure),
		bgmUserEpoch:      make(map[string]uint64),
		inflight:          make(map[string]struct{}),
	}
	if err := service.cleanupOrphans(context.Background()); err != nil && logger != nil {
		logger.Warn("clean orphan series thumbnails", "error", err)
	}
	return service, nil
}

func (s *Service) SetBangumiCoverFetcher(fetcher CoverFetcher) {
	s.bgmFetcher = fetcher
}

func (s *Service) SetPublicBangumiCoverFetcher(fetcher CoverFetcher) {
	s.publicBgmFetcher = fetcher
}

func (s *Service) SetExternalCoverFetcher(fetcher ExternalCoverFetcher) {
	s.externalMu.Lock()
	defer s.externalMu.Unlock()
	s.externalFetcher = fetcher
	s.bookExternalFetcher = nil
	if fetcher != nil {
		s.bookExternalFetcher = func(ctx context.Context, sourceID, entryID, parentEntryID, coverURL string) ([]byte, string, error) {
			if parentEntryID != "" {
				entryID = parentEntryID
			}
			return fetcher(ctx, sourceID, entryID, coverURL)
		}
	}
}

func (s *Service) SetNSFWFallbackCoverFetcher(fetcher NSFWFallbackCoverFetcher, invalidate func()) {
	s.externalMu.Lock()
	defer s.externalMu.Unlock()
	s.nsfwFallbackFetcher, s.invalidateNSFWFallback = fetcher, invalidate
}

// SetLocalFirstPageFetcher is retained for callers built against the old
// thumbnail API. Archive pages are supplied by a reader or a maintenance task.
func (s *Service) SetLocalFirstPageFetcher(func(context.Context, database.Book) ([]byte, error)) {}

func (s *Service) MaybeGenerate(book database.Book, pageNumber int, page []byte) {
	if pageNumber != 1 || len(page) == 0 {
		return
	}
	key := book.ID + "\x00" + archive.BookVersion(book)
	if !s.claim(key) {
		return
	}
	go func() {
		defer s.release(key)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.GenerateBookThumbnail(ctx, book, page); err != nil && s.logger != nil {
			s.logger.Warn("could not cache book thumbnail", "book", book.ID)
		}
		if err := s.Generate(ctx, book, page); err != nil && s.logger != nil {
			s.logger.Warn("could not cache series thumbnail", "series", book.SeriesID)
		}
	}()
}

func (s *Service) Generate(ctx context.Context, book database.Book, page []byte) error {
	return s.GenerateWithOptions(ctx, book, page, false)
}

func (s *Service) GenerateWithOptions(
	ctx context.Context,
	book database.Book,
	page []byte,
	force bool,
) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	started := time.Now()

	first, err := s.store.FirstBookInSeries(ctx, book.SeriesID)
	if err != nil {
		return err
	}
	if first.ID != book.ID || archive.BookVersion(first) != archive.BookVersion(book) {
		return nil
	}
	version := archive.BookVersion(first)
	if current, ok, err := s.metadata(ctx, book.SeriesID); err != nil {
		return err
	} else if !force && ok && current.SourceBookID == first.ID && current.SourceVersion == version {
		if _, err := os.Stat(s.path(current.Path)); err == nil {
			return nil
		}
	}

	thumbnail, width, height, err := compress(page)
	if err != nil {
		return err
	}
	name := fileName(book.SeriesID)
	if err := writeAtomic(s.path(name), thumbnail); err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.store.DB().ExecContext(ctx, `
INSERT INTO series_thumbnails(
 series_id,source_book_id,source_version,path,media_type,width,height,size,
 generation_duration_ns,created_at,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(series_id) DO UPDATE SET
 source_book_id=excluded.source_book_id,source_version=excluded.source_version,
 path=excluded.path,media_type=excluded.media_type,width=excluded.width,height=excluded.height,
 size=excluded.size,generation_duration_ns=excluded.generation_duration_ns,
 updated_at=excluded.updated_at`,
		book.SeriesID, first.ID, version, name, "image/jpeg", width, height,
		len(thumbnail), time.Since(started).Nanoseconds(), now, now)
	if err != nil {
		_ = os.Remove(s.path(name))
		return err
	}
	if s.logger != nil {
		s.logger.Info("series thumbnail generated", "series", book.SeriesID, "book", book.ID, "width", width, "height", height)
	}
	return nil
}

func (s *Service) SetGenerationDuration(
	ctx context.Context,
	seriesID string,
	duration time.Duration,
) error {
	if duration <= 0 {
		return nil
	}
	_, err := s.store.DB().ExecContext(ctx, `
UPDATE series_thumbnails SET generation_duration_ns=? WHERE series_id=?`,
		duration.Nanoseconds(), seriesID)
	return err
}

func (s *Service) Get(ctx context.Context, seriesID string) (Data, bool, error) {
	series, err := s.store.SeriesByID(ctx, seriesID)
	if err != nil {
		return Data{}, false, err
	}
	seriesID = series.ID
	if preferred := s.getPreferredCover(ctx, seriesID); len(preferred.Bytes) > 0 {
		if _, err := s.store.SeriesByID(ctx, seriesID); err != nil {
			return Data{}, false, err
		}
		return preferred, true, nil
	}

	return s.getBookCover(ctx, seriesID)
}

func (s *Service) getBookCover(ctx context.Context, seriesID string) (Data, bool, error) {
	s.operationMu.RLock()
	defer s.operationMu.RUnlock()

	if data, ok, err := s.getOwnBookCover(ctx, seriesID, false); err != nil || ok {
		return data, ok, err
	}
	// Union library fallback: reuse existing physical source series cover read-only (zero duplicate disk files).
	if fallbackIDs, err := s.store.UnionSeriesCoverFallback(ctx, seriesID); err == nil {
		for _, srcSeriesID := range fallbackIDs {
			if data, ok, fErr := s.getOwnBookCover(ctx, srcSeriesID, true); fErr == nil && ok {
				return data, true, nil
			}
		}
	}
	return Data{}, false, nil
}

func (s *Service) getOwnBookCover(ctx context.Context, seriesID string, includeHidden bool) (Data, bool, error) {
	var (
		first database.Book
		err   error
	)
	if includeHidden {
		first, err = s.store.FirstBookInSeriesAny(ctx, seriesID)
	} else {
		first, err = s.store.FirstBookInSeries(ctx, seriesID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Data{}, false, nil
	}
	if err != nil {
		return Data{}, false, err
	}
	item, ok, err := s.metadata(ctx, seriesID)
	if err != nil {
		return Data{}, false, err
	}
	if !ok {
		return s.readBookThumbnail(ctx, first)
	}
	if item.SourceBookID != first.ID || item.SourceVersion != archive.BookVersion(first) {
		s.delete(ctx, seriesID, item)
		return s.readBookThumbnail(ctx, first)
	}
	data, err := os.ReadFile(s.path(item.Path))
	if errors.Is(err, fs.ErrNotExist) {
		s.delete(ctx, seriesID, item)
		return s.readBookThumbnail(ctx, first)
	}
	if err != nil {
		return Data{}, false, err
	}
	return Data{
		Bytes: data, MediaType: item.MediaType, Width: item.Width, Height: item.Height,
	}, true, nil
}

func (s *Service) GetOrFetchBangumiCover(ctx context.Context, subjectID int64, coverURL string) (Data, error) {
	scope, ok := database.UserScopeFromContext(ctx)
	if !ok || !scope.IsAdmin {
		return Data{}, sql.ErrNoRows
	}
	scope.AllowNSFW, scope.ShowNSFW = true, true
	return s.getOrFetchBangumiCover(database.WithUserScope(ctx, scope), subjectID, coverURL, false)
}

func (s *Service) getOrFetchBangumiCover(ctx context.Context, subjectID int64, previewURL string, nsfwSeries bool) (Data, error) {
	if subjectID <= 0 {
		return Data{}, errors.New("invalid Bangumi subject ID")
	}
	expectedFile := fmt.Sprintf("bgm_%d.jpg", subjectID)
	diskFull := filepath.Join(s.bgmDir, expectedFile)
	if previewURL != "" {
		// A preview URL supplied by the caller must never replace the canonical
		// image subsequently served from a series thumbnail endpoint.
		sum := sha256.Sum256([]byte(previewURL))
		diskFull = filepath.Join(s.bgmDir, fmt.Sprintf("preview_%d_%x.jpg", subjectID, sum))
	}
	return s.fetchBangumiCover(ctx, subjectID, previewURL, diskFull, nsfwSeries, false)
}

func (s *Service) fetchBangumiCover(ctx context.Context, subjectID int64, coverURL, diskFull string, nsfwSeries, knownPublic bool) (Data, error) {
	scope, _ := database.UserScopeFromContext(ctx)
	publicOnly := knownPublic || !scope.EffectiveShowNSFW()
	if raw := s.readBangumiCover(diskFull, publicOnly); len(raw) > 0 {
		return Data{Bytes: raw, MediaType: "image/jpeg"}, nil
	}
	if nsfwSeries && s.bangumiNSFWCoolingDown(scope.UserID) {
		return Data{}, errBangumiCoverUnavailable
	}
	if s.bgmFetcher == nil && (!knownPublic || s.publicBgmFetcher == nil) {
		return Data{}, errors.New("Bangumi cover fetcher is not configured")
	}
	flightKey := fmt.Sprintf("%s:%t:%s", scope.UserID, publicOnly, diskFull)
	result := s.bgmFlight.DoChan(flightKey, func() (any, error) {
		s.bgmMu.RLock()
		failureUntil, epoch, userEpoch := s.bgmFailures[flightKey], s.bgmEpoch, s.bgmUserEpoch[scope.UserID]
		s.bgmMu.RUnlock()
		if raw := s.readBangumiCover(diskFull, publicOnly); len(raw) > 0 {
			return raw, nil
		}
		if time.Now().Before(failureUntil) {
			return nil, errBangumiCoverUnavailable
		}
		select {
		case s.bgmSem <- struct{}{}:
			defer func() { <-s.bgmSem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if raw := s.readBangumiCover(diskFull, publicOnly); len(raw) > 0 {
			return raw, nil
		}
		if nsfwSeries && s.bangumiNSFWCoolingDown(scope.UserID) {
			return nil, errBangumiCoverUnavailable
		}
		dlCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		var fetched []byte
		var err error
		if knownPublic && s.publicBgmFetcher != nil {
			fetched, err = s.publicBgmFetcher(dlCtx, coverURL, subjectID)
		} else {
			if knownPublic {
				scope.AllowNSFW, scope.ShowNSFW = false, false
				dlCtx = database.WithUserScope(dlCtx, scope)
			}
			fetched, err = s.bgmFetcher(dlCtx, coverURL, subjectID)
		}
		if err != nil {
			if nsfwSeries {
				s.recordBangumiNSFWResult(scope.UserID, epoch, userEpoch, err)
			}
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				ttl := bgmTransientFailureTTL
				if isPermanentBangumiCoverError(err) {
					ttl = bgmFailureTTL
				}
				s.cacheBangumiCoverFailure(flightKey, scope.UserID, epoch, userEpoch, ttl)
			}
			return nil, err
		}
		compressed, _, _, err := compressWithMaxEdge(fetched, 420)
		if nsfwSeries {
			s.recordBangumiNSFWResult(scope.UserID, epoch, userEpoch, err)
		}
		if err != nil {
			s.cacheBangumiCoverFailure(flightKey, scope.UserID, epoch, userEpoch, bgmFailureTTL)
			return nil, err
		}
		s.bgmMu.Lock()
		defer s.bgmMu.Unlock()
		if raw := readBangumiCoverFile(diskFull, publicOnly); len(raw) > 0 {
			return raw, nil
		}
		if !publicOnly {
			if err := os.Remove(diskFull + ".public"); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
		if err := writeAtomic(diskFull, compressed); err != nil {
			return nil, err
		}
		if publicOnly {
			if err := writeAtomic(diskFull+".public", nil); err != nil {
				return nil, err
			}
		}
		delete(s.bgmFailures, flightKey)
		return compressed, nil
	})
	select {
	case <-ctx.Done():
		return Data{}, ctx.Err()
	case fetched := <-result:
		if fetched.Err != nil {
			return Data{}, fetched.Err
		}
		return Data{Bytes: fetched.Val.([]byte), MediaType: "image/jpeg"}, nil
	}
}

func (s *Service) bangumiNSFWCoolingDown(userID string) bool {
	s.bgmMu.RLock()
	defer s.bgmMu.RUnlock()
	return time.Now().Before(s.bgmNSFWFailures[userID].until)
}

func (s *Service) recordBangumiNSFWResult(userID string, epoch, userEpoch uint64, err error) {
	s.bgmMu.Lock()
	defer s.bgmMu.Unlock()
	if epoch != s.bgmEpoch || userEpoch != s.bgmUserEpoch[userID] {
		return
	}
	var unavailable interface{ CoverUnavailable() bool }
	failure := s.bgmNSFWFailures[userID]
	now := time.Now()
	if errors.As(err, &unavailable) && unavailable.CoverUnavailable() {
		if now.Before(failure.until) {
			return
		}
		if !failure.until.IsZero() {
			failure = bgmNSFWFailure{}
		}
		failure.consecutive++
		if failure.consecutive >= 2 {
			failure.until = now.Add(bgmNSFWCooldown)
		}
		s.bgmNSFWFailures[userID] = failure
	} else if err == nil || !now.Before(failure.until) {
		delete(s.bgmNSFWFailures, userID)
	}
}

func (s *Service) readBangumiCover(path string, publicOnly bool) []byte {
	s.bgmMu.RLock()
	defer s.bgmMu.RUnlock()
	return readBangumiCoverFile(path, publicOnly)
}

func readBangumiCoverFile(path string, publicOnly bool) []byte {
	if publicOnly {
		if _, err := os.Stat(path + ".public"); err != nil {
			return nil
		}
	}
	raw, _ := os.ReadFile(path)
	return raw
}

func isPermanentBangumiCoverError(err error) bool {
	var unavailable interface{ CoverUnavailable() bool }
	return errors.As(err, &unavailable) && unavailable.CoverUnavailable()
}

func (s *Service) cacheBangumiCoverFailure(key, userID string, epoch, userEpoch uint64, ttl time.Duration) {
	s.bgmMu.Lock()
	defer s.bgmMu.Unlock()
	if epoch != s.bgmEpoch || userEpoch != s.bgmUserEpoch[userID] {
		return
	}
	now := time.Now()
	if len(s.bgmFailures) >= maxBgmFailures {
		for existing, until := range s.bgmFailures {
			if !now.Before(until) {
				delete(s.bgmFailures, existing)
			}
		}
		if len(s.bgmFailures) >= maxBgmFailures {
			for existing := range s.bgmFailures {
				delete(s.bgmFailures, existing)
				break
			}
		}
	}
	s.bgmFailures[key] = now.Add(ttl)
}

func (s *Service) InvalidateBangumiCoverFailures() {
	s.bgmMu.Lock()
	s.invalidateBangumiCoverFailuresLocked()
	s.bgmMu.Unlock()
	s.invalidateFallbackCoverCache()
}

func (s *Service) InvalidateUserBangumiCoverFailures(userID string) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		s.InvalidateBangumiCoverFailures()
		return
	}
	prefix := userID + ":"
	s.bgmMu.Lock()
	for key := range s.bgmFailures {
		if strings.HasPrefix(key, prefix) {
			delete(s.bgmFailures, key)
		}
	}
	delete(s.bgmNSFWFailures, userID)
	s.bgmUserEpoch[userID]++
	s.bgmMu.Unlock()
}

func (s *Service) invalidateBangumiCoverFailuresLocked() {
	clear(s.bgmFailures)
	clear(s.bgmNSFWFailures)
	s.bgmEpoch++
}

func (s *Service) invalidateFallbackCoverCache() {
	s.externalMu.RLock()
	invalidate := s.invalidateNSFWFallback
	s.externalMu.RUnlock()
	if invalidate != nil {
		invalidate()
	}
}

func (s *Service) importedCover(ref string) Data {
	raw, mediaType, _ := importedcover.Read(s.importedCoversDir, ref)
	return Data{Bytes: raw, MediaType: mediaType}
}

func (s *Service) coverRatingAllowed(ctx context.Context, nsfw, known bool) bool {
	return s.store.ShowNSFW(ctx) || (known && !nsfw)
}

func (s *Service) getPreferredCover(ctx context.Context, seriesID string) Data {
	var bgmID sql.NullInt64
	var mode, coverPath, sourceID, entryID, coverURL string
	var nsfw bool
	err := s.store.DB().QueryRowContext(ctx, `
SELECT bgm_subject_id,cover_source,bgm_cover_path,ext_source_id,ext_entry_id,ext_cover_url,is_nsfw
FROM series_metadata WHERE series_id=?`, seriesID).Scan(&bgmID, &mode, &coverPath, &sourceID, &entryID, &coverURL, &nsfw)
	if err != nil {
		return Data{}
	}
	if _, scoped := database.UserScopeFromContext(ctx); !scoped {
		ctx = database.WithUserScope(ctx, database.UserScope{UserID: database.PrimaryAdminUserID, IsAdmin: true, AllowNSFW: true, ShowNSFW: s.store.ShowNSFW(ctx)})
	}
	candidates, err := s.store.ListSeriesCoverCandidates(ctx, seriesID)
	if err != nil {
		return Data{}
	}
	s.externalMu.RLock()
	fetch := s.externalFetcher
	fallback := s.nsfwFallbackFetcher
	s.externalMu.RUnlock()

	fetchExternal := func(sourceID, entryID, ref string) Data {
		if fetch == nil || sourceID == "" || entryID == "" || ref == "" {
			return Data{}
		}
		raw, mediaType, _ := fetch(ctx, sourceID, entryID, ref)
		return Data{Bytes: raw, MediaType: mediaType}
	}
	fetchBangumi := func(subjectID int64, isNSFW bool) Data {
		data, err := s.getOrFetchBangumiCover(ctx, subjectID, "", isNSFW && s.store.ShowNSFW(ctx))
		if err == nil && subjectID == bgmID.Int64 {
			expected := fmt.Sprintf("bgm_%d.jpg", subjectID)
			if coverPath != expected {
				_ = s.store.SetSeriesBgmCoverPath(ctx, seriesID, subjectID, expected)
			}
		}
		return data
	}
	fetchCandidate := func(c database.SeriesCoverCandidate) Data {
		if !s.coverRatingAllowed(ctx, c.IsNSFW, c.NSFWKnown) {
			return Data{}
		}
		switch c.CoverKind {
		case database.CoverKindImportedFile:
			return s.importedCover(c.CoverRef)
		case database.CoverKindBangumi:
			id, _ := strconv.ParseInt(c.EntryID, 10, 64)
			return fetchBangumi(id, c.IsNSFW)
		case database.CoverKindHTTPURL:
			return fetchExternal(c.SourceID, c.EntryID, c.CoverRef)
		}
		return Data{}
	}
	for _, candidate := range candidates {
		if candidate.SourceID == database.CoverCandidateSourceManual {
			if data := fetchCandidate(candidate); len(data.Bytes) > 0 {
				return data
			}
		}
	}
	if mode == database.CoverSourceBook {
		return Data{}
	}
	preferred := func(c database.SeriesCoverCandidate) bool {
		switch mode {
		case database.CoverSourceBangumi:
			return c.CoverKind == database.CoverKindBangumi
		case "external", "ext":
			return c.CoverKind != database.CoverKindBangumi
		default:
			return true
		}
	}
	for _, candidate := range candidates {
		if candidate.SourceID != database.CoverCandidateSourceManual && preferred(candidate) {
			if data := fetchCandidate(candidate); len(data.Bytes) > 0 {
				return data
			}
		}
	}
	// An explicit legacy Bangumi choice still applies when a series gains its first external candidate.
	if mode == database.CoverSourceBangumi && bgmID.Int64 > 0 &&
		!slices.ContainsFunc(candidates, func(c database.SeriesCoverCandidate) bool { return c.CoverKind == database.CoverKindBangumi }) {
		if data := fetchBangumi(bgmID.Int64, nsfw); len(data.Bytes) > 0 {
			return data
		}
	}
	for _, candidate := range candidates {
		if candidate.SourceID != database.CoverCandidateSourceManual && !preferred(candidate) {
			if data := fetchCandidate(candidate); len(data.Bytes) > 0 {
				return data
			}
		}
	}
	if len(candidates) == 0 {
		if mode != database.CoverSourceBangumi {
			if data := fetchExternal(sourceID, entryID, coverURL); len(data.Bytes) > 0 {
				return data
			}
			if bgmID.Int64 > 0 {
				if data := fetchBangumi(bgmID.Int64, nsfw); len(data.Bytes) > 0 {
					return data
				}
			}
		} else if coverURL != "" {
			if data := fetchExternal(sourceID, entryID, coverURL); len(data.Bytes) > 0 {
				return data
			}
		}
	}
	if nsfw && bgmID.Int64 > 0 && s.store.ShowNSFW(ctx) && fallback != nil {
		raw, mediaType, err := fallback(ctx, seriesID)
		if err == nil {
			if _, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
				return Data{Bytes: raw, MediaType: mediaType}
			}
		}
	}
	return Data{}
}

func (s *Service) BangumiStats(ctx context.Context) (Stats, error) {
	s.bgmMu.RLock()
	defer s.bgmMu.RUnlock()
	var st Stats
	dirs := []string{s.bgmDir}
	if s.bgmDir != s.bookBgmDir {
		dirs = append(dirs, s.bookBgmDir)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return st, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || filepath.Ext(name) != ".jpg" || (dir != s.bgmDir && !isBangumiCoverFile(name)) {
				continue
			}
			if info, err := entry.Info(); err == nil {
				st.Files++
				st.Bytes += info.Size()
			}
		}
	}
	return st, nil
}

func (s *Service) ClearBangumiCovers(ctx context.Context) error {
	defer s.invalidateFallbackCoverCache()
	s.bgmMu.Lock()
	defer s.bgmMu.Unlock()
	s.invalidateBangumiCoverFailuresLocked()
	if err := os.RemoveAll(s.bgmDir); err != nil {
		return err
	}
	if err := os.MkdirAll(s.bgmDir, 0o700); err != nil {
		return err
	}
	if s.bookBgmDir != s.bgmDir {
		entries, err := os.ReadDir(s.bookBgmDir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !isBangumiCoverFile(strings.TrimSuffix(entry.Name(), ".public")) {
				continue
			}
			if err := os.Remove(filepath.Join(s.bookBgmDir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	_, err := s.store.DB().ExecContext(ctx, `UPDATE series_metadata SET bgm_cover_path='' WHERE bgm_cover_path<>''`)
	return err
}

func isBangumiCoverFile(name string) bool {
	if !strings.HasPrefix(name, "bgm_") || !strings.HasSuffix(name, ".jpg") {
		return false
	}
	id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(name, "bgm_"), ".jpg"), "_unverified"), 10, 64)
	return err == nil && id > 0
}

func (s *Service) HasValid(ctx context.Context, seriesID string) (bool, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	if ok, err := s.hasValidOwnLocked(ctx, seriesID, false); err != nil || ok {
		return ok, err
	}
	if fallbackIDs, err := s.store.UnionSeriesCoverFallback(ctx, seriesID); err == nil {
		for _, srcSeriesID := range fallbackIDs {
			if ok, fErr := s.hasValidOwnLocked(ctx, srcSeriesID, true); fErr == nil && ok {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *Service) hasValidOwnLocked(ctx context.Context, seriesID string, includeHidden bool) (bool, error) {
	var (
		first database.Book
		err   error
	)
	if includeHidden {
		first, err = s.store.FirstBookInSeriesAny(ctx, seriesID)
	} else {
		first, err = s.store.FirstBookInSeries(ctx, seriesID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	item, ok, err := s.metadata(ctx, seriesID)
	if err != nil || !ok {
		return false, err
	}
	if item.SourceBookID != first.ID || item.SourceVersion != archive.BookVersion(first) {
		s.delete(ctx, seriesID, item)
		return false, nil
	}
	if _, err := os.Stat(s.path(item.Path)); errors.Is(err, fs.ErrNotExist) {
		s.delete(ctx, seriesID, item)
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) Stats(ctx context.Context) (Stats, error) {
	var stats Stats
	err := s.store.DB().QueryRowContext(ctx,
		`SELECT count(*),coalesce(sum(size),0) FROM series_thumbnails`).
		Scan(&stats.Files, &stats.Bytes)
	return stats, err
}

func (s *Service) SeriesStats(
	ctx context.Context,
	seriesID string,
) (SeriesStats, bool, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	first, err := s.store.FirstBookInSeries(ctx, seriesID)
	if errors.Is(err, sql.ErrNoRows) {
		return SeriesStats{}, false, nil
	}
	if err != nil {
		return SeriesStats{}, false, err
	}
	item, ok, err := s.metadata(ctx, seriesID)
	if err != nil || !ok {
		return SeriesStats{}, false, err
	}
	if item.SourceBookID != first.ID || item.SourceVersion != archive.BookVersion(first) {
		s.delete(ctx, seriesID, item)
		return SeriesStats{}, false, nil
	}
	if _, err := os.Stat(s.path(item.Path)); errors.Is(err, fs.ErrNotExist) {
		s.delete(ctx, seriesID, item)
		return SeriesStats{}, false, nil
	} else if err != nil {
		return SeriesStats{}, false, err
	}
	return SeriesStats{
		SourceBookID:       item.SourceBookID,
		SourceVersion:      item.SourceVersion,
		MediaType:          item.MediaType,
		Width:              item.Width,
		Height:             item.Height,
		Size:               item.Size,
		GenerationDuration: item.GenerationDuration,
		UpdatedAt:          item.UpdatedAt,
	}, true, nil
}

func (s *Service) Clear(ctx context.Context) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if err := os.RemoveAll(s.root); err != nil {
		return err
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return err
	}
	_, err := s.store.DB().ExecContext(ctx, `DELETE FROM series_thumbnails`)
	return err
}

type metadata struct {
	SourceBookID       string
	SourceVersion      string
	Path               string
	MediaType          string
	Width              int
	Height             int
	Size               int
	GenerationDuration time.Duration
	UpdatedAt          time.Time
}

func (s *Service) metadata(ctx context.Context, seriesID string) (metadata, bool, error) {
	var item metadata
	var durationNS int64
	var updated string
	err := s.store.DB().QueryRowContext(ctx, `
SELECT source_book_id,source_version,path,media_type,width,height,size,generation_duration_ns,updated_at
FROM series_thumbnails WHERE series_id=?`, seriesID).Scan(
		&item.SourceBookID, &item.SourceVersion, &item.Path,
		&item.MediaType, &item.Width, &item.Height, &item.Size, &durationNS, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return metadata{}, false, nil
	}
	item.GenerationDuration = time.Duration(durationNS)
	item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return item, err == nil, err
}

func (s *Service) delete(ctx context.Context, seriesID string, item metadata) {
	_ = os.Remove(s.path(item.Path))
	_, _ = s.store.DB().ExecContext(ctx,
		`DELETE FROM series_thumbnails WHERE series_id=?`, seriesID)
}

func (s *Service) path(name string) string {
	return filepath.Join(s.root, filepath.Base(name))
}

func (s *Service) claim(seriesID string) bool {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if _, exists := s.inflight[seriesID]; exists {
		return false
	}
	s.inflight[seriesID] = struct{}{}
	return true
}

func (s *Service) release(seriesID string) {
	s.inflightMu.Lock()
	delete(s.inflight, seriesID)
	s.inflightMu.Unlock()
}

func (s *Service) cleanupOrphans(ctx context.Context) error {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT path FROM series_thumbnails`)
	if err != nil {
		return err
	}
	known := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		known[filepath.Base(name)] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, ok := known[entry.Name()]; !ok {
			_ = os.Remove(filepath.Join(s.root, entry.Name()))
		}
	}
	return nil
}

func fileName(seriesID string) string {
	sum := sha256.Sum256([]byte(seriesID))
	return hex.EncodeToString(sum[:]) + ".jpg"
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".thumbnail-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func compress(data []byte) ([]byte, int, int, error) {
	return compressWithMaxEdge(data, defaultMaxEdge)
}

func compressWithMaxEdge(data []byte, maxEdge int) ([]byte, int, int, error) {
	if maxEdge <= 0 {
		maxEdge = defaultMaxEdge
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode cover image configuration: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 ||
		int64(config.Width)*int64(config.Height) > maxImagePixels {
		return nil, 0, 0, errors.New("cover image dimensions are invalid or too large")
	}
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode cover image: %w", err)
	}
	width, height := scaledSize(config.Width, config.Height, maxEdge)
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	stddraw.Draw(target, target.Bounds(), &image.Uniform{C: color.White}, image.Point{}, stddraw.Src)
	xdraw.CatmullRom.Scale(target, target.Bounds(), source, source.Bounds(), stddraw.Over, nil)

	var output bytes.Buffer
	if err := jpeg.Encode(&output, target, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, 0, 0, err
	}
	return output.Bytes(), width, height, nil
}

func scaledSize(width, height, maxEdge int) (int, int) {
	if width <= maxEdge && height <= maxEdge {
		return width, height
	}
	if width >= height {
		return maxEdge, max(1, height*maxEdge/width)
	}
	return max(1, width*maxEdge/height), maxEdge
}
