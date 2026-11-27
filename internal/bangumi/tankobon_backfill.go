package bangumi

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/diagnostics"
)

const (
	tankobonSeriesBatchSize     = 50
	tankobonBackfillAlgoVersion = "v2"
)

func tankobonBackfillSignature(status ArchiveStatus) string {
	base := archiveDumpSignature(status)
	if base == "" {
		base = "imported:" + status.ImportedAt + ":" + strconv.FormatInt(status.SubjectCount, 10)
	}
	return base + "|algo:" + tankobonBackfillAlgoVersion
}

type tankobonChain struct {
	sourceID      string
	rank          int
	higherSources map[string]bool
}

type tankobonBackfillPlan struct {
	chains       map[string]tankobonChain
	versions     map[string]database.LibraryMetadataChainState
	defaultChain tankobonChain
}

func (s *Service) tankobonBackfillPlan(ctx context.Context) (tankobonBackfillPlan, error) {
	// Read versions first so a concurrent configuration change can be detected
	// before applying a batch with an outdated chain.
	versions, err := s.store.ListAllLibraryMetadataChainStates(ctx)
	if err != nil {
		return tankobonBackfillPlan{}, err
	}
	chains, err := s.store.ListAllLibraryMetadataChains(ctx)
	if err != nil {
		return tankobonBackfillPlan{}, err
	}
	sources, err := s.store.ListMetadataSources(ctx, "")
	if err != nil {
		return tankobonBackfillPlan{}, err
	}
	byID := make(map[string]database.MetadataSource, len(sources))
	for _, src := range sources {
		byID[src.ID] = src
	}
	plan := tankobonBackfillPlan{
		chains: make(map[string]tankobonChain, len(chains)), versions: versions,
		defaultChain: tankobonChain{sourceID: "bangumi"},
	}
	builtin, configured := byID["bangumi"]
	if configured && !builtin.Enabled {
		plan.defaultChain.rank = -1
	}
	for libID, links := range chains {
		chain := tankobonChain{rank: -1, higherSources: make(map[string]bool)}
		for rank, link := range links {
			if !link.Enabled {
				continue
			}
			src := byID[link.SourceID]
			if (link.SourceID == "bangumi" && plan.defaultChain.rank == 0) ||
				(src.Kind == "bangumi_archive" && src.Enabled && src.Ready) {
				chain.sourceID = link.SourceID
				chain.rank = rank
				break
			}
			if src.Enabled && src.Kind != "bangumi_archive" {
				chain.higherSources[link.SourceID] = true
			}
		}
		plan.chains[libID] = chain
	}
	return plan, nil
}

type tankobonSeriesRow struct {
	database.Series
	rowID, subjectID int64
	isNSFW           bool
	chain            tankobonChain
	chainVersion     string
}

const visibleTankobonLibrary = `l.enabled=1 AND NOT EXISTS (
 SELECT 1 FROM union_layers ul WHERE ul.member_library_id=l.id)`

func (s *Service) tankobonSeriesBatch(ctx context.Context, after int64, plan tankobonBackfillPlan) ([]tankobonSeriesRow, error) {
	rows, err := s.store.DB().QueryContext(ctx, `
SELECT sm.rowid, s.id, s.library_id, s.one_shot, s.one_shot_override, s.books_count, sm.bgm_subject_id, coalesce(sm.is_nsfw, 0)
FROM series_metadata sm CROSS JOIN series s ON s.id=sm.series_id
JOIN libraries l ON l.id=s.library_id
WHERE sm.rowid>? AND sm.bgm_subject_id>0 AND `+visibleTankobonLibrary+`
ORDER BY sm.rowid LIMIT ?`, after, tankobonSeriesBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	batch := make([]tankobonSeriesRow, 0, tankobonSeriesBatchSize)
	for rows.Next() {
		var row tankobonSeriesRow
		if err := rows.Scan(&row.rowID, &row.ID, &row.LibraryID, &row.OneShot, &row.OneShotOverride, &row.BooksCount, &row.subjectID, &row.isNSFW); err != nil {
			return nil, err
		}
		chain, ok := plan.chains[row.LibraryID]
		if !ok {
			chain = plan.defaultChain
		}
		row.chain, row.chainVersion = chain, plan.versions[row.LibraryID].ChainVersion
		batch = append(batch, row)
	}
	return batch, rows.Err()
}

type tankobonBookMetadata struct {
	title, summary, releaseDate, isbn string
	subjectID                         int64
	coverURL, coverPath               string
	nsfw                              bool
}

type tankobonBookBinding struct{ parent, entry, origin string }

type tankobonBookCover struct {
	parent, entry, kind, ref string
	rank                     int
	nsfw, nsfwKnown          bool
}

type tankobonBookRow struct {
	VolumeMatchBook
	seriesID, chainVersion, origin string
	parentID                       int64
	metadata                       tankobonBookMetadata
	binding                        tankobonBookBinding
	cover                          tankobonBookCover
	titleLock, summaryLock         bool
	releaseDateLock, isbnLock      bool
	exists, higherPriority         bool
}

type tankobonBatchQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadTankobonBookBatch(ctx context.Context, db tankobonBatchQueryer, targets map[string]tankobonSeriesRow) ([]tankobonBookRow, error) {
	args := make([]any, 0, 2*len(targets))
	for id, target := range targets {
		args = append(args, id, target.chain.sourceID)
	}
	values := strings.TrimSuffix(strings.Repeat("(?,?),", len(targets)), ",")
	rows, err := db.QueryContext(ctx, `
WITH targets(series_id,source_id) AS (VALUES `+values+`)
SELECT b.id,b.series_id,b.name,sm.bgm_subject_id,coalesce(cs.chain_version,''),
 coalesce(m.number,''),m.book_id IS NOT NULL,coalesce(m.match_origin,''),coalesce(m.ext_source_id,''),
 coalesce(m.title,''),coalesce(m.summary,''),coalesce(m.release_date,''),coalesce(m.isbn,''),
 coalesce(m.title_lock,0),coalesce(m.summary_lock,0),coalesce(m.release_date_lock,0),coalesce(m.isbn_lock,0),
 coalesce(m.bgm_subject_id,0),coalesce(m.bgm_cover_url,''),coalesce(m.bgm_cover_path,''),coalesce(m.bgm_is_nsfw,0),
 coalesce(sb.source_id,''),coalesce(sb.series_entry_id,''),coalesce(sb.entry_id,''),coalesce(sb.match_origin,''),
 coalesce(c.series_entry_id,''),coalesce(c.entry_id,''),coalesce(c.cover_kind,''),coalesce(c.cover_ref,''),
 coalesce(c.rank_order,0),coalesce(c.is_nsfw,0),coalesce(c.nsfw_known,0)
FROM targets CROSS JOIN books b ON b.series_id=targets.series_id
JOIN series_metadata sm ON sm.series_id=b.series_id
JOIN libraries l ON l.id=b.library_id
LEFT JOIN library_metadata_chain_state cs ON cs.library_id=l.id
LEFT JOIN book_metadata m ON m.book_id=b.id
LEFT JOIN book_source_bindings sb ON sb.book_id=b.id
LEFT JOIN book_cover_candidates c ON c.book_id=b.id AND c.source_id=targets.source_id
WHERE sm.bgm_subject_id>0 AND `+visibleTankobonLibrary+`
ORDER BY b.series_id,b.effective_number_sort,b.name COLLATE NOCASE,b.id,sb.source_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var books []tankobonBookRow
	for rows.Next() {
		var row tankobonBookRow
		var extSource, source string
		var binding tankobonBookBinding
		m, c := &row.metadata, &row.cover
		if err := rows.Scan(&row.BookID, &row.seriesID, &row.Name, &row.parentID, &row.chainVersion,
			&row.Number, &row.exists, &row.origin, &extSource,
			&m.title, &m.summary, &m.releaseDate, &m.isbn, &row.titleLock, &row.summaryLock, &row.releaseDateLock, &row.isbnLock,
			&m.subjectID, &m.coverURL, &m.coverPath, &m.nsfw,
			&source, &binding.parent, &binding.entry, &binding.origin,
			&c.parent, &c.entry, &c.kind, &c.ref, &c.rank, &c.nsfw, &c.nsfwKnown); err != nil {
			return nil, err
		}
		chain := targets[row.seriesID].chain
		higher := chain.higherSources
		if len(books) == 0 || books[len(books)-1].BookID != row.BookID {
			row.higherPriority = higher[extSource]
			books = append(books, row)
		}
		book := &books[len(books)-1]
		book.higherPriority = book.higherPriority || higher[source]
		if source == chain.sourceID {
			book.binding = binding
		}
	}
	return books, rows.Err()
}

type tankobonBookUpdate struct {
	id, sourceID, origin string
	metadata             tankobonBookMetadata
	binding              tankobonBookBinding
	cover                tankobonBookCover
	writeMetadata        bool
	writeBinding         bool
	writeCover           bool
	searchDirty          bool
}

func diffTankobonBook(book tankobonBookRow, match VolumeMatchResult, chain tankobonChain) (tankobonBookUpdate, bool) {
	if !match.NSFWKnown || book.origin == "manual" || book.origin == "agent" ||
		book.binding.origin == "manual" || book.binding.origin == "agent" {
		return tankobonBookUpdate{}, false
	}
	legacy := book.exists && book.origin == ""
	if legacy && book.metadata.subjectID > 0 && book.metadata.subjectID != match.SubjectID {
		return tankobonBookUpdate{}, false
	}
	onlyEmpty := legacy || book.higherPriority
	m := book.metadata
	for _, field := range []struct {
		dst    *string
		value  string
		locked bool
	}{
		{&m.title, match.Title, book.titleLock},
		{&m.summary, match.Summary, book.summaryLock},
		{&m.releaseDate, match.Date, book.releaseDateLock},
		{&m.isbn, match.ISBN, book.isbnLock},
	} {
		if !field.locked && field.value != "" && (!onlyEmpty || strings.TrimSpace(*field.dst) == "") {
			*field.dst = field.value
		}
	}
	if m.subjectID != match.SubjectID || m.coverURL != match.CoverURL {
		m.coverPath = ""
	}
	m.subjectID, m.coverURL, m.nsfw = match.SubjectID, match.CoverURL, match.NSFW

	parent, entry := strconv.FormatInt(book.parentID, 10), strconv.FormatInt(match.SubjectID, 10)
	binding := tankobonBookBinding{parent: parent, entry: entry, origin: "auto"}
	cover := tankobonBookCover{
		parent: parent, entry: entry, kind: database.CoverKindBangumi, ref: match.CoverURL,
		rank: chain.rank, nsfw: match.NSFW, nsfwKnown: true,
	}
	update := tankobonBookUpdate{
		id: book.BookID, sourceID: chain.sourceID, origin: book.origin, metadata: m, binding: binding, cover: cover,
		writeMetadata: m != book.metadata, writeBinding: binding != book.binding, writeCover: cover != book.cover,
		searchDirty: m.title != book.metadata.title || m.summary != book.metadata.summary,
	}
	if !book.exists {
		update.origin = "auto"
	}
	return update, update.writeMetadata || update.writeBinding || update.writeCover
}

func loadArchiveSubjectNSFWBatch(ctx context.Context, db *sql.DB, parents []int64) (map[int64]bool, error) {
	if len(parents) == 0 {
		return nil, nil
	}
	args := make([]any, len(parents))
	for i, id := range parents {
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, `
SELECT id, coalesce(is_nsfw, 0) FROM bgm_subjects WHERE id IN `+database.Placeholders(len(parents)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]bool, len(parents))
	for rows.Next() {
		var id int64
		var isNSFW bool
		if err := rows.Scan(&id, &isNSFW); err != nil {
			return nil, err
		}
		out[id] = isNSFW
	}
	return out, rows.Err()
}

func diffTankobonBatch(ctx context.Context, books []tankobonBookRow, targets map[string]tankobonSeriesRow, items map[int64][]TankobonSubject, parentNSFW map[int64]bool) ([]tankobonBookUpdate, int, error) {
	var updates []tankobonBookUpdate
	matchedSeries := 0
	for start := 0; start < len(books); {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		seriesID := books[start].seriesID
		end := start + 1
		for end < len(books) && books[end].seriesID == seriesID {
			end++
		}
		target := targets[seriesID]
		batch := books[start:end]
		start = end
		if batch[0].chainVersion != target.chainVersion {
			return nil, 0, database.ErrMetadataChanged
		}
		if batch[0].parentID != target.subjectID {
			continue // The series was rebound after we selected its offline subject.
		}
		volumes := make([]VolumeMatchBook, 0, len(batch))
		byID := make(map[string]tankobonBookRow, len(batch))
		for _, book := range batch {
			volumes = append(volumes, book.VolumeMatchBook)
			byID[book.BookID] = book
		}
		matches := MatchBooksToTankobons(target.subjectID, target.EffectiveOneShot(), volumes, items[target.subjectID])
		if len(matches) != 0 {
			matchedSeries++
		}
		for i := range matches {
			match := &matches[i]
			if match.MatchedBy == "oneshot_fallback" {
				nsfw, found := parentNSFW[target.subjectID]
				match.NSFWKnown = found
				match.NSFW = target.isNSFW || (found && nsfw)
			}
			if update, changed := diffTankobonBook(byID[match.BookID], *match, target.chain); changed {
				updates = append(updates, update)
			}
		}
	}
	return updates, matchedSeries, nil
}

func (s *Service) syncOfflineTankobonBooks(ctx context.Context, force bool) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	a := s.archive
	a.mu.RLock()
	db, status, hasISBN := a.db, a.status, a.hasISBNCol
	a.mu.RUnlock()
	if db == nil || !status.Ready || status.NeedsTankobonReimport {
		return 0, nil
	}
	signature := tankobonBackfillSignature(status)
	prev, err := s.store.GetSetting(ctx, settingArchiveSyncedBooksDump)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	completed := prev == signature
	if completed && !force {
		return 0, nil
	}

	started := time.Now()
	plan, err := s.tankobonBackfillPlan(ctx)
	if err != nil {
		return 0, err
	}
	var cursor int64
	var updatedBooks, matchedSeries int
	planRetries := 0
	for {
		if err := ctx.Err(); err != nil {
			return updatedBooks, err
		}
		batch, err := s.tankobonSeriesBatch(ctx, cursor, plan)
		if err != nil {
			return updatedBooks, err
		}
		if len(batch) == 0 {
			break
		}
		previousCursor := cursor
		cursor = batch[len(batch)-1].rowID
		targets := make(map[string]tankobonSeriesRow, len(batch))
		var parents []int64
		seen := make(map[int64]bool, len(batch))
		for _, target := range batch {
			if target.chain.rank < 0 {
				continue
			}
			targets[target.ID] = target
			if !seen[target.subjectID] {
				seen[target.subjectID] = true
				parents = append(parents, target.subjectID)
			}
		}
		if len(targets) == 0 {
			continue
		}
		items, err := loadArchiveTankobonsBatch(ctx, db, hasISBN, parents)
		if err != nil {
			return updatedBooks, err
		}
		var oneshotParents []int64
		oneshotSeen := make(map[int64]bool)
		for id, target := range targets {
			if len(items[target.subjectID]) == 0 {
				if target.BooksCount != 1 {
					delete(targets, id)
					continue
				}
				if !oneshotSeen[target.subjectID] {
					oneshotSeen[target.subjectID] = true
					oneshotParents = append(oneshotParents, target.subjectID)
				}
			}
		}
		if len(targets) == 0 {
			continue
		}
		parentNSFW, err := loadArchiveSubjectNSFWBatch(ctx, db, oneshotParents)
		if err != nil {
			return updatedBooks, err
		}

		n, matched, err := s.applyTankobonBookBatch(ctx, db, targets, items, parentNSFW, completed)
		if errors.Is(err, database.ErrMetadataChanged) && planRetries < 3 {
			// A metadata-source sync can change a chain's version just after
			// the archive import queued us. Retry this batch with a fresh plan.
			plan, err = s.tankobonBackfillPlan(ctx)
			if err == nil {
				planRetries++
				cursor = previousCursor
				continue
			}
		}
		if err != nil {
			return updatedBooks, err
		}
		planRetries = 0
		matchedSeries += matched
		if n > 0 {
			completed = false
			updatedBooks += n
		}
	}

	if err := ctx.Err(); err != nil {
		return updatedBooks, err
	}
	if !completed {
		if err := s.markTankobonBackfillComplete(ctx, db, signature); err != nil {
			return updatedBooks, err
		}
	}
	duration := time.Since(started).Milliseconds()
	if s.logger != nil {
		s.logger.Info("backfilled offline Bangumi tankobon metadata", "updated_books", updatedBooks, "matched_series", matchedSeries, "duration_ms", duration)
	}
	diagnostics.Default().Record(diagnostics.Event{
		Level: diagnostics.LevelInfo, Component: "bangumi", Stage: "bangumi_archive", Action: "tankobon_backfill",
		DurationMs: duration, Meta: map[string]any{"updatedBooks": updatedBooks, "matchedSeries": matchedSeries},
	})
	return updatedBooks, nil
}

func (s *Service) markTankobonBackfillComplete(ctx context.Context, archiveDB *sql.DB, signature string) error {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at
WHERE settings.value<>excluded.value`, settingArchiveSyncedBooksDump, signature, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return s.commitForArchive(tx, archiveDB)
}

func (s *Service) applyTankobonBookBatch(ctx context.Context, archiveDB *sql.DB, targets map[string]tankobonSeriesRow, items map[int64][]TankobonSubject, parentNSFW map[int64]bool, clearCompletion bool) (int, int, error) {
	for attempt := 0; attempt < 3; attempt++ {
		books, err := loadTankobonBookBatch(ctx, s.store.DB(), targets)
		if err != nil {
			return 0, 0, err
		}
		updates, matched, err := diffTankobonBatch(ctx, books, targets, items, parentNSFW)
		if err != nil || len(updates) == 0 {
			return 0, matched, err
		}
		err = s.commitTankobonBookBatch(ctx, archiveDB, targets, books, updates, clearCompletion)
		if err == nil {
			return len(updates), matched, nil
		}
		if !errors.Is(err, database.ErrMetadataChanged) {
			return 0, 0, err
		}
	}
	return 0, 0, database.ErrMetadataChanged
}

func (s *Service) commitTankobonBookBatch(ctx context.Context, archiveDB *sql.DB, targets map[string]tankobonSeriesRow, before []tankobonBookRow, updates []tankobonBookUpdate, clearCompletion bool) error {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Revalidate all inputs that can change the match or its allowed fields
	// after the outside-transaction read, including concurrent manual edits.
	current, err := loadTankobonBookBatch(ctx, tx, targets)
	if err != nil {
		return err
	}
	if !slices.Equal(before, current) {
		return database.ErrMetadataChanged
	}
	visible, searchDirty, err := writeTankobonBookUpdates(ctx, tx, updates)
	if err != nil {
		return err
	}
	if visible {
		if _, err := tx.ExecContext(ctx, `UPDATE shelf_revision SET bulk_mode=0,catalog=catalog+1 WHERE id=1`); err != nil {
			return err
		}
	}
	if clearCompletion {
		// A forced pass for the same dump must also resume after a partial write.
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, settingArchiveSyncedBooksDump); err != nil {
			return err
		}
	}
	err = s.commitForArchive(tx, archiveDB)
	if err == nil && searchDirty {
		s.store.NotifyBookSearchDirty()
	}
	return err
}

func (s *Service) commitForArchive(tx *sql.Tx, archiveDB *sql.DB) error {
	s.archive.mu.RLock()
	defer s.archive.mu.RUnlock()
	if s.archive.db != archiveDB {
		return errors.New("Bangumi archive changed during tankobon backfill")
	}
	return tx.Commit()
}

func writeTankobonBookUpdates(ctx context.Context, tx *sql.Tx, updates []tankobonBookUpdate) (visible, searchDirty bool, err error) {
	metadata, err := tx.PrepareContext(ctx, `
INSERT INTO book_metadata(book_id,title,summary,release_date,isbn,bgm_subject_id,bgm_cover_url,bgm_cover_path,bgm_is_nsfw,match_origin,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(book_id) DO UPDATE SET title=excluded.title,summary=excluded.summary,release_date=excluded.release_date,isbn=excluded.isbn,
 bgm_subject_id=excluded.bgm_subject_id,bgm_cover_url=excluded.bgm_cover_url,bgm_cover_path=excluded.bgm_cover_path,
 bgm_is_nsfw=excluded.bgm_is_nsfw,updated_at=excluded.updated_at`)
	if err != nil {
		return false, false, err
	}
	defer metadata.Close()
	bindings, err := tx.PrepareContext(ctx, `
INSERT INTO book_source_bindings(book_id,source_id,series_entry_id,entry_id,match_origin,updated_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(book_id,source_id) DO UPDATE SET series_entry_id=excluded.series_entry_id,entry_id=excluded.entry_id,
 match_origin=excluded.match_origin,updated_at=excluded.updated_at`)
	if err != nil {
		return false, false, err
	}
	defer bindings.Close()
	covers, err := tx.PrepareContext(ctx, `
INSERT INTO book_cover_candidates(book_id,source_id,series_entry_id,entry_id,cover_kind,cover_ref,rank_order,is_nsfw,nsfw_known,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(book_id,source_id) DO UPDATE SET series_entry_id=excluded.series_entry_id,entry_id=excluded.entry_id,
 cover_kind=excluded.cover_kind,cover_ref=excluded.cover_ref,rank_order=excluded.rank_order,
 is_nsfw=excluded.is_nsfw,nsfw_known=excluded.nsfw_known,updated_at=excluded.updated_at`)
	if err != nil {
		return false, false, err
	}
	defer covers.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	bulk := slices.ContainsFunc(updates, func(update tankobonBookUpdate) bool { return update.writeMetadata })
	if bulk {
		if _, err := tx.ExecContext(ctx, `UPDATE shelf_revision SET bulk_mode=1 WHERE id=1`); err != nil {
			return false, false, err
		}
	}
	visible = bulk
	for _, update := range updates {
		if update.writeMetadata {
			m := update.metadata
			if _, err := metadata.ExecContext(ctx, update.id, m.title, m.summary, m.releaseDate, m.isbn, m.subjectID, m.coverURL, m.coverPath, m.nsfw, update.origin, now); err != nil {
				return false, false, err
			}
			searchDirty = searchDirty || update.searchDirty
		}
		if update.writeBinding {
			b := update.binding
			if _, err := bindings.ExecContext(ctx, update.id, update.sourceID, b.parent, b.entry, b.origin, now); err != nil {
				return false, false, err
			}
		}
		if update.writeCover {
			visible = true
			c := update.cover
			if _, err := covers.ExecContext(ctx, update.id, update.sourceID, c.parent, c.entry, c.kind, c.ref, c.rank, c.nsfw, c.nsfwKnown, now); err != nil {
				return false, false, err
			}
		}
	}
	return visible, searchDirty, nil
}
