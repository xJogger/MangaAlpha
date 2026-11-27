package bangumi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xJogger/fake-komga-115/internal/database"
)

var (
	ErrScrapeAlreadyQueued = errors.New("a Bangumi scrape job is already queued or running for this target")
	ErrLibraryHidden       = errors.New("library is hidden")
)

const (
	maxScrapeStoredErrors = 20

	ScrapeModeIncremental    = "incremental"
	ScrapeModeRetryUnmatched = "retry_unmatched"
	ScrapeModeForce          = "force"
)

func NormalizeScrapeMode(raw string, legacyForce bool) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ScrapeModeForce:
		return ScrapeModeForce
	case ScrapeModeRetryUnmatched, "retry", "unmatched":
		return ScrapeModeRetryUnmatched
	case ScrapeModeIncremental:
		return ScrapeModeIncremental
	default:
		if legacyForce {
			return ScrapeModeForce
		}
		return ScrapeModeIncremental
	}
}

type Service struct {
	store   *database.Store
	archive *ArchiveIndex
	live    *LiveClient
	tracker *trackerManager
	logger  *slog.Logger

	ctx       context.Context
	cancel    context.CancelFunc
	jobs      chan scrapeJob
	rankDone  chan struct{}
	schedDone chan struct{}
	nowFunc   func() time.Time

	mu                    sync.Mutex
	archiveWorkers        sync.WaitGroup
	queued                map[string]string
	activeRunID           string
	activeStop            context.CancelFunc
	coverInvalidator      func()
	userCoverInvalidator  func(string)
	metadataArchiveClient func(context.Context) *http.Client
}

type scrapeJob struct {
	runID     string
	libraryID string // empty means all enabled libraries
	mode      string
}

func NewService(store *database.Store, dataDir string, logger *slog.Logger) (*Service, error) {
	archiveIdx, err := NewArchiveIndex(dataDir, logger)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = store.DB().ExecContext(ctx, `
UPDATE bangumi_scrape_runs SET status='failed',completed_at=?,
 errors_json='["service restarted before scrape job completed"]'
WHERE status IN ('queued','running')`, now)

	liveClient := NewLiveClient()
	if savedProxy, pErr := store.GetSetting(ctx, SettingProxyURL); pErr == nil && strings.TrimSpace(savedProxy) != "" {
		if _, normalized, vErr := ValidateProxyURL(savedProxy); vErr == nil {
			_ = archiveIdx.SetProxyURL(normalized)
			_ = liveClient.SetProxyURL(normalized)
		}
	}
	savedSrc, _ := store.GetSetting(ctx, SettingArchiveSource)
	savedCustom, _ := store.GetSetting(ctx, SettingArchiveCustomMetaURL)
	if srcCfg, sErr := ResolveArchiveSource(savedSrc, savedCustom); sErr == nil {
		archiveIdx.SetMetaURL(srcCfg.MetaURL)
	}

	svc := &Service{
		store:     store,
		archive:   archiveIdx,
		live:      liveClient,
		logger:    logger,
		ctx:       ctx,
		cancel:    cancel,
		jobs:      make(chan scrapeJob, 32),
		queued:    make(map[string]string),
		rankDone:  make(chan struct{}),
		schedDone: make(chan struct{}),
		nowFunc:   time.Now,
	}
	syncPending := make(chan syncRequest, 1)
	enqueueSync := func(force bool) {
		select {
		case syncPending <- syncRequest{force: force}:
		default:
			if force {
				select {
				case <-syncPending:
				default:
				}
				select {
				case syncPending <- syncRequest{force: true}:
				default:
				}
			}
		}
	}
	archiveIdx.onImport = func() {
		enqueueSync(true)
	}
	archiveIdx.onUpdateDone = func(result, dumpName, errMsg string) {
		svc.recordArchiveUpdateOutcome(result, dumpName, errMsg)
	}
	go svc.rankWorker(syncPending)
	// Startup check: only runs full-field sync if a newly present archive has not been synced yet.
	enqueueSync(false)
	go svc.archiveScheduleLoop()
	go svc.worker()
	svc.tracker = newTrackerManager(svc)
	svc.live.readToken = svc.tracker.catalogToken
	return svc, nil
}

func (s *Service) now() time.Time {
	if s != nil && s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

func (s *Service) Close() error {
	s.cancel()
	s.mu.Lock()
	if s.activeStop != nil {
		s.activeStop()
	}
	s.mu.Unlock()
	s.archiveWorkers.Wait()
	if s.schedDone != nil {
		<-s.schedDone
	}
	if s.rankDone != nil {
		<-s.rankDone
	}
	if s.tracker != nil {
		s.tracker.Close()
	}
	return s.archive.Close()
}

func (s *Service) TrackerStatus(ctx context.Context, recordLimit int) (TrackerStatus, error) {
	if s == nil || s.tracker == nil {
		return TrackerStatus{NSFWMode: TrackerNSFWModeSkip, Records: []database.BangumiTrackerRecord{}}, nil
	}
	return s.tracker.Status(ctx, recordLimit)
}

func (s *Service) UpdateTrackerConfig(ctx context.Context, upd TrackerConfigUpdate) (TrackerStatus, error) {
	if s == nil || s.tracker == nil {
		return TrackerStatus{}, errors.New("Bangumi tracker is not initialized")
	}
	return s.tracker.UpdateConfig(ctx, upd)
}

func (s *Service) TestTrackerToken(ctx context.Context, candidateToken *string) TrackerTestResult {
	if s == nil || s.tracker == nil {
		return TrackerTestResult{OK: false, Error: "Bangumi tracker is not initialized"}
	}
	return s.tracker.TestToken(ctx, candidateToken)
}

func (s *Service) BuildTrackerOAuthAuthorizeURL(ctx context.Context, redirectURI, state string) (string, error) {
	if s == nil || s.tracker == nil {
		return "", errors.New("Bangumi tracker is not initialized")
	}
	return s.tracker.BuildOAuthAuthorizeURL(ctx, redirectURI, state)
}

func (s *Service) ExchangeTrackerOAuthCode(ctx context.Context, code, redirectURI string) (TrackerStatus, error) {
	if s == nil || s.tracker == nil {
		return TrackerStatus{}, errors.New("Bangumi tracker is not initialized")
	}
	return s.tracker.ExchangeOAuthCode(ctx, code, redirectURI)
}

func (s *Service) RefreshTrackerOAuthNow(ctx context.Context) (TrackerStatus, error) {
	if s == nil || s.tracker == nil {
		return TrackerStatus{}, errors.New("Bangumi tracker is not initialized")
	}
	return s.tracker.RefreshOAuthToken(ctx, true)
}

func (s *Service) NotifyProgress(seriesID string) {
	s.NotifyProgressForUser(database.PrimaryAdminUserID, seriesID)
}

func (s *Service) NotifyProgressForUser(userID, seriesID string) {
	if s == nil || s.tracker == nil {
		return
	}
	s.tracker.NotifyProgressForUser(userID, seriesID)
}

func (s *Service) SyncSeriesTrackerNow(ctx context.Context, seriesID string) (database.BangumiTrackerRecord, error) {
	if s == nil || s.tracker == nil {
		return database.BangumiTrackerRecord{}, errors.New("Bangumi tracker is not initialized")
	}
	return s.tracker.SyncSeries(ctx, seriesID, true)
}

func (s *Service) SeriesTrackerSummary(ctx context.Context, subjectID int64) *SeriesTrackerSummary {
	if s == nil || s.tracker == nil {
		return nil
	}
	return s.tracker.SeriesSummary(ctx, subjectID)
}

// SetAPIBaseURLForTest overrides the Bangumi API base URL in automated tests.
func (s *Service) SetAPIBaseURLForTest(baseURL string) {
	if s != nil && s.live != nil {
		s.live.baseURL = baseURL
	}
}

// SetAPITransportForTest lets tests mock HTTPS requests without changing the API origin.
func (s *Service) SetAPITransportForTest(transport http.RoundTripper) {
	s.live.clientMu.Lock()
	defer s.live.clientMu.Unlock()
	s.live.http = newLiveHTTPClient(transport)
}

// SetOAuthBaseURLForTest overrides the Bangumi OAuth base URL (https://bgm.tv) in automated tests.
func (s *Service) SetOAuthBaseURLForTest(baseURL string) {
	if s != nil && s.tracker != nil {
		s.tracker.SetOAuthBaseURL(baseURL)
	}
}

// SetTrackerDebounceForTest overrides the tracker debounce interval in automated tests.
func (s *Service) SetTrackerDebounceForTest(d time.Duration) {
	if s != nil && s.tracker != nil {
		s.tracker.SetDebounce(d)
	}
}

func (s *Service) ArchiveStatus() ArchiveStatus {
	return s.archive.Status()
}

// ArchiveSummaryByID returns the locally indexed Chinese-enhanced summary for a Bangumi subject.
// When the active archive source is the untranslated official upstream ("official"), it returns ok=false.
func (s *Service) ArchiveSummaryByID(ctx context.Context, subjectID int64) (summary string, source string, ok bool) {
	if s == nil || s.archive == nil || subjectID <= 0 {
		return "", "", false
	}
	srcCfg := s.ArchiveSource(ctx)
	if srcCfg.Source == ArchiveSourceOfficial {
		return "", "", false
	}
	sum, found, err := s.archive.SummaryByID(ctx, subjectID)
	if err != nil || !found {
		return "", "", false
	}
	return sum, srcCfg.Source, true
}

func (s *Service) StartArchiveUpdate() (ArchiveStatus, error) {
	return s.archive.StartOnlineUpdateWithOptions(false)
}

func (s *Service) StartArchiveUpdateWithOptions(force bool) (ArchiveStatus, error) {
	managed := s.metadataManagesArchive(s.ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	var factory func(context.Context) *http.Client
	if managed {
		factory = s.metadataArchiveClient
	}
	if managed && factory == nil {
		return s.ArchiveStatus(), errors.New("metadata archive transport is unavailable")
	}
	ctx, status, err := s.archive.beginOnlineUpdate(s.ctx)
	if err == nil {
		metaURL := s.archive.MetaURL()
		s.archiveWorkers.Add(1)
		go func() {
			defer s.archiveWorkers.Done()
			var client *http.Client
			if factory != nil {
				client = factory(ctx)
				defer client.CloseIdleConnections()
			}
			_ = s.archive.runOnlineUpdate(ctx, force, metaURL, client)
		}()
	}
	return status, err
}

const settingArchiveLegacyUpgradeAttempt = "bangumi_archive_auto_upgrade_attempted_v5"

// StartLegacyArchiveAutoUpgrade gives each legacy dump one automatic upgrade
// attempt across restarts, using the configured archive source and transport.
func (s *Service) StartLegacyArchiveAutoUpgrade(delay time.Duration) {
	needed, signature := s.archive.NeedsLegacyFormatUpgrade()
	if !needed {
		return
	}
	if attempted, _ := s.store.GetSetting(s.ctx, settingArchiveLegacyUpgradeAttempt); attempted == signature {
		if s.logger != nil {
			s.logger.Info("Bangumi legacy archive auto-upgrade already attempted")
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	s.archiveWorkers.Add(1)
	go func() {
		defer s.archiveWorkers.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
		}
		if needed, current := s.archive.NeedsLegacyFormatUpgrade(); !needed || current != signature || s.archive.Status().Updating {
			return
		}
		result, err := s.store.DB().ExecContext(s.ctx, `
INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at
WHERE settings.value<>excluded.value`, settingArchiveLegacyUpgradeAttempt, signature, time.Now().UTC().Format(time.RFC3339Nano))
		if err == nil {
			if n, _ := result.RowsAffected(); n == 0 {
				return
			}
			_, err = s.StartArchiveUpdateWithOptions(false)
		}
		if err != nil && s.ctx.Err() == nil && s.logger != nil {
			s.logger.Warn("start Bangumi legacy archive auto-upgrade", "error", err)
		}
	}()
}

const settingArchiveMetadataManaged = "bangumi_archive_metadata_managed"

// SetMetadataArchiveClient supplies the protected transport for a manifest
// configured through the metadata manager, including later manual updates.
func (s *Service) SetMetadataArchiveClient(factory func(context.Context) *http.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metadataArchiveClient = factory
}

func (s *Service) metadataManagesArchive(ctx context.Context) bool {
	managed, _ := s.store.GetSetting(ctx, settingArchiveMetadataManaged)
	return managed == "true"
}

// UpdateArchiveFromSource uses the caller's vetted transport for both the manifest
// and its download URL. The configured source is changed after a successful update.
func (s *Service) UpdateArchiveFromSource(ctx context.Context, source, customURL string, force bool, client *http.Client) (ArchiveStatus, error) {
	cfg, err := ResolveArchiveSource(source, customURL)
	if err != nil {
		return s.ArchiveStatus(), err
	}
	updateCtx, st, err := s.archive.beginOnlineUpdate(ctx)
	if err != nil {
		return st, err
	}
	force = force || s.archive.MetaURL() != cfg.MetaURL
	if err := s.archive.runOnlineUpdate(updateCtx, force, cfg.MetaURL, client); err != nil {
		return s.ArchiveStatus(), err
	}
	_, err = s.setArchiveSource(ctx, source, customURL, true)
	return s.ArchiveStatus(), err
}

func (s *Service) CancelArchiveUpdate() bool {
	return s.archive.CancelUpdate()
}

func (s *Service) ImportArchiveZip(ctx context.Context, zipPath, dumpName, createdAt string) error {
	return s.archive.ImportFromZipFile(ctx, zipPath, dumpName, createdAt)
}

func (s *Service) FetchCoverBytes(ctx context.Context, coverURL string, subjectID int64) ([]byte, error) {
	return s.live.FetchCoverBytes(ctx, coverURL, subjectID)
}

func (s *Service) SetCoverCacheInvalidator(invalidate func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.coverInvalidator = invalidate
}

func (s *Service) SetUserCoverCacheInvalidator(invalidate func(string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userCoverInvalidator = invalidate
}

func (s *Service) invalidateCoverFailures() {
	s.mu.Lock()
	invalidate := s.coverInvalidator
	s.mu.Unlock()
	if invalidate != nil {
		invalidate()
	}
}

func (s *Service) invalidateUserCoverFailures(userID string) {
	s.mu.Lock()
	userInvalidate := s.userCoverInvalidator
	globalInvalidate := s.coverInvalidator
	s.mu.Unlock()
	if userInvalidate != nil {
		userInvalidate(userID)
		return
	}
	if globalInvalidate != nil {
		globalInvalidate()
	}
}

func (s *Service) RedactedProxyURL(ctx context.Context) string {
	val, err := s.store.GetSetting(ctx, SettingProxyURL)
	if err != nil {
		return ""
	}
	return RedactProxyURL(val)
}

func (s *Service) SetProxyURL(ctx context.Context, inputRaw string) (string, error) {
	existing, _ := s.store.GetSetting(ctx, SettingProxyURL)
	merged, err := MergeProxyURLUpdate(inputRaw, existing)
	if err != nil {
		return "", err
	}
	if err := s.archive.SetProxyURL(merged); err != nil {
		return "", err
	}
	if err := s.live.SetProxyURL(merged); err != nil {
		return "", err
	}
	if err := s.store.SetSetting(ctx, SettingProxyURL, merged); err != nil {
		return "", err
	}
	return RedactProxyURL(merged), nil
}

func (s *Service) ArchiveSource(ctx context.Context) ArchiveSourceConfig {
	savedSrc, _ := s.store.GetSetting(ctx, SettingArchiveSource)
	savedCustom, _ := s.store.GetSetting(ctx, SettingArchiveCustomMetaURL)
	cfg, err := ResolveArchiveSource(savedSrc, savedCustom)
	if err != nil {
		cfg, _ = ResolveArchiveSource(ArchiveSourceZH, "")
	}
	return cfg
}

func (s *Service) SetArchiveSource(ctx context.Context, source, customURL string) (ArchiveSourceConfig, error) {
	return s.setArchiveSource(ctx, source, customURL, false)
}

func (s *Service) setArchiveSource(ctx context.Context, source, customURL string, managed bool) (ArchiveSourceConfig, error) {
	oldSource := s.ArchiveSource(ctx).Source
	cfg, err := ResolveArchiveSource(source, customURL)
	if err != nil {
		return ArchiveSourceConfig{}, err
	}
	if managed {
		if err := s.store.SetSetting(ctx, settingArchiveMetadataManaged, "true"); err != nil {
			return ArchiveSourceConfig{}, err
		}
	}
	if err := s.store.SetSetting(ctx, SettingArchiveSource, cfg.Source); err != nil {
		return ArchiveSourceConfig{}, err
	}
	if cfg.Source == ArchiveSourceCustom || strings.TrimSpace(customURL) != "" {
		if err := s.store.SetSetting(ctx, SettingArchiveCustomMetaURL, cfg.CustomURL); err != nil {
			return ArchiveSourceConfig{}, err
		}
	}
	s.archive.SetMetaURL(cfg.MetaURL)
	if !managed && s.metadataManagesArchive(ctx) {
		if err := s.store.SetSetting(ctx, settingArchiveMetadataManaged, "false"); err != nil {
			return ArchiveSourceConfig{}, err
		}
	}
	s.adaptDefaultArchiveScheduleOnSourceChange(ctx, oldSource, cfg.Source)
	return cfg, nil
}

func (s *Service) TestConnectivity(ctx context.Context, candidateRaw *string) (ConnectivityReport, error) {
	existing, _ := s.store.GetSetting(ctx, SettingProxyURL)
	targetProxy := existing
	if candidateRaw != nil {
		merged, err := MergeProxyURLUpdate(*candidateRaw, existing)
		if err != nil {
			return ConnectivityReport{}, err
		}
		targetProxy = merged
	}
	return ProbeConnectivity(ctx, targetProxy, s.live.baseURL, s.archive.MetaURL())
}

// SearchCandidates searches Bangumi for manual matching in the UI.
// Supports both keyword search and direct Subject ID / bgm.tv URL input.
func (s *Service) SearchCandidates(
	ctx context.Context,
	series database.Series,
	queryText string,
) ([]SubjectCandidate, ParsedQuery, error) {
	ctx, err := s.catalogContext(ctx, series.ID)
	if err != nil {
		return nil, ParsedQuery{}, err
	}
	queryText = strings.TrimSpace(queryText)
	useSeriesName := queryText == "" || queryText == series.Name
	var parsed ParsedQuery
	if useSeriesName {
		queryText = series.Name
		parsed = ParseSeriesWithPath(series.Name, series.RelativePath)
	} else {
		parsed = ParseSeriesName(queryText)
	}

	if id, ok := ParseSubjectIDInput(queryText); ok {
		cand, err := s.live.SubjectByID(ctx, id)
		if err == nil {
			return []SubjectCandidate{cand}, parsed, nil
		}
		if localCand, ok, lErr := s.archive.SubjectByID(ctx, id); lErr == nil && ok && (!localCand.NSFW || showLiveNSFW(ctx)) {
			return []SubjectCandidate{localCand}, parsed, nil
		}
	}

	searchKeyword := queryText
	terms := parsed.lookupTitles()
	useLookupTerms := useSeriesName || len(parsed.compoundTitles) > 0
	if useLookupTerms && len(terms) > 0 {
		searchKeyword = terms[0]
	}

	seen := map[int64]bool{}
	var merged []SubjectCandidate
	oneShot := series.EffectiveOneShot()
	if liveResults, err := s.live.Search(ctx, searchKeyword, parsed, oneShot, 15); err == nil {
		for _, item := range liveResults {
			if !seen[item.ID] {
				seen[item.ID] = true
				merged = append(merged, item)
			}
		}
	}
	if useLookupTerms && len(terms) > 1 && len(merged) < 5 {
		if liveAlt, err := s.live.Search(ctx, terms[1], parsed, oneShot, 10); err == nil {
			for _, item := range liveAlt {
				if !seen[item.ID] {
					seen[item.ID] = true
					merged = append(merged, item)
				}
			}
		}
	}
	if s.archive.Ready() {
		if localResults, err := s.archive.Search(ctx, parsed, oneShot, 15); err == nil {
			for _, item := range localResults {
				if !seen[item.ID] && (!item.NSFW || showLiveNSFW(ctx)) {
					seen[item.ID] = true
					merged = append(merged, item)
				}
			}
		}
	}
	return merged, parsed, nil
}

// catalogContext checks the target series before Bangumi reads. Administrator
// management and background scraping can see NSFW regardless of shelf preferences.
func (s *Service) catalogContext(ctx context.Context, seriesID string) (context.Context, error) {
	scope, ok := database.UserScopeFromContext(ctx)
	if ok && s.store != nil {
		var err error
		if scope.IsAdmin {
			_, err = s.store.SeriesByIDAny(ctx, seriesID, false)
		} else {
			_, err = s.store.SeriesByID(ctx, seriesID)
		}
		if err != nil {
			return ctx, err
		}
	}
	if !ok {
		scope = database.UserScope{UserID: database.PrimaryAdminUserID, IsAdmin: true}
	}
	if scope.IsAdmin {
		scope.AllowNSFW, scope.ShowNSFW = true, true
		ctx = database.WithUserScope(ctx, scope)
	}
	return ctx, nil
}

// BindSubject binds a specific Bangumi Subject ID to a Series (preferring live API details, falling back to local archive).
func (s *Service) BindSubject(
	ctx context.Context,
	series database.Series,
	subjectID int64,
) (database.SeriesMetadata, error) {
	return s.BindSubjectWithOrigin(ctx, series, subjectID, "manual")
}

func (s *Service) BindSubjectWithOrigin(ctx context.Context, series database.Series, subjectID int64, origin string) (database.SeriesMetadata, error) {
	ctx, err := s.catalogContext(ctx, series.ID)
	if err != nil {
		return database.SeriesMetadata{}, err
	}
	cand, err := s.live.SubjectByID(ctx, subjectID)
	if err != nil {
		localCand, ok, localErr := s.archive.SubjectByID(ctx, subjectID)
		if localErr != nil || !ok {
			return database.SeriesMetadata{}, fmt.Errorf("fetch Bangumi subject %d: %w", subjectID, err)
		}
		cand = localCand
	}
	if cand.NSFW && !showLiveNSFW(ctx) {
		return database.SeriesMetadata{}, ErrSubjectNotFound
	}
	cand.Confidence = 1.0
	parsed := ParseSeriesWithPath(series.Name, series.RelativePath)
	scraped := ToScrapedMetadataWithQuery(series.Name, parsed, cand)
	scraped.MatchOrigin = origin
	meta, err := s.store.ApplyScrapedMetadata(ctx, series, scraped)
	if err == nil {
		s.invalidateCoverFailures()
	}
	return meta, err
}

// OfflineMatch uses the same archive and confidence threshold as the Bangumi
// scraper, and leaves the final write to the caller.
func (s *Service) OfflineMatch(ctx context.Context, series database.Series) (database.ScrapedMetadata, bool, error) {
	if !s.archive.Ready() {
		return database.ScrapedMetadata{}, false, nil
	}
	parsed := ParseSeriesWithPath(series.Name, series.RelativePath)
	candidates, err := s.archive.Search(ctx, parsed, series.EffectiveOneShot(), 10)
	if err != nil || len(candidates) == 0 || candidates[0].Confidence < AutoMatchMinConfidence {
		return database.ScrapedMetadata{}, false, err
	}
	return ToScrapedMetadataWithQuery(series.Name, parsed, candidates[0]), true, nil
}

func (s *Service) OfflineSubject(ctx context.Context, series database.Series, subjectID int64) (database.ScrapedMetadata, bool, error) {
	if !s.archive.Ready() {
		return database.ScrapedMetadata{}, false, nil
	}
	candidate, found, err := s.archive.SubjectByID(ctx, subjectID)
	if err != nil || !found {
		return database.ScrapedMetadata{}, false, err
	}
	candidate.Confidence = 1
	parsed := ParseSeriesWithPath(series.Name, series.RelativePath)
	return ToScrapedMetadataWithQuery(series.Name, parsed, candidate), true, nil
}

// OfflineTankobons returns linked tankobon single-volume subjects from the local
// bangumi-archive.db with zero network requests.
func (s *Service) OfflineTankobons(ctx context.Context, parentSubjectID int64) ([]TankobonSubject, bool, error) {
	if s == nil || s.archive == nil || parentSubjectID <= 0 {
		return nil, false, nil
	}
	items, found, err := s.archive.Tankobons(ctx, parentSubjectID)
	if err != nil || !found {
		return items, found, err
	}
	if scope, ok := database.UserScopeFromContext(ctx); ok && !scope.EffectiveShowNSFW() {
		items = slices.DeleteFunc(items, func(item TankobonSubject) bool { return item.NSFW })
		if len(items) == 0 {
			return nil, false, nil
		}
	}
	return items, true, nil
}

// TankobonsForSubject returns linked tankobon single-volume subjects for parentSubjectID,
// querying the local offline archive first and optionally falling back to Bangumi Live API.
func (s *Service) TankobonsForSubject(ctx context.Context, parentSubjectID int64, allowLiveFallback bool) ([]TankobonSubject, error) {
	if s == nil || parentSubjectID <= 0 {
		return nil, nil
	}
	items, found, err := s.OfflineTankobons(ctx, parentSubjectID)
	if err != nil {
		return nil, err
	}
	if found && len(items) > 0 {
		return items, nil
	}
	if !allowLiveFallback || s.live == nil {
		return nil, nil
	}
	return s.live.SubjectRelations(ctx, parentSubjectID)
}

func (s *Service) VerifyTankobonRatings(ctx context.Context, matches []VolumeMatchResult) {
	if s.live != nil {
		s.live.VerifyTankobonRatings(ctx, matches)
	}
}

// AutoScrapeOne runs automatic title parsing & matching on a single Series.
func (s *Service) AutoScrapeOne(
	ctx context.Context,
	series database.Series,
) (matched bool, meta database.SeriesMetadata, err error) {
	matched, meta, _, err = s.AutoScrapeOneDetailed(ctx, series)
	return matched, meta, err
}

// AutoScrapeOneDetailed is AutoScrapeOne that also returns the top evaluated
// candidates (with score breakdowns) so a failed auto-match can explain itself.
func (s *Service) AutoScrapeOneDetailed(
	ctx context.Context,
	series database.Series,
) (matched bool, meta database.SeriesMetadata, candidates []SubjectCandidate, err error) {
	ctx, err = s.catalogContext(ctx, series.ID)
	if err != nil {
		return false, database.SeriesMetadata{}, nil, err
	}
	parsed := ParseSeriesWithPath(series.Name, series.RelativePath)
	terms := parsed.lookupTitles()
	oneShot := series.EffectiveOneShot()
	if s.archive.Ready() {
		candidates, err = s.archive.Search(ctx, parsed, oneShot, 10)
		if err != nil {
			return false, database.SeriesMetadata{}, nil, err
		}
		if !showLiveNSFW(ctx) {
			candidates = slices.DeleteFunc(candidates, func(candidate SubjectCandidate) bool { return candidate.NSFW })
		}
	} else if len(terms) > 0 {
		maxTries := min(2, len(terms))
		for i := 0; i < maxTries; i++ {
			res, sErr := s.live.Search(ctx, terms[i], parsed, oneShot, 10)
			if sErr != nil {
				if i == 0 {
					return false, database.SeriesMetadata{}, nil, sErr
				}
				break
			}
			if len(res) > 0 && (len(candidates) == 0 || res[0].Confidence > candidates[0].Confidence) {
				candidates = res
			}
			if len(candidates) > 0 && candidates[0].Confidence >= AutoMatchMinConfidence {
				break
			}
		}
	}
	if len(candidates) == 0 || candidates[0].Confidence < AutoMatchMinConfidence {
		bestConf := 0.0
		if len(candidates) > 0 {
			bestConf = candidates[0].Confidence
		}
		inferredNSFW := InferPathNSFW(series.Name, series.RelativePath)
		if mErr := s.store.MarkSeriesScrapeStatusWithNSFW(ctx, series, database.ScrapeStatusUnmatched, bestConf, inferredNSFW); mErr != nil {
			return false, database.SeriesMetadata{}, nil, mErr
		}
		cur, _ := s.store.SeriesMetadataByID(ctx, series)
		return false, cur, topCandidates(candidates, 3), nil
	}
	best := candidates[0]
	// When matching via online search (which lacks full infobox persons), enrich via SubjectByID if needed.
	if !s.archive.Ready() && len(best.Authors) == 0 {
		if full, fErr := s.live.SubjectByID(ctx, best.ID); fErr == nil {
			full.Confidence = best.Confidence
			best = full
		}
	}
	scraped := ToScrapedMetadataWithQuery(series.Name, parsed, best)
	scraped.MatchOrigin = "auto"
	meta, err = s.store.ApplyScrapedMetadata(ctx, series, scraped)
	if err != nil {
		return false, database.SeriesMetadata{}, nil, err
	}
	return true, meta, topCandidates(candidates, 3), nil
}

func topCandidates(candidates []SubjectCandidate, limit int) []SubjectCandidate {
	if len(candidates) > limit {
		return candidates[:limit]
	}
	return candidates
}

func (s *Service) StartBatch(
	ctx context.Context,
	libraryID string,
	force bool,
) (database.BangumiScrapeRun, error) {
	return s.StartBatchWithMode(ctx, libraryID, NormalizeScrapeMode("", force))
}

func (s *Service) StartBatchWithMode(
	ctx context.Context,
	libraryID string,
	rawMode string,
) (database.BangumiScrapeRun, error) {
	baseMode := NormalizeScrapeMode(rawMode, false)
	force := baseMode == ScrapeModeForce
	libraryID = strings.TrimSpace(libraryID)
	if libraryID != "" {
		lib, err := s.store.LibraryBasic(ctx, libraryID)
		if err != nil {
			return database.BangumiScrapeRun{}, err
		}
		if !lib.Enabled || lib.UnionParentID != "" {
			return database.BangumiScrapeRun{}, ErrLibraryHidden
		}
	}
	queueKey := libraryID
	if queueKey == "" {
		queueKey = "__all__"
	}
	s.mu.Lock()
	if _, exists := s.queued[queueKey]; exists {
		s.mu.Unlock()
		return database.BangumiScrapeRun{}, ErrScrapeAlreadyQueued
	}
	runID := randomScrapeID()
	s.queued[queueKey] = runID
	s.mu.Unlock()

	modeLabel := baseMode
	if s.archive.Ready() {
		modeLabel += ":offline"
	} else {
		modeLabel += ":online"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var libParam any
	if libraryID != "" {
		libParam = libraryID
	}
	forceInt := 0
	if force {
		forceInt = 1
	}
	if _, err := s.store.DB().ExecContext(ctx, `
INSERT INTO bangumi_scrape_runs(id,library_id,mode,force,status,created_at)
VALUES(?,?,?,?,'queued',?)`, runID, libParam, modeLabel, forceInt, now); err != nil {
		s.releaseQueued(queueKey)
		return database.BangumiScrapeRun{}, err
	}
	run, err := s.Run(ctx, runID)
	if err != nil {
		s.releaseQueued(queueKey)
		return database.BangumiScrapeRun{}, err
	}
	select {
	case s.jobs <- scrapeJob{runID: runID, libraryID: libraryID, mode: baseMode}:
		return run, nil
	case <-s.ctx.Done():
		s.releaseQueued(queueKey)
		return database.BangumiScrapeRun{}, context.Canceled
	}
}

func (s *Service) CancelBatch(ctx context.Context, runID string) error {
	res, err := s.store.DB().ExecContext(ctx, `
UPDATE bangumi_scrape_runs SET cancel_requested=1
WHERE id=? AND id NOT LIKE 'ext_%' AND status IN ('queued','running')`, strings.TrimSpace(runID))
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	s.mu.Lock()
	if s.activeRunID == runID && s.activeStop != nil {
		s.activeStop()
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) CancelLibrary(ctx context.Context, libraryID string) {
	rows, err := s.store.DB().QueryContext(ctx,
		`SELECT id FROM bangumi_scrape_runs WHERE library_id=? AND id NOT LIKE 'ext_%' AND status IN ('queued','running')`, libraryID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		_ = s.CancelBatch(ctx, id)
	}
}

func (s *Service) Runs(ctx context.Context, limit int) ([]database.BangumiScrapeRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	rows, err := s.store.DB().QueryContext(ctx, `
SELECT r.id,r.library_id,coalesce(l.name,'全部未隐藏书库'),r.mode,r.force,r.status,
 r.total_series,r.processed_series,r.matched_count,r.unmatched_count,r.skipped_count,r.failed_count,
 r.current_series,r.errors_json,r.cancel_requested,r.started_at,r.completed_at,r.created_at
FROM bangumi_scrape_runs r
LEFT JOIN libraries l ON l.id=r.library_id
WHERE r.id NOT LIKE 'ext_%'
ORDER BY r.created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]database.BangumiScrapeRun, 0)
	for rows.Next() {
		run, err := scanScrapeRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *Service) Run(ctx context.Context, runID string) (database.BangumiScrapeRun, error) {
	row := s.store.DB().QueryRowContext(ctx, `
SELECT r.id,r.library_id,coalesce(l.name,'全部未隐藏书库'),r.mode,r.force,r.status,
 r.total_series,r.processed_series,r.matched_count,r.unmatched_count,r.skipped_count,r.failed_count,
 r.current_series,r.errors_json,r.cancel_requested,r.started_at,r.completed_at,r.created_at
FROM bangumi_scrape_runs r
LEFT JOIN libraries l ON l.id=r.library_id
WHERE r.id=?`, runID)
	return scanScrapeRun(row)
}

func scanScrapeRun(scanner interface{ Scan(...any) error }) (database.BangumiScrapeRun, error) {
	var (
		run        database.BangumiScrapeRun
		forceInt   int
		cancelInt  int
		errorsJSON string
	)
	if err := scanner.Scan(
		&run.ID, &run.LibraryID, &run.LibraryName, &run.Mode, &forceInt, &run.Status,
		&run.TotalSeries, &run.ProcessedSeries, &run.MatchedCount, &run.UnmatchedCount,
		&run.SkippedCount, &run.FailedCount, &run.CurrentSeries, &errorsJSON,
		&cancelInt, &run.StartedAt, &run.CompletedAt, &run.CreatedAt,
	); err != nil {
		return database.BangumiScrapeRun{}, err
	}
	run.Force = forceInt != 0
	run.CancelRequested = cancelInt != 0
	_ = json.Unmarshal([]byte(errorsJSON), &run.Errors)
	if run.Errors == nil {
		run.Errors = []string{}
	}
	return run, nil
}

func (s *Service) worker() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case item := <-s.jobs:
			s.executeBatch(item)
		}
	}
}

func (s *Service) executeBatch(item scrapeJob) {
	queueKey := item.libraryID
	if queueKey == "" {
		queueKey = "__all__"
	}
	ctx, cancel := context.WithCancel(s.ctx)
	ctx = database.WithAutomaticMetadata(ctx)
	s.mu.Lock()
	s.activeRunID = item.runID
	s.activeStop = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.queued, queueKey)
		s.activeRunID = ""
		s.activeStop = nil
		s.mu.Unlock()
	}()

	run, err := s.Run(ctx, item.runID)
	if err != nil {
		return
	}
	if run.CancelRequested || (item.libraryID != "" && !s.store.IsLibraryVisible(ctx, item.libraryID)) {
		s.finishRun(item.runID, run, "canceled")
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = s.store.DB().ExecContext(ctx,
		`UPDATE bangumi_scrape_runs SET status='running',started_at=? WHERE id=?`, now, item.runID)

	targets, err := s.loadTargetSeries(ctx, item.libraryID)
	if err != nil {
		run.Errors = append(run.Errors, err.Error())
		s.finishRun(item.runID, run, "failed")
		return
	}
	run.TotalSeries = len(targets)
	_, _ = s.store.DB().ExecContext(ctx,
		`UPDATE bangumi_scrape_runs SET total_series=? WHERE id=?`, run.TotalSeries, item.runID)

	libEnabledCache := map[string]bool{}
	lastFlush := time.Now()
	for i, target := range targets {
		if ctx.Err() != nil {
			s.finishRun(item.runID, run, "canceled")
			return
		}
		libOK, seenLib := libEnabledCache[target.series.LibraryID]
		if !seenLib {
			libOK = s.store.IsLibraryVisible(ctx, target.series.LibraryID)
			libEnabledCache[target.series.LibraryID] = libOK
		}
		if !libOK {
			if item.libraryID != "" {
				s.finishRun(item.runID, run, "canceled")
				return
			}
			run.ProcessedSeries++
			run.SkippedCount++
		} else if target.matchOrigin == "manual" || target.matchOrigin == "agent" ||
			(target.matchOrigin == "" && target.bound) || shouldSkipByMode(item.mode, target.scrapeStatus) {
			run.ProcessedSeries++
			run.SkippedCount++
		} else {
			run.CurrentSeries = target.series.Name
			matched, _, sErr := s.AutoScrapeOne(ctx, target.series)
			if errors.Is(sErr, context.Canceled) || ctx.Err() != nil {
				s.finishRun(item.runID, run, "canceled")
				return
			}
			run.ProcessedSeries++
			switch {
			case errors.Is(sErr, database.ErrMetadataChanged):
				run.SkippedCount++
			case sErr != nil:
				run.FailedCount++
				if len(run.Errors) < maxScrapeStoredErrors {
					run.Errors = append(run.Errors, target.series.Name+": "+sErr.Error())
				}
			case matched:
				run.MatchedCount++
			default:
				run.UnmatchedCount++
			}
		}

		// Flush progress every 25 items or 400ms so 60k-series offline runs don't bottleneck on progress writes!
		if i == len(targets)-1 || run.ProcessedSeries%25 == 0 || time.Since(lastFlush) >= 400*time.Millisecond {
			lastFlush = time.Now()
			clear(libEnabledCache)
			if err := s.flushRunProgress(ctx, item.runID, run); err != nil {
				s.finishRun(item.runID, run, "canceled")
				return
			}
		}
	}

	status := "success"
	if run.FailedCount > 0 {
		status = "partial"
	}
	s.finishRun(item.runID, run, status)
	if s.logger != nil {
		s.logger.Info("Bangumi scrape job completed",
			"run", item.runID, "total", run.TotalSeries,
			"matched", run.MatchedCount, "unmatched", run.UnmatchedCount,
			"skipped", run.SkippedCount, "failed", run.FailedCount)
	}
}

type scrapeTarget struct {
	series       database.Series
	scrapeStatus string
	matchOrigin  string
	bound        bool
}

func shouldSkipByMode(mode string, scrapeStatus string) bool {
	switch mode {
	case ScrapeModeForce:
		return false
	case ScrapeModeRetryUnmatched:
		return scrapeStatus == database.ScrapeStatusMatched
	default:
		return scrapeStatus == database.ScrapeStatusMatched || scrapeStatus == database.ScrapeStatusUnmatched
	}
}

func (s *Service) loadTargetSeries(ctx context.Context, libraryID string) ([]scrapeTarget, error) {
	query := `
SELECT s.id,s.library_id,s.cid,s.name,s.relative_path,s.one_shot,s.one_shot_override,s.books_count,
       coalesce(sm.scrape_status,'none'),coalesce(sm.match_origin,''),
       (coalesce(sm.bgm_subject_id,0)>0 OR coalesce(sm.ext_entry_id,'')<>'')
FROM series s
JOIN libraries l ON l.id=s.library_id
LEFT JOIN series_metadata sm ON sm.series_id=s.id`
	var args []any
	if libraryID != "" {
		query += ` WHERE s.library_id=? AND l.enabled=1 AND NOT EXISTS (SELECT 1 FROM union_layers ul WHERE ul.member_library_id=l.id)`
		args = append(args, libraryID)
	} else {
		query += ` WHERE l.enabled=1 AND NOT EXISTS (SELECT 1 FROM union_layers ul WHERE ul.member_library_id=l.id)`
	}
	query += ` ORDER BY s.updated_at DESC,s.id ASC`
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scrapeTarget
	for rows.Next() {
		var t scrapeTarget
		if err := rows.Scan(
			&t.series.ID, &t.series.LibraryID, &t.series.CID, &t.series.Name,
			&t.series.RelativePath, &t.series.OneShot, &t.series.OneShotOverride, &t.series.BooksCount,
			&t.scrapeStatus, &t.matchOrigin, &t.bound,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Service) flushRunProgress(ctx context.Context, runID string, run database.BangumiScrapeRun) error {
	errorsJSON, _ := json.Marshal(run.Errors)
	res, err := s.store.DB().ExecContext(ctx, `
UPDATE bangumi_scrape_runs SET total_series=?,processed_series=?,matched_count=?,
 unmatched_count=?,skipped_count=?,failed_count=?,current_series=?,errors_json=?
WHERE id=? AND cancel_requested=0`,
		run.TotalSeries, run.ProcessedSeries, run.MatchedCount, run.UnmatchedCount,
		run.SkippedCount, run.FailedCount, run.CurrentSeries, string(errorsJSON), runID)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return context.Canceled
	}
	return nil
}

func (s *Service) finishRun(runID string, run database.BangumiScrapeRun, status string) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	errorsJSON, _ := json.Marshal(run.Errors)
	_, _ = s.store.DB().ExecContext(context.Background(), `
UPDATE bangumi_scrape_runs SET status=?,completed_at=?,total_series=?,processed_series=?,
 matched_count=?,unmatched_count=?,skipped_count=?,failed_count=?,current_series=?,errors_json=?
WHERE id=?`,
		status, now, run.TotalSeries, run.ProcessedSeries, run.MatchedCount,
		run.UnmatchedCount, run.SkippedCount, run.FailedCount, run.CurrentSeries, string(errorsJSON), runID)
}

func (s *Service) releaseQueued(key string) {
	s.mu.Lock()
	delete(s.queued, key)
	s.mu.Unlock()
}

func randomScrapeID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
