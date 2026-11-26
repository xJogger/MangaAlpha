package extmeta

import (
	"cmp"
	"context"
	"errors"
	"path/filepath"
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
	blobSourceID, kind, key, isBlob := ParseSQLiteBlobCoverRef(coverURL)
	if isBlob {
		if blobSourceID != sourceID {
			return nil, "", ErrInvalidConfig
		}
		return m.fetchBlobCover(ctx, sourceID, strings.TrimSpace(entryID), strings.TrimSpace(parentEntryID), kind, key)
	}
	src, err := m.store.GetMetadataSource(ctx, sourceID)
	if err != nil {
		return nil, "", err
	}
	if !src.Enabled || !src.Ready || src.Kind != KindOfflineSQLite || m.covers == nil || coverURL == "" {
		return nil, "", ErrUnavailable
	}
	if scope, ok := database.UserScopeFromContext(ctx); ok && !scope.AllowNSFW {
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

func (m *Manager) fetchBlobCover(ctx context.Context, sourceID, entryID, parentEntryID, kind, key string) ([]byte, string, error) {
	// Hold the source generation stable through authorization and cache lookup.
	m.mu.RLock()
	defer m.mu.RUnlock()
	src, err := m.store.GetMetadataSource(ctx, sourceID)
	if err != nil {
		return nil, "", err
	}
	if !src.Enabled || !src.Ready || src.Kind != KindOfflineSQLite || m.covers == nil {
		return nil, "", ErrUnavailable
	}
	upstream := m.upstreamPath(src)
	if upstream == "" {
		return nil, "", ErrUnavailable
	}
	if src.LocalPath != "" && filepath.Clean(src.LocalPath) != m.sqlitePath(src.ID) {
		if _, err := checkedLocalFile(upstream); err != nil {
			return nil, "", ErrUnavailable
		}
	}
	db, checkpointedOnly, err := openKomgaSQLiteSnapshot(upstream)
	if err != nil {
		return nil, "", ErrUnavailable
	}
	defer db.Close()
	showNSFW := m.store.ShowNSFW(ctx)
	if checkpointedOnly && !showNSFW {
		return nil, "", ErrUnavailable
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", ErrUnavailable
	}
	defer tx.Rollback()

	var nsfw bool
	var seriesID, bookKey string
	load := func(loadCtx context.Context) ([]byte, string, error) {
		switch kind {
		case SQLiteBlobKindKomgaSeries, SQLiteBlobKindKomgaBook:
			return readKomgaThumbnailBLOB(loadCtx, tx, kind, key)
		default:
			return readOpenCoverBLOB(loadCtx, tx, kind, seriesID, bookKey)
		}
	}
	switch kind {
	case SQLiteBlobKindKomgaSeries, SQLiteBlobKindKomgaBook:
		nsfw, err = komgaCoverRating(ctx, tx, kind, key, entryID, parentEntryID)
	case SQLiteBlobKindOpenEntry, SQLiteBlobKindOpenBook:
		seriesID = key
		if kind == SQLiteBlobKindOpenBook {
			seriesID, bookKey, err = resolveOpenBookCover(ctx, tx, key, entryID, parentEntryID)
			key = openBookCoverKey(seriesID, bookKey)
		} else if entryID != "" && entryID != seriesID {
			err = ErrEntryNotFound
		}
		if err == nil {
			var entry *Entry
			entry, err = getMangaAlphaMetaV1Entry(ctx, tx, sourceID, seriesID)
			if err == nil {
				nsfw = entry.IsNSFW
			}
		}
	}
	if err != nil || (nsfw && !showNSFW) {
		return nil, "", ErrUnavailable
	}
	if entryID == "" {
		entryID = kind + "-" + key
	}
	ref := EncodeSQLiteBlobCoverRef(sourceID, kind, key)
	cacheKey := strings.Join([]string{ref, src.LocalSHA256, src.LastUpdatedAt}, "\x00")
	data, err := m.covers.GetOrLoadLocal(ctx, sourceID, entryID, cacheKey, maxSQLiteBlobCoverBytes, load)
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
	entry, err := m.entry(ctx, sourceID, strings.TrimSpace(entryID))
	if err != nil {
		return nil, "", err
	}
	return m.FetchCover(ctx, sourceID, entry.EntryID, entry.CoverURL)
}
