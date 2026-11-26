package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (s *Store) SeriesRowIDs(ctx context.Context, ids []string) (map[string]int64, error) {
	return s.tableRowIDs(ctx, "series", ids)
}

func (s *Store) BookRowIDs(ctx context.Context, ids []string) (map[string]int64, error) {
	return s.tableRowIDs(ctx, "books", ids)
}

func (s *Store) LibraryRowIDs(ctx context.Context, ids []string) (map[string]int64, error) {
	return s.tableRowIDs(ctx, "libraries", ids)
}

func (s *Store) CollectionRowIDs(ctx context.Context, ids []string) (map[string]int64, error) {
	return s.tableRowIDs(ctx, "collections", ids)
}

func (s *Store) tableRowIDs(ctx context.Context, table string, ids []string) (map[string]int64, error) {
	out := make(map[string]int64, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	switch table {
	case "series", "books", "libraries", "collections":
	default:
		return nil, fmt.Errorf("unsupported table %q", table)
	}
	unique := cleanStringSliceCaseSensitive(ids)
	for _, chunk := range chunkStrings(unique, sqlInChunkSize) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		query := fmt.Sprintf(`SELECT id, rowid FROM %s WHERE id IN %s`, table, Placeholders(len(chunk)))
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var rowID int64
			if err := rows.Scan(&id, &rowID); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = rowID
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *Store) SeriesByRowID(ctx context.Context, rowID int64) (Series, error) {
	if rowID <= 0 {
		return Series{}, sql.ErrNoRows
	}
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM series WHERE rowid=?`, rowID).Scan(&id); err != nil {
		return Series{}, err
	}
	return s.SeriesByID(ctx, id)
}

func (s *Store) BookByRowID(ctx context.Context, rowID int64) (Book, error) {
	if rowID <= 0 {
		return Book{}, sql.ErrNoRows
	}
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM books WHERE rowid=?`, rowID).Scan(&id); err != nil {
		return Book{}, err
	}
	return s.BookByID(ctx, id)
}

func (s *Store) LibraryByRowID(ctx context.Context, rowID int64) (Library, error) {
	if rowID <= 0 {
		return Library{}, sql.ErrNoRows
	}
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM libraries WHERE rowid=?`, rowID).Scan(&id); err != nil {
		return Library{}, err
	}
	lib, err := s.LibraryBasic(ctx, id)
	if err != nil {
		return Library{}, err
	}
	if !lib.Enabled || lib.UnionParentID != "" {
		return Library{}, sql.ErrNoRows
	}
	if scope, ok := UserScopeFromContext(ctx); ok && !scope.DisplaysLibrary(lib.ID) {
		return Library{}, sql.ErrNoRows
	}
	return lib, nil
}

func (s *Store) CollectionByRowID(ctx context.Context, rowID int64) (Collection, error) {
	if rowID <= 0 {
		return Collection{}, sql.ErrNoRows
	}
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM collections WHERE rowid=?`, rowID).Scan(&id); err != nil {
		return Collection{}, err
	}
	return s.CollectionByID(ctx, id)
}

// SeriesBookOrdinals returns a stable 1-based ordinal for every book in a series
// ordered by natural ascending reading order (number_sort ASC, name ASC, id ASC).
// Used by OPDS Suwayomi chapter titles ("1 - Vol.01.cbz") so KOReader's chapter
// chooser identifies previous/next chapters accurately regardless of sort order or filters.
func (s *Store) SeriesBookOrdinals(ctx context.Context, seriesID string) (map[string]int, error) {
	out := make(map[string]int)
	if seriesID == "" {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT b.id
FROM books b
WHERE b.series_id=?
ORDER BY b.number_sort ASC, b.name COLLATE NOCASE ASC, b.id ASC`, seriesID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		idx++
		out[id] = idx
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// OnDeckBooksPage returns the next unread book for each visible series that has at
// least one completed book and no currently in-progress book, ordered by most recent
// completed activity. Driven from book_read_progress so large libraries stay fast.
func (s *Store) OnDeckBooksPage(ctx context.Context, page, size int) ([]Book, int64, error) {
	if page < 0 {
		page = 0
	}
	if size <= 0 || size > 500 {
		size = 20
	}
	enabledLibs, err := s.enabledLibraryIDs(ctx)
	if err != nil {
		return nil, 0, err
	}
	if len(enabledLibs) == 0 {
		return []Book{}, 0, nil
	}
	userID := UserIDFromContext(ctx)
	excludeNSFW := !s.ShowNSFW(ctx)
	args := make([]any, 0, 1+len(enabledLibs))
	args = append(args, userID)
	for _, id := range enabledLibs {
		args = append(args, id)
	}
	nsfwClause := ""
	if excludeNSFW {
		nsfwClause = ` AND NOT EXISTS (SELECT 1 FROM series_metadata sm WHERE sm.series_id=s.id AND sm.is_nsfw=1)`
	}
	// Pick visible series with >=1 completed book and 0 in-progress books, ordered by latest read activity.
	seriesQuery := `
SELECT p.series_id
FROM book_read_progress p
JOIN series s ON s.id=p.series_id
WHERE p.user_id=? AND s.library_id IN ` + Placeholders(len(enabledLibs)) + nsfwClause + `
GROUP BY p.series_id
HAVING SUM(CASE WHEN p.completed=1 THEN 1 ELSE 0 END) > 0
   AND SUM(CASE WHEN p.completed=0 THEN 1 ELSE 0 END) = 0
ORDER BY MAX(p.updated_at) DESC, p.series_id ASC`
	rows, err := s.db.QueryContext(ctx, seriesQuery, args...)
	if err != nil {
		return nil, 0, err
	}
	var candidateSeries []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return nil, 0, err
		}
		candidateSeries = append(candidateSeries, sid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, err
	}
	rows.Close()
	if len(candidateSeries) == 0 {
		return []Book{}, 0, nil
	}

	// For each candidate series, find its first unread book (by number_sort, name, id).
	nextBookIDs := make([]string, 0, len(candidateSeries))
	for _, sid := range candidateSeries {
		var bookID string
		err := s.db.QueryRowContext(ctx, `
SELECT b.id
FROM books b
WHERE b.series_id=?
  AND NOT EXISTS (SELECT 1 FROM book_read_progress p WHERE p.user_id=? AND p.book_id=b.id)
ORDER BY b.number_sort ASC, b.name COLLATE NOCASE ASC, b.id ASC
LIMIT 1`, sid, userID).Scan(&bookID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, 0, err
		}
		nextBookIDs = append(nextBookIDs, bookID)
	}
	total := int64(len(nextBookIDs))
	if total == 0 {
		return []Book{}, 0, nil
	}
	start := page * size
	if start >= len(nextBookIDs) {
		return []Book{}, total, nil
	}
	end := min(start+size, len(nextBookIDs))
	sliceIDs := nextBookIDs[start:end]

	out := make([]Book, 0, len(sliceIDs))
	for _, bid := range sliceIDs {
		book, err := s.BookByID(ctx, bid)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, 0, err
		}
		out = append(out, book)
	}
	return out, total, nil
}
