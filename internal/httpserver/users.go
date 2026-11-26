package httpserver

import (
	"database/sql"
	"errors"
	"maps"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/xJogger/fake-komga-115/internal/database"
)

// --- Admin User Management Handlers ---

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username          string   `json:"username"`
		Password          string   `json:"password"`
		AllLibraries      *bool    `json:"allLibraries"`
		AllowedLibraryIDs []string `json:"allowedLibraryIds"`
		AllowNSFW         bool     `json:"allowNsfw"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	allLibs := false
	if req.AllLibraries != nil {
		allLibs = *req.AllLibraries
	}
	created, err := s.store.CreateUser(r.Context(), database.CreateUserInput{
		Username:          req.Username,
		Password:          req.Password,
		AllLibraries:      allLibs,
		AllowedLibraryIDs: req.AllowedLibraryIDs,
		AllowNSFW:         req.AllowNSFW,
	})
	if err != nil {
		switch {
		case errors.Is(err, database.ErrDuplicateUsername):
			writeError(w, http.StatusConflict, "DUPLICATE_USERNAME", "该用户名已存在")
		case errors.Is(err, database.ErrInvalidUsername), errors.Is(err, database.ErrWeakPassword):
			writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", err.Error())
		default:
			s.writeDBError(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request) {
	userID := strings.TrimSpace(chi.URLParam(r, "userID"))
	var req struct {
		Username          *string   `json:"username"`
		Enabled           *bool     `json:"enabled"`
		AllLibraries      *bool     `json:"allLibraries"`
		AllowedLibraryIDs *[]string `json:"allowedLibraryIds"`
		AllowNSFW         *bool     `json:"allowNsfw"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	updated, err := s.store.UpdateUser(r.Context(), userID, database.UpdateUserInput{
		Username:          req.Username,
		Enabled:           req.Enabled,
		AllLibraries:      req.AllLibraries,
		AllowedLibraryIDs: req.AllowedLibraryIDs,
		AllowNSFW:         req.AllowNSFW,
	})
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeError(w, http.StatusNotFound, "NOT_FOUND", "用户不存在")
		case errors.Is(err, database.ErrCannotModifyAdmin):
			writeError(w, http.StatusBadRequest, "CANNOT_MODIFY_ADMIN", "主管理员账号不允许被停用或降权")
		case errors.Is(err, database.ErrDuplicateUsername):
			writeError(w, http.StatusConflict, "DUPLICATE_USERNAME", "该用户名已存在")
		case errors.Is(err, database.ErrInvalidUsername):
			writeError(w, http.StatusBadRequest, "INVALID_USERNAME", err.Error())
		default:
			s.writeDBError(w, r, err)
		}
		return
	}
	if !updated.Enabled {
		s.sse.DisconnectUser(updated.ID)
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) adminResetUserPassword(w http.ResponseWriter, r *http.Request) {
	userID := strings.TrimSpace(chi.URLParam(r, "userID"))
	var req struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.store.ResetUserPassword(r.Context(), userID, req.Password); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeError(w, http.StatusNotFound, "NOT_FOUND", "用户不存在")
		case errors.Is(err, database.ErrCannotModifyAdmin):
			writeError(w, http.StatusBadRequest, "CANNOT_MODIFY_ADMIN", "主管理员密码请通过「账号与 API Key」使用当前密码修改")
		case errors.Is(err, database.ErrWeakPassword):
			writeError(w, http.StatusBadRequest, "WEAK_PASSWORD", err.Error())
		default:
			s.writeDBError(w, r, err)
		}
		return
	}
	s.sse.DisconnectUser(userID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminKickUserSessions(w http.ResponseWriter, r *http.Request) {
	userID := strings.TrimSpace(chi.URLParam(r, "userID"))
	if _, err := s.store.UserByID(r.Context(), userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "用户不存在")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	n, err := s.store.KickUserSessions(r.Context(), userID)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	s.sse.DisconnectUser(userID)
	writeJSON(w, http.StatusOK, map[string]any{"kickedSessions": n})
}

func (s *Server) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	userID := strings.TrimSpace(chi.URLParam(r, "userID"))
	if err := s.store.DeleteUser(r.Context(), userID); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeError(w, http.StatusNotFound, "NOT_FOUND", "用户不存在")
		case errors.Is(err, database.ErrCannotModifyAdmin):
			writeError(w, http.StatusBadRequest, "CANNOT_MODIFY_ADMIN", "主管理员账号不允许删除")
		default:
			s.writeDBError(w, r, err)
		}
		return
	}
	s.sse.DisconnectUser(userID)
	w.WriteHeader(http.StatusNoContent)
}

// --- Authenticated Self-Service Handlers (/admin/api/me*) ---

func (s *Server) getMyAccount(w http.ResponseWriter, r *http.Request) {
	principal, _ := s.currentPrincipal(r)
	user, err := s.store.UserByID(r.Context(), principal.UserID)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	scope := user.Scope()
	libs, err := s.store.AuthorizedVisibleLibrariesBasic(database.WithUserScope(r.Context(), scope))
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	authorizedLibs := make([]map[string]any, 0, len(libs))
	visibleLibs := make([]map[string]any, 0, len(libs))
	hiddenLibIDs := make([]string, 0)
	for _, lib := range libs {
		info := map[string]any{
			"id":                lib.ID,
			"name":              lib.Name,
			"providerType":      lib.ProviderType,
			"webdavAccountName": lib.WebDAVAccountName,
		}
		_, hidden := scope.HiddenLibraries[lib.ID]
		if hidden {
			hiddenLibIDs = append(hiddenLibIDs, lib.ID)
		} else {
			visibleLibs = append(visibleLibs, info)
		}
		entry := maps.Clone(info)
		entry["hidden"] = hidden
		authorizedLibs = append(authorizedLibs, entry)
	}
	origin := s.requestOrigin(r)
	localURL := localServiceURL(s.runtime.Host, s.runtime.Port)
	lanURLs := mihonAddresses(s.runtime.Host, s.runtime.Port)
	baseURL := origin
	if baseURL == "" {
		baseURL = localURL
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":                user,
		"isAdmin":             user.IsAdmin(),
		"effectiveNsfw":       user.EffectiveShowNSFW(),
		"visibleLibraries":    visibleLibs,
		"authorizedLibraries": authorizedLibs,
		"hiddenLibraryIds":    hiddenLibIDs,
		"clients": map[string]any{
			"origin":          origin,
			"localUrl":        localURL,
			"lanUrls":         lanURLs,
			"komgaUrl":        baseURL,
			"koreaderOpdsUrl": strings.TrimRight(baseURL, "/") + "/api/opds/v1.2",
			"komgaOpdsUrl":    strings.TrimRight(baseURL, "/") + "/opds/v1.2/catalog",
		},
	})
}

func (s *Server) putMyPreferences(w http.ResponseWriter, r *http.Request) {
	principal, _ := s.currentPrincipal(r)
	var req struct {
		ShowNSFW         *bool     `json:"showNsfw"`
		HiddenLibraryIDs *[]string `json:"hiddenLibraryIds"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	prefs, err := s.store.SetUserPreferences(r.Context(), principal.UserID, req.ShowNSFW, req.HiddenLibraryIDs)
	if err != nil {
		if errors.Is(err, database.ErrNSFWNotAllowed) {
			writeError(w, http.StatusForbidden, "NSFW_NOT_ALLOWED", "管理员未向当前账号授予 NSFW 访问权限")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}
