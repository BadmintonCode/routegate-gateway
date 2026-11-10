package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- canned SQL driver (same pattern as the adminapi contract tests) --------

type tokenFakeState struct {
	mu           sync.Mutex
	rows         func(query string, args []driver.NamedValue) (driver.Rows, error)
	execErr      error
	affected     int64
	lastInsertID int64
	execs        []string
}

type tokenFakeDriver struct{}

func (tokenFakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("open unsupported") }

type tokenFakeConnector struct{ state *tokenFakeState }

func (c tokenFakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &tokenFakeConn{state: c.state}, nil
}
func (c tokenFakeConnector) Driver() driver.Driver { return tokenFakeDriver{} }

type tokenFakeConn struct{ state *tokenFakeState }

func (c *tokenFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (c *tokenFakeConn) Close() error              { return nil }
func (c *tokenFakeConn) Begin() (driver.Tx, error) { return c, nil }
func (c *tokenFakeConn) Commit() error             { return nil }
func (c *tokenFakeConn) Rollback() error           { return nil }

func (c *tokenFakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) { return c, nil }

func (c *tokenFakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if c.state.rows == nil {
		return nil, errors.New("no rows scripted")
	}
	return c.state.rows(query, args)
}

func (c *tokenFakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.execs = append(c.state.execs, query)
	if c.state.execErr != nil {
		return nil, c.state.execErr
	}
	return tokenFakeResult{affected: c.state.affected, lastInsertID: c.state.lastInsertID}, nil
}

type tokenFakeResult struct{ affected, lastInsertID int64 }

func (r tokenFakeResult) LastInsertId() (int64, error) { return r.lastInsertID, nil }
func (r tokenFakeResult) RowsAffected() (int64, error) { return r.affected, nil }

type tokenFakeRows struct {
	cols []string
	vals [][]driver.Value
	pos  int
}

func (r *tokenFakeRows) Columns() []string { return r.cols }
func (r *tokenFakeRows) Close() error      { return nil }
func (r *tokenFakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.pos])
	r.pos++
	return nil
}

func newTokenFakeDB(t *testing.T, state *tokenFakeState) *sql.DB {
	t.Helper()
	db := sql.OpenDB(tokenFakeConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// tokenJoinRow renders the admin_api_tokens JOIN users row ResolveToken reads.
func tokenJoinRow(t *testing.T, expires time.Time, revoked any) *tokenFakeRows {
	t.Helper()
	return &tokenFakeRows{
		cols: []string{"id", "user_id", "name", "prefix", "created_at", "expires_at", "last_used_at", "revoked_at", "username", "role"},
		vals: [][]driver.Value{{int64(7), int64(3), "ci-bot", "rgt_01234567", expires.Add(-time.Hour), expires, nil, revoked, "ci-owner", "user"}},
	}
}

// --- resolution --------------------------------------------------------------

func TestResolveTokenRoundTrip(t *testing.T) {
	raw, err := GenerateAPITokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "rgt_") || len(raw) != 68 {
		t.Fatalf("raw secret shape = %q (len %d), want rgt_ + 64 hex", raw, len(raw))
	}
	var gotHash string
	state := &tokenFakeState{rows: func(query string, args []driver.NamedValue) (driver.Rows, error) {
		if !strings.Contains(query, "FROM admin_api_tokens t JOIN users u") {
			return nil, errors.New("unexpected query: " + query)
		}
		if len(args) != 1 {
			return nil, errors.New("hash lookup needs exactly one arg")
		}
		gotHash, _ = args[0].Value.(string)
		return tokenJoinRow(t, time.Now().Add(time.Hour), nil), nil
	}}
	db := newTokenFakeDB(t, state)

	res, err := ResolveToken(context.Background(), db, raw)
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if gotHash != HashAPIToken(raw) {
		t.Fatalf("lookup hash = %q, want the SHA-256 of the raw secret", gotHash)
	}
	if res.Token.ID != 7 || res.Token.UserID != 3 || res.Username != "ci-owner" || res.Role != "user" {
		t.Fatalf("resolved identity wrong: %+v", res)
	}
	// The display prefix (12 chars) survives resolution; the secret never does.
	if res.Token.Prefix != "rgt_01234567" {
		t.Fatalf("prefix = %q, want the stored display prefix", res.Token.Prefix)
	}
}

func TestResolveTokenRejections(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		raw   string
		rows  func(t *testing.T) func(string, []driver.NamedValue) (driver.Rows, error)
		query bool // whether the DB must (true) or must not (false) be hit
	}{
		{"unknown hash", "rgt_" + strings.Repeat("ab", 32), func(t *testing.T) func(string, []driver.NamedValue) (driver.Rows, error) {
			return func(string, []driver.NamedValue) (driver.Rows, error) { return nil, sql.ErrNoRows }
		}, true},
		{"expired", "rgt_" + strings.Repeat("ab", 32), func(t *testing.T) func(string, []driver.NamedValue) (driver.Rows, error) {
			return func(string, []driver.NamedValue) (driver.Rows, error) {
				return tokenJoinRow(t, now.Add(-time.Minute), nil), nil // expiry boundary: one tick past is dead
			}
		}, true},
		{"expired at the exact instant", "rgt_" + strings.Repeat("ab", 32), func(t *testing.T) func(string, []driver.NamedValue) (driver.Rows, error) {
			return func(string, []driver.NamedValue) (driver.Rows, error) {
				return tokenJoinRow(t, now, nil), nil
			}
		}, true},
		{"revoked", "rgt_" + strings.Repeat("ab", 32), func(t *testing.T) func(string, []driver.NamedValue) (driver.Rows, error) {
			return func(string, []driver.NamedValue) (driver.Rows, error) {
				return tokenJoinRow(t, now.Add(time.Hour), now.Add(-time.Minute)), nil
			}
		}, true},
		{"disabled user (join hides the row)", "rgt_" + strings.Repeat("ab", 32), func(t *testing.T) func(string, []driver.NamedValue) (driver.Rows, error) {
			return func(string, []driver.NamedValue) (driver.Rows, error) { return nil, sql.ErrNoRows }
		}, true},
		{"missing rgt_ prefix", strings.Repeat("ab", 32), nil, false},
		{"truncated secret", "rgt_0123456789abcdef", nil, false},
		{"empty", "", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			queries := 0
			state := &tokenFakeState{}
			if tc.rows != nil {
				fn := tc.rows(t)
				state.rows = func(q string, a []driver.NamedValue) (driver.Rows, error) {
					queries++
					return fn(q, a)
				}
			}
			db := newTokenFakeDB(t, state)
			_, err := ResolveToken(context.Background(), db, tc.raw)
			if !errors.Is(err, ErrTokenInvalid) {
				t.Fatalf("err = %v, want ErrTokenInvalid", err)
			}
			if tc.query && queries != 1 {
				t.Fatalf("queries = %d, want 1", queries)
			}
			if !tc.query && queries != 0 {
				t.Fatalf("bad-shape token hit the database %d times, want 0", queries)
			}
		})
	}
}

func TestResolveTokenStorageErrorStaysWrapped(t *testing.T) {
	sentinel := errors.New("dial tcp refused (test)")
	state := &tokenFakeState{rows: func(string, []driver.NamedValue) (driver.Rows, error) {
		return nil, sentinel
	}}
	db := newTokenFakeDB(t, state)
	_, err := ResolveToken(context.Background(), db, "rgt_"+strings.Repeat("ab", 32))
	if err == nil || errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want a wrapped infrastructure error (not the sentinel)", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the driver error for logging", err)
	}
}

// --- create / list / revoke / touch ------------------------------------------

func TestCreateTokenInsertsDerivedColumns(t *testing.T) {
	state := &tokenFakeState{affected: 1, lastInsertID: 11}
	db := newTokenFakeDB(t, state)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().AddDate(0, 0, 30)
	id, err := CreateToken(context.Background(), tx, 3, "ci-bot", expires, strings.Repeat("h", 64), "rgt_01234567")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if id != 11 {
		t.Fatalf("id = %d, want the driver LastInsertId", id)
	}
	if len(state.execs) != 1 || !strings.Contains(state.execs[0], "INSERT INTO admin_api_tokens") {
		t.Fatalf("execs = %v, want one admin_api_tokens insert", state.execs)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestListTokensForUserKeepsRevokedAndExpired(t *testing.T) {
	now := time.Now()
	state := &tokenFakeState{rows: func(query string, args []driver.NamedValue) (driver.Rows, error) {
		if !strings.Contains(query, "FROM admin_api_tokens WHERE user_id") {
			return nil, errors.New("unexpected query: " + query)
		}
		if len(args) != 1 || args[0].Value.(int64) != 3 {
			return nil, errors.New("list must be owner-scoped")
		}
		return &tokenFakeRows{
			cols: []string{"id", "user_id", "name", "prefix", "created_at", "expires_at", "last_used_at", "revoked_at"},
			vals: [][]driver.Value{
				{int64(7), int64(3), "active-bot", "rgt_01234567", now.Add(-time.Hour), now.Add(time.Hour), nil, nil},
				{int64(8), int64(3), "revoked-bot", "rgt_89abcdef", now.Add(-48 * time.Hour), now.Add(time.Hour), now.Add(-time.Minute), now.Add(-2 * time.Hour)},
				{int64(9), int64(3), "expired-bot", "rgt_deadbeef", now.Add(-48 * time.Hour), now.Add(-24 * time.Hour), nil, nil},
			},
		}, nil
	}}
	db := newTokenFakeDB(t, state)

	tokens, err := ListTokensForUser(context.Background(), db, 3)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("tokens = %d, want all three rows including revoked/expired", len(tokens))
	}
	if tokens[1].RevokedAt == nil || tokens[1].LastUsedAt == nil {
		t.Fatalf("revoked row lost its timestamps: %+v", tokens[1])
	}
	if tokens[2].RevokedAt != nil {
		t.Fatalf("expired row must stay only expired: %+v", tokens[2])
	}
}

func TestRevokeTokenRequiresOwnedRow(t *testing.T) {
	cases := []struct {
		name     string
		affected int64
		want     error
	}{
		{"owned row", 1, nil},
		{"foreign or missing id", 0, sql.ErrNoRows},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &tokenFakeState{affected: tc.affected}
			db := newTokenFakeDB(t, state)
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			err = RevokeToken(context.Background(), tx, 7, 3)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(state.execs) != 1 || !strings.Contains(state.execs[0], "user_id = ?") {
				t.Fatalf("execs = %v, want the owner-scoped update", state.execs)
			}
		})
	}
}

func TestTouchTokenUsageThrottledPredicate(t *testing.T) {
	state := &tokenFakeState{affected: 1}
	db := newTokenFakeDB(t, state)
	if err := TouchTokenUsage(context.Background(), db, 7); err != nil {
		t.Fatalf("touch failed: %v", err)
	}
	if len(state.execs) != 1 {
		t.Fatalf("execs = %v, want one", state.execs)
	}
	if !strings.Contains(state.execs[0], "INTERVAL 60 SECOND") ||
		!strings.Contains(state.execs[0], "last_used_at IS NULL") {
		t.Fatalf("touch SQL lost the 60s throttle predicate: %s", state.execs[0])
	}
}

// TestAdminAPITokenJSONShape pins the snake_case contract and the absence of
// any hash or secret field (the AlertChannel bug class).
func TestAdminAPITokenJSONShape(t *testing.T) {
	now := time.Now()
	b, err := json.Marshal(AdminAPIToken{
		ID: 7, UserID: 3, Name: "ci-bot", Prefix: "rgt_01234567",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "user_id", "name", "prefix", "created_at", "expires_at", "last_used_at", "revoked_at"} {
		if _, ok := m[key]; !ok {
			t.Errorf("JSON output missing %q: %s", key, b)
		}
	}
	for _, banned := range []string{"token_hash", "hash", "token", "secret"} {
		if _, ok := m[banned]; ok {
			t.Errorf("JSON output leaks %q: %s", banned, b)
		}
	}
}
