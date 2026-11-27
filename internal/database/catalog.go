package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xJogger/fake-komga-115/internal/cjk"
)

const (
	defaultLargeUnpagedBooksThreshold = 2000
	progressTombstoneRetention        = 90 * 24 * time.Hour
	sqlInChunkSize                    = 400
)

type SeriesQuery struct {
	Search        string
	LibraryIDs    []string
	CollectionIDs []string
	ReadStatus    []string
	Statuses      []string
	Genres        []string
	Tags          []string
	Publishers    []string
	Authors       []string
	Languages     []string
	ScrapeStatus  string
	IncludeHidden bool
	GroupBangumi  bool
	NSFWMode      string // "" = follow ShowNSFW(ctx), "all" = both, "only" = nsfw only, "exclude" = non-nsfw only
	OneShot       *bool
	Empty         bool
	Page          int
	Size          int
	Sort          string
}

type BookQuery struct {
	Search             string
	LibraryIDs         []string
	ReadListIDs        []string
	SeriesID           string
	DirectSeriesAccess bool // Opening a known series uses authorization without the personal display filter.
	ReadStatus         []string
	Tags               []string
	Authors            []string
	OneShot            *bool
	Empty              bool
	Page               int
	Size               int
	Unpaged            bool
	Sort               string
}

func (s *Store) largeUnpagedThreshold() int64 {
	if s.unpagedBookThreshold > 0 {
		return s.unpagedBookThreshold
	}
	return defaultLargeUnpagedBooksThreshold
}

func (s *Store) querySeriesPage(ctx context.Context, q SeriesQuery) ([]Series, int64, error) {
	if q.Page < 0 {
		q.Page = 0
	}
	if q.Size <= 0 || q.Size > 500 {
		q.Size = 20
	}
	if q.Empty {
		return nil, 0, nil
	}
	if q.NSFWMode == "" {
		if s.ShowNSFW(ctx) {
			q.NSFWMode = "all"
		} else {
			q.NSFWMode = "exclude"
		}
	}
	var enabledLibs []string
	if !q.IncludeHidden {
		var err error
		enabledLibs, err = s.catalogLibraryIDs(ctx, q.LibraryIDs)
		if err != nil {
			return nil, 0, err
		}
		if len(enabledLibs) == 0 {
			return []Series{}, 0, nil
		}
	}
	needSeriesFTS := strings.TrimSpace(q.Search) != "" ||
		len(q.Publishers) > 0 || len(q.Authors) > 0 || len(q.Tags) > 0 || len(q.Genres) > 0
	seriesFTSReady := needSeriesFTS && s.ensureSearchFTSFresh(ctx, "s")

	searchDriven := false
	var searchCandSQL string
	var searchCandArgs []any
	source := " FROM series s"
	var sourceArgs []any
	if !q.GroupBangumi && strings.TrimSpace(q.Search) != "" {
		var searchLibs []string
		if !q.IncludeHidden {
			var libEmpty bool
			searchLibs, libEmpty = intersectLibraryIDs(q.LibraryIDs, enabledLibs)
			if libEmpty {
				return []Series{}, 0, nil
			}
		} else if len(q.LibraryIDs) > 0 {
			searchLibs = cleanStringSliceCaseSensitive(q.LibraryIDs)
		}
		if candSQL, candArgs, ok := buildSeriesSearchCandidateSQL(q.Search, searchLibs, seriesFTSReady); ok {
			searchCandSQL = candSQL
			searchCandArgs = candArgs
			source = " FROM (" + candSQL + ") _drv CROSS JOIN series s ON s.id=_drv.series_id"
			sourceArgs = candArgs
			searchDriven = true
		}
	}
	userID := UserIDFromContext(ctx)
	filterQ := q
	if searchDriven {
		filterQ.Search = ""
	}
	where, args, empty := buildSeriesFilters(filterQ, enabledLibs, userID, seriesFTSReady)
	if empty {
		return []Series{}, 0, nil
	}
	noExtraNonSearchFilters := len(q.CollectionIDs) == 0 &&
		q.OneShot == nil && len(q.Statuses) == 0 && len(q.Genres) == 0 &&
		len(q.Tags) == 0 && len(q.Publishers) == 0 && len(q.Authors) == 0 &&
		len(q.Languages) == 0 && strings.TrimSpace(q.ScrapeStatus) == "" &&
		len(q.ReadStatus) == 0
	noExtraFilters := noExtraNonSearchFilters && strings.TrimSpace(q.Search) == ""
	if !q.GroupBangumi && !searchDriven {
		if drvSQL, drvArgs, drvKind := selectiveSeriesDriver(q, userID, seriesFTSReady); drvSQL != "" {
			source = " FROM (" + drvSQL + ") _drv CROSS JOIN series s ON s.id=_drv.series_id"
			sourceArgs = drvArgs
			// Avoid evaluating the same selective driver subquery a second time inside WHERE.
			switch drvKind {
			case "collection":
				filterQ.CollectionIDs = nil
			case "publisher":
				filterQ.Publishers = nil
			case "author":
				filterQ.Authors = nil
			case "tag":
				filterQ.Tags = nil
			case "genre":
				filterQ.Genres = nil
			}
			where, args, empty = buildSeriesFilters(filterQ, enabledLibs, userID, seriesFTSReady)
			if empty {
				return []Series{}, 0, nil
			}
		}
	}
	sortValue := strings.ToLower(strings.TrimSpace(q.Sort))
	dir := sortDirection(sortValue)
	primary := `COALESCE(NULLIF((SELECT sm.title_sort FROM series_metadata sm WHERE sm.series_id=s.id),''), s.name) COLLATE NOCASE`
	ties := " " + dir + ",s.id " + dir
	var orderArgs []any
	// Search CTE path uses a single LEFT JOIN series_metadata sm instead of repeated correlated subqueries.
	searchJoinSM := true
	searchPrimary := `COALESCE(NULLIF(sm.title_sort,''), s.name) COLLATE NOCASE`
	searchTies := ties
	switch {
	case strings.Contains(sortValue, "random"):
		primary, ties = "RANDOM()", ""
		searchJoinSM = false
		searchPrimary, searchTies = primary, ties
	case strings.HasPrefix(sortValue, "relevance") && strings.TrimSpace(q.Search) != "":
		phrase := extractPrimarySearchPhrase(q.Search)
		key := cjk.SearchKey(phrase)
		if phrase != "" && key != "" {
			prefix := escapeLike(key) + "%"
			contains := "%" + escapeLike(key) + "%"
			primary = `CASE
 WHEN cjk_key(COALESCE(NULLIF((SELECT sm.title FROM series_metadata sm WHERE sm.series_id=s.id),''), s.name)) = ? THEN 0
 WHEN cjk_key(s.name) = ? THEN 1
 WHEN cjk_key(COALESCE(NULLIF((SELECT sm.title FROM series_metadata sm WHERE sm.series_id=s.id),''), s.name)) LIKE ? ESCAPE '\' THEN 2
 WHEN cjk_key(s.name) LIKE ? ESCAPE '\' THEN 3
 WHEN cjk_key(COALESCE(NULLIF((SELECT sm.title FROM series_metadata sm WHERE sm.series_id=s.id),''), s.name)) LIKE ? ESCAPE '\' THEN 4
 WHEN cjk_key(s.name) LIKE ? ESCAPE '\' THEN 5
 ELSE 6 END`
			ties = ` ASC, COALESCE(NULLIF((SELECT sm.title_sort FROM series_metadata sm WHERE sm.series_id=s.id),''), s.name) COLLATE NOCASE ASC, s.id ASC`
			searchPrimary = `CASE
 WHEN cjk_key(COALESCE(NULLIF(sm.title,''), s.name)) = ? THEN 0
 WHEN cjk_key(s.name) = ? THEN 1
 WHEN cjk_key(COALESCE(NULLIF(sm.title,''), s.name)) LIKE ? ESCAPE '\' THEN 2
 WHEN cjk_key(s.name) LIKE ? ESCAPE '\' THEN 3
 WHEN cjk_key(COALESCE(NULLIF(sm.title,''), s.name)) LIKE ? ESCAPE '\' THEN 4
 WHEN cjk_key(s.name) LIKE ? ESCAPE '\' THEN 5
 ELSE 6 END`
			searchTies = ` ASC, COALESCE(NULLIF(sm.title_sort,''), s.name) COLLATE NOCASE ASC, s.id ASC`
			orderArgs = append(orderArgs, key, key, prefix, prefix, contains, contains)
		}
	case strings.HasPrefix(sortValue, "lastmodifieddate"):
		primary = `s.sort_modified_at`
		ties = " " + dir + ",s.name COLLATE NOCASE ASC,s.id ASC"
		searchJoinSM = false
		searchPrimary, searchTies = primary, ties
	case strings.HasPrefix(sortValue, "createddate"):
		primary = `s.sort_created_at`
		ties = " " + dir + ",s.name COLLATE NOCASE ASC,s.id ASC"
		searchJoinSM = false
		searchPrimary, searchTies = primary, ties
	case strings.HasPrefix(sortValue, "bgmrank"):
		primary = `(SELECT sm.bgm_rank FROM series_metadata sm WHERE sm.series_id=s.id AND sm.bgm_rank>0)`
		ties = " " + dir + " NULLS LAST,s.name COLLATE NOCASE ASC,s.id ASC"
		searchPrimary = `CASE WHEN sm.bgm_rank>0 THEN sm.bgm_rank END`
		searchTies = ties
	case strings.HasPrefix(sortValue, "bgmscore"):
		primary = `COALESCE((SELECT sm.bgm_score FROM series_metadata sm WHERE sm.series_id=s.id), -1)`
		ties = " " + dir + ",s.name COLLATE NOCASE ASC,s.id ASC"
		searchPrimary = `COALESCE(sm.bgm_score, -1)`
		searchTies = ties
	}

	var total int64
	if q.GroupBangumi {
		// Apply visibility and other filters before grouping/pagination. Pick the
		// edition with the most volumes (books_count DESC, then union library,
		// library name, series name, id) as the representative anchor while keeping
		// group-level date/rank/score rollups from all eligible versions.
		groupKey := "CASE WHEN NOT " + effectiveOneShotSQL + " AND gm.bgm_subject_id>0 THEN 'bgm:'||gm.bgm_subject_id ELSE 'series:'||s.id END"
		baseFrom := " FROM series s JOIN libraries gl ON gl.id=s.library_id LEFT JOIN series_metadata gm ON gm.series_id=s.id" + where
		groupCount := "SELECT count(*) FROM (SELECT 1" + baseFrom + " GROUP BY " + groupKey + ")"
		if err := s.db.QueryRowContext(ctx, groupCount, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
		if total == 0 {
			return []Series{}, 0, nil
		}
		sortExpr := ""
		switch {
		case strings.HasPrefix(sortValue, "lastmodifieddate"):
			sortExpr = ", MAX(s.sort_modified_at) OVER (PARTITION BY " + groupKey + ") AS sort_value"
			primary = "grouped.sort_value"
		case strings.HasPrefix(sortValue, "createddate"):
			sortExpr = ", MAX(s.sort_created_at) OVER (PARTITION BY " + groupKey + ") AS sort_value"
			primary = "grouped.sort_value"
		case strings.HasPrefix(sortValue, "bgmrank"):
			sortExpr = ", MIN(CASE WHEN gm.bgm_rank>0 THEN gm.bgm_rank END) OVER (PARTITION BY " + groupKey + ") AS sort_value"
			primary = "grouped.sort_value"
		case strings.HasPrefix(sortValue, "bgmscore"):
			sortExpr = ", COALESCE(MAX(gm.bgm_score), -1) OVER (PARTITION BY " + groupKey + ") AS sort_value"
			primary = "grouped.sort_value"
		}
		rankedSelect := "SELECT s.id AS id, ROW_NUMBER() OVER (" +
			"PARTITION BY " + groupKey +
			" ORDER BY s.books_count DESC, CASE WHEN gl.provider_type='union' THEN 0 ELSE 1 END ASC, gl.name COLLATE NOCASE ASC, s.name COLLATE NOCASE ASC, s.id ASC" +
			") AS _rn" + sortExpr + baseFrom
		groupedCols := "id"
		if sortExpr != "" {
			groupedCols += ", sort_value"
		}
		source = " FROM series s JOIN (SELECT " + groupedCols + " FROM (" + rankedSelect + ") WHERE _rn=1) grouped ON grouped.id=s.id"
		sourceArgs = nil
		where = ""
	} else if strings.TrimSpace(q.Search) != "" {
		// Single-pass query via MATERIALIZED candidate ID CTE: avoids executing search twice
		// while preserving SQLite's bounded top-N heap for ORDER BY ... LIMIT (unlike COUNT(*) OVER()).
		var candCTE string
		var candQueryArgs []any
		switch {
		case searchDriven && noExtraNonSearchFilters && q.NSFWMode == "exclude":
			candCTE = `WITH _cand(series_id) AS MATERIALIZED (SELECT series_id FROM (` + searchCandSQL + `) EXCEPT SELECT sm_n.series_id FROM series_metadata sm_n WHERE sm_n.is_nsfw=1)`
			candQueryArgs = searchCandArgs
		case searchDriven && noExtraNonSearchFilters && q.NSFWMode == "all":
			candCTE = `WITH _cand(series_id) AS MATERIALIZED (` + searchCandSQL + `)`
			candQueryArgs = searchCandArgs
		case searchDriven:
			candCTE = `WITH _cand(series_id) AS MATERIALIZED (SELECT s.id AS series_id` + source + where + `)`
			candQueryArgs = append(append([]any(nil), sourceArgs...), args...)
		default:
			candCTE = `WITH _cand(series_id) AS MATERIALIZED (SELECT s.id AS series_id` + source + where + `)`
			candQueryArgs = append(append([]any(nil), sourceArgs...), args...)
		}
		smJoin := ""
		if searchJoinSM {
			smJoin = ` LEFT JOIN series_metadata sm ON sm.series_id=s.id`
		}
		query := candCTE + `, _tot AS (SELECT COUNT(*) AS n FROM _cand)
SELECT s.id,s.library_id,s.cid,s.name,s.relative_path,s.one_shot,s.one_shot_override,s.file_modified_at,s.created_at,s.updated_at,s.books_count,
 (SELECT n FROM _tot) AS _total
FROM _cand _c
CROSS JOIN series s ON s.id=_c.series_id` + smJoin + ` ORDER BY ` + searchPrimary + searchTies + ` LIMIT ? OFFSET ?`
		queryArgs := make([]any, 0, len(candQueryArgs)+len(orderArgs)+2)
		queryArgs = append(queryArgs, candQueryArgs...)
		queryArgs = append(queryArgs, orderArgs...)
		queryArgs = append(queryArgs, q.Size, q.Page*q.Size)
		rows, err := s.db.QueryContext(ctx, query, queryArgs...)
		if err != nil {
			return nil, 0, err
		}
		defer rows.Close()
		items, winTotal, err := scanSeriesRowsWithTotal(rows)
		if err != nil {
			return nil, 0, err
		}
		if len(items) > 0 || q.Page == 0 {
			return items, winTotal, nil
		}
		// Rare case: out-of-range page > 0 returned 0 rows, fetch exact total.
		if err := s.db.QueryRowContext(ctx, candCTE+` SELECT COUNT(*) FROM _cand`, candQueryArgs...).Scan(&total); err != nil {
			return nil, 0, err
		}
		return []Series{}, total, nil
	} else {
		countDone := false
		if noExtraFilters && !q.IncludeHidden && len(q.LibraryIDs) == 0 &&
			((s.ShowNSFW(ctx) && q.NSFWMode == "all") || (!s.ShowNSFW(ctx) && q.NSFWMode == "exclude")) {
			if _, cnt, _, _, cErr := s.Counts(ctx); cErr == nil {
				total = cnt
				countDone = true
			}
		}
		if !countDone {
			countArgs := append(append([]any(nil), sourceArgs...), args...)
			if err := s.db.QueryRowContext(ctx, "SELECT count(*)"+source+where, countArgs...).Scan(&total); err != nil {
				return nil, 0, err
			}
		}
		if total == 0 {
			return []Series{}, 0, nil
		}
		// Fast path for default Bangumi rank ASC on unfiltered visible libraries:
		// drive directly from partial covering index idx_series_metadata_bgm_rank.
		if noExtraFilters && !q.IncludeHidden && strings.HasPrefix(sortValue, "bgmrank") && dir == "ASC" && q.NSFWMode != "only" {
			if fastItems, ok, err := s.querySeriesBgmRankAscFast(ctx, q, enabledLibs, total); err != nil {
				return nil, 0, err
			} else if ok {
				return fastItems, total, nil
			}
		}
	}
	query := `
SELECT s.id,s.library_id,s.cid,s.name,s.relative_path,s.one_shot,s.one_shot_override,s.file_modified_at,s.created_at,s.updated_at,s.books_count` +
		source + where + ` ORDER BY ` + primary + ties + ` LIMIT ? OFFSET ?`
	queryArgs := make([]any, 0, len(sourceArgs)+len(args)+len(orderArgs)+2)
	queryArgs = append(queryArgs, sourceArgs...)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, orderArgs...)
	queryArgs = append(queryArgs, q.Size, q.Page*q.Size)
	rows, err := s.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items, err := scanSeriesRows(rows)
	return items, total, err
}

func selectiveSeriesDriver(q SeriesQuery, userID string, useFTS bool) (string, []any, string) {
	if len(q.CollectionIDs) > 0 {
		ids := cleanStringSliceCaseSensitive(q.CollectionIDs)
		args := make([]any, 0, 1+len(ids))
		args = append(args, userID)
		for _, id := range ids {
			args = append(args, id)
		}
		return `SELECT DISTINCT cs.series_id FROM collection_series cs JOIN collections c ON c.id=cs.collection_id WHERE c.user_id=? AND cs.collection_id IN ` + Placeholders(len(ids)), args, "collection"
	}
	if len(q.Publishers) > 0 {
		sql, args := buildFoldedTaxonomySeriesSubquery("sm", "series_metadata", "publisher", "publisher_fts", q.Publishers, useFTS)
		return sql, args, "publisher"
	}
	if len(q.Authors) > 0 {
		sql, args := buildFoldedTaxonomySeriesSubquery("sa", "series_authors", "name", "authors_fts", q.Authors, useFTS)
		return sql, args, "author"
	}
	if len(q.Tags) > 0 {
		sql, args := buildFoldedTaxonomySeriesSubquery("st", "series_tags", "tag", "tags_fts", q.Tags, useFTS)
		return sql, args, "tag"
	}
	if len(q.Genres) > 0 {
		sql, args := buildFoldedTaxonomySeriesSubquery("sg", "series_genres", "genre", "genres_fts", q.Genres, useFTS)
		return sql, args, "genre"
	}
	return "", nil, ""
}

func (s *Store) querySeriesBgmRankAscFast(
	ctx context.Context,
	q SeriesQuery,
	enabledLibs []string,
	total int64,
) ([]Series, bool, error) {
	libs, empty := intersectLibraryIDs(q.LibraryIDs, enabledLibs)
	if empty {
		return []Series{}, true, nil
	}
	offset := q.Page * q.Size
	// Fetch an extra slice so ties on the boundary of bgm_rank are ordered identically by name/id.
	nsfwClause := ""
	if q.NSFWMode != "all" {
		nsfwClause = " AND sm.is_nsfw=0"
	}
	args := make([]any, 0, len(libs)+2)
	for _, id := range libs {
		args = append(args, id)
	}
	args = append(args, q.Size+1, offset)
	query := `
SELECT s.id,s.library_id,s.cid,s.name,s.relative_path,s.one_shot,s.one_shot_override,s.file_modified_at,s.created_at,s.updated_at,s.books_count
FROM series_metadata sm
CROSS JOIN series s ON s.id=sm.series_id
WHERE sm.bgm_rank>0` + nsfwClause + ` AND s.library_id IN ` + Placeholders(len(libs)) + `
ORDER BY sm.bgm_rank ASC, s.name COLLATE NOCASE ASC, s.id ASC
LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items, err := scanSeriesRows(rows)
	if err != nil {
		return nil, false, err
	}
	if len(items) > q.Size {
		return items[:q.Size], true, nil
	}
	if int64(offset+len(items)) >= total {
		return items, true, nil
	}
	return nil, false, nil
}

func sortDirection(value string) string {
	if strings.HasSuffix(value, ",desc") {
		return "DESC"
	}
	return "ASC"
}

func (s *Store) SeriesByID(ctx context.Context, id string) (Series, error) {
	return s.seriesByID(ctx, id, false, !s.ShowNSFW(ctx))
}

func (s *Store) SeriesByIDAny(ctx context.Context, id string, includeHidden bool) (Series, error) {
	return s.seriesByID(ctx, id, includeHidden, false)
}

func (s *Store) resolveUnionSeriesID(ctx context.Context, sourceSeriesID string) (string, bool) {
	var unionSeriesID string
	err := s.db.QueryRowContext(ctx, `
SELECT uss.union_series_id
FROM union_series_sources uss
JOIN series src ON src.id=uss.source_series_id
JOIN libraries sl ON sl.id=src.library_id AND sl.enabled=1
JOIN series us ON us.id=uss.union_series_id
JOIN libraries ul ON ul.id=us.library_id AND ul.enabled=1
WHERE uss.source_series_id=?
LIMIT 1`, sourceSeriesID).Scan(&unionSeriesID)
	if err != nil || unionSeriesID == "" {
		return "", false
	}
	return unionSeriesID, true
}

func (s *Store) resolveUnionBookID(ctx context.Context, sourceBookID string) (string, bool) {
	var unionBookID string
	err := s.db.QueryRowContext(ctx, `
SELECT ubs.union_book_id
FROM union_book_sources ubs
JOIN books src ON src.id=ubs.source_book_id
JOIN libraries sl ON sl.id=src.library_id AND sl.enabled=1
JOIN books ub ON ub.id=ubs.union_book_id
JOIN libraries ul ON ul.id=ub.library_id AND ul.enabled=1
WHERE ubs.source_book_id=?
LIMIT 1`, sourceBookID).Scan(&unionBookID)
	if err != nil || unionBookID == "" {
		return "", false
	}
	return unionBookID, true
}

func (s *Store) seriesByID(ctx context.Context, id string, includeHidden, excludeNSFW bool) (Series, error) {
	item, err := s.seriesByIDDirect(ctx, id, includeHidden, excludeNSFW)
	if err == nil || includeHidden || !errors.Is(err, sql.ErrNoRows) {
		return item, err
	}
	// Transparent compatibility alias for clients (e.g. Mihon saved manga / bookmarks)
	// requesting an old physical sub-library series ID after enrollment into an enabled union library.
	if unionID, ok := s.resolveUnionSeriesID(ctx, id); ok && unionID != id {
		return s.seriesByIDDirect(ctx, unionID, false, excludeNSFW)
	}
	return Series{}, err
}

func (s *Store) seriesByIDDirect(ctx context.Context, id string, includeHidden, excludeNSFW bool) (Series, error) {
	var item Series
	var modified sql.NullString
	var created, updated string
	query := `
SELECT s.id,s.library_id,s.cid,s.name,s.relative_path,s.one_shot,s.one_shot_override,s.file_modified_at,s.created_at,s.updated_at,s.books_count
FROM series s`
	if !includeHidden {
		query += ` JOIN libraries l ON l.id=s.library_id AND l.enabled=1 AND NOT EXISTS (SELECT 1 FROM union_layers ul WHERE ul.member_library_id=l.id)`
	}
	query += ` WHERE s.id=?`
	if excludeNSFW {
		query += ` AND NOT EXISTS (SELECT 1 FROM series_metadata sm_nsfw WHERE sm_nsfw.series_id=s.id AND sm_nsfw.is_nsfw=1)`
	}
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&item.ID, &item.LibraryID, &item.CID, &item.Name, &item.RelativePath, &item.OneShot, &item.OneShotOverride,
		&modified, &created, &updated, &item.BooksCount,
	)
	if err != nil {
		return Series{}, err
	}
	if !includeHidden {
		if scope, ok := UserScopeFromContext(ctx); ok && !scope.AllowsLibrary(item.LibraryID) {
			return Series{}, sql.ErrNoRows
		}
	}
	item.FileModifiedAt = parseNullableTime(modified)
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, nil
}

func isRecentProgressQuery(q BookQuery) bool {
	return len(q.ReadStatus) == 1 && strings.EqualFold(strings.TrimSpace(q.ReadStatus[0]), "IN_PROGRESS") &&
		strings.EqualFold(strings.TrimSpace(q.Sort), "readProgress.readDate,desc")
}

// progressTableMode inspects ReadStatus and returns whether books can be driven
// directly from book_read_progress ("in_progress", "read", "any_progress", or "").
func progressTableMode(statuses []string) string {
	if len(statuses) == 0 {
		return ""
	}
	hasRead, hasInProgress := false, false
	for _, raw := range statuses {
		switch strings.ToUpper(strings.TrimSpace(raw)) {
		case "READ":
			hasRead = true
		case "IN_PROGRESS":
			hasInProgress = true
		case "UNREAD":
			return ""
		}
	}
	switch {
	case hasRead && hasInProgress:
		return "any_progress"
	case hasInProgress:
		return "in_progress"
	case hasRead:
		return "read"
	default:
		return ""
	}
}

func isGlobalUnfilteredUnpagedBooks(q BookQuery) bool {
	return q.Unpaged &&
		q.SeriesID == "" &&
		strings.TrimSpace(q.Search) == "" &&
		len(q.LibraryIDs) == 0 &&
		len(q.ReadListIDs) == 0 &&
		len(q.ReadStatus) == 0 &&
		len(q.Tags) == 0 &&
		len(q.Authors) == 0 &&
		q.OneShot == nil &&
		strings.TrimSpace(q.Sort) == ""
}

func (s *Store) queryBooksPage(ctx context.Context, q BookQuery) ([]Book, int64, error) {
	if q.Page < 0 {
		q.Page = 0
	}
	if q.Size <= 0 || q.Size > 1000 {
		q.Size = 20
	}
	if q.Empty {
		return nil, 0, nil
	}
	var enabledLibs []string
	if q.SeriesID != "" {
		series, err := s.SeriesByID(ctx, q.SeriesID)
		if errors.Is(err, sql.ErrNoRows) {
			return []Book{}, 0, nil
		}
		if err != nil {
			return nil, 0, err
		}
		if scope, ok := UserScopeFromContext(ctx); ok && !q.DirectSeriesAccess && !scope.DisplaysLibrary(series.LibraryID) {
			return []Book{}, 0, nil
		}
		q.SeriesID = series.ID
		enabledLibs = []string{series.LibraryID}
	} else {
		var err error
		enabledLibs, err = s.catalogLibraryIDs(ctx, q.LibraryIDs)
		if err != nil {
			return nil, 0, err
		}
		if len(enabledLibs) == 0 {
			return []Book{}, 0, nil
		}
	}

	// Large-library protection for Koharia's global unfiltered GET /api/v1/books?unpaged=true&deleted=false:
	// instead of serializing 200k+ untouched unread books (~240 MiB JSON), return all visible books
	// belonging to every series with active reading progress or a recent (90-day) progress reset tombstone.
	if isGlobalUnfilteredUnpagedBooks(q) {
		_, _, visibleBooks, _, cErr := s.Counts(ctx)
		if cErr == nil && visibleBooks > s.largeUnpagedThreshold() {
			return s.queryProgressScopedBooks(ctx, enabledLibs, !s.ShowNSFW(ctx))
		}
	}

	needSeriesFTS := strings.TrimSpace(q.Search) != "" || len(q.Tags) > 0 || len(q.Authors) > 0
	needBooksFTS := strings.TrimSpace(q.Search) != ""
	seriesFTSReady := needSeriesFTS && s.ensureSearchFTSFresh(ctx, "s")
	booksFTSReady := needBooksFTS && s.ensureSearchFTSFresh(ctx, "b")

	cacheCount := q.SeriesID == "" && q.Search == "" &&
		len(q.ReadListIDs)+len(q.ReadStatus)+len(q.Tags)+len(q.Authors) == 0
	progMode := progressTableMode(q.ReadStatus)
	if progMode != "" {
		q.ReadStatus = nil
	}
	bookSearchDriven := false
	var bookCandSQL string
	var bookCandArgs []any
	source := " FROM books b"
	var sourceArgs []any
	if progMode == "" && q.SeriesID == "" && strings.TrimSpace(q.Search) != "" {
		searchLibs, libEmpty := intersectLibraryIDs(q.LibraryIDs, enabledLibs)
		if libEmpty {
			return []Book{}, 0, nil
		}
		if candSQL, candArgs, ok := buildBookSearchCandidateSQL(q.Search, searchLibs, seriesFTSReady, booksFTSReady); ok {
			bookCandSQL = candSQL
			bookCandArgs = candArgs
			source = " FROM (" + candSQL + ") _bdrv CROSS JOIN books b ON b.rowid=_bdrv.book_rowid"
			sourceArgs = candArgs
			bookSearchDriven = true
		}
	}
	filterQ := q
	if bookSearchDriven {
		filterQ.Search = ""
	}
	userID := UserIDFromContext(ctx)
	excludeNSFW := !s.ShowNSFW(ctx)
	where, args, empty := buildBookFilters(filterQ, enabledLibs, excludeNSFW, userID, seriesFTSReady, booksFTSReady)
	if empty {
		return []Book{}, 0, nil
	}
	if progMode != "" {
		source = " FROM book_read_progress p CROSS JOIN books b ON b.id=p.book_id"
		where += " AND p.user_id=?"
		args = append(args, userID)
		switch progMode {
		case "in_progress":
			where += " AND p.completed=0"
		case "read":
			where += " AND p.completed=1"
		}
	}
	order := bookNumberOrder
	sortValue := strings.ToLower(strings.TrimSpace(q.Sort))
	var orderArgs []any
	switch {
	case strings.Contains(sortValue, "random"):
		order = "RANDOM()"
	case strings.HasPrefix(sortValue, "relevance") && strings.TrimSpace(q.Search) != "":
		phrase := extractPrimarySearchPhrase(q.Search)
		key := cjk.SearchKey(phrase)
		if phrase != "" && key != "" {
			prefix := escapeLike(key) + "%"
			contains := "%" + escapeLike(key) + "%"
			order = `CASE
 WHEN cjk_key(b.name) = ? THEN 0
 WHEN cjk_key(b.name) LIKE ? ESCAPE '\' THEN 1
 WHEN cjk_key(b.name) LIKE ? ESCAPE '\' THEN 2
 ELSE 3 END ASC, b.name COLLATE NOCASE ASC, b.effective_number_sort ASC, b.id ASC`
			orderArgs = append(orderArgs, key, prefix, contains)
		}
	case strings.HasPrefix(sortValue, "readprogress.readdate"), strings.HasPrefix(sortValue, "readdate"):
		if progMode != "" {
			order = "p.updated_at " + sortDirection(sortValue) + ",b.effective_number_sort ASC,b.id ASC"
		} else {
			order = `COALESCE((SELECT p.updated_at FROM book_read_progress p WHERE p.user_id=? AND p.book_id=b.id), '') ` +
				sortDirection(sortValue) + `,b.effective_number_sort ASC,b.id ASC`
			orderArgs = append(orderArgs, userID)
		}
	case strings.HasPrefix(sortValue, "lastmodifieddate"):
		order = "COALESCE(b.file_modified_at,b.updated_at) " +
			sortDirection(sortValue) + ",b.name COLLATE NOCASE ASC,b.id ASC"
	case strings.HasPrefix(sortValue, "createddate"):
		order = "COALESCE(b.file_created_at,b.created_at) " +
			sortDirection(sortValue) + ",b.name COLLATE NOCASE ASC,b.id ASC"
	case strings.HasPrefix(sortValue, "metadata.title"):
		order = "b.name COLLATE NOCASE " +
			sortDirection(sortValue) + ",b.id " + sortDirection(sortValue)
	case strings.Contains(sortValue, "name,desc"):
		order = "b.name COLLATE NOCASE DESC,b.id DESC"
	case strings.Contains(sortValue, "name,asc"):
		order = "b.name COLLATE NOCASE ASC,b.id ASC"
	case strings.Contains(sortValue, ",desc"):
		order = reverseBookNumberOrder
	}

	// For paged search queries, materialize matching 8-byte book rowids once (avoiding
	// double execution and keeping SQLite's bounded top-N heap for ORDER BY ... LIMIT),
	// and defer LEFT JOIN zip_indexes until AFTER LIMIT/OFFSET.
	if strings.TrimSpace(q.Search) != "" && !q.Unpaged && progMode == "" {
		noExtraBookFilters := q.SeriesID == "" && len(q.ReadListIDs) == 0 && len(q.ReadStatus) == 0 &&
			len(q.Tags) == 0 && len(q.Authors) == 0 && q.OneShot == nil
		var candCTE string
		var candQueryArgs []any
		switch {
		case bookSearchDriven && noExtraBookFilters && excludeNSFW:
			candCTE = `WITH _cand(book_rowid) AS MATERIALIZED (SELECT book_rowid FROM (` + bookCandSQL + `) EXCEPT SELECT b4.rowid FROM series_metadata sm_n CROSS JOIN books b4 ON b4.series_id=sm_n.series_id WHERE sm_n.is_nsfw=1)`
			candQueryArgs = bookCandArgs
		case bookSearchDriven && noExtraBookFilters:
			candCTE = `WITH _cand(book_rowid) AS MATERIALIZED (` + bookCandSQL + `)`
			candQueryArgs = bookCandArgs
		default:
			candCTE = `WITH _cand(book_rowid) AS MATERIALIZED (SELECT b.rowid AS book_rowid` + source + where + `)`
			candQueryArgs = append(append([]any(nil), sourceArgs...), args...)
		}
		query := candCTE + `, _tot AS (SELECT COUNT(*) AS n FROM _cand), _page AS (
SELECT b.id,b.series_id,b.library_id,b.file_id,b.parent_cid,b.name,b.size,b.pick_code,b.sha1,
 b.file_created_at,b.file_modified_at,b.effective_number_sort AS number_sort,b.created_at,b.updated_at
FROM _cand _c
CROSS JOIN books b ON b.rowid=_c.book_rowid
ORDER BY ` + order + ` LIMIT ? OFFSET ?
)
SELECT p.id,p.series_id,p.library_id,p.file_id,p.parent_cid,p.name,p.size,p.pick_code,p.sha1,
 p.file_created_at,p.file_modified_at,p.number_sort,p.created_at,p.updated_at,
 coalesce(z.page_count,0),(SELECT n FROM _tot) AS _total
FROM _page p
LEFT JOIN zip_indexes z ON z.book_id=p.id`
		queryArgs := make([]any, 0, len(candQueryArgs)+len(orderArgs)+2)
		queryArgs = append(queryArgs, candQueryArgs...)
		queryArgs = append(queryArgs, orderArgs...)
		queryArgs = append(queryArgs, q.Size, q.Page*q.Size)
		rows, err := s.db.QueryContext(ctx, query, queryArgs...)
		if err != nil {
			return nil, 0, err
		}
		defer rows.Close()
		items, winTotal, err := scanBookRowsWithTotal(rows)
		if err != nil {
			return nil, 0, err
		}
		if len(items) > 0 || q.Page == 0 {
			return items, winTotal, nil
		}
		var total int64
		if err := s.db.QueryRowContext(ctx, candCTE+` SELECT COUNT(*) FROM _cand`, candQueryArgs...).Scan(&total); err != nil {
			return nil, 0, err
		}
		return []Book{}, total, nil
	}

	countArgs := append(append([]any(nil), sourceArgs...), args...)
	count := func(ctx context.Context) (int64, error) {
		var total int64
		err := s.db.QueryRowContext(ctx, "SELECT count(*)"+source+where, countArgs...).Scan(&total)
		return total, err
	}
	var total int64
	var err error
	if cacheCount {
		// Use the same library intersection as the SQL filters, including personal visibility.
		libraries, _ := intersectLibraryIDs(q.LibraryIDs, enabledLibs)
		total, err = s.cachedBookCount(ctx, libraries, excludeNSFW, q.OneShot, count)
	} else {
		total, err = count(ctx)
	}
	if err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []Book{}, 0, nil
	}
	query := bookPageSelect(source, where, order, q.SeriesID != "")
	queryArgs := make([]any, 0, len(sourceArgs)+len(args)+len(orderArgs)+2)
	queryArgs = append(queryArgs, sourceArgs...)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, orderArgs...)
	if !q.Unpaged {
		query += ` LIMIT ? OFFSET ?`
		queryArgs = append(queryArgs, q.Size, q.Page*q.Size)
	}
	rows, err := s.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items, err := scanBookRows(rows)
	return items, total, err
}

// Keep the page in reading order using the corresponding index. The separate
// count query can use the most selective index for its filters.
func bookPageSelect(source, where, order string, singleSeries bool) string {
	if source == " FROM books b" && (order == bookNumberOrder || order == reverseBookNumberOrder) {
		index := "idx_books_eff_number"
		if singleSeries {
			index = "idx_books_series_eff_number"
		}
		source += " INDEXED BY " + index
	}
	return `
SELECT b.id,b.series_id,b.library_id,b.file_id,b.parent_cid,b.name,b.size,b.pick_code,b.sha1,
 b.file_created_at,b.file_modified_at,b.effective_number_sort,b.created_at,b.updated_at,
 coalesce(z.page_count,0)` + source + ` LEFT JOIN zip_indexes z ON z.book_id=b.id` + where + ` ORDER BY ` + order
}

func (s *Store) queryProgressScopedBooks(
	ctx context.Context,
	enabledLibs []string,
	excludeNSFW bool,
) ([]Book, int64, error) {
	userID := UserIDFromContext(ctx)
	cutoff := time.Now().UTC().Add(-progressTombstoneRetention).Format(time.RFC3339Nano)
	libArgs := make([]any, 0, 3+len(enabledLibs))
	libArgs = append(libArgs, userID, userID, cutoff)
	for _, id := range enabledLibs {
		libArgs = append(libArgs, id)
	}
	// Resolve the small set of visible series IDs first (typically tens of rows) so the
	// books lookup uses idx_books_series_number instead of scanning all 200k+ books by library.
	seriesQuery := `
SELECT DISTINCT src.series_id
FROM (
  SELECT series_id FROM book_read_progress WHERE user_id=?
  UNION
  SELECT series_id FROM book_read_progress_tombstones WHERE user_id=? AND deleted_at>=?
) src
CROSS JOIN series s ON s.id=src.series_id
WHERE s.library_id IN ` + Placeholders(len(enabledLibs))
	if excludeNSFW {
		seriesQuery += ` AND NOT EXISTS (SELECT 1 FROM series_metadata sm WHERE sm.series_id=s.id AND sm.is_nsfw=1)`
	}
	seriesQuery += ` ORDER BY src.series_id ASC`
	sRows, err := s.db.QueryContext(ctx, seriesQuery, libArgs...)
	if err != nil {
		return nil, 0, err
	}
	var seriesIDs []string
	for sRows.Next() {
		var sid string
		if err := sRows.Scan(&sid); err != nil {
			sRows.Close()
			return nil, 0, err
		}
		seriesIDs = append(seriesIDs, sid)
	}
	if err := sRows.Err(); err != nil {
		sRows.Close()
		return nil, 0, err
	}
	sRows.Close()
	if len(seriesIDs) == 0 {
		return []Book{}, 0, nil
	}

	out := make([]Book, 0, len(seriesIDs)*8)
	for _, chunk := range chunkStrings(seriesIDs, sqlInChunkSize) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		query := `
SELECT b.id,b.series_id,b.library_id,b.file_id,b.parent_cid,b.name,b.size,b.pick_code,b.sha1,
 b.file_created_at,b.file_modified_at,b.effective_number_sort,b.created_at,b.updated_at,
 coalesce(z.page_count,0)
FROM books b
LEFT JOIN zip_indexes z ON z.book_id=b.id
WHERE b.series_id IN ` + Placeholders(len(chunk)) + `
ORDER BY b.series_id ASC, b.effective_number_sort ASC, b.name COLLATE NOCASE ASC, b.id ASC`
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, 0, err
		}
		part, err := scanBookRows(rows)
		rows.Close()
		if err != nil {
			return nil, 0, err
		}
		out = append(out, part...)
	}
	return out, int64(len(out)), nil
}

func (s *Store) BookByID(ctx context.Context, id string) (Book, error) {
	return s.bookByID(ctx, id, false, !s.ShowNSFW(ctx))
}

func (s *Store) BookByIDAny(ctx context.Context, id string) (Book, error) {
	return s.bookByID(ctx, id, true, false)
}

func (s *Store) bookByID(ctx context.Context, id string, includeHidden, excludeNSFW bool) (Book, error) {
	item, err := s.bookByIDDirect(ctx, id, includeHidden, excludeNSFW)
	if err == nil || includeHidden || !errors.Is(err, sql.ErrNoRows) {
		return item, err
	}
	if unionID, ok := s.resolveUnionBookID(ctx, id); ok && unionID != id {
		return s.bookByIDDirect(ctx, unionID, false, excludeNSFW)
	}
	return Book{}, err
}

func (s *Store) bookByIDDirect(ctx context.Context, id string, includeHidden, excludeNSFW bool) (Book, error) {
	var item Book
	var fileCreated, fileModified sql.NullString
	var created, updated string
	query := `
SELECT b.id,b.series_id,b.library_id,b.file_id,b.parent_cid,b.name,b.size,b.pick_code,b.sha1,
 b.file_created_at,b.file_modified_at,b.effective_number_sort,b.created_at,b.updated_at,
 coalesce(z.page_count,0)
FROM books b`
	if !includeHidden {
		query += ` JOIN libraries l ON l.id=b.library_id AND l.enabled=1 AND NOT EXISTS (SELECT 1 FROM union_layers ul WHERE ul.member_library_id=l.id)`
	}
	query += `
LEFT JOIN zip_indexes z ON z.book_id=b.id
WHERE b.id=?`
	if excludeNSFW {
		query += ` AND NOT EXISTS (SELECT 1 FROM series_metadata sm_nsfw WHERE sm_nsfw.series_id=b.series_id AND sm_nsfw.is_nsfw=1)`
	}
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&item.ID, &item.SeriesID, &item.LibraryID, &item.FileID, &item.ParentCID,
		&item.Name, &item.Size, &item.PickCode, &item.SHA1, &fileCreated, &fileModified,
		&item.NumberSort, &created, &updated, &item.PageCount,
	)
	if err != nil {
		return Book{}, err
	}
	if !includeHidden {
		if scope, ok := UserScopeFromContext(ctx); ok && !scope.AllowsLibrary(item.LibraryID) {
			return Book{}, sql.ErrNoRows
		}
	}
	item.FileCreatedAt = parseNullableTime(fileCreated)
	item.FileModifiedAt = parseNullableTime(fileModified)
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, nil
}

func (s *Store) SeriesByIDs(ctx context.Context, ids []string) (map[string]Series, error) {
	out := make(map[string]Series, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	unique := cleanStringSliceCaseSensitive(ids)
	excludeNSFW := !s.ShowNSFW(ctx)
	scope, hasScope := UserScopeFromContext(ctx)
	for _, chunk := range chunkStrings(unique, sqlInChunkSize) {
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}
		query := `
SELECT s.id,s.library_id,s.cid,s.name,s.relative_path,s.one_shot,s.one_shot_override,s.file_modified_at,s.created_at,s.updated_at,s.books_count
FROM series s
JOIN libraries l ON l.id=s.library_id AND l.enabled=1 AND NOT EXISTS (SELECT 1 FROM union_layers ul WHERE ul.member_library_id=l.id)
WHERE s.id IN ` + Placeholders(len(args))
		if excludeNSFW {
			query += ` AND NOT EXISTS (SELECT 1 FROM series_metadata sm_nsfw WHERE sm_nsfw.series_id=s.id AND sm_nsfw.is_nsfw=1)`
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		items, err := scanSeriesRows(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if hasScope && !scope.AllowsLibrary(item.LibraryID) {
				continue
			}
			out[item.ID] = item
		}
	}
	return out, nil
}

func (s *Store) SetSeriesOneShotOverride(ctx context.Context, seriesID string, enabled bool) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE series SET one_shot_override=?
WHERE id=? AND (NOT ? OR (one_shot=0 AND books_count=1))`, enabled, seriesID, enabled)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 0 {
		return err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM series WHERE id=?`, seriesID).Scan(new(int)); err != nil {
		return err
	}
	return ErrInvalidOneShotOverride
}

var ErrInvalidOneShotOverride = errors.New("one-shot override requires a regular series with exactly one book")

const bookNumberOrder = "b.effective_number_sort ASC,b.name COLLATE NOCASE ASC,b.id ASC"
const reverseBookNumberOrder = "b.effective_number_sort DESC,b.name COLLATE NOCASE DESC,b.id DESC"

const effectiveOneShotSQL = "(s.one_shot = 1 OR (s.one_shot_override = 1 AND s.books_count = 1))"

func oneShotPredicate(onlyOneShots bool) string {
	if onlyOneShots {
		return effectiveOneShotSQL
	}
	return "NOT " + effectiveOneShotSQL
}

const firstBookIDQuery = `SELECT b.id FROM books b WHERE b.series_id=? ORDER BY ` + bookNumberOrder + ` LIMIT 1`

func (s *Store) FirstBookInSeries(ctx context.Context, seriesID string) (Book, error) {
	var id string
	err := s.db.QueryRowContext(ctx, firstBookIDQuery, seriesID).Scan(&id)
	if err != nil {
		return Book{}, err
	}
	return s.bookByID(ctx, id, false, false)
}

func (s *Store) FirstBookInSeriesAny(ctx context.Context, seriesID string) (Book, error) {
	var id string
	err := s.db.QueryRowContext(ctx, firstBookIDQuery, seriesID).Scan(&id)
	if err != nil {
		return Book{}, err
	}
	return s.bookByID(ctx, id, true, false)
}

func (s *Store) NextBookInSeries(ctx context.Context, current Book) (Book, error) {
	return s.adjacentBookInSeries(ctx, current, ">", bookNumberOrder)
}

func (s *Store) PreviousBookInSeries(ctx context.Context, current Book) (Book, error) {
	return s.adjacentBookInSeries(ctx, current, "<", reverseBookNumberOrder)
}

func siblingBookIDQuery(op, order string) string {
	// Separate ranges let SQLite seek by name and ID when many books share a
	// number. A row-value inequality only seeks by number with this collation.
	seek := func(equal, field, current string) string {
		return `(SELECT b.id FROM books b INDEXED BY idx_books_series_eff_number
WHERE b.series_id=cur.series_id` + equal + ` AND ` + field + op + current +
			` ORDER BY ` + order + ` LIMIT 1)`
	}
	sameNumber := " AND b.effective_number_sort=cur.effective_number_sort"
	return `SELECT COALESCE(` +
		seek(sameNumber+" AND b.name COLLATE NOCASE=cur.name", "b.id", "cur.id") + "," +
		seek(sameNumber, "b.name COLLATE NOCASE", "cur.name") + "," +
		seek("", "b.effective_number_sort", "cur.effective_number_sort") + `)
FROM books cur JOIN series s ON s.id=cur.series_id
WHERE cur.id=? AND cur.series_id=? AND NOT ` + effectiveOneShotSQL
}

func (s *Store) adjacentBookInSeries(ctx context.Context, current Book, op, order string) (Book, error) {
	var id sql.NullString
	err := s.db.QueryRowContext(ctx, siblingBookIDQuery(op, order), current.ID, current.SeriesID).Scan(&id)
	if err != nil {
		return Book{}, err
	}
	if !id.Valid {
		return Book{}, sql.ErrNoRows
	}
	return s.BookByID(ctx, id.String)
}

type countsResult struct {
	libraries  int64
	series     int64
	books      int64
	comicBytes int64
}

func (s *Store) Counts(
	ctx context.Context,
) (libraries, series, books, comicBytes int64, err error) {
	res, err := shelfCached(ctx, s, "counts", false, s.queryCounts)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return res.libraries, res.series, res.books, res.comicBytes, nil
}

func (s *Store) queryCounts(ctx context.Context) (countsResult, error) {
	enabledLibs, err := s.enabledLibraryIDs(ctx)
	if err != nil {
		return countsResult{}, err
	}
	out := countsResult{libraries: int64(len(enabledLibs))}
	if len(enabledLibs) == 0 {
		return out, nil
	}
	libPlaceholders := Placeholders(len(enabledLibs))
	libArgs := make([]any, len(enabledLibs))
	for i, id := range enabledLibs {
		libArgs[i] = id
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM series WHERE library_id IN `+libPlaceholders, libArgs...).Scan(&out.series); err != nil {
		return countsResult{}, err
	}
	// Covering scan on idx_books_library_size(library_id, size) — single pass, no table lookups.
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*), coalesce(sum(size),0) FROM books WHERE library_id IN `+libPlaceholders, libArgs...).
		Scan(&out.books, &out.comicBytes); err != nil {
		return countsResult{}, err
	}
	if s.ShowNSFW(ctx) {
		return out, nil
	}
	// Subtract the small NSFW subset directly using idx_series_metadata_nsfw_series(is_nsfw, series_id).
	var nsfwSeries, nsfwBooks, nsfwBytes int64
	if err := s.db.QueryRowContext(ctx, `
SELECT count(*)
FROM series_metadata sm
JOIN series s ON s.id=sm.series_id
WHERE sm.is_nsfw=1 AND s.library_id IN `+libPlaceholders, libArgs...).Scan(&nsfwSeries); err != nil {
		return countsResult{}, err
	}
	if nsfwSeries > 0 {
		if err := s.db.QueryRowContext(ctx, `
SELECT count(*), coalesce(sum(b.size),0)
FROM books b
WHERE b.series_id IN (
  SELECT sm.series_id
  FROM series_metadata sm
  JOIN series s ON s.id=sm.series_id
  WHERE sm.is_nsfw=1 AND s.library_id IN `+libPlaceholders+`
)`, libArgs...).Scan(&nsfwBooks, &nsfwBytes); err != nil {
			return countsResult{}, err
		}
		out.series = max(int64(0), out.series-nsfwSeries)
		out.books = max(int64(0), out.books-nsfwBooks)
		out.comicBytes = max(int64(0), out.comicBytes-nsfwBytes)
	}
	return out, nil
}

func intersectLibraryIDs(requested, enabled []string) ([]string, bool) {
	if len(enabled) == 0 {
		return nil, true
	}
	if len(requested) == 0 {
		return enabled, false
	}
	reqClean := cleanStringSliceCaseSensitive(requested)
	if len(reqClean) == 0 {
		return nil, true
	}
	allowed := make(map[string]bool, len(enabled))
	for _, id := range enabled {
		allowed[id] = true
	}
	out := make([]string, 0, len(reqClean))
	for _, id := range reqClean {
		if allowed[id] {
			out = append(out, id)
		}
	}
	return out, len(out) == 0
}

func buildSeriesFilters(q SeriesQuery, enabledLibs []string, userID string, useFTS bool) (string, []any, bool) {
	var clauses []string
	var args []any
	if !q.IncludeHidden {
		libs, empty := intersectLibraryIDs(q.LibraryIDs, enabledLibs)
		if empty {
			return "", nil, true
		}
		clauses = append(clauses, "s.library_id IN "+Placeholders(len(libs)))
		for _, value := range libs {
			args = append(args, value)
		}
	} else if len(q.LibraryIDs) > 0 {
		libs := cleanStringSliceCaseSensitive(q.LibraryIDs)
		clauses = append(clauses, "s.library_id IN "+Placeholders(len(libs)))
		for _, value := range libs {
			args = append(args, value)
		}
	}

	switch strings.ToLower(strings.TrimSpace(q.NSFWMode)) {
	case "only", "true":
		clauses = append(clauses,
			"s.id IN (SELECT series_id FROM series_metadata WHERE is_nsfw=1)")
	case "all":
	default:
		clauses = append(clauses,
			"s.id NOT IN (SELECT series_id FROM series_metadata WHERE is_nsfw=1)")
	}

	if len(q.CollectionIDs) > 0 {
		cids := cleanStringSliceCaseSensitive(q.CollectionIDs)
		clauses = append(clauses,
			"s.id IN (SELECT cs.series_id FROM collection_series cs JOIN collections c ON c.id=cs.collection_id WHERE c.user_id=? AND cs.collection_id IN "+Placeholders(len(cids))+")")
		args = append(args, userID)
		for _, id := range cids {
			args = append(args, id)
		}
	}
	if searchSQL, searchArgs := buildSeriesSearchSQL(q.Search, useFTS); searchSQL != "" {
		clauses = append(clauses, searchSQL)
		args = append(args, searchArgs...)
	}
	if q.OneShot != nil {
		clauses = append(clauses, oneShotPredicate(*q.OneShot))
	}
	if len(q.Statuses) > 0 {
		var statusClauses []string
		for _, st := range cleanStringSlice(q.Statuses) {
			upper := strings.ToUpper(st)
			if upper == "ONGOING" {
				statusClauses = append(statusClauses,
					`COALESCE((SELECT sm.status FROM series_metadata sm WHERE sm.series_id=s.id),'ONGOING')='ONGOING'`)
			} else {
				statusClauses = append(statusClauses,
					`s.id IN (SELECT sm.series_id FROM series_metadata sm WHERE sm.status=?)`)
				args = append(args, upper)
			}
		}
		if len(statusClauses) > 0 {
			clauses = append(clauses, "("+strings.Join(statusClauses, " OR ")+")")
		}
	}
	if len(q.Genres) > 0 {
		if sub, subArgs := buildFoldedTaxonomySeriesSubquery("sg", "series_genres", "genre", "genres_fts", q.Genres, useFTS); sub != "" {
			clauses = append(clauses, `s.id IN (`+sub+`)`)
			args = append(args, subArgs...)
		}
	}
	if len(q.Tags) > 0 {
		if sub, subArgs := buildFoldedTaxonomySeriesSubquery("st", "series_tags", "tag", "tags_fts", q.Tags, useFTS); sub != "" {
			clauses = append(clauses, `s.id IN (`+sub+`)`)
			args = append(args, subArgs...)
		}
	}
	if len(q.Publishers) > 0 {
		if sub, subArgs := buildFoldedTaxonomySeriesSubquery("sm", "series_metadata", "publisher", "publisher_fts", q.Publishers, useFTS); sub != "" {
			clauses = append(clauses, `s.id IN (`+sub+`)`)
			args = append(args, subArgs...)
		}
	}
	if len(q.Authors) > 0 {
		if sub, subArgs := buildFoldedTaxonomySeriesSubquery("sa", "series_authors", "name", "authors_fts", q.Authors, useFTS); sub != "" {
			clauses = append(clauses, `s.id IN (`+sub+`)`)
			args = append(args, subArgs...)
		}
	}
	if len(q.Languages) > 0 {
		langs := cleanStringSlice(q.Languages)
		clauses = append(clauses,
			`s.id IN (SELECT sm.series_id FROM series_metadata sm WHERE sm.language COLLATE NOCASE IN `+Placeholders(len(langs))+`)`)
		for _, l := range langs {
			args = append(args, l)
		}
	}
	if scrapeStatus := strings.ToLower(strings.TrimSpace(q.ScrapeStatus)); scrapeStatus != "" {
		if scrapeStatus == ScrapeStatusNone || scrapeStatus == "pending" {
			clauses = append(clauses,
				`COALESCE((SELECT sm.scrape_status FROM series_metadata sm WHERE sm.series_id=s.id),'none')='none'`)
		} else {
			clauses = append(clauses,
				`s.id IN (SELECT sm.series_id FROM series_metadata sm WHERE sm.scrape_status=?)`)
			args = append(args, scrapeStatus)
		}
	}
	if conditions, condArgs := readStatusConditions(q.ReadStatus, "series", userID); len(conditions) > 0 {
		clauses = append(clauses, "("+strings.Join(conditions, " OR ")+")")
		args = append(args, condArgs...)
	}
	if len(clauses) == 0 {
		return "", args, false
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, false
}

func readStatusConditions(statuses []string, target, userID string) ([]string, []any) {
	seen := map[string]bool{}
	var conditions []string
	var args []any
	for _, raw := range statuses {
		status := strings.ToUpper(strings.TrimSpace(raw))
		if seen[status] {
			continue
		}
		seen[status] = true
		switch target {
		case "series":
			switch status {
			case "READ":
				conditions = append(conditions, "("+seriesReadCondition()+")")
				args = append(args, userID)
			case "IN_PROGRESS":
				conditions = append(conditions, "("+seriesInProgressCondition()+")")
				args = append(args, userID, userID)
			case "UNREAD":
				conditions = append(conditions, "("+seriesUnreadCondition()+")")
				args = append(args, userID)
			}
		case "book":
			switch status {
			case "READ":
				conditions = append(conditions,
					`EXISTS (SELECT 1 FROM book_read_progress p WHERE p.user_id=? AND p.book_id=b.id AND p.completed=1)`)
				args = append(args, userID)
			case "IN_PROGRESS":
				conditions = append(conditions,
					`EXISTS (SELECT 1 FROM book_read_progress p WHERE p.user_id=? AND p.book_id=b.id AND p.completed=0)`)
				args = append(args, userID)
			case "UNREAD":
				conditions = append(conditions,
					`NOT EXISTS (SELECT 1 FROM book_read_progress p WHERE p.user_id=? AND p.book_id=b.id)`)
				args = append(args, userID)
			}
		}
	}
	return conditions, args
}

func seriesReadCondition() string {
	return `s.books_count>0
AND NOT EXISTS (
 SELECT 1 FROM books not_read_book
 WHERE not_read_book.series_id=s.id
 AND NOT EXISTS (
  SELECT 1 FROM book_read_progress read_progress
  WHERE read_progress.user_id=? AND read_progress.book_id=not_read_book.id AND read_progress.completed=1
 )
)`
}

func seriesInProgressCondition() string {
	return `EXISTS (
 SELECT 1 FROM book_read_progress progressed
 WHERE progressed.user_id=? AND progressed.series_id=s.id
)
AND EXISTS (
 SELECT 1 FROM books incomplete_book
 WHERE incomplete_book.series_id=s.id
 AND NOT EXISTS (
  SELECT 1 FROM book_read_progress completed_progress
  WHERE completed_progress.user_id=? AND completed_progress.book_id=incomplete_book.id AND completed_progress.completed=1
 )
)`
}

func seriesUnreadCondition() string {
	return `NOT EXISTS (
 SELECT 1 FROM book_read_progress progressed
 WHERE progressed.user_id=? AND progressed.series_id=s.id
)`
}

func buildBookFilters(q BookQuery, enabledLibs []string, excludeNSFW bool, userID string, useSeriesFTS, useBooksFTS bool) (string, []any, bool) {
	libs, empty := intersectLibraryIDs(q.LibraryIDs, enabledLibs)
	if empty {
		return "", nil, true
	}
	clauses := []string{
		"b.library_id IN " + Placeholders(len(libs)),
	}
	args := make([]any, 0, len(libs)+4)
	for _, value := range libs {
		args = append(args, value)
	}
	if excludeNSFW {
		clauses = append(clauses,
			"b.series_id NOT IN (SELECT series_id FROM series_metadata WHERE is_nsfw=1)")
	}
	if len(q.ReadListIDs) > 0 {
		rids := cleanStringSliceCaseSensitive(q.ReadListIDs)
		clauses = append(clauses,
			"b.id IN (SELECT rb.book_id FROM readlist_books rb JOIN readlists r ON r.id=rb.readlist_id WHERE r.user_id=? AND rb.readlist_id IN "+Placeholders(len(rids))+")")
		args = append(args, userID)
		for _, id := range rids {
			args = append(args, id)
		}
	}
	if searchSQL, searchArgs := buildBookSearchSQL(q.Search, useSeriesFTS, useBooksFTS); searchSQL != "" {
		clauses = append(clauses, searchSQL)
		args = append(args, searchArgs...)
	}
	if q.SeriesID != "" {
		clauses = append(clauses, "b.series_id=?")
		args = append(args, q.SeriesID)
	}
	if q.OneShot != nil {
		clauses = append(clauses,
			"b.series_id IN (SELECT s.id FROM series s WHERE s.library_id IN "+Placeholders(len(libs))+
				" AND "+oneShotPredicate(*q.OneShot)+")")
		for _, id := range libs {
			args = append(args, id)
		}
	}
	if len(q.Tags) > 0 {
		if sub, subArgs := buildFoldedTaxonomySeriesSubquery("st", "series_tags", "tag", "tags_fts", q.Tags, useSeriesFTS); sub != "" {
			clauses = append(clauses, `b.series_id IN (`+sub+`)`)
			args = append(args, subArgs...)
		}
	}
	if len(q.Authors) > 0 {
		if sub, subArgs := buildFoldedTaxonomySeriesSubquery("sa", "series_authors", "name", "authors_fts", q.Authors, useSeriesFTS); sub != "" {
			clauses = append(clauses, `b.series_id IN (`+sub+`)`)
			args = append(args, subArgs...)
		}
	}
	if conditions, condArgs := readStatusConditions(q.ReadStatus, "book", userID); len(conditions) > 0 {
		clauses = append(clauses, "("+strings.Join(conditions, " OR ")+")")
		args = append(args, condArgs...)
	}
	if len(clauses) == 0 {
		return "", args, false
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, false
}

func scanSeriesRows(rows *sql.Rows) ([]Series, error) {
	out := make([]Series, 0)
	for rows.Next() {
		var item Series
		var modified sql.NullString
		var created, updated string
		if err := rows.Scan(
			&item.ID, &item.LibraryID, &item.CID, &item.Name, &item.RelativePath, &item.OneShot, &item.OneShotOverride,
			&modified, &created, &updated, &item.BooksCount,
		); err != nil {
			return nil, err
		}
		item.FileModifiedAt = parseNullableTime(modified)
		item.CreatedAt = parseTime(created)
		item.UpdatedAt = parseTime(updated)
		out = append(out, item)
	}
	return out, rows.Err()
}

func scanSeriesRowsWithTotal(rows *sql.Rows) ([]Series, int64, error) {
	out := make([]Series, 0)
	var total int64
	for rows.Next() {
		var item Series
		var modified sql.NullString
		var created, updated string
		if err := rows.Scan(
			&item.ID, &item.LibraryID, &item.CID, &item.Name, &item.RelativePath, &item.OneShot, &item.OneShotOverride,
			&modified, &created, &updated, &item.BooksCount, &total,
		); err != nil {
			return nil, 0, err
		}
		item.FileModifiedAt = parseNullableTime(modified)
		item.CreatedAt = parseTime(created)
		item.UpdatedAt = parseTime(updated)
		out = append(out, item)
	}
	return out, total, rows.Err()
}

func scanBookRows(rows *sql.Rows) ([]Book, error) {
	out := make([]Book, 0)
	for rows.Next() {
		var item Book
		var fileCreated, fileModified sql.NullString
		var created, updated string
		if err := rows.Scan(
			&item.ID, &item.SeriesID, &item.LibraryID, &item.FileID, &item.ParentCID,
			&item.Name, &item.Size, &item.PickCode, &item.SHA1, &fileCreated, &fileModified,
			&item.NumberSort, &created, &updated, &item.PageCount,
		); err != nil {
			return nil, err
		}
		item.FileCreatedAt = parseNullableTime(fileCreated)
		item.FileModifiedAt = parseNullableTime(fileModified)
		item.CreatedAt = parseTime(created)
		item.UpdatedAt = parseTime(updated)
		out = append(out, item)
	}
	return out, rows.Err()
}

func scanBookRowsWithTotal(rows *sql.Rows) ([]Book, int64, error) {
	out := make([]Book, 0)
	var total int64
	for rows.Next() {
		var item Book
		var fileCreated, fileModified sql.NullString
		var created, updated string
		if err := rows.Scan(
			&item.ID, &item.SeriesID, &item.LibraryID, &item.FileID, &item.ParentCID,
			&item.Name, &item.Size, &item.PickCode, &item.SHA1, &fileCreated, &fileModified,
			&item.NumberSort, &created, &updated, &item.PageCount, &total,
		); err != nil {
			return nil, 0, err
		}
		item.FileCreatedAt = parseNullableTime(fileCreated)
		item.FileModifiedAt = parseNullableTime(fileModified)
		item.CreatedAt = parseTime(created)
		item.UpdatedAt = parseTime(updated)
		out = append(out, item)
	}
	return out, total, rows.Err()
}

func cleanStringSliceCaseSensitive(items []string) []string {
	out := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, raw := range items {
		v := strings.TrimSpace(raw)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func chunkStrings(items []string, chunkSize int) [][]string {
	if len(items) == 0 {
		return nil
	}
	if chunkSize <= 0 || len(items) <= chunkSize {
		return [][]string{items}
	}
	out := make([][]string, 0, (len(items)+chunkSize-1)/chunkSize)
	for start := 0; start < len(items); start += chunkSize {
		end := min(start+chunkSize, len(items))
		out = append(out, items[start:end])
	}
	return out
}

func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

func parseNullableTime(value sql.NullString) *time.Time {
	if !value.Valid || value.String == "" {
		return nil
	}
	parsed := parseTime(value.String)
	return &parsed
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Unix(0, 0).UTC()
	}
	return parsed
}

func Placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return fmt.Sprintf("(%s)", strings.TrimSuffix(strings.Repeat("?,", count), ","))
}
