package extmeta

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/cjk"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/diagnostics"
)

var ErrNoReadySources = errors.New("目标书库暂无已就绪的离线数据源，请先一键下载 Bangumi 离线索引库或同步扩展离线源")

const (
	scrapeProgressBatch    = 25
	scrapeProgressInterval = 400 * time.Millisecond
	topLevelLibraryFilter  = "l.enabled=1 AND NOT EXISTS (SELECT 1 FROM union_layers ul WHERE ul.member_library_id=l.id)"
)

type ScrapeRun struct {
	ID            string `json:"id"`
	LibraryID     string `json:"libraryId"`
	LibraryName   string `json:"libraryName,omitempty"`
	CurrentSeries string `json:"currentSeries,omitempty"`
	Mode          string `json:"mode"`
	Status        string `json:"status"`
	AllowOnline   bool   `json:"allowOnline"`
	StartedAt     string `json:"startedAt"`
	CompletedAt   string `json:"completedAt,omitempty"`
	TotalSeries   int    `json:"totalSeries"`
	Processed     int    `json:"processedSeries"`
	Matched       int    `json:"matchedCount"`
	Mapped        int    `json:"mappingCount"`
	Unmatched     int    `json:"unmatchedCount"`
	Skipped       int    `json:"skippedCount"`
	Failed        int    `json:"failedCount"`
	Error         string `json:"error,omitempty"`
}

type scrapeSeries struct {
	series    database.Series
	coverSHA1 string
	bound     bool
}

type scrapePlan struct {
	sources       map[string]database.MetadataSource
	chains        map[string][]database.LibraryMetadataSource
	mappings      *MappingIndex
	readySidecars map[string]bool
	bySchema      map[string][]string
	online        []database.MetadataSource
}

func (p scrapePlan) chain(libraryID string) []database.LibraryMetadataSource {
	if chain := p.chains[libraryID]; len(chain) > 0 {
		return chain
	}
	return []database.LibraryMetadataSource{{SourceID: TargetSourceBangumi, Enabled: true}}
}

func (p scrapePlan) hasReadyChain(libraryID string, bgmReady bool) bool {
	for _, link := range p.chain(libraryID) {
		if !link.Enabled {
			continue
		}
		if p.readySidecars[link.SourceID] ||
			(bgmReady && (link.SourceID == TargetSourceBangumi || p.sources[link.SourceID].Kind == KindBangumiArchive)) {
			return true
		}
	}
	return false
}

func (m *Manager) HasReadySourcesForLibrary(ctx context.Context, libraryID string, allowOnline bool) (bool, error) {
	if err := checkAdmin(ctx); err != nil {
		return false, err
	}
	plan, err := m.scrapePlan(ctx)
	if err != nil {
		return false, err
	}
	if (allowOnline && len(plan.online) > 0) || plan.mappings.Len() > 0 {
		return true, nil
	}
	bgmReady := m.bangumi != nil && m.bangumi.ArchiveStatus().Ready
	if libraryID != "" {
		return plan.hasReadyChain(libraryID, bgmReady), nil
	}
	rows, err := m.store.DB().QueryContext(ctx, "SELECT l.id FROM libraries l WHERE "+topLevelLibraryFilter)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	hasLibraries := false
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return false, err
		}
		hasLibraries = true
		if plan.hasReadyChain(id, bgmReady) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return !hasLibraries && (bgmReady || len(plan.readySidecars) > 0), nil
}

func (m *Manager) StartMultiScrape(ctx context.Context, libraryID, mode string, allowOnline bool) (ScrapeRun, error) {
	if err := checkAdmin(ctx); err != nil {
		return ScrapeRun{}, err
	}
	libraryID = strings.TrimSpace(libraryID)
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = bangumi.ScrapeModeIncremental
	}
	switch mode {
	case bangumi.ScrapeModeIncremental, bangumi.ScrapeModeRetryUnmatched, bangumi.ScrapeModeForce:
	default:
		return ScrapeRun{}, ErrInvalidConfig
	}
	libraryName := "全部未隐藏书库"
	if libraryID != "" {
		lib, err := m.store.LibraryBasic(ctx, libraryID)
		if err != nil {
			return ScrapeRun{}, err
		}
		if !lib.Enabled || lib.UnionParentID != "" {
			return ScrapeRun{}, ErrUnavailable
		}
		libraryName = lib.Name
	}
	ready, err := m.HasReadySourcesForLibrary(ctx, libraryID, allowOnline)
	if err != nil {
		return ScrapeRun{}, err
	}
	if !ready {
		return ScrapeRun{}, ErrNoReadySources
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ScrapeRun{}, ErrUnavailable
	}
	if m.scrapeCancel != nil {
		return ScrapeRun{}, ErrBusy
	}
	run := ScrapeRun{
		ID: "ext_" + newRunID(), LibraryID: libraryID, LibraryName: libraryName, Mode: mode, AllowOnline: allowOnline,
		Status: "running", StartedAt: timestamp(),
	}
	storedMode := mode
	if allowOnline {
		storedMode += "+online"
	}
	if _, err := m.store.DB().ExecContext(ctx, `
INSERT INTO bangumi_scrape_runs(id,library_id,mode,force,status,started_at,created_at)
VALUES(?,nullif(?,''),?,?,'running',?,?)`, run.ID, libraryID, storedMode, mode == bangumi.ScrapeModeForce,
		run.StartedAt, run.StartedAt); err != nil {
		return ScrapeRun{}, err
	}
	jobCtx, cancel := context.WithCancel(m.ctx)
	m.scrapeCancel = cancel
	m.scrapeFlush = make(chan struct{}, 1)
	m.scrapes = append(m.scrapes, run)
	if len(m.scrapes) > runHistoryLimit {
		m.scrapes = slices.Delete(m.scrapes, 0, len(m.scrapes)-runHistoryLimit)
	}
	flush := m.scrapeFlush
	m.wg.Go(func() {
		defer cancel()
		m.executeScrape(jobCtx, run, flush)
	})
	return run, nil
}

func (m *Manager) executeScrape(ctx context.Context, run ScrapeRun, flush <-chan struct{}) {
	started := time.Now()
	flushCtx, stopFlush := context.WithCancel(ctx)
	flushed := make(chan struct{})
	initial := run
	go func() {
		defer close(flushed)
		m.flushScrapeProgress(flushCtx, initial, flush)
	}()

	err := m.multiScrape(database.WithAutomaticMetadata(ctx), &run)
	stopFlush()
	<-flushed
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case ctx.Err() != nil:
		run.Status = "canceled"
	case err != nil:
		run.Status, run.Error = "failed", "metadata scrape failed"
	case run.Failed > 0:
		run.Status = "partial"
	default:
		run.Status = "success"
	}
	run.CompletedAt = timestamp()
	if err := m.saveScrapeRun(context.Background(), run); err != nil {
		run.Status, run.Error = "failed", "could not save metadata scrape status"
		if m.logger != nil {
			m.logger.Warn(run.Error, "runId", run.ID)
		}
	}
	diagnostics.Default().Record(diagnostics.Event{
		Component: "extmeta", Stage: "extmeta_scrape", Action: "finish",
		DurationMs: time.Since(started).Milliseconds(),
		Meta: map[string]any{
			"mode": run.Mode, "status": run.Status, "allowOnline": run.AllowOnline,
			"total": run.TotalSeries, "matched": run.Matched, "mapped": run.Mapped,
			"unmatched": run.Unmatched, "skipped": run.Skipped, "failed": run.Failed,
		},
	})
	m.scrapes[len(m.scrapes)-1] = run
	m.scrapeCancel = nil
	m.scrapeFlush = nil
}

func (m *Manager) flushScrapeProgress(ctx context.Context, saved ScrapeRun, flush <-chan struct{}) {
	ticker := time.NewTicker(scrapeProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-flush:
		}
		m.mu.RLock()
		run := m.scrapes[len(m.scrapes)-1]
		m.mu.RUnlock()
		if run != saved && m.saveScrapeRun(ctx, run) == nil {
			saved = run
		}
	}
}

func (m *Manager) saveScrapeRun(ctx context.Context, run ScrapeRun) error {
	messages := []string{}
	if run.Error != "" {
		messages = append(messages, run.Error)
	}
	errorsJSON, _ := json.Marshal(messages)
	_, err := m.store.DB().ExecContext(ctx, `
UPDATE bangumi_scrape_runs SET status=?,completed_at=nullif(?,''),total_series=?,processed_series=?,
 matched_count=?,unmatched_count=?,skipped_count=?,failed_count=?,current_series=?,errors_json=?
WHERE id=?`, run.Status, run.CompletedAt, run.TotalSeries, run.Processed, run.Matched, run.Unmatched,
		run.Skipped, run.Failed, run.CurrentSeries, string(errorsJSON), run.ID)
	return err
}

const selectScrapeRuns = `
SELECT r.id,coalesce(r.library_id,''),coalesce(l.name,'全部未隐藏书库'),r.mode,r.status,
 coalesce(r.started_at,''),coalesce(r.completed_at,''),r.total_series,r.processed_series,
 r.matched_count,r.unmatched_count,r.skipped_count,r.failed_count,r.current_series,r.errors_json
FROM bangumi_scrape_runs r LEFT JOIN libraries l ON l.id=r.library_id
WHERE r.id LIKE 'ext_%'`

func scanStoredScrapeRun(row interface{ Scan(...any) error }) (ScrapeRun, error) {
	var run ScrapeRun
	var errorsJSON string
	if err := row.Scan(&run.ID, &run.LibraryID, &run.LibraryName, &run.Mode, &run.Status, &run.StartedAt,
		&run.CompletedAt, &run.TotalSeries, &run.Processed, &run.Matched, &run.Unmatched, &run.Skipped,
		&run.Failed, &run.CurrentSeries, &errorsJSON); err != nil {
		return ScrapeRun{}, err
	}
	run.Mode, run.AllowOnline = strings.CutSuffix(run.Mode, "+online")
	var messages []string
	if json.Unmarshal([]byte(errorsJSON), &messages) == nil && len(messages) > 0 {
		run.Error = messages[0]
	}
	return run, nil
}

// mergeScrapeProgress overlays the in-process snapshot while retaining the current library name.
// The caller holds m.mu for reading.
func (m *Manager) mergeScrapeProgress(run ScrapeRun) ScrapeRun {
	for _, live := range m.scrapes {
		if live.ID == run.ID {
			live.LibraryName = run.LibraryName
			return live
		}
	}
	return run
}

func (m *Manager) ScrapeRunByID(ctx context.Context, runID string) (ScrapeRun, error) {
	if err := checkAdmin(ctx); err != nil {
		return ScrapeRun{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	run, err := scanStoredScrapeRun(m.store.DB().QueryRowContext(ctx, selectScrapeRuns+" AND r.id=?", runID))
	if err != nil {
		return ScrapeRun{}, err
	}
	return m.mergeScrapeProgress(run), nil
}

func (m *Manager) ScrapeRuns() []ScrapeRun {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ScrapeRun, 0)
	rows, err := m.store.DB().QueryContext(context.Background(), selectScrapeRuns+" ORDER BY r.created_at DESC LIMIT 30")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		run, err := scanStoredScrapeRun(rows)
		if err != nil {
			break
		}
		out = append(out, m.mergeScrapeProgress(run))
	}
	return out
}

func (m *Manager) CancelScrape(runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scrapeCancel == nil || m.scrapes[len(m.scrapes)-1].ID != runID {
		return sql.ErrNoRows
	}
	m.scrapeCancel()
	return nil
}

func (m *Manager) CancelLibrary(libraryID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scrapeCancel != nil && m.scrapes[len(m.scrapes)-1].LibraryID == libraryID {
		m.scrapeCancel()
	}
	// An all-library run checks visibility at each write and before online requests.
}

func (m *Manager) scrapePlan(ctx context.Context) (scrapePlan, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sources, err := m.store.ListMetadataSources(ctx, "")
	if err != nil {
		return scrapePlan{}, err
	}
	chains, err := m.store.ListAllLibraryMetadataChains(ctx)
	if err != nil {
		return scrapePlan{}, err
	}
	plan := scrapePlan{
		sources: make(map[string]database.MetadataSource, len(sources)),
		chains:  chains, mappings: m.mappingIndex, readySidecars: make(map[string]bool),
		bySchema: make(map[string][]string),
	}
	for _, src := range sources {
		if !src.Enabled {
			continue
		}
		plan.sources[src.ID] = src
		if sidecar := m.sidecars[src.ID]; src.Kind == KindOfflineSQLite && src.Ready && sidecar != nil {
			plan.readySidecars[src.ID] = true
			schema := sidecar.Schema()
			plan.bySchema[schema] = append(plan.bySchema[schema], src.ID)
		}
		if src.OnlineEnabled && src.OnlineAPIURL != "" && src.Kind == KindOfflineSQLite {
			plan.online = append(plan.online, src)
		}
	}
	return plan, nil
}

func (m *Manager) scrapeTargets(ctx context.Context, libraryID, mode string) ([]scrapeSeries, error) {
	query := `
SELECT s.id,s.library_id,s.name,s.relative_path,s.one_shot,
       coalesce(sm.ext_cover_url,''),
       (coalesce(sm.bgm_subject_id,0)>0 OR coalesce(sm.ext_entry_id,'')<>'')
FROM series s JOIN libraries l ON l.id=s.library_id
LEFT JOIN series_metadata sm ON sm.series_id=s.id
WHERE ` + topLevelLibraryFilter + `
  AND coalesce(sm.match_origin,'') NOT IN ('manual','agent')
  AND NOT (coalesce(sm.match_origin,'')='' AND (coalesce(sm.bgm_subject_id,0)>0 OR coalesce(sm.ext_entry_id,'')<>''))`
	var args []any
	if libraryID != "" {
		query += " AND s.library_id=?"
		args = append(args, libraryID)
	}
	switch mode {
	case bangumi.ScrapeModeIncremental:
		query += " AND coalesce(sm.scrape_status,'none')='none'"
	case bangumi.ScrapeModeRetryUnmatched:
		query += " AND coalesce(sm.scrape_status,'none')<>'matched'"
	}
	rows, err := m.store.DB().QueryContext(ctx, query+" ORDER BY s.library_id,s.id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scrapeSeries
	for rows.Next() {
		var target scrapeSeries
		var coverURL string
		if err := rows.Scan(&target.series.ID, &target.series.LibraryID, &target.series.Name,
			&target.series.RelativePath, &target.series.OneShot, &coverURL, &target.bound); err != nil {
			return nil, err
		}
		target.coverSHA1 = ExtractThumbSHA1(coverURL)
		out = append(out, target)
	}
	return out, rows.Err()
}

func (m *Manager) multiScrape(ctx context.Context, run *ScrapeRun) error {
	plan, err := m.scrapePlan(ctx)
	if err != nil {
		return err
	}
	targets, err := m.scrapeTargets(ctx, run.LibraryID, run.Mode)
	if err != nil {
		return err
	}
	run.TotalSeries = len(targets)
	m.publishScrapeProgress(run)
	pending := make([]scrapeSeries, 0)
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		run.CurrentSeries = target.series.Name
		m.publishScrapeProgress(run)
		incoming, matched, err := m.matchOffline(ctx, target, plan)
		if err == nil && !matched && run.AllowOnline && len(plan.online) > 0 {
			pending = append(pending, target)
		} else {
			m.finishSeries(ctx, run, target, incoming, matched, err)
		}
	}
	return m.enrichOnline(ctx, run, plan.online, pending)
}

func (m *Manager) finishSeries(ctx context.Context, run *ScrapeRun, target scrapeSeries, incoming database.ScrapedMetadata, matched bool, err error) error {
	if err == nil {
		switch {
		case matched:
			_, err = m.store.ApplyScrapedMetadata(ctx, target.series, incoming)
			if err == nil && incoming.ExtSourceID != "" && incoming.ExtEntryID != "" {
				err = m.syncSeriesBooks(ctx, target.series, incoming.ExtSourceID, incoming.ExtEntryID, incoming.MatchOrigin)
				if err != nil && !errors.Is(err, database.ErrMetadataChanged) && m.logger != nil {
					m.logger.Warn("failed to align series books metadata", "seriesId", target.series.ID, "sourceId", incoming.ExtSourceID, "error", err)
				}
			}
		case !target.bound:
			inferred := bangumi.InferPathNSFW(target.series.Name, target.series.RelativePath)
			err = m.store.MarkSeriesScrapeStatusWithNSFW(ctx, target.series, database.ScrapeStatusUnmatched, 0, inferred)
		}
	}
	run.Processed++
	switch {
	case errors.Is(err, database.ErrMetadataChanged):
		run.Skipped++
	case err != nil:
		run.Failed++
		run.Error = "metadata scrape failed for one or more series"
	case matched:
		run.Matched++
		if incoming.MatchOrigin == "mapping" {
			run.Mapped++
		}
	default:
		run.Unmatched++
	}
	m.publishScrapeProgress(run)
	return err
}

func (m *Manager) publishScrapeProgress(run *ScrapeRun) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := len(m.scrapes) - 1
	if run.Processed != m.scrapes[i].Processed && run.Processed%scrapeProgressBatch == 0 {
		select {
		case m.scrapeFlush <- struct{}{}:
		default:
		}
	}
	m.scrapes[i] = *run
}

func (m *Manager) matchOffline(ctx context.Context, target scrapeSeries, plan scrapePlan) (database.ScrapedMetadata, bool, error) {
	series := target.series
	chain := plan.chain(series.LibraryID)
	parsed := bangumi.ParseSeriesWithPath(series.Name, series.RelativePath)
	titles := append(slices.Clone(parsed.Candidates), series.Name)
	var embedded *EmbeddedMeta
	var readCachedMeta bool
	var sourceErr error
	for _, link := range chain {
		if !link.Enabled {
			continue
		}
		src := plan.sources[link.SourceID]
		if link.SourceID == TargetSourceBangumi || src.Kind == KindBangumiArchive {
			if m.bangumi == nil {
				continue
			}
			meta, found, err := m.bangumi.OfflineMatch(ctx, series)
			if found {
				meta.MatchOrigin = "auto"
				return meta, true, nil
			}
			if err != nil {
				sourceErr = err
			}
		}
		if !plan.readySidecars[link.SourceID] {
			continue
		}
		if !readCachedMeta {
			readCachedMeta = true
			embedded = m.cachedEmbeddedMetaFromDB(ctx, series.ID)
		}
		entry, confidence, err := m.matchSidecar(ctx, src.ID, titles, parsed.Authors, target.coverSHA1, embedded)
		if err != nil {
			sourceErr = err
			continue
		}
		if entry != nil {
			return m.externalMetadata(ctx, src, entry, "auto", confidence), true, nil
		}
	}
	meta, found, err := m.matchMapping(ctx, target.series, parsed, chain, plan)
	if found {
		return meta, true, nil
	}
	return meta, false, errors.Join(sourceErr, err)
}

func (m *Manager) matchMapping(ctx context.Context, series database.Series, parsed bangumi.ParsedQuery, chain []database.LibraryMetadataSource, plan scrapePlan) (database.ScrapedMetadata, bool, error) {
	key := firstNonEmpty(append(slices.Clone(parsed.Candidates), series.Name)...)
	authors := parsed.Authors
	if len(authors) == 0 {
		authors = []string{""}
	}
	for _, author := range authors {
		if rec, ok := plan.mappings.Lookup(key, series.Name, author, TargetSourceBangumi); ok && m.bangumi != nil {
			if id, err := strconv.ParseInt(rec.TargetID, 10, 64); err == nil && id > 0 {
				meta, found, err := m.bangumi.OfflineSubject(ctx, series, id)
				if found || err != nil {
					meta.MatchOrigin = "mapping"
					return meta, found, err
				}
			}
		}
		for _, schema := range []string{TargetSourceMetaV1, SchemaMangaAlphaMetaV2, SchemaKomgaSQLiteV1, TargetSourceGalleryV1} {
			rec, ok := plan.mappings.Lookup(key, series.Name, author, schema)
			if !ok {
				continue
			}
			bySchemaIDs := append([]string(nil), plan.bySchema[schema]...)
			if schema == SchemaMangaAlphaMetaV2 {
				for _, v1ID := range plan.bySchema[TargetSourceMetaV1] {
					if !slices.Contains(bySchemaIDs, v1ID) {
						bySchemaIDs = append(bySchemaIDs, v1ID)
					}
				}
			}
			ids := make([]string, 0, len(bySchemaIDs))
			for _, link := range chain {
				if link.Enabled && slices.Contains(bySchemaIDs, link.SourceID) {
					ids = append(ids, link.SourceID)
				}
			}
			for _, id := range bySchemaIDs {
				if !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
			for _, id := range ids {
				entry, err := m.entry(ctx, id, rec.TargetID)
				if errors.Is(err, ErrEntryNotFound) || errors.Is(err, ErrUnavailable) {
					continue
				}
				if err != nil {
					return database.ScrapedMetadata{}, false, err
				}
				return m.externalMetadata(ctx, plan.sources[id], entry, "mapping", 1), true, nil
			}
		}
	}
	return database.ScrapedMetadata{}, false, nil
}

func (m *Manager) matchSidecar(ctx context.Context, id string, titles, authors []string, sha1 string, embedded *EmbeddedMeta) (*Entry, float64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sidecar := m.sidecars[id]
	if sidecar == nil {
		return nil, 0, nil
	}
	if embedded != nil {
		if sidecar.Schema() == SchemaGalleryV1 {
			entryID, token, ok := directEntryID(embedded.ExternalURL)
			if !ok && embedded.ExternalURL == "" {
				entryID, token, ok = directEntryID(embedded.ExternalID)
			}
			if ok {
				entry, err := getEntryWithToken(ctx, sidecar, entryID, token)
				if !errors.Is(err, ErrEntryNotFound) {
					return entry, 1, err
				}
			}
		}
		if strings.TrimSpace(embedded.Series) != "" {
			titles = append([]string{embedded.Series}, titles...)
		}
		if strings.TrimSpace(embedded.Title) != "" {
			titles = append(slices.Clone(titles), embedded.Title)
		}
		authors = append(slices.Clone(authors), embedded.Authors...)
	}
	if sha1 != "" {
		hits, err := sidecar.LookupByThumbSHA1(ctx, sha1)
		if err != nil {
			return nil, 0, err
		}
		if len(hits) == 1 {
			entry, err := sidecar.GetEntry(ctx, hits[0].EntryID)
			if !errors.Is(err, ErrEntryNotFound) {
				return entry, 1, err
			}
		}
	}
	return matchSidecarTitle(ctx, sidecar, titles, authors, false)
}

func matchSidecarTitle(ctx context.Context, sidecar *SidecarStore, titles, authors []string, requireCover bool) (*Entry, float64, error) {
	if sidecar.Schema() == SchemaGalleryV1 {
		hits, err := sidecar.searchWithHints(ctx, titles, authors, 10)
		if err != nil {
			return nil, 0, err
		}
		hit, ok := selectGalleryMatch(hits, requireCover)
		if !ok {
			return nil, 0, nil
		}
		entry, err := sidecar.GetEntry(ctx, hit.EntryID)
		if errors.Is(err, ErrEntryNotFound) {
			return nil, 0, nil
		}
		return entry, hit.Score, err
	}
	seen := make(map[string]bool, len(titles))
	for _, title := range titles {
		title = strings.TrimSpace(title)
		if title == "" || seen[title] {
			continue
		}
		seen[title] = true
		hits, err := sidecar.SearchByTitle(ctx, title, 2)
		if err != nil {
			return nil, 0, err
		}
		if len(hits) > 0 && hits[0].Score >= 0.95 && (len(hits) == 1 || hits[0].Score-hits[1].Score >= 0.05) {
			entry, err := sidecar.GetEntry(ctx, hits[0].EntryID)
			if errors.Is(err, ErrEntryNotFound) {
				continue
			}
			if err != nil {
				return nil, 0, err
			}
			if !requireCover || entry.CoverURL != "" {
				return entry, hits[0].Score, nil
			}
		}
	}
	return nil, 0, nil
}

func selectGalleryMatch(hits []SearchMatch, requireCover bool) (best SearchMatch, found bool) {
	if len(hits) == 0 || hits[0].Score < 0.95 {
		return best, false
	}
	for i, hit := range hits {
		if hits[0].Score-hit.Score >= 0.05 {
			break
		}
		gallery := hit.gallery
		if gallery == nil {
			return SearchMatch{}, false
		}
		for _, previous := range hits[:i] {
			other := previous.gallery
			sameRoot := gallery.rootGID > 0 && gallery.rootGID == other.rootGID
			authorConflict := len(gallery.authors) > 0 && len(other.authors) > 0 && !sharesKey(gallery.authors, other.authors)
			if authorConflict || (!sameRoot && !sharesKey(gallery.cores, other.cores)) {
				return SearchMatch{}, false
			}
		}
		if requireCover && !gallery.hasCover {
			continue
		}
		if !found || compareGalleryVersions(gallery, best.gallery) < 0 {
			best, found = hit, true
		}
	}
	return best, found
}

var (
	numericEntryID   = regexp.MustCompile(`^[0-9]+$`)
	entryIDWithToken = regexp.MustCompile(`^([0-9]+)/([A-Za-z0-9_-]+)/?$`)
	galleryEntryPath = regexp.MustCompile(`(?:^|/)g/([0-9]+)/([A-Za-z0-9_-]+)(?:/|$)`)
)

func directEntryID(raw string) (id, token string, ok bool) {
	raw = strings.TrimSpace(raw)
	if numericEntryID.MatchString(raw) {
		return raw, "", true
	}
	if match := entryIDWithToken.FindStringSubmatch(raw); match != nil {
		return match[1], match[2], true
	}
	if u, err := url.Parse(raw); err == nil && (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "") {
		if match := galleryEntryPath.FindStringSubmatch(u.Path); match != nil {
			return match[1], match[2], true
		}
	}
	return "", "", false
}

func getEntryWithToken(ctx context.Context, sidecar *SidecarStore, entryID, token string) (*Entry, error) {
	entry, err := sidecar.GetEntry(ctx, entryID)
	if err == nil && token != "" && entry.Token != "" && !strings.EqualFold(entry.Token, token) {
		return nil, ErrEntryNotFound
	}
	return entry, err
}

func (m *Manager) entry(ctx context.Context, sourceID, entryID string) (*Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if sidecar := m.sidecars[sourceID]; sidecar != nil {
		if id, token, ok := directEntryID(entryID); ok {
			return getEntryWithToken(ctx, sidecar, id, token)
		}
		return sidecar.GetEntry(ctx, entryID)
	}
	return nil, ErrUnavailable
}

type CandidateResult struct {
	Parsed     bangumi.ParsedQuery `json:"parsed"`
	SourceID   string              `json:"sourceId,omitempty"`
	Candidates any                 `json:"candidates"`
}

func (m *Manager) SearchCandidates(ctx context.Context, seriesID, sourceID, query string) (CandidateResult, error) {
	if err := checkAdmin(ctx); err != nil {
		return CandidateResult{}, err
	}
	series, err := m.store.SeriesByIDAny(ctx, seriesID, false)
	if err != nil {
		return CandidateResult{}, err
	}
	if sourceID == TargetSourceBangumi {
		if m.bangumi == nil {
			return CandidateResult{}, ErrUnavailable
		}
		hits, parsed, err := m.bangumi.SearchCandidates(ctx, series, query)
		if hits == nil {
			hits = []bangumi.SubjectCandidate{}
		}
		return CandidateResult{Parsed: parsed, SourceID: sourceID, Candidates: hits}, err
	}
	src, err := m.store.GetMetadataSource(ctx, sourceID)
	if err != nil {
		return CandidateResult{}, err
	}
	if !src.Enabled || !src.Ready || src.Kind != KindOfflineSQLite {
		return CandidateResult{}, ErrUnavailable
	}
	parsed := bangumi.ParseSeriesWithPath(series.Name, series.RelativePath)
	hits := make([]*Entry, 0)
	dict := m.sourceTagDictionary(ctx, src.TagDictSourceID)
	m.mu.RLock()
	defer m.mu.RUnlock()
	sidecar := m.sidecars[sourceID]
	if sidecar == nil {
		return CandidateResult{}, ErrUnavailable
	}
	query = strings.TrimSpace(query)
	if id, token, ok := directEntryID(query); ok {
		entry, err := getEntryWithToken(ctx, sidecar, id, token)
		if err == nil {
			hits = append(hits, entry)
		} else if !errors.Is(err, ErrEntryNotFound) {
			return CandidateResult{}, err
		}
	} else {
		if query != "" {
			parsed = bangumi.ParseSeriesName(query)
		}
		names := append(slices.Clone(parsed.Candidates), firstNonEmpty(query, series.Name))
		seen := make(map[string]bool)
		for _, name := range names {
			matches, err := sidecar.SearchByTitle(ctx, name, 20)
			if err != nil {
				return CandidateResult{}, err
			}
			for _, hit := range matches {
				if seen[hit.EntryID] || len(hits) >= 20 {
					continue
				}
				entry, err := sidecar.GetEntry(ctx, hit.EntryID)
				if err != nil {
					return CandidateResult{}, err
				}
				hits = append(hits, entry)
				seen[hit.EntryID] = true
			}
		}
	}
	for i, hit := range hits {
		entry := *hit
		entry.SourceID = sourceID
		entry.Tags = slices.Clone(hit.Tags)
		if dict != nil {
			entry.Tags = dict.TranslateList(entry.Tags, TagDisplayMode(src.TagDisplayMode))
		}
		hits[i] = &entry
	}
	return CandidateResult{Parsed: parsed, SourceID: sourceID, Candidates: hits}, nil
}

func (m *Manager) BindSeriesExternal(ctx context.Context, seriesID, sourceID, entryID, origin string) (database.SeriesMetadata, error) {
	if err := checkAdmin(ctx); err != nil {
		return database.SeriesMetadata{}, err
	}
	series, err := m.store.SeriesByIDAny(ctx, seriesID, false)
	if err != nil {
		return database.SeriesMetadata{}, err
	}
	if origin == "" {
		origin = "manual"
	}
	if origin != "manual" && origin != "agent" && origin != "mapping" && origin != "auto" {
		return database.SeriesMetadata{}, ErrInvalidConfig
	}
	if sourceID == TargetSourceBangumi {
		if m.bangumi == nil {
			return database.SeriesMetadata{}, ErrUnavailable
		}
		id, ok := bangumi.ParseSubjectIDInput(entryID)
		if !ok {
			return database.SeriesMetadata{}, ErrInvalidConfig
		}
		return m.bangumi.BindSubjectWithOrigin(ctx, series, id, origin)
	}
	src, err := m.store.GetMetadataSource(ctx, sourceID)
	if err != nil {
		return database.SeriesMetadata{}, err
	}
	if !src.Enabled || !src.Ready || src.Kind != KindOfflineSQLite {
		return database.SeriesMetadata{}, ErrUnavailable
	}
	entry, err := m.entry(ctx, sourceID, entryID)
	if err != nil {
		return database.SeriesMetadata{}, err
	}
	saved, err := m.store.ApplyScrapedMetadata(ctx, series, m.externalMetadata(ctx, src, entry, origin, 1))
	if err != nil {
		return saved, err
	}
	if err := m.syncSeriesBooks(ctx, series, src.ID, entry.EntryID, origin); err != nil {
		return saved, err
	}
	return saved, nil
}

func (m *Manager) sourceTagDictionary(ctx context.Context, sourceID string) *TagDictionary {
	if sourceID == "" {
		return nil
	}
	source, err := m.store.GetMetadataSource(ctx, sourceID)
	if err != nil || !source.Enabled || !source.Ready {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.dictionaries[sourceID]
}

func (m *Manager) externalMetadata(ctx context.Context, src database.MetadataSource, entry *Entry, origin string, confidence float64) database.ScrapedMetadata {
	tags := append([]string{}, entry.Tags...)
	if dict := m.sourceTagDictionary(ctx, src.TagDictSourceID); dict != nil {
		tags = dict.TranslateList(tags, TagDisplayMode(src.TagDisplayMode))
	}

	var authors []database.Author
	if detailed, ok := entry.RawFields["detailedAuthors"].([]KomgaAuthor); ok && len(detailed) > 0 {
		authors = make([]database.Author, 0, len(detailed))
		for _, a := range detailed {
			name := strings.TrimSpace(a.Name)
			if name == "" {
				continue
			}
			role := strings.TrimSpace(a.Role)
			if role == "" {
				role = "writer"
			}
			authors = append(authors, database.Author{Name: name, Role: role})
		}
	} else {
		authors = make([]database.Author, 0, len(entry.Authors))
		for _, name := range entry.Authors {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				authors = append(authors, database.Author{Name: trimmed, Role: "writer"})
			}
		}
	}

	altTitles := make([]database.AlternateTitle, 0, 2)
	seenAlt := make(map[string]struct{}, 4)
	addAlt := func(label, title string) {
		title = strings.TrimSpace(title)
		label = strings.TrimSpace(label)
		if title == "" || title == strings.TrimSpace(entry.Title) {
			return
		}
		key := strings.ToLower(label) + "\x00" + strings.ToLower(title)
		if _, dup := seenAlt[key]; dup {
			return
		}
		seenAlt[key] = struct{}{}
		altTitles = append(altTitles, database.AlternateTitle{Label: label, Title: title})
	}
	if alts, ok := entry.RawFields["alternateTitles"].([]KomgaAlternateTitle); ok {
		for _, at := range alts {
			addAlt(at.Label, at.Title)
		}
	}
	if entry.TitleAlt != "" {
		addAlt("", entry.TitleAlt)
	}

	var genres []string
	if rawGenres, ok := entry.RawFields["genres"].([]string); ok && len(rawGenres) > 0 {
		genres = slices.Clone(rawGenres)
	} else {
		genres = []string{}
		if entry.Category != "" {
			genres = append(genres, entry.Category)
		}
	}

	var links []database.Link
	if len(entry.Links) > 0 {
		links = make([]database.Link, 0, len(entry.Links))
		for _, l := range entry.Links {
			if strings.TrimSpace(l.URL) != "" {
				links = append(links, database.Link{Label: strings.TrimSpace(l.Label), URL: strings.TrimSpace(l.URL)})
			}
		}
		if len(links) == 0 {
			links = nil
		}
	}

	var sharingLabels []string
	if len(entry.SharingLabels) > 0 {
		sharingLabels = dedupeStrings(entry.SharingLabels)
	}

	return database.ScrapedMetadata{
		ExtSourceID:      src.ID,
		ExtEntryID:       entry.EntryID,
		ExtCoverURL:      entry.CoverURL,
		MatchOrigin:      origin,
		Confidence:       confidence,
		Status:           entry.Status,
		Title:            entry.Title,
		TitleSort:        entry.TitleSort,
		Summary:          entry.Summary,
		Publisher:        entry.Publisher,
		Language:         entry.Language,
		ReadingDirection: entry.ReadingDirection,
		AgeRating:        entry.AgeRating,
		TotalBookCount:   entry.TotalBookCount,
		ReleaseDate:      entry.ReleaseDate,
		AlternateTitles:  altTitles,
		Authors:          authors,
		Tags:             tags,
		Genres:           genres,
		Links:            links,
		SharingLabels:    sharingLabels,
		IsNSFW:           entry.IsNSFW,
	}
}

func (m *Manager) cachedEmbeddedMetaFromDB(ctx context.Context, seriesID string) *EmbeddedMeta {
	book, err := m.store.FirstBookInSeriesAny(ctx, seriesID)
	if err != nil {
		return nil
	}
	bm, ok, err := m.store.BookMetadataByID(ctx, book.ID)
	if err != nil || !ok {
		return nil
	}
	authors := make([]string, 0, len(bm.Authors))
	for _, a := range bm.Authors {
		if name := strings.TrimSpace(a.Name); name != "" {
			authors = append(authors, name)
		}
	}
	var extURL, extID string
	for _, link := range bm.Links {
		u := strings.TrimSpace(link.URL)
		if u == "" {
			continue
		}
		if _, _, isGallery := directEntryID(u); isGallery {
			extURL = u
			break
		}
		if extURL == "" {
			extURL = u
		}
	}
	title := strings.TrimSpace(bm.Title)
	summary := strings.TrimSpace(bm.Summary)
	if title == "" && len(authors) == 0 && extURL == "" && extID == "" && len(bm.Tags) == 0 && summary == "" {
		return nil
	}
	return &EmbeddedMeta{
		Title:       title,
		Series:      strings.TrimSpace(bm.SeriesGroup),
		Summary:     summary,
		Authors:     dedupeStrings(authors),
		Tags:        slices.Clone(bm.Tags),
		ExternalURL: extURL,
		ExternalID:  extID,
	}
}

type localBookCandidate struct {
	ID         string
	Name       string
	FileID     string
	NumberSort float64
}

type remoteBookEntry struct {
	EntryID     string
	FileNames   []string
	Title       string
	Summary     string
	Number      string
	NumberSort  *float64
	ReleaseDate string
	ISBN        string
	Authors     []database.Author
	Tags        []string
	Links       []database.Link
	CoverURL    string
}

type bookMatchPair struct {
	localIdx  int
	remoteIdx int
}

type parsedBookNumber struct {
	val      float64
	kind     string
	ok       bool
	explicit bool
}

var (
	reBookBracketTag    = regexp.MustCompile(`(?:\[[^\[\]]*\]|【[^【】]*】)`)
	reBookAllParenTag   = regexp.MustCompile(`(?:\[[^\[\]]*\]|【[^【】]*】|\([^()]*\)|（[^（）]*）)`)
	reBookExplicitVol   = regexp.MustCompile(`(?i)(?:第\s*0*(\d+(?:\.\d+)?)\s*[卷冊册巻]|\bvol(?:ume)?\.?\s*0*(\d+(?:\.\d+)?)|(?:^|[\s._(\[-])v0*(\d+(?:\.\d+)?)\b)`)
	reBookExplicitCh    = regexp.MustCompile(`(?i)(?:第\s*0*(\d+(?:\.\d+)?)\s*[話话回章]|\b(?:ch(?:apter)?|ep(?:isode)?)\.?\s*0*(\d+(?:\.\d+)?))`)
	reBookTrailingNum   = regexp.MustCompile(`(?:^|[\s._#-])0*(\d{1,4}(?:\.\d+)?)\s*$`)
	bookArchiveExts     = map[string]struct{}{".cbz": {}, ".cbr": {}, ".cb7": {}, ".cbt": {}, ".zip": {}, ".rar": {}, ".7z": {}, ".tar": {}, ".pdf": {}, ".epub": {}, ".mobi": {}, ".azw3": {}}
	bookSeparatorsClean = strings.NewReplacer("_", " ", "　", " ")
)

func (m *Manager) syncSeriesBooks(ctx context.Context, series database.Series, sourceID, entryID, origin string) error {
	if sourceID == "" || sourceID == TargetSourceBangumi || entryID == "" {
		return nil
	}
	src, err := m.store.GetMetadataSource(ctx, sourceID)
	if err != nil {
		return err
	}
	// Online-only enrichment has no offline book catalog to align.
	if !src.Ready {
		return nil
	}
	m.mu.RLock()
	sidecar := m.sidecars[sourceID]
	m.mu.RUnlock()
	if sidecar == nil || sidecar.upstreamDB == nil {
		return ErrUnavailable
	}

	displayMode := TagDisplayMode(src.TagDisplayMode)
	dict := m.sourceTagDictionary(ctx, src.TagDictSourceID)

	var remotes []remoteBookEntry
	switch sidecar.Schema() {
	case SchemaKomgaSQLiteV1:
		ks, err := ReadKomgaSeries(ctx, sidecar.upstreamDB, entryID)
		if errors.Is(err, ErrEntryNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		remotes = make([]remoteBookEntry, 0, len(ks.Books))
		for _, bk := range ks.Books {
			tags := dedupeStrings(bk.Tags)
			if dict != nil && len(tags) > 0 {
				tags = dict.TranslateList(tags, displayMode)
			}
			var authors []database.Author
			if len(bk.Authors) > 0 {
				authors = make([]database.Author, 0, len(bk.Authors))
				for _, a := range bk.Authors {
					if name := strings.TrimSpace(a.Name); name != "" {
						role := strings.TrimSpace(a.Role)
						if role == "" {
							role = "writer"
						}
						authors = append(authors, database.Author{Name: name, Role: role})
					}
				}
			}
			var links []database.Link
			if len(bk.Links) > 0 {
				links = make([]database.Link, 0, len(bk.Links))
				for _, l := range bk.Links {
					if u := strings.TrimSpace(l.URL); u != "" {
						links = append(links, database.Link{Label: strings.TrimSpace(l.Label), URL: u})
					}
				}
			}
			var coverURL string
			if bk.SelectedThumbnailID != "" {
				coverURL = EncodeSQLiteBlobCoverRef(sourceID, SQLiteBlobKindKomgaBook, bk.SelectedThumbnailID)
			}
			var files []string
			if bk.Name != "" {
				files = append(files, bk.Name)
			}
			if tail := komgaPathTail(bk.URL); tail != "" {
				files = append(files, tail)
			}
			remotes = append(remotes, remoteBookEntry{
				EntryID:     bk.KomgaBookID,
				FileNames:   files,
				Title:       bk.Title,
				Summary:     bk.Summary,
				Number:      bk.Number,
				NumberSort:  bk.NumberSort,
				ReleaseDate: bk.ReleaseDate,
				ISBN:        bk.ISBN,
				Authors:     authors,
				Tags:        tags,
				Links:       links,
				CoverURL:    coverURL,
			})
		}
	case SchemaMangaAlphaMetaV1, SchemaMangaAlphaMetaV2:
		openBooks, err := ReadOpenOfflineBooks(ctx, sidecar.upstreamDB, entryID)
		if err != nil {
			return err
		}
		if len(openBooks) == 0 {
			return nil
		}
		remotes = make([]remoteBookEntry, 0, len(openBooks))
		for _, bk := range openBooks {
			tags := dedupeStrings(bk.Tags)
			if dict != nil && len(tags) > 0 {
				tags = dict.TranslateList(tags, displayMode)
			}
			var authors []database.Author
			if len(bk.Authors) > 0 {
				authors = make([]database.Author, 0, len(bk.Authors))
				for _, a := range bk.Authors {
					if name := strings.TrimSpace(a.Name); name != "" {
						role := strings.TrimSpace(a.Role)
						if role == "" {
							role = "writer"
						}
						authors = append(authors, database.Author{Name: name, Role: role})
					}
				}
			}
			var links []database.Link
			if len(bk.Links) > 0 {
				links = make([]database.Link, 0, len(bk.Links))
				for _, l := range bk.Links {
					if u := strings.TrimSpace(l.URL); u != "" {
						links = append(links, database.Link{Label: strings.TrimSpace(l.Label), URL: u})
					}
				}
			}
			coverURL := strings.TrimSpace(bk.CoverURL)
			if coverURL == "" && bk.HasCoverBlob {
				coverURL = EncodeSQLiteBlobCoverRef(sourceID, SQLiteBlobKindOpenBook, openBookCoverKey(bk.SeriesEntryID, bk.BookKey))
			}
			var files []string
			if bk.FileName != "" {
				files = append(files, bk.FileName)
			}
			if bk.BookKey != "" {
				files = append(files, bk.BookKey)
			}
			remotes = append(remotes, remoteBookEntry{
				EntryID:     bk.BookKey,
				FileNames:   files,
				Title:       bk.Title,
				Summary:     bk.Summary,
				Number:      bk.Number,
				NumberSort:  bk.NumberSort,
				ReleaseDate: bk.ReleaseDate,
				ISBN:        bk.ISBN,
				Authors:     authors,
				Tags:        tags,
				Links:       links,
				CoverURL:    coverURL,
			})
		}
	default:
		return nil
	}

	if len(remotes) == 0 {
		return nil
	}

	rows, err := m.store.DB().QueryContext(ctx, `
SELECT id, name, coalesce(file_id,''), number_sort
FROM books
WHERE series_id=?
ORDER BY number_sort ASC, name COLLATE NOCASE ASC, id ASC`, series.ID)
	if err != nil {
		return err
	}
	var locals []localBookCandidate
	for rows.Next() {
		var item localBookCandidate
		if scanErr := rows.Scan(&item.ID, &item.Name, &item.FileID, &item.NumberSort); scanErr != nil {
			rows.Close()
			return scanErr
		}
		locals = append(locals, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(locals) == 0 {
		return nil
	}

	pairs := alignSeriesBooks(locals, remotes)
	if len(pairs) == 0 {
		return nil
	}
	return m.writeAlignedSeriesBooks(ctx, series.ID, sourceID, entryID, origin, locals, remotes, pairs)
}

func (m *Manager) writeAlignedSeriesBooks(ctx context.Context, seriesID, sourceID, entryID, origin string, locals []localBookCandidate, remotes []remoteBookEntry, pairs []bookMatchPair) error {
	// The store starts immediate transactions. Hold the writer lock from the
	// parent binding check through all book writes so a rebind cannot interleave.
	tx, err := m.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var bound bool
	if err := tx.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM series_metadata WHERE series_id=? AND ext_source_id=? AND ext_entry_id=? AND match_origin=?)`,
		seriesID, sourceID, entryID, origin).Scan(&bound); err != nil {
		return err
	}
	if !bound {
		return database.ErrMetadataChanged
	}
	automatic := origin == "auto" || origin == "mapping"
	for _, pair := range pairs {
		loc, rem := locals[pair.localIdx], remotes[pair.remoteIdx]
		if rem.NumberSort != nil && (math.IsNaN(*rem.NumberSort) || math.IsInf(*rem.NumberSort, 0)) {
			return errors.New("book number sort must be finite")
		}
		authors, tags, links := bookEnrichmentJSON(rem)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO book_metadata(book_id,title,summary,number,number_sort,release_date,isbn,authors_json,tags_json,links_json,
 ext_source_id,ext_entry_id,ext_cover_url,match_origin,updated_at)
SELECT id,?,?,?,?,?,?,coalesce(?,'[]'),coalesce(?,'[]'),coalesce(?,'[]'),?,?,?,?,?
FROM books WHERE id=? AND series_id=?
ON CONFLICT(book_id) DO UPDATE SET
 title=CASE WHEN NOT book_metadata.title_lock AND excluded.title<>'' THEN excluded.title ELSE book_metadata.title END,
 summary=CASE WHEN NOT book_metadata.summary_lock AND excluded.summary<>'' THEN excluded.summary ELSE book_metadata.summary END,
 number=CASE WHEN NOT book_metadata.number_lock AND excluded.number<>'' THEN excluded.number ELSE book_metadata.number END,
 number_sort=CASE WHEN NOT book_metadata.number_sort_lock THEN coalesce(excluded.number_sort,book_metadata.number_sort) ELSE book_metadata.number_sort END,
 release_date=CASE WHEN NOT book_metadata.release_date_lock AND excluded.release_date<>'' THEN excluded.release_date ELSE book_metadata.release_date END,
 isbn=CASE WHEN NOT book_metadata.isbn_lock AND excluded.isbn<>'' THEN excluded.isbn ELSE book_metadata.isbn END,
 authors_json=CASE WHEN NOT book_metadata.authors_lock AND excluded.authors_json<>'[]' THEN excluded.authors_json ELSE book_metadata.authors_json END,
 tags_json=CASE WHEN NOT book_metadata.tags_lock AND excluded.tags_json<>'[]' THEN excluded.tags_json ELSE book_metadata.tags_json END,
 links_json=CASE WHEN NOT book_metadata.links_lock AND excluded.links_json<>'[]' THEN excluded.links_json ELSE book_metadata.links_json END,
 ext_cover_path=CASE WHEN (book_metadata.ext_source_id,book_metadata.ext_entry_id,book_metadata.ext_cover_url)
  IS (excluded.ext_source_id,excluded.ext_entry_id,excluded.ext_cover_url) THEN book_metadata.ext_cover_path ELSE '' END,
 ext_source_id=excluded.ext_source_id,ext_entry_id=excluded.ext_entry_id,ext_cover_url=excluded.ext_cover_url,
 match_origin=excluded.match_origin,updated_at=excluded.updated_at
WHERE NOT ? OR (book_metadata.match_origin NOT IN ('manual','agent')
 AND (book_metadata.match_origin<>'' OR (book_metadata.bgm_subject_id=0 AND book_metadata.ext_entry_id='')))`,
			strings.TrimSpace(rem.Title), strings.TrimSpace(rem.Summary), strings.TrimSpace(rem.Number),
			rem.NumberSort, strings.TrimSpace(rem.ReleaseDate), strings.TrimSpace(rem.ISBN), authors, tags, links,
			sourceID, rem.EntryID, rem.CoverURL, origin, timestamp(), loc.ID, seriesID, automatic); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func bookEnrichmentJSON(rem remoteBookEntry) (authorsJSON, tagsJSON, linksJSON any) {
	var authors []database.Author
	seenAuthors := make(map[string]bool, len(rem.Authors))
	for _, author := range rem.Authors {
		author.Role = firstNonEmpty(strings.ToLower(author.Role), "writer")
		key := strings.ToLower(author.Name) + "\x00" + author.Role
		if !seenAuthors[key] {
			seenAuthors[key] = true
			authors = append(authors, author)
		}
	}
	var links []database.Link
	seenLinks := make(map[string]bool, len(rem.Links))
	for _, link := range rem.Links {
		link.Label = firstNonEmpty(link.Label, "Link")
		if !seenLinks[link.URL] {
			seenLinks[link.URL] = true
			links = append(links, link)
		}
	}
	if len(authors) > 0 {
		raw, _ := json.Marshal(authors)
		authorsJSON = string(raw)
	}
	if tags := dedupeStrings(rem.Tags); len(tags) > 0 {
		raw, _ := json.Marshal(tags)
		tagsJSON = string(raw)
	}
	if len(links) > 0 {
		raw, _ := json.Marshal(links)
		linksJSON = string(raw)
	}
	return authorsJSON, tagsJSON, linksJSON
}

func alignSeriesBooks(locals []localBookCandidate, remotes []remoteBookEntry) []bookMatchPair {
	usedL := make([]bool, len(locals))
	usedR := make([]bool, len(remotes))
	var pairs []bookMatchPair

	localNames := make([][]string, len(locals))
	localNums := make([]parsedBookNumber, len(locals))
	localExtras := make([]bool, len(locals))
	for i, loc := range locals {
		localNames[i] = []string{loc.Name, path.Base(strings.ReplaceAll(loc.FileID, "\\", "/"))}
		localNums[i], localExtras[i] = bookMatchMarkers(localNames[i], "", nil)
	}
	remoteNums := make([]parsedBookNumber, len(remotes))
	remoteExtras := make([]bool, len(remotes))
	for j, rem := range remotes {
		names := append(slices.Clone(rem.FileNames), rem.Title)
		remoteNums[j], remoteExtras[j] = bookMatchMarkers(names, rem.Number, rem.NumberSort)
	}
	compatible := func(i, j int) bool {
		return localExtras[i] == remoteExtras[j] && !bookNumbersConflict(localNums[i], remoteNums[j])
	}

	matchUniqueKeyTier := func(localKeysFn, remoteKeysFn func(int) []string) {
		localMap := make(map[string][]int, len(locals))
		for i := range locals {
			if usedL[i] {
				continue
			}
			for _, k := range localKeysFn(i) {
				localMap[k] = append(localMap[k], i)
			}
		}
		remoteMap := make(map[string][]int, len(remotes))
		for j := range remotes {
			if usedR[j] {
				continue
			}
			for _, k := range remoteKeysFn(j) {
				remoteMap[k] = append(remoteMap[k], j)
			}
		}
		keys := make([]string, 0, len(localMap))
		for k := range localMap {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			ls, rs := localMap[k], remoteMap[k]
			if len(ls) == 1 && len(rs) == 1 && !usedL[ls[0]] && !usedR[rs[0]] && compatible(ls[0], rs[0]) {
				usedL[ls[0]], usedR[rs[0]] = true, true
				pairs = append(pairs, bookMatchPair{localIdx: ls[0], remoteIdx: rs[0]})
			}
		}
	}

	// Match exact filenames, normalized filenames, then unambiguous numbering.
	for _, stripBrackets := range []bool{false, true} {
		matchUniqueKeyTier(
			func(i int) []string { return bookFilenameKeys(stripBrackets, localNames[i]...) },
			func(j int) []string { return bookFilenameKeys(stripBrackets, remotes[j].FileNames...) },
		)
	}
	numberKeys := func(number parsedBookNumber, extra bool) []string {
		if !number.ok || extra {
			return nil
		}
		return []string{fmt.Sprintf("%s:%.3f", number.kind, number.val)}
	}
	matchUniqueKeyTier(
		func(i int) []string { return numberKeys(localNums[i], localExtras[i]) },
		func(j int) []string { return numberKeys(remoteNums[j], remoteExtras[j]) },
	)

	if len(locals) == 1 && len(remotes) == 1 && !usedL[0] && compatible(0, 0) {
		return append(pairs, bookMatchPair{localIdx: 0, remoteIdx: 0})
	}

	// Tier 3b: Normalized title similarity match on remaining books.
	for li := range locals {
		if usedL[li] {
			continue
		}
		lKeys := bookNormalizedTitleKeys(localNames[li]...)
		if len(lKeys) == 0 {
			continue
		}
		bestIdx := -1
		bestScore := 0.0
		secondScore := 0.0
		for rj := range remotes {
			if usedR[rj] || !compatible(li, rj) {
				continue
			}
			candidates := append([]string{remotes[rj].Title}, remotes[rj].FileNames...)
			rKeys := bookNormalizedTitleKeys(candidates...)
			score := 0.0
			for _, lk := range lKeys {
				for _, rk := range rKeys {
					if s := scoreNormalizedPair(lk, rk); s > score {
						score = s
					}
				}
			}
			if score > bestScore {
				secondScore = bestScore
				bestScore = score
				bestIdx = rj
			} else if score > secondScore {
				secondScore = score
			}
		}
		if bestIdx >= 0 && bestScore >= 0.80 && bestScore-secondScore >= 0.05 {
			usedL[li], usedR[bestIdx] = true, true
			pairs = append(pairs, bookMatchPair{localIdx: li, remoteIdx: bestIdx})
		}
	}
	return pairs
}

func stripBookArchiveExt(raw string) string {
	base := strings.TrimSpace(raw)
	if base == "" {
		return ""
	}
	ext := strings.ToLower(path.Ext(base))
	if _, ok := bookArchiveExts[ext]; ok {
		base = strings.TrimSuffix(base, base[len(base)-len(ext):])
	}
	return strings.TrimSpace(base)
}

func bookFilenameKeys(stripBrackets bool, inputs ...string) []string {
	var keys []string
	for _, raw := range inputs {
		stem := stripBookArchiveExt(raw)
		if stem == "" {
			continue
		}
		if stripBrackets {
			stem = reBookBracketTag.ReplaceAllString(stem, " ")
		}
		stem = strings.TrimSpace(bookSeparatorsClean.Replace(stem))
		if key := cjk.ExactTitleKey(stem); key != "" && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

func bookNormalizedTitleKeys(inputs ...string) []string {
	var keys []string
	for _, raw := range inputs {
		stem := stripBookArchiveExt(raw)
		if stem == "" {
			continue
		}
		stem = reBookAllParenTag.ReplaceAllString(stem, " ")
		stem = strings.TrimSpace(bookSeparatorsClean.Replace(stem))
		if key := cjk.ExactTitleKey(stem); key != "" && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

func bookMatchMarkers(names []string, number string, sort *float64) (parsedBookNumber, bool) {
	var parsed parsedBookNumber
	var extra bool
	for _, name := range names {
		stem := stripBookArchiveExt(name)
		extra = extra || bangumi.IsVolumeExtra(stem)
		candidate := parseBookNumber(name, "", nil)
		if candidate.ok && (!parsed.ok || candidate.explicit && !parsed.explicit) {
			parsed = candidate
		}
	}
	if !parsed.ok {
		parsed = parseBookNumber("", number, sort)
	}
	return parsed, extra
}

func parseBookNumber(rawName, numberStr string, numberSort *float64) parsedBookNumber {
	stem := stripBookArchiveExt(rawName)
	if stem != "" {
		if m := reBookExplicitVol.FindStringSubmatch(stem); len(m) >= 4 {
			numRaw := firstNonEmpty(m[1], m[2], m[3])
			if v, err := strconv.ParseFloat(numRaw, 64); err == nil {
				return parsedBookNumber{val: v, kind: "vol", ok: true, explicit: true}
			}
		}
		if m := reBookExplicitCh.FindStringSubmatch(stem); len(m) >= 3 {
			numRaw := firstNonEmpty(m[1], m[2])
			if v, err := strconv.ParseFloat(numRaw, 64); err == nil {
				return parsedBookNumber{val: v, kind: "ch", ok: true, explicit: true}
			}
		}
		cleaned := strings.TrimSpace(bookSeparatorsClean.Replace(reBookAllParenTag.ReplaceAllString(stem, " ")))
		if m := reBookTrailingNum.FindStringSubmatch(cleaned); len(m) == 2 {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil && (v < 1900 || v > 2099) {
				return parsedBookNumber{val: v, kind: "vol", ok: true}
			}
		}
	}
	if n := strings.TrimSpace(numberStr); n != "" {
		if v, err := strconv.ParseFloat(n, 64); err == nil && v >= 0 {
			return parsedBookNumber{val: v, kind: "vol", ok: true}
		}
	}
	if numberSort != nil && *numberSort >= 0 {
		return parsedBookNumber{val: *numberSort, kind: "vol", ok: true}
	}
	return parsedBookNumber{}
}

func bookNumbersConflict(a, b parsedBookNumber) bool {
	if !a.ok || !b.ok {
		return false
	}
	return a.kind != b.kind || math.Abs(a.val-b.val) >= 0.001
}

func (m *Manager) ClearSeriesExternal(ctx context.Context, seriesID string) (database.SeriesMetadata, error) {
	if err := checkAdmin(ctx); err != nil {
		return database.SeriesMetadata{}, err
	}
	series, err := m.store.SeriesByIDAny(ctx, seriesID, false)
	if err != nil {
		return database.SeriesMetadata{}, err
	}
	return m.store.ClearSeriesExternalBinding(ctx, series)
}

type MappingExport struct {
	Count          int             `json:"count"`
	ExcludedNSFW   int             `json:"excludedNsfw"`
	SkippedPrivacy int             `json:"skippedPrivacy"`
	Sample         []MappingRecord `json:"sample"`
	NDJSON         []byte          `json:"-"`
}

func (m *Manager) ExportCommunityMappings(ctx context.Context, filter database.MappingExportFilter, preview bool) (MappingExport, error) {
	if err := checkAdmin(ctx); err != nil {
		return MappingExport{}, err
	}
	safeOnly := !filter.IncludeNSFW
	filter.IncludeNSFW = true // Collect the exclusion count from the same selection.
	rows, err := m.store.ListSeriesForMappingExport(ctx, filter)
	if err != nil {
		return MappingExport{}, err
	}
	plan, err := m.scrapePlan(ctx)
	if err != nil {
		return MappingExport{}, err
	}
	result := MappingExport{Sample: []MappingRecord{}}
	records := make([]MappingRecord, 0, len(rows))
	for _, row := range rows {
		if safeOnly && row.IsNSFW {
			result.ExcludedNSFW++
			continue
		}
		if strings.ContainsAny(row.FolderName, "/\\") || containsSensitiveOrPrivateValue(row.FolderName) || hasControlChars(row.FolderName) {
			result.SkippedPrivacy++
			continue
		}
		parsed := bangumi.ParseSeriesName(row.FolderName)
		key := firstNonEmpty(append(parsed.Candidates, row.FolderName)...)
		var author string
		if len(parsed.Authors) > 0 {
			author = parsed.Authors[0]
		} else if len(row.Authors) > 0 {
			author = row.Authors[0].Name
		}
		rec := MappingRecord{
			Schema: MappingSchemaV1, Key: key, RawKey: row.FolderName, Author: author,
			IsNSFW: row.IsNSFW, Origin: row.MatchOrigin,
		}
		switch {
		case row.BgmSubjectID != nil:
			rec.TargetSource, rec.TargetID = TargetSourceBangumi, strconv.FormatInt(*row.BgmSubjectID, 10)
		default:
			rec.TargetID = row.ExtEntryID
			m.mu.RLock()
			if sidecar := m.sidecars[row.ExtSourceID]; sidecar != nil {
				rec.TargetSource = sidecar.Schema()
			}
			m.mu.RUnlock()
			if rec.TargetSource == "" {
				rec.TargetSource = plan.sources[row.ExtSourceID].SchemaType
			}
		}
		if !isValidTargetSource(rec.TargetSource) {
			continue
		}
		if _, ok := sanitizeMappingRecord(rec, true); !ok {
			result.SkippedPrivacy++
			continue
		}
		records = append(records, rec)
	}
	data, err := SanitizeAndExportMappings(records, ExportMappingsOptions{IncludeNSFW: !safeOnly})
	if err != nil {
		return MappingExport{}, err
	}
	result.Count = bytes.Count(data, []byte{'\n'})
	sample := bytes.SplitAfterN(data, []byte{'\n'}, 31)
	result.Sample, err = ParseMappingsNDJSON(bytes.NewReader(bytes.Join(sample[:min(30, len(sample))], nil)))
	if result.Sample == nil {
		result.Sample = []MappingRecord{}
	}
	if !preview {
		result.NDJSON = data
	}
	return result, err
}
