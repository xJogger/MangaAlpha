package database

import (
	"context"
	"database/sql"
	"fmt"
)

// Delete sparse bindings first so legacy columns can project a surviving binding.
// Source IDs on imported cover candidates are provenance and outlive the source.
func clearMetadataSourceBindings(ctx context.Context, tx *sql.Tx, sourceFilter, id string) error {
	for _, table := range []string{"series_source_bindings", "book_source_bindings", "series_cover_candidates", "book_cover_candidates"} {
		query := "DELETE FROM " + table + " WHERE source_id " + sourceFilter
		if table == "series_cover_candidates" || table == "book_cover_candidates" {
			query += " AND cover_kind<>'imported_file'"
		}
		if _, err := tx.ExecContext(ctx, query, id); err != nil {
			return err
		}
	}

	for _, entity := range []string{"series", "book"} {
		libraryJoin := "JOIN series owner ON owner.id=a.id"
		stateAssignments := `
 scrape_status=CASE WHEN b.source_id IS NOT NULL THEN 'matched' WHEN coalesce(m.bgm_subject_id,0)<=0 THEN 'none' ELSE m.scrape_status END,
 scrape_confidence=CASE WHEN coalesce(m.bgm_subject_id,0)<=0 THEN coalesce(b.confidence,0) ELSE m.scrape_confidence END,`
		coverParentMatch := ""
		if entity == "book" {
			libraryJoin = "JOIN books owner ON owner.id=a.id"
			stateAssignments = `
 cover_source=CASE WHEN c.cover_ref IS NULL AND m.cover_source='ext' THEN 'auto' ELSE m.cover_source END,`
			coverParentMatch = " AND (c.series_entry_id='' OR c.series_entry_id=b.series_entry_id)"
		}
		// %[1]s is the entity prefix, shared by the tables and their primary keys.
		query := fmt.Sprintf(`
WITH removed AS MATERIALIZED (SELECT id FROM metadata_sources WHERE id %[2]s),
affected(id) AS MATERIALIZED (
 SELECT %[1]s_id FROM %[1]s_metadata WHERE ext_source_id<>'' AND ext_source_id IN removed
 UNION
 SELECT %[1]s_id FROM %[1]s_metadata WHERE ext_cover_url GLOB 'sqlite-blob://*'
   AND substr(ext_cover_url,15,instr(substr(ext_cover_url || '/',15),'/')-1) IN removed
),
chosen AS MATERIALIZED (
 SELECT a.id, (
  SELECT sb.source_id
  FROM %[1]s_source_bindings sb
  JOIN metadata_sources ms ON ms.id=sb.source_id AND ms.enabled=1
  LEFT JOIN library_metadata_sources link ON link.library_id=owner.library_id AND link.source_id=sb.source_id
  WHERE sb.%[1]s_id=a.id AND coalesce(link.enabled,1)=1
  ORDER BY link.priority IS NULL,link.priority,sb.source_id LIMIT 1
 ) AS source_id
 FROM affected a %[3]s
)
UPDATE %[1]s_metadata AS m SET
 ext_source_id=coalesce(b.source_id,''), ext_entry_id=coalesce(b.entry_id,''),
 ext_cover_url=coalesce(c.cover_ref,''), ext_cover_path='',
 match_origin=CASE WHEN coalesce(m.bgm_subject_id,0)>0 THEN m.match_origin ELSE coalesce(b.match_origin,'') END,
 %[4]s
 updated_at=?
FROM chosen a
LEFT JOIN %[1]s_source_bindings b ON b.%[1]s_id=a.id AND b.source_id=a.source_id
LEFT JOIN %[1]s_cover_candidates c ON c.%[1]s_id=a.id AND c.source_id=a.source_id
 AND c.cover_kind='http_url' AND (c.entry_id='' OR c.entry_id=b.entry_id) %[5]s
WHERE m.%[1]s_id=a.id`, entity, sourceFilter, libraryJoin, stateAssignments, coverParentMatch)
		if _, err := tx.ExecContext(ctx, query, id, nowText()); err != nil {
			return err
		}
	}
	return nil
}
