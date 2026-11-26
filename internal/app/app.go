package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xJogger/fake-komga-115/internal/archive"
	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/cache"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/diagnostics"
	"github.com/xJogger/fake-komga-115/internal/extmeta"
	"github.com/xJogger/fake-komga-115/internal/httpserver"
	"github.com/xJogger/fake-komga-115/internal/maintenance"
	"github.com/xJogger/fake-komga-115/internal/oneonefive"
	"github.com/xJogger/fake-komga-115/internal/pipeline"
	"github.com/xJogger/fake-komga-115/internal/scanner"
	"github.com/xJogger/fake-komga-115/internal/thumbnail"
	"github.com/xJogger/fake-komga-115/internal/updater"
	"github.com/xJogger/fake-komga-115/internal/webdav"
	"golang.org/x/time/rate"
)

const SettingDiagnosticsVerbose = "diagnostics_verbose"

type Options struct {
	Host                 string
	Port                 int
	DataDir              string
	Logger               *slog.Logger
	LoopbackToken        string
	TrustedProxies       string
	InitialAdminUsername string
	InitialAdminPassword string
}

type App struct {
	server     *http.Server
	rawHandler http.Handler
	store      *database.Store
	scanner    *scanner.Manager
	pipelines  *pipeline.Manager
	covers     *thumbnail.BatchManager
	tasks      *maintenance.Manager
	archive    *archive.Service
	bangumi    *bangumi.Service
	metadata   *extmeta.Manager
	updater    *updater.Service
	diag       *diagnostics.Recorder
	logger     *slog.Logger
	listenAddr atomic.Value
	warmCancel context.CancelFunc
	warmDone   chan struct{}

	lanMu     sync.Mutex
	lanServer *http.Server
	lanPort   int
	lanAddr   string
	lanErr    string
}

func New(_ context.Context, options Options) (*App, error) {
	if options.LoopbackToken != "" && options.Host != "127.0.0.1" {
		return nil, errors.New("loopback guard requires IPv4 loopback")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	dataDir, err := filepath.Abs(options.DataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	dataDir = filepath.Clean(dataDir)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	diagRecorder, err := diagnostics.New(dataDir, false)
	if err != nil {
		return nil, fmt.Errorf("init diagnostics: %w", err)
	}
	diagnostics.SetDefault(diagRecorder)

	store, err := database.Open(filepath.Join(dataDir, "fake-komga-115.db"))
	if err != nil {
		diagRecorder.Record(diagnostics.Event{
			Level:     diagnostics.LevelError,
			Component: "app",
			Stage:     "startup",
			Action:    "open_db",
			Code:      diagnostics.ClassifyError(err),
			Detail:    err.Error(),
		})
		return nil, err
	}
	if rawVerbose, sErr := store.GetSetting(context.Background(), SettingDiagnosticsVerbose); sErr == nil {
		if parsed, pErr := strconv.ParseBool(strings.TrimSpace(rawVerbose)); pErr == nil {
			diagRecorder.SetVerbose(parsed)
		}
	}
	if err := seedInitialAdminFromOptions(context.Background(), store, options); err != nil {
		store.Close()
		return nil, err
	}
	cacheManager, err := cache.New(store, filepath.Join(dataDir, "cache"))
	if err != nil {
		store.Close()
		return nil, err
	}
	client := oneonefive.New(store, options.Logger)
	webdavManager := webdav.NewManager()
	scanManager := scanner.NewWithWebDAV(store, client, webdavManager, options.Logger)
	archiveService := archive.NewServiceWithWebDAV(store, client, webdavManager, cacheManager, options.Logger)
	archiveService.SetComicInfoParser(parseBookComicInfo)

	bangumiService, err := bangumi.NewService(store, filepath.Join(dataDir, "bangumi"), options.Logger)
	if err != nil {
		archiveService.Close()
		scanManager.Close()
		store.Close()
		return nil, err
	}

	thumbnailService, err := thumbnail.NewWithDataDir(
		store, filepath.Join(dataDir, "thumbnails", "series"), dataDir, options.Logger,
	)
	if err != nil {
		_ = bangumiService.Close()
		archiveService.Close()
		scanManager.Close()
		store.Close()
		return nil, err
	}
	thumbnailService.SetBangumiCoverFetcher(bangumiService.FetchCoverBytes)
	thumbnailService.SetTankobonMatcher(func(ctx context.Context, series database.Series, meta database.SeriesMetadata, books []thumbnail.VolumeMatchBook, allowLive bool) ([]thumbnail.VolumeMatchResult, error) {
		subjectID := *meta.BgmSubjectID
		var items []bangumi.TankobonSubject
		var err error
		// Preserve offline relation ordering even when the caller hides adult entries.
		offlineCtx := database.WithUserScope(ctx, database.UserScope{AllowNSFW: true, ShowNSFW: true})
		items, _, err = bangumiService.OfflineTankobons(offlineCtx, subjectID)
		if err == nil && len(items) == 0 && allowLive {
			items, err = bangumiService.TankobonsForSubject(ctx, subjectID, true)
		}
		if err != nil {
			return nil, err
		}
		if len(items) == 0 && series.BooksCount > 1 {
			return nil, nil
		}
		volumes := make([]bangumi.VolumeMatchBook, len(books))
		for i, book := range books {
			volumes[i] = bangumi.VolumeMatchBook{BookID: book.BookID, Name: book.Name, Number: book.Number}
		}
		matches := bangumi.MatchBooksToTankobons(subjectID, series.EffectiveOneShot(), volumes, items)
		out := make([]thumbnail.VolumeMatchResult, 0, len(matches))
		for _, match := range matches {
			if match.MatchedBy == "oneshot_fallback" {
				official, found, err := bangumiService.OfflineSubject(offlineCtx, series, subjectID)
				match.NSFWKnown = err == nil && found
				match.NSFW = meta.IsNSFW || (match.NSFWKnown && official.IsNSFW)
			}
			out = append(out, thumbnail.VolumeMatchResult{
				BookID: match.BookID, SubjectID: match.SubjectID, CoverURL: match.CoverURL, NSFW: match.NSFW, NSFWKnown: match.NSFWKnown,
			})
		}
		return out, nil
	})

	proxyURLProvider := func(ctx context.Context) string {
		proxy, _ := store.GetSetting(ctx, bangumi.SettingProxyURL)
		return proxy
	}
	externalCovers, err := extmeta.NewCoverService(extmeta.CoverConfig{DataDir: dataDir, ProxyURLProvider: proxyURLProvider})
	var metadataManager *extmeta.Manager
	if err == nil {
		metadataManager, err = extmeta.NewManager(store, bangumiService, externalCovers, dataDir, options.Logger, extmeta.ManagerOptions{
			ProxyURLProvider: proxyURLProvider,
		})
	}
	if err != nil {
		_ = bangumiService.Close()
		archiveService.Close()
		scanManager.Close()
		store.Close()
		return nil, err
	}
	thumbnailService.SetExternalCoverFetcher(metadataManager.FetchCover)
	publicCovers := rate.NewLimiter(12, 8)
	thumbnailService.SetPublicBangumiCoverFetcher(func(ctx context.Context, coverURL string, subjectID int64) ([]byte, error) {
		if err := publicCovers.Wait(ctx); err != nil {
			return nil, err
		}
		return fetchPublicBangumiCover(ctx, proxyURLProvider(ctx), coverURL, subjectID)
	})
	thumbnailService.SetBookExternalCoverFetcher(func(ctx context.Context, sourceID, entryID, parentEntryID, coverURL string) ([]byte, string, error) {
		if sourceID == "" && !strings.HasPrefix(coverURL, extmeta.SQLiteBlobCoverPrefix) {
			cover, err := externalCovers.GetOrFetch(ctx, "book-custom", entryID, coverURL)
			return cover.Bytes, cover.MediaType, err
		}
		return metadataManager.FetchBookCover(ctx, sourceID, entryID, parentEntryID, coverURL)
	})
	thumbnailService.SetNSFWFallbackCoverFetcher(metadataManager.FetchFallbackNSFWCover, metadataManager.InvalidateFallbackCoverCache)
	metadataManager.StartScheduler()

	coverManager := thumbnail.NewBatchManager(
		store, archiveService, thumbnailService, options.Logger,
	)
	maintenanceManager := maintenance.New(
		store, archiveService, thumbnailService, options.Logger,
	)
	pipelineManager := pipeline.New(
		store, scanManager, bangumiService, coverManager, options.Logger,
	)
	pipelineManager.SetMetadataManager(metadataManager)
	// When a physical sub-library finishes a manual/standalone scan, silently refresh its parent union library.
	scanManager.AddCompletionListener(func(libraryID, _ string, trigger, status string) {
		if status != "success" || trigger == "pipeline" {
			return
		}
		parentID, isSub, err := store.UnionParentLibraryID(context.Background(), libraryID)
		if err != nil || !isSub || parentID == "" {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if stats, rErr := store.RefreshUnionLibrary(ctx, parentID); rErr != nil {
				options.Logger.Error("auto-refresh union library after sub-library scan",
					"union", parentID, "member", libraryID, "error", rErr)
			} else {
				options.Logger.Info("auto-refreshed union library after sub-library scan",
					"union", parentID, "member", libraryID, "series", stats.SeriesCount, "books", stats.BooksCount)
			}
		}()
	})
	updaterService := updater.NewService(store, options.Logger)
	handler := httpserver.New(
		store, client, scanManager, cacheManager, archiveService, thumbnailService,
		coverManager, maintenanceManager, httpserver.RuntimeInfo{
			DataDir:        dataDir,
			Host:           options.Host,
			Port:           options.Port,
			TrustedProxies: options.TrustedProxies,
		}, options.Logger,
	).WithServices(webdavManager, bangumiService).WithPipeline(pipelineManager).WithDiagnostics(diagRecorder).WithUpdater(updaterService).WithMetadata(metadataManager)

	rawHandler := handler.Handler()
	var routes http.Handler = rawHandler
	if options.LoopbackToken != "" {
		routes = loopbackGuard(options.LoopbackToken, routes)
	}
	server := &http.Server{
		Addr:              net.JoinHostPort(options.Host, strconv.Itoa(options.Port)),
		Handler:           routes,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return &App{
		server: server, rawHandler: rawHandler, store: store, scanner: scanManager, pipelines: pipelineManager,
		covers: coverManager, tasks: maintenanceManager, archive: archiveService, bangumi: bangumiService,
		updater: updaterService, metadata: metadataManager, diag: diagRecorder, logger: options.Logger,
	}, nil
}

func (a *App) Diagnostics() *diagnostics.Recorder { return a.diag }
func (a *App) Updater() *updater.Service          { return a.updater }

func (a *App) HasAdmin(ctx context.Context) (bool, error) {
	return a.store.HasAdmin(ctx)
}

func (a *App) SetDiagnosticsVerbose(ctx context.Context, verbose bool) error {
	if err := a.store.SetSetting(ctx, SettingDiagnosticsVerbose, strconv.FormatBool(verbose)); err != nil {
		return err
	}
	a.diag.SetVerbose(verbose)
	return nil
}

// StartLANServer starts an auxiliary 0.0.0.0:<port> HTTP listener sharing the
// standard authenticated MangaAlpha handler (without the Android WebView
// loopback cookie guard) so LAN devices and Komga clients can connect.
// Requires an initialized administrator password before binding.
func (a *App) StartLANServer(port int) (string, error) {
	if port < 1024 || port > 65535 {
		err := errors.New("LAN port must be between 1024 and 65535")
		a.setLANError(err.Error())
		return "", err
	}
	claimed, err := a.store.HasAdmin(context.Background())
	if err != nil {
		a.setLANError(err.Error())
		return "", err
	}
	if !claimed {
		err := errors.New("administrator password must be configured before enabling LAN server")
		a.setLANError(err.Error())
		a.diag.Record(diagnostics.Event{
			Level:     diagnostics.LevelWarn,
			Component: "app",
			Stage:     "lan_server",
			Action:    "start_blocked",
			Code:      "ADMIN_REQUIRED",
			Detail:    err.Error(),
			Meta:      map[string]any{"port": port, "lanEnabled": false},
		})
		return "", err
	}

	a.lanMu.Lock()
	if a.lanServer != nil && a.lanPort == port {
		addr := a.lanAddr
		a.lanMu.Unlock()
		return addr, nil
	}
	prev := a.lanServer
	a.lanServer = nil
	a.lanPort = 0
	a.lanAddr = ""
	a.lanErr = ""
	a.lanMu.Unlock()

	if prev != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = prev.Shutdown(ctx)
		cancel()
	}

	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		a.setLANError(err.Error())
		a.diag.Record(diagnostics.Event{
			Level:     diagnostics.LevelError,
			Component: "app",
			Stage:     "lan_server",
			Action:    "bind_failed",
			Code:      diagnostics.ClassifyError(err),
			Detail:    err.Error(),
			Meta:      map[string]any{"port": port, "lanEnabled": false},
		})
		return "", err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           a.rawHandler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	bound := listener.Addr().String()
	a.lanMu.Lock()
	a.lanServer = srv
	a.lanPort = port
	a.lanAddr = bound
	a.lanErr = ""
	a.lanMu.Unlock()

	a.logger.Info("LAN server listening", "address", bound)
	a.diag.Record(diagnostics.Event{
		Level:     diagnostics.LevelInfo,
		Component: "app",
		Stage:     "lan_server",
		Action:    "started",
		Detail:    "auxiliary LAN listener started",
		Meta:      map[string]any{"port": port, "lanEnabled": true},
	})
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logger.Warn("LAN server stopped with error", "error", err)
			a.lanMu.Lock()
			if a.lanServer == srv {
				a.lanServer = nil
				a.lanPort = 0
				a.lanAddr = ""
				a.lanErr = err.Error()
			}
			a.lanMu.Unlock()
			a.diag.Record(diagnostics.Event{
				Level:     diagnostics.LevelError,
				Component: "app",
				Stage:     "lan_server",
				Action:    "serve_error",
				Code:      diagnostics.ClassifyError(err),
				Detail:    err.Error(),
				Meta:      map[string]any{"port": port, "lanEnabled": false},
			})
		}
	}()
	return bound, nil
}

func (a *App) StopLANServer() error {
	a.lanMu.Lock()
	srv := a.lanServer
	port := a.lanPort
	a.lanServer = nil
	a.lanPort = 0
	a.lanAddr = ""
	a.lanErr = ""
	a.lanMu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := srv.Shutdown(ctx)
	a.logger.Info("LAN server stopped", "port", port)
	a.diag.Record(diagnostics.Event{
		Level:     diagnostics.LevelInfo,
		Component: "app",
		Stage:     "lan_server",
		Action:    "stopped",
		Detail:    "auxiliary LAN listener stopped",
		Meta:      map[string]any{"port": port, "lanEnabled": false},
	})
	return err
}

func (a *App) LANStatus() (active bool, port int, lastErr string) {
	a.lanMu.Lock()
	defer a.lanMu.Unlock()
	return a.lanServer != nil, a.lanPort, a.lanErr
}

func (a *App) setLANError(msg string) {
	a.lanMu.Lock()
	a.lanErr = msg
	a.lanMu.Unlock()
}

func (a *App) Run() error {
	errCh, err := a.Start()
	if err != nil {
		return err
	}
	return <-errCh
}

// ListenAddr returns the bound TCP address after Start succeeds (useful when Port is 0, e.g., embedded mobile runtimes).
func (a *App) ListenAddr() string {
	if v, ok := a.listenAddr.Load().(string); ok {
		return v
	}
	return a.server.Addr
}

// Start binds the listening socket before returning. Callers can safely open a
// browser after this succeeds without racing a later bind failure.
func (a *App) Start() (<-chan error, error) {
	listener, err := net.Listen("tcp", a.server.Addr)
	if err != nil {
		return nil, err
	}
	addr := listener.Addr().String()
	a.listenAddr.Store(addr)
	a.logger.Info("server listening", "address", addr)
	a.diag.Record(diagnostics.Event{
		Level:     diagnostics.LevelInfo,
		Component: "app",
		Stage:     "startup",
		Action:    "listening",
		Detail:    "HTTP server started",
	})
	ctx, cancel := context.WithCancel(context.Background())
	a.warmCancel, a.warmDone = cancel, make(chan struct{})
	go func() {
		defer close(a.warmDone)
		a.store.WarmShelf(ctx)
	}()
	errCh := make(chan error, 1)
	go func() {
		err := a.server.Serve(listener)
		if err == http.ErrServerClosed {
			err = nil
		}
		errCh <- err
		close(errCh)
	}()
	return errCh, nil
}

func (a *App) Shutdown(ctx context.Context) error {
	a.diag.Record(diagnostics.Event{
		Level:     diagnostics.LevelInfo,
		Component: "app",
		Stage:     "shutdown",
		Action:    "stop",
		Detail:    "HTTP server shutting down",
	})
	_ = a.StopLANServer()
	err := a.server.Shutdown(ctx)
	if a.metadata != nil {
		a.metadata.Close()
	}
	return err
}

func (a *App) Close() error {
	_ = a.StopLANServer()
	if a.warmCancel != nil {
		a.warmCancel()
		<-a.warmDone
	}
	if a.pipelines != nil {
		a.pipelines.Close()
	}
	if a.metadata != nil {
		a.metadata.Close()
	}
	a.tasks.Close()
	a.covers.Close()
	a.scanner.Close()
	a.archive.Close()
	if a.updater != nil {
		a.updater.Close()
	}
	if a.bangumi != nil {
		_ = a.bangumi.Close()
	}
	return a.store.Close()
}

func fetchPublicBangumiCover(ctx context.Context, proxy, coverURL string, subjectID int64) ([]byte, error) {
	if !bangumi.IsAllowedBangumiCoverURL(coverURL) {
		coverURL = bangumi.CoverEndpointURL(subjectID)
	}
	client := extmeta.NewSafeHTTPClient(proxy, 20*time.Second)
	checkRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !bangumi.IsAllowedBangumiCoverURL(req.URL.String()) {
			return bangumi.ErrCoverUnavailable
		}
		return checkRedirect(req, via)
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, coverURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MangaAlpha")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.HasPrefix(path.Base(resp.Request.URL.Path), "no_icon_subject") {
		return nil, bangumi.ErrCoverUnavailable
	}
	const maxCoverBytes = 16 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCoverBytes+1))
	if err == nil && (len(raw) == 0 || len(raw) > maxCoverBytes) {
		return nil, bangumi.ErrCoverUnavailable
	}
	return raw, err
}

func parseBookComicInfo(reader io.Reader) (database.BookScrapedMetadata, database.SeriesComicInfoBackfill, error) {
	meta, err := extmeta.ParseComicInfoXML(reader)
	if err != nil {
		return database.BookScrapedMetadata{}, database.SeriesComicInfoBackfill{}, err
	}
	var authors []database.Author
	for _, author := range meta.DetailedAuthors {
		authors = append(authors, database.Author{Name: author.Name, Role: author.Role})
	}
	var links []database.Link
	for _, link := range meta.Links {
		links = append(links, database.Link{Label: link.Label, URL: link.URL})
	}
	book := database.BookScrapedMetadata{
		Title: meta.Title, Summary: meta.Summary, Number: meta.Number, NumberSort: meta.NumberSort,
		ReleaseDate: meta.ReleaseDate, ISBN: meta.ISBN, Authors: authors, Tags: meta.Tags, Links: links,
		SeriesGroup: meta.SeriesGroup, StoryArc: meta.StoryArc, StoryArcNumber: meta.StoryArcNumber,
	}
	series := database.SeriesComicInfoBackfill{
		Title: meta.Series, TitleSort: meta.SeriesSort, Summary: meta.Summary,
		Publisher: meta.Publisher, Language: meta.LanguageISO, ReleaseDate: meta.ReleaseDate,
		TotalBookCount: meta.Count, AgeRating: meta.AgeRatingInt, ReadingDirection: meta.ReadingDirection,
		Genres: meta.Genres, Tags: meta.Tags, Authors: authors, Links: links, IsNSFW: meta.IsNSFW,
	}
	if meta.LocalizedSeries != "" {
		series.AlternateTitles = []database.AlternateTitle{{Title: meta.LocalizedSeries}}
	}
	return book, series, nil
}

func seedInitialAdminFromOptions(ctx context.Context, store *database.Store, options Options) error {
	user := strings.TrimSpace(options.InitialAdminUsername)
	pass := options.InitialAdminPassword
	if user == "" && pass == "" {
		return nil
	}
	claimed, err := store.HasAdmin(ctx)
	if err != nil {
		return fmt.Errorf("check administrator setup state: %w", err)
	}
	if claimed {
		options.Logger.Info("administrator already configured; ignoring FK115_ADMIN_USERNAME/FK115_ADMIN_PASSWORD")
		return nil
	}
	if user == "" || pass == "" {
		return errors.New("both FK115_ADMIN_USERNAME and FK115_ADMIN_PASSWORD must be set together for unattended initialization")
	}
	admin, err := store.ClaimAdmin(ctx, user, pass)
	if err != nil {
		return fmt.Errorf("initialize administrator from environment: %w", err)
	}
	options.Logger.Info("initial administrator configured from environment", "username", admin.Username)
	return nil
}

// The Android WebView uses a per-process cookie in addition to normal app
// authentication. The loopback port alone is not a security boundary.
func loopbackGuard(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		cookie, err := r.Cookie("fk115_mobile_guard")
		if host != "127.0.0.1" || err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(token)) != 1 {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
