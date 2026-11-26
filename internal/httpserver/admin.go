package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/xJogger/fake-komga-115/internal/archive"
	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/buildinfo"
	"github.com/xJogger/fake-komga-115/internal/cache"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/id"
	"github.com/xJogger/fake-komga-115/internal/localfs"
	"github.com/xJogger/fake-komga-115/internal/maintenance"
	"github.com/xJogger/fake-komga-115/internal/oneonefive"
	"github.com/xJogger/fake-komga-115/internal/pipeline"
	"github.com/xJogger/fake-komga-115/internal/scanner"
	"github.com/xJogger/fake-komga-115/internal/thumbnail"
	"github.com/xJogger/fake-komga-115/internal/webdav"
)

func (s *Server) adminRoutes(r chi.Router) {
	// Auth, self-service account & personal API key / session routes (available to any authenticated user;
	// /auth/status, /auth/setup, /auth/login, /auth/logout are gated by authMiddleware)
	r.Get("/auth/status", s.authStatusHandler)
	r.Post("/auth/setup", s.authSetupHandler)
	r.Post("/auth/login", s.authLoginHandler)
	r.Post("/auth/logout", s.authLogoutHandler)
	r.Put("/auth/credentials", s.updateCredentialsHandler)
	r.Get("/auth/sessions", s.listSessionsHandler)
	r.Delete("/auth/sessions/{sessionID}", s.deleteSessionHandler)
	r.Get("/auth/api-keys", s.listAPIKeysHandler)
	r.Post("/auth/api-keys", s.createAPIKeyHandler)
	r.Delete("/auth/api-keys/{keyID}", s.deleteAPIKeyHandler)

	// Personal account overview & preferences
	r.Get("/me", s.getMyAccount)
	r.Put("/me/preferences", s.putMyPreferences)

	// Built-in bookshelf & reader endpoints available to any authorized user (library & NSFW visibility enforced per user)
	r.Get("/bookshelf/series", s.bookshelfSeries)
	r.Get("/bookshelf/versions/{seriesID}", s.bookshelfVersions)
	r.Get("/bookshelf/series/{seriesID}/versions", s.bookshelfVersions)
	r.Get("/bookshelf/continue-reading", s.bookshelfContinueReading)
	r.Get("/bookshelf/history", s.bookshelfHistory)
	r.Get("/series/{seriesID}/detail", s.seriesRichDetail)
	r.Get("/books/{bookID}/detail", s.bookRichDetail)
	r.Post("/books/{bookID}/unlock", s.unlockBookArchiveHandler)

	// Per-user Bangumi userscript pairing & Bangumi progress tracker
	r.Get("/bangumi/userscript.user.js", s.downloadBangumiUserscript)
	r.Post("/bangumi/userscript/pair-key", s.createUserscriptPairKey)
	r.Get("/bangumi/tracker", s.getBangumiTracker)
	r.Put("/bangumi/tracker/config", s.putBangumiTrackerConfig)
	r.Post("/bangumi/tracker/test", s.testBangumiTracker)
	r.Post("/bangumi/tracker/oauth/start", s.startBangumiTrackerOAuth)
	r.Get("/bangumi/tracker/oauth/callback", s.bangumiTrackerOAuthCallback)
	r.Post("/bangumi/tracker/oauth/exchange", s.exchangeBangumiTrackerOAuthCode)
	r.Post("/bangumi/tracker/oauth/refresh", s.refreshBangumiTrackerOAuth)
	r.Post("/series/{seriesID}/bangumi/tracker/retry", s.retrySeriesBangumiTracker)

	// Administrator-only management routes
	r.Group(func(ar chi.Router) {
		ar.Use(s.requireAdmin)
		s.metadataRoutes(ar)

		// Multi-user management
		ar.Get("/users", s.adminListUsers)
		ar.Post("/users", s.adminCreateUser)
		ar.Put("/users/{userID}", s.adminUpdateUser)
		ar.Post("/users/{userID}/reset-password", s.adminResetUserPassword)
		ar.Post("/users/{userID}/kick", s.adminKickUserSessions)
		ar.Delete("/users/{userID}", s.adminDeleteUser)

		// Encrypted archive password vault management (plaintext passwords are admin-only)
		ar.Get("/archive-passwords", s.listArchivePasswordsHandler)
		ar.Delete("/archive-passwords", s.clearArchivePasswordsHandler)
		ar.Delete("/archive-passwords/{passwordID}", s.deleteArchivePasswordHandler)
		ar.Delete("/archive-passwords/bindings/{bookID}", s.deleteArchivePasswordBindingHandler)

		// Agent Device Auth approval
		ar.Get("/agent/auth-requests/{requestID}", s.getAdminAgentAuthRequest)
		ar.Post("/agent/auth-requests/{requestID}/approve", s.approveAdminAgentAuthRequest)
		ar.Post("/agent/auth-requests/{requestID}/reject", s.rejectAdminAgentAuthRequest)

		// Status & 115 account
		ar.Get("/status", s.adminStatus)
		ar.Get("/account", s.getAccount)
		ar.Put("/account", s.putAccount)
		ar.Post("/account/test", s.testAccount)
		ar.Post("/account/refresh", s.refreshAccount)
		ar.Get("/115/folders", s.list115Folders)

		// Local & WebDAV storage sources
		ar.Get("/local/folders", s.listLocalFolders)
		ar.Get("/webdav-accounts", s.listWebDAVAccounts)
		ar.Post("/webdav-accounts", s.createWebDAVAccount)
		ar.Put("/webdav-accounts/{accountID}", s.updateWebDAVAccount)
		ar.Delete("/webdav-accounts/{accountID}", s.deleteWebDAVAccount)
		ar.Post("/webdav-accounts/test", s.testWebDAVAccount)
		ar.Get("/webdav-accounts/{accountID}/folders", s.listWebDAVFolders)

		// Libraries, union refresh, schedules, pipelines, settings, scans, covers, maintenance, cache
		ar.Get("/libraries", s.listLibraries)
		ar.Post("/libraries", s.createLibrary)
		ar.Put("/libraries/{libraryID}", s.updateLibrary)
		ar.Delete("/libraries/{libraryID}", s.deleteLibrary)
		ar.Post("/libraries/{libraryID}/union-refresh", s.refreshUnionLibrary)
		ar.Get("/schedules", s.listLibrarySchedules)
		ar.Put("/libraries/{libraryID}/schedule", s.putLibrarySchedule)
		ar.Get("/pipelines", s.listPipelineRuns)
		ar.Post("/pipelines", s.startPipelineRun)
		ar.Post("/pipelines/{runID}/cancel", s.cancelPipelineRun)
		ar.Get("/settings", s.getSettings)
		ar.Put("/settings", s.putSettings)
		ar.Get("/scans", s.listScans)
		ar.Post("/scans", s.startScan)
		ar.Post("/scans/{runID}/cancel", s.cancelScan)
		ar.Get("/cover-jobs", s.listCoverJobs)
		ar.Post("/cover-jobs", s.startCoverJob)
		ar.Post("/cover-jobs/{runID}/cancel", s.cancelCoverJob)
		ar.Get("/maintenance-jobs", s.listMaintenanceJobs)
		ar.Post("/maintenance-jobs", s.startMaintenanceJob)
		ar.Post("/maintenance-jobs/{runID}/cancel", s.cancelMaintenanceJob)
		ar.Get("/cache", s.cacheStats)
		ar.Delete("/cache/{cacheType}", s.clearCache)

		// Sanitized diagnostics log preview, verbose toggle, export & clear
		ar.Get("/diagnostics", s.getDiagnostics)
		ar.Put("/diagnostics/config", s.putDiagnosticsConfig)
		ar.Get("/diagnostics/export", s.exportDiagnostics)
		ar.Delete("/diagnostics", s.clearDiagnostics)

		// Cross-platform release update detection
		ar.Get("/update", s.getUpdateStatus)
		ar.Get("/update/status", s.getUpdateStatus)
		ar.Post("/update/check", s.postUpdateCheck)
		ar.Put("/update/config", s.putUpdateConfig)

		// Global Bangumi scraping, proxy & offline archive management
		ar.Get("/bangumi/status", s.bangumiStatus)
		ar.Put("/bangumi/config", s.putBangumiConfig)
		ar.Post("/bangumi/proxy/test", s.testBangumiProxy)
		ar.Get("/bangumi/subjects/{subjectID}/cover", s.bangumiSubjectCover)
		ar.Get("/bangumi/archive/source", s.getBangumiArchiveSource)
		ar.Put("/bangumi/archive/source", s.putBangumiArchiveSource)
		ar.Get("/bangumi/archive/schedule", s.getBangumiArchiveSchedule)
		ar.Put("/bangumi/archive/schedule", s.putBangumiArchiveSchedule)
		ar.Post("/bangumi/archive/update", s.startBangumiArchiveUpdate)
		ar.Post("/bangumi/archive/cancel", s.cancelBangumiArchiveUpdate)
		ar.Get("/bangumi/runs", s.listBangumiRuns)
		ar.Post("/bangumi/runs", s.startBangumiRun)
		ar.Post("/bangumi/runs/{runID}/cancel", s.cancelBangumiRun)
		ar.Get("/series/{seriesID}/bangumi/candidates", s.searchSeriesBangumi)
		ar.Post("/series/{seriesID}/bangumi/bind", s.bindSeriesBangumi)
		ar.Post("/series/{seriesID}/bangumi/auto", s.autoMatchSeriesBangumi)
		ar.Delete("/series/{seriesID}/bangumi/bind", s.clearSeriesBangumi)
	})
}

func (s *Server) adminStatus(w http.ResponseWriter, r *http.Request) {
	account, _ := s.store.Account(r.Context())
	webdavAccounts, _ := s.store.WebDAVAccounts(r.Context())
	libraries, series, books, comicBytes, err := s.store.Counts(adminCatalogContext(r.Context()))
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	hiddenLibraries, _ := s.store.HiddenLibraryCount(r.Context())
	nsfwSeries, _ := s.store.NSFWSeriesCount(r.Context())
	showNSFW := s.store.ShowNSFW(r.Context())
	performance, err := s.store.GlobalPerformance(r.Context())
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	matched, unmatched, _, _ := s.store.ScrapeSummary(r.Context())
	writeJSON(w, 200, map[string]any{
		"configured":      account.RefreshToken != "" || len(webdavAccounts) > 0 || (libraries+hiddenLibraries) > 0,
		"configured115":   account.RefreshToken != "",
		"webdavAccounts":  len(webdavAccounts),
		"libraries":       libraries,
		"hiddenLibraries": hiddenLibraries,
		"series":          series,
		"books":           books,
		"comicBytes":      comicBytes,
		"bgmMatched":      matched,
		"bgmUnmatched":    unmatched,
		"showNsfw":        showNSFW,
		"nsfwSeries":      nsfwSeries,
		"version":         buildinfo.Version,
		"dataDir":         s.runtime.DataDir,
		"adminUrl":        localServiceURL(s.runtime.Host, s.runtime.Port),
		"mihonAddresses":  mihonAddresses(s.runtime.Host, s.runtime.Port),
		"performance":     performance,
	})
}

func localServiceURL(host string, port int) string {
	host = strings.TrimSpace(host)
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

func mihonAddresses(host string, port int) []string {
	host = strings.TrimSpace(host)
	if host != "" && host != "0.0.0.0" && host != "::" && host != "[::]" {
		return []string{localServiceURL(host, port)}
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var result []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 ||
			isVirtualInterface(iface.Name) {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil {
				continue
			}
			ip = ip.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			url := "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(port))
			if _, ok := seen[url]; ok {
				continue
			}
			seen[url] = struct{}{}
			result = append(result, url)
		}
	}
	sort.Strings(result)
	return result
}

func isVirtualInterface(name string) bool {
	name = strings.ToLower(name)
	for _, prefix := range []string{
		"br-", "docker", "veth", "virbr", "vmnet", "vboxnet", "vethernet", "wsl",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	account, err := s.store.Account(r.Context())
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"hasAccessToken":  account.AccessToken != "",
		"hasRefreshToken": account.RefreshToken != "",
		"updatedAt":       account.UpdatedAt,
	})
}

func (s *Server) putAccount(w http.ResponseWriter, r *http.Request) {
	var request struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	existing, _ := s.store.Account(r.Context())
	newRefreshProvided := strings.TrimSpace(request.RefreshToken) != ""
	if strings.TrimSpace(request.AccessToken) == "" && !newRefreshProvided {
		request.AccessToken = existing.AccessToken
	}
	if strings.TrimSpace(request.RefreshToken) == "" {
		request.RefreshToken = existing.RefreshToken
	}
	info, err := s.client.SaveAndTest(r.Context(), request.AccessToken, request.RefreshToken)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ONEONEFIVE_AUTH_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, info)
}

func (s *Server) testAccount(w http.ResponseWriter, r *http.Request) {
	info, err := s.client.Test(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "ONEONEFIVE_AUTH_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, info)
}

func (s *Server) refreshAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.client.RefreshToken(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, "ONEONEFIVE_AUTH_FAILED", err.Error())
		return
	}
	info, err := s.client.Test(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "ONEONEFIVE_AUTH_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, info)
}

type folder115Item struct {
	ID       string `json:"id"`
	FileID   string `json:"fileId"`
	ParentID string `json:"parentId"`
	Name     string `json:"name"`
	PickCode string `json:"pickCode"`
	IsDir    bool   `json:"isDir"`
}

func filter115Folders(files []oneonefive.File) []folder115Item {
	folders := make([]folder115Item, 0)
	for _, file := range files {
		if !file.IsDir {
			continue
		}
		fid := strings.TrimSpace(file.ID)
		if fid == "" {
			continue
		}
		folders = append(folders, folder115Item{
			ID:       fid,
			FileID:   fid,
			ParentID: file.ParentID,
			Name:     file.Name,
			PickCode: file.PickCode,
			IsDir:    true,
		})
	}
	return folders
}

func (s *Server) list115Folders(w http.ResponseWriter, r *http.Request) {
	cid := strings.TrimSpace(r.URL.Query().Get("cid"))
	if cid == "" {
		cid = "0"
	}
	files, err := s.client.ListDirectory(r.Context(), cid)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ONEONEFIVE_LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"cid": cid, "folders": filter115Folders(files)})
}

func (s *Server) listLocalFolders(w http.ResponseWriter, r *http.Request) {
	rawPath := strings.TrimSpace(r.URL.Query().Get("path"))
	current, parent, folders, err := localfs.BrowseDirectories(rawPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, "LOCAL_BROWSE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current": current,
		"parent":  parent,
		"folders": folders,
	})
}

// --- WebDAV Account Management ---

func (s *Server) listWebDAVAccounts(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.WebDAVAccounts(r.Context())
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

type webdavAccountRequest struct {
	Name          string  `json:"name"`
	BaseURL       string  `json:"baseUrl"`
	Username      string  `json:"username"`
	Password      string  `json:"password"`
	InsecureTLS   bool    `json:"insecureTls"`
	RatePerSecond float64 `json:"ratePerSecond"`
}

func (s *Server) createWebDAVAccount(w http.ResponseWriter, r *http.Request) {
	var req webdavAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	cleanURL, err := webdav.ValidateBaseURL(req.BaseURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_WEBDAV_URL", err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_WEBDAV_NAME", "name is required")
		return
	}
	item, err := s.store.UpsertWebDAVAccount(r.Context(), database.WebDAVAccountSecret{
		WebDAVAccount: database.WebDAVAccount{
			Name:          strings.TrimSpace(req.Name),
			BaseURL:       cleanURL,
			Username:      strings.TrimSpace(req.Username),
			InsecureTLS:   req.InsecureTLS,
			RatePerSecond: req.RatePerSecond,
		},
		Password: req.Password,
	}, false)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) updateWebDAVAccount(w http.ResponseWriter, r *http.Request) {
	accountID := chi.URLParam(r, "accountID")
	if _, err := s.store.WebDAVAccount(r.Context(), accountID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "WebDAV account not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	var req webdavAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	cleanURL, err := webdav.ValidateBaseURL(req.BaseURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_WEBDAV_URL", err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_WEBDAV_NAME", "name is required")
		return
	}
	item, err := s.store.UpsertWebDAVAccount(r.Context(), database.WebDAVAccountSecret{
		WebDAVAccount: database.WebDAVAccount{
			ID:            accountID,
			Name:          strings.TrimSpace(req.Name),
			BaseURL:       cleanURL,
			Username:      strings.TrimSpace(req.Username),
			InsecureTLS:   req.InsecureTLS,
			RatePerSecond: req.RatePerSecond,
		},
		Password: req.Password,
	}, true)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) deleteWebDAVAccount(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteWebDAVAccount(r.Context(), chi.URLParam(r, "accountID"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "WebDAV 账号不存在或已被删除。")
			return
		}
		var inUseErr *database.WebDAVAccountInUseError
		if errors.As(err, &inUseErr) {
			msg := "无法删除该 WebDAV 账号：仍有书库正在使用，请先在「书库管理」中移除对应书库。"
			if len(inUseErr.Libraries) > 0 {
				msg = "无法删除该 WebDAV 账号，以下书库仍在使用：" + strings.Join(inUseErr.Libraries, "、") + "。请先在「书库管理」中移除对应书库。"
			}
			writeError(w, http.StatusConflict, "WEBDAV_IN_USE", msg)
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testWebDAVAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID            string  `json:"id"`
		BaseURL       string  `json:"baseUrl"`
		Username      string  `json:"username"`
		Password      string  `json:"password"`
		InsecureTLS   bool    `json:"insecureTls"`
		RatePerSecond float64 `json:"ratePerSecond"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	var secret database.WebDAVAccountSecret
	if req.ID != "" && req.BaseURL == "" {
		var err error
		secret, err = s.store.WebDAVAccountSecret(r.Context(), req.ID)
		if err != nil {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "WebDAV account not found.")
			return
		}
	} else {
		cleanURL, err := webdav.ValidateBaseURL(req.BaseURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_WEBDAV_URL", err.Error())
			return
		}
		password := req.Password
		if password == "" && req.ID != "" {
			if existing, err := s.store.WebDAVAccountSecret(r.Context(), req.ID); err == nil {
				if sameSchemeAndHost(cleanURL, existing.BaseURL) {
					password = existing.Password
				} else if existing.Password != "" {
					writeError(w, http.StatusBadRequest, "PASSWORD_REQUIRED", "修改 WebDAV 服务器地址后，请重新输入密码再测试连接。")
					return
				}
			}
		}
		secret = database.WebDAVAccountSecret{
			WebDAVAccount: database.WebDAVAccount{
				BaseURL:       cleanURL,
				Username:      strings.TrimSpace(req.Username),
				InsecureTLS:   req.InsecureTLS,
				RatePerSecond: req.RatePerSecond,
			},
			Password: password,
		}
	}
	if err := s.webdav.TestConnection(r.Context(), secret); err != nil {
		writeError(w, http.StatusBadGateway, "WEBDAV_TEST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) listWebDAVFolders(w http.ResponseWriter, r *http.Request) {
	secret, err := s.store.WebDAVAccountSecret(r.Context(), chi.URLParam(r, "accountID"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "WebDAV account not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	remoteDir := r.URL.Query().Get("path")
	current, parent, folders, err := s.webdav.ListFolders(r.Context(), secret, remoteDir)
	if err != nil {
		writeError(w, http.StatusBadGateway, "WEBDAV_LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current": current,
		"parent":  parent,
		"folders": folders,
	})
}

// --- Libraries ---

func (s *Server) listLibraries(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.Libraries(r.Context(), false)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, 200, items)
}

func (s *Server) createLibrary(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name              string   `json:"name"`
		ProviderType      string   `json:"providerType"`
		RootCID           string   `json:"rootCid"`
		WebDAVAccountID   string   `json:"webdavAccountId"`
		RootPath          string   `json:"rootPath"`
		Enabled           *bool    `json:"enabled"`
		OneShot           bool     `json:"oneShot"`
		OneshotsDirectory string   `json:"oneshotsDirectory"`
		MemberLibraryIDs  []string `json:"memberLibraryIds"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" {
		writeError(w, 400, "INVALID_LIBRARY", "name is required")
		return
	}
	provider := strings.ToLower(strings.TrimSpace(request.ProviderType))
	if provider == "" {
		provider = database.Provider115
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}

	if provider == database.ProviderUnion {
		lib, stats, err := s.store.CreateUnionLibrary(r.Context(), request.Name, enabled, request.MemberLibraryIDs)
		if err != nil {
			writeError(w, 400, "INVALID_UNION_LIBRARY", err.Error())
			return
		}
		if s.sse != nil {
			s.sse.PublishMatching("LibraryAdded", map[string]any{"libraryId": lib.ID}, func(uid string) bool {
				u, uErr := s.store.UserByID(r.Context(), uid)
				if uErr != nil || !u.Enabled {
					return false
				}
				return u.IsAdmin() || u.AllLibraries
			})
		}
		writeJSON(w, 201, map[string]any{
			"library": lib,
			"stats":   stats,
		})
		return
	}

	var item database.Library
	switch provider {
	case database.Provider115:
		rootCID := strings.TrimSpace(request.RootCID)
		if rootCID == "" {
			writeError(w, 400, "INVALID_LIBRARY", "rootCid is required for 115 libraries")
			return
		}
		item = database.Library{
			ID:           id.Library(rootCID),
			Name:         request.Name,
			ProviderType: database.Provider115,
			RootCID:      rootCID,
			Enabled:      enabled,
			OneShot:      request.OneShot,
		}
	case database.ProviderLocal:
		cleanRoot, err := localfs.CleanRoot(request.RootPath)
		if err != nil {
			writeError(w, 400, "INVALID_LOCAL_PATH", err.Error())
			return
		}
		locator := "local:" + cleanRoot
		item = database.Library{
			ID:           id.Library(locator),
			Name:         request.Name,
			ProviderType: database.ProviderLocal,
			RootCID:      locator,
			RootPath:     cleanRoot,
			Enabled:      enabled,
			OneShot:      request.OneShot,
		}
	case database.ProviderWebDAV:
		accountID := strings.TrimSpace(request.WebDAVAccountID)
		if accountID == "" {
			writeError(w, 400, "INVALID_LIBRARY", "webdavAccountId is required for WebDAV libraries")
			return
		}
		if _, err := s.store.WebDAVAccount(r.Context(), accountID); err != nil {
			writeError(w, 400, "INVALID_WEBDAV_ACCOUNT", "Selected WebDAV account does not exist")
			return
		}
		cleanRemote := webdav.CleanRemotePath(request.RootPath)
		locator := "webdav:" + accountID + ":" + cleanRemote
		item = database.Library{
			ID:              id.Library(locator),
			Name:            request.Name,
			ProviderType:    database.ProviderWebDAV,
			RootCID:         locator,
			WebDAVAccountID: accountID,
			RootPath:        cleanRemote,
			Enabled:         enabled,
			OneShot:         request.OneShot,
		}
	default:
		writeError(w, 400, "INVALID_PROVIDER", "Unsupported providerType: "+provider)
		return
	}

	item.OneshotsDirectory = strings.TrimSpace(request.OneshotsDirectory)

	var existing int
	_ = s.store.DB().QueryRowContext(r.Context(), `SELECT count(*) FROM libraries WHERE id=?`, item.ID).Scan(&existing)
	if existing > 0 {
		writeError(w, 409, "LIBRARY_EXISTS", "This library root is already configured.")
		return
	}

	if err := s.store.UpsertLibrary(r.Context(), item); err != nil {
		if errors.Is(err, database.ErrWebDAVAccountNotFound) {
			writeError(w, 400, "INVALID_WEBDAV_ACCOUNT", "Selected WebDAV account does not exist")
		} else if strings.Contains(strings.ToLower(err.Error()), "unique") {
			writeError(w, 409, "LIBRARY_EXISTS", "This library root is already configured.")
		} else {
			s.writeDBError(w, r, err)
		}
		return
	}
	item, _ = s.store.Library(r.Context(), item.ID)
	writeJSON(w, 201, item)
}

func (s *Server) updateLibrary(w http.ResponseWriter, r *http.Request) {
	item, err := s.store.Library(r.Context(), chi.URLParam(r, "libraryID"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, "NOT_FOUND", "Library not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	wasEnabled := item.Enabled
	var request struct {
		Name              *string   `json:"name"`
		Enabled           *bool     `json:"enabled"`
		OneShot           *bool     `json:"oneShot"`
		OneshotsDirectory *string   `json:"oneshotsDirectory"`
		MemberLibraryIDs  *[]string `json:"memberLibraryIds"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if item.EffectiveProvider() == database.ProviderUnion {
		var memberIDs []string
		if request.MemberLibraryIDs != nil {
			memberIDs = *request.MemberLibraryIDs
		}
		updated, _, err := s.store.UpdateUnionLibrary(r.Context(), item.ID, request.Name, request.Enabled, memberIDs)
		if err != nil {
			writeError(w, 400, "INVALID_UNION_LIBRARY", err.Error())
			return
		}
		if wasEnabled && !updated.Enabled {
			s.cancelLibraryJobs(r.Context(), updated.ID)
		}
		writeJSON(w, 200, updated)
		return
	}
	if request.Name != nil {
		cleanName := strings.TrimSpace(*request.Name)
		if cleanName == "" {
			writeError(w, 400, "INVALID_LIBRARY", "name is required")
			return
		}
		item.Name = cleanName
	}
	if request.Enabled != nil {
		item.Enabled = *request.Enabled
	}
	if (request.OneShot != nil && *request.OneShot != item.OneShot) ||
		(request.OneshotsDirectory != nil && strings.TrimSpace(*request.OneshotsDirectory) != item.OneshotsDirectory) {
		var active int
		if err := s.store.DB().QueryRowContext(r.Context(), `
SELECT count(*) FROM scan_runs WHERE library_id=? AND status IN ('queued','running')`,
			item.ID).Scan(&active); err != nil {
			s.writeDBError(w, r, err)
			return
		}
		if active > 0 {
			writeError(w, 409, "SCAN_ACTIVE",
				"Cancel the active or queued scan before changing One-Shots mode.")
			return
		}
		if request.OneShot != nil {
			item.OneShot = *request.OneShot
		}
		if request.OneshotsDirectory != nil {
			item.OneshotsDirectory = strings.TrimSpace(*request.OneshotsDirectory)
		}
	}
	if err := s.store.UpsertLibrary(r.Context(), item); err != nil {
		s.writeDBError(w, r, err)
		return
	}
	if wasEnabled && !item.Enabled {
		s.cancelLibraryJobs(r.Context(), item.ID)
	}
	item, _ = s.store.Library(r.Context(), item.ID)
	writeJSON(w, 200, item)
}

func (s *Server) cancelLibraryJobs(ctx context.Context, libraryID string) {
	if s.metadata != nil {
		s.metadata.CancelLibrary(libraryID)
	}
	if s.scanner != nil {
		s.scanner.CancelLibrary(ctx, libraryID)
	}
	if s.covers != nil {
		s.covers.CancelLibrary(ctx, libraryID)
	}
	if s.bangumi != nil {
		s.bangumi.CancelLibrary(ctx, libraryID)
	}
	if s.tasks != nil {
		s.tasks.CancelLibrary(ctx, libraryID)
	}
	if s.pipelines != nil {
		s.pipelines.CancelLibrary(ctx, libraryID)
	}
}

func (s *Server) deleteLibrary(w http.ResponseWriter, r *http.Request) {
	libraryID := chi.URLParam(r, "libraryID")
	var active int
	if err := s.store.DB().QueryRowContext(r.Context(), `
SELECT count(*) FROM scan_runs WHERE library_id=? AND status IN ('queued','running')`,
		libraryID).Scan(&active); err != nil {
		s.writeDBError(w, r, err)
		return
	}
	if active > 0 {
		writeError(w, 409, "SCAN_ACTIVE", "Cancel the active or queued scan before deleting this Library.")
		return
	}
	if err := s.store.DeleteLibrary(r.Context(), libraryID); err != nil {
		if errors.Is(err, database.ErrLibraryInUnion) {
			writeError(w, 409, "LIBRARY_IN_UNION", err.Error())
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) refreshUnionLibrary(w http.ResponseWriter, r *http.Request) {
	libraryID := chi.URLParam(r, "libraryID")
	lib, err := s.store.LibraryBasic(r.Context(), libraryID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, "NOT_FOUND", "Library not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	if lib.EffectiveProvider() != database.ProviderUnion {
		writeError(w, 400, "NOT_UNION_LIBRARY", "Target library is not a union library.")
		return
	}
	stats, err := s.store.RefreshUnionLibrary(r.Context(), libraryID)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	updated, _ := s.store.Library(r.Context(), libraryID)
	writeJSON(w, 200, map[string]any{
		"library": updated,
		"stats":   stats,
	})
}

func (s *Server) listLibrarySchedules(w http.ResponseWriter, r *http.Request) {
	schedules, err := s.store.LibrarySchedules(r.Context())
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"timezone":  time.Now().Location().String(),
		"schedules": schedules,
	})
}

func (s *Server) putLibrarySchedule(w http.ResponseWriter, r *http.Request) {
	libraryID := chi.URLParam(r, "libraryID")
	var req database.LibrarySchedule
	if !decodeJSON(w, r, &req) {
		return
	}
	req.LibraryID = libraryID
	saved, err := s.store.UpsertLibrarySchedule(r.Context(), req)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, "NOT_FOUND", "Library not found.")
			return
		}
		writeError(w, 400, "INVALID_SCHEDULE", err.Error())
		return
	}
	writeJSON(w, 200, saved)
}

func (s *Server) listPipelineRuns(w http.ResponseWriter, r *http.Request) {
	if s.pipelines == nil {
		runs, err := s.store.PipelineRuns(r.Context(), intQuery(r, "limit", 30))
		if err != nil {
			s.writeDBError(w, r, err)
			return
		}
		writeJSON(w, 200, runs)
		return
	}
	runs, err := s.pipelines.Runs(r.Context(), intQuery(r, "limit", 30))
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, 200, runs)
}

func (s *Server) startPipelineRun(w http.ResponseWriter, r *http.Request) {
	if s.pipelines == nil {
		writeError(w, 503, "PIPELINE_UNAVAILABLE", "Pipeline service is not initialized.")
		return
	}
	var req struct {
		LibraryID string `json:"libraryId"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.LibraryID) == "" {
		writeError(w, 400, "INVALID_REQUEST", "libraryId is required")
		return
	}
	run, err := s.pipelines.Start(r.Context(), req.LibraryID, "manual")
	if err != nil {
		status, code := 409, "PIPELINE_START_FAILED"
		if errors.Is(err, sql.ErrNoRows) {
			status, code = 404, "NOT_FOUND"
		} else if errors.Is(err, pipeline.ErrAlreadyQueued) {
			status, code = 409, "PIPELINE_ACTIVE"
		}
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, 202, run)
}

func (s *Server) cancelPipelineRun(w http.ResponseWriter, r *http.Request) {
	if s.pipelines == nil {
		writeError(w, 404, "NOT_FOUND", "Pipeline service unavailable.")
		return
	}
	if err := s.pipelines.Cancel(r.Context(), chi.URLParam(r, "runID")); err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, 200, settings)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if !decodeJSON(w, r, &raw) {
		return
	}
	normalizeShowNSFWSettingAlias(raw)
	request := make(map[string]string, len(raw))
	for key, value := range raw {
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			var flag bool
			if key != "show_nsfw" || json.Unmarshal(value, &flag) != nil {
				writeError(w, http.StatusBadRequest, "INVALID_SETTING", "Invalid value for "+key)
				return
			}
			text = strconv.FormatBool(flag)
		}
		request[key] = text
	}
	allowed := map[string]bool{
		"api_rate_per_second":                   true,
		"range_block_size":                      true,
		"range_cache_max_bytes":                 true,
		"page_cache_max_bytes":                  true,
		"page_prefetch_count":                   true,
		"max_page_size":                         true,
		"volume_index_prefetch_enabled":         true,
		"volume_index_prefetch_remaining_pages": true,
		"downurl_ttl_seconds":                   true,
		"rar_index_block_size":                  true,
		"rar_max_dictionary_size":               true,
		"show_nsfw":                             true,
	}
	nonNegativeIntegers := map[string]bool{
		"range_block_size": true, "range_cache_max_bytes": true,
		"page_cache_max_bytes": true, "page_prefetch_count": true,
		"max_page_size": true, "downurl_ttl_seconds": true,
		"rar_index_block_size": true, "rar_max_dictionary_size": true,
	}
	for key, value := range request {
		if !allowed[key] {
			writeError(w, 400, "INVALID_SETTING", "Unknown setting: "+key)
			return
		}
		if strings.TrimSpace(value) == "" {
			writeError(w, 400, "INVALID_SETTING", "Setting value cannot be empty: "+key)
			return
		}
		if key == "volume_index_prefetch_enabled" || key == "show_nsfw" {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				writeError(w, 400, "INVALID_SETTING", key+" must be true or false")
				return
			}
			request[key] = strconv.FormatBool(parsed)
			continue
		}
		if key == "volume_index_prefetch_remaining_pages" {
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil || number < 1 || number > 100 {
				writeError(w, 400, "INVALID_SETTING",
					"Volume index prefetch remaining pages must be between 1 and 100.")
				return
			}
			continue
		}
		if nonNegativeIntegers[key] {
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil || number < 0 {
				writeError(w, 400, "INVALID_SETTING", "Setting must be a non-negative integer: "+key)
				return
			}
		}
	}
	for key, value := range request {
		if err := s.store.SetSetting(r.Context(), key, value); err != nil {
			s.writeDBError(w, r, err)
			return
		}
	}
	if value, ok := request["range_cache_max_bytes"]; ok {
		limit, _ := strconv.ParseInt(value, 10, 64)
		if err := s.cache.EnforceLimit(r.Context(), cache.TypeRange, limit); err != nil {
			writeError(w, 500, "CACHE_LIMIT_FAILED", err.Error())
			return
		}
	}
	if value, ok := request["page_cache_max_bytes"]; ok {
		limit, _ := strconv.ParseInt(value, 10, 64)
		if err := s.cache.EnforceLimit(r.Context(), cache.TypePage, limit); err != nil {
			writeError(w, 500, "CACHE_LIMIT_FAILED", err.Error())
			return
		}
	}
	s.client.Reload(r.Context())
	s.getSettings(w, r)
}

func normalizeShowNSFWSettingAlias[T any](settings map[string]T) {
	if value, ok := settings["showNsfw"]; ok {
		if _, canonical := settings["show_nsfw"]; !canonical {
			settings["show_nsfw"] = value
		}
		delete(settings, "showNsfw")
	}
}

func (s *Server) listScans(w http.ResponseWriter, r *http.Request) {
	runs, err := s.scanner.Runs(r.Context(), intQuery(r, "limit", 50))
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, 200, runs)
}

func (s *Server) startScan(w http.ResponseWriter, r *http.Request) {
	var request struct {
		LibraryID string `json:"libraryId"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.LibraryID == "" {
		runs, err := s.scanner.StartAll(r.Context(), "manual")
		if err != nil {
			writeError(w, 409, "SCAN_START_FAILED", err.Error())
			return
		}
		writeJSON(w, 202, runs)
		return
	}
	run, err := s.scanner.StartLibrary(r.Context(), request.LibraryID, "manual")
	if err != nil {
		if errors.Is(err, scanner.ErrUnionLibraryNotScannable) {
			writeError(w, 400, "UNION_NOT_SCANNABLE", err.Error())
			return
		}
		writeError(w, 409, "SCAN_START_FAILED", err.Error())
		return
	}
	writeJSON(w, 202, run)
}

func (s *Server) cancelScan(w http.ResponseWriter, r *http.Request) {
	if err := s.scanner.Cancel(r.Context(), chi.URLParam(r, "runID")); err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listCoverJobs(w http.ResponseWriter, r *http.Request) {
	runs, err := s.covers.Runs(r.Context(), intQuery(r, "limit", 50))
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, 200, runs)
}

func (s *Server) startCoverJob(w http.ResponseWriter, r *http.Request) {
	var request struct {
		LibraryID      string `json:"libraryId"`
		All            bool   `json:"all"`
		Limit          int    `json:"limit"`
		MissingUnbound bool   `json:"missingUnbound"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	request.LibraryID = strings.TrimSpace(request.LibraryID)
	if request.LibraryID == "" {
		writeError(w, 400, "INVALID_LIBRARY", "libraryId is required")
		return
	}
	start := s.covers.Start
	if request.MissingUnbound {
		start = s.covers.StartMissingUnbound
	}
	run, err := start(r.Context(), request.LibraryID, request.All, request.Limit)
	if err != nil {
		status := http.StatusConflict
		code := "COVER_JOB_START_FAILED"
		if errors.Is(err, sql.ErrNoRows) {
			status, code = http.StatusNotFound, "NOT_FOUND"
		} else if !errors.Is(err, thumbnail.ErrBatchAlreadyQueued) &&
			strings.Contains(err.Error(), "limit") {
			status, code = http.StatusBadRequest, "INVALID_LIMIT"
		}
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) cancelCoverJob(w http.ResponseWriter, r *http.Request) {
	if err := s.covers.Cancel(r.Context(), chi.URLParam(r, "runID")); err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listMaintenanceJobs(w http.ResponseWriter, r *http.Request) {
	runs, err := s.tasks.Runs(r.Context(), maintenance.ListFilter{
		TargetType: strings.TrimSpace(r.URL.Query().Get("targetType")),
		TargetID:   strings.TrimSpace(r.URL.Query().Get("targetId")),
		Limit:      intQuery(r, "limit", 20),
	})
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, 200, runs)
}

func (s *Server) startMaintenanceJob(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Operation string `json:"operation"`
		SeriesID  string `json:"seriesId"`
		BookID    string `json:"bookId"`
		Force     bool   `json:"force"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Operation = strings.TrimSpace(request.Operation)
	request.SeriesID = strings.TrimSpace(request.SeriesID)
	request.BookID = strings.TrimSpace(request.BookID)
	var (
		run maintenance.Run
		err error
	)
	switch request.Operation {
	case maintenance.OperationBookIndex:
		if request.BookID == "" {
			writeError(w, http.StatusBadRequest, "INVALID_TARGET", "bookId is required")
			return
		}
		run, err = s.tasks.StartBookIndex(r.Context(), request.BookID, request.Force)
	case maintenance.OperationSeriesIndex:
		if request.SeriesID == "" {
			writeError(w, http.StatusBadRequest, "INVALID_TARGET", "seriesId is required")
			return
		}
		run, err = s.tasks.StartSeriesIndex(r.Context(), request.SeriesID, request.Force)
	case maintenance.OperationSeriesThumbnail:
		if request.SeriesID == "" {
			writeError(w, http.StatusBadRequest, "INVALID_TARGET", "seriesId is required")
			return
		}
		run, err = s.tasks.StartSeriesThumbnail(r.Context(), request.SeriesID, request.Force)
	default:
		writeError(w, http.StatusBadRequest, "INVALID_OPERATION", "Unknown maintenance operation.")
		return
	}
	if err != nil {
		status := http.StatusConflict
		code := "MAINTENANCE_JOB_START_FAILED"
		if errors.Is(err, sql.ErrNoRows) {
			status, code = http.StatusNotFound, "NOT_FOUND"
		} else if errors.Is(err, maintenance.ErrJobAlreadyQueued) {
			status, code = http.StatusConflict, "JOB_ALREADY_RUNNING"
		} else if errors.Is(err, maintenance.ErrInvalidOperation) {
			status, code = http.StatusBadRequest, "INVALID_OPERATION"
		}
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) cancelMaintenanceJob(w http.ResponseWriter, r *http.Request) {
	if err := s.tasks.Cancel(r.Context(), chi.URLParam(r, "runID")); err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) cacheStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.cache.Stats(r.Context())
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	var indexes int64
	_ = s.store.DB().QueryRowContext(r.Context(), `SELECT count(*) FROM zip_indexes`).Scan(&indexes)
	thumbnailStats, err := s.thumbs.Stats(r.Context())
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	bgmThumbStats, _ := s.thumbs.BangumiStats(r.Context())
	writeJSON(w, 200, map[string]any{
		"entries":          stats,
		"archiveIndexes":   indexes,
		"zipIndexes":       indexes,
		"seriesThumbnails": thumbnailStats,
		"bangumiCovers":    bgmThumbStats,
	})
}

func (s *Server) clearCache(w http.ResponseWriter, r *http.Request) {
	cacheType := chi.URLParam(r, "cacheType")
	if cacheType == "archive" || cacheType == "zip" {
		_, err := s.store.DB().ExecContext(r.Context(), `DELETE FROM zip_indexes`)
		if err != nil {
			s.writeDBError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if cacheType == "thumbnail" {
		if err := s.thumbs.Clear(r.Context()); err != nil {
			writeError(w, 500, "CACHE_CLEAR_FAILED", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if cacheType == "bangumi" {
		if err := s.thumbs.ClearBangumiCovers(r.Context()); err != nil {
			writeError(w, 500, "CACHE_CLEAR_FAILED", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if cacheType != cache.TypeRange && cacheType != cache.TypePage && cacheType != "all" {
		writeError(w, 400, "INVALID_CACHE_TYPE", "Use range, page, archive, thumbnail, bangumi, or all.")
		return
	}
	if err := s.cache.Clear(r.Context(), cacheType); err != nil {
		writeError(w, 500, "CACHE_CLEAR_FAILED", err.Error())
		return
	}
	if cacheType == "all" {
		_, _ = s.store.DB().ExecContext(r.Context(), `DELETE FROM zip_indexes`)
		_, _ = s.store.DB().ExecContext(r.Context(), `DELETE FROM downurl_cache`)
		if err := s.thumbs.Clear(r.Context()); err != nil {
			writeError(w, 500, "CACHE_CLEAR_FAILED", err.Error())
			return
		}
		_ = s.thumbs.ClearBangumiCovers(r.Context())
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Rich Detail & Bangumi Handlers ---

func (s *Server) seriesRichDetail(w http.ResponseWriter, r *http.Request) {
	series, err := s.store.SeriesByID(r.Context(), chi.URLParam(r, "seriesID"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	library, _ := s.store.LibraryBasic(r.Context(), series.LibraryID)
	meta, _ := s.store.SeriesMetadataByID(r.Context(), series)
	progress, _ := s.store.SeriesReadProgress(r.Context(), series.ID)
	books, _, err := s.store.BooksPage(r.Context(), database.BookQuery{SeriesID: series.ID, DirectSeriesAccess: true, Unpaged: true})
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	bookDTOList, err := s.bookDTOs(r, books)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	pageProgresses, _ := s.store.BookPageProgressesBySeries(r.Context(), series.ID)
	indexStats, _ := s.store.ArchiveIndexStatsBySeries(r.Context(), series.ID)
	downloadStats, _ := s.store.BookDownloadStatsBySeries(r.Context(), series.ID)

	var unionSeriesSources []database.UnionSeriesSource
	var unionBookSources map[string][]database.UnionBookSource
	if library.EffectiveProvider() == database.ProviderUnion {
		unionSeriesSources, unionBookSources, _ = s.store.UnionSeriesSourceDetails(r.Context(), series.ID)
	}

	var totalSize int64
	currentVersions := make(map[string]string, len(books))
	bookExtras := make(map[string]any, len(books))
	for _, b := range books {
		totalSize += b.Size
		ver := archive.BookVersion(b)
		currentVersions[b.ID] = ver
		extra := map[string]any{
			"inferredProgress": pageProgressInline(pageProgresses[b.ID]),
			"indexStatus":      indexStatusInline(indexStats[b.ID], ver),
			"downloadStat":     downloadStatInline(downloadStats[b.ID]),
		}
		if len(unionBookSources) > 0 {
			if srcs := unionBookSources[b.ID]; len(srcs) > 0 {
				extra["unionSources"] = srcs
			}
		}
		bookExtras[b.ID] = extra
	}
	perf, _ := s.store.SeriesPerformance(r.Context(), series.ID, currentVersions)
	if coverStats, hasCover, _ := s.thumbs.SeriesStats(r.Context(), series.ID); hasCover && coverStats.GenerationDuration > 0 {
		perf.HasCoverDuration = true
		perf.CoverDuration = coverStats.GenerationDuration
		perf.CoverCompletedAt = coverStats.UpdatedAt
	}
	var inferred map[string]any
	if latest, hasLatest, _ := s.store.LatestBookPageProgressInSeries(r.Context(), series.ID); hasLatest {
		inferred = map[string]any{
			"bookId":   latest.BookID,
			"bookName": latest.BookName,
			"last":     inferredProgressText(latest.BookName, latest.LastLoadedPage, latest.PageCount),
			"max":      inferredProgressText(latest.BookName, latest.MaxLoadedPage, latest.PageCount),
			"updated":  latest.UpdatedAt.UTC().Format(time.RFC3339),
		}
	}
	libPayload := map[string]any{
		"id":                library.ID,
		"name":              library.Name,
		"providerType":      library.ProviderType,
		"webdavAccountName": library.WebDAVAccountName,
		"oneShot":           library.OneShot,
		"oneshotsDirectory": library.OneshotsDirectory,
	}
	if len(library.UnionLayers) > 0 {
		libPayload["unionLayers"] = library.UnionLayers
	}
	var trackerSummary *bangumi.SeriesTrackerSummary
	if s.bangumi != nil && meta.BgmSubjectID != nil && *meta.BgmSubjectID > 0 {
		trackerSummary = s.bangumi.SeriesTrackerSummary(r.Context(), *meta.BgmSubjectID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"series":             seriesDTOFull(series, progress, meta),
		"rawMeta":            meta,
		"tracker":            trackerSummary,
		"originalName":       series.Name,
		"relativePath":       series.RelativePath,
		"library":            libPayload,
		"unionSeriesSources": unionSeriesSources,
		"totalSize":          formatBytes(totalSize),
		"books":              bookDTOList,
		"bookExtras":         bookExtras,
		"inferred":           inferred,
		"indexSummary":       seriesIndexSummary(perf),
		"coverSummary":       seriesCoverSummary(perf),
		"downloadStat":       seriesDownloadSummary(perf),
	})
}

func (s *Server) bookRichDetail(w http.ResponseWriter, r *http.Request) {
	book, err := s.store.BookByID(r.Context(), chi.URLParam(r, "bookID"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Book not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	series, err := s.store.SeriesByID(r.Context(), book.SeriesID)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	library, _ := s.store.LibraryBasic(r.Context(), book.LibraryID)
	seriesMeta, _ := s.store.SeriesMetadataByID(r.Context(), series)
	bookMeta, _, err := s.store.BookMetadataByID(r.Context(), book.ID)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	progress, hasProgress, _ := s.store.BookReadProgress(r.Context(), book.ID)
	pageProgress, hasPageProgress, _ := s.store.BookPageProgress(r.Context(), book.ID)
	indexStats, hasIndexStats, _ := s.store.ArchiveIndexStats(r.Context(), book.ID)
	downloadStats, hasDownloadStats, _ := s.store.BookDownloadStats(r.Context(), book.ID)

	var prevBookID, nextBookID string
	if prev, err := s.store.PreviousBookInSeries(r.Context(), book); err == nil {
		prevBookID = prev.ID
	}
	if next, err := s.store.NextBookInSeries(r.Context(), book); err == nil {
		nextBookID = next.ID
	}

	currentVer := archive.BookVersion(book)
	indexText := "尚未建立当前文件版本索引"
	indexDuration := ""
	indexUpdated := ""
	if hasIndexStats && indexStats.Version == currentVer {
		indexText = strconv.Itoa(indexStats.PageCount) + " 页（已建索引）"
		if indexStats.HasDuration {
			indexDuration = formatDuration(indexStats.Duration)
		}
		indexUpdated = webTime(indexStats.CompletedAt)
	} else if hasIndexStats {
		indexText = "已有旧文件版本索引，打开或重建后自动更新"
	}

	dlSpeed := "尚无真实 Range 下载统计"
	dlBytes := ""
	dlUpdated := ""
	if hasDownloadStats && downloadStats.HasDownload {
		dlSpeed = formatSpeed(downloadStats.Bytes, downloadStats.Duration)
		dlBytes = formatBytes(downloadStats.Bytes)
		dlUpdated = webTime(downloadStats.UpdatedAt)
	}

	var inferred map[string]any
	if hasPageProgress {
		inferred = map[string]any{
			"last":    pageNumberText(pageProgress.LastLoadedPage, pageProgress.PageCount),
			"max":     pageNumberText(pageProgress.MaxLoadedPage, pageProgress.PageCount),
			"updated": webTime(pageProgress.UpdatedAt),
		}
	}

	var unionSources []database.UnionBookSource
	if library.EffectiveProvider() == database.ProviderUnion {
		_, allSources, _ := s.store.UnionBookSourceBooks(r.Context(), book.ID)
		scope, hasScope := database.UserScopeFromContext(r.Context())
		for i, src := range allSources {
			if hasScope && !scope.IsAdmin && !scope.AllowsLibrary(src.SourceLibraryID) {
				continue
			}
			src.Active = i == 0
			unionSources = append(unionSources, src)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"book":              bookDTOWithProgressAndMeta(book, series, seriesMeta, progress, hasProgress, bookMeta),
		"seriesId":          series.ID,
		"seriesTitle":       seriesMeta.Title,
		"originalSeries":    series.Name,
		"libraryId":         library.ID,
		"libraryName":       library.Name,
		"providerType":      library.ProviderType,
		"webdavAccountName": library.WebDAVAccountName,
		"unionSources":      unionSources,
		"relativePath":      webBookPath(series, book),
		"archiveType":       webArchiveType(book.Name),
		"previousBookId":    prevBookID,
		"nextBookId":        nextBookID,
		"indexStatus":       indexText,
		"indexDuration":     indexDuration,
		"indexUpdated":      indexUpdated,
		"downloadSpeed":     dlSpeed,
		"downloadBytes":     dlBytes,
		"downloadAt":        dlUpdated,
		"inferred":          inferred,
	})
}

func (s *Server) bangumiStatus(w http.ResponseWriter, r *http.Request) {
	matched, unmatched, total, _ := s.store.ScrapeSummary(r.Context())
	nsfwCount, _ := s.store.NSFWSeriesCount(r.Context())
	showNSFW := s.store.ShowNSFW(r.Context())
	var arch bangumi.ArchiveStatus
	var archSched bangumi.ArchiveSchedule
	var archSource bangumi.ArchiveSourceConfig
	proxyURL := ""
	if s.bangumi != nil {
		arch = s.bangumi.ArchiveStatus()
		archSched = s.bangumi.ArchiveSchedule(r.Context())
		archSource = s.bangumi.ArchiveSource(r.Context())
		proxyURL = s.bangumi.RedactedProxyURL(r.Context())
	} else {
		archSched = bangumi.DefaultArchiveSchedule()
		archSource, _ = bangumi.ResolveArchiveSource(bangumi.ArchiveSourceZH, "")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"archive":         arch,
		"archiveSchedule": archSched,
		"archiveSource":   archSource,
		"proxyUrl":        proxyURL,
		"showNsfw":        showNSFW,
		"nsfwCount":       nsfwCount,
		"matchedCount":    matched,
		"unmatchedCount":  unmatched,
		"pendingCount":    max(int64(0), total-matched-unmatched),
		"totalSeries":     total,
	})
}

func (s *Server) putBangumiConfig(w http.ResponseWriter, r *http.Request) {
	if s.bangumi == nil {
		writeError(w, http.StatusServiceUnavailable, "BANGUMI_UNAVAILABLE", "Bangumi service is not initialized.")
		return
	}
	var req struct {
		ProxyURL         *string `json:"proxyUrl"`
		ShowNSFW         *bool   `json:"showNsfw"`
		LegacyShowNSFW   *bool   `json:"show_nsfw"`
		ArchiveSource    *string `json:"archiveSource"`
		ArchiveCustomURL *string `json:"archiveCustomUrl"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	redacted := s.bangumi.RedactedProxyURL(r.Context())
	if req.ProxyURL != nil {
		var err error
		redacted, err = s.bangumi.SetProxyURL(r.Context(), *req.ProxyURL)
		if err != nil {
			if errors.Is(err, bangumi.ErrInvalidProxyURL) {
				writeError(w, http.StatusBadRequest, "INVALID_PROXY_URL", err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, "BANGUMI_CONFIG_FAILED", err.Error())
			return
		}
	}
	if req.ArchiveSource != nil {
		customURL := ""
		if req.ArchiveCustomURL != nil {
			customURL = *req.ArchiveCustomURL
		}
		if _, err := s.bangumi.SetArchiveSource(r.Context(), *req.ArchiveSource, customURL); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ARCHIVE_SOURCE", err.Error())
			return
		}
	}
	showNSFW := s.store.ShowNSFW(r.Context())
	if req.ShowNSFW == nil && req.LegacyShowNSFW != nil {
		req.ShowNSFW = req.LegacyShowNSFW
	}
	if req.ShowNSFW != nil {
		var err error
		showNSFW, err = s.store.SetUserShowNSFW(r.Context(), database.PrimaryAdminUserID, *req.ShowNSFW)
		if err != nil {
			s.writeDBError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"proxyUrl":      redacted,
		"showNsfw":      showNSFW,
		"archiveSource": s.bangumi.ArchiveSource(r.Context()),
	})
}

func (s *Server) testBangumiProxy(w http.ResponseWriter, r *http.Request) {
	if s.bangumi == nil {
		writeError(w, http.StatusServiceUnavailable, "BANGUMI_UNAVAILABLE", "Bangumi service is not initialized.")
		return
	}
	var req struct {
		ProxyURL *string `json:"proxyUrl"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	report, err := s.bangumi.TestConnectivity(r.Context(), req.ProxyURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PROXY_URL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) bangumiSubjectCover(w http.ResponseWriter, r *http.Request) {
	subjectID, err := strconv.ParseInt(chi.URLParam(r, "subjectID"), 10, 64)
	if err != nil || subjectID <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_SUBJECT", "Invalid Bangumi subject ID.")
		return
	}
	coverURL := strings.TrimSpace(r.URL.Query().Get("u"))
	if coverURL == bangumi.CoverEndpointURL(subjectID) || !bangumi.IsAllowedBangumiCoverURL(coverURL) {
		coverURL = ""
	}
	data, err := s.thumbs.GetOrFetchBangumiCover(r.Context(), subjectID, coverURL)
	if err != nil {
		s.writeUncachedPlaceholder(w)
		return
	}
	w.Header().Set("Content-Type", data.MediaType)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data.Bytes)))
	_, _ = w.Write(data.Bytes)
}

func (s *Server) startBangumiArchiveUpdate(w http.ResponseWriter, r *http.Request) {
	if s.bangumi == nil {
		writeError(w, http.StatusServiceUnavailable, "BANGUMI_UNAVAILABLE", "Bangumi service is not initialized.")
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	if r != nil && r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	}
	st, err := s.bangumi.StartArchiveUpdateWithOptions(req.Force)
	if err != nil {
		writeError(w, http.StatusConflict, "ARCHIVE_UPDATE_ACTIVE", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) cancelBangumiArchiveUpdate(w http.ResponseWriter, _ *http.Request) {
	if s.bangumi == nil || !s.bangumi.CancelArchiveUpdate() {
		writeError(w, http.StatusNotFound, "NOT_RUNNING", "No active Bangumi archive download.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listBangumiRuns(w http.ResponseWriter, r *http.Request) {
	if s.bangumi == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	runs, err := s.bangumi.Runs(r.Context(), intQuery(r, "limit", 30))
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) startBangumiRun(w http.ResponseWriter, r *http.Request) {
	if s.bangumi == nil {
		writeError(w, http.StatusServiceUnavailable, "BANGUMI_UNAVAILABLE", "Bangumi service is not initialized.")
		return
	}
	var req struct {
		LibraryID string `json:"libraryId"`
		Mode      string `json:"mode"`
		Force     bool   `json:"force"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	run, err := s.bangumi.StartBatchWithMode(r.Context(), req.LibraryID, bangumi.NormalizeScrapeMode(req.Mode, req.Force))
	if err != nil {
		status := http.StatusConflict
		code := "BANGUMI_SCRAPE_CONFLICT"
		if errors.Is(err, sql.ErrNoRows) {
			status, code = http.StatusNotFound, "NOT_FOUND"
		}
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) cancelBangumiRun(w http.ResponseWriter, r *http.Request) {
	if s.bangumi == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Bangumi service unavailable.")
		return
	}
	if err := s.bangumi.CancelBatch(r.Context(), chi.URLParam(r, "runID")); err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) searchSeriesBangumi(w http.ResponseWriter, r *http.Request) {
	if s.bangumi == nil {
		writeError(w, http.StatusServiceUnavailable, "BANGUMI_UNAVAILABLE", "Bangumi service is not initialized.")
		return
	}
	series, err := s.store.SeriesByIDAny(r.Context(), chi.URLParam(r, "seriesID"), false)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
		return
	}
	candidates, parsed, err := s.bangumi.SearchCandidates(r.Context(), series, r.URL.Query().Get("q"))
	if err != nil {
		if errors.Is(err, bangumi.ErrNSFWBlocked) {
			writeError(w, http.StatusUnprocessableEntity, "NSFW_BLOCKED", "NSFW Bangumi 条目已被禁用。")
			return
		}
		writeError(w, http.StatusBadGateway, "BANGUMI_SEARCH_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"parsed":     parsed,
		"candidates": candidates,
	})
}

func (s *Server) bindSeriesBangumi(w http.ResponseWriter, r *http.Request) {
	if s.bangumi == nil {
		writeError(w, http.StatusServiceUnavailable, "BANGUMI_UNAVAILABLE", "Bangumi service is not initialized.")
		return
	}
	series, err := s.store.SeriesByIDAny(r.Context(), chi.URLParam(r, "seriesID"), false)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
		return
	}
	var req struct {
		SubjectID int64 `json:"subjectId"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SubjectID <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_SUBJECT", "subjectId must be positive.")
		return
	}
	meta, err := s.bangumi.BindSubjectWithOrigin(r.Context(), series, req.SubjectID, "manual")
	if err != nil {
		if errors.Is(err, bangumi.ErrNSFWBlocked) {
			writeError(w, http.StatusUnprocessableEntity, "NSFW_BLOCKED", "不支持绑定 NSFW 条目。")
			return
		}
		writeError(w, http.StatusBadGateway, "BANGUMI_BIND_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

func (s *Server) autoMatchSeriesBangumi(w http.ResponseWriter, r *http.Request) {
	if s.bangumi == nil {
		writeError(w, http.StatusServiceUnavailable, "BANGUMI_UNAVAILABLE", "Bangumi service is not initialized.")
		return
	}
	series, err := s.store.SeriesByIDAny(r.Context(), chi.URLParam(r, "seriesID"), false)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
		return
	}
	matched, meta, candidates, err := s.bangumi.AutoScrapeOneDetailed(r.Context(), series)
	if err != nil {
		writeError(w, http.StatusBadGateway, "BANGUMI_SCRAPE_FAILED", err.Error())
		return
	}
	payload := map[string]any{
		"matched":  matched,
		"metadata": meta,
	}
	if !matched && len(candidates) > 0 {
		payload["candidates"] = candidates
		payload["threshold"] = bangumi.AutoMatchMinConfidence
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) clearSeriesBangumi(w http.ResponseWriter, r *http.Request) {
	series, err := s.store.SeriesByID(r.Context(), chi.URLParam(r, "seriesID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
		return
	}
	meta, err := s.store.ClearSeriesBangumiBinding(r.Context(), series)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

// --- Bangumi Progress Tracker Handlers ---

func (s *Server) getBangumiTracker(w http.ResponseWriter, r *http.Request) {
	if s.bangumi == nil {
		writeError(w, http.StatusServiceUnavailable, "BANGUMI_UNAVAILABLE", "Bangumi service is not initialized.")
		return
	}
	st, err := s.bangumi.TrackerStatus(r.Context(), intQuery(r, "limit", 30))
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	if principal, ok := s.currentPrincipal(r); ok && !principal.IsAdmin {
		st.CustomOAuthClientID = ""
		st.HasCustomOAuthSecret = false
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) putBangumiTrackerConfig(w http.ResponseWriter, r *http.Request) {
	if s.bangumi == nil {
		writeError(w, http.StatusServiceUnavailable, "BANGUMI_UNAVAILABLE", "Bangumi service is not initialized.")
		return
	}
	var req bangumi.TrackerConfigUpdate
	if !decodeJSON(w, r, &req) {
		return
	}
	if principal, ok := s.currentPrincipal(r); ok && !principal.IsAdmin {
		if req.CustomOAuthClientID != nil || req.CustomOAuthClientSecret != nil || req.ClearCustomOAuthApp {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "仅管理员可修改全局自定义 Bangumi OAuth 应用配置。")
			return
		}
	}
	st, err := s.bangumi.UpdateTrackerConfig(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, bangumi.ErrTrackerTokenInvalid):
			writeError(w, http.StatusBadRequest, "INVALID_BGM_TOKEN", err.Error())
		case errors.Is(err, bangumi.ErrTrackerNotBound):
			writeError(w, http.StatusBadRequest, "TRACKER_NOT_BOUND", err.Error())
		default:
			writeError(w, http.StatusBadRequest, "INVALID_TRACKER_CONFIG", err.Error())
		}
		return
	}
	if principal, ok := s.currentPrincipal(r); ok && !principal.IsAdmin {
		st.CustomOAuthClientID = ""
		st.HasCustomOAuthSecret = false
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) testBangumiTracker(w http.ResponseWriter, r *http.Request) {
	if s.bangumi == nil {
		writeError(w, http.StatusServiceUnavailable, "BANGUMI_UNAVAILABLE", "Bangumi service is not initialized.")
		return
	}
	var req struct {
		AccessToken *string `json:"accessToken"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	res := s.bangumi.TestTrackerToken(r.Context(), req.AccessToken)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) retrySeriesBangumiTracker(w http.ResponseWriter, r *http.Request) {
	if s.bangumi == nil {
		writeError(w, http.StatusServiceUnavailable, "BANGUMI_UNAVAILABLE", "Bangumi service is not initialized.")
		return
	}
	series, err := s.store.SeriesByID(r.Context(), chi.URLParam(r, "seriesID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
		return
	}
	rec, err := s.bangumi.SyncSeriesTrackerNow(r.Context(), series.ID)
	if err != nil {
		status := http.StatusBadGateway
		code := "TRACKER_SYNC_FAILED"
		switch {
		case errors.Is(err, bangumi.ErrTrackerNotBound), errors.Is(err, bangumi.ErrTrackerUnboundItem):
			status, code = http.StatusBadRequest, "TRACKER_UNAVAILABLE"
		case errors.Is(err, bangumi.ErrTrackerTokenInvalid):
			status, code = http.StatusUnauthorized, "INVALID_BGM_TOKEN"
		}
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rec)
}
