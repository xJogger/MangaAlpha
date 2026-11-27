package extmeta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/cjk"
	_ "modernc.org/sqlite"
)

const sidecarBatchSize = 5000

var thumbSHA1Re = regexp.MustCompile(`(?:^|[/=_-])([0-9a-fA-F]{40})(?:$|[._/?&=-])`)

// SearchMatch represents a fast lookup or title search hit from the sidecar index.
type SearchMatch struct {
	SourceID  string  `json:"sourceId,omitempty"`
	EntryID   string  `json:"entryId"`
	Title     string  `json:"title"`
	Category  string  `json:"category,omitempty"`
	ThumbSHA1 string  `json:"thumbSha1,omitempty"`
	CoverURL  string  `json:"coverUrl,omitempty"`
	IsNSFW    bool    `json:"isNsfw"`
	Score     float64 `json:"score"`
	MatchType string  `json:"matchType"`

	gallery *galleryMatch
}

// ExtractThumbSHA1 extracts a 40-character lowercase SHA-1 hex digest from a cover URL
// (e.g. "/4c/6a/<40hex>-..."). Returns "" if none is present.
func ExtractThumbSHA1(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if isHex40(s) {
		return strings.ToLower(s)
	}
	m := thumbSHA1Re.FindStringSubmatch(s)
	if len(m) == 2 {
		return strings.ToLower(m[1])
	}
	return ""
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')) {
			return false
		}
	}
	return true
}

// BuildSidecarIndex opens upstreamDBPath strictly read-only,
// detects or validates schema, and builds a companion FTS5 + SHA-1 sidecar SQLite database
// at sidecarPath via staged atomic replacement.
func BuildSidecarIndex(
	ctx context.Context,
	upstreamDBPath, sidecarPath, schema string,
	onProgress func(indexed int64),
) error {
	if strings.TrimSpace(sidecarPath) == "" {
		return errors.New("sidecarPath must not be empty")
	}

	upDB, err := openKomgaReadOnlySQLite(upstreamDBPath)
	if err != nil {
		return fmt.Errorf("open upstream sqlite: %w", err)
	}
	defer upDB.Close()

	resolvedSchema, err := DetectSchema(ctx, upDB, schema)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(sidecarPath), 0o700); err != nil {
		return err
	}
	stagingPath := sidecarPath + ".staging"
	_ = os.Remove(stagingPath)
	defer os.Remove(stagingPath)

	sideDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(stagingPath))
	if err != nil {
		return fmt.Errorf("create sidecar sqlite: %w", err)
	}
	defer sideDB.Close()

	if _, err := sideDB.ExecContext(ctx, `
PRAGMA journal_mode = MEMORY;
PRAGMA synchronous = OFF;
PRAGMA temp_store = MEMORY;
CREATE TABLE ext_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE ext_entries (
  entry_id TEXT PRIMARY KEY,
  norm_title TEXT NOT NULL,
  norm_alt TEXT NOT NULL,
  raw_title TEXT NOT NULL,
  thumb_sha1 TEXT NOT NULL,
  category TEXT NOT NULL,
  is_nsfw INTEGER NOT NULL
);
CREATE TABLE ext_entry_keys (
  entry_id TEXT NOT NULL,
  norm_key TEXT NOT NULL
);
CREATE VIRTUAL TABLE ext_fts USING fts5(
  entry_id UNINDEXED,
  text,
  tokenize='unicode61'
);
`); err != nil {
		return fmt.Errorf("init sidecar schema: %w", err)
	}

	var indexedCount int64
	switch resolvedSchema {
	case SchemaMangaAlphaMetaV1, SchemaMangaAlphaMetaV2:
		indexedCount, err = buildSidecarFromMangaAlphaMetaV1(ctx, upDB, sideDB, onProgress)
	case SchemaGalleryV1:
		indexedCount, err = buildSidecarFromGalleryV1(ctx, upDB, sideDB, onProgress)
	case SchemaKomgaSQLiteV1:
		indexedCount, err = buildSidecarFromKomgaSQLiteV1(ctx, upDB, sideDB, onProgress)
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedSchema, resolvedSchema)
	}
	if err != nil {
		return err
	}

	if _, err := sideDB.ExecContext(ctx, `
INSERT INTO ext_meta(key, value) VALUES('schema', ?), ('entry_count', ?);
CREATE INDEX idx_ext_entries_norm_title ON ext_entries(norm_title);
CREATE INDEX idx_ext_entries_norm_alt ON ext_entries(norm_alt);
CREATE INDEX idx_ext_entries_thumb_sha1 ON ext_entries(thumb_sha1) WHERE thumb_sha1 <> '';
CREATE INDEX idx_ext_entry_keys_norm ON ext_entry_keys(norm_key);
INSERT INTO ext_fts(ext_fts) VALUES('optimize');
PRAGMA journal_mode = DELETE;
`, resolvedSchema, strconv.FormatInt(indexedCount, 10)); err != nil {
		return fmt.Errorf("finalize sidecar indexes: %w", err)
	}

	if err := sideDB.Close(); err != nil {
		return err
	}
	if err := os.Chmod(stagingPath, 0o600); err != nil {
		return err
	}
	if err := os.Rename(stagingPath, sidecarPath); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(indexedCount)
	}
	return nil
}

type sidecarRow struct {
	entryID   string
	title     string
	titleAlt  string
	coverURL  string
	category  string
	isNSFW    int
	extraKeys []string
	ftsExtra  []string
}

func parseFlexibleAuthorNames(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		var objs []OpenAuthor
		if json.Unmarshal([]byte(raw), &objs) == nil && len(objs) > 0 {
			names := make([]string, 0, len(objs))
			for _, obj := range objs {
				if name := strings.TrimSpace(obj.Name); name != "" {
					names = append(names, name)
				}
			}
			if len(names) > 0 {
				return dedupeStrings(names)
			}
		}
	}
	return parseStringListField(raw)
}

func buildSidecarFromMangaAlphaMetaV1(
	ctx context.Context,
	upDB, sideDB *sql.DB,
	onProgress func(int64),
) (int64, error) {
	cols, err := tableColumns(ctx, upDB, "entries")
	if err != nil {
		return 0, err
	}
	var total int64
	var lastRowID int64
	hasTitleSort := cols["title_sort"]
	hasAuthors := cols["authors"]
	hasAgeRating := cols["age_rating"]
	hasSharingLabels := cols["sharing_labels_json"]

	selTitleSort := "''"
	if hasTitleSort {
		selTitleSort = "COALESCE(title_sort,'')"
	}
	selAuthors := "''"
	if hasAuthors {
		selAuthors = "COALESCE(authors,'')"
	}
	selAgeRating := "NULL"
	if hasAgeRating {
		selAgeRating = "age_rating"
	}
	selSharing := "''"
	if hasSharingLabels {
		selSharing = "COALESCE(sharing_labels_json,'')"
	}
	query := fmt.Sprintf(`
SELECT rowid, COALESCE(id,''), COALESCE(title,''), COALESCE(title_alt,''), COALESCE(cover_url,''), COALESCE(category,''), COALESCE(is_nsfw,0), %s, %s, %s, %s
FROM entries
WHERE rowid > ?
ORDER BY rowid ASC
LIMIT ?`, selTitleSort, selAuthors, selAgeRating, selSharing)

	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		rows, err := upDB.QueryContext(ctx, query, lastRowID, sidecarBatchSize)
		if err != nil {
			return total, err
		}
		batch := make([]sidecarRow, 0, sidecarBatchSize)
		for rows.Next() {
			var rid int64
			var r sidecarRow
			var titleSort, authorsRaw, sharingRaw string
			var ageRating sql.NullInt64
			if err := rows.Scan(&rid, &r.entryID, &r.title, &r.titleAlt, &r.coverURL, &r.category, &r.isNSFW, &titleSort, &authorsRaw, &ageRating, &sharingRaw); err != nil {
				rows.Close()
				return total, err
			}
			lastRowID = rid
			if strings.TrimSpace(r.entryID) == "" {
				continue
			}
			if r.isNSFW != 0 {
				r.isNSFW = 1
			}
			if ageRating.Valid && ageRating.Int64 >= 18 {
				r.isNSFW = 1
			}
			if sharingRaw != "" {
				for _, label := range parseStringListField(sharingRaw) {
					if isNSFWKeyword(label) {
						r.isNSFW = 1
						break
					}
				}
			}

			if ts := strings.TrimSpace(titleSort); ts != "" {
				r.extraKeys = append(r.extraKeys, ts)
				r.ftsExtra = append(r.ftsExtra, ts)
			}
			if authors := parseFlexibleAuthorNames(authorsRaw); len(authors) > 0 {
				r.ftsExtra = append(r.ftsExtra, authors...)
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return total, err
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}
		if err := insertSidecarBatch(ctx, sideDB, batch); err != nil {
			return total, err
		}
		total += int64(len(batch))
		if onProgress != nil {
			onProgress(total)
		}
		if len(batch) < sidecarBatchSize {
			break
		}
	}
	return total, nil
}

func komgaPathTail(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return ""
	}
	pathPart := trimmed
	if u, err := url.Parse(trimmed); err == nil && u.Path != "" {
		pathPart = u.Path
	} else if decoded, err := url.PathUnescape(trimmed); err == nil {
		pathPart = decoded
	}
	pathPart = strings.TrimRight(strings.ReplaceAll(pathPart, "\\", "/"), "/")
	if pathPart == "" {
		return ""
	}
	if idx := strings.LastIndexByte(pathPart, '/'); idx >= 0 {
		pathPart = pathPart[idx+1:]
	}
	if decoded, err := url.PathUnescape(pathPart); err == nil {
		pathPart = decoded
	}
	return strings.TrimSpace(pathPart)
}

func buildSidecarFromKomgaSQLiteV1(
	ctx context.Context,
	upDB, sideDB *sql.DB,
	onProgress func(int64),
) (int64, error) {
	info, err := DetectKomgaSchema(ctx, upDB)
	if err != nil {
		return 0, err
	}
	seriesList, err := queryKomgaSeries(ctx, upDB, info, "", false, false)
	if err != nil {
		return 0, err
	}
	var total int64
	batch := make([]sidecarRow, 0, min(len(seriesList), sidecarBatchSize))
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := insertSidecarBatch(ctx, sideDB, batch); err != nil {
			return err
		}
		total += int64(len(batch))
		batch = batch[:0]
		if onProgress != nil {
			onProgress(total)
		}
		return nil
	}

	for _, ks := range seriesList {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		primaryTitle := strings.TrimSpace(ks.Title)
		if primaryTitle == "" {
			primaryTitle = strings.TrimSpace(ks.Name)
		}
		titleAlt := ""
		if name := strings.TrimSpace(ks.Name); name != "" && !strings.EqualFold(name, primaryTitle) {
			titleAlt = name
		} else if len(ks.AlternateTitles) > 0 {
			titleAlt = strings.TrimSpace(ks.AlternateTitles[0].Title)
		}

		extraKeys := make([]string, 0, len(ks.AlternateTitles)+3)
		if name := strings.TrimSpace(ks.Name); name != "" {
			extraKeys = append(extraKeys, name)
		}
		if ts := strings.TrimSpace(ks.TitleSort); ts != "" {
			extraKeys = append(extraKeys, ts)
		}
		tail := komgaPathTail(ks.URL)
		if tail != "" {
			extraKeys = append(extraKeys, tail)
		}
		for _, at := range ks.AlternateTitles {
			if t := strings.TrimSpace(at.Title); t != "" {
				extraKeys = append(extraKeys, t)
			}
		}

		ftsExtra := append([]string(nil), extraKeys...)
		for _, a := range ks.Authors {
			if name := strings.TrimSpace(a.Name); name != "" {
				ftsExtra = append(ftsExtra, name)
			}
		}
		if pub := strings.TrimSpace(ks.Publisher); pub != "" {
			ftsExtra = append(ftsExtra, pub)
		}

		nsfwInt := 0
		if ks.NSFW {
			nsfwInt = 1
		}
		cat := "komga"
		if len(ks.Genres) > 0 {
			cat = strings.Join(ks.Genres, ", ")
		}
		batch = append(batch, sidecarRow{
			entryID:  ks.KomgaSeriesID,
			title:    primaryTitle,
			titleAlt: titleAlt,

			category:  cat,
			isNSFW:    nsfwInt,
			extraKeys: dedupeStrings(extraKeys),
			ftsExtra:  dedupeStrings(ftsExtra),
		})
		if len(batch) >= sidecarBatchSize {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := flush(); err != nil {
		return total, err
	}
	return total, nil
}

func buildSidecarFromGalleryV1(
	ctx context.Context,
	upDB, sideDB *sql.DB,
	onProgress func(int64),
) (int64, error) {
	var total int64
	var lastGID int64 = -1
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		rows, err := upDB.QueryContext(ctx, `
SELECT gid, COALESCE(title,''), COALESCE(title_jpn,''), COALESCE(thumb,''), COALESCE(category,'')
FROM gallery
WHERE gid > ?
ORDER BY gid ASC
LIMIT ?`, lastGID, sidecarBatchSize)
		if err != nil {
			return total, err
		}
		batch := make([]sidecarRow, 0, sidecarBatchSize)
		for rows.Next() {
			var gid int64
			var r sidecarRow
			if err := rows.Scan(&gid, &r.title, &r.titleAlt, &r.coverURL, &r.category); err != nil {
				rows.Close()
				return total, err
			}
			lastGID = gid
			r.entryID = strconv.FormatInt(gid, 10)
			if IsGalleryCategoryNSFW(r.category) {
				r.isNSFW = 1
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return total, err
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}
		if err := insertSidecarBatch(ctx, sideDB, batch); err != nil {
			return total, err
		}
		total += int64(len(batch))
		if onProgress != nil {
			onProgress(total)
		}
		if len(batch) < sidecarBatchSize {
			break
		}
	}
	return total, nil
}

func insertSidecarBatch(ctx context.Context, sideDB *sql.DB, batch []sidecarRow) error {
	tx, err := sideDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	entryStmt, err := tx.PrepareContext(ctx, `
INSERT INTO ext_entries(entry_id, norm_title, norm_alt, raw_title, thumb_sha1, category, is_nsfw)
VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer entryStmt.Close()

	keyStmt, err := tx.PrepareContext(ctx, `
INSERT INTO ext_entry_keys(entry_id, norm_key) VALUES(?,?)`)
	if err != nil {
		return err
	}
	defer keyStmt.Close()

	ftsStmt, err := tx.PrepareContext(ctx, `
INSERT INTO ext_fts(entry_id, text) VALUES(?, ?)`)
	if err != nil {
		return err
	}
	defer ftsStmt.Close()

	for _, r := range batch {
		rawTitle := strings.TrimSpace(r.title)
		if rawTitle == "" {
			rawTitle = strings.TrimSpace(r.titleAlt)
		}
		normTitle := cjk.ExactTitleKey(r.title)
		normAlt := cjk.ExactTitleKey(r.titleAlt)
		thumbSHA1 := ExtractThumbSHA1(r.coverURL)

		if _, err := entryStmt.ExecContext(
			ctx,
			r.entryID,
			normTitle,
			normAlt,
			rawTitle,
			thumbSHA1,
			strings.TrimSpace(r.category),
			r.isNSFW,
		); err != nil {
			return err
		}

		if len(r.extraKeys) > 0 {
			seenKeys := make(map[string]struct{}, len(r.extraKeys)+2)
			if normTitle != "" {
				seenKeys[normTitle] = struct{}{}
			}
			if normAlt != "" {
				seenKeys[normAlt] = struct{}{}
			}
			for _, rawKey := range r.extraKeys {
				nk := cjk.ExactTitleKey(rawKey)
				if nk == "" {
					continue
				}
				if _, dup := seenKeys[nk]; dup {
					continue
				}
				seenKeys[nk] = struct{}{}
				if _, err := keyStmt.ExecContext(ctx, r.entryID, nk); err != nil {
					return err
				}
			}
		}

		ftsParts := make([]string, 0, 2+len(r.ftsExtra))
		ftsParts = append(ftsParts, r.title, r.titleAlt)
		ftsParts = append(ftsParts, r.ftsExtra...)
		ftsText := cjk.FTSJoinValues(ftsParts)
		if ftsText != "" {
			if _, err := ftsStmt.ExecContext(ctx, r.entryID, ftsText); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// SidecarStore provides sub-5ms title search and SHA-1 cover lookup via a companion
// SQLite FTS5 index, paired with read-only primary-key lookups against the upstream database.
type SidecarStore struct {
	sourceID         string
	schema           string
	sidecarDB        *sql.DB
	upstreamDB       *sql.DB
	hasKeysTable     bool
	checkpointedOnly bool
}

// OpenSidecarStore opens the sidecar index and (optionally) the upstream database in strict read-only mode.
func OpenSidecarStore(sourceID, upstreamDBPath, sidecarPath, schema string) (*SidecarStore, error) {
	sideDB, err := openReadOnlySQLite(sidecarPath)
	if err != nil {
		return nil, fmt.Errorf("open sidecar sqlite: %w", err)
	}

	var storedSchema string
	_ = sideDB.QueryRow(`SELECT value FROM ext_meta WHERE key='schema'`).Scan(&storedSchema)

	var upDB *sql.DB
	var checkpointedOnly bool
	if strings.TrimSpace(upstreamDBPath) != "" {
		upDB, checkpointedOnly, err = openKomgaSQLiteSnapshot(upstreamDBPath)
		if err != nil {
			sideDB.Close()
			return nil, fmt.Errorf("open upstream sqlite: %w", err)
		}
		targetSchema := strings.TrimSpace(schema)
		if targetSchema == "" {
			targetSchema = storedSchema
		}
		resolved, detErr := DetectSchema(context.Background(), upDB, targetSchema)
		if detErr != nil {
			upDB.Close()
			sideDB.Close()
			return nil, detErr
		}
		storedSchema = resolved
	} else if strings.TrimSpace(schema) != "" {
		storedSchema = strings.TrimSpace(schema)
	}

	tables, _ := listSQLiteTables(context.Background(), sideDB)

	return &SidecarStore{
		sourceID:     strings.TrimSpace(sourceID),
		schema:       storedSchema,
		sidecarDB:    sideDB,
		upstreamDB:   upDB,
		hasKeysTable: tables["ext_entry_keys"], checkpointedOnly: checkpointedOnly,
	}, nil
}

// Schema returns the resolved upstream database schema name.
func (s *SidecarStore) Schema() string {
	return s.schema
}

// Close closes both the sidecar and upstream read-only SQLite handles.
func (s *SidecarStore) Close() error {
	var errs []error
	if s.sidecarDB != nil {
		if err := s.sidecarDB.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.upstreamDB != nil {
		if err := s.upstreamDB.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// LookupByThumbSHA1 performs an indexed lookup by 40-hex SHA-1 cover hash (or URL containing it).
func (s *SidecarStore) LookupByThumbSHA1(ctx context.Context, sha1Hex string) ([]SearchMatch, error) {
	if s == nil || s.sidecarDB == nil {
		return nil, errors.New("sidecar store is not open")
	}
	digest := ExtractThumbSHA1(sha1Hex)
	if digest == "" {
		return nil, nil
	}

	rows, err := s.sidecarDB.QueryContext(ctx, `
SELECT entry_id, raw_title, thumb_sha1, category, is_nsfw
FROM ext_entries WHERE thumb_sha1 = ? ORDER BY entry_id ASC`, digest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SearchMatch
	for rows.Next() {
		var m SearchMatch
		var nsfw int
		if err := rows.Scan(&m.EntryID, &m.Title, &m.ThumbSHA1, &m.Category, &nsfw); err != nil {
			return nil, err
		}
		m.SourceID = s.sourceID
		m.IsNSFW = nsfw != 0
		m.Score = 1.0
		m.MatchType = "thumb_sha1"
		out = append(out, m)
	}
	return out, rows.Err()
}

// SearchByTitle finds entries matching query via normalized exact lookup + FTS5 recall + similarity scoring.
func (s *SidecarStore) SearchByTitle(ctx context.Context, query string, limit int) ([]SearchMatch, error) {
	return s.SearchWithHints(ctx, []string{query}, nil, limit)
}

// SearchWithHints ranks matches across title variants and uses author hints for gallery entries.
func (s *SidecarStore) SearchWithHints(ctx context.Context, titles, authors []string, limit int) ([]SearchMatch, error) {
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 200)
	hits, err := s.searchWithHints(ctx, titles, authors, limit)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, err
}

func (s *SidecarStore) searchWithHints(ctx context.Context, titles, authors []string, limit int) ([]SearchMatch, error) {
	if s == nil || s.sidecarDB == nil {
		return nil, errors.New("sidecar store is not open")
	}
	gallery := s.schema == SchemaGalleryV1 && s.upstreamDB != nil
	queries := make(map[string]string)
	addQuery := func(title string) {
		key := cjk.ExactTitleKey(title)
		if key != "" {
			if _, exists := queries[key]; !exists {
				queries[key] = title
			}
		}
	}
	authorKeys := titleKeys(authors)
	for _, title := range titles {
		addQuery(title)
		if gallery && strings.ContainsAny(title, "[【(（") {
			core, hints := parseGalleryTitle(title)
			addQuery(core)
			authorKeys = append(authorKeys, hints...)
		}
	}
	candidates := make(map[string]SearchMatch)
	for _, query := range queries {
		if err := s.recallTitle(ctx, query, limit, gallery, candidates); err != nil {
			return nil, err
		}
	}
	if gallery && len(candidates) > 0 {
		if err := s.scoreGalleryMatches(ctx, candidates, queries, authorKeys); err != nil {
			return nil, err
		}
	}
	hits := make([]SearchMatch, 0, len(candidates))
	for _, hit := range candidates {
		if gallery && hit.gallery == nil {
			continue
		}
		hits = append(hits, hit)
	}
	slices.SortFunc(hits, func(a, b SearchMatch) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		if a.gallery != nil && b.gallery != nil {
			return compareGalleryVersions(a.gallery, b.gallery)
		}
		return strings.Compare(a.EntryID, b.EntryID)
	})
	return hits, nil
}

func (s *SidecarStore) recallTitle(ctx context.Context, query string, limit int, gallery bool, candidates map[string]SearchMatch) error {
	normQ := cjk.ExactTitleKey(query)
	ftsPhrase := cjk.FTSQueryPhrase(query)
	recallBudget := max(limit*4, 40)
	exactLimit := limit * 2
	if gallery {
		recallBudget = max(limit*6, 60)
		exactLimit = recallBudget
	}

	recalled := make(map[string]struct{})
	addCandidate := func(entryID, rawTitle, thumbSHA1, category string, isNSFW int, normTitle, normAlt, matchType string, exactKey bool) {
		recalled[entryID] = struct{}{}
		score := scoreTitleMatch(normQ, normTitle, normAlt)
		if exactKey && score < 0.97 {
			score = 0.97
		}
		if score <= 0 && matchType == "fts" {
			score = 0.25
		}
		if prev, ok := candidates[entryID]; ok && (score < prev.Score ||
			(score == prev.Score && matchType >= prev.MatchType)) {
			return
		}
		candidates[entryID] = SearchMatch{
			SourceID:  s.sourceID,
			EntryID:   entryID,
			Title:     rawTitle,
			Category:  category,
			ThumbSHA1: thumbSHA1,
			IsNSFW:    isNSFW != 0,
			Score:     score,
			MatchType: matchType,
		}
	}

	// 1. Exact normalized title / alternate-title / extra-alias lookup.
	if normQ != "" {
		exactRows, err := s.sidecarDB.QueryContext(ctx, `
SELECT entry_id, raw_title, thumb_sha1, category, is_nsfw, norm_title, norm_alt
FROM ext_entries
WHERE norm_title = ? OR norm_alt = ?
LIMIT ?`, normQ, normQ, exactLimit)
		if err != nil {
			return err
		}
		for exactRows.Next() {
			var eid, rawTitle, sha1, cat, nt, na string
			var nsfw int
			if err := exactRows.Scan(&eid, &rawTitle, &sha1, &cat, &nsfw, &nt, &na); err != nil {
				exactRows.Close()
				return err
			}
			addCandidate(eid, rawTitle, sha1, cat, nsfw, nt, na, "exact", false)
		}
		if err := exactRows.Err(); err != nil {
			exactRows.Close()
			return err
		}
		exactRows.Close()

		if s.hasKeysTable {
			keyRows, err := s.sidecarDB.QueryContext(ctx, `
SELECT DISTINCT e.entry_id, e.raw_title, e.thumb_sha1, e.category, e.is_nsfw, e.norm_title, e.norm_alt
FROM ext_entry_keys k
JOIN ext_entries e ON e.entry_id = k.entry_id
WHERE k.norm_key = ?
LIMIT ?`, normQ, exactLimit)
			if err != nil {
				return err
			}
			for keyRows.Next() {
				var eid, rawTitle, sha1, cat, nt, na string
				var nsfw int
				if err := keyRows.Scan(&eid, &rawTitle, &sha1, &cat, &nsfw, &nt, &na); err != nil {
					keyRows.Close()
					return err
				}
				addCandidate(eid, rawTitle, sha1, cat, nsfw, nt, na, "exact", true)
			}
			if err := keyRows.Err(); err != nil {
				keyRows.Close()
				return err
			}
			keyRows.Close()
		}
	}

	// 2. FTS5 recall (adjacent phrase first, then multi-word AND fallback if still under budget).
	runFTS := func(matchExpr string) error {
		if matchExpr == "" {
			return nil
		}
		ftsRows, err := s.sidecarDB.QueryContext(ctx, `
SELECT e.entry_id, e.raw_title, e.thumb_sha1, e.category, e.is_nsfw, e.norm_title, e.norm_alt
FROM ext_fts f
JOIN ext_entries e ON e.entry_id = f.entry_id
WHERE ext_fts MATCH ?
ORDER BY bm25(ext_fts)
LIMIT ?`, matchExpr, recallBudget)
		if err != nil {
			return err
		}
		for ftsRows.Next() {
			var eid, rawTitle, sha1, cat, nt, na string
			var nsfw int
			if err := ftsRows.Scan(&eid, &rawTitle, &sha1, &cat, &nsfw, &nt, &na); err != nil {
				ftsRows.Close()
				return err
			}
			addCandidate(eid, rawTitle, sha1, cat, nsfw, nt, na, "fts", false)
		}
		err = ftsRows.Err()
		ftsRows.Close()
		return err
	}

	if (gallery || len(recalled) < limit) && ftsPhrase != "" {
		if err := runFTS(ftsPhrase); err != nil {
			return err
		}
	}
	andThreshold := limit
	if gallery {
		andThreshold = recallBudget
	}
	if len(recalled) < andThreshold {
		if andExpr := buildMultiTermFTSQuery(query); andExpr != "" && andExpr != ftsPhrase {
			if err := runFTS(andExpr); err != nil {
				return err
			}
		}
	}

	return nil
}

type galleryMatch struct {
	gid, rootGID   int64
	cores, authors []string
	active         bool
	authorMatched  bool
	authorConflict bool
	rating         float64
	hasCover       bool
}

var (
	gallerySuffix = regexp.MustCompile(`(?:\s*(?:\[[^\[\]]*\]|【[^【】]*】))+\s*$`)
	galleryCredit = regexp.MustCompile(`\[[^\[\]]*?[(（]([^()（）]+)[)）][^\[\]]*\]|【[^【】]*?[(（]([^()（）]+)[)）][^【】]*】`)
)

func parseGalleryTitle(raw string) (core string, authors []string) {
	parsed := bangumi.ParseSeriesName(raw)
	if len(parsed.Candidates) == 0 {
		return "", nil
	}
	core = parsed.Candidates[0]
	// Trailing release/language labels can look like author brackets to the folder parser.
	if trimmed := gallerySuffix.ReplaceAllString(raw, ""); trimmed != raw {
		if prefix := bangumi.ParseSeriesName(trimmed); len(prefix.Candidates) > 0 &&
			cjk.ExactTitleKey(prefix.Candidates[0]) == cjk.ExactTitleKey(core) {
			raw, parsed = trimmed, prefix
		}
	}
	// In a circle (artist) credit, the artist identifies the work across releases.
	credited := galleryCredit.ReplaceAllString(raw, "[$1$2]")
	if credited != raw {
		parsed = bangumi.ParseSeriesName(credited)
	}
	return core, titleKeys(parsed.Authors)
}

func titleKeys(values []string) []string {
	var keys []string
	for _, value := range values {
		if key := cjk.ExactTitleKey(value); key != "" && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

func sharesKey(a, b []string) bool {
	for _, key := range a {
		if slices.Contains(b, key) {
			return true
		}
	}
	return false
}

func (s *SidecarStore) scoreGalleryMatches(ctx context.Context, hits map[string]SearchMatch, queries map[string]string, authorKeys []string) error {
	ids := make([]int64, 0, len(hits))
	for id := range hits {
		if gid, err := strconv.ParseInt(id, 10, 64); err == nil {
			ids = append(ids, gid)
		}
	}
	// Bind the candidate IDs in one parameter to stay within SQLite's variable limit.
	batch, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	rows, err := s.upstreamDB.QueryContext(ctx, `
SELECT gid, COALESCE(title,''), COALESCE(title_jpn,''), COALESCE(expunged,0),
       COALESCE(removed,0), COALESCE(replaced,0), COALESCE(root_gid,0), COALESCE(rating,0),
       trim(COALESCE(thumb,'')) <> ''
FROM gallery WHERE gid IN (SELECT value FROM json_each(?))`, string(batch))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var g galleryMatch
		var title, titleJPN string
		var expunged, removed, replaced int
		if err := rows.Scan(&g.gid, &title, &titleJPN, &expunged, &removed, &replaced, &g.rootGID, &g.rating, &g.hasCover); err != nil {
			return err
		}
		g.active = expunged == 0 && removed == 0 && replaced == 0
		core, authors := parseGalleryTitle(title)
		altCore, altAuthors := parseGalleryTitle(titleJPN)
		g.cores = titleKeys([]string{core, altCore})
		g.authors = append(authors, altAuthors...)
		g.authorMatched = sharesKey(authorKeys, g.authors)
		g.authorConflict = len(authorKeys) > 0 && len(g.authors) > 0 && !g.authorMatched

		id := strconv.FormatInt(g.gid, 10)
		hit := hits[id]
		if g.authorConflict && hit.Score > 0.90 && hit.MatchType != "exact" {
			hit.Score = 0.90
		}
		rawTitle, rawAlt := cjk.ExactTitleKey(title), cjk.ExactTitleKey(titleJPN)
		for query := range queries {
			if score := scoreTitleMatch(query, rawTitle, rawAlt); score > hit.Score {
				if g.authorConflict && query != rawTitle && query != rawAlt && score > 0.90 {
					score = 0.90
				}
				if score > hit.Score {
					hit.Score, hit.MatchType = score, "fts"
				}
			}
			for _, key := range g.cores {
				score := scoreNormalizedPair(query, key)
				if query == key {
					switch {
					case g.authorMatched:
						score = 0.99
					case g.authorConflict:
						score = 0.90
					default:
						score = 0.96
					}
				} else if g.authorConflict && score > 0.90 {
					score = 0.90
				}
				if score > hit.Score {
					hit.Score, hit.MatchType = score, "core"
				}
			}
		}
		if !g.active {
			hit.Score = max(0, hit.Score-0.01)
		}
		hit.gallery = &g
		hits[id] = hit
	}
	return rows.Err()
}

func compareGalleryVersions(a, b *galleryMatch) int {
	switch {
	case a.active != b.active:
		if a.active {
			return -1
		}
		return 1
	case a.authorMatched != b.authorMatched:
		if a.authorMatched {
			return -1
		}
		return 1
	case a.rating > b.rating:
		return -1
	case a.rating < b.rating:
		return 1
	case a.gid > b.gid:
		return -1
	case a.gid < b.gid:
		return 1
	default:
		return 0
	}
}

// GetEntry loads the complete metadata record for entryID from the read-only upstream SQLite DB.
func (s *SidecarStore) GetEntry(ctx context.Context, entryID string) (*Entry, error) {
	if s == nil || s.upstreamDB == nil {
		return nil, errors.New("upstream database is not configured")
	}
	if s.schema == SchemaKomgaSQLiteV1 {
		info, err := DetectKomgaSchema(ctx, s.upstreamDB)
		if err != nil {
			return nil, err
		}
		ks, err := readKomgaSeries(ctx, s.upstreamDB, info, strings.TrimSpace(entryID), false, false)
		if err != nil {
			return nil, err
		}
		return komgaSeriesToEntry(s.sourceID, ks), nil
	}
	return getEntryFromUpstream(ctx, s.upstreamDB, s.sourceID, s.schema, entryID)
}

func komgaSeriesToEntry(sourceID string, ks *KomgaSeriesEntry) *Entry {
	if ks == nil {
		return nil
	}
	title := strings.TrimSpace(ks.Title)
	if title == "" {
		title = strings.TrimSpace(ks.Name)
	}
	var titleAlt string
	if name := strings.TrimSpace(ks.Name); name != "" && !strings.EqualFold(name, title) {
		titleAlt = name
	} else if len(ks.AlternateTitles) > 0 {
		titleAlt = strings.TrimSpace(ks.AlternateTitles[0].Title)
	}
	authors := make([]string, 0, len(ks.Authors))
	for _, a := range ks.Authors {
		if name := strings.TrimSpace(a.Name); name != "" {
			authors = append(authors, name)
		}
	}
	authors = dedupeStrings(authors)
	links := make([]OpenLink, 0, len(ks.Links))
	for _, l := range ks.Links {
		if strings.TrimSpace(l.URL) != "" {
			links = append(links, OpenLink{Label: l.Label, URL: l.URL})
		}
	}

	cat := ""
	if len(ks.Genres) > 0 {
		cat = strings.Join(ks.Genres, ", ")
	}
	rawFields := map[string]any{
		"komgaSeriesId": ks.KomgaSeriesID,
		"oneShot":       ks.OneShot,
		"defaultTitle":  ks.DefaultTitle,
		"defaultStatus": ks.DefaultStatus,
	}
	if ks.Name != "" {
		rawFields["name"] = ks.Name
	}
	if ks.URL != "" {
		rawFields["url"] = ks.URL
	}
	if len(ks.Genres) > 0 {
		rawFields["genres"] = ks.Genres
	}
	if len(ks.AlternateTitles) > 0 {
		rawFields["alternateTitles"] = ks.AlternateTitles
	}
	if len(ks.Authors) > 0 {
		rawFields["detailedAuthors"] = ks.Authors
	}
	return &Entry{
		SourceID:         sourceID,
		EntryID:          ks.KomgaSeriesID,
		Title:            title,
		TitleAlt:         titleAlt,
		Authors:          authors,
		Category:         cat,
		Tags:             dedupeStrings(ks.Tags),
		Summary:          strings.TrimSpace(ks.Summary),
		IsNSFW:           ks.NSFW,
		Status:           strings.TrimSpace(ks.Status),
		TitleSort:        strings.TrimSpace(ks.TitleSort),
		Publisher:        strings.TrimSpace(ks.Publisher),
		Language:         strings.TrimSpace(ks.Language),
		ReleaseDate:      strings.TrimSpace(ks.ReleaseDate),
		TotalBookCount:   ks.TotalBookCount,
		AgeRating:        ks.AgeRating,
		ReadingDirection: strings.TrimSpace(ks.ReadingDirection),
		Links:            links,
		SharingLabels:    dedupeStrings(ks.SharingLabels),
		HasCoverBlob:     ks.SelectedThumbnailID != "",
		RawFields:        rawFields,
	}
}

func buildMultiTermFTSQuery(query string) string {
	fields := strings.Fields(query)
	if len(fields) <= 1 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		if p := cjk.FTSQueryPhrase(f); p != "" {
			parts = append(parts, "("+p+")")
		}
	}
	if len(parts) <= 1 {
		return ""
	}
	return strings.Join(parts, " AND ")
}

func scoreTitleMatch(normQuery, normTitle, normAlt string) float64 {
	if normQuery == "" {
		return 0
	}
	if normTitle == normQuery {
		return 1.0
	}
	if normAlt != "" && normAlt == normQuery {
		return 0.97
	}
	best := scoreNormalizedPair(normQuery, normTitle)
	if normAlt != "" {
		if altScore := scoreNormalizedPair(normQuery, normAlt) * 0.96; altScore > best {
			best = altScore
		}
	}
	return best
}

func scoreNormalizedPair(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1.0
	}
	lenA := utf8.RuneCountInString(a)
	lenB := utf8.RuneCountInString(b)
	ratio := float64(min(lenA, lenB)) / float64(max(lenA, lenB))
	if strings.Contains(b, a) || strings.Contains(a, b) {
		return 0.65 + 0.30*ratio
	}
	dice := runeBigramDice(a, b)
	if dice == 0 {
		return 0
	}
	return 0.25 + 0.65*dice
}

func runeBigramDice(a, b string) float64 {
	ra := []rune(a)
	rb := []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	if len(ra) == 1 || len(rb) == 1 {
		set := make(map[rune]struct{}, len(ra))
		for _, r := range ra {
			set[r] = struct{}{}
		}
		overlap := 0
		for _, r := range rb {
			if _, ok := set[r]; ok {
				overlap++
			}
		}
		return float64(2*overlap) / float64(len(ra)+len(rb))
	}

	bgA := make(map[string]int, len(ra)-1)
	for i := 0; i < len(ra)-1; i++ {
		bgA[string(ra[i:i+2])]++
	}
	var intersection int
	for i := 0; i < len(rb)-1; i++ {
		bg := string(rb[i : i+2])
		if cnt := bgA[bg]; cnt > 0 {
			bgA[bg] = cnt - 1
			intersection++
		}
	}
	return float64(2*intersection) / float64((len(ra)-1)+(len(rb)-1))
}
