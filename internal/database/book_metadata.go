package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
)

type BookMetadata struct {
	BookID      string
	Title       string
	Summary     string
	Number      string
	NumberSort  *float64
	ReleaseDate string
	ISBN        string
	Authors     []Author
	Tags        []string
	Links       []Link

	TitleLock       bool
	SummaryLock     bool
	NumberLock      bool
	NumberSortLock  bool
	ReleaseDateLock bool
	ISBNLock        bool
	AuthorsLock     bool
	TagsLock        bool
	LinksLock       bool

	BgmSubjectID     int64
	BgmCoverURL      string
	BgmCoverPath     string
	BgmIsNSFW        bool
	ExtSourceID      string
	ExtEntryID       string
	ExtCoverURL      string
	ExtCoverPath     string
	CustomCoverPath  string
	CustomCoverURL   string
	CoverSource      string
	SeriesGroup      string
	StoryArc         string
	StoryArcNumber   string
	ComicInfoVersion string
	ComicInfoStatus  string
	MatchOrigin      string
	UpdatedAt        time.Time

	// Sparse rows also store unrelated bindings and covers. Track empty
	// overrides separately from the column defaults in those rows.
	explicitEmpty int
}

const (
	bookTitleOverride = 1 << iota
	bookNumberOverride
	bookReleaseDateOverride
	bookAuthorsOverride
	bookSummaryOverride
	bookISBNOverride
	bookTagsOverride
	bookLinksOverride
)

func (m BookMetadata) HasTitle() bool {
	return m.Title != "" || m.explicitEmpty&bookTitleOverride != 0
}

func (m BookMetadata) HasNumber() bool {
	return m.Number != "" || m.explicitEmpty&bookNumberOverride != 0
}

func (m BookMetadata) HasReleaseDate() bool {
	return m.ReleaseDate != "" || m.explicitEmpty&bookReleaseDateOverride != 0
}

func (m BookMetadata) HasAuthors() bool {
	return len(m.Authors) != 0 || m.explicitEmpty&bookAuthorsOverride != 0
}

func (m BookMetadata) HasSummary() bool {
	return m.Summary != "" || m.explicitEmpty&bookSummaryOverride != 0
}

func (m BookMetadata) HasISBN() bool {
	return m.ISBN != "" || m.explicitEmpty&bookISBNOverride != 0
}

func (m BookMetadata) HasTags() bool {
	return len(m.Tags) != 0 || m.explicitEmpty&bookTagsOverride != 0
}

func (m BookMetadata) HasLinks() bool {
	return len(m.Links) != 0 || m.explicitEmpty&bookLinksOverride != 0
}

type BookMetadataPatch struct {
	Title           *string   `json:"title"`
	Summary         *string   `json:"summary"`
	Number          *string   `json:"number"`
	NumberSort      *float64  `json:"numberSort"`
	ClearNumberSort bool      `json:"clearNumberSort"`
	ReleaseDate     *string   `json:"releaseDate"`
	ISBN            *string   `json:"isbn"`
	Authors         *[]Author `json:"authors"`
	Tags            *[]string `json:"tags"`
	Links           *[]Link   `json:"links"`

	TitleLock       *bool `json:"titleLock"`
	SummaryLock     *bool `json:"summaryLock"`
	NumberLock      *bool `json:"numberLock"`
	NumberSortLock  *bool `json:"numberSortLock"`
	ReleaseDateLock *bool `json:"releaseDateLock"`
	ISBNLock        *bool `json:"isbnLock"`
	AuthorsLock     *bool `json:"authorsLock"`
	TagsLock        *bool `json:"tagsLock"`
	LinksLock       *bool `json:"linksLock"`
}

type BookScrapedMetadata struct {
	Title       string
	Summary     string
	Number      string
	NumberSort  *float64
	ReleaseDate string
	ISBN        string
	Authors     []Author
	Tags        []string
	Links       []Link

	BgmSubjectID     int64
	BgmCoverURL      string
	BgmCoverPath     string
	BgmIsNSFW        bool
	ExtSourceID      string
	ExtEntryID       string
	ExtCoverURL      string
	ExtCoverPath     string
	SeriesGroup      string
	StoryArc         string
	StoryArcNumber   string
	ComicInfoVersion string
	ComicInfoStatus  string
	MatchOrigin      string
}

const bookMetadataColumns = `book_id,title,summary,number,number_sort,release_date,isbn,authors_json,tags_json,links_json,
title_lock,summary_lock,number_lock,number_sort_lock,release_date_lock,isbn_lock,authors_lock,tags_lock,links_lock,
bgm_subject_id,bgm_cover_url,bgm_cover_path,bgm_is_nsfw,ext_source_id,ext_entry_id,ext_cover_url,ext_cover_path,
custom_cover_path,custom_cover_url,cover_source,series_group,story_arc,story_arc_number,comicinfo_version,comicinfo_status,
match_origin,updated_at,explicit_empty`

func scanBookMetadata(row interface{ Scan(...any) error }) (BookMetadata, error) {
	var m BookMetadata
	var authors, tags, links, updated string
	err := row.Scan(&m.BookID, &m.Title, &m.Summary, &m.Number, &m.NumberSort, &m.ReleaseDate, &m.ISBN,
		&authors, &tags, &links, &m.TitleLock, &m.SummaryLock, &m.NumberLock, &m.NumberSortLock, &m.ReleaseDateLock,
		&m.ISBNLock, &m.AuthorsLock, &m.TagsLock, &m.LinksLock, &m.BgmSubjectID, &m.BgmCoverURL, &m.BgmCoverPath,
		&m.BgmIsNSFW, &m.ExtSourceID, &m.ExtEntryID, &m.ExtCoverURL, &m.ExtCoverPath, &m.CustomCoverPath,
		&m.CustomCoverURL, &m.CoverSource, &m.SeriesGroup, &m.StoryArc, &m.StoryArcNumber, &m.ComicInfoVersion,
		&m.ComicInfoStatus, &m.MatchOrigin, &updated, &m.explicitEmpty)
	if err != nil {
		return BookMetadata{}, err
	}
	_ = json.Unmarshal([]byte(authors), &m.Authors)
	_ = json.Unmarshal([]byte(tags), &m.Tags)
	_ = json.Unmarshal([]byte(links), &m.Links)
	m.UpdatedAt = parseTime(updated)
	return m, nil
}

func (s *Store) BookMetadataByID(ctx context.Context, bookID string) (BookMetadata, bool, error) {
	m, err := scanBookMetadata(s.db.QueryRowContext(ctx,
		`SELECT `+bookMetadataColumns+` FROM book_metadata WHERE book_id=?`, bookID))
	if errors.Is(err, sql.ErrNoRows) {
		return BookMetadata{}, false, nil
	}
	return m, err == nil, err
}

func (s *Store) BookMetadataMap(ctx context.Context, bookIDs []string) (map[string]BookMetadata, error) {
	out := make(map[string]BookMetadata)
	for _, chunk := range chunkStrings(bookIDs, 400) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT `+bookMetadataColumns+` FROM book_metadata WHERE book_id IN `+Placeholders(len(chunk)), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			m, err := scanBookMetadata(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[m.BookID] = m
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func patchBookField[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

func patchBookOverride[T any](dst, src *T, overrides *int, field int) {
	if src != nil {
		*dst = *src
		*overrides |= field
	}
}

func (s *Store) PatchBookMetadata(ctx context.Context, bookID string, patch BookMetadataPatch) error {
	return s.updateBookMetadata(ctx, bookID, func(m *BookMetadata) error {
		patchBookOverride(&m.Title, patch.Title, &m.explicitEmpty, bookTitleOverride)
		patchBookOverride(&m.Summary, patch.Summary, &m.explicitEmpty, bookSummaryOverride)
		patchBookOverride(&m.Number, patch.Number, &m.explicitEmpty, bookNumberOverride)
		patchBookOverride(&m.ReleaseDate, patch.ReleaseDate, &m.explicitEmpty, bookReleaseDateOverride)
		patchBookOverride(&m.ISBN, patch.ISBN, &m.explicitEmpty, bookISBNOverride)
		patchBookOverride(&m.Authors, patch.Authors, &m.explicitEmpty, bookAuthorsOverride)
		patchBookOverride(&m.Tags, patch.Tags, &m.explicitEmpty, bookTagsOverride)
		patchBookOverride(&m.Links, patch.Links, &m.explicitEmpty, bookLinksOverride)
		if patch.ClearNumberSort {
			m.NumberSort = nil
		} else if patch.NumberSort != nil {
			m.NumberSort = patch.NumberSort
		}
		patchBookField(&m.TitleLock, patch.TitleLock)
		patchBookField(&m.SummaryLock, patch.SummaryLock)
		patchBookField(&m.NumberLock, patch.NumberLock)
		patchBookField(&m.NumberSortLock, patch.NumberSortLock)
		patchBookField(&m.ReleaseDateLock, patch.ReleaseDateLock)
		patchBookField(&m.ISBNLock, patch.ISBNLock)
		patchBookField(&m.AuthorsLock, patch.AuthorsLock)
		patchBookField(&m.TagsLock, patch.TagsLock)
		patchBookField(&m.LinksLock, patch.LinksLock)
		return nil
	})
}

func (s *Store) UpsertBookScrapedMetadata(ctx context.Context, bookID string, meta BookScrapedMetadata) error {
	return s.updateBookMetadata(ctx, bookID, func(m *BookMetadata) error {
		if automated, _ := ctx.Value(automaticMetadataKey{}).(bool); automated &&
			(m.MatchOrigin == "manual" || m.MatchOrigin == "agent" ||
				(m.MatchOrigin == "" && (m.BgmSubjectID > 0 || m.ExtEntryID != ""))) {
			return ErrMetadataChanged
		}
		for _, field := range []struct {
			dst    *string
			value  string
			locked bool
		}{
			{&m.Title, meta.Title, m.TitleLock},
			{&m.Summary, meta.Summary, m.SummaryLock},
			{&m.Number, meta.Number, m.NumberLock},
			{&m.ReleaseDate, meta.ReleaseDate, m.ReleaseDateLock},
			{&m.ISBN, meta.ISBN, m.ISBNLock},
			{&m.SeriesGroup, meta.SeriesGroup, false},
			{&m.StoryArc, meta.StoryArc, false},
			{&m.StoryArcNumber, meta.StoryArcNumber, false},
			{&m.MatchOrigin, meta.MatchOrigin, false},
		} {
			if value := strings.TrimSpace(field.value); value != "" && !field.locked {
				*field.dst = value
			}
		}
		if !m.NumberSortLock && meta.NumberSort != nil {
			m.NumberSort = meta.NumberSort
		}
		if !m.AuthorsLock && meta.Authors != nil {
			m.Authors = meta.Authors
			m.explicitEmpty |= bookAuthorsOverride
		}
		if !m.TagsLock && meta.Tags != nil {
			m.Tags = meta.Tags
			m.explicitEmpty |= bookTagsOverride
		}
		if !m.LinksLock && meta.Links != nil {
			m.Links = meta.Links
			m.explicitEmpty |= bookLinksOverride
		}
		if meta.BgmSubjectID > 0 {
			if m.BgmSubjectID != meta.BgmSubjectID || (meta.BgmCoverURL != "" && m.BgmCoverURL != meta.BgmCoverURL) {
				m.BgmCoverPath = ""
			}
			if m.BgmSubjectID != meta.BgmSubjectID || meta.BgmCoverURL != "" {
				m.BgmCoverURL = meta.BgmCoverURL
			}
			if meta.BgmCoverPath != "" {
				m.BgmCoverPath = meta.BgmCoverPath
			}
			m.BgmSubjectID, m.BgmIsNSFW = meta.BgmSubjectID, meta.BgmIsNSFW
		}
		if meta.ExtSourceID != "" {
			if m.ExtSourceID != meta.ExtSourceID || m.ExtEntryID != meta.ExtEntryID || m.ExtCoverURL != meta.ExtCoverURL {
				m.ExtCoverPath = ""
			}
			m.ExtSourceID, m.ExtEntryID, m.ExtCoverURL = meta.ExtSourceID, meta.ExtEntryID, meta.ExtCoverURL
			if meta.ExtCoverPath != "" {
				m.ExtCoverPath = meta.ExtCoverPath
			}
		}
		if meta.ComicInfoVersion != "" || meta.ComicInfoStatus != "" {
			m.ComicInfoVersion, m.ComicInfoStatus = meta.ComicInfoVersion, meta.ComicInfoStatus
		}
		return nil
	})
}

func (s *Store) SetBookCustomCover(ctx context.Context, bookID, coverPath, coverURL string) error {
	return s.updateBookMetadata(ctx, bookID, func(m *BookMetadata) error {
		m.CustomCoverPath, m.CustomCoverURL = coverPath, coverURL
		return nil
	})
}

func (s *Store) ClearBookCustomCover(ctx context.Context, bookID string) error {
	return s.updateBookMetadata(ctx, bookID, func(m *BookMetadata) error {
		m.CustomCoverPath, m.CustomCoverURL = "", ""
		if m.CoverSource == "custom" {
			m.CoverSource = CoverSourceAuto
		}
		return nil
	})
}

func (s *Store) SetBookCoverSource(ctx context.Context, bookID, source string) error {
	switch source {
	case CoverSourceAuto, CoverSourceBangumi, CoverSourceBook, "custom", "ext":
		return s.updateBookMetadata(ctx, bookID, func(m *BookMetadata) error {
			m.CoverSource = source
			return nil
		})
	default:
		return errors.New("invalid book cover source")
	}
}

func (s *Store) SetBookComicInfoSentinel(ctx context.Context, bookID, version, status string) error {
	switch status {
	case "", "parsed", "absent", "skipped_solid", "skipped_encrypted", "skipped_unsupported":
		return s.updateBookMetadata(ctx, bookID, func(m *BookMetadata) error {
			m.ComicInfoVersion, m.ComicInfoStatus = version, status
			return nil
		})
	default:
		return errors.New("invalid ComicInfo status")
	}
}

func (s *Store) updateBookMetadata(ctx context.Context, bookID string, update func(*BookMetadata) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	m, err := scanBookMetadata(tx.QueryRowContext(ctx,
		`SELECT `+bookMetadataColumns+` FROM book_metadata WHERE book_id=?`, bookID))
	if errors.Is(err, sql.ErrNoRows) {
		result, err := tx.ExecContext(ctx, `INSERT INTO book_metadata(book_id) SELECT id FROM books WHERE id=?`, bookID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return sql.ErrNoRows
		}
		m = BookMetadata{CoverSource: CoverSourceAuto}
	} else if err != nil {
		return err
	}
	if err := update(&m); err != nil {
		return err
	}
	if m.NumberSort != nil && (math.IsInf(*m.NumberSort, 0) || math.IsNaN(*m.NumberSort)) {
		return errors.New("book number sort must be finite")
	}
	authors, _ := json.Marshal(cleanAuthors(m.Authors))
	tags, _ := json.Marshal(cleanStringSlice(m.Tags))
	links, _ := json.Marshal(cleanWebLinks(m.Links))
	if _, err := tx.ExecContext(ctx, `
UPDATE book_metadata SET title=?,summary=?,number=?,number_sort=?,release_date=?,isbn=?,authors_json=?,tags_json=?,links_json=?,
 title_lock=?,summary_lock=?,number_lock=?,number_sort_lock=?,release_date_lock=?,isbn_lock=?,authors_lock=?,tags_lock=?,links_lock=?,
 bgm_subject_id=?,bgm_cover_url=?,bgm_cover_path=?,bgm_is_nsfw=?,ext_source_id=?,ext_entry_id=?,ext_cover_url=?,ext_cover_path=?,
 custom_cover_path=?,custom_cover_url=?,cover_source=?,series_group=?,story_arc=?,story_arc_number=?,comicinfo_version=?,comicinfo_status=?,
 match_origin=?,updated_at=?,explicit_empty=? WHERE book_id=?`,
		m.Title, m.Summary, m.Number, m.NumberSort, m.ReleaseDate, m.ISBN, string(authors), string(tags), string(links),
		m.TitleLock, m.SummaryLock, m.NumberLock, m.NumberSortLock, m.ReleaseDateLock, m.ISBNLock, m.AuthorsLock, m.TagsLock, m.LinksLock,
		m.BgmSubjectID, m.BgmCoverURL, m.BgmCoverPath, m.BgmIsNSFW, m.ExtSourceID, m.ExtEntryID, m.ExtCoverURL, m.ExtCoverPath,
		m.CustomCoverPath, m.CustomCoverURL, m.CoverSource, m.SeriesGroup, m.StoryArc, m.StoryArcNumber, m.ComicInfoVersion,
		m.ComicInfoStatus, m.MatchOrigin, nowText(), m.explicitEmpty, bookID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.wakeSearchFTSWorker()
	return nil
}
