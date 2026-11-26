package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/xJogger/fake-komga-115/internal/cjk"
	"modernc.org/sqlite"
)

const (
	searchFTSSeriesVersionKey = "search_fts_series_version"
	searchFTSBooksVersionKey  = "search_fts_books_version"
	searchFTSSeriesAnchorKey  = "search_fts_series_anchor"
	searchFTSBooksAnchorKey   = "search_fts_books_anchor"
	currentSearchFTSVersion   = "v3"

	searchFTSSeriesBatchSize = 2000
	searchFTSBooksBatchSize  = 5000
	searchFTSSyncInlineLimit = 500
)

var registerCJKSQLFuncsOnce sync.Once

func registerCJKSQLFunctions() {
	registerCJKSQLFuncsOnce.Do(func() {
		// Runtime-only deterministic helpers (never referenced in persistent SQLite triggers
		// so external sqlite3 tools remain 100% compatible with the database file).
		_ = sqlite.RegisterDeterministicScalarFunction(
			"cjk_key",
			1,
			func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
				if len(args) == 0 || args[0] == nil {
					return "", nil
				}
				return cjk.SearchKey(driverValueToString(args[0])), nil
			},
		)
		_ = sqlite.RegisterDeterministicScalarFunction(
			"cjk_contains",
			2,
			func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
				if len(args) < 2 || args[0] == nil || args[1] == nil {
					return int64(0), nil
				}
				needle := driverValueToString(args[1])
				if needle == "" {
					return int64(0), nil
				}
				haystack := cjk.SearchKey(driverValueToString(args[0]))
				if haystack == "" {
					return int64(0), nil
				}
				if strings.Contains(haystack, needle) {
					return int64(1), nil
				}
				return int64(0), nil
			},
		)
	})
}

func driverValueToString(v driver.Value) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func ensureSearchFTSTables(db *sql.DB) error {
	// If an older dev schema had contentfull FTS5 or TEXT dirty queue, migrate cleanly.
	var dirtyHasRowID bool
	if ok, _ := tableExists(db, "search_fts_dirty"); ok {
		dirtyHasRowID, _ = columnExists(db, "search_fts_dirty", "row_id")
		if !dirtyHasRowID {
			_, _ = db.Exec(`
DROP TRIGGER IF EXISTS trg_sfts_series_ai;
DROP TRIGGER IF EXISTS trg_sfts_series_au;
DROP TRIGGER IF EXISTS trg_sfts_series_ad;
DROP TRIGGER IF EXISTS trg_sfts_meta_ai;
DROP TRIGGER IF EXISTS trg_sfts_meta_au;
DROP TRIGGER IF EXISTS trg_sfts_meta_ad;
DROP TRIGGER IF EXISTS trg_sfts_tags_ai;
DROP TRIGGER IF EXISTS trg_sfts_tags_ad;
DROP TRIGGER IF EXISTS trg_sfts_genres_ai;
DROP TRIGGER IF EXISTS trg_sfts_genres_ad;
DROP TRIGGER IF EXISTS trg_sfts_authors_ai;
DROP TRIGGER IF EXISTS trg_sfts_authors_ad;
DROP TRIGGER IF EXISTS trg_sfts_books_ai;
DROP TRIGGER IF EXISTS trg_sfts_books_au;
DROP TRIGGER IF EXISTS trg_sfts_books_ad;
DROP TRIGGER IF EXISTS trg_sfts_book_metadata_ai;
DROP TRIGGER IF EXISTS trg_sfts_book_metadata_au;
DROP TRIGGER IF EXISTS trg_sfts_book_metadata_ad;
DROP TABLE IF EXISTS series_search_fts;
DROP TABLE IF EXISTS books_search_fts;
DROP TABLE IF EXISTS series_search_docs;
DROP TABLE IF EXISTS book_search_docs;
DROP TABLE IF EXISTS search_fts_dirty;
`)
		}
	}

	// Upgrade unguarded UPDATE triggers (without WHEN OLD.x IS NOT NEW.x) so unchanged
	// scanner upserts and rank-only Bangumi backfills never re-enqueue unchanged rows.
	var trgDef string
	if err := db.QueryRow(`SELECT COALESCE(sql,'') FROM sqlite_master WHERE type='trigger' AND name='trg_sfts_books_au'`).Scan(&trgDef); err == nil &&
		trgDef != "" && !strings.Contains(strings.ToUpper(trgDef), "WHEN OLD.") {
		_, _ = db.Exec(`
DROP TRIGGER IF EXISTS trg_sfts_series_au;
DROP TRIGGER IF EXISTS trg_sfts_meta_au;
DROP TRIGGER IF EXISTS trg_sfts_books_au;
`)
	}

	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS search_fts_dirty (
  kind TEXT NOT NULL,
  row_id INTEGER NOT NULL,
  gen INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (kind, row_id)
) WITHOUT ROWID;

CREATE VIRTUAL TABLE IF NOT EXISTS series_search_fts USING fts5(
  name_fts,
  title_fts,
  alt_fts,
  publisher_fts,
  tags_fts,
  genres_fts,
  authors_fts,
  content='',
  contentless_delete=1,
  tokenize='unicode61 remove_diacritics 2'
);

CREATE VIRTUAL TABLE IF NOT EXISTS books_search_fts USING fts5(
  name_fts,
  content='',
  contentless_delete=1,
  tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER IF NOT EXISTS trg_sfts_series_ai AFTER INSERT ON series BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen) VALUES('s', NEW.rowid, 1)
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;
CREATE TRIGGER IF NOT EXISTS trg_sfts_series_au AFTER UPDATE OF name, library_id ON series
WHEN OLD.name IS NOT NEW.name OR OLD.library_id IS NOT NEW.library_id BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen) VALUES('s', NEW.rowid, 1)
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;
CREATE TRIGGER IF NOT EXISTS trg_sfts_series_ad AFTER DELETE ON series BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen) VALUES('s', OLD.rowid, 1)
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;

CREATE TRIGGER IF NOT EXISTS trg_sfts_meta_ai AFTER INSERT ON series_metadata BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen)
  SELECT 's', s.rowid, 1 FROM series s WHERE s.id = NEW.series_id
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;
CREATE TRIGGER IF NOT EXISTS trg_sfts_meta_au AFTER UPDATE OF title, publisher, alternate_titles_json ON series_metadata
WHEN OLD.title IS NOT NEW.title OR OLD.publisher IS NOT NEW.publisher OR OLD.alternate_titles_json IS NOT NEW.alternate_titles_json BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen)
  SELECT 's', s.rowid, 1 FROM series s WHERE s.id = NEW.series_id
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;
CREATE TRIGGER IF NOT EXISTS trg_sfts_meta_ad AFTER DELETE ON series_metadata BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen)
  SELECT 's', s.rowid, 1 FROM series s WHERE s.id = OLD.series_id
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;

CREATE TRIGGER IF NOT EXISTS trg_sfts_tags_ai AFTER INSERT ON series_tags BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen)
  SELECT 's', s.rowid, 1 FROM series s WHERE s.id = NEW.series_id
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;
CREATE TRIGGER IF NOT EXISTS trg_sfts_tags_ad AFTER DELETE ON series_tags BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen)
  SELECT 's', s.rowid, 1 FROM series s WHERE s.id = OLD.series_id
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;

CREATE TRIGGER IF NOT EXISTS trg_sfts_genres_ai AFTER INSERT ON series_genres BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen)
  SELECT 's', s.rowid, 1 FROM series s WHERE s.id = NEW.series_id
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;
CREATE TRIGGER IF NOT EXISTS trg_sfts_genres_ad AFTER DELETE ON series_genres BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen)
  SELECT 's', s.rowid, 1 FROM series s WHERE s.id = OLD.series_id
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;

CREATE TRIGGER IF NOT EXISTS trg_sfts_authors_ai AFTER INSERT ON series_authors BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen)
  SELECT 's', s.rowid, 1 FROM series s WHERE s.id = NEW.series_id
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;
CREATE TRIGGER IF NOT EXISTS trg_sfts_authors_ad AFTER DELETE ON series_authors BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen)
  SELECT 's', s.rowid, 1 FROM series s WHERE s.id = OLD.series_id
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;

CREATE TRIGGER IF NOT EXISTS trg_sfts_books_ai AFTER INSERT ON books BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen) VALUES('b', NEW.rowid, 1)
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;
CREATE TRIGGER IF NOT EXISTS trg_sfts_books_au AFTER UPDATE OF name, library_id ON books
WHEN OLD.name IS NOT NEW.name OR OLD.library_id IS NOT NEW.library_id BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen) VALUES('b', NEW.rowid, 1)
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;
CREATE TRIGGER IF NOT EXISTS trg_sfts_books_ad AFTER DELETE ON books BEGIN
  INSERT INTO search_fts_dirty(kind, row_id, gen) VALUES('b', OLD.rowid, 1)
  ON CONFLICT(kind, row_id) DO UPDATE SET gen = gen + 1;
END;
CREATE TRIGGER IF NOT EXISTS trg_sfts_book_metadata_ai AFTER INSERT ON book_metadata BEGIN
  INSERT INTO search_fts_dirty(kind,row_id,gen)
  SELECT 'b',b.rowid,1 FROM books b WHERE b.id=NEW.book_id
  ON CONFLICT(kind,row_id) DO UPDATE SET gen=gen+1;
END;
CREATE TRIGGER IF NOT EXISTS trg_sfts_book_metadata_au AFTER UPDATE OF title,summary,tags_json,authors_json ON book_metadata
WHEN (OLD.title,OLD.summary,OLD.tags_json,OLD.authors_json) IS NOT (NEW.title,NEW.summary,NEW.tags_json,NEW.authors_json) BEGIN
  INSERT INTO search_fts_dirty(kind,row_id,gen)
  SELECT 'b',b.rowid,1 FROM books b WHERE b.id=NEW.book_id
  ON CONFLICT(kind,row_id) DO UPDATE SET gen=gen+1;
END;
CREATE TRIGGER IF NOT EXISTS trg_sfts_book_metadata_ad AFTER DELETE ON book_metadata BEGIN
  INSERT INTO search_fts_dirty(kind,row_id,gen)
  SELECT 'b',b.rowid,1 FROM books b WHERE b.id=OLD.book_id
  ON CONFLICT(kind,row_id) DO UPDATE SET gen=gen+1;
END;
`)
	return err
}

type ftsRowAnchor struct {
	RowID int64  `json:"r"`
	ID    string `json:"id"`
}

func (s *Store) saveRowAnchors(ctx context.Context, table, settingKey string) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, id FROM `+table+` ORDER BY rowid DESC LIMIT 3`)
	if err != nil {
		return
	}
	defer rows.Close()
	var anchors []ftsRowAnchor
	for rows.Next() {
		var a ftsRowAnchor
		if err := rows.Scan(&a.RowID, &a.ID); err == nil {
			anchors = append(anchors, a)
		}
	}
	raw, _ := json.Marshal(anchors)
	_ = s.SetSetting(ctx, settingKey, string(raw))
}

// rowAnchorsShifted detects whether an external VACUUM renumbered implicit
// rowids on table while the process was stopped.
func (s *Store) rowAnchorsShifted(ctx context.Context, table, settingKey string) bool {
	raw, err := s.GetSetting(ctx, settingKey)
	if err != nil || strings.TrimSpace(raw) == "" {
		return false
	}
	var anchors []ftsRowAnchor
	if err := json.Unmarshal([]byte(raw), &anchors); err != nil {
		return false
	}
	for _, a := range anchors {
		var curRowID int64
		err := s.db.QueryRowContext(ctx, `SELECT rowid FROM `+table+` WHERE id=?`, a.ID).Scan(&curRowID)
		if err == nil && curRowID != a.RowID {
			return true
		}
	}
	return false
}

func (s *Store) startSearchFTSWorker() {
	ctx := context.Background()
	seriesVer, _ := s.GetSetting(ctx, searchFTSSeriesVersionKey)
	booksVer, _ := s.GetSetting(ctx, searchFTSBooksVersionKey)

	needSeries := seriesVer != currentSearchFTSVersion || s.rowAnchorsShifted(ctx, "series", searchFTSSeriesAnchorKey)
	needBooks := booksVer != currentSearchFTSVersion || s.rowAnchorsShifted(ctx, "books", searchFTSBooksAnchorKey)

	if needSeries || needBooks {
		var hasSeries, hasBooks int
		_ = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM series LIMIT 1)`).Scan(&hasSeries)
		_ = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM books LIMIT 1)`).Scan(&hasBooks)
		if needSeries && hasSeries == 0 {
			_ = s.SetSetting(ctx, searchFTSSeriesVersionKey, currentSearchFTSVersion)
			needSeries = false
		}
		if needBooks && hasBooks == 0 {
			_ = s.SetSetting(ctx, searchFTSBooksVersionKey, currentSearchFTSVersion)
			needBooks = false
		}
	}

	if !needSeries {
		s.ftsSeriesReady.Store(true)
	}
	if !needBooks {
		s.ftsBooksReady.Store(true)
	}

	s.ftsWG.Add(1)
	go s.runSearchFTSWorker(needSeries, needBooks)
}

func (s *Store) wakeSearchFTSWorker() {
	if s.ftsWake == nil {
		return
	}
	select {
	case s.ftsWake <- struct{}{}:
	default:
	}
}

func (s *Store) runSearchFTSWorker(needSeries, needBooks bool) {
	defer s.ftsWG.Done()
	ctx := s.ftsStopCtx

	retryBackoff := 2 * time.Second
	for (needSeries || needBooks) && ctx.Err() == nil {
		failed := false
		if needSeries {
			if err := s.rebuildSeriesFTS(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("rebuild series FTS5 index failed; will retry", "error", err, "backoff", retryBackoff)
				failed = true
			} else {
				s.ftsSeriesReady.Store(true)
				needSeries = false
			}
		}
		if ctx.Err() != nil {
			return
		}
		if needBooks && !failed {
			if err := s.rebuildBooksFTS(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("rebuild books FTS5 index failed; will retry", "error", err, "backoff", retryBackoff)
				failed = true
			} else {
				s.ftsBooksReady.Store(true)
				needBooks = false
			}
		}
		if !failed {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-s.ftsWake:
		case <-time.After(retryBackoff):
		}
		if retryBackoff < 30*time.Second {
			retryBackoff = min(retryBackoff*2, 30*time.Second)
		}
	}
	if ctx.Err() != nil {
		return
	}

	s.drainDirtyLoop(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.ftsWake:
			s.drainDirtyLoop(ctx)
		}
	}
}

func (s *Store) drainDirtyLoop(ctx context.Context) {
	for ctx.Err() == nil {
		moreS := false
		moreB := false
		if s.ftsSeriesReady.Load() {
			n, err := s.syncDirtySearchFTSKind(ctx, "s", searchFTSSeriesBatchSize)
			if err == nil && n >= searchFTSSeriesBatchSize {
				moreS = true
			}
		}
		if ctx.Err() != nil {
			return
		}
		if s.ftsBooksReady.Load() {
			n, err := s.syncDirtySearchFTSKind(ctx, "b", searchFTSBooksBatchSize)
			if err == nil && n >= searchFTSBooksBatchSize {
				moreB = true
			}
		}
		if !moreS && !moreB {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// WaitSearchIndexReady waits until both series and books FTS5 indexes have
// completed initial build and drained pending dirty rows.
func (s *Store) WaitSearchIndexReady(ctx context.Context) error {
	for {
		if s.ftsSeriesReady.Load() && s.ftsBooksReady.Load() {
			for {
				ns, err := s.syncDirtySearchFTSKind(ctx, "s", searchFTSSeriesBatchSize)
				if err != nil {
					return err
				}
				nb, err := s.syncDirtySearchFTSKind(ctx, "b", searchFTSBooksBatchSize)
				if err != nil {
					return err
				}
				if ns == 0 && nb == 0 {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ftsStopCtx.Done():
			return s.ftsStopCtx.Err()
		case <-time.After(15 * time.Millisecond):
		}
	}
}

// SearchIndexReady reports whether series and books FTS5 indexes are ready.
func (s *Store) SearchIndexReady() (seriesReady, booksReady bool) {
	return s.ftsSeriesReady.Load(), s.ftsBooksReady.Load()
}

// ensureSearchFTSFresh checks whether the requested FTS5 index is ready and
// up to date. Small dirty backlogs (<= searchFTSSyncInlineLimit) are drained
// synchronously in milliseconds for read-after-write consistency; larger
// backlogs wake the background worker and gracefully fall back.
func (s *Store) ensureSearchFTSFresh(ctx context.Context, kind string) bool {
	if kind == "s" && !s.ftsSeriesReady.Load() {
		return false
	}
	if kind == "b" && !s.ftsBooksReady.Load() {
		return false
	}
	var hasDirty int
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM search_fts_dirty WHERE kind=? LIMIT 1)`, kind).Scan(&hasDirty); err != nil {
		return false
	}
	if hasDirty == 0 {
		return true
	}
	var dirtyCnt int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM (SELECT 1 FROM search_fts_dirty WHERE kind=? LIMIT ?)`,
		kind, searchFTSSyncInlineLimit+1).Scan(&dirtyCnt); err != nil {
		return false
	}
	if dirtyCnt > searchFTSSyncInlineLimit {
		s.wakeSearchFTSWorker()
		return false
	}
	if _, err := s.syncDirtySearchFTSKind(ctx, kind, searchFTSSyncInlineLimit); err != nil {
		return false
	}
	return true
}

type dirtyRowItem struct {
	rowID int64
	gen   int64
}

type seriesFTSRow struct {
	rowID     int64
	seriesID  string
	name      string
	title     string
	publisher string
	altJSON   string
	tags      []string
	genres    []string
	authors   []string
}

func decodeAlternateTitleTexts(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" || raw == "null" {
		return nil
	}
	var items []AlternateTitle
	if err := json.Unmarshal([]byte(raw), &items); err == nil {
		out := make([]string, 0, len(items))
		for _, item := range items {
			if t := strings.TrimSpace(item.Title); t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	var plain []string
	if err := json.Unmarshal([]byte(raw), &plain); err == nil {
		return plain
	}
	return nil
}

func (s *Store) rebuildSeriesFTS(ctx context.Context) error {
	s.ftsMu.Lock()
	err := func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `INSERT INTO series_search_fts(series_search_fts) VALUES('delete-all')`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM search_fts_dirty WHERE kind='s'`); err != nil {
			return err
		}
		return tx.Commit()
	}()
	s.ftsMu.Unlock()
	if err != nil {
		return err
	}

	var lastRowID int64
	for ctx.Err() == nil {
		batch, err := s.loadSeriesFTSBatchAfter(ctx, lastRowID, searchFTSSeriesBatchSize)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		lastRowID = batch[len(batch)-1].rowID
		s.ftsMu.Lock()
		err = s.upsertSeriesFTSRowsLocked(ctx, batch, nil, nil, false)
		s.ftsMu.Unlock()
		if err != nil {
			return err
		}
		if len(batch) < searchFTSSeriesBatchSize {
			break
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for ctx.Err() == nil {
		n, err := s.syncDirtySearchFTSKind(ctx, "s", searchFTSSeriesBatchSize)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.ftsMu.Lock()
	_, _ = s.db.ExecContext(ctx, `INSERT INTO series_search_fts(series_search_fts) VALUES('optimize')`)
	s.ftsMu.Unlock()
	s.saveRowAnchors(ctx, "series", searchFTSSeriesAnchorKey)
	return s.SetSetting(ctx, searchFTSSeriesVersionKey, currentSearchFTSVersion)
}

func (s *Store) loadSeriesFTSBatchAfter(ctx context.Context, lastRowID int64, limit int) ([]seriesFTSRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT s.rowid, s.id, s.name,
       COALESCE(sm.title,''), COALESCE(sm.publisher,''), COALESCE(sm.alternate_titles_json,'')
FROM series s
LEFT JOIN series_metadata sm ON sm.series_id=s.id
WHERE s.rowid > ?
ORDER BY s.rowid ASC
LIMIT ?`, lastRowID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]seriesFTSRow, 0, min(limit, 256))
	for rows.Next() {
		var r seriesFTSRow
		if err := rows.Scan(&r.rowID, &r.seriesID, &r.name, &r.title, &r.publisher, &r.altJSON); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	if err := s.attachSeriesFTSTaxonomies(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) loadSeriesFTSByRowIDs(ctx context.Context, rowIDs []int64) ([]seriesFTSRow, map[int64]bool, error) {
	if len(rowIDs) == 0 {
		return nil, nil, nil
	}
	present := make(map[int64]bool, len(rowIDs))
	out := make([]seriesFTSRow, 0, len(rowIDs))
	for start := 0; start < len(rowIDs); start += sqlInChunkSize {
		end := min(start+sqlInChunkSize, len(rowIDs))
		chunk := rowIDs[start:end]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `
SELECT s.rowid, s.id, s.name,
       COALESCE(sm.title,''), COALESCE(sm.publisher,''), COALESCE(sm.alternate_titles_json,'')
FROM series s
LEFT JOIN series_metadata sm ON sm.series_id=s.id
WHERE s.rowid IN `+Placeholders(len(chunk)), args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var r seriesFTSRow
			if err := rows.Scan(&r.rowID, &r.seriesID, &r.name, &r.title, &r.publisher, &r.altJSON); err != nil {
				rows.Close()
				return nil, nil, err
			}
			present[r.rowID] = true
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rows.Close()
	}
	if len(out) > 0 {
		if err := s.attachSeriesFTSTaxonomies(ctx, out); err != nil {
			return nil, nil, err
		}
	}
	return out, present, nil
}

func (s *Store) attachSeriesFTSTaxonomies(ctx context.Context, batch []seriesFTSRow) error {
	idxByID := make(map[string]int, len(batch))
	ids := make([]string, len(batch))
	for i := range batch {
		idxByID[batch[i].seriesID] = i
		ids[i] = batch[i].seriesID
	}
	for _, chunk := range chunkStrings(ids, sqlInChunkSize) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		ph := Placeholders(len(chunk))

		tRows, err := s.db.QueryContext(ctx, `SELECT series_id, tag FROM series_tags WHERE series_id IN `+ph, args...)
		if err != nil {
			return err
		}
		for tRows.Next() {
			var sid, val string
			if err := tRows.Scan(&sid, &val); err != nil {
				tRows.Close()
				return err
			}
			if idx, ok := idxByID[sid]; ok {
				batch[idx].tags = append(batch[idx].tags, val)
			}
		}
		if err := tRows.Err(); err != nil {
			tRows.Close()
			return err
		}
		tRows.Close()

		gRows, err := s.db.QueryContext(ctx, `SELECT series_id, genre FROM series_genres WHERE series_id IN `+ph, args...)
		if err != nil {
			return err
		}
		for gRows.Next() {
			var sid, val string
			if err := gRows.Scan(&sid, &val); err != nil {
				gRows.Close()
				return err
			}
			if idx, ok := idxByID[sid]; ok {
				batch[idx].genres = append(batch[idx].genres, val)
			}
		}
		if err := gRows.Err(); err != nil {
			gRows.Close()
			return err
		}
		gRows.Close()

		aRows, err := s.db.QueryContext(ctx, `SELECT series_id, name FROM series_authors WHERE series_id IN `+ph, args...)
		if err != nil {
			return err
		}
		for aRows.Next() {
			var sid, val string
			if err := aRows.Scan(&sid, &val); err != nil {
				aRows.Close()
				return err
			}
			if idx, ok := idxByID[sid]; ok {
				batch[idx].authors = append(batch[idx].authors, val)
			}
		}
		if err := aRows.Err(); err != nil {
			aRows.Close()
			return err
		}
		aRows.Close()
	}
	return nil
}

type bookFTSRow struct {
	rowID                                       int64
	name, title, summary, tagsJSON, authorsJSON string
}

const bookFTSColumns = `b.rowid,b.name,COALESCE(bm.title,''),COALESCE(bm.summary,''),COALESCE(bm.tags_json,'[]'),COALESCE(bm.authors_json,'[]')`

func (r bookFTSRow) indexText() string {
	var tags []string
	var authors []Author
	_ = json.Unmarshal([]byte(r.tagsJSON), &tags)
	_ = json.Unmarshal([]byte(r.authorsJSON), &authors)
	parts := append([]string{r.name, r.title, r.summary}, tags...)
	for _, author := range authors {
		parts = append(parts, author.Name)
	}
	return cjk.FTSIndexText(strings.Join(parts, " "))
}

func (s *Store) rebuildBooksFTS(ctx context.Context) error {
	s.ftsMu.Lock()
	err := func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `INSERT INTO books_search_fts(books_search_fts) VALUES('delete-all')`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM search_fts_dirty WHERE kind='b'`); err != nil {
			return err
		}
		return tx.Commit()
	}()
	s.ftsMu.Unlock()
	if err != nil {
		return err
	}

	var lastRowID int64
	for ctx.Err() == nil {
		rows, err := s.db.QueryContext(ctx, `
SELECT `+bookFTSColumns+`
FROM books b LEFT JOIN book_metadata bm ON bm.book_id=b.id
WHERE b.rowid > ?
ORDER BY b.rowid ASC
LIMIT ?`, lastRowID, searchFTSBooksBatchSize)
		if err != nil {
			return err
		}
		batch := make([]bookFTSRow, 0, min(searchFTSBooksBatchSize, 512))
		for rows.Next() {
			var r bookFTSRow
			if err := rows.Scan(&r.rowID, &r.name, &r.title, &r.summary, &r.tagsJSON, &r.authorsJSON); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}
		lastRowID = batch[len(batch)-1].rowID
		s.ftsMu.Lock()
		err = s.upsertBookFTSRowsLocked(ctx, batch, nil, nil, false)
		s.ftsMu.Unlock()
		if err != nil {
			return err
		}
		if len(batch) < searchFTSBooksBatchSize {
			break
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for ctx.Err() == nil {
		n, err := s.syncDirtySearchFTSKind(ctx, "b", searchFTSBooksBatchSize)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.ftsMu.Lock()
	_, _ = s.db.ExecContext(ctx, `INSERT INTO books_search_fts(books_search_fts) VALUES('optimize')`)
	s.ftsMu.Unlock()
	s.saveRowAnchors(ctx, "books", searchFTSBooksAnchorKey)
	return s.SetSetting(ctx, searchFTSBooksVersionKey, currentSearchFTSVersion)
}

func (s *Store) loadBooksFTSByRowIDs(ctx context.Context, rowIDs []int64) ([]bookFTSRow, map[int64]bool, error) {
	if len(rowIDs) == 0 {
		return nil, nil, nil
	}
	present := make(map[int64]bool, len(rowIDs))
	out := make([]bookFTSRow, 0, len(rowIDs))
	for start := 0; start < len(rowIDs); start += sqlInChunkSize {
		end := min(start+sqlInChunkSize, len(rowIDs))
		chunk := rowIDs[start:end]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `
SELECT `+bookFTSColumns+` FROM books b LEFT JOIN book_metadata bm ON bm.book_id=b.id WHERE b.rowid IN `+Placeholders(len(chunk)), args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var r bookFTSRow
			if err := rows.Scan(&r.rowID, &r.name, &r.title, &r.summary, &r.tagsJSON, &r.authorsJSON); err != nil {
				rows.Close()
				return nil, nil, err
			}
			present[r.rowID] = true
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rows.Close()
	}
	return out, present, nil
}

// syncDirtySearchFTSKind reads a snapshot of dirty (row_id, gen) entries, loads
// their latest source state, updates FTS5, and atomically deletes only matching
// (row_id, gen) rows in the same transaction so concurrent writes (which bump
// gen) or transaction rollbacks never lose updates.
func (s *Store) syncDirtySearchFTSKind(ctx context.Context, kind string, limit int) (int, error) {
	s.ftsMu.Lock()
	defer s.ftsMu.Unlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT row_id, gen FROM search_fts_dirty WHERE kind=? ORDER BY row_id ASC LIMIT ?`, kind, limit)
	if err != nil {
		return 0, err
	}
	var items []dirtyRowItem
	var rowIDs []int64
	for rows.Next() {
		var it dirtyRowItem
		if err := rows.Scan(&it.rowID, &it.gen); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
		rowIDs = append(rowIDs, it.rowID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(items) == 0 {
		return 0, nil
	}

	if kind == "s" {
		loaded, present, err := s.loadSeriesFTSByRowIDs(ctx, rowIDs)
		if err != nil {
			return 0, err
		}
		var deleted []int64
		for _, rid := range rowIDs {
			if !present[rid] {
				deleted = append(deleted, rid)
			}
		}
		if err := s.upsertSeriesFTSRowsLocked(ctx, loaded, deleted, items, true); err != nil {
			return 0, err
		}
		return len(items), nil
	}

	loaded, present, err := s.loadBooksFTSByRowIDs(ctx, rowIDs)
	if err != nil {
		return 0, err
	}
	var deleted []int64
	for _, rid := range rowIDs {
		if !present[rid] {
			deleted = append(deleted, rid)
		}
	}
	if err := s.upsertBookFTSRowsLocked(ctx, loaded, deleted, items, true); err != nil {
		return 0, err
	}
	return len(items), nil
}

func (s *Store) upsertSeriesFTSRowsLocked(ctx context.Context, rows []seriesFTSRow, deletedRowIDs []int64, clearDirty []dirtyRowItem, deleteBeforeInsert bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var deleteFTSStmt *sql.Stmt
	if deleteBeforeInsert || len(deletedRowIDs) > 0 {
		deleteFTSStmt, err = tx.PrepareContext(ctx, `DELETE FROM series_search_fts WHERE rowid=?`)
		if err != nil {
			return err
		}
		defer deleteFTSStmt.Close()
	}

	insertFTSStmt, err := tx.PrepareContext(ctx, `
INSERT INTO series_search_fts(
  rowid, name_fts, title_fts, alt_fts, publisher_fts, tags_fts, genres_fts, authors_fts
) VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insertFTSStmt.Close()

	for _, rid := range deletedRowIDs {
		if _, err := deleteFTSStmt.ExecContext(ctx, rid); err != nil {
			return err
		}
	}

	for _, r := range rows {
		if deleteBeforeInsert {
			if _, err := deleteFTSStmt.ExecContext(ctx, r.rowID); err != nil {
				return err
			}
		}
		if _, err := insertFTSStmt.ExecContext(ctx,
			r.rowID,
			cjk.FTSIndexText(r.name),
			cjk.FTSIndexText(r.title),
			cjk.FTSJoinValues(decodeAlternateTitleTexts(r.altJSON)),
			cjk.FTSIndexText(r.publisher),
			cjk.FTSJoinValues(r.tags),
			cjk.FTSJoinValues(r.genres),
			cjk.FTSJoinValues(r.authors),
		); err != nil {
			return err
		}
	}

	if len(clearDirty) > 0 {
		delDirtyStmt, err := tx.PrepareContext(ctx, `DELETE FROM search_fts_dirty WHERE kind='s' AND row_id=? AND gen=?`)
		if err != nil {
			return err
		}
		defer delDirtyStmt.Close()
		for _, it := range clearDirty {
			if _, err := delDirtyStmt.ExecContext(ctx, it.rowID, it.gen); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) upsertBookFTSRowsLocked(ctx context.Context, rows []bookFTSRow, deletedRowIDs []int64, clearDirty []dirtyRowItem, deleteBeforeInsert bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var deleteFTSStmt *sql.Stmt
	if deleteBeforeInsert || len(deletedRowIDs) > 0 {
		deleteFTSStmt, err = tx.PrepareContext(ctx, `DELETE FROM books_search_fts WHERE rowid=?`)
		if err != nil {
			return err
		}
		defer deleteFTSStmt.Close()
	}

	insertFTSStmt, err := tx.PrepareContext(ctx, `
INSERT INTO books_search_fts(rowid, name_fts) VALUES(?,?)`)
	if err != nil {
		return err
	}
	defer insertFTSStmt.Close()

	for _, rid := range deletedRowIDs {
		if _, err := deleteFTSStmt.ExecContext(ctx, rid); err != nil {
			return err
		}
	}

	for _, r := range rows {
		if deleteBeforeInsert {
			if _, err := deleteFTSStmt.ExecContext(ctx, r.rowID); err != nil {
				return err
			}
		}
		if _, err := insertFTSStmt.ExecContext(ctx, r.rowID, r.indexText()); err != nil {
			return err
		}
	}

	if len(clearDirty) > 0 {
		delDirtyStmt, err := tx.PrepareContext(ctx, `DELETE FROM search_fts_dirty WHERE kind='b' AND row_id=? AND gen=?`)
		if err != nil {
			return err
		}
		defer delDirtyStmt.Close()
		for _, it := range clearDirty {
			if _, err := delDirtyStmt.ExecContext(ctx, it.rowID, it.gen); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
