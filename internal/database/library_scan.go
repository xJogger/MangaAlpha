package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// CommitLibraryScan publishes a complete staged directory listing atomically.
func (s *Store) CommitLibraryScan(ctx context.Context, libraryID, runID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var active int
	if err := tx.QueryRowContext(ctx, `
SELECT 1 FROM scan_runs r JOIN libraries l ON l.id=r.library_id
WHERE r.id=? AND l.id=? AND r.status='running' AND r.cancel_requested=0 AND l.enabled=1`,
		runID, libraryID).Scan(&active); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return context.Canceled
		}
		return err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE shelf_revision SET bulk_mode=1 WHERE id=1`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO series(id,library_id,cid,name,relative_path,one_shot,file_modified_at,created_at,updated_at,seen_scan_id)
SELECT id,library_id,cid,name,relative_path,one_shot,file_modified_at,created_at,updated_at,run_id
FROM scan_series_staging WHERE run_id=?
ON CONFLICT(id) DO UPDATE SET
 name=excluded.name,relative_path=excluded.relative_path,one_shot=excluded.one_shot,
 file_modified_at=excluded.file_modified_at,updated_at=excluded.updated_at,seen_scan_id=excluded.seen_scan_id
WHERE (series.name,series.relative_path,series.one_shot,series.file_modified_at)
   IS NOT (excluded.name,excluded.relative_path,excluded.one_shot,excluded.file_modified_at)`, runID); err != nil {
		return err
	}

	reassigned, err := preserveReassignedBookState(ctx, tx, runID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,sha1,
 file_created_at,file_modified_at,number_sort,created_at,updated_at,seen_scan_id)
SELECT id,series_id,library_id,file_id,parent_cid,name,size,pick_code,sha1,
 file_created_at,file_modified_at,number_sort,created_at,updated_at,run_id
FROM scan_books_staging WHERE run_id=?
ON CONFLICT(library_id,file_id) DO UPDATE SET
 series_id=excluded.series_id,parent_cid=excluded.parent_cid,name=excluded.name,size=excluded.size,
 pick_code=excluded.pick_code,sha1=excluded.sha1,file_created_at=excluded.file_created_at,
 file_modified_at=excluded.file_modified_at,number_sort=excluded.number_sort,updated_at=excluded.updated_at,
 seen_scan_id=excluded.seen_scan_id
WHERE (books.series_id,books.parent_cid,books.name,books.size,books.pick_code,books.sha1,
       books.file_created_at,books.file_modified_at,books.number_sort)
   IS NOT (excluded.series_id,excluded.parent_cid,excluded.name,excluded.size,excluded.pick_code,excluded.sha1,
           excluded.file_created_at,excluded.file_modified_at,excluded.number_sort)`, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM books WHERE library_id=?
 AND NOT EXISTS (SELECT 1 FROM scan_books_staging t WHERE t.run_id=? AND t.library_id=books.library_id AND t.file_id=books.file_id)`,
		libraryID, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM series WHERE library_id=?
 AND NOT EXISTS (SELECT 1 FROM scan_series_staging t WHERE t.run_id=? AND t.id=series.id)
 AND NOT EXISTS (SELECT 1 FROM books b WHERE b.series_id=series.id)`,
		libraryID, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
WITH rollups AS (
 SELECT series_id,count(*) AS n,MAX(NULLIF(file_modified_at,'')) AS modified,MAX(NULLIF(file_created_at,'')) AS created
 FROM scan_books_staging WHERE run_id=? GROUP BY series_id
)
UPDATE series AS s
SET books_count=r.n,latest_book_modified_at=r.modified,latest_book_created_at=r.created,
    sort_modified_at=COALESCE(r.modified,NULLIF(s.file_modified_at,''),s.updated_at),
    sort_created_at=COALESCE(r.created,s.created_at)
FROM rollups r WHERE s.id=r.series_id AND (
 (s.books_count,s.latest_book_modified_at,s.latest_book_created_at,s.sort_modified_at,s.sort_created_at)
 IS NOT (r.n,r.modified,r.created,COALESCE(r.modified,NULLIF(s.file_modified_at,''),s.updated_at),COALESCE(r.created,s.created_at))
)`, runID); err != nil {
		return err
	}
	for _, table := range []string{"scan_books_staging", "scan_series_staging"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE run_id=?`, runID); err != nil {
			return err
		}
	}
	// Bulk mode suppresses shelf triggers while the catalog and progress are moved.
	if _, err := tx.ExecContext(ctx, `
UPDATE shelf_revision SET bulk_mode=0,catalog=catalog+1,progress=progress+? WHERE id=1`, boolInt(reassigned)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.wakeSearchFTSWorker()
	return nil
}

func preserveReassignedBookState(ctx context.Context, tx *sql.Tx, runID string) (bool, error) {
	// Keep the old and new memberships available before upserting the books.
	if _, err := tx.ExecContext(ctx, `
CREATE TEMP TABLE scan_reassigned_books(book_id TEXT PRIMARY KEY, old_series_id TEXT NOT NULL, new_series_id TEXT NOT NULL)
WITHOUT ROWID`); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO scan_reassigned_books
SELECT b.id,b.series_id,t.series_id
FROM scan_books_staging t JOIN books b ON b.library_id=t.library_id AND b.file_id=t.file_id
WHERE t.run_id=? AND b.series_id<>t.series_id`, runID)
	if err != nil {
		return false, err
	}
	moves, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if moves > 0 {
		for _, table := range []string{"book_read_progress", "book_read_progress_tombstones", "book_page_progress", "book_download_stats"} {
			if _, err := tx.ExecContext(ctx, `
UPDATE `+table+` AS p SET series_id=m.new_series_id FROM scan_reassigned_books m
WHERE p.book_id=m.book_id AND p.series_id=m.old_series_id`); err != nil {
				return false, err
			}
		}
		if err := carrySeriesScanState(ctx, tx, runID); err != nil {
			return false, err
		}
		// A split or merge can carry the old classification without inheriting
		// an ambiguous title, binding, or collection membership.
		if _, err := tx.ExecContext(ctx, `
INSERT INTO series_metadata(series_id,is_nsfw,updated_at)
SELECT DISTINCT m.new_series_id,1,?
FROM scan_reassigned_books m JOIN series_metadata old ON old.series_id=m.old_series_id
WHERE old.is_nsfw=1
ON CONFLICT(series_id) DO UPDATE SET is_nsfw=1,updated_at=excluded.updated_at
WHERE series_metadata.nsfw_lock=0 AND series_metadata.is_nsfw=0`, nowText()); err != nil {
			return false, err
		}
	}
	_, err = tx.ExecContext(ctx, `DROP TABLE temp.scan_reassigned_books`)
	return moves > 0, err
}

func carrySeriesScanState(ctx context.Context, tx *sql.Tx, runID string) error {
	if _, err := tx.ExecContext(ctx, `
CREATE TEMP TABLE scan_series_successors(old_id TEXT PRIMARY KEY,new_id TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO scan_series_successors
SELECT m.old_series_id,min(m.new_series_id)
FROM scan_reassigned_books m
WHERE NOT EXISTS (SELECT 1 FROM scan_series_staging t WHERE t.run_id=? AND t.id=m.old_series_id)
GROUP BY m.old_series_id
HAVING min(m.new_series_id)=max(m.new_series_id)`, runID); err != nil {
		return err
	}
	// A target has an unambiguous predecessor only when all its surviving old
	// books came from that predecessor. Ignore newly discovered books here.
	if _, err := tx.ExecContext(ctx, `
WITH ambiguous AS MATERIALIZED (
 SELECT t.series_id
 FROM scan_books_staging t
 JOIN books b ON b.library_id=t.library_id AND b.file_id=t.file_id
 WHERE t.run_id=?
 GROUP BY t.series_id
 HAVING min(b.series_id)<>max(b.series_id)
)
DELETE FROM scan_series_successors WHERE new_id IN (SELECT series_id FROM ambiguous)`, runID); err != nil {
		return err
	}
	for _, pair := range []struct{ table, fields string }{
		{"series_genres", "genre"}, {"series_tags", "tag"}, {"series_authors", "name,role"},
	} {
		if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO `+pair.table+`(series_id,`+pair.fields+`)
SELECT next.new_id,`+pair.fields+`
FROM scan_series_successors next JOIN `+pair.table+` old ON old.series_id=next.old_id
WHERE NOT EXISTS (SELECT 1 FROM series_metadata sm WHERE sm.series_id=next.new_id)`); err != nil {
			return err
		}
	}
	_, fields, _ := strings.Cut(seriesMetadataColumns, ",")
	if _, err := tx.ExecContext(ctx, `
INSERT INTO series_metadata(series_id,`+fields+`)
SELECT next.new_id,`+fields+`
FROM scan_series_successors next JOIN series_metadata sm ON sm.series_id=next.old_id
WHERE true ON CONFLICT(series_id) DO NOTHING`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO collection_series(collection_id,series_id,position)
SELECT cs.collection_id,next.new_id,cs.position
FROM scan_series_successors next JOIN collection_series cs ON cs.series_id=next.old_id
WHERE true ON CONFLICT(collection_id,series_id) DO NOTHING`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DROP TABLE temp.scan_series_successors`)
	return err
}
