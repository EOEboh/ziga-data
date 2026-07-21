package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrNotFound is returned by the auth lookups when no matching row exists, so
// callers can distinguish "absent" from a real query error.
var ErrNotFound = errors.New("not found")

// User is an account. PasswordHash is empty for Google-only users;
// EmailVerifiedAt is zero until the address is verified.
type User struct {
	ID              int64
	Email           string
	EmailVerifiedAt time.Time
	PasswordHash    string
	CreatedAt       time.Time
}

// Verified reports whether the account's email has been verified.
func (u *User) Verified() bool { return !u.EmailVerifiedAt.IsZero() }

// OAuthAccount links a user to a Google identity and holds their encrypted
// OAuth tokens. AccessToken/RefreshToken are ciphertext (AES-GCM, see
// internal/secretbox); this package never sees the plaintext. BrokenAt is set
// when a refresh fails (revoked access) so the app can prompt a reconnect.
type OAuthAccount struct {
	UserID          int64
	Provider        string
	GoogleSub       string
	AccessTokenEnc  []byte
	RefreshTokenEnc []byte
	TokenExpiry     time.Time
	Scopes          string
	ConnectedAt     time.Time
	BrokenAt        time.Time
}

func (a *OAuthAccount) Broken() bool { return !a.BrokenAt.IsZero() }

// Session is a server-side session. ID is the hex SHA-256 of the opaque token
// held in the user's cookie, so a database read cannot reconstruct a live
// session cookie.
type Session struct {
	ID        string
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// UserSheet is a user's single lead destination. CreatedByApp distinguishes an
// auto-created sheet from one attached via the Google Picker. BrokenAt marks a
// destination whose OAuth access was lost.
type UserSheet struct {
	UserID        int64
	SpreadsheetID string
	SheetTab      string
	CreatedByApp  bool
	ConnectedAt   time.Time
	BrokenAt      time.Time
}

func (s *UserSheet) Broken() bool { return !s.BrokenAt.IsZero() }

// AuthTokenKind is the purpose of a single-use token.
type AuthTokenKind string

const (
	TokenVerifyEmail   AuthTokenKind = "verify"
	TokenPasswordReset AuthTokenKind = "reset"
)

// createAuthTables creates the multi-tenant tables. Called from Open. Uses
// CREATE TABLE IF NOT EXISTS so it is safe to run on every boot.
func createAuthTables(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			email             TEXT NOT NULL UNIQUE,
			email_verified_at TEXT,
			password_hash     TEXT,
			created_at        TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS oauth_accounts (
			user_id           INTEGER NOT NULL,
			provider          TEXT NOT NULL,
			google_sub        TEXT NOT NULL UNIQUE,
			access_token_enc  BLOB,
			refresh_token_enc BLOB,
			token_expiry      TEXT,
			scopes            TEXT,
			connected_at      TEXT NOT NULL,
			broken_at         TEXT,
			PRIMARY KEY (user_id, provider)
		);
		CREATE TABLE IF NOT EXISTS sessions (
			id         TEXT PRIMARY KEY,
			user_id    INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
		CREATE TABLE IF NOT EXISTS user_sheets (
			user_id        INTEGER PRIMARY KEY,
			spreadsheet_id TEXT NOT NULL,
			sheet_tab      TEXT NOT NULL,
			created_by_app INTEGER NOT NULL,
			connected_at   TEXT NOT NULL,
			broken_at      TEXT
		);
		CREATE TABLE IF NOT EXISTS auth_tokens (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id    INTEGER NOT NULL,
			kind       TEXT NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			expires_at TEXT NOT NULL,
			used_at    TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_auth_tokens_hash ON auth_tokens(token_hash);
	`)
	return err
}

// --- users ---

// CreateUser inserts a new account. passwordHash may be empty (Google-only).
// The email UNIQUE constraint surfaces as an error the caller maps to 409.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (*User, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO users (email, password_hash, created_at) VALUES (?, ?, ?)`,
		email, nullIfEmpty(passwordHash), now.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Email: email, PasswordHash: passwordHash, CreatedAt: now}, nil
}

const userCols = `id, email, email_verified_at, password_hash, created_at`

func scanUser(row scanner) (*User, error) {
	var u User
	var verifiedAt, passwordHash sql.NullString
	var createdAt string
	if err := row.Scan(&u.ID, &u.Email, &verifiedAt, &passwordHash, &createdAt); err != nil {
		return nil, err
	}
	u.PasswordHash = passwordHash.String
	u.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	if verifiedAt.Valid && verifiedAt.String != "" {
		u.EmailVerifiedAt, _ = time.Parse(time.RFC3339, verifiedAt.String)
	}
	return &u, nil
}

// GetUser returns the user by id, or ErrNotFound.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// GetUserByEmail returns the user by email, or ErrNotFound.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ?`, email)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// SetPasswordHash sets (or replaces) a user's password hash. An empty hash
// clears it (Google-only account).
func (s *Store) SetPasswordHash(ctx context.Context, userID int64, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`,
		nullIfEmpty(hash), userID)
	return err
}

// MarkEmailVerified stamps email_verified_at (idempotent: keeps the first
// timestamp).
func (s *Store) MarkEmailVerified(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE users SET email_verified_at = COALESCE(email_verified_at, ?) WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), userID)
	return err
}

// --- sessions ---

// CreateSession stores a session keyed by the token's SHA-256 (id).
func (s *Store) CreateSession(ctx context.Context, id string, userID int64, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		id, userID, time.Now().UTC().Format(time.RFC3339), expiresAt.UTC().Format(time.RFC3339))
	return err
}

// GetSession returns a non-expired session by id, or ErrNotFound (also when
// expired).
func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, created_at, expires_at FROM sessions WHERE id = ?`, id)
	var sess Session
	var createdAt, expiresAt string
	if err := row.Scan(&sess.ID, &sess.UserID, &createdAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	sess.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	sess.ExpiresAt, _ = time.Parse(time.RFC3339, expiresAt)
	if time.Now().UTC().After(sess.ExpiresAt) {
		return nil, ErrNotFound
	}
	return &sess, nil
}

// DeleteSession removes a session (logout). Absent ids are a no-op.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// DeleteExpiredSessions prunes sessions past their expiry. Returns the count.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- oauth accounts ---

// UpsertOAuthAccount inserts or updates the user's Google link and encrypted
// tokens, clearing any previous broken state.
func (s *Store) UpsertOAuthAccount(ctx context.Context, a *OAuthAccount) error {
	if a.ConnectedAt.IsZero() {
		a.ConnectedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO oauth_accounts
			(user_id, provider, google_sub, access_token_enc, refresh_token_enc,
			 token_expiry, scopes, connected_at, broken_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT(user_id, provider) DO UPDATE SET
			google_sub        = excluded.google_sub,
			access_token_enc  = excluded.access_token_enc,
			refresh_token_enc = excluded.refresh_token_enc,
			token_expiry      = excluded.token_expiry,
			scopes            = excluded.scopes,
			broken_at         = NULL`,
		a.UserID, a.Provider, a.GoogleSub, a.AccessTokenEnc, a.RefreshTokenEnc,
		rfcOrEmpty(a.TokenExpiry), a.Scopes, a.ConnectedAt.UTC().Format(time.RFC3339))
	return err
}

// GetOAuthAccount returns the user's link for a provider, or ErrNotFound.
func (s *Store) GetOAuthAccount(ctx context.Context, userID int64, provider string) (*OAuthAccount, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT user_id, provider, google_sub, access_token_enc, refresh_token_enc,
		       token_expiry, scopes, connected_at, broken_at
		FROM oauth_accounts WHERE user_id = ? AND provider = ?`, userID, provider)
	return scanOAuth(row)
}

// GetOAuthAccountBySub finds a Google link by its stable subject id, or
// ErrNotFound — used at login to map a Google identity back to a user.
func (s *Store) GetOAuthAccountBySub(ctx context.Context, googleSub string) (*OAuthAccount, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT user_id, provider, google_sub, access_token_enc, refresh_token_enc,
		       token_expiry, scopes, connected_at, broken_at
		FROM oauth_accounts WHERE google_sub = ?`, googleSub)
	return scanOAuth(row)
}

func scanOAuth(row scanner) (*OAuthAccount, error) {
	var a OAuthAccount
	var expiry, connectedAt, brokenAt, scopes sql.NullString
	if err := row.Scan(&a.UserID, &a.Provider, &a.GoogleSub, &a.AccessTokenEnc, &a.RefreshTokenEnc,
		&expiry, &scopes, &connectedAt, &brokenAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.Scopes = scopes.String
	if expiry.Valid && expiry.String != "" {
		a.TokenExpiry, _ = time.Parse(time.RFC3339, expiry.String)
	}
	if connectedAt.Valid {
		a.ConnectedAt, _ = time.Parse(time.RFC3339, connectedAt.String)
	}
	if brokenAt.Valid && brokenAt.String != "" {
		a.BrokenAt, _ = time.Parse(time.RFC3339, brokenAt.String)
	}
	return &a, nil
}

// UpdateOAuthTokens persists refreshed tokens (called after a token refresh)
// and clears any broken flag.
func (s *Store) UpdateOAuthTokens(ctx context.Context, userID int64, provider string, accessEnc []byte, expiry time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE oauth_accounts SET access_token_enc = ?, token_expiry = ?, broken_at = NULL
		WHERE user_id = ? AND provider = ?`,
		accessEnc, rfcOrEmpty(expiry), userID, provider)
	return err
}

// MarkOAuthBroken flags a link whose refresh failed (revoked access) so the UI
// can prompt a reconnect instead of silently failing writes.
func (s *Store) MarkOAuthBroken(ctx context.Context, userID int64, provider string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE oauth_accounts SET broken_at = COALESCE(broken_at, ?)
		WHERE user_id = ? AND provider = ?`,
		time.Now().UTC().Format(time.RFC3339), userID, provider)
	return err
}

// DeleteOAuthAccount removes a user's provider link (disconnect Google).
func (s *Store) DeleteOAuthAccount(ctx context.Context, userID int64, provider string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM oauth_accounts WHERE user_id = ? AND provider = ?`,
		userID, provider)
	return err
}

// --- user sheets ---

// SetUserSheet sets (or replaces) the user's single lead destination, clearing
// any broken state.
func (s *Store) SetUserSheet(ctx context.Context, sh *UserSheet) error {
	if sh.ConnectedAt.IsZero() {
		sh.ConnectedAt = time.Now().UTC()
	}
	createdByApp := 0
	if sh.CreatedByApp {
		createdByApp = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_sheets (user_id, spreadsheet_id, sheet_tab, created_by_app, connected_at, broken_at)
		VALUES (?, ?, ?, ?, ?, NULL)
		ON CONFLICT(user_id) DO UPDATE SET
			spreadsheet_id = excluded.spreadsheet_id,
			sheet_tab      = excluded.sheet_tab,
			created_by_app = excluded.created_by_app,
			connected_at   = excluded.connected_at,
			broken_at      = NULL`,
		sh.UserID, sh.SpreadsheetID, sh.SheetTab, createdByApp, sh.ConnectedAt.UTC().Format(time.RFC3339))
	return err
}

// GetUserSheet returns the user's destination, or ErrNotFound.
func (s *Store) GetUserSheet(ctx context.Context, userID int64) (*UserSheet, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT user_id, spreadsheet_id, sheet_tab, created_by_app, connected_at, broken_at
		FROM user_sheets WHERE user_id = ?`, userID)
	var sh UserSheet
	var createdByApp int
	var connectedAt string
	var brokenAt sql.NullString
	if err := row.Scan(&sh.UserID, &sh.SpreadsheetID, &sh.SheetTab, &createdByApp, &connectedAt, &brokenAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	sh.CreatedByApp = createdByApp != 0
	sh.ConnectedAt, _ = time.Parse(time.RFC3339, connectedAt)
	if brokenAt.Valid && brokenAt.String != "" {
		sh.BrokenAt, _ = time.Parse(time.RFC3339, brokenAt.String)
	}
	return &sh, nil
}

// MarkSheetBroken flags the destination as unwritable (lost OAuth access).
func (s *Store) MarkSheetBroken(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE user_sheets SET broken_at = COALESCE(broken_at, ?) WHERE user_id = ?`,
		time.Now().UTC().Format(time.RFC3339), userID)
	return err
}

// --- single-use auth tokens (email verify, password reset) ---

// CreateAuthToken stores a single-use token (by its SHA-256 hash) with an
// expiry.
func (s *Store) CreateAuthToken(ctx context.Context, userID int64, kind AuthTokenKind, tokenHash string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_tokens (user_id, kind, token_hash, expires_at) VALUES (?, ?, ?, ?)`,
		userID, kind, tokenHash, expiresAt.UTC().Format(time.RFC3339))
	return err
}

// ConsumeAuthToken atomically validates and marks a token used, returning the
// owning user id. It fails (ErrNotFound) if the token is unknown, of the wrong
// kind, already used, or expired — so a token works at most once.
func (s *Store) ConsumeAuthToken(ctx context.Context, kind AuthTokenKind, tokenHash string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE auth_tokens SET used_at = ?
		WHERE token_hash = ? AND kind = ? AND used_at IS NULL AND expires_at >= ?`,
		now, tokenHash, kind, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrNotFound
	}
	var userID int64
	if err := s.db.QueryRowContext(ctx, `SELECT user_id FROM auth_tokens WHERE token_hash = ?`, tokenHash).
		Scan(&userID); err != nil {
		return 0, err
	}
	return userID, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func rfcOrEmpty(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
