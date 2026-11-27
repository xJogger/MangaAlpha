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
	ID            string   `json:"id"`
	LibraryID     string   `json:"libraryId"`
	LibraryIDs    []string `json:"libraryIds,omitempty"`
	LibraryName   string   `json:"libraryName,omitempty"`
	CurrentSeries string   `json:"currentSeries,omitempty"`
	Mode          string   `json:"mode"`
	Status        string   `json:"status"`
	AllowOnline   bool     `json:"allowOnline"`
	StartedAt     string   `json:"startedAt"`
	CompletedAt   string   `json:"completedAt,omitempty"`
	TotalSeries   int      `json:"totalSeries"`
	Processed     int      `json:"processedSeries"`
	Matched       int      `json:"matchedCount"`
	Mapped        int      `json:"mappingCount"`
	Unmatched     int      `json:"unmatchedCount"`
	Skipped       int      `json:"skippedCount"`
	Failed        int      `json:"failedCount"`
	Error         string   `json:"error,omitempty"`
}

type scrapeSeries struct {
	series    database.Series
	coverSHA1 string
	bound     bool
	subjectID int64
	origin    string
}

type scrapePlan struct {
	bangumiDisabled bool
	sources         map[string]database.MetadataSource
	chains          map[string][]database.LibraryMetadataSource
	mappings        *MappingIndex
	readySidecars   map[string]bool
	bySchema        map[string][]string
	online          []database.MetadataSource
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
			(bgmReady && ((link.SourceID == TargetSourceBangumi && !p.bangumiDisabled) || (p.sources[link.SourceID].Kind == KindBangumiArchive && p.sources[link.SourceID].Ready))) {
			return true
		}
	}
	return false
}

func (m *Manager) HasReadySourcesForLibrary(ctx context.Context, libraryID string, allowOnline bool) (bool, error) {
	var ids []string
	if libraryID != "" {
		ids = []string{libraryID}
	}
	return m.HasReadySourcesForLibraries(ctx, ids, allowOnline)
}

func (m *Manager) HasReadySourcesForLibraries(ctx context.Context, ids []string, allowOnline bool) (bool, error) {
	if err := checkAdmin(ctx); err != nil {
		return false, err
	}
	plan, err := m.scrapePlan(ctx)
	if err != nil {
		return false, err
	}
	if allowOnline && len(plan.online) > 0 {
		return true, nil
	}
	bgmReady := m.bangumi != nil && m.bangumi.ArchiveStatus().Ready
	if len(ids) != 0 {
		for _, id := range ids {
			if plan.hasReadyChain(id, bgmReady) {
				return true, nil
			}
		}
		return false, nil
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

type scrapeLibrary struct{ id, name string }

func (m *Manager) topLevelLibraries(ctx context.Context) ([]scrapeLibrary, error) {
	rows, err := m.store.DB().QueryContext(ctx, "SELECT l.id,l.name FROM libraries l WHERE "+topLevelLibraryFilter+" ORDER BY l.id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var libs []scrapeLibrary
	for rows.Next() {
		var lib scrapeLibrary
		if err := rows.Scan(&lib.id, &lib.name); err != nil {
			return nil, err
		}
		libs = append(libs, lib)
	}
	return libs, rows.Err()
}

func scrapeLibraryLabel(ids []string, names map[string]string) string {
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		labels = append(labels, names[id])
	}
	return formatScrapeLibraryNames(labels)
}

func formatScrapeLibraryNames(names []string) string {
	switch len(names) {
	case 0:
		return "全部未隐藏书库"
	case 1, 2:
		return strings.Join(names, "、")
	default:
		return fmt.Sprintf("%s 等 %d 个书库", strings.Join(names[:2], "、"), len(names))
	}
}

func (m *Manager) StartMultiScrape(ctx context.Context, libraryID, mode string, allowOnline bool, requested ...[]string) (ScrapeRun, error) {
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
	var ids []string
	if len(requested) > 0 {
		ids = requested[0]
	}
	if len(ids) == 0 && libraryID != "" {
		ids = []string{libraryID}
	}

	eligible, err := m.topLevelLibraries(ctx)
	if err != nil {
		return ScrapeRun{}, err
	}
	names := make(map[string]string, len(eligible))
	for _, lib := range eligible {
		names[lib.id] = lib.name
	}
	selected := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if _, ok := names[id]; !ok {
			if _, err := m.store.LibraryBasic(ctx, id); err != nil {
				return ScrapeRun{}, err
			}
			return ScrapeRun{}, ErrUnavailable
		}
		if !seen[id] {
			selected = append(selected, id)
			seen[id] = true
		}
	}
	ready, err := m.HasReadySourcesForLibraries(ctx, selected, allowOnline)
	if err != nil {
		return ScrapeRun{}, err
	}
	if !ready {
		return ScrapeRun{}, ErrNoReadySources
	}
	all := len(selected) == 0 || (len(selected) > 1 && len(selected) == len(eligible))
	if len(selected) == 0 {
		for _, lib := range eligible {
			selected = append(selected, lib.id)
		}
	}
	versions := make(map[string]string, len(selected))
	states, err := m.store.ListAllLibraryMetadataChainStates(ctx)
	if err != nil {
		return ScrapeRun{}, err
	}
	for _, id := range selected {
		versions[id] = states[id].ChainVersion
	}
	storedIDs := selected
	if all {
		storedIDs = nil
	}
	libraryID = ""
	if len(storedIDs) == 1 {
		libraryID = storedIDs[0]
	}
	libraryName := scrapeLibraryLabel(storedIDs, names)
	idsJSON := ""
	if len(storedIDs) > 0 {
		raw, _ := json.Marshal(storedIDs)
		idsJSON = string(raw)
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
		ID: "ext_" + newRunID(), LibraryID: libraryID, LibraryName: libraryName, LibraryIDs: storedIDs, Mode: mode, AllowOnline: allowOnline,
		Status: "running", StartedAt: timestamp(),
	}
	storedMode := mode
	if allowOnline {
		storedMode += "+online"
	}
	if _, err := m.store.DB().ExecContext(ctx, `
INSERT INTO bangumi_scrape_runs(id,library_id,library_ids_json,mode,force,status,started_at,created_at)
VALUES(?,nullif(?,''),?,?,?,'running',?,?)`, run.ID, libraryID, idsJSON, storedMode, mode == bangumi.ScrapeModeForce,
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
		m.executeScrape(jobCtx, run, flush, selected, versions)
	})
	response := run
	response.LibraryIDs = slices.Clone(run.LibraryIDs)
	return response, nil
}

func (m *Manager) executeScrape(ctx context.Context, run ScrapeRun, flush <-chan struct{}, selected []string, versions map[string]string) {
	started := time.Now()
	flushCtx, stopFlush := context.WithCancel(ctx)
	flushed := make(chan struct{})
	initial := run
	go func() {
		defer close(flushed)
		m.flushScrapeProgress(flushCtx, initial, flush)
	}()

	err := m.multiScrape(database.WithAutomaticMetadata(ctx), &run, selected)
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
	if run.Status == "success" && run.Mode == bangumi.ScrapeModeForce {
		for id, version := range versions {
			if err := m.store.MarkLibraryMetadataChainApplied(context.Background(), id, version); err != nil && m.logger != nil {
				m.logger.Warn("could not mark metadata chain applied", "libraryId", id)
			}
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
		if (run.Processed != saved.Processed || run.TotalSeries != saved.TotalSeries || run.CurrentSeries != saved.CurrentSeries) && m.saveScrapeRun(ctx, run) == nil {
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
SELECT r.id,coalesce(r.library_id,''),r.library_ids_json,coalesce((
 SELECT json_group_array(coalesce(l.name, x.value)) FROM json_each(
 CASE WHEN r.library_ids_json<>'' THEN r.library_ids_json
      WHEN r.library_id IS NOT NULL THEN json_array(r.library_id) ELSE '[]' END
 ) x LEFT JOIN libraries l ON l.id=x.value
),'[]'),r.mode,r.status,
 coalesce(r.started_at,''),coalesce(r.completed_at,''),r.total_series,r.processed_series,
 r.matched_count,r.unmatched_count,r.skipped_count,r.failed_count,r.current_series,r.errors_json
FROM bangumi_scrape_runs r
WHERE r.id LIKE 'ext_%'`

func scanStoredScrapeRun(row interface{ Scan(...any) error }) (ScrapeRun, error) {
	var run ScrapeRun
	var errorsJSON, idsJSON, namesJSON string
	if err := row.Scan(&run.ID, &run.LibraryID, &idsJSON, &namesJSON, &run.Mode, &run.Status, &run.StartedAt,
		&run.CompletedAt, &run.TotalSeries, &run.Processed, &run.Matched, &run.Unmatched, &run.Skipped,
		&run.Failed, &run.CurrentSeries, &errorsJSON); err != nil {
		return ScrapeRun{}, err
	}
	_ = json.Unmarshal([]byte(idsJSON), &run.LibraryIDs)
	if len(run.LibraryIDs) == 0 && run.LibraryID != "" {
		run.LibraryIDs = []string{run.LibraryID}
	}
	var names []string
	_ = json.Unmarshal([]byte(namesJSON), &names)
	run.LibraryName = formatScrapeLibraryNames(names)
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
			live.LibraryIDs = slices.Clone(live.LibraryIDs)
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
	if m.scrapeCancel != nil {
		run := m.scrapes[len(m.scrapes)-1]
		if run.LibraryID == libraryID || slices.Contains(run.LibraryIDs, libraryID) {
			m.scrapeCancel()
		}
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
			if src.ID == TargetSourceBangumi {
				plan.bangumiDisabled = true
			}
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

func (m *Manager) scrapeTargets(ctx context.Context, ids []string, mode string) ([]scrapeSeries, error) {
	query := `
SELECT s.id,s.library_id,s.name,s.relative_path,s.one_shot,
       coalesce(sm.ext_cover_url,''),
       (coalesce(sm.bgm_subject_id,0)>0 OR coalesce(sm.ext_entry_id,'')<>''),coalesce(sm.bgm_subject_id,0),coalesce(sm.match_origin,'')
FROM series s JOIN libraries l ON l.id=s.library_id
LEFT JOIN series_metadata sm ON sm.series_id=s.id
WHERE ` + topLevelLibraryFilter + `
  AND coalesce(sm.match_origin,'') NOT IN ('manual','agent')
`
	if ids == nil {
		ids = []string{}
	}
	raw, _ := json.Marshal(ids)
	query += " AND s.library_id IN (SELECT value FROM json_each(?))"
	if mode != bangumi.ScrapeModeForce {
		query += " AND NOT (coalesce(sm.match_origin,'')='' AND (coalesce(sm.bgm_subject_id,0)>0 OR coalesce(sm.ext_entry_id,'')<>''))"
	}
	switch mode {
	case bangumi.ScrapeModeIncremental:
		query += " AND coalesce(sm.scrape_status,'none')='none'"
	case bangumi.ScrapeModeRetryUnmatched:
		query += " AND coalesce(sm.scrape_status,'none')<>'matched'"
	}
	rows, err := m.store.DB().QueryContext(ctx, query+" ORDER BY s.library_id,s.id", string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scrapeSeries
	for rows.Next() {
		var target scrapeSeries
		var coverURL string
		if err := rows.Scan(&target.series.ID, &target.series.LibraryID, &target.series.Name,
			&target.series.RelativePath, &target.series.OneShot, &coverURL, &target.bound, &target.subjectID, &target.origin); err != nil {
			return nil, err
		}
		target.coverSHA1 = ExtractThumbSHA1(coverURL)
		out = append(out, target)
	}
	return out, rows.Err()
}

func (m *Manager) multiScrape(ctx context.Context, run *ScrapeRun, selected []string) error {
	plan, err := m.scrapePlan(ctx)
	if err != nil {
		return err
	}
	targets, err := m.scrapeTargets(ctx, selected, run.Mode)
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
		hits, err := m.matchOffline(ctx, target, plan, run.Mode == bangumi.ScrapeModeForce)
		if err == nil && len(hits) == 0 && !target.bound && run.AllowOnline && len(plan.online) > 0 {
			pending = append(pending, target)
			continue
		}
		var incoming database.ScrapedMetadata
		if err == nil && len(hits) > 0 {
			incoming, _, err = m.applyOfflineHits(ctx, target.series, hits, run.Mode == bangumi.ScrapeModeForce, &target.subjectID)
		} else if err == nil && run.Mode == bangumi.ScrapeModeForce && target.bound && target.subjectID == 0 && target.origin != "" {
			inferred := bangumi.InferPathNSFW(target.series.Name, target.series.RelativePath)
			_, _, err = m.store.ApplyMetadataChain(ctx, target.series, database.ChainSeriesMatch{
				Metadata: database.ScrapedMetadata{IsNSFW: inferred}, Replace: true, MarkUnmatched: true, ExpectedSubjectID: &target.subjectID,
			})
		} else if err == nil && !target.bound {
			inferred := bangumi.InferPathNSFW(target.series.Name, target.series.RelativePath)
			err = m.store.MarkSeriesScrapeStatusWithNSFW(ctx, target.series, database.ScrapeStatusUnmatched, 0, inferred)
		}
		m.recordScrapeResult(run, incoming, len(hits) > 0, err)
	}
	return m.enrichOnline(ctx, run, plan.online, pending)
}

func (m *Manager) finishSeries(ctx context.Context, run *ScrapeRun, target scrapeSeries, incoming database.ScrapedMetadata, matched bool, err error) error {
	if err == nil {
		switch {
		case matched:
			_, err = m.store.ApplyScrapedMetadata(ctx, target.series, incoming)

		case !target.bound:
			inferred := bangumi.InferPathNSFW(target.series.Name, target.series.RelativePath)
			err = m.store.MarkSeriesScrapeStatusWithNSFW(ctx, target.series, database.ScrapeStatusUnmatched, 0, inferred)
		}
	}
	m.recordScrapeResult(run, incoming, matched, err)
	return err
}

func (m *Manager) recordScrapeResult(run *ScrapeRun, incoming database.ScrapedMetadata, matched bool, err error) {
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
	hit := offlineHit{source: src, sourceID: src.ID, entry: entry, meta: m.externalMetadata(ctx, src, entry, origin, 1)}
	chain, err := m.store.ListLibraryMetadataSources(ctx, series.LibraryID)
	if err != nil {
		return database.SeriesMetadata{}, err
	}
	for rank, link := range chain {
		if link.SourceID == src.ID {
			hit.rank = rank
			break
		}
	}
	_, saved, err := m.applyOfflineHits(ctx, series, []offlineHit{hit}, false, nil)
	return saved, err
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
		ExtCoverURL:      "",
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
	ID             string
	Name           string
	FileID         string
	NumberSort     float64
	MetadataNumber string
}

type remoteBookEntry struct {
	EntryID      string
	FileNames    []string
	Title        string
	Summary      string
	Number       string
	NumberSort   *float64
	ReleaseDate  string
	ISBN         string
	Authors      []database.Author
	Tags         []string
	Links        []database.Link
	CoverURL     string
	DefaultTitle bool
	ThumbnailID  string
	HasCoverBlob bool
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

func (m *Manager) localSeriesBooks(ctx context.Context, seriesID string) ([]localBookCandidate, error) {
	rows, err := m.store.DB().QueryContext(ctx, `
SELECT b.id,b.name,coalesce(b.file_id,''),b.number_sort,coalesce(m.number,'')
FROM books b LEFT JOIN book_metadata m ON m.book_id=b.id
WHERE b.series_id=?
ORDER BY b.number_sort ASC, b.name COLLATE NOCASE ASC, b.id ASC`, seriesID)
	if err != nil {
		return nil, err
	}
	var locals []localBookCandidate
	for rows.Next() {
		var item localBookCandidate
		if scanErr := rows.Scan(&item.ID, &item.Name, &item.FileID, &item.NumberSort, &item.MetadataNumber); scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		locals = append(locals, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return locals, nil
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
	m.importedMu.Lock()
	defer m.importedMu.Unlock()
	saved, refs, err := m.store.ClearSeriesExternalBindingWithCoverRefs(ctx, series)
	if err == nil {
		m.removeUnusedImportedCovers(ctx, refs)
	}
	return saved, err
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
