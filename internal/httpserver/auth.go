package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/xJogger/fake-komga-115/internal/buildinfo"
	"github.com/xJogger/fake-komga-115/internal/database"
)

const (
	sessionCookieName      = "fk115_session"
	komgaSessionCookieName = "KOMGA-SESSION"
	// RFC 6265bis maximum single-cookie lifetime honored by Chromium/WebKit (400 days),
	// paired with sliding renewal in authMiddleware so active browsers never expire.
	cookieMaxAgeSeconds = 400 * 24 * 3600
)

type authContextKey struct{}

type authPrincipal struct {
	UserID            string
	Username          string
	Role              string
	IsAdmin           bool
	AllowNSFW         bool
	ShowNSFW          bool
	AllLibraries      bool
	AllowedLibraryIDs []string
	HiddenLibraryIDs  []string
	Method            string // "session", "apikey", "basic"
	RawSession        string
	Refreshed         bool
}

func principalFromUser(u database.User, method, rawSession string, refreshed bool) authPrincipal {
	allowed := append([]string(nil), u.AllowedLibraryIDs...)
	if allowed == nil {
		allowed = []string{}
	}
	return authPrincipal{
		UserID:            u.ID,
		Username:          u.Username,
		Role:              u.Role,
		IsAdmin:           u.IsAdmin(),
		AllowNSFW:         u.AllowNSFW,
		ShowNSFW:          u.ShowNSFW,
		AllLibraries:      u.AllLibraries || u.IsAdmin(),
		AllowedLibraryIDs: allowed,
		HiddenLibraryIDs:  u.HiddenLibraryIDs,
		Method:            method,
		RawSession:        rawSession,
		Refreshed:         refreshed,
	}
}

func (p authPrincipal) userScope() database.UserScope {
	return database.User{
		ID:                p.UserID,
		Username:          p.Username,
		Role:              p.Role,
		AllowNSFW:         p.AllowNSFW,
		ShowNSFW:          p.ShowNSFW,
		AllLibraries:      p.AllLibraries,
		AllowedLibraryIDs: p.AllowedLibraryIDs,
		HiddenLibraryIDs:  p.HiddenLibraryIDs,
	}.Scope()
}

func withPrincipalContext(ctx context.Context, p authPrincipal) context.Context {
	ctx = context.WithValue(ctx, authContextKey{}, p)
	return database.WithUserScope(ctx, p.userScope())
}

// adminCatalogContext keeps the administrator's global catalog independent of personal display preferences.
func adminCatalogContext(ctx context.Context) context.Context {
	scope, ok := database.UserScopeFromContext(ctx)
	if !ok || !scope.IsAdmin {
		return ctx
	}
	scope.HiddenLibraries = nil
	return database.WithUserScope(ctx, scope)
}

func (s *Server) currentPrincipal(r *http.Request) (authPrincipal, bool) {
	val, ok := r.Context().Value(authContextKey{}).(authPrincipal)
	return val, ok
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := s.currentPrincipal(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Authentication required.")
			return
		}
		if !principal.IsAdmin {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "Administrator privileges required.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path

		// 1. Always public health check, static UI/reader bundles, ticket/state-authenticated callbacks, and read-only Agent discovery guide.
		if p == "/health" || p == "/favicon.ico" ||
			strings.HasPrefix(p, "/ui/") ||
			strings.HasPrefix(p, "/assets/") ||
			isStaticReaderAssetPath(p) ||
			isPublicUserscriptPath(p, r.Method) ||
			isBangumiOAuthCallbackPath(p, r.Method) ||
			isPublicAgentDiscoveryPath(p, r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		claimed, err := s.store.HasAdmin(r.Context())
		if err != nil {
			s.writeDBErrorMessage(w, r, err, "Failed to verify authentication state.")
			return
		}

		// 2. Unclaimed state (first install or first boot after upgrading from pre-v2):
		if !claimed {
			switch {
			case p == "/setup" && (r.Method == http.MethodGet || r.Method == http.MethodHead),
				p == "/api/v1/claim" && (r.Method == http.MethodGet || r.Method == http.MethodPost),
				p == "/admin/api/auth/status" && r.Method == http.MethodGet,
				p == "/admin/api/auth/setup" && r.Method == http.MethodPost:
				if isUnsafeHTTPMethod(r.Method) && !checkSameOriginCSRF(r) {
					writeError(w, http.StatusForbidden, "CSRF_REJECTED", "Cross-origin request rejected.")
					return
				}
				next.ServeHTTP(w, r)
				return
			case isAPIRequestPath(p):
				writeError(w, http.StatusUnauthorized, "SETUP_REQUIRED", "Initial administrator password setup is required.")
				return
			default:
				http.Redirect(w, r, "/setup", http.StatusTemporaryRedirect)
				return
			}
		}

		// 3. Claimed state: try authenticating incoming credentials.
		principal, authed, basicRateLimited, basicRetryAfter := s.authenticateRequest(r)

		// Public auth/claim status endpoints & Agent Device Auth handshake when already claimed:
		if isPublicAgentAuthFlowPath(p, r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if p == "/api/v1/claim" && r.Method == http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		if p == "/admin/api/auth/status" && r.Method == http.MethodGet {
			if authed {
				r = r.WithContext(withPrincipalContext(r.Context(), principal))
			}
			next.ServeHTTP(w, r)
			return
		}
		if (p == "/admin/api/auth/login" || p == "/admin/api/auth/logout") && r.Method == http.MethodPost {
			if !checkSameOriginCSRF(r) {
				writeError(w, http.StatusForbidden, "CSRF_REJECTED", "Cross-origin request rejected.")
				return
			}
			if authed {
				r = r.WithContext(withPrincipalContext(r.Context(), principal))
			}
			next.ServeHTTP(w, r)
			return
		}
		if p == "/setup" {
			if authed {
				http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
			} else {
				http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
			}
			return
		}
		if p == "/login" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			if authed {
				http.Redirect(w, r, sanitizeRedirectTarget(r.URL.Query().Get("redirect")), http.StatusTemporaryRedirect)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if !authed {
			if basicRateLimited {
				writeRateLimited(w, basicRetryAfter)
				return
			}
			if isAPIRequestPath(p) {
				if !isBrowserUIXHR(r) {
					w.Header().Set("WWW-Authenticate", `Basic realm="fake-komga-115"`)
				}
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Authentication required.")
				return
			}
			target := r.URL.Path
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, "/login?redirect="+url.QueryEscape(target), http.StatusTemporaryRedirect)
			return
		}

		if (principal.Method == "session" || principal.Method == "basic") && isUnsafeHTTPMethod(r.Method) && !checkSameOriginCSRF(r) {
			writeError(w, http.StatusForbidden, "CSRF_REJECTED", "Cross-origin request rejected.")
			return
		}
		if expectedUID := strings.TrimSpace(r.Header.Get("X-FK115-User-Id")); expectedUID != "" && expectedUID != principal.UserID {
			writeError(w, http.StatusConflict, "USER_CONTEXT_MISMATCH", "Active session user changed; please reload.")
			return
		}

		if principal.Method == "session" && principal.Refreshed && principal.RawSession != "" {
			s.setSessionCookies(w, r, principal.RawSession, true)
		}
		r = r.WithContext(withPrincipalContext(r.Context(), principal))
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authenticateRequest(r *http.Request) (authPrincipal, bool, bool, time.Duration) {
	// 1. Session Cookie (fk115_session or KOMGA-SESSION) or X-Auth-Token header
	for _, token := range candidateSessionTokens(r) {
		if sess, ok, err := s.store.ValidateSession(r.Context(), token); err == nil && ok {
			if user, uErr := s.store.UserByID(r.Context(), sess.UserID); uErr == nil && user.Enabled {
				return principalFromUser(user, "session", token, sess.Refreshed), true, false, 0
			}
		}
	}
	// 2. X-API-Key header or Authorization: Bearer <api-key>
	for _, rawKey := range candidateAPIKeys(r) {
		if keyRec, ok, err := s.store.ValidateAPIKey(r.Context(), rawKey); err == nil && ok {
			if user, uErr := s.store.UserByID(r.Context(), keyRec.UserID); uErr == nil && user.Enabled {
				return principalFromUser(user, "apikey", "", false), true, false, 0
			}
		}
	}
	// 3. HTTP Basic Auth (protected by per-(IP, account) + per-IP backstop brute-force limiter)
	if username, password, ok := r.BasicAuth(); ok {
		clientIP := s.trustedProxies.clientIP(r)
		if allowed, retryAfter := s.authLimiter.AllowAccount(clientIP, username); !allowed {
			return authPrincipal{}, false, true, retryAfter
		}
		user, valid, err := s.store.VerifyUserPassword(r.Context(), username, password)
		if err == nil && valid && user.Enabled {
			s.authLimiter.RecordAccountSuccess(clientIP, username)
			return principalFromUser(user, "basic", "", false), true, false, 0
		}
		if err == nil {
			s.authLimiter.RecordAccountFailure(clientIP, username)
		}
	}
	return authPrincipal{}, false, false, 0
}

func candidateSessionTokens(r *http.Request) []string {
	var tokens []string
	if c, err := r.Cookie(sessionCookieName); err == nil && strings.TrimSpace(c.Value) != "" {
		tokens = append(tokens, strings.TrimSpace(c.Value))
	}
	if c, err := r.Cookie(komgaSessionCookieName); err == nil && strings.TrimSpace(c.Value) != "" {
		tokens = append(tokens, strings.TrimSpace(c.Value))
	}
	if hdr := strings.TrimSpace(r.Header.Get("X-Auth-Token")); hdr != "" {
		tokens = append(tokens, hdr)
	}
	return tokens
}

func candidateAPIKeys(r *http.Request) []string {
	var keys []string
	if k := strings.TrimSpace(r.Header.Get("X-API-Key")); k != "" {
		keys = append(keys, k)
	}
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		if bearer := strings.TrimSpace(auth[7:]); bearer != "" {
			keys = append(keys, bearer)
		}
	}
	return keys
}

func isAPIRequestPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/") ||
		p == "/admin/api" || strings.HasPrefix(p, "/admin/api/") ||
		p == "/opds" || strings.HasPrefix(p, "/opds/")
}

func isStaticReaderAssetPath(p string) bool {
	if !strings.HasPrefix(p, "/reader/") {
		return false
	}
	rel := strings.TrimPrefix(p, "/reader/")
	if rel == "" || strings.Contains(rel, "..") || strings.Contains(rel, "\\") {
		return false
	}
	// Entry routes like /reader/series/... or /reader/book/... MUST always require authentication.
	if strings.HasPrefix(rel, "assets/") || strings.HasPrefix(rel, "locales/") {
		return true
	}
	// Only allow single-segment known static files directly under /reader/.
	if strings.Contains(rel, "/") {
		return false
	}
	switch rel {
	case "LICENSE-Suwayomi.txt",
		"BUILD-MANIFEST.json",
		"site.webmanifest",
		"robots.txt",
		"favicon.ico",
		"favicon.svg",
		"notFound.svg":
		return true
	}
	lower := strings.ToLower(rel)
	return strings.HasSuffix(lower, ".png") ||
		strings.HasSuffix(lower, ".svg") ||
		strings.HasSuffix(lower, ".ico")
}

func isBrowserUIXHR(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("X-Requested-With"), "XMLHttpRequest") {
		return true
	}
	if r.Header.Get("X-FK115-UI") == "1" {
		return true
	}
	return false
}

func sanitizeRedirectTarget(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") ||
		strings.Contains(raw, "\\") ||
		strings.HasPrefix(raw, "/login") || strings.HasPrefix(raw, "/setup") {
		return "/"
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "/"
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return "/"
	}
	return raw
}

func (s *Server) setSessionCookies(w http.ResponseWriter, r *http.Request, token string, _ bool) {
	secure := s.isHTTPSRequest(r)
	expires := time.Now().UTC().Add(time.Duration(cookieMaxAgeSeconds) * time.Second)
	for _, name := range []string{sessionCookieName, komgaSessionCookieName} {
		cookie := &http.Cookie{
			Name:     name,
			Value:    token,
			Path:     "/",
			MaxAge:   cookieMaxAgeSeconds,
			Expires:  expires,
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
		}
		http.SetCookie(w, cookie)
	}
}

func (s *Server) clearSessionCookies(w http.ResponseWriter, r *http.Request) {
	secure := s.isHTTPSRequest(r)
	for _, name := range []string{sessionCookieName, komgaSessionCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			Expires:  time.Unix(0, 0),
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// --- Auth & Claim Handlers ---

func (s *Server) authStatusHandler(w http.ResponseWriter, r *http.Request) {
	claimed, err := s.store.HasAdmin(r.Context())
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	principal, authed := s.currentPrincipal(r)
	resp := map[string]any{
		"claimed":       claimed,
		"authenticated": authed,
		"userId":        principal.UserID,
		"username":      principal.Username,
		"role":          principal.Role,
		"isAdmin":       principal.IsAdmin,
		"allowNsfw":     principal.AllowNSFW,
		"showNsfw":      principal.AllowNSFW && principal.ShowNSFW,
		"allLibraries":  principal.AllLibraries,
		"method":        principal.Method,
	}
	if authed {
		libraries, err := s.store.AuthorizedVisibleLibrariesBasic(r.Context())
		if err != nil {
			s.writeDBError(w, r, err)
			return
		}
		scope := principal.userScope()
		hasHidden := false
		for _, lib := range libraries {
			if !scope.DisplaysLibrary(lib.ID) {
				hasHidden = true
				break
			}
		}
		resp["authorizedLibrariesCount"] = len(libraries)
		resp["hasHiddenLibraries"] = hasHidden
		resp["version"] = buildinfo.Version
		if codename, quote := buildinfo.CodenameForVersion(buildinfo.Version); codename != "" {
			resp["codename"] = codename
			resp["codenameQuote"] = quote
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) authSetupHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Remember *bool  `json:"remember"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	admin, err := s.store.ClaimAdmin(r.Context(), req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, database.ErrAlreadyClaimed):
			writeError(w, http.StatusConflict, "ALREADY_CLAIMED", err.Error())
		case errors.Is(err, database.ErrInvalidUsername), errors.Is(err, database.ErrWeakPassword):
			writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", err.Error())
		default:
			s.writeDBError(w, r, err)
		}
		return
	}
	remember := true
	if req.Remember != nil {
		remember = *req.Remember
	}
	if rawToken, _, sErr := s.store.CreateSessionForUser(r.Context(), database.PrimaryAdminUserID, admin.Username, remember, r.UserAgent()); sErr == nil {
		s.setSessionCookies(w, r, rawToken, remember)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"userId":       database.PrimaryAdminUserID,
		"username":     admin.Username,
		"role":         database.RoleAdmin,
		"isAdmin":      true,
		"allowNsfw":    true,
		"showNsfw":     false,
		"allLibraries": true,
	})
}

func authRateLimitKey(clientIP, username string) string {
	u := strings.ToLower(strings.TrimSpace(username))
	if u == "" {
		return clientIP
	}
	return clientIP + "\x00" + u
}

func (s *Server) authLoginHandler(w http.ResponseWriter, r *http.Request) {
	clientIP := s.trustedProxies.clientIP(r)
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if allowed, retryAfter := s.authLimiter.AllowAccount(clientIP, req.Username); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}
	user, verifiedHash, ok, err := s.store.VerifyUserPasswordWithHash(r.Context(), req.Username, req.Password)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	if !ok || !user.Enabled {
		s.authLimiter.RecordAccountFailure(clientIP, req.Username)
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "用户名或密码不正确")
		return
	}
	s.authLimiter.RecordAccountSuccess(clientIP, req.Username)
	rawToken, _, err := s.store.CreateSessionForUserIfPasswordHash(r.Context(), user.ID, user.Username, verifiedHash, req.Remember, r.UserAgent())
	if err != nil {
		if errors.Is(err, database.ErrInvalidCredentials) {
			writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "账号状态或密码已变更，请重试")
			return
		}
		writeError(w, http.StatusInternalServerError, "SESSION_ERROR", err.Error())
		return
	}
	s.setSessionCookies(w, r, rawToken, req.Remember)
	writeJSON(w, http.StatusOK, map[string]any{
		"userId":       user.ID,
		"username":     user.Username,
		"role":         user.Role,
		"isAdmin":      user.IsAdmin(),
		"allowNsfw":    user.AllowNSFW,
		"showNsfw":     user.EffectiveShowNSFW(),
		"allLibraries": user.AllLibraries,
	})
}

func (s *Server) authLogoutHandler(w http.ResponseWriter, r *http.Request) {
	for _, tok := range candidateSessionTokens(r) {
		_ = s.store.DeleteSession(r.Context(), tok)
	}
	s.clearSessionCookies(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateCredentialsHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		NewUsername     string `json:"newUsername"`
		NewPassword     string `json:"newPassword"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	principal, _ := s.currentPrincipal(r)
	newUsername := strings.TrimSpace(req.NewUsername)
	if !principal.IsAdmin && newUsername != "" && newUsername != principal.Username {
		writeError(w, http.StatusForbidden, "USERNAME_CHANGE_FORBIDDEN", "普通用户不允许自行修改用户名，请联系管理员处理。")
		return
	}
	if !principal.IsAdmin {
		newUsername = ""
	}
	updated, err := s.store.UpdateUserCredentials(
		r.Context(), principal.UserID, req.CurrentPassword, newUsername, req.NewPassword, principal.RawSession,
	)
	if err != nil {
		switch {
		case errors.Is(err, database.ErrInvalidCredentials):
			writeError(w, http.StatusUnauthorized, "INVALID_PASSWORD", "当前密码不正确")
		case errors.Is(err, database.ErrDuplicateUsername):
			writeError(w, http.StatusConflict, "DUPLICATE_USERNAME", "该用户名已被其他账号占用")
		case errors.Is(err, database.ErrInvalidUsername), errors.Is(err, database.ErrWeakPassword):
			writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", err.Error())
		default:
			s.writeDBError(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) listSessionsHandler(w http.ResponseWriter, r *http.Request) {
	principal, _ := s.currentPrincipal(r)
	items, err := s.store.ListSessionsForUser(r.Context(), principal.UserID, principal.RawSession)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) deleteSessionHandler(w http.ResponseWriter, r *http.Request) {
	principal, _ := s.currentPrincipal(r)
	if err := s.store.DeleteSessionByPrefixForUser(r.Context(), principal.UserID, chi.URLParam(r, "sessionID")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Session not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listAPIKeysHandler(w http.ResponseWriter, r *http.Request) {
	principal, _ := s.currentPrincipal(r)
	items, err := s.store.ListAPIKeysForUser(r.Context(), principal.UserID)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createAPIKeyHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	principal, _ := s.currentPrincipal(r)
	rawKey, info, err := s.store.CreateAPIKeyForUserIfSession(r.Context(), principal.UserID, principal.RawSession, req.Name)
	if err != nil {
		if errors.Is(err, database.ErrInvalidCredentials) {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Session is no longer valid.")
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_API_KEY", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":    rawKey,
		"record": info,
	})
}

func (s *Server) deleteAPIKeyHandler(w http.ResponseWriter, r *http.Request) {
	principal, _ := s.currentPrincipal(r)
	if err := s.store.DeleteAPIKeyForUser(r.Context(), principal.UserID, chi.URLParam(r, "keyID")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "API key not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Komga-standard claim / login / user endpoints ---

func (s *Server) komgaClaimStatus(w http.ResponseWriter, r *http.Request) {
	claimed, err := s.store.HasAdmin(r.Context())
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"isClaimed": claimed})
}

func (s *Server) komgaClaimCreate(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.Header.Get("X-Komga-Email"))
	password := r.Header.Get("X-Komga-Password")
	if username == "" && password == "" {
		var body struct {
			Username string `json:"username"`
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
		username = strings.TrimSpace(body.Username)
		if username == "" {
			username = strings.TrimSpace(body.Email)
		}
		password = body.Password
	}
	admin, err := s.store.ClaimAdmin(r.Context(), username, password)
	if err != nil {
		switch {
		case errors.Is(err, database.ErrAlreadyClaimed):
			writeError(w, http.StatusBadRequest, "ALREADY_CLAIMED", err.Error())
		case errors.Is(err, database.ErrInvalidUsername), errors.Is(err, database.ErrWeakPassword):
			writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", err.Error())
		default:
			s.writeDBError(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    "admin",
		"email": admin.Username,
		"roles": []string{"USER", "ADMIN", "FILE_DOWNLOAD", "PAGE_STREAMING"},
	})
}

func (s *Server) komgaSetCookie(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.currentPrincipal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Authentication required.")
		return
	}
	remember := r.URL.Query().Get("remember-me") == "true"
	if principal.Method == "session" && principal.RawSession != "" {
		s.setSessionCookies(w, r, principal.RawSession, remember)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	rawToken, _, err := s.store.CreateSessionForUser(r.Context(), principal.UserID, principal.Username, remember, r.UserAgent())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "SESSION_ERROR", err.Error())
		return
	}
	s.setSessionCookies(w, r, rawToken, remember)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) komgaPatchMyPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		Password        string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	principal, _ := s.currentPrincipal(r)
	if req.CurrentPassword == "" {
		if _, pass, ok := r.BasicAuth(); ok {
			req.CurrentPassword = pass
		}
	}
	if _, err := s.store.UpdateUserCredentials(r.Context(), principal.UserID, req.CurrentPassword, "", req.Password, principal.RawSession); err != nil {
		if errors.Is(err, database.ErrInvalidCredentials) {
			writeError(w, http.StatusUnauthorized, "INVALID_PASSWORD", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_PASSWORD", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
