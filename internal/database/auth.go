package database

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

var (
	ErrAlreadyClaimed      = errors.New("administrator account is already configured")
	ErrInvalidCredentials  = errors.New("invalid username or password")
	ErrInvalidUsername     = errors.New("username must be 1 to 64 characters without leading/trailing spaces")
	ErrWeakPassword        = errors.New("password must be between 6 and 256 characters")
	basicAuthCacheDuration = 5 * time.Minute
)

const (
	argonTime    uint32 = 2
	argonMemory  uint32 = 32 * 1024
	argonThreads uint8  = 2
	argonKeyLen  uint32 = 32

	PermanentSessionExpiresAt = "9999-12-31T23:59:59Z"

	// dummyArgonHash matches our Argon2id parameters (m=32MiB, t=2, p=2) and is verified
	// on unknown usernames to prevent username enumeration via response timing.
	dummyArgonHash = "$argon2id$v=19$m=32768,t=2,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

// argonSem caps concurrent Argon2id derivations (4 x 32MiB = ~128MiB max) so public
// login / Basic Auth floods cannot trigger OOM on low-memory hosts or containers.
var argonSem = make(chan struct{}, 4)

func (s *Store) HasAdmin(ctx context.Context) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id=? AND role='admin'`, PrimaryAdminUserID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.db.QueryRowContext(ctx, `SELECT 1 FROM auth_admin WHERE id=1`).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
	}
	return err == nil, err
}

func (s *Store) Admin(ctx context.Context) (AdminUser, bool, error) {
	var user AdminUser
	err := s.db.QueryRowContext(ctx,
		`SELECT username,created_at,updated_at FROM users WHERE id=? AND role='admin'`, PrimaryAdminUserID,
	).Scan(&user.Username, &user.CreatedAt, &user.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.db.QueryRowContext(ctx,
			`SELECT username,created_at,updated_at FROM auth_admin WHERE id=1`,
		).Scan(&user.Username, &user.CreatedAt, &user.UpdatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return AdminUser{}, false, nil
		}
	}
	if err != nil {
		return AdminUser{}, false, err
	}
	return user, true, nil
}

func (s *Store) ClaimAdmin(ctx context.Context, username, password string) (AdminUser, error) {
	username = strings.TrimSpace(username)
	if err := validateAdminUsername(username); err != nil {
		return AdminUser{}, err
	}
	if err := validateAdminPassword(password); err != nil {
		return AdminUser{}, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return AdminUser{}, err
	}
	now := nowText()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AdminUser{}, err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE id=?`, PrimaryAdminUserID).Scan(&exists); err != nil {
		return AdminUser{}, err
	}
	if exists > 0 {
		return AdminUser{}, ErrAlreadyClaimed
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO users(id,username,password_hash,role,enabled,all_libraries,allow_nsfw,show_nsfw,created_at,updated_at)
VALUES(?,?,?,'admin',1,1,1,
  coalesce((SELECT value IN ('1','t','T','TRUE','true','True') FROM settings WHERE key='show_nsfw'),0),?,?)`,
		PrimaryAdminUserID, username, hash, now, now); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") ||
			strings.Contains(strings.ToLower(err.Error()), "constraint") {
			return AdminUser{}, ErrAlreadyClaimed
		}
		return AdminUser{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO auth_admin(id,username,password_hash,created_at,updated_at)
VALUES(1,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET username=excluded.username,password_hash=excluded.password_hash,updated_at=excluded.updated_at`,
		username, hash, now, now); err != nil {
		return AdminUser{}, err
	}
	if err := tx.Commit(); err != nil {
		return AdminUser{}, err
	}
	s.clearBasicAuthCache()
	return AdminUser{Username: username, CreatedAt: now, UpdatedAt: now}, nil
}

// VerifyUserPassword authenticates any enabled user (admin or sub-user) in constant time.
func (s *Store) VerifyUserPassword(ctx context.Context, username, password string) (User, bool, error) {
	u, _, ok, err := s.VerifyUserPasswordWithHash(ctx, username, password)
	return u, ok, err
}

// VerifyUserPasswordWithHash authenticates any enabled user and returns the verified password hash
// so session creation can be conditioned on the hash not changing concurrently (e.g. admin reset).
func (s *Store) VerifyUserPasswordWithHash(ctx context.Context, username, password string) (User, string, bool, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return User{}, "", false, nil
	}
	user, storedHash, err := s.UserByUsername(ctx, username)
	if errors.Is(err, sql.ErrNoRows) {
		_, _ = verifyPasswordCtx(ctx, password, dummyArgonHash)
		return User{}, "", false, nil
	}
	if err != nil {
		return User{}, "", false, err
	}
	if !user.Enabled {
		_, _ = verifyPasswordCtx(ctx, password, dummyArgonHash)
		return User{}, "", false, nil
	}
	cacheKey := basicCacheKey(user.ID+":"+user.Username, password, storedHash)
	if s.checkBasicAuthCache(cacheKey) {
		return user, storedHash, true, nil
	}
	ok, err := verifyPasswordCtx(ctx, password, storedHash)
	if err != nil {
		return User{}, "", false, err
	}
	if !ok {
		return User{}, "", false, nil
	}
	s.putBasicAuthCache(cacheKey)
	return user, storedHash, true, nil
}

func (s *Store) VerifyAdminPassword(ctx context.Context, username, password string) (AdminUser, bool, error) {
	u, ok, err := s.VerifyUserPassword(ctx, username, password)
	if err != nil || !ok || !u.IsAdmin() {
		return AdminUser{}, false, err
	}
	return AdminUser{Username: u.Username, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt}, true, nil
}

func (s *Store) UpdateAdminCredentials(
	ctx context.Context,
	currentPassword, newUsername, newPassword, keepSessionToken string,
) (AdminUser, error) {
	u, err := s.UpdateUserCredentials(ctx, PrimaryAdminUserID, currentPassword, newUsername, newPassword, keepSessionToken)
	if err != nil {
		return AdminUser{}, err
	}
	return AdminUser{Username: u.Username, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt}, nil
}

func (s *Store) UpdateUserCredentials(
	ctx context.Context,
	userID, currentPassword, newUsername, newPassword, keepSessionToken string,
) (User, error) {
	userID = normalizeUserID(userID)
	var storedHash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=?`, userID).Scan(&storedHash); err != nil {
		return User{}, err
	}
	cur, err := s.UserByID(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if !verifyPassword(currentPassword, storedHash) {
		return User{}, ErrInvalidCredentials
	}
	targetUsername := strings.TrimSpace(newUsername)
	if targetUsername == "" {
		targetUsername = cur.Username
	}
	if !cur.IsAdmin() && !strings.EqualFold(targetUsername, cur.Username) {
		return User{}, errors.New("only administrators can change usernames")
	}
	if err := validateAdminUsername(targetUsername); err != nil {
		return User{}, err
	}
	targetHash := storedHash
	if newPassword != "" {
		if err := validateAdminPassword(newPassword); err != nil {
			return User{}, err
		}
		targetHash, err = hashPassword(newPassword)
		if err != nil {
			return User{}, err
		}
	}
	now := nowText()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()

	if !strings.EqualFold(targetUsername, cur.Username) {
		var dup int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM users WHERE username=? COLLATE NOCASE AND id<>?`, targetUsername, userID,
		).Scan(&dup); err != nil {
			return User{}, err
		}
		if dup > 0 {
			return User{}, ErrDuplicateUsername
		}
	}
	resUpd, err := tx.ExecContext(ctx, `
UPDATE users SET username=?,password_hash=?,updated_at=? WHERE id=? AND password_hash=? AND enabled=1`,
		targetUsername, targetHash, now, userID, storedHash)
	if err != nil {
		return User{}, err
	}
	if n, _ := resUpd.RowsAffected(); n == 0 {
		return User{}, ErrInvalidCredentials
	}
	if cur.IsAdmin() {
		_, _ = tx.ExecContext(ctx, `
UPDATE auth_admin SET username=?,password_hash=?,updated_at=? WHERE id=1`,
			targetUsername, targetHash, now)
	}
	if keepSessionToken != "" {
		keepHash := sha256Hex(keepSessionToken)
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM auth_sessions WHERE user_id=? AND token_hash<>?`, userID, keepHash); err != nil {
			return User{}, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE auth_sessions SET username=? WHERE user_id=? AND token_hash=?`, targetUsername, userID, keepHash); err != nil {
			return User{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id=?`, userID); err != nil {
			return User{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	s.clearBasicAuthCache()
	cur.Username = targetUsername
	cur.UpdatedAt = now
	return cur, nil
}

func (s *Store) CreateSession(
	ctx context.Context,
	username string,
	remember bool,
	userAgent string,
) (string, AuthSession, error) {
	userID := UserIDFromContext(ctx)
	if u, _, err := s.UserByUsername(ctx, username); err == nil {
		userID = u.ID
	}
	return s.CreateSessionForUser(ctx, userID, username, remember, userAgent)
}

func (s *Store) CreateSessionForUser(
	ctx context.Context,
	userID, username string,
	remember bool,
	userAgent string,
) (string, AuthSession, error) {
	return s.CreateSessionForUserIfPasswordHash(ctx, userID, username, "", remember, userAgent)
}

func (s *Store) CreateSessionForUserIfPasswordHash(
	ctx context.Context,
	userID, username, expectedPasswordHash string,
	_ bool,
	userAgent string,
) (string, AuthSession, error) {
	userID = normalizeUserID(userID)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE expires_at<=?`, nowText())
	rawToken, err := randomURLToken(32)
	if err != nil {
		return "", AuthSession{}, err
	}
	tokenHash := sha256Hex(rawToken)
	now := time.Now().UTC()
	createdText := now.Format(time.RFC3339Nano)
	expiresText := PermanentSessionExpiresAt
	ua := strings.TrimSpace(userAgent)
	if len(ua) > 256 {
		ua = ua[:256]
	}
	if expectedPasswordHash != "" {
		res, err := s.db.ExecContext(ctx, `
INSERT INTO auth_sessions(token_hash,user_id,username,remember,user_agent,created_at,last_seen_at,expires_at)
SELECT ?,?,?,1,?,?,?,?
WHERE EXISTS (SELECT 1 FROM users WHERE id=? AND enabled=1 AND password_hash=?)`,
			tokenHash, userID, username, ua, createdText, createdText, expiresText,
			userID, expectedPasswordHash,
		)
		if err != nil {
			return "", AuthSession{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return "", AuthSession{}, ErrInvalidCredentials
		}
	} else {
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO auth_sessions(token_hash,user_id,username,remember,user_agent,created_at,last_seen_at,expires_at)
VALUES(?,?,?,1,?,?,?,?)`,
			tokenHash, userID, username, ua, createdText, createdText, expiresText,
		); err != nil {
			return "", AuthSession{}, err
		}
	}
	return rawToken, AuthSession{
		ID:         tokenHash[:16],
		UserID:     userID,
		Username:   username,
		Remember:   true,
		UserAgent:  ua,
		CreatedAt:  createdText,
		LastSeenAt: createdText,
		ExpiresAt:  expiresText,
	}, nil
}

func (s *Store) ValidateSession(ctx context.Context, rawToken string) (AuthSession, bool, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return AuthSession{}, false, nil
	}
	tokenHash := sha256Hex(rawToken)
	var (
		session     AuthSession
		rememberInt int
		userEnabled sql.NullInt64
		liveName    sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
SELECT s.user_id,s.username,s.remember,s.user_agent,s.created_at,s.last_seen_at,s.expires_at,
 u.enabled,u.username
FROM auth_sessions s
LEFT JOIN users u ON u.id=s.user_id
WHERE s.token_hash=?`, tokenHash).Scan(
		&session.UserID, &session.Username, &rememberInt, &session.UserAgent,
		&session.CreatedAt, &session.LastSeenAt, &session.ExpiresAt,
		&userEnabled, &liveName,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthSession{}, false, nil
	}
	if err != nil {
		return AuthSession{}, false, err
	}
	// Reject sessions belonging to deleted or disabled users.
	if !userEnabled.Valid || userEnabled.Int64 == 0 {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE token_hash=?`, tokenHash)
		return AuthSession{}, false, nil
	}
	if liveName.Valid && liveName.String != "" {
		session.Username = liveName.String
	}
	now := time.Now().UTC()
	if session.ExpiresAt != PermanentSessionExpiresAt {
		expiresAt, parseErr := time.Parse(time.RFC3339Nano, session.ExpiresAt)
		if parseErr != nil || !expiresAt.After(now) {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE token_hash=?`, tokenHash)
			return AuthSession{}, false, nil
		}
	}
	session.ID = tokenHash[:16]
	session.Remember = true
	lastSeen, _ := time.Parse(time.RFC3339Nano, session.LastSeenAt)
	if now.Sub(lastSeen) > 5*time.Minute || session.ExpiresAt != PermanentSessionExpiresAt {
		newSeen := now.Format(time.RFC3339Nano)
		_, _ = s.db.ExecContext(ctx, `
UPDATE auth_sessions SET remember=1,last_seen_at=?,expires_at=? WHERE token_hash=?`,
			newSeen, PermanentSessionExpiresAt, tokenHash)
		session.LastSeenAt = newSeen
		session.ExpiresAt = PermanentSessionExpiresAt
		session.Refreshed = true
	}
	return session, true, nil
}

func (s *Store) DeleteSession(ctx context.Context, rawToken string) error {
	if strings.TrimSpace(rawToken) == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE token_hash=?`, sha256Hex(rawToken))
	return err
}

func (s *Store) DeleteSessionByPrefix(ctx context.Context, prefix string) error {
	return s.DeleteSessionByPrefixForUser(ctx, UserIDFromContext(ctx), prefix)
}

func (s *Store) DeleteSessionByPrefixForUser(ctx context.Context, userID, prefix string) error {
	prefix = strings.TrimSpace(prefix)
	if len(prefix) != 16 {
		return sql.ErrNoRows
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM auth_sessions WHERE user_id=? AND substr(token_hash,1,16)=?`,
		normalizeUserID(userID), prefix)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ListSessions(ctx context.Context, currentRawToken string) ([]AuthSession, error) {
	return s.ListSessionsForUser(ctx, UserIDFromContext(ctx), currentRawToken)
}

func (s *Store) ListSessionsForUser(ctx context.Context, userID, currentRawToken string) ([]AuthSession, error) {
	now := nowText()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE expires_at<=?`, now)
	currentHash := ""
	if strings.TrimSpace(currentRawToken) != "" {
		currentHash = sha256Hex(strings.TrimSpace(currentRawToken))
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT token_hash,user_id,username,remember,user_agent,created_at,last_seen_at,expires_at
FROM auth_sessions WHERE user_id=? ORDER BY last_seen_at DESC`, normalizeUserID(userID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AuthSession, 0)
	for rows.Next() {
		var (
			tokenHash   string
			item        AuthSession
			rememberInt int
		)
		if err := rows.Scan(
			&tokenHash, &item.UserID, &item.Username, &rememberInt, &item.UserAgent,
			&item.CreatedAt, &item.LastSeenAt, &item.ExpiresAt,
		); err != nil {
			return nil, err
		}
		item.ID = tokenHash[:16]
		item.Remember = rememberInt != 0
		item.Current = currentHash != "" && tokenHash == currentHash
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) CreateAPIKey(ctx context.Context, name string) (string, APIKey, error) {
	return s.CreateAPIKeyForUser(ctx, UserIDFromContext(ctx), name)
}

func (s *Store) CreateAPIKeyForUser(ctx context.Context, userID, name string) (string, APIKey, error) {
	return s.CreateAPIKeyForUserIfSession(ctx, userID, "", name)
}

func (s *Store) CreateAPIKeyForUserIfSession(ctx context.Context, userID, rawSessionToken, name string) (string, APIKey, error) {
	userID = normalizeUserID(userID)
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return "", APIKey{}, errors.New("API key name must be between 1 and 80 characters")
	}
	rawRandom, err := randomURLToken(28)
	if err != nil {
		return "", APIKey{}, err
	}
	rawKey := "fk115_" + rawRandom
	id, err := randomHexID(16)
	if err != nil {
		return "", APIKey{}, err
	}
	prefix := rawKey[:12]
	keyHash := sha256Hex(rawKey)
	now := nowText()
	if strings.TrimSpace(rawSessionToken) != "" {
		sessHash := sha256Hex(strings.TrimSpace(rawSessionToken))
		res, err := s.db.ExecContext(ctx, `
INSERT INTO auth_api_keys(id,user_id,name,key_prefix,key_hash,created_at)
SELECT ?,?,?,?,?,?
WHERE EXISTS (SELECT 1 FROM users WHERE id=? AND enabled=1)
  AND EXISTS (SELECT 1 FROM auth_sessions WHERE user_id=? AND token_hash=? AND expires_at>?)`,
			id, userID, name, prefix, keyHash, now,
			userID, userID, sessHash, now,
		)
		if err != nil {
			return "", APIKey{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return "", APIKey{}, ErrInvalidCredentials
		}
	} else {
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO auth_api_keys(id,user_id,name,key_prefix,key_hash,created_at)
VALUES(?,?,?,?,?,?)`, id, userID, name, prefix, keyHash, now); err != nil {
			return "", APIKey{}, err
		}
	}
	return rawKey, APIKey{
		ID:        id,
		UserID:    userID,
		Name:      name,
		KeyPrefix: prefix,
		CreatedAt: now,
	}, nil
}

func (s *Store) ValidateAPIKey(ctx context.Context, rawKey string) (APIKey, bool, error) {
	rawKey = strings.TrimSpace(rawKey)
	if rawKey == "" {
		return APIKey{}, false, nil
	}
	keyHash := sha256Hex(rawKey)
	var (
		item        APIKey
		userEnabled sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
SELECT k.id,k.user_id,k.name,k.key_prefix,k.created_at,k.last_used_at,u.enabled
FROM auth_api_keys k
LEFT JOIN users u ON u.id=k.user_id
WHERE k.key_hash=?`,
		keyHash).Scan(&item.ID, &item.UserID, &item.Name, &item.KeyPrefix, &item.CreatedAt, &item.LastUsedAt, &userEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, false, nil
	}
	if err != nil {
		return APIKey{}, false, err
	}
	if !userEnabled.Valid || userEnabled.Int64 == 0 {
		return APIKey{}, false, nil
	}
	now := nowText()
	if item.LastUsedAt == nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE auth_api_keys SET last_used_at=? WHERE id=?`, now, item.ID)
	} else if parsed, err := time.Parse(time.RFC3339Nano, *item.LastUsedAt); err == nil && time.Since(parsed) > 5*time.Minute {
		_, _ = s.db.ExecContext(ctx, `UPDATE auth_api_keys SET last_used_at=? WHERE id=?`, now, item.ID)
	}
	return item, true, nil
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	return s.ListAPIKeysForUser(ctx, UserIDFromContext(ctx))
}

func (s *Store) ListAPIKeysForUser(ctx context.Context, userID string) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id,user_id,name,key_prefix,created_at,last_used_at
FROM auth_api_keys WHERE user_id=? ORDER BY created_at DESC`, normalizeUserID(userID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]APIKey, 0)
	for rows.Next() {
		var item APIKey
		if err := rows.Scan(
			&item.ID, &item.UserID, &item.Name, &item.KeyPrefix, &item.CreatedAt, &item.LastUsedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	return s.DeleteAPIKeyForUser(ctx, UserIDFromContext(ctx), id)
}

func (s *Store) DeleteAPIKeyForUser(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM auth_api_keys WHERE user_id=? AND id=?`,
		normalizeUserID(userID), strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteAPIKeysByNamePrefix removes API keys owned by the current user whose name equals prefix or starts with prefix + " ".
func (s *Store) DeleteAPIKeysByNamePrefix(ctx context.Context, prefix string) (int64, error) {
	return s.DeleteAPIKeysByNamePrefixForUser(ctx, UserIDFromContext(ctx), prefix)
}

func (s *Store) DeleteAPIKeysByNamePrefixForUser(ctx context.Context, userID, prefix string) (int64, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `
DELETE FROM auth_api_keys
WHERE user_id=? AND (name=? OR name LIKE ? ESCAPE '\')`,
		normalizeUserID(userID), prefix, escapeLike(prefix)+" %",
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func validateAdminUsername(username string) error {
	if username == "" || len(username) > 64 || strings.ContainsAny(username, ":\r\n\t\x00") {
		return ErrInvalidUsername
	}
	return nil
}

func validateAdminPassword(password string) error {
	if len(password) < 6 || len(password) > 256 {
		return ErrWeakPassword
	}
	return nil
}

func hashPassword(password string) (string, error) {
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", err
	}
	derived := argon2.IDKey([]byte(password), salt[:], argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory,
		argonTime,
		argonThreads,
		base64.RawStdEncoding.EncodeToString(salt[:]),
		base64.RawStdEncoding.EncodeToString(derived),
	), nil
}

func verifyPasswordCtx(ctx context.Context, password, encoded string) (bool, error) {
	select {
	case argonSem <- struct{}{}:
		defer func() { <-argonSem }()
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return verifyPassword(password, encoded), nil
}

func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var (
		version int
		memory  uint32
		timeVal uint32
		threads uint8
	)
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeVal, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(expected) == 0 {
		return false
	}
	derived := argon2.IDKey([]byte(password), salt, timeVal, memory, threads, uint32(len(expected)))
	return subtle.ConstantTimeCompare(derived, expected) == 1
}

func basicCacheKey(username, password, storedHash string) string {
	return sha256Hex(strings.ToLower(username) + "\x00" + password + "\x00" + storedHash)
}

func (s *Store) checkBasicAuthCache(key string) bool {
	s.basicAuthMu.RLock()
	exp, ok := s.basicAuthCache[key]
	s.basicAuthMu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		s.basicAuthMu.Lock()
		delete(s.basicAuthCache, key)
		s.basicAuthMu.Unlock()
		return false
	}
	return true
}

func (s *Store) putBasicAuthCache(key string) {
	s.basicAuthMu.Lock()
	if len(s.basicAuthCache) > 256 {
		s.basicAuthCache = make(map[string]time.Time)
	}
	s.basicAuthCache[key] = time.Now().Add(basicAuthCacheDuration)
	s.basicAuthMu.Unlock()
}

func (s *Store) clearBasicAuthCache() {
	s.basicAuthMu.Lock()
	s.basicAuthCache = make(map[string]time.Time)
	s.basicAuthMu.Unlock()
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func randomURLToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func randomHexID(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
