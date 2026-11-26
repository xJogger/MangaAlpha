package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/xJogger/fake-komga-115/internal/archive"
	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/cache"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/diagnostics"
	"github.com/xJogger/fake-komga-115/internal/extmeta"
	"github.com/xJogger/fake-komga-115/internal/maintenance"
	"github.com/xJogger/fake-komga-115/internal/oneonefive"
	"github.com/xJogger/fake-komga-115/internal/pipeline"
	"github.com/xJogger/fake-komga-115/internal/scanner"
	"github.com/xJogger/fake-komga-115/internal/thumbnail"
	"github.com/xJogger/fake-komga-115/internal/updater"
	"github.com/xJogger/fake-komga-115/internal/webdav"
)

//go:embed static/web_*.html static/web.css static/ui static/reader_dist static/mangaalpha-bangumi.user.js
var staticFiles embed.FS

type Server struct {
	store          *database.Store
	client         *oneonefive.Client
	webdav         *webdav.Manager
	bangumi        *bangumi.Service
	metadata       *extmeta.Manager
	scanner        *scanner.Manager
	pipelines      *pipeline.Manager
	cache          *cache.Manager
	archive        *archive.Service
	thumbs         *thumbnail.Service
	covers         *thumbnail.BatchManager
	tasks          *maintenance.Manager
	diag           *diagnostics.Recorder
	updater        *updater.Service
	logger         *slog.Logger
	router         chi.Router
	web            *template.Template
	runtime        RuntimeInfo
	sse            *sseHub
	trustedProxies *trustedProxyChecker
	authLimiter    *authRateLimiter
	bgmOAuthStates *bangumiOAuthStateStore
	agentAuth      *agentAuthStore

	placeholder []byte
}

type RuntimeInfo struct {
	DataDir        string
	Host           string
	Port           int
	TrustedProxies string
}

func New(
	store *database.Store,
	client *oneonefive.Client,
	scanManager *scanner.Manager,
	cacheManager *cache.Manager,
	archiveService *archive.Service,
	thumbnailService *thumbnail.Service,
	coverManager *thumbnail.BatchManager,
	maintenanceManager *maintenance.Manager,
	runtimeInfo RuntimeInfo,
	logger *slog.Logger,
) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{
		store: store, client: client, webdav: webdav.NewManager(),
		scanner: scanManager, cache: cacheManager,
		archive: archiveService, thumbs: thumbnailService, covers: coverManager,
		tasks:          maintenanceManager,
		runtime:        runtimeInfo,
		logger:         logger,
		sse:            newSSEHub(),
		trustedProxies: newTrustedProxyChecker(runtimeInfo.TrustedProxies),
		authLimiter:    newAuthRateLimiter(),
		bgmOAuthStates: newBangumiOAuthStateStore(),
		agentAuth:      newAgentAuthStore(),
		placeholder:    makePlaceholder(),
		web:            template.Must(template.ParseFS(staticFiles, "static/web_*.html")),
	}
	s.router = s.routes()
	return s
}

func (s *Server) WithMetadata(manager *extmeta.Manager) *Server {
	s.metadata = manager
	return s
}

func (s *Server) WithServices(webdavManager *webdav.Manager, bangumiService *bangumi.Service) *Server {
	if webdavManager != nil {
		s.webdav = webdavManager
	}
	if bangumiService != nil {
		s.bangumi = bangumiService
		if s.thumbs != nil {
			bangumiService.SetCoverCacheInvalidator(s.thumbs.InvalidateBangumiCoverFailures)
			bangumiService.SetUserCoverCacheInvalidator(s.thumbs.InvalidateUserBangumiCoverFailures)
		}
	}
	return s
}

func (s *Server) WithPipeline(pipelineManager *pipeline.Manager) *Server {
	if pipelineManager != nil {
		s.pipelines = pipelineManager
	}
	return s
}

func (s *Server) WithDiagnostics(rec *diagnostics.Recorder) *Server {
	if rec != nil {
		s.diag = rec
	}
	return s
}

func (s *Server) WithUpdater(u *updater.Service) *Server {
	if u != nil {
		s.updater = u
	}
	return s
}

func (s *Server) Handler() http.Handler { return s.router }

const staleServiceWorkerScript = `self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    try {
      const keys = await caches.keys();
      await Promise.all(keys.map((k) => caches.delete(k).catch(() => false)));
    } catch (_) {}
    try {
      await self.registration.unregister();
    } catch (_) {}
    try {
      const clients = await self.clients.matchAll({ type: 'window' });
      await Promise.all(clients.map((c) => {
        try {
          return typeof c.navigate === 'function' ? c.navigate('/').catch(() => null) : null;
        } catch (_) {
          return null;
        }
      }));
    } catch (_) {}
  })());
});
`

func staleServiceWorkerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) ||
			!strings.EqualFold(strings.TrimSpace(r.Header.Get("Service-Worker")), "script") ||
			isAPIRequestPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		h := w.Header()
		h.Set("Content-Type", "application/javascript; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("Service-Worker-Allowed", "/")
		h.Add("Vary", "Service-Worker")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, staleServiceWorkerScript)
		}
	})
}

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(s.requestLogger)
	r.Use(securityHeadersMiddleware)
	r.Use(staleServiceWorkerMiddleware)
	r.Use(gzipMiddleware)
	r.Use(s.authMiddleware)

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "mangaalpha"})
	})
	r.Get("/favicon.ico", s.serveFavicon)

	// Embedded Bookshelf + Auth + Admin SPA routes
	r.Get("/", s.serveSPA)
	r.Get("/setup", s.serveSPA)
	r.Get("/login", s.serveSPA)
	r.Get("/libraries", s.serveSPA)
	r.Get("/libraries/{libraryID}", s.serveLibraryPageOrSPA)
	r.Get("/collections", s.serveSPA)
	r.Get("/collections/{collectionID}", s.serveCollectionPageOrSPA)
	r.Get("/readlists", s.serveSPA)
	r.Get("/readlists/{readListID}", s.serveReadListPageOrSPA)
	r.Get("/history", s.serveSPA)
	r.Get("/series/{seriesID}", s.serveSeriesPageOrSPA)
	r.Get("/book/{bookID}", s.serveBookPageOrSPA)
	r.Get("/books/{bookID}", s.serveBookPageOrSPA)
	r.Get("/bgm/userscript/latest.user.js", s.serveLatestUserscript)
	r.Get("/bgm/userscript/pair", s.serveUserscriptPairPage)
	r.Get("/bgm/{subjectID}", s.serveBangumiJump)
	r.Get("/agent-auth/{requestID}", s.serveSPA)
	r.Get("/account", s.serveSPA)
	r.Get("/account/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/account", http.StatusTemporaryRedirect)
	})
	r.Get("/admin", s.serveSPA)
	r.Get("/admin/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusTemporaryRedirect)
	})
	r.Get("/ui/*", s.serveUIAsset)
	r.Get("/assets/web.css", s.webStyles)

	// Built-in Suwayomi Reader routes (/reader/*)
	r.Get("/reader", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
	})
	r.Get("/reader/*", s.serveReader)

	// Admin & internal UI API
	r.Route("/admin/api", s.adminRoutes)

	// Agent-friendly management API (/api/agent/v1)
	r.Route("/api/agent/v1", s.agentRoutes)

	// OPDS v1.2 + OPDS-PSE dual entrypoints:
	// 1. /api/opds/v1.2 (Suwayomi-style two-step metadata feed for KOReader opdsforcomic.koplugin)
	// 2. /opds/v1.2     (Standard Komga OPDS v1.2 catalog)
	r.Route("/api/opds/v1.2", s.suwayomiOPDSRoutes)
	r.Route("/opds/v1.2", s.komgaOPDSRoutes)

	// Komga v1 & v2 compatibility API
	r.Route("/api/v1", s.komgaRoutes)
	r.Get("/api/v2/users/me", s.komgaMe)
	r.Patch("/api/v2/users/me/password", s.komgaPatchMyPassword)
	r.Get("/api/v2/authors", s.komgaAuthorsV2)
	r.Get("/api/v2/series/{seriesID}/read-progress/tachiyomi", s.getSeriesProgress)
	r.Put("/api/v2/series/{seriesID}/read-progress/tachiyomi", s.putSeriesProgress)

	r.NotFound(s.notFound)
	return r
}

func (s *Server) serveSPA(w http.ResponseWriter, _ *http.Request) {
	data, err := staticFiles.ReadFile("static/ui/app.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

func (s *Server) serveLibraryPageOrSPA(w http.ResponseWriter, r *http.Request) {
	libraryID := chi.URLParam(r, "libraryID")
	if !s.store.IsLibraryAccessible(r.Context(), libraryID) {
		s.renderNotFound(w)
		return
	}
	s.serveSPA(w, r)
}

func (s *Server) serveFavicon(w http.ResponseWriter, _ *http.Request) {
	data, err := staticFiles.ReadFile("static/ui/mangaalpha-icon.png")
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}

// AppIconPNG returns the embedded MangaAlpha application icon PNG bytes (used by
// the Windows system tray to avoid duplicating binary icon assets).
func AppIconPNG() []byte {
	data, err := staticFiles.ReadFile("static/ui/mangaalpha-icon.png")
	if err != nil {
		return nil
	}
	return data
}

func (s *Server) serveCollectionPageOrSPA(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "collectionID")
	if _, err := s.store.CollectionByID(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.renderNotFound(w)
			return
		}
		s.renderWebError(w, err)
		return
	}
	s.serveSPA(w, r)
}

func (s *Server) serveReadListPageOrSPA(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "readListID")
	if _, err := s.store.ReadListByID(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.renderNotFound(w)
			return
		}
		s.renderWebError(w, err)
		return
	}
	s.serveSPA(w, r)
}

func (s *Server) serveSeriesPageOrSPA(w http.ResponseWriter, r *http.Request) {
	seriesID := chi.URLParam(r, "seriesID")
	series, err := s.store.SeriesByID(r.Context(), seriesID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.renderNotFound(w)
			return
		}
		s.renderWebError(w, err)
		return
	}
	if series.ID != seriesID {
		target := "/series/" + series.ID
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}
	s.serveSPA(w, r)
}

func (s *Server) serveBookPageOrSPA(w http.ResponseWriter, r *http.Request) {
	bookID := chi.URLParam(r, "bookID")
	book, err := s.store.BookByID(r.Context(), bookID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.renderNotFound(w)
			return
		}
		s.renderWebError(w, err)
		return
	}
	if book.ID != bookID {
		target := "/books/" + book.ID
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}
	s.serveSPA(w, r)
}

func (s *Server) serveUIAsset(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(staticFiles, "static/ui")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rel := strings.TrimPrefix(path.Clean(r.URL.Path), "/ui/")
	if rel == "" || rel == "." || strings.Contains(rel, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(sub, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	setStaticContentType(w, rel)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

func (s *Server) serveReader(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(staticFiles, "static/reader_dist")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rel := strings.TrimPrefix(path.Clean(r.URL.Path), "/reader")
	rel = strings.TrimPrefix(rel, "/")

	// Convenience redirect: /reader/book/{bookID} -> /reader/series/{seriesID}/book/{bookID}
	if strings.HasPrefix(rel, "book/") {
		bookID := strings.TrimPrefix(rel, "book/")
		if cut := strings.IndexByte(bookID, '/'); cut >= 0 {
			bookID = bookID[:cut]
		}
		book, err := s.store.BookByID(r.Context(), bookID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				s.renderNotFound(w)
				return
			}
			s.renderWebError(w, err)
			return
		}
		target := "/reader/series/" + book.SeriesID + "/book/" + book.ID
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}

	// Gate direct reader entry URLs (/reader/series/{seriesID}/book/{bookID}) when series/book is missing or library is hidden
	if strings.HasPrefix(rel, "series/") {
		parts := strings.Split(strings.TrimPrefix(rel, "series/"), "/")
		seriesID := parts[0]
		if seriesID == "" {
			s.renderNotFound(w)
			return
		}
		series, err := s.store.SeriesByID(r.Context(), seriesID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				s.renderNotFound(w)
				return
			}
			s.renderWebError(w, err)
			return
		}
		if len(parts) >= 3 && parts[1] == "book" && parts[2] != "" {
			book, err := s.store.BookByID(r.Context(), parts[2])
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					s.renderNotFound(w)
					return
				}
				s.renderWebError(w, err)
				return
			}
			if book.SeriesID != series.ID {
				s.renderNotFound(w)
				return
			}
			if series.ID != seriesID || book.ID != parts[2] {
				target := "/reader/series/" + series.ID + "/book/" + book.ID
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusTemporaryRedirect)
				return
			}
		} else if series.ID != seriesID {
			target := "/reader/series/" + series.ID
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
			return
		}
	}

	if rel != "" && rel != "." && !strings.Contains(rel, "..") && rel != "placeholder.txt" {
		if data, err := fs.ReadFile(sub, rel); err == nil {
			setStaticContentType(w, rel)
			if strings.HasPrefix(rel, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			_, _ = w.Write(data)
			return
		}
		// Missing asset files under /reader/assets/ must 404 rather than fall back to index.html
		if strings.HasPrefix(rel, "assets/") || strings.HasPrefix(rel, "locales/") {
			http.NotFound(w, r)
			return
		}
	}

	indexBytes, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "Built-in reader bundle is not compiled yet. Run scripts/build-reader.sh.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(indexBytes)
}

func setStaticContentType(w http.ResponseWriter, rel string) {
	switch strings.ToLower(path.Ext(rel)) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js", ".mjs":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case ".json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	case ".png":
		w.Header().Set("Content-Type", "image/png")
	case ".jpg", ".jpeg":
		w.Header().Set("Content-Type", "image/jpeg")
	case ".webp":
		w.Header().Set("Content-Type", "image/webp")
	case ".woff2":
		w.Header().Set("Content-Type", "font/woff2")
	case ".woff":
		w.Header().Set("Content-Type", "font/woff")
	case ".ttf":
		w.Header().Set("Content-Type", "font/ttf")
	case ".txt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	case ".ico":
		w.Header().Set("Content-Type", "image/x-icon")
	case ".webmanifest":
		w.Header().Set("Content-Type", "application/manifest+json")
	}
}

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(wrapped, r)
		s.logger.Info("request", "method", r.Method, "path", r.URL.Path,
			"status", wrapped.Status(), "duration", time.Since(start))
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}

func (s *Server) writeDBError(w http.ResponseWriter, r *http.Request, err error) {
	s.writeDBErrorMessage(w, r, err, "")
}

func (s *Server) writeDBErrorMessage(w http.ResponseWriter, r *http.Request, err error, message string) {
	if errors.Is(err, context.Canceled) || (r != nil && errors.Is(r.Context().Err(), context.Canceled)) {
		writeError(w, 499, "REQUEST_CANCELED", "request canceled")
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || (r != nil && errors.Is(r.Context().Err(), context.DeadlineExceeded)) {
		writeError(w, http.StatusGatewayTimeout, "REQUEST_TIMEOUT", "request timed out")
		return
	}
	if s.logger != nil {
		method, urlPath := "", ""
		if r != nil {
			method, urlPath = r.Method, r.URL.Path
		}
		s.logger.Error("database error", "method", method, "path", urlPath, "error", err)
	}
	if message == "" {
		message = "Internal database error."
	}
	writeError(w, http.StatusInternalServerError, "DATABASE_ERROR", message)
}

func intQuery(r *http.Request, key string, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return fallback
	}
	return value
}

func listQuery(r *http.Request, key string) []string {
	var out []string
	for _, group := range r.URL.Query()[key] {
		for _, item := range strings.Split(group, ",") {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
	}
	return out
}

func boolQuery(r *http.Request, key string) *bool {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return nil
	}
	return &parsed
}

func (s *Server) archiveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, archive.ErrRangeNotSupported):
		writeError(w, http.StatusNotImplemented, "RANGE_NOT_SUPPORTED", err.Error())
	case errors.Is(err, archive.ErrPageTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "PAGE_TOO_LARGE", err.Error())
	case errors.Is(err, archive.ErrInvalidZIP), errors.Is(err, archive.ErrUnsupportedZIP):
		writeError(w, http.StatusUnprocessableEntity, "INVALID_ZIP", err.Error())
	case errors.Is(err, archive.ErrSolidRAR):
		writeError(w, http.StatusUnprocessableEntity, "SOLID_RAR_NOT_SUPPORTED", err.Error())
	case errors.Is(err, archive.ErrArchivePasswordRequired):
		writeError(w, http.StatusUnprocessableEntity, "ARCHIVE_PASSWORD_REQUIRED", "此压缩包已加密，请输入解压密码。")
	case errors.Is(err, archive.ErrArchivePasswordInvalid):
		writeError(w, http.StatusUnprocessableEntity, "ARCHIVE_PASSWORD_INVALID", "解压密码不正确，请重新输入。")
	case errors.Is(err, archive.ErrEncryptedRAR):
		writeError(w, http.StatusUnprocessableEntity, "ARCHIVE_PASSWORD_REQUIRED", "此压缩包已加密，请输入解压密码。")
	case errors.Is(err, archive.ErrMultiVolumeRAR):
		writeError(w, http.StatusUnprocessableEntity, "MULTI_VOLUME_RAR_NOT_SUPPORTED", err.Error())
	case errors.Is(err, archive.ErrInvalidRAR), errors.Is(err, archive.ErrUnsupportedRAR):
		writeError(w, http.StatusUnprocessableEntity, "INVALID_RAR", err.Error())
	case errors.Is(err, archive.ErrUnsupportedArchive):
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_ARCHIVE", err.Error())
	case errors.Is(err, context.Canceled):
		writeError(w, 499, "REQUEST_CANCELED", err.Error())
	default:
		s.logger.Error("archive request", "error", err)
		writeError(w, http.StatusBadGateway, "REMOTE_READ_FAILED", "Failed to read the comic archive.")
	}
}

func makePlaceholder() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 320, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 320; x++ {
			shade := uint8(40)
			if (x/32+y/32)%2 == 0 {
				shade = 48
			}
			img.Set(x, y, color.RGBA{shade, shade, shade, 255})
		}
	}
	var buffer bytes.Buffer
	_ = png.Encode(&buffer, img)
	return buffer.Bytes()
}

func (s *Server) writeUncachedPlaceholder(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", fmt.Sprint(len(s.placeholder)))
	_, _ = w.Write(s.placeholder)
}
