package extmeta

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/database"
)

const (
	fallbackCoverTTL        = 10 * time.Minute
	maxFallbackCoverLookups = 4096
)

type fallbackCoverHint struct {
	sourceID, entryID, coverURL string
}

type fallbackCoverLookup struct {
	hint      fallbackCoverHint
	expiresAt time.Time
}

func (m *Manager) FetchCover(ctx context.Context, sourceID, entryID, coverURL string) ([]byte, string, error) {
	return m.fetchCover(ctx, sourceID, entryID, "", coverURL)
}

// FetchBookCover checks a book's upstream parent binding when it is available.
func (m *Manager) FetchBookCover(ctx context.Context, sourceID, entryID, parentEntryID, coverURL string) ([]byte, string, error) {
	return m.fetchCover(ctx, sourceID, entryID, parentEntryID, coverURL)
}

func (m *Manager) fetchCover(ctx context.Context, sourceID, entryID, parentEntryID, coverURL string) ([]byte, string, error) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(coverURL)), SQLiteBlobCoverPrefix) {
		return nil, "", ErrUnavailable
	}
	src, err := m.store.GetMetadataSource(ctx, sourceID)
	if err != nil {
		return nil, "", err
	}
	if !src.Enabled || !src.Ready || src.Kind != KindOfflineSQLite || m.covers == nil || coverURL == "" {
		return nil, "", ErrUnavailable
	}
	if !m.store.ShowNSFW(ctx) {
		entry, err := m.entry(ctx, sourceID, firstNonEmpty(parentEntryID, entryID))
		if err != nil || entry.IsNSFW {
			return nil, "", ErrUnavailable
		}
	}
	u, err := validateHTTPURL(coverURL)
	if err != nil || u.User != nil {
		return nil, "", ErrInvalidConfig
	}
	data, err := m.covers.GetOrFetch(ctx, sourceID, firstNonEmpty(parentEntryID, entryID), coverURL)
	return data.Bytes, data.MediaType, err
}

// FetchFallbackNSFWCover resolves a cover from offline sources without rebinding the series.
func (m *Manager) FetchFallbackNSFWCover(ctx context.Context, seriesID string) ([]byte, string, error) {
	scope, ok := database.UserScopeFromContext(ctx)
	if !ok || !scope.EffectiveShowNSFW() || m.covers == nil {
		return nil, "", ErrUnavailable
	}
	series, err := m.store.SeriesByID(ctx, seriesID)
	if err != nil {
		return nil, "", err
	}
	hint, err := m.lookupFallbackCover(ctx, series)
	if err != nil {
		return nil, "", err
	}
	if hint.coverURL == "" {
		return nil, "", ErrUnavailable
	}
	return m.FetchCover(ctx, hint.sourceID, hint.entryID, hint.coverURL)
}

func (m *Manager) lookupFallbackCover(ctx context.Context, series database.Series) (fallbackCoverHint, error) {
	m.mu.RLock()
	cached, epoch := m.fallbackCovers[series.ID], m.fallbackEpoch
	m.mu.RUnlock()
	if time.Now().Before(cached.expiresAt) {
		return cached.hint, nil
	}
	key := strconv.FormatUint(epoch, 10) + ":" + series.ID
	value, err, _ := m.fallbackFlight.Do(key, func() (any, error) {
		m.mu.RLock()
		cached := m.fallbackCovers[series.ID]
		if time.Now().Before(cached.expiresAt) {
			m.mu.RUnlock()
			return cached.hint, nil
		}
		hint, err := m.findFallbackCoverLocked(ctx, series)
		m.mu.RUnlock()
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.fallbackEpoch == epoch {
			if m.fallbackCovers == nil {
				m.fallbackCovers = make(map[string]fallbackCoverLookup)
			}
			if len(m.fallbackCovers) >= maxFallbackCoverLookups {
				for id := range m.fallbackCovers {
					delete(m.fallbackCovers, id)
					break
				}
			}
			m.fallbackCovers[series.ID] = fallbackCoverLookup{hint, time.Now().Add(fallbackCoverTTL)}
		}
		return hint, nil
	})
	if err != nil {
		return fallbackCoverHint{}, err
	}
	return value.(fallbackCoverHint), nil
}

func (m *Manager) findFallbackCoverLocked(ctx context.Context, series database.Series) (fallbackCoverHint, error) {
	meta, err := m.store.SeriesMetadataByID(ctx, series)
	if err != nil {
		return fallbackCoverHint{}, err
	}
	parsed := bangumi.ParseSeriesWithPath(series.Name, series.RelativePath)
	titles := []string{meta.Title}
	for _, alternate := range meta.AlternateTitles {
		titles = append(titles, alternate.Title)
	}
	titles = append(titles, parsed.Candidates...)
	titles = append(titles, series.Name)
	authors := slices.Clone(parsed.Authors)
	for _, author := range meta.Authors {
		authors = append(authors, author.Name)
	}
	chain, err := m.store.ListLibraryMetadataSources(ctx, series.LibraryID)
	if err != nil {
		return fallbackCoverHint{}, err
	}
	sources, err := m.store.ListMetadataSources(ctx, KindOfflineSQLite)
	if err != nil {
		return fallbackCoverHint{}, err
	}
	slices.SortFunc(sources, func(a, b database.MetadataSource) int { return cmp.Compare(a.ID, b.ID) })
	ids := make([]string, 0, len(sources))
	for _, link := range chain {
		if link.Enabled && link.SourceEnabled && link.SourceReady && link.SourceKind == KindOfflineSQLite {
			ids = append(ids, link.SourceID)
		}
	}
	for _, src := range sources {
		if src.Enabled && src.Ready && !slices.Contains(ids, src.ID) {
			ids = append(ids, src.ID)
		}
	}
	var lookupErr error
	for _, id := range ids {
		sidecar := m.sidecars[id]
		if sidecar == nil {
			continue
		}
		entry, _, err := matchSidecarTitle(ctx, sidecar, titles, authors, true)
		if err != nil {
			lookupErr = errors.Join(lookupErr, err)
			continue
		}
		if entry != nil {
			return fallbackCoverHint{id, entry.EntryID, entry.CoverURL}, nil
		}
	}
	return fallbackCoverHint{}, lookupErr
}

func (m *Manager) InvalidateFallbackCoverCache() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invalidateFallbackCoverCacheLocked()
}

func (m *Manager) invalidateFallbackCoverCacheLocked() {
	clear(m.fallbackCovers)
	m.fallbackEpoch++
}

func (m *Manager) EntryCover(ctx context.Context, sourceID, entryID string) ([]byte, string, error) {
	if err := checkAdmin(ctx); err != nil {
		return nil, "", err
	}
	src, err := m.store.GetMetadataSource(ctx, sourceID)
	if err != nil {
		return nil, "", err
	}
	if !src.Ready || !src.Enabled {
		return nil, "", ErrUnavailable
	}
	entry, err := m.entry(ctx, sourceID, strings.TrimSpace(entryID))
	if err != nil {
		return nil, "", err
	}
	if entry.CoverURL != "" {
		return m.FetchCover(ctx, sourceID, entry.EntryID, entry.CoverURL)
	}
	if !src.ImportCovers {
		return nil, "", ErrEntryNotFound
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	sidecar := m.sidecars[sourceID]
	current, err := m.store.GetMetadataSource(ctx, sourceID)
	if err != nil || !current.Enabled || !current.ImportCovers || sourceGeneration(current) != sourceGeneration(src) || sidecar == nil {
		return nil, "", ErrUnavailable
	}
	if sidecar.checkpointedOnly && !m.store.ShowNSFW(ctx) {
		return nil, "", ErrUnavailable
	}
	tx, err := sidecar.upstreamDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	switch sidecar.Schema() {
	case SchemaKomgaSQLiteV1:
		info, err := DetectKomgaSchema(ctx, tx)
		if err != nil {
			return nil, "", err
		}
		series, err := readKomgaSeries(ctx, tx, info, entry.EntryID, true, true)
		if err != nil {
			return nil, "", err
		}
		if series.NSFW && !m.store.ShowNSFW(ctx) {
			return nil, "", ErrUnavailable
		}
		return readKomgaSeriesThumbnail(ctx, tx, series)
	case SchemaMangaAlphaMetaV1, SchemaMangaAlphaMetaV2:
		snapshot, err := getMangaAlphaMetaV1Entry(ctx, tx, sourceID, entry.EntryID)
		if err != nil {
			return nil, "", err
		}
		if !snapshot.HasCoverBlob {
			return nil, "", ErrEntryNotFound
		}
		if snapshot.IsNSFW && !m.store.ShowNSFW(ctx) {
			return nil, "", ErrUnavailable
		}
		return readOpenCoverBLOB(ctx, tx, SQLiteBlobKindOpenEntry, entry.EntryID, "")
	default:
		return nil, "", ErrEntryNotFound
	}
}
