package database

import (
	"context"
	"database/sql"
	"encoding/json"
)

// SetImportedCoverCleanup installs the callback for files potentially orphaned by
// catalog deletion. It runs after the database write commits.
func (s *Store) SetImportedCoverCleanup(cleanup func(context.Context, []string)) {
	s.coverCleanupMu.Lock()
	defer s.coverCleanupMu.Unlock()
	s.coverCleanup = cleanup
}

func (s *Store) importedCoverCleanup() func(context.Context, []string) {
	s.coverCleanupMu.RLock()
	defer s.coverCleanupMu.RUnlock()
	return s.coverCleanup
}

func queryImportedCoverRefs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// deleteUnionCatalogRows runs inside either of the union deletion transactions.
func (s *Store) deleteUnionCatalogRows(ctx context.Context, tx *sql.Tx, bookIDs, seriesIDs []string) ([]string, error) {
	var refs []string
	if s.importedCoverCleanup() != nil && (len(bookIDs) > 0 || len(seriesIDs) > 0) {
		books, _ := json.Marshal(bookIDs)
		series, _ := json.Marshal(seriesIDs)
		var err error
		refs, err = queryImportedCoverRefs(ctx, tx, `
SELECT cover_ref FROM book_cover_candidates WHERE cover_kind='imported_file' AND book_id IN (SELECT value FROM json_each(?))
UNION
SELECT cover_ref FROM series_cover_candidates WHERE cover_kind='imported_file' AND series_id IN (SELECT value FROM json_each(?))`, string(books), string(series))
		if err != nil {
			return nil, err
		}
	}
	if err := execChunkedDelete(ctx, tx, `DELETE FROM books WHERE id IN `, bookIDs); err != nil {
		return nil, err
	}
	if err := execChunkedDelete(ctx, tx, `DELETE FROM series WHERE id IN `, seriesIDs); err != nil {
		return nil, err
	}
	return refs, nil
}
