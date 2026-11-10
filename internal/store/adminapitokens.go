package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Admin API tokens (migrations/0009): user-scoped temporary Bearer
// credentials for AI and machine clients. A token authenticates AS its
// issuing user — it carries no independent permission scope; the user's role
// and status are resolved live from the users row on every request, so role
// changes and disabling the user take effect immediately. Only the SHA-256
// hash of the raw secret is stored; the raw secret is shown once at mint time
// and never persisted, logged, or audited.

const (
	apiTokenPrefix    = "rgt_"
	apiTokenSecretLen = 64 // 32 random bytes as lowercase hex
	apiTokenRawLen    = len(apiTokenPrefix) + apiTokenSecretLen
	apiTokenPrefixLen = 12 // display prefix length: "rgt_" + 8 hex chars
)

// tokenQuerier is satisfied by *sql.DB and *sql.Tx; it carries the row-query,
// list-query, and exec shapes the token helpers need (the plain querier in
// store.go only covers QueryRowContext).
type tokenQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// AdminAPIToken is one admin_api_tokens row. TokenHash is deliberately not a
// struct field: nothing that reads tokens needs it again, and the raw secret
// never outlives the create response.
type AdminAPIToken struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"user_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// ResolvedAPIToken is the live identity a presented Bearer token acts as,
// joined from admin_api_tokens and users at request time.
type ResolvedAPIToken struct {
	Token    AdminAPIToken
	Username string
	Role     string
}

// ErrTokenInvalid is the single failure answer for API-token resolution:
// unknown token, expired, revoked, or the acting user is disabled. Callers
// must not distinguish between these — one uniform answer avoids handing out
// an existence oracle for tokens or accounts.
var ErrTokenInvalid = errors.New("admin api token invalid")

// GenerateAPITokenSecret mints one raw secret: "rgt_" + 32 random bytes as
// lowercase hex (68 characters). The caller returns it to the user exactly
// once; only its SHA-256 hash ever reaches storage.
func GenerateAPITokenSecret() (string, error) {
	b := make([]byte, apiTokenSecretLen/2)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("api token randomness: %w", err)
	}
	return apiTokenPrefix + hex.EncodeToString(b), nil
}

// HashAPIToken renders the stored form of a raw token: lowercase hex SHA-256.
func HashAPIToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// APITokenDisplayPrefix returns the display prefix stored alongside the hash:
// the first apiTokenPrefixLen characters of the raw token, enough to spot the
// token in the panel without revealing the secret.
func APITokenDisplayPrefix(raw string) string {
	if len(raw) < apiTokenPrefixLen {
		return raw
	}
	return raw[:apiTokenPrefixLen]
}

// CreateToken inserts one token row inside the caller's transaction and
// returns its id. The hash and prefix are derived by the caller from the raw
// secret (which this function never sees).
func CreateToken(ctx context.Context, tx *sql.Tx, userID int64, name string, expiresAt time.Time, hash, prefix string) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO admin_api_tokens (user_id, name, token_hash, prefix, expires_at) VALUES (?, ?, ?, ?, ?)`,
		userID, name, hash, prefix, expiresAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListTokensForUser returns every token of one user in id order, including
// revoked and expired rows so the panel can badge them. Rows carry no hash
// and no secret.
func ListTokensForUser(ctx context.Context, q tokenQuerier, userID int64) ([]AdminAPIToken, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, user_id, name, prefix, created_at, expires_at, last_used_at, revoked_at
		 FROM admin_api_tokens WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminAPIToken
	for rows.Next() {
		var t AdminAPIToken
		var lastUsed, revoked sql.NullTime
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.CreatedAt, &t.ExpiresAt, &lastUsed, &revoked); err != nil {
			continue
		}
		if lastUsed.Valid {
			lu := lastUsed.Time
			t.LastUsedAt = &lu
		}
		if revoked.Valid {
			rv := revoked.Time
			t.RevokedAt = &rv
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken sets revoked_at on exactly one owned token row: the owner check
// lives in the SQL, so a foreign or missing id affects no rows and surfaces
// as sql.ErrNoRows (the uniform not-found answer). Revoking an already
// revoked token preserves the original timestamp.
func RevokeToken(ctx context.Context, tx *sql.Tx, id, userID int64) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE admin_api_tokens SET revoked_at = COALESCE(revoked_at, NOW()) WHERE id = ? AND user_id = ?`,
		id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// ResolveToken authenticates a raw Bearer secret: hash lookup with the user
// joined live, then expiry/revocation checks. Every failure — unknown token,
// wrong shape, expired, revoked, or the acting user no longer active — is the
// one ErrTokenInvalid answer; unexpected storage failures are wrapped so the
// caller can log them without changing the client-visible behavior.
func ResolveToken(ctx context.Context, q tokenQuerier, rawToken string) (*ResolvedAPIToken, error) {
	if len(rawToken) != apiTokenRawLen || !strings.HasPrefix(rawToken, apiTokenPrefix) {
		return nil, ErrTokenInvalid
	}
	var t AdminAPIToken
	var username, role string
	var lastUsed, revoked sql.NullTime
	err := q.QueryRowContext(ctx,
		`SELECT t.id, t.user_id, t.name, t.prefix, t.created_at, t.expires_at, t.last_used_at, t.revoked_at, u.username, u.role
		 FROM admin_api_tokens t JOIN users u ON u.id = t.user_id
		 WHERE t.token_hash = ? AND u.status = 'active'`,
		HashAPIToken(rawToken)).Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.CreatedAt, &t.ExpiresAt, &lastUsed, &revoked, &username, &role)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTokenInvalid // unknown token or disabled user
		}
		return nil, fmt.Errorf("resolve api token: %w", err)
	}
	if lastUsed.Valid {
		lu := lastUsed.Time
		t.LastUsedAt = &lu
	}
	if revoked.Valid {
		return nil, ErrTokenInvalid
	}
	if !time.Now().Before(t.ExpiresAt) {
		return nil, ErrTokenInvalid
	}
	return &ResolvedAPIToken{Token: t, Username: username, Role: role}, nil
}

// TouchTokenUsage refreshes last_used_at, throttled to once per minute per
// token so request-rate-heavy machine clients do not turn every call into a
// row write. Best-effort by contract: callers log failures and move on.
func TouchTokenUsage(ctx context.Context, q tokenQuerier, id int64) error {
	_, err := q.ExecContext(ctx,
		`UPDATE admin_api_tokens SET last_used_at = NOW() WHERE id = ? AND (last_used_at IS NULL OR last_used_at < NOW() - INTERVAL 60 SECOND)`,
		id)
	return err
}
