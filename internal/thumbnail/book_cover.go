package thumbnail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xJogger/fake-komga-115/internal/archive"
	"github.com/xJogger/fake-komga-115/internal/database"
)

const maxCustomBookCoverSize = 10 << 20

type VolumeMatchBook struct {
	BookID string
	Name   string
	Number string
}

type VolumeMatchResult struct {
	BookID    string
	SubjectID int64
	CoverURL  string
	NSFW      bool
	NSFWKnown bool
}

// TankobonMatcher aligns local volume names with a series' Bangumi volumes.
type TankobonMatcher func(context.Context, database.Series, database.SeriesMetadata, []VolumeMatchBook, bool) ([]VolumeMatchResult, error)

func (s *Service) SetTankobonMatcher(matcher TankobonMatcher) {
	s.externalMu.Lock()
	defer s.externalMu.Unlock()
	s.tankobonMatcher = matcher
}

func (s *Service) MatchTankobons(ctx context.Context, series database.Series, meta database.SeriesMetadata, books []VolumeMatchBook, allowLive bool) ([]VolumeMatchResult, error) {
	s.externalMu.RLock()
	matcher := s.tankobonMatcher
	s.externalMu.RUnlock()
	if matcher == nil || meta.BgmSubjectID == nil || *meta.BgmSubjectID <= 0 || len(books) == 0 {
		return nil, nil
	}
	return matcher(ctx, series, meta, books, allowLive)
}

type bookCoverCandidate struct {
	book database.Book
	meta database.BookMetadata
}

func (s *Service) bookCoverCandidates(ctx context.Context, book database.Book) ([]bookCoverCandidate, error) {
	meta, _, err := s.store.BookMetadataByID(ctx, book.ID)
	sources, _, sourceErr := s.store.UnionBookSourceBooks(ctx, book.ID)
	if err != nil && len(sources) == 0 {
		return nil, err
	}
	if sourceErr != nil {
		return nil, sourceErr
	}
	out := []bookCoverCandidate{{book, meta}}
	if len(sources) == 0 {
		return out, nil
	}
	scope, scoped := database.UserScopeFromContext(ctx)
	for _, source := range sources {
		var enabled, nsfw bool
		err := s.store.DB().QueryRowContext(ctx, `
SELECT l.enabled,coalesce(sm.is_nsfw,0) FROM libraries l
LEFT JOIN series_metadata sm ON sm.series_id=? WHERE l.id=?`, source.SeriesID, source.LibraryID).Scan(&enabled, &nsfw)
		if err != nil || !enabled || (nsfw && !s.store.ShowNSFW(ctx)) {
			continue
		}
		var sourceMeta database.BookMetadata
		if !scoped || scope.AllowsLibrary(source.LibraryID) {
			sourceMeta, _, _ = s.store.BookMetadataByID(ctx, source.ID)
		}
		out = append(out, bookCoverCandidate{source, sourceMeta})
	}
	return out, nil
}

// GetBookCover resolves a visible book's cover using metadata and image caches.
func (s *Service) GetBookCover(ctx context.Context, bookID string) (Data, bool, error) {
	book, err := s.store.BookByID(ctx, bookID)
	if err != nil {
		return Data{}, false, err
	}
	if _, scoped := database.UserScopeFromContext(ctx); !scoped {
		ctx = database.WithUserScope(ctx, database.UserScope{
			UserID: database.PrimaryAdminUserID, IsAdmin: true, AllowNSFW: true, ShowNSFW: s.store.ShowNSFW(ctx),
		})
	}
	candidates, err := s.bookCoverCandidates(ctx, book)
	if err != nil {
		return Data{}, false, err
	}
	mode := candidates[0].meta.CoverSource
	explicit := mode != "" && mode != database.CoverSourceAuto
	for _, source := range []string{"custom", "ext", database.CoverSourceBangumi, database.CoverSourceBook} {
		if explicit && mode != source {
			continue
		}
		for _, candidate := range candidates {
			ownMode := candidate.meta.CoverSource
			if !explicit && ownMode != "" && ownMode != database.CoverSourceAuto && ownMode != source {
				continue
			}
			var data Data
			switch source {
			case "custom", "ext":
				data = s.fetchBookUserCover(ctx, candidate.book, candidate.meta, source)
			case database.CoverSourceBangumi:
				if candidate.meta.BgmSubjectID > 0 {
					data, _ = s.getBookBangumiCover(ctx, candidate.meta)
				}
			case database.CoverSourceBook:
				var err error
				data, _, err = s.readBookThumbnail(ctx, candidate.book)
				if err != nil {
					return Data{}, false, err
				}
			}
			if len(data.Bytes) > 0 {
				return s.visibleBookCover(ctx, book.ID, data)
			}
		}
		if source == database.CoverSourceBangumi && candidates[0].meta.BgmSubjectID <= 0 {
			if data := s.matchBookCover(ctx, book, candidates[0].meta); len(data.Bytes) > 0 {
				return s.visibleBookCover(ctx, book.ID, data)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Data{}, false, err
	}
	return Data{}, false, nil
}

func (s *Service) visibleBookCover(ctx context.Context, id string, data Data) (Data, bool, error) {
	if _, err := s.store.BookByID(ctx, id); err != nil {
		return Data{}, false, err
	}
	return data, true, nil
}

func (s *Service) fetchBookUserCover(ctx context.Context, book database.Book, m database.BookMetadata, source string) Data {
	var sourceID, entryID, parentEntryID, coverURL string
	switch source {
	case "custom":
		if m.CustomCoverPath != "" && filepath.Base(m.CustomCoverPath) == m.CustomCoverPath {
			path := filepath.Join(s.bookCoverDir, m.CustomCoverPath)
			s.bookCoverMu.RLock()
			raw, err := os.ReadFile(path)
			s.bookCoverMu.RUnlock()
			if err == nil && len(raw) > 0 {
				return Data{Bytes: raw, MediaType: "image/jpeg"}
			}
		}
		entryID, coverURL = book.ID, m.CustomCoverURL
	case "ext":
		sourceID, entryID, coverURL = m.ExtSourceID, m.ExtEntryID, m.ExtCoverURL
		if sourceID != "" && coverURL != "" {
			_ = s.store.DB().QueryRowContext(ctx, `
SELECT ext_entry_id FROM series_metadata WHERE series_id=? AND ext_source_id=?`, book.SeriesID, sourceID).Scan(&parentEntryID)
		}
	}
	s.externalMu.RLock()
	fetch := s.bookExternalFetcher
	s.externalMu.RUnlock()
	if fetch != nil && coverURL != "" {
		if raw, mediaType, err := fetch(ctx, sourceID, entryID, parentEntryID, coverURL); err == nil {
			return Data{Bytes: raw, MediaType: mediaType}
		}
	}
	return Data{}
}

func (s *Service) matchBookCover(ctx context.Context, book database.Book, meta database.BookMetadata) Data {
	series, err := s.store.SeriesByID(ctx, book.SeriesID)
	if err != nil {
		return Data{}
	}
	seriesMeta, err := s.store.SeriesMetadataByID(ctx, series)
	if err != nil {
		return Data{}
	}
	rows, err := s.store.DB().QueryContext(ctx, `
SELECT b.id,b.name,coalesce(m.number,'') FROM books b
LEFT JOIN book_metadata m ON m.book_id=b.id WHERE b.series_id=?`, series.ID)
	if err != nil {
		return Data{}
	}
	defer rows.Close()
	var volumes []VolumeMatchBook
	for rows.Next() {
		var volume VolumeMatchBook
		if err := rows.Scan(&volume.BookID, &volume.Name, &volume.Number); err != nil {
			return Data{}
		}
		volumes = append(volumes, volume)
	}
	if rows.Err() != nil {
		return Data{}
	}
	matches, err := s.MatchTankobons(ctx, series, seriesMeta, volumes, false)
	if err != nil {
		return Data{}
	}
	for _, match := range matches {
		if match.BookID == book.ID {
			data, _, _ := s.CacheBookBangumiCover(ctx, book.ID, meta, &match, false)
			return data
		}
	}
	return Data{}
}

// CacheBookBangumiCover resolves an existing binding or an eligible new match.
func (s *Service) CacheBookBangumiCover(ctx context.Context, bookID string, meta database.BookMetadata, match *VolumeMatchResult, force bool) (Data, bool, error) {
	if match != nil && match.SubjectID > 0 && (meta.BgmSubjectID == 0 || force) {
		if !match.NSFWKnown {
			if match.NSFW && !s.store.ShowNSFW(ctx) {
				return Data{}, false, nil
			}
			path := filepath.Join(s.bookBgmDir, fmt.Sprintf("bgm_%d_unverified.jpg", match.SubjectID))
			cached := len(s.readBangumiCover(path, !s.store.ShowNSFW(ctx))) > 0
			data, err := s.fetchBangumiCover(ctx, match.SubjectID, match.CoverURL, path, match.NSFW, false)
			return data, !cached && len(data.Bytes) > 0, err
		}
		var err error
		meta, err = s.bindBookBangumiCover(ctx, bookID, meta.BgmSubjectID, *match, force)
		if err != nil {
			return Data{}, false, err
		}
	}
	if meta.BgmSubjectID <= 0 || (meta.BgmIsNSFW && !s.store.ShowNSFW(ctx)) {
		return Data{}, false, nil
	}
	cached := len(s.readBangumiCover(s.bookBangumiPath(meta.BgmSubjectID), !s.store.ShowNSFW(ctx) || !meta.BgmIsNSFW)) > 0
	data, err := s.getBookBangumiCover(ctx, meta)
	return data, !cached && len(data.Bytes) > 0, err
}

func (s *Service) bindBookBangumiCover(ctx context.Context, bookID string, previousID int64, match VolumeMatchResult, force bool) (database.BookMetadata, error) {
	// Update only the volume binding, with a compare-and-set to preserve
	// manual bindings and matches stored while this lookup was in flight.
	_, err := s.store.DB().ExecContext(ctx, `
INSERT INTO book_metadata(book_id,bgm_subject_id,bgm_cover_url,bgm_is_nsfw,match_origin,updated_at)
SELECT id,?,?,?,'auto',? FROM books WHERE id=?
ON CONFLICT(book_id) DO UPDATE SET
 bgm_subject_id=excluded.bgm_subject_id,bgm_cover_url=excluded.bgm_cover_url,bgm_is_nsfw=excluded.bgm_is_nsfw,
 bgm_cover_path=CASE WHEN book_metadata.bgm_subject_id=excluded.bgm_subject_id
 AND book_metadata.bgm_cover_url=excluded.bgm_cover_url THEN book_metadata.bgm_cover_path ELSE '' END,
 match_origin=CASE WHEN book_metadata.match_origin='' AND book_metadata.ext_entry_id='' THEN 'auto' ELSE book_metadata.match_origin END,
 updated_at=excluded.updated_at
WHERE book_metadata.bgm_subject_id=? AND coalesce(book_metadata.match_origin,'') NOT IN ('manual','agent')
 AND (book_metadata.bgm_subject_id=0 OR (? AND book_metadata.match_origin<>''))`,
		match.SubjectID, match.CoverURL, match.NSFW, time.Now().UTC().Format(time.RFC3339Nano),
		bookID, previousID, force)
	if err != nil {
		return database.BookMetadata{}, err
	}
	meta, _, err := s.store.BookMetadataByID(ctx, bookID)
	return meta, err
}

func (s *Service) getBookBangumiCover(ctx context.Context, meta database.BookMetadata) (Data, error) {
	if meta.BgmIsNSFW && !s.store.ShowNSFW(ctx) {
		return Data{}, nil
	}
	return s.fetchBangumiCover(ctx, meta.BgmSubjectID, meta.BgmCoverURL,
		s.bookBangumiPath(meta.BgmSubjectID), meta.BgmIsNSFW, !meta.BgmIsNSFW)
}

func (s *Service) bookBangumiPath(subjectID int64) string {
	return filepath.Join(s.bookBgmDir, fmt.Sprintf("bgm_%d.jpg", subjectID))
}

type BookExternalCoverFetcher func(ctx context.Context, sourceID, entryID, parentEntryID, coverURL string) ([]byte, string, error)

func (s *Service) SetBookExternalCoverFetcher(fetcher BookExternalCoverFetcher) {
	s.externalMu.Lock()
	defer s.externalMu.Unlock()
	s.bookExternalFetcher = fetcher
}

// HasSourceBookBangumiCover resolves existing bindings on eligible union layers.
func (s *Service) HasSourceBookBangumiCover(ctx context.Context, book database.Book) (bool, error) {
	candidates, err := s.bookCoverCandidates(ctx, book)
	if err != nil {
		return false, err
	}
	for _, candidate := range candidates[1:] {
		if candidate.meta.BgmSubjectID <= 0 {
			continue
		}
		if data, err := s.getBookBangumiCover(ctx, candidate.meta); err == nil && len(data.Bytes) > 0 {
			return true, nil
		}
	}
	return false, ctx.Err()
}

func (s *Service) HasBookUserCover(ctx context.Context, book database.Book) (bool, error) {
	candidates, err := s.bookCoverCandidates(ctx, book)
	if err != nil {
		return false, err
	}
	for _, candidate := range candidates {
		m := candidate.meta
		if m.CustomCoverPath != "" || m.CustomCoverURL != "" || m.ExtCoverURL != "" {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) HasBookThumbnail(ctx context.Context, book database.Book) (bool, error) {
	candidates, err := s.bookCoverCandidates(ctx, book)
	if err != nil {
		return false, err
	}
	for _, candidate := range candidates {
		s.bookThumbnailMu.RLock()
		item, ok, err := s.bookThumbnail(ctx, candidate.book)
		if ok {
			_, err = os.Stat(filepath.Join(s.bookThumbnailDir, filepath.Base(item.Path)))
			ok = err == nil
			if errors.Is(err, fs.ErrNotExist) {
				err = nil
			}
		}
		s.bookThumbnailMu.RUnlock()
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

func (s *Service) bookThumbnail(ctx context.Context, book database.Book) (metadata, bool, error) {
	var item metadata
	err := s.store.DB().QueryRowContext(ctx, `
SELECT path,media_type,width,height FROM book_thumbnails WHERE book_id=? AND source_version=?`,
		book.ID, archive.BookVersion(book)).Scan(&item.Path, &item.MediaType, &item.Width, &item.Height)
	if errors.Is(err, sql.ErrNoRows) {
		return metadata{}, false, nil
	}
	return item, err == nil, err
}

func (s *Service) readBookThumbnail(ctx context.Context, book database.Book) (Data, bool, error) {
	s.bookThumbnailMu.RLock()
	defer s.bookThumbnailMu.RUnlock()
	item, ok, err := s.bookThumbnail(ctx, book)
	if !ok || err != nil {
		return Data{}, false, err
	}
	raw, err := os.ReadFile(filepath.Join(s.bookThumbnailDir, filepath.Base(item.Path)))
	if errors.Is(err, fs.ErrNotExist) {
		return Data{}, false, nil
	}
	return Data{Bytes: raw, MediaType: item.MediaType, Width: item.Width, Height: item.Height}, err == nil, err
}

// GenerateBookThumbnail caches the first page already read by a reader or maintenance job.
func (s *Service) GenerateBookThumbnail(ctx context.Context, book database.Book, page []byte) error {
	started := time.Now()
	s.bookThumbnailMu.Lock()
	defer s.bookThumbnailMu.Unlock()
	current, err := s.store.BookByIDAny(ctx, book.ID)
	if err != nil {
		return err
	}
	version := archive.BookVersion(book)
	if version != archive.BookVersion(current) {
		return nil
	}
	if item, ok, err := s.bookThumbnail(ctx, book); err != nil {
		return err
	} else if ok {
		if _, err := os.Stat(filepath.Join(s.bookThumbnailDir, filepath.Base(item.Path))); err == nil {
			return nil
		}
	}
	jpeg, width, height, err := compress(page)
	if err != nil {
		return err
	}
	name := fileName(book.ID + "\x00" + version)
	target := filepath.Join(s.bookThumbnailDir, name)
	var previous string
	if err := s.store.DB().QueryRowContext(ctx, `SELECT path FROM book_thumbnails WHERE book_id=?`, book.ID).
		Scan(&previous); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeAtomic(target, jpeg); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.store.DB().ExecContext(ctx, `
INSERT INTO book_thumbnails(book_id,source_version,path,media_type,width,height,size,generation_duration_ns,created_at,updated_at)
VALUES(?,?,?,'image/jpeg',?,?,?,?,?,?)
ON CONFLICT(book_id) DO UPDATE SET source_version=excluded.source_version,path=excluded.path,
media_type=excluded.media_type,width=excluded.width,height=excluded.height,size=excluded.size,
generation_duration_ns=excluded.generation_duration_ns,updated_at=excluded.updated_at`,
		book.ID, version, name, width, height, len(jpeg), time.Since(started).Nanoseconds(), now, now)
	if err != nil && previous != name {
		_ = os.Remove(target)
	}
	if err == nil && previous != "" && previous != name {
		_ = os.Remove(filepath.Join(s.bookThumbnailDir, filepath.Base(previous)))
	}
	return err
}

func (s *Service) SaveBookCustomCover(ctx context.Context, bookID string, raw []byte) (string, error) {
	if scope, ok := database.UserScopeFromContext(ctx); ok && !scope.IsAdmin {
		return "", sql.ErrNoRows
	}
	book, err := s.store.BookByID(ctx, bookID)
	if err != nil {
		return "", err
	}
	if len(raw) > maxCustomBookCoverSize {
		return "", errors.New("book cover exceeds 10 MiB")
	}
	jpeg, _, _, err := compressWithMaxEdge(raw, 420)
	if err != nil {
		return "", err
	}
	name := bookCustomCoverName(book.ID)
	target := filepath.Join(s.bookCoverDir, name)
	s.bookCoverMu.Lock()
	defer s.bookCoverMu.Unlock()
	previous, readErr := os.ReadFile(target)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return "", readErr
	}
	if err := writeAtomic(target, jpeg); err != nil {
		return "", err
	}
	if err := s.store.SetBookCustomCover(ctx, book.ID, name, ""); err != nil {
		if readErr == nil {
			return "", errors.Join(err, writeAtomic(target, previous))
		}
		return "", errors.Join(err, os.Remove(target))
	}
	return name, nil
}

func (s *Service) DeleteBookCustomCover(ctx context.Context, bookID string) error {
	if scope, ok := database.UserScopeFromContext(ctx); ok && !scope.IsAdmin {
		return sql.ErrNoRows
	}
	book, err := s.store.BookByID(ctx, bookID)
	if err != nil {
		return err
	}
	s.bookCoverMu.Lock()
	defer s.bookCoverMu.Unlock()
	if err := s.store.ClearBookCustomCover(ctx, book.ID); err != nil {
		return err
	}
	err = os.Remove(filepath.Join(s.bookCoverDir, bookCustomCoverName(book.ID)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func bookCustomCoverName(id string) string {
	if filepath.IsLocal(id) && filepath.Base(id) == id && !strings.ContainsAny(id, `/\:`) {
		return id + ".jpg"
	}
	return fileName(id)
}
