package extmeta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/xJogger/fake-komga-115/internal/bangumi"
	_ "modernc.org/sqlite"
)

const (
	// FormatKomgaSQLite identifies a read-only Komga database.sqlite metadata source.
	FormatKomgaSQLite = "komga_sqlite_v1"
	// FormatOpenV1 identifies the single-table MangaAlpha open metadata schema.
	FormatOpenV1 = SchemaMangaAlphaMetaV1
	// FormatOpenV2 is the backwards-compatible alias for extended mangaalpha_meta_v1 with optional series columns and books table.
	FormatOpenV2 = "mangaalpha_meta_v2"
	// FormatTagDictV1 identifies tag_translation_text_v1 JSON/gzipped dictionaries.
	FormatTagDictV1 = "tag_translation_text_v1"

	// SQLiteBlobCoverPrefix is the virtual cover URI scheme for covers stored inside offline SQLite BLOBs.
	SQLiteBlobCoverPrefix     = "sqlite-blob://"
	SQLiteBlobKindKomgaSeries = "komga-series"
	SQLiteBlobKindKomgaBook   = "komga-book"
	SQLiteBlobKindOpenEntry   = "open-entry"
	SQLiteBlobKindOpenBook    = "open-book"

	maxSQLiteBlobCoverBytes = 16 << 20 // 16 MiB
)

// KomgaLink represents a web link stored in Komga metadata tables.
type KomgaLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// KomgaAlternateTitle represents an alternate title stored in Komga metadata tables.
type KomgaAlternateTitle struct {
	Label string `json:"label"`
	Title string `json:"title"`
}

// KomgaAuthor represents an author role stored in Komga BOOK_METADATA_AUTHOR.
type KomgaAuthor struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// KomgaBookEntry holds series-scoped book metadata extracted from a Komga database.sqlite.
type KomgaBookEntry struct {
	KomgaBookID         string        `json:"komgaBookId"`
	Name                string        `json:"name"`
	URL                 string        `json:"url"`
	Title               string        `json:"title"`
	Summary             string        `json:"summary"`
	Number              string        `json:"number"`
	NumberSort          *float64      `json:"numberSort,omitempty"`
	ReleaseDate         string        `json:"releaseDate"`
	ISBN                string        `json:"isbn"`
	Authors             []KomgaAuthor `json:"authors,omitempty"`
	Tags                []string      `json:"tags,omitempty"`
	Links               []KomgaLink   `json:"links,omitempty"`
	SelectedThumbnailID string        `json:"selectedThumbnailId,omitempty"`
}

// KomgaSeriesEntry holds complete series and book metadata extracted from a Komga database.sqlite.
type KomgaSeriesEntry struct {
	KomgaSeriesID       string                `json:"komgaSeriesId"`
	Name                string                `json:"name"`
	URL                 string                `json:"url"`
	OneShot             bool                  `json:"oneShot"`
	Status              string                `json:"status"`
	Title               string                `json:"title"`
	TitleSort           string                `json:"titleSort"`
	Summary             string                `json:"summary"`
	ReadingDirection    string                `json:"readingDirection"`
	Publisher           string                `json:"publisher"`
	AgeRating           *int                  `json:"ageRating,omitempty"`
	Language            string                `json:"language"`
	TotalBookCount      *int                  `json:"totalBookCount,omitempty"`
	ReleaseDate         string                `json:"releaseDate"`
	Genres              []string              `json:"genres,omitempty"`
	Tags                []string              `json:"tags,omitempty"`
	SharingLabels       []string              `json:"sharingLabels,omitempty"`
	Links               []KomgaLink           `json:"links,omitempty"`
	AlternateTitles     []KomgaAlternateTitle `json:"alternateTitles,omitempty"`
	Authors             []KomgaAuthor         `json:"authors,omitempty"`
	NSFW                bool                  `json:"nsfw"`
	SelectedThumbnailID string                `json:"selectedThumbnailId,omitempty"`
	Books               []KomgaBookEntry      `json:"books,omitempty"`
}

// KomgaSchemaInfo summarizes detected required and optional tables in a Komga SQLite file.
type KomgaSchemaInfo struct {
	HasSeriesMetadataGenre          bool
	HasSeriesMetadataTag            bool
	HasSeriesMetadataLink           bool
	HasSeriesMetadataSharing        bool
	HasSeriesMetadataAlternateTitle bool
	HasBookMetadataAuthor           bool
	HasBookMetadataTag              bool
	HasBookMetadataLink             bool
	HasThumbnailSeries              bool
	HasThumbnailBook                bool
}

// DetectSQLiteWALWarning checks whether an uncheckpointed non-empty -wal file sits beside dbPath.
func DetectSQLiteWALWarning(dbPath string) (bool, int64) {
	trimmed := strings.TrimSpace(dbPath)
	if trimmed == "" {
		return false, 0
	}
	info, err := os.Stat(trimmed + "-wal")
	if err != nil || info.IsDir() || info.Size() <= 0 {
		return false, 0
	}
	return true, info.Size()
}

// EncodeSQLiteBlobCoverRef encodes a virtual sqlite-blob:// cover reference.
func EncodeSQLiteBlobCoverRef(sourceID, kind, recordID string) string {
	sourceID = strings.TrimSpace(sourceID)
	kind = strings.TrimSpace(kind)
	recordID = strings.TrimSpace(recordID)
	if sourceID == "" || kind == "" || recordID == "" {
		return ""
	}
	return SQLiteBlobCoverPrefix + url.PathEscape(sourceID) + "/" + url.PathEscape(kind) + "/" + url.PathEscape(recordID)
}

// ParseSQLiteBlobCoverRef decodes and validates a sqlite-blob://<sourceID>/<kind>/<recordID> reference.
func ParseSQLiteBlobCoverRef(raw string) (sourceID, kind, recordID string, ok bool) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, SQLiteBlobCoverPrefix) {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(trimmed, SQLiteBlobCoverPrefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	s, err1 := url.PathUnescape(parts[0])
	k, err2 := url.PathUnescape(parts[1])
	r, err3 := url.PathUnescape(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return "", "", "", false
	}
	s = strings.TrimSpace(s)
	k = strings.TrimSpace(k)
	r = strings.TrimSpace(r)
	if s == "" || r == "" || strings.Contains(s, "/") || s == "." || s == ".." {
		return "", "", "", false
	}
	switch k {
	case SQLiteBlobKindKomgaSeries, SQLiteBlobKindKomgaBook, SQLiteBlobKindOpenEntry, SQLiteBlobKindOpenBook:
	default:
		return "", "", "", false
	}
	return s, k, r, true
}

func openKomgaReadOnlySQLite(dbPath string) (*sql.DB, error) {
	db, _, err := openKomgaSQLiteSnapshot(dbPath)
	return db, err
}

// openKomgaSQLiteSnapshot reports when a pending WAL could not be read and
// the returned database contains only checkpointed data.
func openKomgaSQLiteSnapshot(dbPath string) (*sql.DB, bool, error) {
	if strings.TrimSpace(dbPath) == "" {
		return nil, false, errors.New("sqlite path must not be empty")
	}
	hasWAL, _ := DetectSQLiteWALWarning(dbPath)
	if hasWAL && fileReadable(dbPath+"-wal") && fileReadable(dbPath+"-shm") {
		if db, err := openReadOnlySQLiteWithMode(dbPath, false); err == nil {
			return db, false, nil
		}
	}
	db, err := openReadOnlySQLiteWithMode(dbPath, true)
	return db, hasWAL, err
}

func fileReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func openReadOnlySQLiteWithMode(path string, immutable bool) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uriPath := filepath.ToSlash(abs)
	uriPath = strings.ReplaceAll(uriPath, "%", "%25")
	uriPath = strings.ReplaceAll(uriPath, "?", "%3F")
	uriPath = strings.ReplaceAll(uriPath, "#", "%23")
	dsn := "file:" + uriPath + "?mode=ro"
	if immutable {
		dsn += "&immutable=1"
	} else {
		// SQLite must open the existing WAL index read-only as well.
		dsn += "&readonly_shm=1"
	}
	dsn += "&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	// Force SQLite to read the database schema and acquire a WAL snapshot.
	var count int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_schema").Scan(&count); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func hasKomgaRequiredTables(tables map[string]bool) bool {
	return tables["series"] && tables["series_metadata"] && tables["book"] && tables["book_metadata"]
}

// DetectKomgaSchema validates required Komga tables and discovers optional subtables.
func DetectKomgaSchema(ctx context.Context, db sqliteQueryer) (KomgaSchemaInfo, error) {
	if db == nil {
		return KomgaSchemaInfo{}, errors.New("sqlite db must not be nil")
	}
	tables, err := listSQLiteTables(ctx, db)
	if err != nil {
		return KomgaSchemaInfo{}, err
	}
	if !hasKomgaRequiredTables(tables) {
		return KomgaSchemaInfo{}, fmt.Errorf("%w: missing required Komga tables (SERIES, SERIES_METADATA, BOOK, BOOK_METADATA)", ErrUnsupportedSchema)
	}
	return KomgaSchemaInfo{
		HasSeriesMetadataGenre:          tables["series_metadata_genre"],
		HasSeriesMetadataTag:            tables["series_metadata_tag"],
		HasSeriesMetadataLink:           tables["series_metadata_link"],
		HasSeriesMetadataSharing:        tables["series_metadata_sharing"],
		HasSeriesMetadataAlternateTitle: tables["series_metadata_alternate_title"],
		HasBookMetadataAuthor:           tables["book_metadata_author"],
		HasBookMetadataTag:              tables["book_metadata_tag"],
		HasBookMetadataLink:             tables["book_metadata_link"],
		HasThumbnailSeries:              tables["thumbnail_series"],
		HasThumbnailBook:                tables["thumbnail_book"],
	}, nil
}

// ReadKomgaSeries loads a single series and all of its books/metadata from an open Komga SQLite DB.
func ReadKomgaSeries(ctx context.Context, db *sql.DB, seriesID string) (*KomgaSeriesEntry, error) {
	seriesID = strings.TrimSpace(seriesID)
	if seriesID == "" {
		return nil, ErrEntryNotFound
	}
	schemaInfo, err := DetectKomgaSchema(ctx, db)
	if err != nil {
		return nil, err
	}
	entries, err := queryKomgaSeries(ctx, db, schemaInfo, seriesID, true)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, ErrEntryNotFound
	}
	return &entries[0], nil
}

// ReadKomgaAllSeries loads all series and associated book metadata from an open Komga SQLite DB.
func ReadKomgaAllSeries(ctx context.Context, db *sql.DB) ([]KomgaSeriesEntry, error) {
	schemaInfo, err := DetectKomgaSchema(ctx, db)
	if err != nil {
		return nil, err
	}
	return queryKomgaSeries(ctx, db, schemaInfo, "", true)
}

func queryKomgaSeries(ctx context.Context, db sqliteQueryer, info KomgaSchemaInfo, onlySeriesID string, includeBooks bool) ([]KomgaSeriesEntry, error) {
	seriesCols, err := tableColumns(ctx, db, "SERIES")
	if err != nil {
		return nil, err
	}
	metaCols, err := tableColumns(ctx, db, "SERIES_METADATA")
	if err != nil {
		return nil, err
	}

	colOrEmpty := func(cols map[string]bool, alias, col string) string {
		if cols[strings.ToLower(col)] {
			return fmt.Sprintf("COALESCE(%s.%s, '')", alias, col)
		}
		return "''"
	}
	colOrZero := func(cols map[string]bool, alias, col string) string {
		if cols[strings.ToLower(col)] {
			return fmt.Sprintf("COALESCE(%s.%s, 0)", alias, col)
		}
		return "0"
	}
	colOrNullInt := func(cols map[string]bool, alias, col string) string {
		if cols[strings.ToLower(col)] {
			return fmt.Sprintf("%s.%s", alias, col)
		}
		return "NULL"
	}

	query := fmt.Sprintf(`
SELECT s.ID, %s, %s, %s,
       %s, %s, %s, %s, %s, %s, %s, %s, %s, %s
FROM SERIES s
LEFT JOIN SERIES_METADATA sm ON sm.SERIES_ID = s.ID
WHERE 1=1`,
		colOrEmpty(seriesCols, "s", "NAME"),
		colOrEmpty(seriesCols, "s", "URL"),
		colOrZero(seriesCols, "s", "ONESHOT"),
		colOrEmpty(metaCols, "sm", "STATUS"),
		colOrEmpty(metaCols, "sm", "TITLE"),
		colOrEmpty(metaCols, "sm", "TITLE_SORT"),
		colOrEmpty(metaCols, "sm", "SUMMARY"),
		colOrEmpty(metaCols, "sm", "READING_DIRECTION"),
		colOrEmpty(metaCols, "sm", "PUBLISHER"),
		colOrNullInt(metaCols, "sm", "AGE_RATING"),
		colOrEmpty(metaCols, "sm", "LANGUAGE"),
		colOrNullInt(metaCols, "sm", "TOTAL_BOOK_COUNT"),
		colOrEmpty(metaCols, "sm", "RELEASE_DATE"),
	)
	if seriesCols["deleted"] {
		query += " AND COALESCE(s.DELETED, 0) = 0"
	}
	var args []any
	if onlySeriesID != "" {
		query += " AND s.ID = ?"
		args = append(args, onlySeriesID)
	}
	query += " ORDER BY s.ROWID ASC"

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var seriesList []KomgaSeriesEntry
	indexByID := make(map[string]int)
	for rows.Next() {
		var (
			entry          KomgaSeriesEntry
			oneShot        int
			ageRating      sql.NullInt64
			totalBookCount sql.NullInt64
		)
		if err := rows.Scan(
			&entry.KomgaSeriesID, &entry.Name, &entry.URL, &oneShot,
			&entry.Status, &entry.Title, &entry.TitleSort, &entry.Summary,
			&entry.ReadingDirection, &entry.Publisher, &ageRating,
			&entry.Language, &totalBookCount, &entry.ReleaseDate,
		); err != nil {
			return nil, err
		}
		entry.KomgaSeriesID = strings.TrimSpace(entry.KomgaSeriesID)
		entry.Name = strings.TrimSpace(entry.Name)
		entry.URL = strings.TrimSpace(entry.URL)
		entry.OneShot = oneShot != 0
		entry.Status = strings.TrimSpace(entry.Status)
		entry.Title = strings.TrimSpace(entry.Title)
		if entry.Title == "" {
			entry.Title = entry.Name
		}
		entry.TitleSort = strings.TrimSpace(entry.TitleSort)
		if entry.TitleSort == "" {
			entry.TitleSort = entry.Title
		}
		entry.Summary = strings.TrimSpace(entry.Summary)
		entry.ReadingDirection = strings.TrimSpace(entry.ReadingDirection)
		entry.Publisher = strings.TrimSpace(entry.Publisher)
		entry.Language = strings.TrimSpace(entry.Language)
		entry.ReleaseDate = strings.TrimSpace(entry.ReleaseDate)
		if ageRating.Valid {
			v := int(ageRating.Int64)
			entry.AgeRating = &v
		}
		if totalBookCount.Valid && totalBookCount.Int64 > 0 {
			v := int(totalBookCount.Int64)
			entry.TotalBookCount = &v
		}
		indexByID[entry.KomgaSeriesID] = len(seriesList)
		seriesList = append(seriesList, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(seriesList) == 0 {
		return nil, nil
	}

	if err := loadKomgaSeriesSubtables(ctx, db, info, onlySeriesID, seriesList, indexByID); err != nil {
		return nil, err
	}
	if includeBooks {
		if err := loadKomgaBooks(ctx, db, info, onlySeriesID, seriesList, indexByID); err != nil {
			return nil, err
		}
	}
	for i := range seriesList {
		finalizeKomgaSeriesEntry(&seriesList[i])
	}
	return seriesList, nil
}

func loadKomgaSeriesSubtables(
	ctx context.Context,
	db sqliteQueryer,
	info KomgaSchemaInfo,
	onlySeriesID string,
	seriesList []KomgaSeriesEntry,
	indexByID map[string]int,
) error {
	loadStrings := func(enabled bool, table, col string, assign func(*KomgaSeriesEntry, string)) error {
		if !enabled {
			return nil
		}
		q := fmt.Sprintf("SELECT SERIES_ID, COALESCE(%s, '') FROM %s", col, table)
		var args []any
		if onlySeriesID != "" {
			q += " WHERE SERIES_ID = ?"
			args = append(args, onlySeriesID)
		}
		q += " ORDER BY ROWID ASC"
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sid, val string
			if err := rows.Scan(&sid, &val); err != nil {
				return err
			}
			if idx, ok := indexByID[strings.TrimSpace(sid)]; ok {
				if trimmed := strings.TrimSpace(val); trimmed != "" {
					assign(&seriesList[idx], trimmed)
				}
			}
		}
		return rows.Err()
	}

	if err := loadStrings(info.HasSeriesMetadataGenre, "SERIES_METADATA_GENRE", "GENRE", func(e *KomgaSeriesEntry, v string) {
		e.Genres = appendUniqueString(e.Genres, v)
	}); err != nil {
		return err
	}
	if err := loadStrings(info.HasSeriesMetadataTag, "SERIES_METADATA_TAG", "TAG", func(e *KomgaSeriesEntry, v string) {
		e.Tags = appendUniqueString(e.Tags, v)
	}); err != nil {
		return err
	}
	if err := loadStrings(info.HasSeriesMetadataSharing, "SERIES_METADATA_SHARING", "LABEL", func(e *KomgaSeriesEntry, v string) {
		e.SharingLabels = appendUniqueString(e.SharingLabels, v)
	}); err != nil {
		return err
	}

	if info.HasSeriesMetadataLink {
		q := "SELECT SERIES_ID, COALESCE(LABEL, ''), COALESCE(URL, '') FROM SERIES_METADATA_LINK"
		var args []any
		if onlySeriesID != "" {
			q += " WHERE SERIES_ID = ?"
			args = append(args, onlySeriesID)
		}
		q += " ORDER BY ROWID ASC"
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var sid, label, rawURL string
			if err := rows.Scan(&sid, &label, &rawURL); err != nil {
				rows.Close()
				return err
			}
			if idx, ok := indexByID[strings.TrimSpace(sid)]; ok {
				if u := strings.TrimSpace(rawURL); u != "" {
					seriesList[idx].Links = append(seriesList[idx].Links, KomgaLink{
						Label: strings.TrimSpace(label),
						URL:   u,
					})
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}

	if info.HasSeriesMetadataAlternateTitle {
		q := "SELECT SERIES_ID, COALESCE(LABEL, ''), COALESCE(TITLE, '') FROM SERIES_METADATA_ALTERNATE_TITLE"
		var args []any
		if onlySeriesID != "" {
			q += " WHERE SERIES_ID = ?"
			args = append(args, onlySeriesID)
		}
		q += " ORDER BY ROWID ASC"
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var sid, label, title string
			if err := rows.Scan(&sid, &label, &title); err != nil {
				rows.Close()
				return err
			}
			if idx, ok := indexByID[strings.TrimSpace(sid)]; ok {
				if t := strings.TrimSpace(title); t != "" {
					seriesList[idx].AlternateTitles = append(seriesList[idx].AlternateTitles, KomgaAlternateTitle{
						Label: strings.TrimSpace(label),
						Title: t,
					})
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}

	if info.HasThumbnailSeries {
		thumbCols, err := tableColumns(ctx, db, "THUMBNAIL_SERIES")
		if err != nil {
			return err
		}
		orderExpr := "ROWID ASC"
		if thumbCols["selected"] {
			if thumbCols["type"] {
				orderExpr = "COALESCE(SELECTED, 0) DESC, CASE WHEN TYPE='USER_UPLOADED' THEN 0 ELSE 1 END, ROWID ASC"
			} else {
				orderExpr = "COALESCE(SELECTED, 0) DESC, ROWID ASC"
			}
		}
		q := "SELECT ID, SERIES_ID FROM THUMBNAIL_SERIES"
		var args []any
		if onlySeriesID != "" {
			q += " WHERE SERIES_ID = ?"
			args = append(args, onlySeriesID)
		}
		q += " ORDER BY " + orderExpr
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var tid, sid string
			if err := rows.Scan(&tid, &sid); err != nil {
				rows.Close()
				return err
			}
			if idx, ok := indexByID[strings.TrimSpace(sid)]; ok && seriesList[idx].SelectedThumbnailID == "" {
				seriesList[idx].SelectedThumbnailID = strings.TrimSpace(tid)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

func loadKomgaBooks(
	ctx context.Context,
	db sqliteQueryer,
	info KomgaSchemaInfo,
	onlySeriesID string,
	seriesList []KomgaSeriesEntry,
	indexByID map[string]int,
) error {
	bookCols, err := tableColumns(ctx, db, "BOOK")
	if err != nil {
		return err
	}
	bmCols, err := tableColumns(ctx, db, "BOOK_METADATA")
	if err != nil {
		return err
	}

	colOrEmpty := func(cols map[string]bool, alias, col string) string {
		if cols[strings.ToLower(col)] {
			return fmt.Sprintf("COALESCE(%s.%s, '')", alias, col)
		}
		return "''"
	}
	numSortExpr := "NULL"
	if bmCols["number_sort"] {
		numSortExpr = "bm.NUMBER_SORT"
	}

	q := fmt.Sprintf(`
SELECT b.ID, b.SERIES_ID, %s, %s,
       %s, %s, %s, %s, %s, %s
FROM BOOK b
LEFT JOIN BOOK_METADATA bm ON bm.BOOK_ID = b.ID
WHERE 1=1`,
		colOrEmpty(bookCols, "b", "NAME"),
		colOrEmpty(bookCols, "b", "URL"),
		colOrEmpty(bmCols, "bm", "TITLE"),
		colOrEmpty(bmCols, "bm", "SUMMARY"),
		colOrEmpty(bmCols, "bm", "NUMBER"),
		numSortExpr,
		colOrEmpty(bmCols, "bm", "RELEASE_DATE"),
		colOrEmpty(bmCols, "bm", "ISBN"),
	)
	if bookCols["deleted"] {
		q += " AND COALESCE(b.DELETED, 0) = 0"
	}
	var args []any
	if onlySeriesID != "" {
		q += " AND b.SERIES_ID = ?"
		args = append(args, onlySeriesID)
	}
	if bmCols["number_sort"] {
		q += " ORDER BY COALESCE(bm.NUMBER_SORT, 1e18) ASC, b.ROWID ASC"
	} else {
		q += " ORDER BY b.ROWID ASC"
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	type bookLoc struct {
		seriesIdx int
		bookIdx   int
	}
	locByBookID := make(map[string]bookLoc)
	for rows.Next() {
		var (
			b       KomgaBookEntry
			sid     string
			numSort sql.NullFloat64
		)
		if err := rows.Scan(
			&b.KomgaBookID, &sid, &b.Name, &b.URL,
			&b.Title, &b.Summary, &b.Number, &numSort, &b.ReleaseDate, &b.ISBN,
		); err != nil {
			return err
		}
		sIdx, ok := indexByID[strings.TrimSpace(sid)]
		if !ok {
			continue
		}
		b.KomgaBookID = strings.TrimSpace(b.KomgaBookID)
		b.Name = strings.TrimSpace(b.Name)
		b.URL = strings.TrimSpace(b.URL)
		b.Title = strings.TrimSpace(b.Title)
		b.Summary = strings.TrimSpace(b.Summary)
		b.Number = strings.TrimSpace(b.Number)
		b.ReleaseDate = strings.TrimSpace(b.ReleaseDate)
		b.ISBN = strings.TrimSpace(b.ISBN)
		if numSort.Valid {
			v := numSort.Float64
			b.NumberSort = &v
		}
		bIdx := len(seriesList[sIdx].Books)
		seriesList[sIdx].Books = append(seriesList[sIdx].Books, b)
		locByBookID[b.KomgaBookID] = bookLoc{seriesIdx: sIdx, bookIdx: bIdx}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(locByBookID) == 0 {
		return nil
	}

	whereClause := ""
	var subArgs []any
	if onlySeriesID != "" {
		whereClause = " WHERE BOOK_ID IN (SELECT ID FROM BOOK WHERE SERIES_ID = ?)"
		subArgs = append(subArgs, onlySeriesID)
	}

	if info.HasBookMetadataAuthor {
		arows, err := db.QueryContext(ctx, "SELECT BOOK_ID, COALESCE(NAME, ''), COALESCE(ROLE, '') FROM BOOK_METADATA_AUTHOR"+whereClause+" ORDER BY ROWID ASC", subArgs...)
		if err != nil {
			return err
		}
		for arows.Next() {
			var bid, name, role string
			if err := arows.Scan(&bid, &name, &role); err != nil {
				arows.Close()
				return err
			}
			if loc, ok := locByBookID[strings.TrimSpace(bid)]; ok {
				name = strings.TrimSpace(name)
				role = strings.ToLower(strings.TrimSpace(role))
				if name != "" {
					if role == "" {
						role = "writer"
					}
					bk := &seriesList[loc.seriesIdx].Books[loc.bookIdx]
					bk.Authors = appendUniqueKomgaAuthor(bk.Authors, KomgaAuthor{Name: name, Role: role})
				}
			}
		}
		if err := arows.Err(); err != nil {
			arows.Close()
			return err
		}
		arows.Close()
	}

	if info.HasBookMetadataTag {
		trows, err := db.QueryContext(ctx, "SELECT BOOK_ID, COALESCE(TAG, '') FROM BOOK_METADATA_TAG"+whereClause+" ORDER BY ROWID ASC", subArgs...)
		if err != nil {
			return err
		}
		for trows.Next() {
			var bid, tag string
			if err := trows.Scan(&bid, &tag); err != nil {
				trows.Close()
				return err
			}
			if loc, ok := locByBookID[strings.TrimSpace(bid)]; ok {
				if tag = strings.TrimSpace(tag); tag != "" {
					bk := &seriesList[loc.seriesIdx].Books[loc.bookIdx]
					bk.Tags = appendUniqueString(bk.Tags, tag)
				}
			}
		}
		if err := trows.Err(); err != nil {
			trows.Close()
			return err
		}
		trows.Close()
	}

	if info.HasBookMetadataLink {
		lrows, err := db.QueryContext(ctx, "SELECT BOOK_ID, COALESCE(LABEL, ''), COALESCE(URL, '') FROM BOOK_METADATA_LINK"+whereClause+" ORDER BY ROWID ASC", subArgs...)
		if err != nil {
			return err
		}
		for lrows.Next() {
			var bid, label, rawURL string
			if err := lrows.Scan(&bid, &label, &rawURL); err != nil {
				lrows.Close()
				return err
			}
			if loc, ok := locByBookID[strings.TrimSpace(bid)]; ok {
				if u := strings.TrimSpace(rawURL); u != "" {
					bk := &seriesList[loc.seriesIdx].Books[loc.bookIdx]
					bk.Links = append(bk.Links, KomgaLink{Label: strings.TrimSpace(label), URL: u})
				}
			}
		}
		if err := lrows.Err(); err != nil {
			lrows.Close()
			return err
		}
		lrows.Close()
	}

	if info.HasThumbnailBook {
		thumbCols, err := tableColumns(ctx, db, "THUMBNAIL_BOOK")
		if err != nil {
			return err
		}
		orderExpr := "ROWID ASC"
		if thumbCols["selected"] {
			if thumbCols["type"] {
				orderExpr = "COALESCE(SELECTED, 0) DESC, CASE WHEN TYPE='USER_UPLOADED' THEN 0 ELSE 1 END, ROWID ASC"
			} else {
				orderExpr = "COALESCE(SELECTED, 0) DESC, ROWID ASC"
			}
		}
		trows, err := db.QueryContext(ctx, "SELECT ID, BOOK_ID FROM THUMBNAIL_BOOK"+whereClause+" ORDER BY "+orderExpr, subArgs...)
		if err != nil {
			return err
		}
		for trows.Next() {
			var tid, bid string
			if err := trows.Scan(&tid, &bid); err != nil {
				trows.Close()
				return err
			}
			if loc, ok := locByBookID[strings.TrimSpace(bid)]; ok {
				bk := &seriesList[loc.seriesIdx].Books[loc.bookIdx]
				if bk.SelectedThumbnailID == "" {
					bk.SelectedThumbnailID = strings.TrimSpace(tid)
				}
			}
		}
		if err := trows.Err(); err != nil {
			trows.Close()
			return err
		}
		trows.Close()
	}
	return nil
}

func finalizeKomgaSeriesEntry(e *KomgaSeriesEntry) {
	earliestRelease := e.ReleaseDate
	for _, bk := range e.Books {
		for _, au := range bk.Authors {
			e.Authors = appendUniqueKomgaAuthor(e.Authors, au)
		}
		if bk.ReleaseDate != "" && (earliestRelease == "" || bk.ReleaseDate < earliestRelease) {
			earliestRelease = bk.ReleaseDate
		}
	}
	if e.ReleaseDate == "" {
		e.ReleaseDate = earliestRelease
	}
	e.NSFW = isKomgaSeriesNSFW(e)
}

func isKomgaSeriesNSFW(e *KomgaSeriesEntry) bool {
	if e.AgeRating != nil && *e.AgeRating >= 18 {
		return true
	}
	for _, label := range e.SharingLabels {
		if isNSFWKeyword(label) {
			return true
		}
	}
	for _, tag := range e.Tags {
		if isNSFWKeyword(tag) {
			return true
		}
	}
	for _, genre := range e.Genres {
		if isNSFWKeyword(genre) {
			return true
		}
	}
	return bangumi.InferPathNSFW(e.Name+" "+e.Title, e.URL)
}

func isNSFWKeyword(raw string) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return false
	}
	switch s {
	case "nsfw", "hentai", "18+", "r18", "r18+", "r-18", "x18", "x18+", "18禁",
		"adult", "adults", "adults only", "adults only 18+", "explicit", "erotica", "smut",
		"成人", "成人漫画", "成人向", "成年コミック", "エロ漫画", "h漫":
		return true
	}
	for _, kw := range []string{"nsfw", "hentai", "18+", "r18", "r-18", "x18", "18禁", "成年コミック", "成人漫画", "成人向", "エロ漫画"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// komgaCoverRating resolves the actual thumbnail owner and classifies its series.
func komgaCoverRating(ctx context.Context, db sqliteQueryer, kind, thumbID, entryID, parentEntryID string) (bool, error) {
	var ownerID, seriesID string
	var query string
	switch kind {
	case SQLiteBlobKindKomgaSeries:
		query = `SELECT SERIES_ID, SERIES_ID FROM THUMBNAIL_SERIES WHERE ID=?`
	case SQLiteBlobKindKomgaBook:
		query = `SELECT b.ID, b.SERIES_ID FROM THUMBNAIL_BOOK t JOIN BOOK b ON b.ID=t.BOOK_ID WHERE t.ID=?`
	default:
		return false, ErrInvalidConfig
	}
	if err := db.QueryRowContext(ctx, query, thumbID).Scan(&ownerID, &seriesID); err != nil {
		return false, err
	}
	if seriesID == "" || (entryID != "" && ownerID != entryID) || (parentEntryID != "" && seriesID != parentEntryID) {
		return false, ErrEntryNotFound
	}
	info, err := DetectKomgaSchema(ctx, db)
	if err != nil {
		return false, err
	}
	entries, err := queryKomgaSeries(ctx, db, info, seriesID, false)
	if err != nil {
		return false, err
	}
	if len(entries) != 1 {
		return false, ErrEntryNotFound
	}
	return entries[0].NSFW, nil
}

// ReadKomgaThumbnailBLOB lazily reads a single THUMBNAIL_SERIES or THUMBNAIL_BOOK BLOB
// strictly in read-only mode, enforcing a 16 MiB cap to prevent OOM.
func ReadKomgaThumbnailBLOB(ctx context.Context, dbPath, kind, thumbID string) ([]byte, string, error) {
	db, err := openKomgaReadOnlySQLite(dbPath)
	if err != nil {
		return nil, "", err
	}
	defer db.Close()
	return readKomgaThumbnailBLOB(ctx, db, kind, thumbID)
}

func readKomgaThumbnailBLOB(ctx context.Context, db sqliteQueryer, kind, thumbID string) ([]byte, string, error) {
	thumbID = strings.TrimSpace(thumbID)
	if thumbID == "" {
		return nil, "", ErrEntryNotFound
	}
	var table string
	switch kind {
	case SQLiteBlobKindKomgaSeries:
		table = "THUMBNAIL_SERIES"
	case SQLiteBlobKindKomgaBook:
		table = "THUMBNAIL_BOOK"
	default:
		return nil, "", fmt.Errorf("%w: unsupported thumbnail kind %q", ErrInvalidConfig, kind)
	}

	tables, err := listSQLiteTables(ctx, db)
	if err != nil {
		return nil, "", err
	}
	if !tables[strings.ToLower(table)] {
		return nil, "", ErrEntryNotFound
	}
	cols, err := tableColumns(ctx, db, table)
	if err != nil {
		return nil, "", err
	}
	if !cols["thumbnail"] || !cols["id"] {
		return nil, "", ErrEntryNotFound
	}
	mediaExpr := "''"
	if cols["media_type"] {
		mediaExpr = "COALESCE(MEDIA_TYPE, '')"
	}

	var (
		blobSize  sql.NullInt64
		mediaType string
		blob      []byte
	)
	query := fmt.Sprintf(`
SELECT LENGTH(THUMBNAIL), %s,
       CASE WHEN LENGTH(THUMBNAIL) <= ? THEN THUMBNAIL ELSE NULL END
FROM %s WHERE ID = ? LIMIT 1`, mediaExpr, table)
	if err := db.QueryRowContext(ctx, query, maxSQLiteBlobCoverBytes, thumbID).Scan(&blobSize, &mediaType, &blob); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrEntryNotFound
		}
		return nil, "", err
	}
	if blobSize.Valid && blobSize.Int64 > maxSQLiteBlobCoverBytes {
		return nil, "", fmt.Errorf("%w: thumbnail BLOB exceeds %d bytes limit", ErrInvalidImage, maxSQLiteBlobCoverBytes)
	}
	if len(blob) == 0 {
		return nil, "", ErrEntryNotFound
	}
	if _, detMIME, ok := DetectImageFormat(blob); ok {
		return blob, detMIME, nil
	}
	mediaType = strings.TrimSpace(mediaType)
	if strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return blob, mediaType, nil
	}
	return nil, "", fmt.Errorf("%w: unrecognized Komga thumbnail BLOB bytes", ErrInvalidImage)
}

func appendUniqueString(list []string, item string) []string {
	item = strings.TrimSpace(item)
	if item == "" {
		return list
	}
	for _, existing := range list {
		if strings.EqualFold(existing, item) {
			return list
		}
	}
	return append(list, item)
}

func appendUniqueKomgaAuthor(list []KomgaAuthor, item KomgaAuthor) []KomgaAuthor {
	key := strings.ToLower(item.Name) + "\x00" + strings.ToLower(item.Role)
	for _, existing := range list {
		if strings.ToLower(existing.Name)+"\x00"+strings.ToLower(existing.Role) == key {
			return list
		}
	}
	return append(list, item)
}

func tableColumns(ctx context.Context, db sqliteQueryer, tableName string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info("%s")`, strings.ReplaceAll(tableName, `"`, `""`)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := make(map[string]bool)
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err == nil {
			cols[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	return cols, rows.Err()
}

func isValidKomgaOpenSchemaTarget(s string) bool {
	return slices.Contains([]string{FormatKomgaSQLite, FormatOpenV1, FormatOpenV2, SchemaGalleryV1, FormatTagDictV1}, s)
}
