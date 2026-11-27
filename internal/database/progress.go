package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xJogger/fake-komga-115/internal/cjk"
)

func (s *Store) SeriesReadProgress(ctx context.Context, seriesID string) (SeriesReadProgress, error) {
	userID := UserIDFromContext(ctx)
	rows, err := s.db.QueryContext(ctx, `
SELECT b.effective_number_sort, CASE WHEN p.book_id IS NULL THEN 0 ELSE 1 END AS has_progress,
 coalesce(p.completed,0)
FROM books b
LEFT JOIN book_read_progress p ON p.user_id=? AND p.book_id=b.id
WHERE b.series_id=?
ORDER BY b.effective_number_sort ASC,b.name COLLATE NOCASE ASC,b.id ASC`, userID, seriesID)
	if err != nil {
		return SeriesReadProgress{}, err
	}
	defer rows.Close()

	var progress SeriesReadProgress
	continuous := true
	for rows.Next() {
		var numberSort float64
		var hasProgress, completed int
		if err := rows.Scan(&numberSort, &hasProgress, &completed); err != nil {
			return SeriesReadProgress{}, err
		}
		progress.BooksCount++
		progress.MaxNumberSort = numberSort
		switch {
		case hasProgress != 0 && completed != 0:
			progress.BooksReadCount++
			if continuous {
				progress.LastReadContinuousNumberSort = numberSort
			}
		case hasProgress != 0:
			progress.BooksInProgressCount++
			continuous = false
		default:
			continuous = false
		}
	}
	if err := rows.Err(); err != nil {
		return SeriesReadProgress{}, err
	}
	progress.BooksUnreadCount = progress.BooksCount -
		progress.BooksReadCount - progress.BooksInProgressCount
	return progress, nil
}

func (s *Store) SeriesReadProgresses(
	ctx context.Context,
	seriesIDs []string,
) (map[string]SeriesReadProgress, error) {
	out := make(map[string]SeriesReadProgress, len(seriesIDs))
	if len(seriesIDs) == 0 {
		return out, nil
	}
	userID := UserIDFromContext(ctx)
	unique := cleanStringSliceCaseSensitive(seriesIDs)
	continuousMap := make(map[string]bool, len(unique))
	for _, id := range unique {
		continuousMap[id] = true
	}
	for _, chunk := range chunkStrings(unique, sqlInChunkSize) {
		args := make([]any, 0, 1+len(chunk))
		args = append(args, userID)
		for _, id := range chunk {
			args = append(args, id)
		}
		query := `
SELECT b.series_id, b.effective_number_sort,
 CASE WHEN p.book_id IS NULL THEN 0 ELSE 1 END AS has_progress,
 coalesce(p.completed,0)
FROM books b
LEFT JOIN book_read_progress p ON p.user_id=? AND p.book_id=b.id
WHERE b.series_id IN ` + Placeholders(len(chunk)) + `
ORDER BY b.series_id ASC,b.effective_number_sort ASC,b.name COLLATE NOCASE ASC,b.id ASC`
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var seriesID string
			var numberSort float64
			var hasProgress, completed int
			if err := rows.Scan(&seriesID, &numberSort, &hasProgress, &completed); err != nil {
				rows.Close()
				return nil, err
			}
			progress := out[seriesID]
			progress.BooksCount++
			progress.MaxNumberSort = numberSort
			switch {
			case hasProgress != 0 && completed != 0:
				progress.BooksReadCount++
				if continuousMap[seriesID] {
					progress.LastReadContinuousNumberSort = numberSort
				}
			case hasProgress != 0:
				progress.BooksInProgressCount++
				continuousMap[seriesID] = false
			default:
				continuousMap[seriesID] = false
			}
			out[seriesID] = progress
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	for id, progress := range out {
		progress.BooksUnreadCount = progress.BooksCount -
			progress.BooksReadCount - progress.BooksInProgressCount
		out[id] = progress
	}
	return out, nil
}

func tombstoneCutoffText() string {
	return time.Now().UTC().Add(-progressTombstoneRetention).Format(time.RFC3339Nano)
}

func (s *Store) DeleteSeriesReadProgress(ctx context.Context, seriesID string) error {
	userID := UserIDFromContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowText()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO book_read_progress_tombstones(user_id,book_id,series_id,deleted_at)
SELECT p.user_id,p.book_id,p.series_id,?
FROM book_read_progress p
WHERE p.user_id=? AND (p.series_id=? OR p.book_id IN (
  SELECT ubs.source_book_id FROM union_book_sources ubs
  JOIN books ub ON ub.id=ubs.union_book_id WHERE ub.series_id=?
))
ON CONFLICT(user_id,book_id) DO UPDATE SET
 series_id=excluded.series_id,
 deleted_at=excluded.deleted_at`, now, userID, seriesID, seriesID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM book_read_progress
WHERE user_id=? AND (series_id=? OR book_id IN (
  SELECT ubs.source_book_id FROM union_book_sources ubs
  JOIN books ub ON ub.id=ubs.union_book_id WHERE ub.series_id=?
))`, userID, seriesID, seriesID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM book_read_progress_tombstones WHERE deleted_at<?`, tombstoneCutoffText()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkSeriesCompleted(ctx context.Context, seriesID string) error {
	userID := UserIDFromContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowText()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO book_read_progress(user_id,book_id,series_id,completed,read_date,completed_at,updated_at)
SELECT ?,id,series_id,1,?,?,? FROM books WHERE series_id=?
UNION ALL
SELECT ?,sb.id,sb.series_id,1,?,?,?
FROM union_book_sources ubs
JOIN books ub ON ub.id=ubs.union_book_id AND ub.series_id=?
JOIN books sb ON sb.id=ubs.source_book_id
ON CONFLICT(user_id,book_id) DO UPDATE SET
 completed=1,read_date=excluded.read_date,completed_at=excluded.completed_at,updated_at=excluded.updated_at`,
		userID, now, now, now, seriesID, userID, now, now, now, seriesID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM book_read_progress_tombstones
WHERE user_id=? AND (series_id=? OR book_id IN (
  SELECT ubs.source_book_id FROM union_book_sources ubs
  JOIN books ub ON ub.id=ubs.union_book_id WHERE ub.series_id=?
))`, userID, seriesID, seriesID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateSeriesReadProgress(
	ctx context.Context,
	seriesID string,
	lastBookNumberSortRead float64,
) error {
	userID := UserIDFromContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := nowText()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO book_read_progress_tombstones(user_id,book_id,series_id,deleted_at)
SELECT p.user_id,p.book_id,p.series_id,?
FROM book_read_progress p
WHERE p.user_id=? AND p.book_id IN (
  SELECT b.id FROM books b
  WHERE b.series_id=? AND b.effective_number_sort>?
  UNION
  SELECT ubs.source_book_id FROM union_book_sources ubs
  JOIN books ub ON ub.id=ubs.union_book_id
  WHERE ub.series_id=? AND ub.effective_number_sort>?
)
ON CONFLICT(user_id,book_id) DO UPDATE SET
 series_id=excluded.series_id,
 deleted_at=excluded.deleted_at`, now, userID, seriesID, lastBookNumberSortRead, seriesID, lastBookNumberSortRead); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM book_read_progress
WHERE user_id=? AND book_id IN (
  SELECT b.id FROM books b
  WHERE b.series_id=? AND b.effective_number_sort>?
  UNION
  SELECT ubs.source_book_id FROM union_book_sources ubs
  JOIN books ub ON ub.id=ubs.union_book_id
  WHERE ub.series_id=? AND ub.effective_number_sort>?
)`, userID, seriesID, lastBookNumberSortRead, seriesID, lastBookNumberSortRead); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO book_read_progress(user_id,book_id,series_id,completed,read_date,completed_at,updated_at)
SELECT ?,b.id,b.series_id,1,?,?,?
FROM books b
WHERE b.series_id=? AND b.effective_number_sort<=?
UNION ALL
SELECT ?,sb.id,sb.series_id,1,?,?,?
FROM union_book_sources ubs
JOIN books ub ON ub.id=ubs.union_book_id AND ub.series_id=?
JOIN books sb ON sb.id=ubs.source_book_id
WHERE ub.effective_number_sort<=?
ON CONFLICT(user_id,book_id) DO UPDATE SET
 series_id=excluded.series_id,
 completed=1,
 completed_at=excluded.completed_at,
 read_date=excluded.read_date,
 updated_at=excluded.updated_at`,
		userID, now, now, now, seriesID, lastBookNumberSortRead,
		userID, now, now, now, seriesID, lastBookNumberSortRead); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM book_read_progress_tombstones
WHERE user_id=? AND book_id IN (
  SELECT b.id FROM books b
  WHERE b.series_id=? AND b.effective_number_sort<=?
  UNION
  SELECT ubs.source_book_id FROM union_book_sources ubs
  JOIN books ub ON ub.id=ubs.union_book_id
  WHERE ub.series_id=? AND ub.effective_number_sort<=?
)`, userID, seriesID, lastBookNumberSortRead, seriesID, lastBookNumberSortRead); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM book_read_progress_tombstones WHERE deleted_at<?`, tombstoneCutoffText()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CompletedBookIDsBySeries(ctx context.Context, seriesID string) (map[string]bool, error) {
	userID := UserIDFromContext(ctx)
	rows, err := s.db.QueryContext(ctx, `
SELECT book_id FROM book_read_progress WHERE user_id=? AND series_id=? AND completed=1`, userID, seriesID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Store) BookReadCompleted(ctx context.Context, bookID string) (bool, error) {
	userID := UserIDFromContext(ctx)
	var completed int
	err := s.db.QueryRowContext(ctx, `
SELECT completed FROM book_read_progress WHERE user_id=? AND book_id=?`, userID, bookID).Scan(&completed)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return completed != 0, nil
}

func (s *Store) BookReadProgress(
	ctx context.Context,
	bookID string,
) (BookReadProgress, bool, error) {
	userID := UserIDFromContext(ctx)
	var progress BookReadProgress
	var completed int
	var page sql.NullInt64
	var readDate, completedAt, updatedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT book_id,series_id,completed,page,read_date,completed_at,updated_at
FROM book_read_progress WHERE user_id=? AND book_id=?`, userID, bookID).Scan(
		&progress.BookID, &progress.SeriesID, &completed,
		&page, &readDate, &completedAt, &updatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return BookReadProgress{}, false, nil
		}
		return BookReadProgress{}, false, err
	}
	progress.Completed = completed != 0
	if page.Valid {
		value := int(page.Int64)
		progress.Page = &value
	}
	progress.ReadDate = parseNullableTime(readDate)
	progress.CompletedAt = parseNullableTime(completedAt)
	progress.UpdatedAt = parseTime(updatedAt.String)
	return progress, true, nil
}

func (s *Store) BookReadProgresses(
	ctx context.Context,
	bookIDs []string,
) (map[string]BookReadProgress, error) {
	out := map[string]BookReadProgress{}
	if len(bookIDs) == 0 {
		return out, nil
	}
	userID := UserIDFromContext(ctx)
	unique := cleanStringSliceCaseSensitive(bookIDs)
	for _, chunk := range chunkStrings(unique, sqlInChunkSize) {
		query := `
SELECT book_id,series_id,completed,page,read_date,completed_at,updated_at
FROM book_read_progress WHERE user_id=? AND book_id IN ` + Placeholders(len(chunk))
		args := make([]any, 0, 1+len(chunk))
		args = append(args, userID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var progress BookReadProgress
			var completed int
			var page sql.NullInt64
			var readDate, completedAt, updatedAt sql.NullString
			if err := rows.Scan(
				&progress.BookID, &progress.SeriesID, &completed,
				&page, &readDate, &completedAt, &updatedAt,
			); err != nil {
				rows.Close()
				return nil, err
			}
			progress.Completed = completed != 0
			if page.Valid {
				value := int(page.Int64)
				progress.Page = &value
			}
			progress.ReadDate = parseNullableTime(readDate)
			progress.CompletedAt = parseNullableTime(completedAt)
			progress.UpdatedAt = parseTime(updatedAt.String)
			out[progress.BookID] = progress
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *Store) UpdateBookReadProgress(
	ctx context.Context,
	book Book,
	completed *bool,
	page *int,
) error {
	userID := UserIDFromContext(ctx)
	now := nowText()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Use only persisted index metadata and read it in the progress write transaction.
	var pageCount int
	var seriesID string
	if err := tx.QueryRowContext(ctx, `
SELECT b.series_id,COALESCE(z.page_count,0) FROM books b LEFT JOIN zip_indexes z ON z.book_id=b.id
WHERE b.id=?`, book.ID).Scan(&seriesID, &pageCount); err != nil {
		return err
	}
	var pageValue any
	completedValue := false
	if page != nil && *page > 0 {
		currentPage := *page
		if pageCount > 0 {
			currentPage = min(currentPage, pageCount)
			completedValue = currentPage == pageCount
		}
		pageValue = currentPage
	}
	if completed != nil {
		completedValue = *completed
		if page == nil && completedValue && pageCount > 0 {
			pageValue = pageCount
		}
	}
	completedInt := boolInt(completedValue)
	var completedAt any
	if completedValue {
		completedAt = now
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO book_read_progress(
 user_id,book_id,series_id,completed,page,read_date,completed_at,updated_at
) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(user_id,book_id) DO UPDATE SET
 series_id=excluded.series_id,
 completed=excluded.completed,
 page=coalesce(excluded.page, book_read_progress.page),
 read_date=excluded.read_date,
 completed_at=excluded.completed_at,
 updated_at=excluded.updated_at`,
		userID, book.ID, seriesID, completedInt, pageValue, now, completedAt, now); err != nil {
		return err
	}
	// Mirror union book progress back to physical replica books for this user.
	if _, err := tx.ExecContext(ctx, `
INSERT INTO book_read_progress(
 user_id,book_id,series_id,completed,page,read_date,completed_at,updated_at
)
SELECT ?,sb.id,sb.series_id,?,?,?,?,?
FROM union_book_sources ubs
JOIN books sb ON sb.id=ubs.source_book_id
WHERE ubs.union_book_id=?
ON CONFLICT(user_id,book_id) DO UPDATE SET
 series_id=excluded.series_id,
 completed=excluded.completed,
 page=coalesce(excluded.page, book_read_progress.page),
 read_date=excluded.read_date,
 completed_at=excluded.completed_at,
 updated_at=excluded.updated_at`,
		userID, completedInt, pageValue, now, completedAt, now, book.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM book_read_progress_tombstones
WHERE user_id=? AND (book_id=? OR book_id IN (SELECT source_book_id FROM union_book_sources WHERE union_book_id=?))`,
		userID, book.ID, book.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteBookReadProgress(ctx context.Context, bookID string) error {
	userID := UserIDFromContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowText()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO book_read_progress_tombstones(user_id,book_id,series_id,deleted_at)
SELECT user_id,book_id,series_id,? FROM book_read_progress
WHERE user_id=? AND (book_id=? OR book_id IN (SELECT source_book_id FROM union_book_sources WHERE union_book_id=?))
ON CONFLICT(user_id,book_id) DO UPDATE SET
 series_id=excluded.series_id,
 deleted_at=excluded.deleted_at`, now, userID, bookID, bookID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM book_read_progress
WHERE user_id=? AND (book_id=? OR book_id IN (SELECT source_book_id FROM union_book_sources WHERE union_book_id=?))`,
		userID, bookID, bookID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM book_read_progress_tombstones WHERE deleted_at<?`, tombstoneCutoffText()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordBookPageProgress(
	ctx context.Context,
	book Book,
	pageNumber, pageCount int,
) error {
	if pageNumber < 1 || pageCount < 1 {
		return nil
	}
	if pageNumber > pageCount {
		pageCount = pageNumber
	}
	userID := UserIDFromContext(ctx)
	now := nowText()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO book_page_progress(
 user_id,book_id,series_id,last_loaded_page,max_loaded_page,page_count,updated_at
) SELECT ?,id,series_id,?,?,?,? FROM books WHERE id=?
ON CONFLICT(user_id,book_id) DO UPDATE SET
 series_id=excluded.series_id,
 last_loaded_page=excluded.last_loaded_page,
 max_loaded_page=MAX(book_page_progress.max_loaded_page, excluded.max_loaded_page),
 page_count=excluded.page_count,
 updated_at=excluded.updated_at`,
		userID, pageNumber, pageNumber, pageCount, now, book.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO book_page_progress(
 user_id,book_id,series_id,last_loaded_page,max_loaded_page,page_count,updated_at
)
SELECT ?,sb.id,sb.series_id,?,?,?,?
FROM union_book_sources ubs
JOIN books sb ON sb.id=ubs.source_book_id
WHERE ubs.union_book_id=?
ON CONFLICT(user_id,book_id) DO UPDATE SET
 series_id=excluded.series_id,
 last_loaded_page=excluded.last_loaded_page,
 max_loaded_page=MAX(book_page_progress.max_loaded_page, excluded.max_loaded_page),
 page_count=excluded.page_count,
 updated_at=excluded.updated_at`,
		userID, pageNumber, pageNumber, pageCount, now, book.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BookPageProgress(
	ctx context.Context,
	bookID string,
) (BookPageProgress, bool, error) {
	userID := UserIDFromContext(ctx)
	var progress BookPageProgress
	var updated string
	err := s.db.QueryRowContext(ctx, `
SELECT book_id,series_id,last_loaded_page,max_loaded_page,page_count,updated_at
FROM book_page_progress WHERE user_id=? AND book_id=?`, userID, bookID).Scan(
		&progress.BookID, &progress.SeriesID, &progress.LastLoadedPage,
		&progress.MaxLoadedPage, &progress.PageCount, &updated,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return BookPageProgress{}, false, nil
		}
		return BookPageProgress{}, false, err
	}
	progress.UpdatedAt = parseTime(updated)
	return progress, true, nil
}

func (s *Store) BookPageProgressesBySeries(
	ctx context.Context,
	seriesID string,
) (map[string]BookPageProgress, error) {
	userID := UserIDFromContext(ctx)
	rows, err := s.db.QueryContext(ctx, `
SELECT book_id,series_id,last_loaded_page,max_loaded_page,page_count,updated_at
FROM book_page_progress WHERE user_id=? AND series_id=?`, userID, seriesID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]BookPageProgress{}
	for rows.Next() {
		var progress BookPageProgress
		var updated string
		if err := rows.Scan(
			&progress.BookID, &progress.SeriesID, &progress.LastLoadedPage,
			&progress.MaxLoadedPage, &progress.PageCount, &updated,
		); err != nil {
			return nil, err
		}
		progress.UpdatedAt = parseTime(updated)
		out[progress.BookID] = progress
	}
	return out, rows.Err()
}

func (s *Store) LatestBookPageProgressInSeries(
	ctx context.Context,
	seriesID string,
) (BookPageProgressView, bool, error) {
	userID := UserIDFromContext(ctx)
	var progress BookPageProgressView
	var updated string
	err := s.db.QueryRowContext(ctx, `
SELECT p.book_id,p.series_id,p.last_loaded_page,p.max_loaded_page,p.page_count,p.updated_at,
 b.name,b.effective_number_sort
FROM book_page_progress p
JOIN books b ON b.id=p.book_id
WHERE p.user_id=? AND p.series_id=?
ORDER BY p.updated_at DESC,b.effective_number_sort ASC,b.name COLLATE NOCASE ASC,b.id ASC
LIMIT 1`, userID, seriesID).Scan(
		&progress.BookID, &progress.SeriesID, &progress.LastLoadedPage,
		&progress.MaxLoadedPage, &progress.PageCount, &updated,
		&progress.BookName, &progress.NumberSort,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return BookPageProgressView{}, false, nil
		}
		return BookPageProgressView{}, false, err
	}
	progress.UpdatedAt = parseTime(updated)
	return progress, true, nil
}

type ReadingActivityQuery struct {
	InProgressOnly bool
	MergeBangumi   bool
	Search         string
	TZOffset       int // Minutes east of UTC.
	Page           int
	Size           int
}

type ReadingActivityBook struct {
	Book     Book
	Series   Series
	Metadata SeriesMetadata
	Progress BookReadProgress
}

type ReadingActivityGroup struct {
	DayKey          string
	GroupKey        string
	MergedByBangumi bool
	Books           []ReadingActivityBook
}

type ReadingActivityPage struct {
	Groups        []ReadingActivityGroup
	TotalElements int64
	TotalBooks    int64
}

// ReadingActivity returns complete groups for the requested page and counts all matching activity.
// The query starts from the current user's progress and fetches book details only for selected groups.
func (s *Store) ReadingActivity(ctx context.Context, q ReadingActivityQuery) (ReadingActivityPage, error) {
	out := ReadingActivityPage{Groups: []ReadingActivityGroup{}}
	libraries, err := s.enabledLibraryIDs(ctx)
	if err != nil || len(libraries) == 0 {
		return out, err
	}

	ids, _ := json.Marshal(libraries)
	userID := UserIDFromContext(ctx)
	args := []any{q.MergeBangumi, userID, string(ids)}
	filter := ""
	if q.InProgressOnly {
		filter += " AND p.completed=0"
	}
	if !s.ShowNSFW(ctx) {
		filter += " AND coalesce(sm.is_nsfw,0)=0"
	}
	if search := strings.TrimSpace(q.Search); search != "" {
		key := cjk.SearchKey(search)
		if key == "" {
			return out, nil
		}
		filter += ` AND (cjk_contains(s.name, ?) OR cjk_contains(sm.title, ?) OR cjk_contains(b.name, ?)
 OR EXISTS (
   SELECT 1 FROM json_each(CASE WHEN json_valid(sm.alternate_titles_json) THEN sm.alternate_titles_json ELSE '[]' END) AS alt
   WHERE cjk_contains(CASE WHEN alt.type='object' THEN alt.value ->> 'title' END, ?)
 ))`
		args = append(args, key, key, key, key)
	}

	dayKey := "''"
	if !q.InProgressOnly {
		dayKey = "coalesce(date(substr(sort_at,1,19), ?), '1970-01-01')"
		args = append(args, fmt.Sprintf("%+d minutes", min(840, max(-720, q.TZOffset))))
	}

	size := int64(min(100, max(1, q.Size)))
	offset := min(int64(max(0, q.Page)), (1<<63-1)/size) * size
	args = append(args, size, offset, userID)
	// Progress writes use UTC RFC3339Nano. Split off the fraction before converting
	// offsets or days so SQLite's millisecond rounding cannot move a record across midnight.
	// The fixed-width UTC seconds plus the normalized fraction also sort exact-second ties.
	query := `
WITH matching AS (
 SELECT b.id AS book_id,
   CASE WHEN ? AND NOT ` + effectiveOneShotSQL + ` AND sm.bgm_subject_id>0
     THEN 'bgm:' || sm.bgm_subject_id ELSE 'series:' || s.id END AS group_key,
   CASE WHEN substr(p.updated_at,-1)='Z' THEN substr(p.updated_at,1,19)
     ELSE coalesce(strftime('%Y-%m-%dT%H:%M:%S', substr(p.updated_at,1,19) || substr(p.updated_at,-6)), '1970-01-01T00:00:00')
   END || CASE WHEN substr(p.updated_at,20,1)='.'
     THEN rtrim(substr(p.updated_at,21,length(p.updated_at)-20-
       CASE WHEN substr(p.updated_at,-1)='Z' THEN 1 ELSE 6 END), '0') ELSE '' END AS sort_at
 FROM book_read_progress p
 CROSS JOIN books b ON b.id=p.book_id
 CROSS JOIN series s ON s.id=b.series_id
 LEFT JOIN series_metadata sm ON sm.series_id=s.id
 WHERE p.user_id=? AND b.library_id IN (SELECT value FROM json_each(?))` + filter + `
),
activity AS MATERIALIZED (
 SELECT book_id,group_key,sort_at,` + dayKey + ` AS day_key FROM matching
),
groups AS MATERIALIZED (
 SELECT day_key,group_key,max(sort_at) AS latest,count(*) AS book_count
 FROM activity GROUP BY day_key,group_key
),
selected AS (
 SELECT day_key,group_key,latest FROM groups
 ORDER BY day_key DESC,latest DESC,group_key LIMIT ? OFFSET ?
)
SELECT totals.group_count,totals.book_count,g.day_key,g.group_key,b.id,
 coalesce(b.series_id,''),coalesce(b.library_id,''),coalesce(b.name,''),coalesce(b.size,0),coalesce(b.sha1,''),
 b.file_created_at,b.file_modified_at,coalesce(b.effective_number_sort,0),coalesce(b.created_at,''),coalesce(b.updated_at,''),
 coalesce(z.page_count,0),coalesce(s.name,''),coalesce(s.one_shot,0),coalesce(s.one_shot_override,0),coalesce(s.books_count,0),coalesce(sm.title,''),sm.authors_json,sm.bgm_subject_id,
 coalesce(p.completed,0),p.page,p.read_date,p.completed_at,coalesce(p.updated_at,'')
FROM (SELECT count(*) AS group_count,coalesce(sum(book_count),0) AS book_count FROM groups) totals
LEFT JOIN selected g ON true
LEFT JOIN activity a ON a.group_key=g.group_key AND a.day_key=g.day_key
LEFT JOIN books b ON b.id=a.book_id
LEFT JOIN series s ON s.id=b.series_id
LEFT JOIN series_metadata sm ON sm.series_id=s.id
LEFT JOIN zip_indexes z ON z.book_id=b.id
LEFT JOIN book_read_progress p ON p.user_id=? AND p.book_id=b.id
ORDER BY g.day_key DESC,g.latest DESC,g.group_key,a.sort_at DESC,a.book_id ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	for rows.Next() {
		var item ReadingActivityBook
		b, sr, meta, progress := &item.Book, &item.Series, &item.Metadata, &item.Progress
		var day, key, id, fileCreated, fileModified, authors, readDate, completedAt sql.NullString
		var subject, page sql.NullInt64
		var created, updated, progressUpdated string
		if err := rows.Scan(&out.TotalElements, &out.TotalBooks, &day, &key, &id,
			&b.SeriesID, &b.LibraryID, &b.Name, &b.Size, &b.SHA1,
			&fileCreated, &fileModified, &b.NumberSort, &created, &updated,
			&b.PageCount, &sr.Name, &sr.OneShot, &sr.OneShotOverride, &sr.BooksCount, &meta.Title, &authors, &subject,
			&progress.Completed, &page, &readDate, &completedAt, &progressUpdated); err != nil {
			return out, err
		}
		if !id.Valid { // Totals are still returned when the requested page is empty.
			continue
		}
		b.ID = id.String
		b.CreatedAt, b.UpdatedAt = parseTime(created), parseTime(updated)
		b.FileCreatedAt, b.FileModifiedAt = parseNullableTime(fileCreated), parseNullableTime(fileModified)
		sr.ID, sr.LibraryID = b.SeriesID, b.LibraryID
		if subject.Valid && subject.Int64 > 0 {
			meta.BgmSubjectID = &subject.Int64
		}
		if authors.Valid {
			_ = json.Unmarshal([]byte(authors.String), &meta.Authors)
		}
		progress.BookID, progress.SeriesID = b.ID, b.SeriesID
		progress.ReadDate, progress.CompletedAt = parseNullableTime(readDate), parseNullableTime(completedAt)
		progress.UpdatedAt = parseTime(progressUpdated)
		if page.Valid {
			value := int(page.Int64)
			progress.Page = &value
		}

		n := len(out.Groups)
		if n == 0 || out.Groups[n-1].DayKey != day.String || out.Groups[n-1].GroupKey != key.String {
			out.Groups = append(out.Groups, ReadingActivityGroup{
				DayKey: day.String, GroupKey: key.String, MergedByBangumi: strings.HasPrefix(key.String, "bgm:"),
			})
			n++
		}
		out.Groups[n-1].Books = append(out.Groups[n-1].Books, item)
	}
	return out, rows.Err()
}
