package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	CoverKindImportedFile = "imported_file"
	CoverKindBangumi      = "bangumi"
	CoverKindHTTPURL      = "http_url"
)

// CoverCandidateSourceManual identifies a manually selected series candidate.
const CoverCandidateSourceManual = "manual"

type SeriesSourceBinding struct {
	SeriesID         string  `json:"seriesId"`
	SourceID         string  `json:"sourceId"`
	EntryID          string  `json:"entryId"`
	MatchOrigin      string  `json:"matchOrigin"`
	Confidence       float64 `json:"confidence"`
	SourceGeneration string  `json:"sourceGeneration"`
	UpdatedAt        string  `json:"updatedAt"`
}

const seriesSourceBindingColumns = "series_id,source_id,entry_id,match_origin,confidence,source_generation,updated_at"

func scanSeriesSourceBinding(rows *sql.Rows) (SeriesSourceBinding, error) {
	var row SeriesSourceBinding
	err := rows.Scan(&row.SeriesID, &row.SourceID, &row.EntryID, &row.MatchOrigin, &row.Confidence, &row.SourceGeneration, &row.UpdatedAt)
	return row, err
}

func (s *Store) ListSeriesSourceBindings(ctx context.Context, seriesID string) ([]SeriesSourceBinding, error) {
	return listMetadataRows(ctx, s.db, "SELECT "+seriesSourceBindingColumns+" FROM series_source_bindings WHERE series_id=? ORDER BY source_id", seriesID, scanSeriesSourceBinding)
}

func (s *Store) UpsertSeriesSourceBindings(ctx context.Context, rows []SeriesSourceBinding) error {
	return writeMetadataRows(ctx, s.db, rows, upsertSeriesSourceBindingsTx)
}

func upsertSeriesSourceBindingsTx(ctx context.Context, tx *sql.Tx, rows []SeriesSourceBinding) error {
	stmt, err := prepareMetadataUpsert(ctx, tx, "series_source_bindings", seriesSourceBindingColumns)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := nowText()
	for _, row := range rows {
		if row.UpdatedAt == "" {
			row.UpdatedAt = now
		}
		if row.MatchOrigin == "" {
			row.MatchOrigin = "auto"
		}
		if _, err := stmt.ExecContext(ctx, row.SeriesID, row.SourceID, row.EntryID, row.MatchOrigin, row.Confidence, row.SourceGeneration, row.UpdatedAt); err != nil {
			return err
		}
	}
	return nil
}

type BookSourceBinding struct {
	BookID           string  `json:"bookId"`
	SourceID         string  `json:"sourceId"`
	SeriesEntryID    string  `json:"seriesEntryId"`
	EntryID          string  `json:"entryId"`
	MatchOrigin      string  `json:"matchOrigin"`
	Confidence       float64 `json:"confidence"`
	SourceGeneration string  `json:"sourceGeneration"`
	UpdatedAt        string  `json:"updatedAt"`
}

const bookSourceBindingColumns = "book_id,source_id,series_entry_id,entry_id,match_origin,confidence,source_generation,updated_at"

func scanBookSourceBinding(rows *sql.Rows) (BookSourceBinding, error) {
	var row BookSourceBinding
	err := rows.Scan(&row.BookID, &row.SourceID, &row.SeriesEntryID, &row.EntryID, &row.MatchOrigin, &row.Confidence, &row.SourceGeneration, &row.UpdatedAt)
	return row, err
}

func (s *Store) ListBookSourceBindings(ctx context.Context, bookID string) ([]BookSourceBinding, error) {
	return listMetadataRows(ctx, s.db, "SELECT "+bookSourceBindingColumns+" FROM book_source_bindings WHERE book_id=? ORDER BY source_id", bookID, scanBookSourceBinding)
}

// ListBookSourceBindingsBySeries groups rows by book ID in one query.
func (s *Store) ListBookSourceBindingsBySeries(ctx context.Context, seriesID string) (map[string][]BookSourceBinding, error) {
	rows, err := listMetadataRows(ctx, s.db, `
SELECT `+qualifyMetadataColumns("m", bookSourceBindingColumns)+`
FROM books b CROSS JOIN book_source_bindings m ON m.book_id=b.id
WHERE b.series_id=? ORDER BY m.book_id,source_id`, seriesID, scanBookSourceBinding)
	if err != nil {
		return nil, err
	}
	grouped := make(map[string][]BookSourceBinding)
	for _, row := range rows {
		grouped[row.BookID] = append(grouped[row.BookID], row)
	}
	return grouped, nil
}

func (s *Store) UpsertBookSourceBindings(ctx context.Context, rows []BookSourceBinding) error {
	return writeMetadataRows(ctx, s.db, rows, upsertBookSourceBindingsTx)
}

func upsertBookSourceBindingsTx(ctx context.Context, tx *sql.Tx, rows []BookSourceBinding) error {
	stmt, err := prepareMetadataUpsert(ctx, tx, "book_source_bindings", bookSourceBindingColumns)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := nowText()
	for _, row := range rows {
		if row.UpdatedAt == "" {
			row.UpdatedAt = now
		}
		if row.MatchOrigin == "" {
			row.MatchOrigin = "auto"
		}
		if _, err := stmt.ExecContext(ctx, row.BookID, row.SourceID, row.SeriesEntryID, row.EntryID, row.MatchOrigin, row.Confidence, row.SourceGeneration, row.UpdatedAt); err != nil {
			return err
		}
	}
	return nil
}

type SeriesCoverCandidate struct {
	SeriesID         string `json:"seriesId"`
	SourceID         string `json:"sourceId"`
	EntryID          string `json:"entryId"`
	CoverKind        string `json:"coverKind"`
	CoverRef         string `json:"coverRef"`
	IsNSFW           bool   `json:"isNsfw"`
	NSFWKnown        bool   `json:"nsfwKnown"`
	RankOrder        int    `json:"rankOrder"`
	SourceGeneration string `json:"sourceGeneration"`
	UpdatedAt        string `json:"updatedAt"`
}

const seriesCoverCandidateColumns = "series_id,source_id,entry_id,cover_kind,cover_ref,is_nsfw,nsfw_known,rank_order,source_generation,updated_at"

func scanSeriesCoverCandidate(rows *sql.Rows) (SeriesCoverCandidate, error) {
	var row SeriesCoverCandidate
	err := rows.Scan(&row.SeriesID, &row.SourceID, &row.EntryID, &row.CoverKind, &row.CoverRef, &row.IsNSFW, &row.NSFWKnown, &row.RankOrder, &row.SourceGeneration, &row.UpdatedAt)
	return row, err
}

func (s *Store) ListSeriesCoverCandidates(ctx context.Context, seriesID string) ([]SeriesCoverCandidate, error) {
	return listMetadataRows(ctx, s.db, "SELECT "+seriesCoverCandidateColumns+" FROM series_cover_candidates WHERE series_id=? ORDER BY rank_order,source_id", seriesID, scanSeriesCoverCandidate)
}

func (s *Store) UpsertSeriesCoverCandidates(ctx context.Context, rows []SeriesCoverCandidate) error {
	return writeMetadataRows(ctx, s.db, rows, upsertSeriesCoverCandidatesTx)
}

func upsertSeriesCoverCandidatesTx(ctx context.Context, tx *sql.Tx, rows []SeriesCoverCandidate) error {
	stmt, err := prepareMetadataUpsert(ctx, tx, "series_cover_candidates", seriesCoverCandidateColumns)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := nowText()
	for _, row := range rows {
		if err := validateCoverCandidate(row.CoverKind, row.CoverRef); err != nil {
			return err
		}
		if row.UpdatedAt == "" {
			row.UpdatedAt = now
		}
		if _, err := stmt.ExecContext(ctx, row.SeriesID, row.SourceID, row.EntryID, row.CoverKind, row.CoverRef, row.IsNSFW, row.NSFWKnown, row.RankOrder, row.SourceGeneration, row.UpdatedAt); err != nil {
			return err
		}
	}
	return nil
}

type BookCoverCandidate struct {
	BookID           string `json:"bookId"`
	SourceID         string `json:"sourceId"`
	SeriesEntryID    string `json:"seriesEntryId"`
	EntryID          string `json:"entryId"`
	CoverKind        string `json:"coverKind"`
	CoverRef         string `json:"coverRef"`
	IsNSFW           bool   `json:"isNsfw"`
	NSFWKnown        bool   `json:"nsfwKnown"`
	RankOrder        int    `json:"rankOrder"`
	SourceGeneration string `json:"sourceGeneration"`
	UpdatedAt        string `json:"updatedAt"`
}

const bookCoverCandidateColumns = "book_id,source_id,series_entry_id,entry_id,cover_kind,cover_ref,is_nsfw,nsfw_known,rank_order,source_generation,updated_at"

func scanBookCoverCandidate(rows *sql.Rows) (BookCoverCandidate, error) {
	var row BookCoverCandidate
	err := rows.Scan(&row.BookID, &row.SourceID, &row.SeriesEntryID, &row.EntryID, &row.CoverKind, &row.CoverRef, &row.IsNSFW, &row.NSFWKnown, &row.RankOrder, &row.SourceGeneration, &row.UpdatedAt)
	return row, err
}

func (s *Store) ListBookCoverCandidates(ctx context.Context, bookID string) ([]BookCoverCandidate, error) {
	return listMetadataRows(ctx, s.db, "SELECT "+bookCoverCandidateColumns+" FROM book_cover_candidates WHERE book_id=? ORDER BY rank_order,source_id", bookID, scanBookCoverCandidate)
}

// ListBookCoverCandidatesBySeries groups rows by book ID in one query.
func (s *Store) ListBookCoverCandidatesBySeries(ctx context.Context, seriesID string) (map[string][]BookCoverCandidate, error) {
	rows, err := listMetadataRows(ctx, s.db, `
SELECT `+qualifyMetadataColumns("m", bookCoverCandidateColumns)+`
FROM books b CROSS JOIN book_cover_candidates m ON m.book_id=b.id
WHERE b.series_id=? ORDER BY m.book_id,rank_order,source_id`, seriesID, scanBookCoverCandidate)
	if err != nil {
		return nil, err
	}
	grouped := make(map[string][]BookCoverCandidate)
	for _, row := range rows {
		grouped[row.BookID] = append(grouped[row.BookID], row)
	}
	return grouped, nil
}

func (s *Store) UpsertBookCoverCandidates(ctx context.Context, rows []BookCoverCandidate) error {
	return writeMetadataRows(ctx, s.db, rows, upsertBookCoverCandidatesTx)
}

func upsertBookCoverCandidatesTx(ctx context.Context, tx *sql.Tx, rows []BookCoverCandidate) error {
	stmt, err := prepareMetadataUpsert(ctx, tx, "book_cover_candidates", bookCoverCandidateColumns)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := nowText()
	for _, row := range rows {
		if err := validateCoverCandidate(row.CoverKind, row.CoverRef); err != nil {
			return err
		}
		if row.UpdatedAt == "" {
			row.UpdatedAt = now
		}
		if _, err := stmt.ExecContext(ctx, row.BookID, row.SourceID, row.SeriesEntryID, row.EntryID, row.CoverKind, row.CoverRef, row.IsNSFW, row.NSFWKnown, row.RankOrder, row.SourceGeneration, row.UpdatedAt); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceSeriesSourceBindingsTx replaces the bindings in the caller's transaction.
// An empty slice clears the bindings for seriesID.
func (s *Store) ReplaceSeriesSourceBindingsTx(ctx context.Context, tx *sql.Tx, seriesID string, rows []SeriesSourceBinding) error {
	replacement := make([]SeriesSourceBinding, len(rows))
	copy(replacement, rows)
	sourceIDs := make([]string, len(rows))
	for i := range replacement {
		replacement[i].SeriesID = seriesID
		sourceIDs[i] = replacement[i].SourceID
	}
	ids, _ := json.Marshal(sourceIDs)
	query := `DELETE FROM series_source_bindings WHERE series_id=? AND source_id NOT IN (SELECT value FROM json_each(?))`
	if automated, _ := ctx.Value(automaticMetadataKey{}).(bool); automated {
		query += " AND match_origin NOT IN ('manual','agent')"
	}
	if _, err := tx.ExecContext(ctx, query, seriesID, ids); err != nil || len(rows) == 0 {
		return err
	}
	return upsertSeriesSourceBindingsTx(ctx, tx, replacement)
}

// DeleteSeriesCoverCandidatesBySource explicitly removes candidates for a source,
// including imported candidates. Source and repository deletion retain imported files.
func (s *Store) DeleteSeriesCoverCandidatesBySource(ctx context.Context, sourceID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM series_cover_candidates WHERE source_id=?", sourceID)
	return err
}

func (s *Store) IsImportedCoverRefReferenced(ctx context.Context, coverRef string) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM series_cover_candidates WHERE cover_kind='imported_file' AND cover_ref=?)
    OR EXISTS (SELECT 1 FROM book_cover_candidates WHERE cover_kind='imported_file' AND cover_ref=?)`,
		coverRef, coverRef).Scan(&found)
	return found, err
}

func listMetadataRows[T any](ctx context.Context, db *sql.DB, query, id string, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]T, 0)
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func writeMetadataRows[T any](ctx context.Context, db *sql.DB, rows []T, write func(context.Context, *sql.Tx, []T) error) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := write(ctx, tx, rows); err != nil {
		return err
	}
	return tx.Commit()
}

func validateCoverCandidate(kind, ref string) error {
	switch kind {
	case CoverKindImportedFile, CoverKindBangumi, CoverKindHTTPURL:
	default:
		return fmt.Errorf("invalid cover candidate kind %q", kind)
	}
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(strings.ToLower(strings.TrimSpace(ref)), "sqlite-blob://") {
		return fmt.Errorf("invalid cover candidate reference")
	}
	return nil
}

// The first two columns form the primary key. A timestamp alone never changes a row.
func prepareMetadataUpsert(ctx context.Context, tx *sql.Tx, table, columns string) (*sql.Stmt, error) {
	names := strings.Split(columns, ",")
	fields := strings.Join(names[2:], ",")
	compared := strings.Join(names[2:len(names)-1], ",")
	protection := ""
	if automated, _ := ctx.Value(automaticMetadataKey{}).(bool); automated && strings.HasSuffix(table, "_source_bindings") {
		protection = " AND match_origin NOT IN ('manual','agent')"
	}
	return tx.PrepareContext(ctx, "INSERT INTO "+table+"("+columns+") VALUES"+Placeholders(len(names))+
		" ON CONFLICT("+strings.Join(names[:2], ",")+") DO UPDATE SET ("+fields+")=(excluded."+strings.ReplaceAll(fields, ",", ",excluded.")+")"+
		" WHERE "+metadataTupleDiff(table, "excluded", compared)+protection)
}

const automaticSeriesCoverCandidates = "source_id NOT IN ('manual','agent')"

func clearSparseSeriesExternalBinding(ctx context.Context, tx *sql.Tx, seriesID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT cover_ref FROM series_cover_candidates
WHERE series_id=? AND cover_kind='imported_file' AND `+automaticSeriesCoverCandidates+`
UNION
SELECT c.cover_ref FROM books b CROSS JOIN book_cover_candidates c ON c.book_id=b.id
WHERE b.series_id=? AND c.cover_kind='imported_file'
ORDER BY cover_ref`, seriesID, seriesID)
	if err != nil {
		return nil, err
	}
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, query := range []string{
		"DELETE FROM series_source_bindings WHERE series_id=?",
		"DELETE FROM series_cover_candidates WHERE series_id=? AND " + automaticSeriesCoverCandidates,
		"DELETE FROM book_source_bindings WHERE book_id IN (SELECT id FROM books WHERE series_id=?)",
		"DELETE FROM book_cover_candidates WHERE book_id IN (SELECT id FROM books WHERE series_id=?)",
	} {
		if _, err := tx.ExecContext(ctx, query, seriesID); err != nil {
			return nil, err
		}
	}
	return refs, nil
}

// These column lists use newlines only for SQL formatting.
func qualifyMetadataColumns(alias, columns string) string {
	columns = strings.ReplaceAll(columns, "\n", "")
	return alias + "." + strings.ReplaceAll(columns, ",", ","+alias+".")
}
