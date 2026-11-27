package extmeta

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/database"
)

type offlineHit struct {
	source   database.MetadataSource
	sourceID string
	rank     int
	entry    *Entry
	meta     database.ScrapedMetadata
}

func (m *Manager) matchOffline(ctx context.Context, target scrapeSeries, plan scrapePlan, force bool) ([]offlineHit, error) {
	series := target.series
	chain := plan.chain(series.LibraryID)
	parsed := bangumi.ParseSeriesWithPath(series.Name, series.RelativePath)
	titles := append(slices.Clone(parsed.Candidates), series.Name)
	var hits []offlineHit
	var embedded *EmbeddedMeta
	readCached := false
	anchorInChain := false
	for rank, link := range chain {
		if !link.Enabled {
			continue
		}
		src := plan.sources[link.SourceID]
		if link.SourceID == TargetSourceBangumi || src.Kind == KindBangumiArchive {
			if m.bangumi == nil || !m.bangumi.ArchiveStatus().Ready || (link.SourceID == TargetSourceBangumi && plan.bangumiDisabled) || (link.SourceID != TargetSourceBangumi && !src.Ready) {
				continue
			}
			anchorInChain = true
			meta, err := m.matchOfflineBangumi(ctx, target, parsed, plan, force)
			if err != nil {
				return nil, err
			}
			if meta.SubjectID > 0 {
				hits = append(hits, offlineHit{source: src, sourceID: link.SourceID, rank: rank, meta: meta})
			}
			continue
		}
		if !plan.readySidecars[link.SourceID] {
			continue
		}
		if !readCached {
			embedded = m.cachedEmbeddedMetaFromDB(ctx, series.ID)
			readCached = true
		}
		entry, confidence, err := m.matchSidecar(ctx, src.ID, titles, parsed.Authors, target.coverSHA1, embedded)
		if err != nil {
			return nil, err
		}
		origin := "auto"
		if entry == nil {
			entry, err = m.mappedOfflineEntry(ctx, series, parsed, src, plan)
			if err != nil {
				return nil, err
			}
			origin, confidence = "mapping", 1
		}
		if entry != nil {
			hits = append(hits, offlineHit{source: src, sourceID: src.ID, rank: rank, entry: entry, meta: m.externalMetadata(ctx, src, entry, origin, confidence)})
		}
	}
	// An existing subject is an identity anchor even when Bangumi is absent
	// from the new chain. Its projection survives without taking chain priority.
	if force && target.subjectID > 0 && !anchorInChain && m.bangumi != nil {
		meta, found, err := m.bangumi.OfflineSubject(ctx, series, target.subjectID)
		if err != nil {
			return nil, err
		}
		if found {
			meta.MatchOrigin = "auto"
			hits = append(hits, offlineHit{sourceID: TargetSourceBangumi, rank: -1, meta: meta})
		}
	}
	return hits, nil
}

func (m *Manager) matchOfflineBangumi(ctx context.Context, target scrapeSeries, parsed bangumi.ParsedQuery, plan scrapePlan, force bool) (database.ScrapedMetadata, error) {
	if force && target.subjectID > 0 {
		meta, _, err := m.bangumi.OfflineSubject(ctx, target.series, target.subjectID)
		meta.MatchOrigin = "auto"
		return meta, err
	}
	meta, found, err := m.bangumi.OfflineMatch(ctx, target.series)
	if found || err != nil {
		meta.MatchOrigin = "auto"
		return meta, err
	}
	for _, author := range mappingAuthors(parsed) {
		rec, ok := plan.mappings.Lookup(mappingTitle(target.series, parsed), target.series.Name, author, TargetSourceBangumi)
		if !ok {
			continue
		}
		id, err := strconv.ParseInt(rec.TargetID, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		meta, found, err = m.bangumi.OfflineSubject(ctx, target.series, id)
		if found || err != nil {
			meta.MatchOrigin = "mapping"
			return meta, err
		}
	}
	return database.ScrapedMetadata{}, nil
}

func mappingTitle(series database.Series, parsed bangumi.ParsedQuery) string {
	return firstNonEmpty(append(slices.Clone(parsed.Candidates), series.Name)...)
}

func mappingAuthors(parsed bangumi.ParsedQuery) []string {
	if len(parsed.Authors) == 0 {
		return []string{""}
	}
	return parsed.Authors
}

func (m *Manager) mappedOfflineEntry(ctx context.Context, series database.Series, parsed bangumi.ParsedQuery, src database.MetadataSource, plan scrapePlan) (*Entry, error) {
	schema := src.SchemaType
	for candidate, ids := range plan.bySchema {
		if slices.Contains(ids, src.ID) {
			schema = candidate
			break
		}
	}
	schemas := []string{schema}
	switch schema {
	case SchemaMangaAlphaMetaV1:
		schemas = append(schemas, SchemaMangaAlphaMetaV2)
	case SchemaMangaAlphaMetaV2:
		schemas = append(schemas, SchemaMangaAlphaMetaV1)
	}
	for _, author := range mappingAuthors(parsed) {
		for _, target := range schemas {
			rec, ok := plan.mappings.Lookup(mappingTitle(series, parsed), series.Name, author, target)
			if !ok {
				continue
			}
			entry, err := m.entry(ctx, src.ID, rec.TargetID)
			if errors.Is(err, ErrEntryNotFound) {
				continue
			}
			return entry, err
		}
	}
	return nil, nil
}

func (m *Manager) applyOfflineHits(ctx context.Context, series database.Series, hits []offlineHit, replace bool, expectedSubjectID *int64) (database.ScrapedMetadata, database.SeriesMetadata, error) {
	locals, err := m.localSeriesBooks(ctx, series.ID)
	if err != nil {
		return database.ScrapedMetadata{}, database.SeriesMetadata{}, err
	}
	m.importedMu.Lock()
	defer m.importedMu.Unlock()
	m.mu.RLock()
	defer m.mu.RUnlock()
	match, created, err := m.buildChainMatch(ctx, series, locals, hits, replace)
	if err != nil {
		m.removeUnusedImportedCovers(ctx, created)
		return match.Metadata, database.SeriesMetadata{}, err
	}
	match.ExpectedSubjectID = expectedSubjectID
	saved, old, err := m.store.ApplyMetadataChain(ctx, series, match)
	if err == nil {
		m.removeUnusedImportedCovers(ctx, old)
	} else {
		m.removeUnusedImportedCovers(ctx, created)
	}
	if err == nil {
		saved, err = m.store.SeriesMetadataByID(ctx, series)
	}
	return match.Metadata, saved, err
}

func (m *Manager) buildChainMatch(ctx context.Context, series database.Series, locals []localBookCandidate, hits []offlineHit, replace bool) (database.ChainSeriesMatch, []string, error) {
	match := database.ChainSeriesMatch{Replace: replace}
	books := make(map[string]*database.ChainBookMatch, len(locals))
	var created []string
	var defaultTitle, defaultSort, defaultStatus string
	for _, hit := range hits {
		meta := hit.meta
		if hit.entry == nil {
			if match.Metadata.SubjectID == 0 {
				match.Metadata.SubjectID, match.Metadata.CoverURL = meta.SubjectID, meta.CoverURL
				match.Metadata.Score, match.Metadata.Rank = meta.Score, meta.Rank
			}
			match.Metadata.IsNSFW = match.Metadata.IsNSFW || meta.IsNSFW
			if hit.rank < 0 {
				continue
			}
			entryID := strconv.FormatInt(meta.SubjectID, 10)
			match.Bindings = append(match.Bindings, database.SeriesSourceBinding{SeriesID: series.ID, SourceID: hit.sourceID, EntryID: entryID, MatchOrigin: meta.MatchOrigin, Confidence: meta.Confidence, SourceGeneration: sourceGeneration(hit.source)})
			if meta.CoverURL != "" {
				match.Covers = append(match.Covers, seriesCandidate(series.ID, hit, entryID, database.CoverKindBangumi, meta.CoverURL))
			}
			if err := m.mergeBangumiBooks(ctx, series, locals, books, hit); err != nil {
				return match, created, err
			}
		} else {
			cover, aligned, refs, err := m.loadOfflineSourceMatch(ctx, hit, locals)
			created = append(created, refs...)
			if err != nil {
				return match, created, err
			}
			if aligned.komga != nil {
				ks := aligned.komga
				meta.Authors = append(meta.Authors, komgaAuthors(ks.Authors)...)
				meta.ReleaseDate = firstNonEmpty(meta.ReleaseDate, ks.ReleaseDate)
				if ks.DefaultTitle {
					defaultTitle = firstNonEmpty(defaultTitle, meta.Title)
					if strings.EqualFold(strings.TrimSpace(meta.TitleSort), strings.TrimSpace(meta.Title)) {
						defaultSort = firstNonEmpty(defaultSort, meta.TitleSort)
						meta.TitleSort = ""
					}
					meta.Title = ""
				}
				if ks.DefaultStatus {
					defaultStatus = firstNonEmpty(defaultStatus, meta.Status)
					meta.Status = ""
				}
			}
			if cover.CoverRef != "" {
				candidate := seriesCandidate(series.ID, hit, meta.ExtEntryID, cover.CoverKind, cover.CoverRef)
				candidate.NSFWKnown = cover.NSFWKnown
				match.Covers = append(match.Covers, candidate)
				if cover.CoverKind == database.CoverKindHTTPURL {
					meta.ExtCoverURL = cover.CoverRef
				}
			}
			match.Bindings = append(match.Bindings, database.SeriesSourceBinding{SeriesID: series.ID, SourceID: hit.sourceID, EntryID: meta.ExtEntryID, MatchOrigin: meta.MatchOrigin, Confidence: meta.Confidence, SourceGeneration: sourceGeneration(hit.source)})
			for _, item := range aligned.books {
				mergeChainBook(books, item)
			}
		}
		mergeSeriesFields(&match.Metadata, meta)
	}
	match.Metadata.Title = firstNonEmpty(match.Metadata.Title, defaultTitle)
	if match.Metadata.TitleSort == "" && match.Metadata.Title == defaultTitle {
		match.Metadata.TitleSort = defaultSort
	}
	match.Metadata.Status = firstNonEmpty(match.Metadata.Status, defaultStatus)
	for _, local := range locals {
		if book := books[local.ID]; book != nil {
			match.Books = append(match.Books, *book)
		}
	}
	return match, created, nil
}

func sourceGeneration(src database.MetadataSource) string {
	return firstNonEmpty(src.LocalSHA256, src.RemoteETag, src.LastUpdatedAt)
}

func seriesCandidate(seriesID string, hit offlineHit, entryID, kind, ref string) database.SeriesCoverCandidate {
	return database.SeriesCoverCandidate{SeriesID: seriesID, SourceID: hit.sourceID, EntryID: entryID,
		CoverKind: kind, CoverRef: ref, IsNSFW: hit.meta.IsNSFW, NSFWKnown: true, RankOrder: hit.rank, SourceGeneration: sourceGeneration(hit.source)}
}

func mergeSeriesFields(dst *database.ScrapedMetadata, src database.ScrapedMetadata) {
	dst.Title = firstNonEmpty(dst.Title, src.Title)
	dst.TitleSort = firstNonEmpty(dst.TitleSort, src.TitleSort)
	dst.Summary = firstNonEmpty(dst.Summary, src.Summary)
	dst.Publisher = firstNonEmpty(dst.Publisher, src.Publisher)
	dst.Language = firstNonEmpty(dst.Language, src.Language)
	dst.ReleaseDate = firstNonEmpty(dst.ReleaseDate, src.ReleaseDate)
	switch strings.ToUpper(strings.TrimSpace(src.Status)) {
	case "ONGOING", "ENDED", "ABANDONED", "HIATUS":
		dst.Status = firstNonEmpty(dst.Status, src.Status)
	}
	switch strings.ToUpper(strings.TrimSpace(src.ReadingDirection)) {
	case "LEFT_TO_RIGHT", "RIGHT_TO_LEFT", "VERTICAL", "WEBTOON":
		dst.ReadingDirection = firstNonEmpty(dst.ReadingDirection, src.ReadingDirection)
	}
	if dst.TotalBookCount == nil && src.TotalBookCount != nil && *src.TotalBookCount > 0 {
		dst.TotalBookCount = src.TotalBookCount
	}
	if dst.AgeRating == nil && src.AgeRating != nil && *src.AgeRating > 0 {
		dst.AgeRating = src.AgeRating
	}
	dst.Genres = append(dst.Genres, src.Genres...)
	dst.Tags = append(dst.Tags, src.Tags...)
	dst.Authors = append(dst.Authors, src.Authors...)
	dst.Links = append(dst.Links, src.Links...)
	dst.AlternateTitles = append(dst.AlternateTitles, src.AlternateTitles...)
	dst.SharingLabels = append(dst.SharingLabels, src.SharingLabels...)
	if dst.ExtSourceID == "" && src.ExtSourceID != "" {
		dst.ExtSourceID, dst.ExtEntryID, dst.ExtCoverURL = src.ExtSourceID, src.ExtEntryID, src.ExtCoverURL
	}
	if dst.MatchOrigin == "" {
		dst.MatchOrigin, dst.Confidence = src.MatchOrigin, src.Confidence
	}
	dst.IsNSFW = dst.IsNSFW || src.IsNSFW
}

type alignedOfflineSource struct {
	komga *KomgaSeriesEntry
	books []database.ChainBookMatch
}

func (m *Manager) loadOfflineSourceMatch(ctx context.Context, hit offlineHit, locals []localBookCandidate) (database.SeriesCoverCandidate, alignedOfflineSource, []string, error) {
	var aligned alignedOfflineSource
	var cover database.SeriesCoverCandidate
	var created []string
	src, err := m.store.GetMetadataSource(ctx, hit.sourceID)
	if err != nil {
		return cover, aligned, nil, err
	}
	sidecar := m.sidecars[hit.sourceID]
	if !src.Enabled || !src.Ready || sidecar == nil || sidecar.upstreamDB == nil ||
		sourceGeneration(src) != sourceGeneration(hit.source) || src.ImportCovers != hit.source.ImportCovers {
		return cover, aligned, nil, ErrUnavailable
	}
	tx, err := sidecar.upstreamDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return cover, aligned, nil, err
	}
	defer tx.Rollback()
	entry := hit.entry
	seriesID := entry.EntryID
	var remotes []remoteBookEntry
	capture := func(raw []byte, err error) (string, error) {
		if errors.Is(err, ErrEntryNotFound) || errors.Is(err, ErrInvalidImage) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		ref, err := writeImportedCover(m.importedCovers, raw)
		if errors.Is(err, ErrInvalidImage) {
			return "", nil
		}
		if err == nil {
			created = append(created, ref)
		}
		return ref, err
	}
	importKomga := func(kind, id string) (string, error) {
		raw, _, err := readKomgaThumbnailBLOB(ctx, tx, kind, id)
		return capture(raw, err)
	}
	importOpen := func(kind, bookKey string) (string, error) {
		raw, _, err := readOpenCoverBLOB(ctx, tx, kind, seriesID, bookKey)
		return capture(raw, err)
	}
	switch sidecar.Schema() {
	case SchemaKomgaSQLiteV1:
		info, err := DetectKomgaSchema(ctx, tx)
		if err != nil {
			return cover, aligned, created, err
		}
		ks, err := readKomgaSeries(ctx, tx, info, seriesID, true, src.ImportCovers)
		if err != nil {
			return cover, aligned, created, err
		}
		if ks.NSFW != entry.IsNSFW {
			return cover, aligned, created, ErrUnavailable
		}
		aligned.komga = ks
		if src.ImportCovers {
			raw, _, err := readKomgaSeriesThumbnail(ctx, tx, ks)
			cover.CoverRef, err = capture(raw, err)
			if err != nil {
				return cover, aligned, created, err
			}
		}
		for _, b := range ks.Books {
			remotes = append(remotes, remoteBookEntry{EntryID: b.KomgaBookID, FileNames: []string{b.Name, komgaPathTail(b.URL)},
				Title: b.Title, DefaultTitle: b.DefaultTitle, Summary: b.Summary, Number: b.Number, NumberSort: b.NumberSort,
				ReleaseDate: b.ReleaseDate, ISBN: b.ISBN, Authors: komgaAuthors(b.Authors), Tags: b.Tags,
				Links:       copyLinks(b.Links, func(l KomgaLink) database.Link { return database.Link{Label: l.Label, URL: l.URL} }),
				ThumbnailID: b.SelectedThumbnailID})
		}
	case SchemaMangaAlphaMetaV1, SchemaMangaAlphaMetaV2:
		snapshot, err := getMangaAlphaMetaV1Entry(ctx, tx, src.ID, seriesID)
		if err != nil {
			return cover, aligned, created, err
		}
		if snapshot.IsNSFW != entry.IsNSFW {
			return cover, aligned, created, ErrUnavailable
		}
		if src.ImportCovers && snapshot.HasCoverBlob {
			cover.CoverRef, err = importOpen(SQLiteBlobKindOpenEntry, "")
			if err != nil {
				return cover, aligned, created, err
			}
		}
		rows, err := ReadOpenOfflineBooks(ctx, tx, seriesID)
		if err != nil {
			return cover, aligned, created, err
		}
		for _, b := range rows {
			authors := make([]database.Author, 0, len(b.Authors))
			for _, a := range b.Authors {
				authors = append(authors, database.Author(a))
			}
			remotes = append(remotes, remoteBookEntry{EntryID: b.BookKey, FileNames: []string{b.FileName, b.BookKey},
				Title: b.Title, Summary: b.Summary, Number: b.Number, NumberSort: b.NumberSort, ReleaseDate: b.ReleaseDate, ISBN: b.ISBN,
				Authors: authors, Tags: b.Tags, Links: copyLinks(b.Links, func(l OpenLink) database.Link { return database.Link(l) }),
				CoverURL: b.CoverURL, HasCoverBlob: b.HasCoverBlob})
		}
	}
	cover.CoverKind, cover.CoverRef = offlineCoverRef(cover.CoverRef, entry.CoverURL)
	cover.NSFWKnown = !sidecar.checkpointedOnly
	dict := m.dictionaries[src.TagDictSourceID]
	for _, pair := range alignSeriesBooks(locals, remotes) {
		local, remote := locals[pair.localIdx], remotes[pair.remoteIdx]
		var ref string
		if src.ImportCovers {
			switch {
			case remote.ThumbnailID != "":
				ref, err = importKomga(SQLiteBlobKindKomgaBook, remote.ThumbnailID)
			case remote.HasCoverBlob:
				ref, err = importOpen(SQLiteBlobKindOpenBook, remote.EntryID)
			}
			if err != nil {
				return cover, aligned, created, err
			}
		}
		kind, ref := offlineCoverRef(ref, remote.CoverURL)
		book := database.ChainBookMatch{BookID: local.ID, Metadata: database.BookScrapedMetadata{
			Title: remote.Title, Summary: remote.Summary, Number: remote.Number, NumberSort: remote.NumberSort,
			ReleaseDate: remote.ReleaseDate, ISBN: remote.ISBN, Authors: remote.Authors, Tags: remote.Tags, Links: remote.Links,
			ExtSourceID: src.ID, ExtEntryID: remote.EntryID, MatchOrigin: hit.meta.MatchOrigin,
		}}
		if remote.DefaultTitle {
			book.Metadata.Title = ""
		}
		if dict != nil {
			book.Metadata.Tags = dict.TranslateList(book.Metadata.Tags, TagDisplayMode(src.TagDisplayMode))
		}
		if kind == database.CoverKindHTTPURL {
			book.Metadata.ExtCoverURL = ref
		}
		gen := sourceGeneration(src)
		book.Bindings = []database.BookSourceBinding{{BookID: local.ID, SourceID: src.ID, SeriesEntryID: seriesID, EntryID: remote.EntryID, MatchOrigin: hit.meta.MatchOrigin, Confidence: hit.meta.Confidence, SourceGeneration: gen}}
		if ref != "" {
			book.Covers = []database.BookCoverCandidate{{BookID: local.ID, SourceID: src.ID, SeriesEntryID: seriesID, EntryID: remote.EntryID, CoverKind: kind, CoverRef: ref, IsNSFW: entry.IsNSFW, NSFWKnown: !sidecar.checkpointedOnly, RankOrder: hit.rank, SourceGeneration: gen}}
		}
		aligned.books = append(aligned.books, book)
	}
	return cover, aligned, created, nil
}

func offlineCoverRef(imported, rawURL string) (string, string) {
	if imported != "" {
		return database.CoverKindImportedFile, imported
	}
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil {
		return database.CoverKindHTTPURL, rawURL
	}
	return "", ""
}

func komgaAuthors(items []KomgaAuthor) []database.Author {
	authors := make([]database.Author, 0, len(items))
	for _, a := range items {
		authors = append(authors, database.Author(a))
	}
	return authors
}

func copyLinks[T any](items []T, convert func(T) database.Link) []database.Link {
	links := make([]database.Link, 0, len(items))
	for _, item := range items {
		links = append(links, convert(item))
	}
	return links
}

func (m *Manager) mergeBangumiBooks(ctx context.Context, series database.Series, locals []localBookCandidate, books map[string]*database.ChainBookMatch, hit offlineHit) error {
	if len(locals) == 0 {
		return nil
	}
	offlineCtx := database.WithUserScope(ctx, database.UserScope{IsAdmin: true, AllowNSFW: true, ShowNSFW: true})
	tankobons, _, err := m.bangumi.OfflineTankobons(offlineCtx, hit.meta.SubjectID)
	if err != nil {
		return err
	}
	volumes := make([]bangumi.VolumeMatchBook, 0, len(locals))
	for _, local := range locals {
		volumes = append(volumes, bangumi.VolumeMatchBook{BookID: local.ID, Name: local.Name, Number: local.MetadataNumber})
	}
	parent := strconv.FormatInt(hit.meta.SubjectID, 10)
	for _, aligned := range bangumi.MatchBooksToTankobons(hit.meta.SubjectID, series.OneShot || len(locals) == 1, volumes, tankobons) {
		if aligned.MatchedBy == "oneshot_fallback" {
			aligned.NSFWKnown, aligned.NSFW = true, hit.meta.IsNSFW
			aligned.CoverURL = hit.meta.CoverURL
		}
		id := strconv.FormatInt(aligned.SubjectID, 10)
		gen := sourceGeneration(hit.source)
		book := database.ChainBookMatch{BookID: aligned.BookID, Metadata: database.BookScrapedMetadata{
			Title: aligned.Title, Summary: aligned.Summary, ISBN: aligned.ISBN, ReleaseDate: aligned.Date, MatchOrigin: hit.meta.MatchOrigin,
		}, Bindings: []database.BookSourceBinding{{BookID: aligned.BookID, SourceID: hit.sourceID, SeriesEntryID: parent, EntryID: id, MatchOrigin: hit.meta.MatchOrigin, Confidence: hit.meta.Confidence, SourceGeneration: gen}}}
		if aligned.NSFWKnown {
			book.Metadata.BgmSubjectID, book.Metadata.BgmCoverURL, book.Metadata.BgmIsNSFW = aligned.SubjectID, aligned.CoverURL, aligned.NSFW
		}
		if aligned.CoverURL != "" {
			book.Covers = []database.BookCoverCandidate{{BookID: aligned.BookID, SourceID: hit.sourceID, SeriesEntryID: parent, EntryID: id, CoverKind: database.CoverKindBangumi, CoverRef: aligned.CoverURL, IsNSFW: aligned.NSFW, NSFWKnown: aligned.NSFWKnown, RankOrder: hit.rank, SourceGeneration: gen}}
		}
		mergeChainBook(books, book)
	}
	return nil
}

func mergeChainBook(books map[string]*database.ChainBookMatch, item database.ChainBookMatch) {
	current := books[item.BookID]
	if current == nil {
		current = &database.ChainBookMatch{BookID: item.BookID}
		books[item.BookID] = current
	}
	dst, src := &current.Metadata, item.Metadata
	dst.Title = firstNonEmpty(dst.Title, src.Title)
	dst.Summary = firstNonEmpty(dst.Summary, src.Summary)
	dst.Number = firstNonEmpty(dst.Number, src.Number)
	dst.ReleaseDate = firstNonEmpty(dst.ReleaseDate, src.ReleaseDate)
	dst.ISBN = firstNonEmpty(dst.ISBN, src.ISBN)
	if dst.NumberSort == nil && src.NumberSort != nil && *src.NumberSort > 0 && !math.IsInf(*src.NumberSort, 0) {
		dst.NumberSort = src.NumberSort
	}
	dst.Authors = append(dst.Authors, src.Authors...)
	dst.Tags = append(dst.Tags, src.Tags...)
	dst.Links = append(dst.Links, src.Links...)
	if dst.BgmSubjectID == 0 && src.BgmSubjectID > 0 {
		dst.BgmSubjectID, dst.BgmCoverURL, dst.BgmIsNSFW = src.BgmSubjectID, src.BgmCoverURL, src.BgmIsNSFW
	}
	if dst.ExtSourceID == "" && src.ExtSourceID != "" {
		dst.ExtSourceID, dst.ExtEntryID, dst.ExtCoverURL = src.ExtSourceID, src.ExtEntryID, src.ExtCoverURL
	}
	dst.MatchOrigin = firstNonEmpty(dst.MatchOrigin, src.MatchOrigin)
	current.Bindings = append(current.Bindings, item.Bindings...)
	current.Covers = append(current.Covers, item.Covers...)
}
