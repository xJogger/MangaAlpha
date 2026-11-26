package extmeta

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	// SchemaAuto automatically inspects sqlite_master to detect the upstream schema.
	SchemaAuto = ""
	// SchemaMangaAlphaMetaV1 identifies the single-table MangaAlpha open metadata schema.
	SchemaMangaAlphaMetaV1 = "mangaalpha_meta_v1"
	// SchemaMangaAlphaMetaV2 is the backwards-compatible alias for extended mangaalpha_meta_v1.
	SchemaMangaAlphaMetaV2 = "mangaalpha_meta_v2"
	// SchemaGalleryV1 identifies the normalized 4-table gallery metadata schema.
	SchemaGalleryV1 = "gallery_v1"
	// SchemaKomgaSQLiteV1 identifies a read-only Komga database.sqlite metadata schema.
	SchemaKomgaSQLiteV1 = "komga_sqlite_v1"
	// SchemaTagTranslationTextV1 identifies tag_translation_text_v1 JSON/gzipped dictionaries.
	SchemaTagTranslationTextV1 = "tag_translation_text_v1"
)

var (
	// ErrUnsupportedSchema is returned when an upstream SQLite database does not match a known schema.
	ErrUnsupportedSchema = errors.New("unsupported upstream metadata schema")
	// ErrEntryNotFound is returned when an entry ID does not exist in the upstream database.
	ErrEntryNotFound = errors.New("metadata entry not found")
)

// OpenLink represents an external link in mangaalpha_meta_v1 / v2 records.
type OpenLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// OpenAuthor represents a role-qualified author in mangaalpha_meta_v1 / v2 book records.
type OpenAuthor struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// OpenBookEntry represents a single volume/book record in the optional open-format books table.
type OpenBookEntry struct {
	SeriesEntryID string       `json:"seriesEntryId"`
	BookKey       string       `json:"bookKey"`
	FileName      string       `json:"fileName,omitempty"`
	Number        string       `json:"number,omitempty"`
	NumberSort    *float64     `json:"numberSort,omitempty"`
	Title         string       `json:"title,omitempty"`
	Summary       string       `json:"summary,omitempty"`
	ReleaseDate   string       `json:"releaseDate,omitempty"`
	Authors       []OpenAuthor `json:"authors,omitempty"`
	Tags          []string     `json:"tags,omitempty"`
	ISBN          string       `json:"isbn,omitempty"`
	Links         []OpenLink   `json:"links,omitempty"`
	CoverURL      string       `json:"coverUrl,omitempty"`
	HasCoverBlob  bool         `json:"hasCoverBlob,omitempty"`
}

// SQLiteProbeResult describes the detected format and capabilities of a local offline metadata file.
type SQLiteProbeResult struct {
	Format          string `json:"format"`
	EntryCount      int64  `json:"entryCount,omitempty"`
	BookCount       int64  `json:"bookCount,omitempty"`
	HasBooksTable   bool   `json:"hasBooksTable,omitempty"`
	HasExtendedCols bool   `json:"hasExtendedCols,omitempty"`
	HasCoverBlobs   bool   `json:"hasCoverBlobs,omitempty"`
	WALWarning      bool   `json:"walWarning,omitempty"`
	WALSizeBytes    int64  `json:"walSizeBytes,omitempty"`
	TagCount        int    `json:"tagCount,omitempty"`
}

// Entry is the unified metadata record returned from any supported offline SQLite schema.
type Entry struct {
	SourceID         string         `json:"sourceId,omitempty"`
	EntryID          string         `json:"entryId"`
	Token            string         `json:"token,omitempty"`
	Title            string         `json:"title"`
	TitleAlt         string         `json:"titleAlt,omitempty"`
	Authors          []string       `json:"authors,omitempty"`
	Category         string         `json:"category,omitempty"`
	Tags             []string       `json:"tags,omitempty"`
	Summary          string         `json:"summary,omitempty"`
	CoverURL         string         `json:"coverUrl,omitempty"`
	Rating           float64        `json:"rating,omitempty"`
	PostedAt         int64          `json:"postedAt,omitempty"`
	IsNSFW           bool           `json:"isNsfw"`
	SourceURL        string         `json:"sourceUrl,omitempty"`
	Status           string         `json:"status,omitempty"`
	TitleSort        string         `json:"titleSort,omitempty"`
	Publisher        string         `json:"publisher,omitempty"`
	Language         string         `json:"language,omitempty"`
	ReleaseDate      string         `json:"releaseDate,omitempty"`
	TotalBookCount   *int           `json:"totalBookCount,omitempty"`
	AgeRating        *int           `json:"ageRating,omitempty"`
	ReadingDirection string         `json:"readingDirection,omitempty"`
	Links            []OpenLink     `json:"links,omitempty"`
	SharingLabels    []string       `json:"sharingLabels,omitempty"`
	HasCoverBlob     bool           `json:"hasCoverBlob,omitempty"`
	RawFields        map[string]any `json:"rawFields,omitempty"`
}

func openReadOnlySQLite(path string) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite path must not be empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uriPath := filepath.ToSlash(abs)
	uriPath = strings.ReplaceAll(uriPath, "?", "%3F")
	uriPath = strings.ReplaceAll(uriPath, "#", "%23")
	dsn := "file:" + uriPath + "?mode=ro&immutable=1&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// DetectSchema inspects tables in an already-open upstream SQLite database and returns
// SchemaMangaAlphaMetaV1 (including mangaalpha_meta_v2 alias), SchemaGalleryV1, or SchemaKomgaSQLiteV1.
func DetectSchema(ctx context.Context, db *sql.DB, requested string) (string, error) {
	req := strings.ToLower(strings.TrimSpace(requested))
	if req == "auto" {
		req = ""
	}

	tables, err := listSQLiteTables(ctx, db)
	if err != nil {
		return "", err
	}

	switch req {
	case SchemaMangaAlphaMetaV1, SchemaMangaAlphaMetaV2:
		if !tables["entries"] {
			return "", fmt.Errorf("%w: missing entries table for %s", ErrUnsupportedSchema, req)
		}
		return SchemaMangaAlphaMetaV1, nil
	case SchemaGalleryV1:
		if !tables["gallery"] {
			return "", fmt.Errorf("%w: missing gallery table for %s", ErrUnsupportedSchema, SchemaGalleryV1)
		}
		return SchemaGalleryV1, nil
	case SchemaKomgaSQLiteV1:
		if !hasKomgaRequiredTables(tables) {
			return "", fmt.Errorf("%w: missing Komga tables for %s", ErrUnsupportedSchema, SchemaKomgaSQLiteV1)
		}
		return SchemaKomgaSQLiteV1, nil
	case "":
		if tables["entries"] {
			return SchemaMangaAlphaMetaV1, nil
		}
		if tables["gallery"] {
			return SchemaGalleryV1, nil
		}
		if hasKomgaRequiredTables(tables) {
			return SchemaKomgaSQLiteV1, nil
		}
		return "", fmt.Errorf("%w: no recognized entries, gallery, or Komga tables found", ErrUnsupportedSchema)
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedSchema, requested)
	}
}

// ProbeSQLiteFile inspects a local file in strict read-only mode and auto-detects
// komga_sqlite_v1, mangaalpha_meta_v1 (and mangaalpha_meta_v2 alias), gallery_v1, or tag_translation_text_v1.
func ProbeSQLiteFile(ctx context.Context, filePath string) (SQLiteProbeResult, error) {
	trimmed := strings.TrimSpace(filePath)
	if trimmed == "" {
		return SQLiteProbeResult{}, errors.New("file path must not be empty")
	}
	info, err := os.Stat(trimmed)
	if err != nil {
		return SQLiteProbeResult{}, err
	}
	if !info.Mode().IsRegular() {
		return SQLiteProbeResult{}, fmt.Errorf("%w: not a regular file", ErrUnsupportedSchema)
	}

	f, err := os.Open(trimmed)
	if err != nil {
		return SQLiteProbeResult{}, err
	}
	header := make([]byte, 16)
	n, readErr := io.ReadFull(f, header)
	_ = f.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return SQLiteProbeResult{}, readErr
	}
	header = header[:n]

	const sqliteMagic = "SQLite format 3\x00"
	if string(header) != sqliteMagic {
		return probeTagDictionaryFile(trimmed)
	}

	walWarn, walSize := DetectSQLiteWALWarning(trimmed)
	db, err := openKomgaReadOnlySQLite(trimmed)
	if err != nil {
		return SQLiteProbeResult{}, err
	}
	defer db.Close()

	tables, err := listSQLiteTables(ctx, db)
	if err != nil {
		return SQLiteProbeResult{}, err
	}

	res := SQLiteProbeResult{
		WALWarning:   walWarn,
		WALSizeBytes: walSize,
	}

	switch {
	case hasKomgaRequiredTables(tables):
		res.Format = FormatKomgaSQLite
		res.HasBooksTable = true
		_ = db.QueryRowContext(ctx, `SELECT count(*) FROM SERIES`).Scan(&res.EntryCount)
		_ = db.QueryRowContext(ctx, `SELECT count(*) FROM BOOK`).Scan(&res.BookCount)
		res.HasCoverBlobs = tables["thumbnail_series"] || tables["thumbnail_book"]
		return res, nil

	case tables["entries"]:
		res.Format = SchemaMangaAlphaMetaV1
		_ = db.QueryRowContext(ctx, `SELECT count(*) FROM entries`).Scan(&res.EntryCount)
		entryCols, err := tableColumns(ctx, db, "entries")
		if err == nil {
			for _, extCol := range []string{
				"status", "title_sort", "publisher", "language", "release_date",
				"total_book_count", "age_rating", "reading_direction",
				"links_json", "sharing_labels_json", "cover_blob",
			} {
				if entryCols[extCol] {
					res.HasExtendedCols = true
					break
				}
			}
			if entryCols["cover_blob"] {
				res.HasCoverBlobs = true
			}
		}
		if tables["books"] {
			res.HasBooksTable = true
			_ = db.QueryRowContext(ctx, `SELECT count(*) FROM books`).Scan(&res.BookCount)
			if bookCols, bErr := tableColumns(ctx, db, "books"); bErr == nil && bookCols["cover_blob"] {
				res.HasCoverBlobs = true
			}
		}
		for _, metaTable := range []string{"meta", "ext_meta"} {
			if tables[metaTable] {
				var declared string
				if db.QueryRowContext(ctx, fmt.Sprintf(`SELECT value FROM %s WHERE key='schema' LIMIT 1`, metaTable)).Scan(&declared) == nil {
					if strings.EqualFold(strings.TrimSpace(declared), SchemaMangaAlphaMetaV2) {
						res.Format = SchemaMangaAlphaMetaV2
						break
					}
				}
			}
		}
		return res, nil

	case tables["gallery"]:
		res.Format = SchemaGalleryV1
		_ = db.QueryRowContext(ctx, `SELECT count(*) FROM gallery`).Scan(&res.EntryCount)
		return res, nil

	default:
		return SQLiteProbeResult{}, fmt.Errorf("%w: unrecognized SQLite tables", ErrUnsupportedSchema)
	}
}

func probeTagDictionaryFile(path string) (SQLiteProbeResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return SQLiteProbeResult{}, err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 64<<10)
	magic, err := br.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return SQLiteProbeResult{}, fmt.Errorf("%w: %v", ErrUnsupportedSchema, err)
	}
	var src io.Reader = br
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, gErr := gzip.NewReader(br)
		if gErr != nil {
			return SQLiteProbeResult{}, fmt.Errorf("%w: %v", ErrUnsupportedSchema, gErr)
		}
		defer gz.Close()
		src = gz
	}
	payload, err := io.ReadAll(io.LimitReader(src, maxTagDictBytes+1))
	if err != nil || int64(len(payload)) > maxTagDictBytes || len(bytes.TrimSpace(payload)) == 0 {
		return SQLiteProbeResult{}, ErrUnsupportedSchema
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return SQLiteProbeResult{}, ErrUnsupportedSchema
	}
	trimmedData := bytes.TrimSpace(envelope.Data)
	if len(trimmedData) == 0 || trimmedData[0] != '[' {
		return SQLiteProbeResult{}, ErrUnsupportedSchema
	}
	dict, err := ParseTagDictionary(bytes.NewReader(payload))
	if err != nil {
		return SQLiteProbeResult{}, fmt.Errorf("%w: %v", ErrUnsupportedSchema, err)
	}
	return SQLiteProbeResult{
		Format:   SchemaTagTranslationTextV1,
		TagCount: dict.Len(),
	}, nil
}

func listSQLiteTables(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type IN ('table','view')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			tables[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	return tables, rows.Err()
}

// IsGalleryCategoryNSFW applies the gallery_v1 NSFW rule: only category "Non-H"
// (case-insensitive, trimmed) is non-NSFW; all other categories default to true.
func IsGalleryCategoryNSFW(category string) bool {
	return !strings.EqualFold(strings.TrimSpace(category), "Non-H")
}

// ParseGalleryTags splits raw gallery_v1 tags into extracted Authors (from artist:/group:)
// and remaining namespace:tag entries, deduplicated in order.
func ParseGalleryTags(rawTags []string) (authors []string, tags []string) {
	seenAuthors := make(map[string]struct{})
	seenTags := make(map[string]struct{})

	for _, raw := range rawTags {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		lower := strings.ToLower(t)
		if strings.HasPrefix(lower, "artist:") {
			name := strings.TrimSpace(t[len("artist:"):])
			if name != "" {
				key := strings.ToLower(name)
				if _, dup := seenAuthors[key]; !dup {
					seenAuthors[key] = struct{}{}
					authors = append(authors, name)
				}
			}
			continue
		}
		if strings.HasPrefix(lower, "group:") {
			name := strings.TrimSpace(t[len("group:"):])
			if name != "" {
				key := strings.ToLower(name)
				if _, dup := seenAuthors[key]; !dup {
					seenAuthors[key] = struct{}{}
					authors = append(authors, name)
				}
			}
			continue
		}
		if _, dup := seenTags[lower]; !dup {
			seenTags[lower] = struct{}{}
			tags = append(tags, t)
		}
	}
	return authors, tags
}

func getEntryFromUpstream(ctx context.Context, db *sql.DB, sourceID, schema, entryID string) (*Entry, error) {
	entryID = strings.TrimSpace(entryID)
	if entryID == "" {
		return nil, ErrEntryNotFound
	}
	switch schema {
	case SchemaMangaAlphaMetaV1, SchemaMangaAlphaMetaV2:
		return getMangaAlphaMetaV1Entry(ctx, db, sourceID, entryID)
	case SchemaGalleryV1:
		return getGalleryV1Entry(ctx, db, sourceID, entryID)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedSchema, schema)
	}
}

func getMangaAlphaMetaV1Entry(ctx context.Context, db *sql.DB, sourceID, entryID string) (*Entry, error) {
	cols, err := tableColumns(ctx, db, "entries")
	if err != nil {
		return nil, err
	}

	var (
		id, title, titleAlt, authorsRaw, summary, tagsRaw, category, coverURL, extraJSON sql.NullString
		rating                                                                           sql.NullFloat64
		isNSFW, updatedAt                                                                sql.NullInt64
		status, titleSort, publisher, language, releaseDate, readingDir                  sql.NullString
		totalBookCount, ageRating                                                        sql.NullInt64
		linksJSON, sharingJSON                                                           sql.NullString
		hasCoverBlobInt                                                                  int
	)

	selectExprs := make([]string, 0, 24)
	scanDests := make([]any, 0, 24)
	addCol := func(col string, expr string, dest any) {
		if cols[col] {
			selectExprs = append(selectExprs, expr)
			scanDests = append(scanDests, dest)
		}
	}

	addCol("id", "id", &id)
	addCol("title", "title", &title)
	addCol("title_alt", "title_alt", &titleAlt)
	addCol("authors", "authors", &authorsRaw)
	addCol("summary", "summary", &summary)
	addCol("tags", "tags", &tagsRaw)
	addCol("category", "category", &category)
	addCol("cover_url", "cover_url", &coverURL)
	addCol("rating", "rating", &rating)
	addCol("is_nsfw", "is_nsfw", &isNSFW)
	addCol("updated_at", "updated_at", &updatedAt)
	addCol("extra_json", "extra_json", &extraJSON)

	// Optional v1/v2 extension columns (backwards compatible when absent).
	addCol("status", "status", &status)
	addCol("title_sort", "title_sort", &titleSort)
	addCol("publisher", "publisher", &publisher)
	addCol("language", "language", &language)
	addCol("release_date", "release_date", &releaseDate)
	addCol("total_book_count", "total_book_count", &totalBookCount)
	addCol("age_rating", "age_rating", &ageRating)
	addCol("reading_direction", "reading_direction", &readingDir)
	addCol("links_json", "links_json", &linksJSON)
	addCol("sharing_labels_json", "sharing_labels_json", &sharingJSON)
	if cols["cover_blob"] {
		selectExprs = append(selectExprs, "(cover_blob IS NOT NULL AND length(cover_blob) > 0)")
		scanDests = append(scanDests, &hasCoverBlobInt)
	}

	if len(selectExprs) == 0 || !cols["id"] {
		return nil, fmt.Errorf("%w: entries table missing id column", ErrUnsupportedSchema)
	}

	query := "SELECT " + strings.Join(selectExprs, ", ") + " FROM entries WHERE id = ?"
	err = db.QueryRowContext(ctx, query, entryID).Scan(scanDests...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEntryNotFound
	}
	if err != nil {
		return nil, err
	}

	entry := &Entry{
		SourceID:         sourceID,
		EntryID:          strings.TrimSpace(id.String),
		Title:            strings.TrimSpace(title.String),
		TitleAlt:         strings.TrimSpace(titleAlt.String),
		Authors:          parseStringListField(authorsRaw.String),
		Category:         strings.TrimSpace(category.String),
		Tags:             parseStringListField(tagsRaw.String),
		Summary:          strings.TrimSpace(summary.String),
		CoverURL:         strings.TrimSpace(coverURL.String),
		Rating:           rating.Float64,
		PostedAt:         updatedAt.Int64,
		IsNSFW:           isNSFW.Int64 != 0,
		Status:           strings.TrimSpace(status.String),
		TitleSort:        strings.TrimSpace(titleSort.String),
		Publisher:        strings.TrimSpace(publisher.String),
		Language:         strings.TrimSpace(language.String),
		ReleaseDate:      strings.TrimSpace(releaseDate.String),
		ReadingDirection: strings.TrimSpace(readingDir.String),
		SharingLabels:    parseStringListField(sharingJSON.String),
		Links:            parseOpenLinksJSON(linksJSON.String),
		HasCoverBlob:     hasCoverBlobInt != 0,
	}
	if totalBookCount.Valid && totalBookCount.Int64 > 0 {
		v := int(totalBookCount.Int64)
		entry.TotalBookCount = &v
	}
	if ageRating.Valid {
		v := int(ageRating.Int64)
		entry.AgeRating = &v
		if v >= 18 {
			entry.IsNSFW = true
		}
	}
	for _, sl := range entry.SharingLabels {
		if isNSFWKeyword(sl) {
			entry.IsNSFW = true
			break
		}
	}
	if entry.CoverURL == "" && entry.HasCoverBlob && sourceID != "" {
		entry.CoverURL = EncodeSQLiteBlobCoverRef(sourceID, SQLiteBlobKindOpenEntry, entry.EntryID)
	}

	if raw := strings.TrimSpace(extraJSON.String); raw != "" {
		var fields map[string]any
		if json.Unmarshal([]byte(raw), &fields) == nil && len(fields) > 0 {
			entry.RawFields = fields
			if entry.Token == "" {
				if v, ok := fields["token"].(string); ok {
					entry.Token = strings.TrimSpace(v)
				}
			}
			if entry.SourceURL == "" {
				if v, ok := fields["source_url"].(string); ok {
					entry.SourceURL = strings.TrimSpace(v)
				} else if v, ok := fields["sourceUrl"].(string); ok {
					entry.SourceURL = strings.TrimSpace(v)
				}
			}
		}
	}
	return entry, nil
}

// ReadOpenOfflineBooks reads volume/book records for seriesEntryID from an optional
// `books` table in a mangaalpha_meta_v1 / mangaalpha_meta_v2 SQLite database.
// Returns (nil, nil) when the database has no `books` table.
func ReadOpenOfflineBooks(ctx context.Context, db *sql.DB, seriesEntryID string) ([]OpenBookEntry, error) {
	if db == nil {
		return nil, nil
	}
	seriesEntryID = strings.TrimSpace(seriesEntryID)
	if seriesEntryID == "" {
		return nil, nil
	}
	tables, err := listSQLiteTables(ctx, db)
	if err != nil {
		return nil, err
	}
	if !tables["books"] {
		return nil, nil
	}
	cols, err := tableColumns(ctx, db, "books")
	if err != nil {
		return nil, err
	}
	if !cols["series_entry_id"] {
		return nil, nil
	}

	colStr := func(name string) string {
		if cols[name] {
			return "COALESCE(" + name + ", '')"
		}
		return "''"
	}
	numSortExpr := "NULL"
	if cols["number_sort"] {
		numSortExpr = "number_sort"
	}
	hasBlobExpr := "0"
	if cols["cover_blob"] {
		hasBlobExpr = "(cover_blob IS NOT NULL AND length(cover_blob) > 0)"
	}
	orderExpr := "rowid ASC"
	if cols["number_sort"] {
		orderExpr = "COALESCE(number_sort, 1e18) ASC, rowid ASC"
	}

	query := fmt.Sprintf(`
SELECT %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s
FROM books
WHERE series_entry_id = ?
ORDER BY %s`,
		colStr("series_entry_id"),
		colStr("book_key"),
		colStr("file_name"),
		colStr("number"),
		numSortExpr,
		colStr("title"),
		colStr("summary"),
		colStr("release_date"),
		colStr("authors_json"),
		colStr("tags_json"),
		colStr("isbn"),
		colStr("links_json"),
		colStr("cover_url"),
		hasBlobExpr,
		orderExpr,
	)

	rows, err := db.QueryContext(ctx, query, seriesEntryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OpenBookEntry
	for rows.Next() {
		var (
			item                             OpenBookEntry
			numSort                          sql.NullFloat64
			authorsJSON, tagsJSON, linksJSON string
			hasBlob                          int
		)
		if err := rows.Scan(
			&item.SeriesEntryID, &item.BookKey, &item.FileName,
			&item.Number, &numSort, &item.Title, &item.Summary,
			&item.ReleaseDate, &authorsJSON, &tagsJSON, &item.ISBN,
			&linksJSON, &item.CoverURL, &hasBlob,
		); err != nil {
			return nil, err
		}
		item.SeriesEntryID = strings.TrimSpace(item.SeriesEntryID)
		item.BookKey = strings.TrimSpace(item.BookKey)
		item.FileName = strings.TrimSpace(item.FileName)
		item.Number = strings.TrimSpace(item.Number)
		item.Title = strings.TrimSpace(item.Title)
		item.Summary = strings.TrimSpace(item.Summary)
		item.ReleaseDate = strings.TrimSpace(item.ReleaseDate)
		item.ISBN = strings.TrimSpace(item.ISBN)
		item.CoverURL = strings.TrimSpace(item.CoverURL)
		item.HasCoverBlob = hasBlob != 0
		if numSort.Valid {
			v := numSort.Float64
			item.NumberSort = &v
		}
		item.Authors = parseOpenAuthorsJSON(authorsJSON)
		item.Tags = parseStringListField(tagsJSON)
		item.Links = parseOpenLinksJSON(linksJSON)
		out = append(out, item)
	}
	return out, rows.Err()
}

// ReadOpenCoverBLOB lazily reads a single cover_blob from an open-format SQLite DB
// (entries for kind="open-entry", books for kind="open-book"), strictly read-only and <= 16 MiB.
func ReadOpenCoverBLOB(ctx context.Context, dbPath, kind, key string) ([]byte, string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, "", ErrEntryNotFound
	}
	switch kind {
	case SQLiteBlobKindOpenEntry, SQLiteBlobKindOpenBook:
	default:
		return nil, "", fmt.Errorf("%w: unsupported open cover kind %q", ErrInvalidConfig, kind)
	}

	db, err := openKomgaReadOnlySQLite(dbPath)
	if err != nil {
		return nil, "", err
	}
	defer db.Close()

	tables, err := listSQLiteTables(ctx, db)
	if err != nil {
		return nil, "", err
	}

	var (
		blobSize sql.NullInt64
		blob     []byte
	)
	if kind == SQLiteBlobKindOpenEntry {
		if !tables["entries"] {
			return nil, "", ErrEntryNotFound
		}
		cols, err := tableColumns(ctx, db, "entries")
		if err != nil || !cols["cover_blob"] || !cols["id"] {
			return nil, "", ErrEntryNotFound
		}
		err = db.QueryRowContext(ctx, `
SELECT LENGTH(cover_blob),
       CASE WHEN LENGTH(cover_blob) <= ? THEN cover_blob ELSE NULL END
FROM entries WHERE id = ? LIMIT 1`, maxSQLiteBlobCoverBytes, key).Scan(&blobSize, &blob)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrEntryNotFound
		}
		if err != nil {
			return nil, "", err
		}
	} else {
		if !tables["books"] {
			return nil, "", ErrEntryNotFound
		}
		cols, err := tableColumns(ctx, db, "books")
		if err != nil || !cols["cover_blob"] || !cols["book_key"] {
			return nil, "", ErrEntryNotFound
		}
		err = db.QueryRowContext(ctx, `
SELECT LENGTH(cover_blob),
       CASE WHEN LENGTH(cover_blob) <= ? THEN cover_blob ELSE NULL END
FROM books WHERE book_key = ? LIMIT 1`, maxSQLiteBlobCoverBytes, key).Scan(&blobSize, &blob)
		if errors.Is(err, sql.ErrNoRows) && cols["series_entry_id"] {
			if sid, bkey, ok := strings.Cut(key, ":"); ok && sid != "" && bkey != "" {
				err = db.QueryRowContext(ctx, `
SELECT LENGTH(cover_blob),
       CASE WHEN LENGTH(cover_blob) <= ? THEN cover_blob ELSE NULL END
FROM books WHERE series_entry_id = ? AND book_key = ? LIMIT 1`, maxSQLiteBlobCoverBytes, sid, bkey).Scan(&blobSize, &blob)
			}
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrEntryNotFound
		}
		if err != nil {
			return nil, "", err
		}
	}

	if blobSize.Valid && blobSize.Int64 > maxSQLiteBlobCoverBytes {
		return nil, "", fmt.Errorf("%w: open cover BLOB exceeds %d bytes limit", ErrInvalidImage, maxSQLiteBlobCoverBytes)
	}
	if len(blob) == 0 {
		return nil, "", ErrEntryNotFound
	}
	_, mimeType, ok := DetectImageFormat(blob)
	if !ok {
		return nil, "", fmt.Errorf("%w: unrecognized open cover BLOB bytes", ErrInvalidImage)
	}
	return blob, mimeType, nil
}

func parseOpenLinksJSON(raw string) []OpenLink {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var objLinks []OpenLink
	if json.Unmarshal([]byte(raw), &objLinks) == nil && len(objLinks) > 0 {
		out := make([]OpenLink, 0, len(objLinks))
		seen := make(map[string]struct{}, len(objLinks))
		for _, lk := range objLinks {
			u := strings.TrimSpace(lk.URL)
			if u == "" {
				continue
			}
			parsed, pErr := url.Parse(u)
			if pErr != nil || (strings.ToLower(parsed.Scheme) != "http" && strings.ToLower(parsed.Scheme) != "https") {
				continue
			}
			if _, dup := seen[u]; dup {
				continue
			}
			seen[u] = struct{}{}
			label := strings.TrimSpace(lk.Label)
			if label == "" {
				label = inferLinkLabel(u)
			}
			out = append(out, OpenLink{Label: label, URL: u})
		}
		return out
	}
	var strLinks []string
	if json.Unmarshal([]byte(raw), &strLinks) == nil && len(strLinks) > 0 {
		out := make([]OpenLink, 0, len(strLinks))
		for _, s := range dedupeStrings(strLinks) {
			if safe := cleanSafeExternalURL(s); safe != "" {
				out = append(out, OpenLink{Label: inferLinkLabel(safe), URL: safe})
			}
		}
		return out
	}
	return nil
}

func parseOpenAuthorsJSON(raw string) []OpenAuthor {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var objAuthors []OpenAuthor
	if json.Unmarshal([]byte(raw), &objAuthors) == nil && len(objAuthors) > 0 {
		out := make([]OpenAuthor, 0, len(objAuthors))
		seen := make(map[string]struct{}, len(objAuthors))
		for _, a := range objAuthors {
			name := strings.TrimSpace(a.Name)
			if name == "" {
				continue
			}
			role := strings.ToLower(strings.TrimSpace(a.Role))
			if role == "" {
				role = "writer"
			}
			key := strings.ToLower(name) + "\x00" + role
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, OpenAuthor{Name: name, Role: role})
		}
		return out
	}
	names := parseStringListField(raw)
	if len(names) == 0 {
		return nil
	}
	out := make([]OpenAuthor, 0, len(names))
	for _, n := range names {
		out = append(out, OpenAuthor{Name: n, Role: "writer"})
	}
	return out
}

func getGalleryV1Entry(ctx context.Context, db *sql.DB, sourceID, entryID string) (*Entry, error) {
	gid, err := strconv.ParseInt(entryID, 10, 64)
	if err != nil {
		return nil, ErrEntryNotFound
	}

	var (
		token, title, titleJpn, category, thumb, uploader sql.NullString
		posted, filecount, filesize, rootGID              sql.NullInt64
		expunged, removed, replaced                       sql.NullInt64
		rating                                            sql.NullFloat64
	)
	err = db.QueryRowContext(ctx, `
SELECT token, title, title_jpn, category, thumb, uploader, posted,
       filecount, filesize, expunged, removed, replaced, rating, root_gid
FROM gallery WHERE gid = ?`, gid).Scan(
		&token, &title, &titleJpn, &category, &thumb, &uploader, &posted,
		&filecount, &filesize, &expunged, &removed, &replaced, &rating, &rootGID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEntryNotFound
	}
	if err != nil {
		return nil, err
	}

	var rawTags []string
	if tables, tErr := listSQLiteTables(ctx, db); tErr == nil && tables["gid_tid"] && tables["tag"] {
		tRows, qErr := db.QueryContext(ctx, `
SELECT t.name
FROM gid_tid gt
JOIN tag t ON t.id = gt.tid
WHERE gt.gid = ?
ORDER BY gt.tid ASC`, gid)
		if qErr != nil {
			return nil, qErr
		}
		for tRows.Next() {
			var tagName sql.NullString
			if sErr := tRows.Scan(&tagName); sErr == nil && strings.TrimSpace(tagName.String) != "" {
				rawTags = append(rawTags, tagName.String)
			}
		}
		if rErr := tRows.Err(); rErr != nil {
			tRows.Close()
			return nil, rErr
		}
		tRows.Close()
	}

	authors, tags := ParseGalleryTags(rawTags)
	catStr := strings.TrimSpace(category.String)

	rawFields := map[string]any{
		"gid": gid,
	}
	if u := strings.TrimSpace(uploader.String); u != "" {
		rawFields["uploader"] = u
	}
	if filecount.Valid {
		rawFields["filecount"] = filecount.Int64
	}
	if filesize.Valid {
		rawFields["filesize"] = filesize.Int64
	}
	if expunged.Valid {
		rawFields["expunged"] = expunged.Int64 != 0
	}
	if removed.Valid {
		rawFields["removed"] = removed.Int64 != 0
	}
	if replaced.Valid {
		rawFields["replaced"] = replaced.Int64 != 0
	}
	if rootGID.Valid && rootGID.Int64 > 0 {
		rawFields["root_gid"] = rootGID.Int64
	}

	return &Entry{
		SourceID:  sourceID,
		EntryID:   strconv.FormatInt(gid, 10),
		Token:     strings.TrimSpace(token.String),
		Title:     strings.TrimSpace(title.String),
		TitleAlt:  strings.TrimSpace(titleJpn.String),
		Authors:   authors,
		Category:  catStr,
		Tags:      tags,
		CoverURL:  strings.TrimSpace(thumb.String),
		Rating:    rating.Float64,
		PostedAt:  posted.Int64,
		IsNSFW:    IsGalleryCategoryNSFW(catStr),
		RawFields: rawFields,
	}, nil
}

func parseStringListField(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		var items []string
		if json.Unmarshal([]byte(raw), &items) == nil {
			return dedupeStrings(items)
		}
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '|' || r == '\n'
	})
	return dedupeStrings(parts)
}

func dedupeStrings(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, it := range items {
		trimmed := strings.TrimSpace(it)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}
