package database

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
)

type ChainBookMatch struct {
	BookID   string
	Metadata BookScrapedMetadata
	Bindings []BookSourceBinding
	Covers   []BookCoverCandidate
}

type ChainSeriesMatch struct {
	Metadata          ScrapedMetadata
	Bindings          []SeriesSourceBinding
	Covers            []SeriesCoverCandidate
	Books             []ChainBookMatch
	Replace           bool
	MarkUnmatched     bool
	ExpectedSubjectID *int64
}

// ApplyMetadataChain commits a series projection, its aligned books, and sparse
// source data together. Imported candidates survive when a source goes away.
func (s *Store) ApplyMetadataChain(ctx context.Context, series Series, match ChainSeriesMatch) (SeriesMetadata, []string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SeriesMetadata{}, nil, err
	}
	defer tx.Rollback()
	if match.Replace {
		ctx = context.WithValue(ctx, rematchLegacyKey{}, true)
	}
	current, err := s.applyScrapedMetadataTx(ctx, tx, series, match.Metadata, true, match.Replace, match.ExpectedSubjectID, match.MarkUnmatched)
	if err != nil {
		return SeriesMetadata{}, nil, err
	}
	if err := s.ReplaceSeriesSourceBindingsTx(ctx, tx, series.ID, match.Bindings); err != nil {
		return SeriesMetadata{}, nil, err
	}
	var refs []string
	for _, candidate := range match.Covers {
		if err := collectReplacedCover(ctx, tx, "series", series.ID, candidate.SourceID, candidate.CoverRef, &refs); err != nil {
			return SeriesMetadata{}, nil, err
		}
	}
	if match.Replace {
		if _, err := tx.ExecContext(ctx, "DELETE FROM series_cover_candidates WHERE series_id=? AND cover_kind<>'imported_file' AND "+automaticSeriesCoverCandidates, series.ID); err != nil {
			return SeriesMetadata{}, nil, err
		}
	}
	if len(match.Covers) > 0 {
		if err := upsertSeriesCoverCandidatesTx(ctx, tx, match.Covers); err != nil {
			return SeriesMetadata{}, nil, err
		}
	}
	if err := applyChainBooks(ctx, tx, series.ID, match, &refs); err != nil {
		return SeriesMetadata{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return SeriesMetadata{}, nil, err
	}
	if len(match.Books) > 0 || match.Replace {
		s.NotifyBookSearchDirty()
	}
	return current, refs, nil
}

func collectReplacedCover(ctx context.Context, tx *sql.Tx, entity, id, sourceID, newRef string, refs *[]string) error {
	var old string
	err := tx.QueryRowContext(ctx, "SELECT cover_ref FROM "+entity+"_cover_candidates WHERE "+entity+"_id=? AND source_id=? AND cover_kind='imported_file' AND cover_ref<>?", id, sourceID, newRef).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err == nil {
		*refs = append(*refs, old)
	}
	return err
}

func applyChainBooks(ctx context.Context, tx *sql.Tx, seriesID string, match ChainSeriesMatch, refs *[]string) error {
	if len(match.Books) == 0 && !match.Replace {
		return nil
	}
	books, err := tx.QueryContext(ctx, "SELECT "+qualifyMetadataColumns("m", bookMetadataColumns)+" FROM books b CROSS JOIN book_metadata m ON m.book_id=b.id WHERE b.series_id=?", seriesID)
	if err != nil {
		return err
	}
	existing := make(map[string]BookMetadata)
	for books.Next() {
		meta, err := scanBookMetadata(books)
		if err != nil {
			books.Close()
			return err
		}
		existing[meta.BookID] = meta
	}
	err = books.Err()
	books.Close()
	if err != nil {
		return err
	}

	if match.Replace {
		for _, query := range []string{
			`DELETE FROM book_source_bindings WHERE match_origin NOT IN ('manual','agent') AND book_id IN (
   SELECT b.id FROM books b LEFT JOIN book_metadata m ON m.book_id=b.id WHERE b.series_id=? AND coalesce(m.match_origin,'') NOT IN ('manual','agent'))`,
			`DELETE FROM book_cover_candidates WHERE cover_kind<>'imported_file' AND source_id NOT IN ('manual','agent') AND book_id IN (
   SELECT b.id FROM books b LEFT JOIN book_metadata m ON m.book_id=b.id WHERE b.series_id=? AND coalesce(m.match_origin,'') NOT IN ('manual','agent'))`,
		} {
			if _, err := tx.ExecContext(ctx, query, seriesID); err != nil {
				return err
			}
		}
	}
	stmt, err := tx.PrepareContext(ctx, upsertBookMetadataSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := nowText()
	var bindings []BookSourceBinding
	var covers []BookCoverCandidate
	automated, _ := ctx.Value(automaticMetadataKey{}).(bool)
	for _, book := range match.Books {
		current, ok := existing[book.BookID]
		if !ok {
			current = BookMetadata{BookID: book.BookID}
		}
		legacy := automated && current.MatchOrigin == "" && (current.BgmSubjectID > 0 || current.ExtEntryID != "")
		if automated && (current.MatchOrigin == "manual" || current.MatchOrigin == "agent" || (legacy && !match.Replace)) {
			for _, c := range book.Covers {
				if c.CoverKind == CoverKindImportedFile {
					*refs = append(*refs, c.CoverRef)
				}
			}
			continue
		}
		delete(existing, book.BookID)
		original := current
		if match.Replace && !legacy && ok {
			resetBookMatchFields(&current)
		}
		incoming := book.Metadata
		if legacy {
			incoming.MatchOrigin = ""
		}
		applyBookScrapedFields(&current, incoming, legacy)
		if current.NumberSort != nil && (math.IsNaN(*current.NumberSort) || math.IsInf(*current.NumberSort, 0)) {
			return errors.New("book number sort must be finite")
		}
		if !ok || !reflect.DeepEqual(original, current) {
			if err := execBookMetadataUpsert(ctx, stmt, current, now); err != nil {
				return err
			}
		}
		bindings = append(bindings, book.Bindings...)
		for _, candidate := range book.Covers {
			if err := collectReplacedCover(ctx, tx, "book", book.BookID, candidate.SourceID, candidate.CoverRef, refs); err != nil {
				return err
			}
		}
		covers = append(covers, book.Covers...)
	}
	if match.Replace {
		for _, current := range existing {
			if current.ExtEntryID == "" || current.MatchOrigin == "" || current.MatchOrigin == "manual" || current.MatchOrigin == "agent" {
				continue
			}
			if current.BgmSubjectID > 0 {
				current.ExtSourceID, current.ExtEntryID, current.ExtCoverURL, current.ExtCoverPath = "", "", "", ""
			} else {
				resetBookMatchFields(&current)
			}
			if err := execBookMetadataUpsert(ctx, stmt, current, now); err != nil {
				return err
			}
		}
	}
	if len(bindings) > 0 {
		if err := upsertBookSourceBindingsTx(ctx, tx, bindings); err != nil {
			return err
		}
	}
	if len(covers) > 0 {
		return upsertBookCoverCandidatesTx(ctx, tx, covers)
	}
	return nil
}

func resetUnlocked[T any](field *T, locked bool) {
	if !locked {
		var zero T
		*field = zero
	}
}

func resetSeriesMatchFields(m *SeriesMetadata) {
	resetUnlocked(&m.Title, m.TitleLock)
	resetUnlocked(&m.TitleSort, m.TitleSortLock)
	resetUnlocked(&m.Summary, m.SummaryLock)
	resetUnlocked(&m.Status, m.StatusLock)
	resetUnlocked(&m.Publisher, m.PublisherLock)
	resetUnlocked(&m.Language, m.LanguageLock)
	resetUnlocked(&m.ReadingDirection, m.ReadingDirLock)
	resetUnlocked(&m.ReleaseDate, m.ReleaseDateLock)
	resetUnlocked(&m.TotalBookCount, m.TotalBookCountLock)
	resetUnlocked(&m.AgeRating, m.AgeRatingLock)
	resetUnlocked(&m.Genres, m.GenresLock)
	resetUnlocked(&m.Tags, m.TagsLock)
	resetUnlocked(&m.Authors, m.AuthorsLock)
	resetUnlocked(&m.Links, m.LinksLock)
	resetUnlocked(&m.SharingLabels, m.SharingLabelsLock)
	resetUnlocked(&m.AlternateTitles, m.AlternateTitlesLock)
}

func resetBookMatchFields(m *BookMetadata) {
	resetUnlocked(&m.Title, m.TitleLock)
	resetUnlocked(&m.Summary, m.SummaryLock)
	resetUnlocked(&m.Number, m.NumberLock)
	resetUnlocked(&m.NumberSort, m.NumberSortLock)
	resetUnlocked(&m.ReleaseDate, m.ReleaseDateLock)
	resetUnlocked(&m.ISBN, m.ISBNLock)
	resetUnlocked(&m.Authors, m.AuthorsLock)
	resetUnlocked(&m.Tags, m.TagsLock)
	resetUnlocked(&m.Links, m.LinksLock)
	if !m.AuthorsLock {
		m.explicitEmpty &^= bookAuthorsOverride
	}
	if !m.TagsLock {
		m.explicitEmpty &^= bookTagsOverride
	}
	if !m.LinksLock {
		m.explicitEmpty &^= bookLinksOverride
	}
	m.ExtSourceID, m.ExtEntryID, m.ExtCoverURL, m.ExtCoverPath = "", "", "", ""
}
