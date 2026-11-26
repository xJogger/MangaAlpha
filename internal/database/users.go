package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
)

const (
	PrimaryAdminUserID = "admin"
	RoleAdmin          = "admin"
	RoleUser           = "user"
)

var (
	ErrDuplicateUsername = errors.New("username already exists")
	ErrCannotModifyAdmin = errors.New("primary administrator cannot be disabled or deleted")
	ErrUserDisabled      = errors.New("user account is disabled")
	ErrNSFWNotAllowed    = errors.New("NSFW access is not granted for this user")
)

type User struct {
	ID                string   `json:"id"`
	Username          string   `json:"username"`
	Role              string   `json:"role"` // "admin" | "user"
	Enabled           bool     `json:"enabled"`
	AllLibraries      bool     `json:"allLibraries"`
	AllowedLibraryIDs []string `json:"allowedLibraryIds"`
	HiddenLibraryIDs  []string `json:"hiddenLibraryIds"`
	AllowNSFW         bool     `json:"allowNsfw"`
	ShowNSFW          bool     `json:"showNsfw"`
	CreatedAt         string   `json:"createdAt"`
	UpdatedAt         string   `json:"updatedAt"`
	SessionCount      int      `json:"sessionCount,omitempty"`
	APIKeyCount       int      `json:"apiKeyCount,omitempty"`
}

func (u User) IsAdmin() bool {
	return strings.EqualFold(u.Role, RoleAdmin)
}

func (u User) EffectiveShowNSFW() bool {
	return u.AllowNSFW && u.ShowNSFW
}

func (u User) Scope() UserScope {
	libs := cleanStringSliceCaseSensitive(u.AllowedLibraryIDs)
	slices.Sort(libs)
	allowedSet := make(map[string]bool, len(libs))
	for _, id := range libs {
		allowedSet[id] = true
	}
	hiddenSet := make(map[string]struct{}, len(u.HiddenLibraryIDs))
	for _, id := range u.HiddenLibraryIDs {
		hiddenSet[id] = struct{}{}
	}
	return UserScope{
		UserID:            u.ID,
		Username:          u.Username,
		IsAdmin:           u.IsAdmin(),
		AllowNSFW:         u.AllowNSFW,
		ShowNSFW:          u.ShowNSFW,
		AllLibraries:      u.AllLibraries || u.IsAdmin(),
		AllowedLibraryIDs: libs,
		HiddenLibraries:   hiddenSet,
		allowedSet:        allowedSet,
	}
}

type UserScope struct {
	UserID            string
	Username          string
	IsAdmin           bool
	AllowNSFW         bool
	ShowNSFW          bool
	AllLibraries      bool
	AllowedLibraryIDs []string
	HiddenLibraries   map[string]struct{}
	allowedSet        map[string]bool
}

func (sc UserScope) EffectiveShowNSFW() bool {
	return sc.AllowNSFW && sc.ShowNSFW
}

func (sc UserScope) AllowsLibrary(libraryID string) bool {
	if sc.AllLibraries || sc.IsAdmin {
		return true
	}
	if sc.allowedSet != nil {
		return sc.allowedSet[libraryID]
	}
	return slices.Contains(sc.AllowedLibraryIDs, libraryID)
}

func (sc UserScope) DisplaysLibrary(libraryID string) bool {
	_, hidden := sc.HiddenLibraries[libraryID]
	return sc.AllowsLibrary(libraryID) && !hidden
}

func (sc UserScope) VisibilityKey() string {
	nsfwFlag := "0"
	if sc.EffectiveShowNSFW() {
		nsfwFlag = "1"
	}
	authorized := "all"
	if !sc.AllLibraries && !sc.IsAdmin {
		authorized = "none"
		if len(sc.AllowedLibraryIDs) > 0 {
			allowed := cleanStringSliceCaseSensitive(sc.AllowedLibraryIDs)
			slices.Sort(allowed)
			authorized = "libs:" + libraryIDsCacheHash(allowed)
		}
	}
	key := authorized + ":" + nsfwFlag
	if len(sc.HiddenLibraries) > 0 {
		key += ":hidden:" + libraryIDsCacheHash(slices.Sorted(maps.Keys(sc.HiddenLibraries)))
	}
	return key
}

func libraryIDsCacheHash(ids []string) string {
	encoded, _ := json.Marshal(ids)
	return sha256Hex(string(encoded))
}

type userScopeContextKey struct{}

func WithUserScope(ctx context.Context, scope UserScope) context.Context {
	if scope.UserID == "" {
		scope.UserID = PrimaryAdminUserID
	}
	if !scope.AllLibraries && !scope.IsAdmin && scope.allowedSet == nil && len(scope.AllowedLibraryIDs) > 0 {
		libs := cleanStringSliceCaseSensitive(scope.AllowedLibraryIDs)
		slices.Sort(libs)
		scope.AllowedLibraryIDs = libs
		scope.allowedSet = make(map[string]bool, len(libs))
		for _, id := range libs {
			scope.allowedSet[id] = true
		}
	}
	return context.WithValue(ctx, userScopeContextKey{}, scope)
}

func WithUserID(ctx context.Context, userID string) context.Context {
	if sc, ok := UserScopeFromContext(ctx); ok {
		sc.UserID = normalizeUserID(userID)
		return context.WithValue(ctx, userScopeContextKey{}, sc)
	}
	return WithUserScope(ctx, UserScope{
		UserID:       normalizeUserID(userID),
		IsAdmin:      userID == "" || userID == PrimaryAdminUserID,
		AllLibraries: true,
		AllowNSFW:    true,
	})
}

func UserScopeFromContext(ctx context.Context) (UserScope, bool) {
	if ctx == nil {
		return UserScope{}, false
	}
	sc, ok := ctx.Value(userScopeContextKey{}).(UserScope)
	return sc, ok
}

func UserIDFromContext(ctx context.Context) string {
	if sc, ok := UserScopeFromContext(ctx); ok && strings.TrimSpace(sc.UserID) != "" {
		return sc.UserID
	}
	return PrimaryAdminUserID
}

func normalizeUserID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return PrimaryAdminUserID
	}
	return id
}

func newUserID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "usr_" + hex.EncodeToString(raw[:]), nil
}

// VisibilityCacheKey returns a cache-key fragment representing the effective library + NSFW
// visibility of ctx (so users with identical visibility can safely share catalog caches).
func (s *Store) VisibilityCacheKey(ctx context.Context) string {
	if sc, ok := UserScopeFromContext(ctx); ok {
		return sc.VisibilityKey()
	}
	if s.ShowNSFW(ctx) {
		return "all:1"
	}
	return "all:0"
}

func (s *Store) loadUserAllowedLibraryIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT library_id FROM user_library_access WHERE user_id=? ORDER BY library_id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var lid string
		if err := rows.Scan(&lid); err != nil {
			return nil, err
		}
		out = append(out, lid)
	}
	return out, rows.Err()
}

func loadUserHiddenLibraryIDs(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, userID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT library_id FROM user_hidden_libraries WHERE user_id=? ORDER BY library_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	var (
		u            User
		enabledInt   int
		allLibsInt   int
		allowNSFWInt int
		showNSFWInt  int
	)
	err := s.db.QueryRowContext(ctx, `
SELECT id,username,role,enabled,all_libraries,allow_nsfw,show_nsfw,created_at,updated_at
FROM users WHERE id=?`, strings.TrimSpace(id)).Scan(
		&u.ID, &u.Username, &u.Role, &enabledInt, &allLibsInt, &allowNSFWInt, &showNSFWInt,
		&u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return User{}, err
	}
	u.Enabled = enabledInt != 0
	u.AllLibraries = allLibsInt != 0 || u.IsAdmin()
	u.AllowNSFW = allowNSFWInt != 0 || u.IsAdmin()
	u.ShowNSFW = showNSFWInt != 0 && u.AllowNSFW
	u.AllowedLibraryIDs = []string{}
	if !u.AllLibraries {
		libs, err := s.loadUserAllowedLibraryIDs(ctx, u.ID)
		if err != nil {
			return User{}, err
		}
		u.AllowedLibraryIDs = libs
	}
	u.HiddenLibraryIDs, err = loadUserHiddenLibraryIDs(ctx, s.db, u.ID)
	if err != nil {
		return User{}, err
	}
	return u, nil
}

func (s *Store) UserByUsername(ctx context.Context, username string) (User, string, error) {
	var (
		u            User
		passwordHash string
		enabledInt   int
		allLibsInt   int
		allowNSFWInt int
		showNSFWInt  int
	)
	err := s.db.QueryRowContext(ctx, `
SELECT id,username,password_hash,role,enabled,all_libraries,allow_nsfw,show_nsfw,created_at,updated_at
FROM users WHERE username=? COLLATE NOCASE`, strings.TrimSpace(username)).Scan(
		&u.ID, &u.Username, &passwordHash, &u.Role, &enabledInt, &allLibsInt, &allowNSFWInt, &showNSFWInt,
		&u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return User{}, "", err
	}
	u.Enabled = enabledInt != 0
	u.AllLibraries = allLibsInt != 0 || u.IsAdmin()
	u.AllowNSFW = allowNSFWInt != 0 || u.IsAdmin()
	u.ShowNSFW = showNSFWInt != 0 && u.AllowNSFW
	u.AllowedLibraryIDs = []string{}
	if !u.AllLibraries {
		libs, err := s.loadUserAllowedLibraryIDs(ctx, u.ID)
		if err != nil {
			return User{}, "", err
		}
		u.AllowedLibraryIDs = libs
	}
	u.HiddenLibraryIDs, err = loadUserHiddenLibraryIDs(ctx, s.db, u.ID)
	if err != nil {
		return User{}, "", err
	}
	return u, passwordHash, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT u.id,u.username,u.role,u.enabled,u.all_libraries,u.allow_nsfw,u.show_nsfw,u.created_at,u.updated_at,
 (SELECT count(*) FROM auth_sessions s WHERE s.user_id=u.id AND s.expires_at>?) AS sess_cnt,
 (SELECT count(*) FROM auth_api_keys k WHERE k.user_id=u.id) AS key_cnt
FROM users u
ORDER BY CASE WHEN u.role='admin' THEN 0 ELSE 1 END, u.created_at ASC, u.id ASC`, nowText())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]User, 0)
	byID := make(map[string]int)
	for rows.Next() {
		var (
			u            User
			enabledInt   int
			allLibsInt   int
			allowNSFWInt int
			showNSFWInt  int
		)
		if err := rows.Scan(
			&u.ID, &u.Username, &u.Role, &enabledInt, &allLibsInt, &allowNSFWInt, &showNSFWInt,
			&u.CreatedAt, &u.UpdatedAt, &u.SessionCount, &u.APIKeyCount,
		); err != nil {
			return nil, err
		}
		u.Enabled = enabledInt != 0
		u.AllLibraries = allLibsInt != 0 || u.IsAdmin()
		u.AllowNSFW = allowNSFWInt != 0 || u.IsAdmin()
		u.ShowNSFW = showNSFWInt != 0 && u.AllowNSFW
		u.AllowedLibraryIDs = []string{}
		u.HiddenLibraryIDs = []string{}
		byID[u.ID] = len(out)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	aRows, err := s.db.QueryContext(ctx, `
SELECT user_id, library_id, 0 AS hidden FROM user_library_access
UNION ALL
SELECT user_id, library_id, 1 FROM user_hidden_libraries
ORDER BY user_id, library_id`)
	if err != nil {
		return nil, err
	}
	defer aRows.Close()
	for aRows.Next() {
		var uid, lid string
		var hidden bool
		if err := aRows.Scan(&uid, &lid, &hidden); err != nil {
			return nil, err
		}
		if idx, ok := byID[uid]; ok {
			if hidden {
				out[idx].HiddenLibraryIDs = append(out[idx].HiddenLibraryIDs, lid)
			} else if !out[idx].AllLibraries {
				out[idx].AllowedLibraryIDs = append(out[idx].AllowedLibraryIDs, lid)
			}
		}
	}
	return out, aRows.Err()
}

type CreateUserInput struct {
	Username          string
	Password          string
	AllLibraries      bool
	AllowedLibraryIDs []string
	AllowNSFW         bool
}

func (s *Store) CreateUser(ctx context.Context, in CreateUserInput) (User, error) {
	username := strings.TrimSpace(in.Username)
	if err := validateAdminUsername(username); err != nil {
		return User{}, err
	}
	if err := validateAdminPassword(in.Password); err != nil {
		return User{}, err
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		return User{}, err
	}
	id, err := newUserID()
	if err != nil {
		return User{}, err
	}
	now := nowText()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()

	var dup int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE username=? COLLATE NOCASE`, username).Scan(&dup); err != nil {
		return User{}, err
	}
	if dup > 0 {
		return User{}, ErrDuplicateUsername
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO users(id,username,password_hash,role,enabled,all_libraries,allow_nsfw,show_nsfw,created_at,updated_at)
VALUES(?,?,?,'user',1,?,?,0,?,?)`,
		id, username, hash, boolInt(in.AllLibraries), boolInt(in.AllowNSFW), now, now,
	); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return User{}, ErrDuplicateUsername
		}
		return User{}, err
	}
	if !in.AllLibraries {
		if err := replaceUserLibraryAccessTx(ctx, tx, id, in.AllowedLibraryIDs); err != nil {
			return User{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	s.clearBasicAuthCache()
	return s.UserByID(ctx, id)
}

type UpdateUserInput struct {
	Username          *string
	Enabled           *bool
	AllLibraries      *bool
	AllowedLibraryIDs *[]string
	AllowNSFW         *bool
}

func (s *Store) UpdateUser(ctx context.Context, userID string, in UpdateUserInput) (User, error) {
	userID = strings.TrimSpace(userID)
	now := nowText()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()

	var cur User
	var enabledInt, allLibsInt, allowNSFWInt, showNSFWInt int
	err = tx.QueryRowContext(ctx, `
SELECT id,username,role,enabled,all_libraries,allow_nsfw,show_nsfw,created_at,updated_at
FROM users WHERE id=?`, userID).Scan(
		&cur.ID, &cur.Username, &cur.Role, &enabledInt, &allLibsInt, &allowNSFWInt, &showNSFWInt,
		&cur.CreatedAt, &cur.UpdatedAt,
	)
	if err != nil {
		return User{}, err
	}
	cur.Enabled = enabledInt != 0
	cur.AllLibraries = allLibsInt != 0
	cur.AllowNSFW = allowNSFWInt != 0
	cur.ShowNSFW = showNSFWInt != 0

	if cur.IsAdmin() {
		if in.Enabled != nil && !*in.Enabled {
			return User{}, ErrCannotModifyAdmin
		}
		if (in.AllLibraries != nil && !*in.AllLibraries) || (in.AllowNSFW != nil && !*in.AllowNSFW) {
			return User{}, ErrCannotModifyAdmin
		}
	}
	nextUsername := cur.Username
	if in.Username != nil {
		nextUsername = strings.TrimSpace(*in.Username)
		if err := validateAdminUsername(nextUsername); err != nil {
			return User{}, err
		}
	}
	nextEnabled := cur.Enabled
	if in.Enabled != nil {
		nextEnabled = *in.Enabled
	}
	nextAllLibs := cur.AllLibraries
	if in.AllLibraries != nil {
		nextAllLibs = *in.AllLibraries
	}
	nextAllowNSFW := cur.AllowNSFW
	if in.AllowNSFW != nil {
		nextAllowNSFW = *in.AllowNSFW
	}
	nextShowNSFW := cur.ShowNSFW
	if !nextAllowNSFW {
		nextShowNSFW = false
	}

	if !strings.EqualFold(nextUsername, cur.Username) {
		var dup int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM users WHERE username=? COLLATE NOCASE AND id<>?`, nextUsername, userID,
		).Scan(&dup); err != nil {
			return User{}, err
		}
		if dup > 0 {
			return User{}, ErrDuplicateUsername
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE users SET username=?,enabled=?,all_libraries=?,allow_nsfw=?,show_nsfw=?,updated_at=? WHERE id=?`,
		nextUsername, boolInt(nextEnabled), boolInt(nextAllLibs), boolInt(nextAllowNSFW), boolInt(nextShowNSFW), now, userID,
	); err != nil {
		return User{}, err
	}
	if cur.IsAdmin() && nextUsername != cur.Username {
		_, _ = tx.ExecContext(ctx, `UPDATE auth_admin SET username=?,updated_at=? WHERE id=1`, nextUsername, now)
	}
	if nextUsername != cur.Username {
		if _, err := tx.ExecContext(ctx, `UPDATE auth_sessions SET username=? WHERE user_id=?`, nextUsername, userID); err != nil {
			return User{}, err
		}
	}
	if !nextEnabled {
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id=?`, userID); err != nil {
			return User{}, err
		}
	}
	if nextAllLibs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_library_access WHERE user_id=?`, userID); err != nil {
			return User{}, err
		}
	} else if in.AllowedLibraryIDs != nil {
		if err := replaceUserLibraryAccessTx(ctx, tx, userID, *in.AllowedLibraryIDs); err != nil {
			return User{}, err
		}
	}
	if !nextAllLibs && !cur.IsAdmin() {
		if _, err := tx.ExecContext(ctx, `
DELETE FROM user_hidden_libraries
WHERE user_id=? AND library_id NOT IN (SELECT library_id FROM user_library_access WHERE user_id=?)`, userID, userID); err != nil {
			return User{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	s.clearBasicAuthCache()
	return s.UserByID(ctx, userID)
}

func (s *Store) ResetUserPassword(ctx context.Context, userID, newPassword string) error {
	userID = strings.TrimSpace(userID)
	if err := validateAdminPassword(newPassword); err != nil {
		return err
	}
	cur, err := s.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if cur.IsAdmin() {
		return ErrCannotModifyAdmin
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	now := nowText()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=?,updated_at=? WHERE id=? AND role<>?`, hash, now, userID, RoleAdmin)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_api_keys WHERE user_id=?`, userID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.clearBasicAuthCache()
	return nil
}

func (s *Store) SetUserShowNSFW(ctx context.Context, userID string, show bool) (bool, error) {
	prefs, err := s.SetUserPreferences(ctx, userID, &show, nil)
	return prefs.ShowNSFW, err
}

func (s *Store) SetUserHiddenLibraries(ctx context.Context, userID string, hiddenIDs []string) ([]string, error) {
	prefs, err := s.SetUserPreferences(ctx, userID, nil, &hiddenIDs)
	return prefs.HiddenLibraryIDs, err
}

type UserPreferences struct {
	AllowNSFW        bool     `json:"allowNsfw"`
	ShowNSFW         bool     `json:"showNsfw"`
	HiddenLibraryIDs []string `json:"hiddenLibraryIds"`
}

// SetUserPreferences applies the supplied preferences together and returns their effective values.
func (s *Store) SetUserPreferences(ctx context.Context, userID string, show *bool, hiddenIDs *[]string) (UserPreferences, error) {
	userID = normalizeUserID(userID)
	if show == nil && hiddenIDs == nil {
		u, err := s.UserByID(ctx, userID)
		return UserPreferences{u.AllowNSFW, u.ShowNSFW, u.HiddenLibraryIDs}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UserPreferences{}, err
	}
	defer tx.Rollback()

	var prefs UserPreferences
	if err := tx.QueryRowContext(ctx, `
SELECT (allow_nsfw<>0 OR role=? COLLATE NOCASE), show_nsfw FROM users WHERE id=?`, RoleAdmin, userID).
		Scan(&prefs.AllowNSFW, &prefs.ShowNSFW); err != nil {
		return UserPreferences{}, err
	}
	prefs.ShowNSFW = prefs.AllowNSFW && prefs.ShowNSFW
	now := nowText()
	if show != nil {
		if *show && !prefs.AllowNSFW {
			return UserPreferences{}, ErrNSFWNotAllowed
		}
		if err := setUserShowNSFWTx(ctx, tx, userID, *show, now); err != nil {
			return UserPreferences{}, err
		}
		prefs.ShowNSFW = *show
	}
	if hiddenIDs != nil {
		if err := replaceUserHiddenLibrariesTx(ctx, tx, userID, *hiddenIDs, now); err != nil {
			return UserPreferences{}, err
		}
	}
	prefs.HiddenLibraryIDs, err = loadUserHiddenLibraryIDs(ctx, tx, userID)
	if err != nil {
		return UserPreferences{}, err
	}
	if err := tx.Commit(); err != nil {
		return UserPreferences{}, err
	}
	return prefs, nil
}

func setUserShowNSFWTx(ctx context.Context, tx *sql.Tx, userID string, show bool, now string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE users SET show_nsfw=?,updated_at=? WHERE id=?`, boolInt(show), now, userID); err != nil {
		return err
	}
	if userID == PrimaryAdminUserID {
		return upsertSetting(ctx, tx, "show_nsfw", strconv.FormatBool(show), now)
	}
	return nil
}

func replaceUserHiddenLibrariesTx(ctx context.Context, tx *sql.Tx, userID string, hiddenIDs []string, now string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_hidden_libraries WHERE user_id=?`, userID); err != nil {
		return err
	}
	ids, _ := json.Marshal(cleanStringSliceCaseSensitive(hiddenIDs))
	if _, err := tx.ExecContext(ctx, `
INSERT INTO user_hidden_libraries(user_id, library_id, created_at)
SELECT u.id, l.id, ?
FROM users u
CROSS JOIN json_each(?) ids
CROSS JOIN libraries l ON l.id=ids.value
WHERE u.id=? AND (u.role=? COLLATE NOCASE OR u.all_libraries<>0 OR EXISTS (
  SELECT 1 FROM user_library_access a WHERE a.user_id=u.id AND a.library_id=l.id
))`, now, string(ids), userID, RoleAdmin); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE shelf_revision SET catalog=catalog+1 WHERE id=1`)
	return err
}

func (s *Store) KickUserSessions(ctx context.Context, userID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id=?`, strings.TrimSpace(userID))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) DeleteUser(ctx context.Context, userID string) error {
	userID = strings.TrimSpace(userID)
	cur, err := s.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if cur.IsAdmin() || userID == PrimaryAdminUserID {
		return ErrCannotModifyAdmin
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`DELETE FROM auth_sessions WHERE user_id=?`,
		`DELETE FROM auth_api_keys WHERE user_id=?`,
		`DELETE FROM book_read_progress WHERE user_id=?`,
		`DELETE FROM book_page_progress WHERE user_id=?`,
		`DELETE FROM book_read_progress_tombstones WHERE user_id=?`,
		`DELETE FROM collections WHERE user_id=?`,
		`DELETE FROM readlists WHERE user_id=?`,
		`DELETE FROM client_settings WHERE user_id=?`,
		`DELETE FROM bangumi_tracker_records WHERE user_id=?`,
		`DELETE FROM user_bangumi_trackers WHERE user_id=?`,
		`DELETE FROM user_library_access WHERE user_id=?`,
		`DELETE FROM users WHERE id=?`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt, userID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.clearBasicAuthCache()
	return nil
}

func replaceUserLibraryAccessTx(ctx context.Context, tx *sql.Tx, userID string, libraryIDs []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_library_access WHERE user_id=?`, userID); err != nil {
		return err
	}
	for _, lid := range cleanStringSliceCaseSensitive(libraryIDs) {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM libraries WHERE id=?`, lid).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO user_library_access(user_id,library_id) VALUES(?,?)`, userID, lid); err != nil {
			return err
		}
	}
	return nil
}

// --- Per-User Bangumi Tracker Config ---

type UserBangumiTracker struct {
	UserID           string
	Enabled          bool
	AuthMode         string // "" | "pat" | "oauth"
	AccessToken      string
	RefreshToken     string
	ExpiresAt        string
	LastRefreshAt    string
	LastRefreshError string
	OAuthRedirectURI string
	BgmUserID        string
	BgmUsername      string
	BgmNickname      string
	NSFWMode         string // "skip" | "private"
	PrivateDefault   bool
	UpdatedAt        string
}

func (s *Store) UserBangumiTrackerConfig(ctx context.Context, userID string) (UserBangumiTracker, error) {
	userID = normalizeUserID(userID)
	var (
		cfg        UserBangumiTracker
		enabledInt int
		privInt    int
	)
	cfg.UserID = userID
	cfg.NSFWMode = "skip"
	err := s.db.QueryRowContext(ctx, `
SELECT enabled,auth_mode,access_token,refresh_token,expires_at,last_refresh_at,last_refresh_error,
 oauth_redirect_uri,bgm_user_id,bgm_username,bgm_nickname,nsfw_mode,private_default,updated_at
FROM user_bangumi_trackers WHERE user_id=?`, userID).Scan(
		&enabledInt, &cfg.AuthMode, &cfg.AccessToken, &cfg.RefreshToken, &cfg.ExpiresAt,
		&cfg.LastRefreshAt, &cfg.LastRefreshError, &cfg.OAuthRedirectURI,
		&cfg.BgmUserID, &cfg.BgmUsername, &cfg.BgmNickname, &cfg.NSFWMode, &privInt, &cfg.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	cfg.Enabled = enabledInt != 0
	cfg.PrivateDefault = privInt != 0
	if strings.TrimSpace(cfg.NSFWMode) == "" {
		cfg.NSFWMode = "skip"
	}
	return cfg, nil
}

func (s *Store) SaveUserBangumiTrackerConfig(ctx context.Context, cfg UserBangumiTracker) error {
	cfg.UserID = normalizeUserID(cfg.UserID)
	if strings.TrimSpace(cfg.NSFWMode) == "" {
		cfg.NSFWMode = "skip"
	}
	var enabledInt int
	err := s.db.QueryRowContext(ctx, `SELECT enabled FROM users WHERE id=?`, cfg.UserID).Scan(&enabledInt)
	if errors.Is(err, sql.ErrNoRows) {
		var userCount int
		_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&userCount)
		if userCount > 0 || cfg.UserID != PrimaryAdminUserID {
			return sql.ErrNoRows
		}
	} else if err != nil {
		return err
	} else if enabledInt == 0 {
		return sql.ErrNoRows
	}
	now := nowText()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO user_bangumi_trackers(
 user_id,enabled,auth_mode,access_token,refresh_token,expires_at,last_refresh_at,last_refresh_error,
 oauth_redirect_uri,bgm_user_id,bgm_username,bgm_nickname,nsfw_mode,private_default,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(user_id) DO UPDATE SET
 enabled=excluded.enabled,
 auth_mode=excluded.auth_mode,
 access_token=excluded.access_token,
 refresh_token=excluded.refresh_token,
 expires_at=excluded.expires_at,
 last_refresh_at=excluded.last_refresh_at,
 last_refresh_error=excluded.last_refresh_error,
 oauth_redirect_uri=excluded.oauth_redirect_uri,
 bgm_user_id=excluded.bgm_user_id,
 bgm_username=excluded.bgm_username,
 bgm_nickname=excluded.bgm_nickname,
 nsfw_mode=excluded.nsfw_mode,
 private_default=excluded.private_default,
 updated_at=excluded.updated_at`,
		cfg.UserID, boolInt(cfg.Enabled), strings.TrimSpace(cfg.AuthMode),
		strings.TrimSpace(cfg.AccessToken), strings.TrimSpace(cfg.RefreshToken),
		strings.TrimSpace(cfg.ExpiresAt), strings.TrimSpace(cfg.LastRefreshAt),
		strings.TrimSpace(cfg.LastRefreshError), strings.TrimSpace(cfg.OAuthRedirectURI),
		strings.TrimSpace(cfg.BgmUserID), strings.TrimSpace(cfg.BgmUsername),
		strings.TrimSpace(cfg.BgmNickname), strings.TrimSpace(cfg.NSFWMode),
		boolInt(cfg.PrivateDefault), now,
	)
	return err
}

// UpdateUserBangumiTrackerOAuthRefresh conditionally persists refreshed OAuth tokens only if the
// owner still exists, is enabled, and still holds the exact refresh token that was exchanged.
func (s *Store) UpdateUserBangumiTrackerOAuthRefresh(ctx context.Context, prevRefreshToken string, cfg UserBangumiTracker) (bool, error) {
	cfg.UserID = normalizeUserID(cfg.UserID)
	now := nowText()
	res, err := s.db.ExecContext(ctx, `
UPDATE user_bangumi_trackers
SET access_token=?, refresh_token=?, expires_at=?, last_refresh_at=?, last_refresh_error=?,
    bgm_user_id=?, bgm_username=?, bgm_nickname=?, updated_at=?
WHERE user_id=? AND auth_mode='oauth' AND refresh_token=?
  AND (
    EXISTS (SELECT 1 FROM users u WHERE u.id=user_bangumi_trackers.user_id AND u.enabled=1)
    OR (user_bangumi_trackers.user_id='admin' AND NOT EXISTS (SELECT 1 FROM users))
  )`,
		strings.TrimSpace(cfg.AccessToken), strings.TrimSpace(cfg.RefreshToken),
		strings.TrimSpace(cfg.ExpiresAt), strings.TrimSpace(cfg.LastRefreshAt),
		strings.TrimSpace(cfg.LastRefreshError), strings.TrimSpace(cfg.BgmUserID),
		strings.TrimSpace(cfg.BgmUsername), strings.TrimSpace(cfg.BgmNickname),
		now, cfg.UserID, strings.TrimSpace(prevRefreshToken),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) ListOAuthBangumiTrackerUserIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT t.user_id
FROM user_bangumi_trackers t
WHERE t.auth_mode='oauth' AND t.refresh_token<>''
  AND (
    EXISTS (SELECT 1 FROM users u WHERE u.id=t.user_id AND u.enabled=1)
    OR (t.user_id='admin' AND NOT EXISTS (SELECT 1 FROM users))
  )`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out = append(out, uid)
	}
	return out, rows.Err()
}
