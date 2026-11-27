package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/xJogger/fake-komga-115/internal/archive"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/thumbnail"
)

const (
	OperationBatchBookCovers = "batch_book_covers"
	OperationBatchComicInfo  = "batch_comicinfo"
	TargetLibrary            = "library"
	batchPageSize            = 128
)

type JobOptions struct {
	FallbackPage1 bool `json:"fallbackPage1"`
	ExtractPage1  bool `json:"extractPage1"`
	Force         bool `json:"force"`
}

// StartJob accepts legacy maintenance operations and the explicit volume batches.
func (m *Manager) StartJob(ctx context.Context, operation, targetType, targetID string, options JobOptions) (Run, error) {
	switch {
	case operation == OperationBookIndex && targetType == TargetBook:
		return m.StartBookIndex(ctx, targetID, options.Force)
	case operation == OperationSeriesIndex && targetType == TargetSeries:
		return m.StartSeriesIndex(ctx, targetID, options.Force)
	case operation == OperationSeriesThumbnail && targetType == TargetSeries:
		return m.StartSeriesThumbnail(ctx, targetID, options.Force)
	case operation != OperationBatchBookCovers && operation != OperationBatchComicInfo:
		return Run{}, ErrInvalidOperation
	}
	if scope, ok := database.UserScopeFromContext(ctx); ok && !scope.IsAdmin {
		return Run{}, sql.ErrNoRows
	}
	switch targetType {
	case TargetSeries:
		series, err := m.store.SeriesByID(ctx, targetID)
		if err != nil {
			return Run{}, err
		}
		return m.enqueue(ctx, operation, targetType, series.ID, series.Name, series.ID, "", options, series.BooksCount)
	case TargetLibrary:
		library, err := m.batchLibrary(ctx, targetID)
		if err != nil {
			return Run{}, err
		}
		var count int
		if err := m.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM books WHERE library_id=?`, library.ID).Scan(&count); err != nil {
			return Run{}, err
		}
		return m.enqueue(ctx, operation, targetType, library.ID, library.Name, "", "", options, count)
	default:
		return Run{}, ErrInvalidOperation
	}
}

func (m *Manager) Enqueue(ctx context.Context, operation, targetType, targetID string, options JobOptions) (Run, error) {
	return m.StartJob(ctx, operation, targetType, targetID, options)
}

func (m *Manager) batchLibrary(ctx context.Context, id string) (database.Library, error) {
	library, err := m.store.LibraryBasic(ctx, id)
	if err == nil && (!library.Enabled || library.UnionParentID != "") {
		return database.Library{}, sql.ErrNoRows
	}
	return library, err
}

func (m *Manager) executeBookBatch(ctx context.Context, run *Run, options JobOptions) {
	ctx = database.WithUserScope(ctx, database.UserScope{
		UserID: database.PrimaryAdminUserID, IsAdmin: true, AllowNSFW: true, ShowNSFW: true,
	})
	var err error
	if run.TargetType == TargetSeries {
		err = m.batchSeries(ctx, run, run.SeriesID, options)
	} else if _, err = m.batchLibrary(ctx, run.TargetID); err == nil {
		err = m.eachLibrarySeries(ctx, run.TargetID, func(id string) error {
			err := m.batchSeries(ctx, run, id, options)
			if errors.Is(err, sql.ErrNoRows) {
				return nil // The series was removed after the cursor page was read.
			}
			return err
		})
	}
	switch {
	case ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, sql.ErrNoRows):
		m.finish(*run, "canceled")
	case err != nil:
		run.Errors = append(run.Errors, safeError(err))
		m.finish(*run, "failed")
	default:
		m.finishWithCounts(*run)
	}
}

func (m *Manager) batchSeries(ctx context.Context, run *Run, seriesID string, options JobOptions) error {
	series, err := m.store.SeriesByID(ctx, seriesID)
	if err != nil {
		return err
	}
	var matches map[string]thumbnail.VolumeMatchResult
	var matchErr error
	if run.Operation == OperationBatchBookCovers && !options.FallbackPage1 {
		matches, matchErr = m.matchSeriesVolumes(ctx, series)
	}
	return m.eachBookPage(ctx, series, func(books []database.Book) error {
		var metas map[string]database.BookMetadata
		if run.Operation == OperationBatchBookCovers && !options.FallbackPage1 {
			metas, err = m.bookMetadataPage(ctx, books)
			if err != nil {
				return err
			}
		}
		lastUpdate := time.Now()
		for _, book := range books {
			if err := ctx.Err(); err != nil {
				return err
			}
			var generated bool
			var err error
			run.CurrentItem = book.Name
			switch run.Operation {
			case OperationBatchBookCovers:
				if options.FallbackPage1 {
					var hasCover bool
					if !options.Force {
						hasCover, err = m.thumbs.HasBookMetadataCover(ctx, book)
					}
					if err == nil && !hasCover {
						generated, err = m.batchPageOne(ctx, book, options.Force)
					}
					break
				}
				meta := metas[book.ID]
				match, found := matches[book.ID]
				var candidate *thumbnail.VolumeMatchResult
				if found {
					candidate = &match
				}
				generated, err = m.batchBookCover(ctx, book, meta, candidate, matchErr, options)
			case OperationBatchComicInfo:
				generated, err = m.batchBookComicInfo(ctx, book, options)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			recordBookResult(run, book, generated, err)
			if time.Since(lastUpdate) >= time.Second {
				if err := m.updateProgress(ctx, run); err != nil {
					return err
				}
				lastUpdate = time.Now()
			}
		}
		return m.updateProgress(ctx, run)
	})
}

func (m *Manager) matchSeriesVolumes(ctx context.Context, series database.Series) (map[string]thumbnail.VolumeMatchResult, error) {
	meta, err := m.store.SeriesMetadataByID(ctx, series)
	if err != nil || meta.BgmSubjectID == nil || *meta.BgmSubjectID <= 0 {
		return nil, err
	}
	var volumes []thumbnail.VolumeMatchBook
	err = m.eachBookPage(ctx, series, func(books []database.Book) error {
		metas, err := m.bookMetadataPage(ctx, books)
		if err != nil {
			return err
		}
		for _, book := range books {
			volumes = append(volumes, thumbnail.VolumeMatchBook{BookID: book.ID, Name: book.Name, Number: metas[book.ID].Number})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	results, err := m.thumbs.MatchTankobons(ctx, series, meta, volumes, true)
	matches := make(map[string]thumbnail.VolumeMatchResult, len(results))
	for _, result := range results {
		matches[result.BookID] = result
	}
	return matches, err
}

func (m *Manager) batchBookCover(ctx context.Context, book database.Book, meta database.BookMetadata, match *thumbnail.VolumeMatchResult, matchErr error, options JobOptions) (bool, error) {
	hasUserCover, err := m.thumbs.HasBookUserCover(ctx, book)
	if err != nil || (hasUserCover && !options.Force) {
		return false, err
	}
	// A Live relation without a confirmed rating cannot replace an existing binding.
	if match != nil && !match.NSFWKnown && meta.BgmSubjectID > 0 {
		match = nil
	}
	cover, generated, coverErr := m.thumbs.CacheBookBangumiCover(ctx, book.ID, meta, match, options.Force)
	if coverErr == nil && len(cover.Bytes) > 0 {
		return generated, nil
	}

	if coverErr != nil {
		return false, coverErr
	}
	return false, matchErr
}

func (m *Manager) batchBookComicInfo(ctx context.Context, book database.Book, options JobOptions) (bool, error) {
	status, parsed, err := m.archive.ExtractBookComicInfoWithOptions(ctx, book, options.Force)
	if err != nil {
		return parsed, err
	}
	if !options.ExtractPage1 || status == archive.ComicInfoSkippedSolid ||
		status == archive.ComicInfoSkippedEncrypted || status == archive.ComicInfoSkippedUnsupported {
		return parsed, nil
	}
	generated, pageErr := m.batchPageOne(ctx, book, false)
	return parsed || generated, pageErr
}

func (m *Manager) batchPageOne(ctx context.Context, book database.Book, force bool) (bool, error) {
	if !force {
		valid, err := m.thumbs.HasBookThumbnail(ctx, book)
		if err != nil || valid {
			return false, err
		}
	}
	page, err := m.archive.ReadPage(archive.WithoutComicInfoCaching(ctx), book, 1)
	switch {
	case errors.Is(err, archive.ErrSolidRAR), errors.Is(err, archive.ErrArchivePasswordRequired),
		errors.Is(err, archive.ErrArchivePasswordInvalid), errors.Is(err, archive.ErrUnsupportedArchive),
		errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	if err := m.thumbs.GenerateBookThumbnailWithOptions(ctx, book, page.Data, force); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) bookMetadataPage(ctx context.Context, books []database.Book) (map[string]database.BookMetadata, error) {
	ids := make([]string, len(books))
	for i, book := range books {
		ids[i] = book.ID
	}
	return m.store.BookMetadataMap(ctx, ids)
}

func (m *Manager) eachLibrarySeries(ctx context.Context, id string, visit func(string) error) error {
	var lastName string
	var lastRow int64
	for {
		rows, err := m.store.DB().QueryContext(ctx, `
SELECT s.id,s.name,s.rowid FROM series s JOIN libraries l ON l.id=s.library_id AND l.enabled=1
WHERE s.library_id=? AND (s.name,s.rowid)>(?,?)
ORDER BY s.name,s.rowid LIMIT ?`, id, lastName, lastRow, batchPageSize)
		if err != nil {
			return err
		}
		ids := make([]string, 0, batchPageSize)
		for rows.Next() {
			var seriesID string
			if err := rows.Scan(&seriesID, &lastName, &lastRow); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, seriesID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, seriesID := range ids {
			if err := visit(seriesID); err != nil {
				return err
			}
		}
		if len(ids) < batchPageSize {
			return nil
		}
	}
}

func (m *Manager) eachBookPage(ctx context.Context, series database.Series, visit func([]database.Book) error) error {
	var lastNumber float64
	var lastRow int64
	for {
		query := `
SELECT b.id,b.file_id,b.name,b.size,b.sha1,b.file_modified_at,b.parent_cid,b.pick_code,b.number_sort,b.rowid
FROM books b JOIN libraries l ON l.id=b.library_id AND l.enabled=1 WHERE b.series_id=?`
		args := []any{series.ID}
		if lastRow != 0 {
			query += ` AND (b.number_sort,b.rowid)>(?,?)`
			args = append(args, lastNumber, lastRow)
		}
		query += ` ORDER BY b.number_sort,b.rowid LIMIT ?`
		args = append(args, batchPageSize)
		rows, err := m.store.DB().QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		books := make([]database.Book, 0, batchPageSize)
		for rows.Next() {
			book := database.Book{SeriesID: series.ID, LibraryID: series.LibraryID}
			var modified sql.NullString
			if err := rows.Scan(&book.ID, &book.FileID, &book.Name, &book.Size, &book.SHA1,
				&modified, &book.ParentCID, &book.PickCode, &lastNumber, &lastRow); err != nil {
				rows.Close()
				return err
			}
			if modified.Valid && modified.String != "" {
				value, err := time.Parse(time.RFC3339Nano, modified.String)
				if err != nil {
					value = time.Unix(0, 0).UTC()
				}
				book.FileModifiedAt = &value
			}
			book.NumberSort = lastNumber
			books = append(books, book)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(books) > 0 {
			if err := visit(books); err != nil {
				return err
			}
		}
		if len(books) < batchPageSize {
			return nil
		}
	}
}
