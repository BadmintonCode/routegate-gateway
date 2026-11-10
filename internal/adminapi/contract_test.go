package adminapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/store"
	"github.com/go-sql-driver/mysql"
)

// --- fake SQL driver ---------------------------------------------------------
//
// The contract tests need the real mux + requireAuth + writeTx pipeline, so
// they run against an in-process *sql.DB backed by a canned driver: the
// session lookup returns a fixed user, config_revision returns a fixed
// revision, and execs return a configurable result or error. That keeps the
// tests hermetic while exercising the actual handler code paths.

type fakeDBState struct {
	mu           sync.Mutex
	role         string // role returned by the session lookup
	loginHash    string // password_hash returned by the login user lookup ("" = no row)
	rev          int64  // current config_revision
	execErr      error  // returned by every ExecContext when set
	rowsAffected int64  // RowsAffected for successful execs
	execLog      []execRecord

	// Admin API token resolution: when bearerRawToken is set, the token JOIN
	// query answers with this row if (and only if) the presented hash matches.
	// bearerUserStatus "" means active; bearerExpiresAt zero means +1h.
	bearerRawToken   string
	bearerUserID     int64
	bearerUsername   string
	bearerRole       string
	bearerUserStatus string
	bearerExpiresAt  time.Time
	bearerRevokedAt  sql.NullTime
}

type execRecord struct {
	query string
	args  []driver.Value
}

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("open unsupported") }

type fakeConnector struct{ state *fakeDBState }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{state: c.state}, nil
}
func (c fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeConn struct{ state *fakeDBState }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return c, nil }
func (c *fakeConn) Commit() error             { return nil }
func (c *fakeConn) Rollback() error           { return nil }

func (c *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) { return c, nil }

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "FROM admin_api_tokens t JOIN users u"):
		// ResolveToken: answer only when the presented hash matches the
		// scripted raw token and the acting user row is active.
		if c.state.bearerRawToken == "" || len(args) != 1 {
			return nil, sql.ErrNoRows
		}
		if h, _ := args[0].Value.(string); h != hashToken(c.state.bearerRawToken) {
			return nil, sql.ErrNoRows
		}
		if c.state.bearerUserStatus != "" && c.state.bearerUserStatus != "active" {
			return nil, sql.ErrNoRows
		}
		expires := c.state.bearerExpiresAt
		if expires.IsZero() {
			expires = time.Now().Add(time.Hour)
		}
		var revoked any
		if c.state.bearerRevokedAt.Valid {
			revoked = c.state.bearerRevokedAt.Time
		}
		return &fakeRows{
			cols: []string{"id", "user_id", "name", "prefix", "created_at", "expires_at", "last_used_at", "revoked_at", "username", "role"},
			vals: [][]driver.Value{{
				int64(7), c.state.bearerUserID, "ci-bot", "rgt_01234567",
				expires.Add(-2 * time.Hour), expires, nil, revoked,
				c.state.bearerUsername, c.state.bearerRole,
			}},
		}, nil
	case strings.Contains(query, "FROM admin_api_tokens WHERE user_id"):
		// ListTokensForUser: one live token and one revoked+expired token,
		// both owned by the session user (id 1).
		now := time.Now()
		return &fakeRows{
			cols: []string{"id", "user_id", "name", "prefix", "created_at", "expires_at", "last_used_at", "revoked_at"},
			vals: [][]driver.Value{
				{int64(7), int64(1), "ci-bot", "rgt_01234567", now.Add(-time.Hour), now.Add(time.Hour), nil, nil},
				{int64(8), int64(1), "old-bot", "rgt_89abcdef", now.Add(-48 * time.Hour), now.Add(-24 * time.Hour), nil, now.Add(-24 * time.Hour)},
			},
		}, nil
	case strings.Contains(query, "FROM sessions s JOIN users"):
		now := time.Now()
		return &fakeRows{
			cols: []string{"user_id", "username", "role", "must_change_password", "expires_at", "absolute_expires_at"},
			vals: [][]driver.Value{{int64(1), "tester", c.state.role, false, now.Add(time.Hour), now.Add(2 * time.Hour)}},
		}, nil
	case strings.Contains(query, "FROM users WHERE username"):
		if c.state.loginHash == "" {
			return nil, sql.ErrNoRows
		}
		return &fakeRows{
			cols: []string{"id", "password_hash", "status", "failed_login_count", "locked_until"},
			vals: [][]driver.Value{{int64(1), c.state.loginHash, "active", int64(0), nil}},
		}, nil
	case strings.Contains(query, "FROM policies p JOIN routes r"):
		return &fakeRows{
			cols: []string{"id", "route_id", "route", "caller", "method", "path_pattern", "effect", "expires_at", "enabled", "remark"},
			vals: [][]driver.Value{{int64(42), int64(1), "demo", "svc-a", "GET", "/v1/**", "allow", nil, true, ""}},
		}, nil
	case strings.Contains(query, "FROM llm_endpoint_models m"):
		now := time.Now()
		return &fakeRows{
			cols: []string{"id", "name", "enabled", "base_url", "api_key", "remark", "updated_at", "model_id", "endpoint_id", "provider_model_name", "display_name", "model_enabled", "is_default", "max_tokens", "temperature", "model_remark", "model_updated_at"},
			vals: [][]driver.Value{{int64(3), "prod", true, "https://api.example.com/v1", "sk-super-secret-9999", "", now, int64(11), int64(3), "deepseek-chat", "DeepSeek Chat", true, true, int64(4096), float64(0.2), "", now}},
		}, nil
	case strings.Contains(query, "FROM llm_endpoints"):
		// Endpoint catalog query: the model rows are loaded separately.
		now := time.Now()
		return &fakeRows{
			cols: []string{"id", "name", "enabled", "base_url", "api_key", "remark", "updated_at"},
			vals: [][]driver.Value{{int64(3), "prod", true, "https://api.example.com/v1", "sk-super-secret-9999", "", now}},
		}, nil
	case strings.Contains(query, "FROM llm_endpoint_models"):
		now := time.Now()
		return &fakeRows{
			cols: []string{"id", "endpoint_id", "provider_model_name", "display_name", "enabled", "is_default", "max_tokens", "temperature", "remark", "updated_at"},
			vals: [][]driver.Value{{int64(11), int64(3), "deepseek-chat", "DeepSeek Chat", true, true, int64(4096), float64(0.2), "", now}},
		}, nil
	case strings.Contains(query, "FROM assistant_sessions WHERE session_key"):
		// One thread owned by user 2; the session user is id 1, so
		// non-superadmin access must be a 404 (isolation).
		now := time.Now()
		return &fakeRows{
			cols: []string{"id", "session_key", "user_id", "user_name", "title", "model_id", "created_at", "updated_at"},
			vals: [][]driver.Value{{int64(9), "as-abc", int64(2), "someone-else", "t", int64(0), now, now}},
		}, nil
	case strings.Contains(query, "config_revision"):
		return &fakeRows{cols: []string{"rev"}, vals: [][]driver.Value{{c.state.rev}}}, nil
	}
	return nil, fmt.Errorf("fakeConn: unexpected query: %s", query)
}

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	c.state.execLog = append(c.state.execLog, execRecord{query: query, args: vals})
	if c.state.execErr != nil {
		return nil, c.state.execErr
	}
	return fakeExecResult{affected: c.state.rowsAffected, lastInsertID: 1}, nil
}

// fakeExecResult supports both LastInsertId (token mint) and RowsAffected
// (revokes, deletes) the way a real MySQL driver result does.
type fakeExecResult struct{ affected, lastInsertID int64 }

func (r fakeExecResult) LastInsertId() (int64, error) { return r.lastInsertID, nil }
func (r fakeExecResult) RowsAffected() (int64, error) { return r.affected, nil }

type fakeRows struct {
	cols []string
	vals [][]driver.Value
	pos  int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.pos])
	r.pos++
	return nil
}

func newFakeDB(t *testing.T, state *fakeDBState) *sql.DB {
	t.Helper()
	db := sql.OpenDB(fakeConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newContractServer wires a Server against the fake driver. The handler must
// go through s.Handler (not mux directly) so the request-ID middleware runs.
func newContractServer(t *testing.T, state *fakeDBState) *Server {
	t.Helper()
	return &Server{cfg: AdminConfig{}, db: newFakeDB(t, state), limiter: newLoginLimiter()}
}

// --- envelope assertions -----------------------------------------------------

const sessionCookie = cookieName + "=unit-test-token"

// forbiddenFragments must never appear in an admin API error body: raw driver
// numbers, driver/product names, DSN fragments, or duplicate-entry text.
var forbiddenFragments = []string{
	"Error 1062", "Duplicate entry", "MySQL", "mysql", "DSN", "@tcp(", "password_hash", "sql:",
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (top, errBody map[string]any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
		t.Fatalf("response body is not JSON: %v; body=%q", err, rec.Body.String())
	}
	raw, ok := top["error"].(map[string]any)
	if !ok {
		t.Fatalf("body has no error object: %s", rec.Body.String())
	}
	return top, raw
}

// assertEnvelopeShape verifies the unified contract on one response: exact
// top-level keys (config_revision only beside error for conflicts), exact
// error keys (request_id only when the middleware assigned one), no-store,
// and no leaked internals. viaMiddleware selects the X-Request-Id header and
// envelope request_id expectations (direct writer unit tests skip them).
func assertEnvelopeShape(t *testing.T, rec *httptest.ResponseRecorder, viaMiddleware bool, wantStatus int, wantCode, wantRequestID string, wantConfigRevision bool) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, wantStatus, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	respID := rec.Header().Get("X-Request-Id")
	if viaMiddleware {
		if respID == "" {
			t.Fatal("X-Request-Id response header missing")
		}
		if wantRequestID != "" && respID != wantRequestID {
			t.Fatalf("X-Request-Id = %q, want echoed %q", respID, wantRequestID)
		}
		if wantRequestID == "" && len(respID) != 32 {
			t.Fatalf("generated X-Request-Id = %q, want 32 hex chars", respID)
		}
	} else if respID != "" {
		t.Fatalf("X-Request-Id set without middleware: %q", respID)
	}

	top, errBody := decodeEnvelope(t, rec)
	wantTop := map[string]bool{"error": true}
	if wantConfigRevision {
		wantTop["config_revision"] = true
	}
	if len(top) != len(wantTop) {
		t.Fatalf("top-level keys = %v, want exactly %v; body=%s", keysOf(top), wantTop, rec.Body.String())
	}
	for k := range wantTop {
		if _, ok := top[k]; !ok {
			t.Fatalf("top-level key %q missing: %s", k, rec.Body.String())
		}
	}
	wantErr := map[string]bool{"code": true, "message": true}
	if viaMiddleware {
		wantErr["request_id"] = true
	}
	if len(errBody) != len(wantErr) {
		t.Fatalf("error keys = %v, want exactly %v; body=%s", keysOf(errBody), wantErr, rec.Body.String())
	}
	for k := range wantErr {
		if _, ok := errBody[k]; !ok {
			t.Fatalf("error key %q missing: %s", k, rec.Body.String())
		}
	}
	if errBody["code"] != wantCode {
		t.Fatalf("error.code = %v, want %q", errBody["code"], wantCode)
	}
	if wantRequestID != "" && errBody["request_id"] != wantRequestID {
		t.Fatalf("error.request_id = %v, want %q", errBody["request_id"], wantRequestID)
	}
	if msg, _ := errBody["message"].(string); msg == "" || strings.HasSuffix(msg, ".") {
		t.Fatalf("error.message = %q, want a sentence-case message without a trailing period", msg)
	}
	assertNoInternals(t, rec.Body.String())
}

func assertNoInternals(t *testing.T, body string) {
	t.Helper()
	for _, frag := range forbiddenFragments {
		if strings.Contains(body, frag) {
			t.Fatalf("response body leaks internal fragment %q: %s", frag, body)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func doRequest(h http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- the contract table ------------------------------------------------------

// validRouteBody passes store.ValidateRoute and reaches the write transaction.
const validRouteBody = `{"name":"r1","path_prefix":"/demo","targets":[{"host":"upstream.test","port":9000}]}`

// TestAdminErrorContract drives representative endpoints through the real mux
// and asserts the unified error contract: status discipline, envelope shape,
// no-store, request ids, and absence of leaked internals.
func TestAdminErrorContract(t *testing.T) {
	cases := []struct {
		name               string
		state              *fakeDBState
		method             string
		path               string
		body               string
		inboundReqID       string
		unauth             bool
		wantStatus         int
		wantCode           string
		wantConfigRevision bool
	}{
		{
			name:       "unauthenticated request is a 401 envelope",
			state:      &fakeDBState{role: "superadmin"},
			unauth:     true,
			method:     http.MethodGet,
			path:       "/admin/api/v1/routes",
			wantStatus: 401, wantCode: "admin.unauthenticated",
		},
		{
			name:         "inbound request id is echoed into the envelope",
			state:        &fakeDBState{role: "superadmin"},
			unauth:       true,
			method:       http.MethodGet,
			path:         "/admin/api/v1/routes",
			inboundReqID: "panel-abc_123.X-9",
			wantStatus:   401, wantCode: "admin.unauthenticated",
		},
		{
			name:       "non-superadmin write is a 403 envelope",
			state:      &fakeDBState{role: "user"},
			method:     http.MethodPost,
			path:       "/admin/api/v1/routes",
			body:       validRouteBody,
			wantStatus: 403, wantCode: "admin.forbidden",
		},
		{
			name:       "invalid route payload is a 400 envelope without internals",
			state:      &fakeDBState{role: "superadmin"},
			method:     http.MethodPost,
			path:       "/admin/api/v1/routes",
			body:       `{"name":"r1","path_prefix":"demo","targets":[{"host":"upstream.test","port":9000}]}`,
			wantStatus: 400, wantCode: "admin.invalid_route",
		},
		{
			name:       "zero-rows delete is a 404 envelope",
			state:      &fakeDBState{role: "superadmin", rowsAffected: 0},
			method:     http.MethodDelete,
			path:       "/admin/api/v1/routes/999",
			body:       `{}`,
			wantStatus: 404, wantCode: "admin.not_found",
		},
		{
			name: "duplicate key becomes a 409 with a fixed message",
			state: &fakeDBState{
				role: "superadmin",
				execErr: &mysql.MySQLError{
					Number:  mysqlErrDupEntry,
					Message: "Duplicate entry 'r1' for key 'routes.uk_name'",
				},
			},
			method: http.MethodPost, path: "/admin/api/v1/routes", body: validRouteBody,
			wantStatus: 409, wantCode: "admin.duplicate",
		},
		{
			name: "storage failure becomes a 500 without driver text",
			state: &fakeDBState{
				role:    "superadmin",
				execErr: &mysql.MySQLError{Number: mysqlErrLockWaitTO, Message: "Lock wait timeout exceeded"},
			},
			method: http.MethodPost, path: "/admin/api/v1/routes", body: validRouteBody,
			wantStatus: 500, wantCode: "admin.write_failed",
		},
		{
			name:       "debug settings load failure is a 500 envelope",
			state:      &fakeDBState{role: "superadmin"},
			method:     http.MethodGet,
			path:       "/admin/api/v1/settings/debug",
			wantStatus: 500, wantCode: "admin.error",
		},
		{
			name:       "biz settings load failure is a 500 envelope",
			state:      &fakeDBState{role: "superadmin"},
			method:     http.MethodGet,
			path:       "/admin/api/v1/settings/biz",
			wantStatus: 500, wantCode: "admin.error",
		},
		{
			name:       "stale expected_revision is a 409 with the current revision beside error",
			state:      &fakeDBState{role: "superadmin", rev: 5},
			method:     http.MethodPost,
			path:       "/admin/api/v1/routes",
			body:       `{"name":"r1","path_prefix":"/demo","expected_revision":4,"targets":[{"host":"upstream.test","port":9000}]}`,
			wantStatus: 409, wantCode: "admin.revision_conflict",
			wantConfigRevision: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newContractServer(t, tc.state)
			headers := map[string]string{}
			if !tc.unauth {
				headers["Cookie"] = sessionCookie
			}
			if tc.inboundReqID != "" {
				headers["X-Request-Id"] = tc.inboundReqID
			}
			rec := doRequest(s.Handler(nil), tc.method, tc.path, tc.body, headers)
			assertEnvelopeShape(t, rec, true, tc.wantStatus, tc.wantCode, tc.inboundReqID, tc.wantConfigRevision)
			if tc.wantConfigRevision {
				top, _ := decodeEnvelope(t, rec)
				if top["config_revision"] != float64(tc.state.rev) {
					t.Fatalf("config_revision = %v, want %d", top["config_revision"], tc.state.rev)
				}
			}
		})
	}
}

// TestSuccessfulWriteAuditsIPAndRequestID: a completed write records the
// client ip and the middleware request id in admin_audit (columns existed but
// request_id was never written before).
func TestSuccessfulWriteAuditsIPAndRequestID(t *testing.T) {
	state := &fakeDBState{role: "superadmin", rev: 5, rowsAffected: 1}
	s := newContractServer(t, state)
	const reqID = "audit-req-42"
	rec := doRequest(s.Handler(nil), http.MethodPost, "/admin/api/v1/routes", validRouteBody,
		map[string]string{"Cookie": sessionCookie, "X-Request-Id": reqID})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	var audit *execRecord
	for i := range state.execLog {
		if strings.Contains(state.execLog[i].query, "INSERT INTO admin_audit") {
			audit = &state.execLog[i]
		}
	}
	if audit == nil {
		t.Fatalf("no admin_audit insert recorded; execs=%d", len(state.execLog))
	}
	if len(audit.args) < 2 {
		t.Fatalf("audit insert has %d args, want at least the trailing request_id/ip pair", len(audit.args))
	}
	if id, _ := audit.args[len(audit.args)-2].(string); id != reqID {
		t.Fatalf("audit request_id = %v, want %q", audit.args[len(audit.args)-2], reqID)
	}
	if ip, _ := audit.args[len(audit.args)-1].(string); ip != "192.0.2.1" {
		t.Fatalf("audit ip = %v, want 192.0.2.1 (httptest default RemoteAddr)", audit.args[len(audit.args)-1])
	}
}

// --- login / session creation ------------------------------------------------

const loginBody = `{"username":"tester","password":"correct horse"}`

// newLoginServer wires a Server whose fake DB answers the login user lookup
// with an active user whose password hash matches loginBody's password.
func newLoginServer(t *testing.T) *Server {
	t.Helper()
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	return newContractServer(t, &fakeDBState{role: "superadmin", loginHash: hash, rowsAffected: 1})
}

// TestLoginIssuesSessionCookie pins the success path end to end: valid
// credentials produce 200 with the HttpOnly session cookie.
func TestLoginIssuesSessionCookie(t *testing.T) {
	s := newLoginServer(t)
	rec := doRequest(s.Handler(nil), http.MethodPost, "/admin/api/v1/auth/login", loginBody, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(cookie, cookieName+"=") || !strings.Contains(cookie, "HttpOnly") {
		t.Fatalf("Set-Cookie = %q, want the %q session cookie with HttpOnly", cookie, cookieName)
	}
}

// TestLoginRandFailureFailsClosed: a crypto/rand failure during session token
// creation must fail the login with a 500 envelope — never a predictable
// token — with the real error only in the server log.
func TestLoginRandFailureFailsClosed(t *testing.T) {
	orig := tokenRand
	tokenRand = func(b []byte) (int, error) { return 0, errors.New("rng unavailable (test)") }
	defer func() { tokenRand = orig }()

	s := newLoginServer(t)
	rec := doRequest(s.Handler(nil), http.MethodPost, "/admin/api/v1/auth/login", loginBody, nil)
	assertEnvelopeShape(t, rec, true, http.StatusInternalServerError, "admin.error", "", false)
	if strings.Contains(rec.Body.String(), "rng unavailable") {
		t.Fatal("RNG error text leaked into the response")
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("a session cookie was issued despite the RNG failure")
	}
}

// TestNewTokenRandFailure pins newToken's contract directly: an RNG failure
// yields an empty token and a non-nil error.
func TestNewTokenRandFailure(t *testing.T) {
	orig := tokenRand
	tokenRand = func(b []byte) (int, error) { return 0, errors.New("rng unavailable (test)") }
	defer func() { tokenRand = orig }()
	tok, err := newToken()
	if err == nil {
		t.Fatal("newToken returned nil error on RNG failure")
	}
	if tok != "" {
		t.Fatalf("newToken returned token %q on RNG failure, want empty", tok)
	}
}

// TestDeletePolicyAuditsBeforeState: a policy delete must capture the deleted
// row as the audit before-state, like every other delete endpoint.
func TestDeletePolicyAuditsBeforeState(t *testing.T) {
	state := &fakeDBState{role: "superadmin", rev: 5, rowsAffected: 1}
	s := newContractServer(t, state)
	rec := doRequest(s.Handler(nil), http.MethodDelete, "/admin/api/v1/policies/42", `{}`,
		map[string]string{"Cookie": sessionCookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	var audit *execRecord
	for i := range state.execLog {
		if strings.Contains(state.execLog[i].query, "INSERT INTO admin_audit") {
			audit = &state.execLog[i]
		}
	}
	if audit == nil {
		t.Fatal("no admin_audit insert recorded")
	}
	// args: actor_id, actor_name, action, resource_type, resource_id, before,
	// after, request_id, ip.
	if len(audit.args) != 9 {
		t.Fatalf("audit insert has %d args, want 9", len(audit.args))
	}
	before, ok := audit.args[5].(string)
	if !ok || !strings.Contains(before, `"caller":"svc-a"`) {
		t.Fatalf("audit before_state = %v, want the deleted policy row JSON", audit.args[5])
	}
}

// --- writeTxError classification (direct table test) -------------------------

// TestWriteTxErrorClassification pins every branch of the classifier: status,
// code, and the exact friendly message. Each response must carry no-store and
// must never contain the raw error text.
func TestWriteTxErrorClassification(t *testing.T) {
	dup := &mysql.MySQLError{Number: mysqlErrDupEntry, Message: "Duplicate entry 'x' for key 'uk'"}
	leak := "Error 1062: Duplicate entry 'x' for key 'uk' (DSN user:pass@tcp(10.9.9.9:3306)/pay_business)"
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{"duplicate 1062", dup, http.StatusConflict, "admin.duplicate", "a record with the same unique value already exists"},
		{"wrapped duplicate", fmt.Errorf("insert route: %w", dup), http.StatusConflict, "admin.duplicate", "a record with the same unique value already exists"},
		{"deadlock 1213", &mysql.MySQLError{Number: mysqlErrLockDeadlock, Message: "Deadlock found"}, http.StatusInternalServerError, "admin.write_failed", "the write could not complete due to concurrent activity, retry"},
		{"lock wait timeout 1205", &mysql.MySQLError{Number: mysqlErrLockWaitTO, Message: "Lock wait timeout"}, http.StatusInternalServerError, "admin.write_failed", "the write could not complete due to concurrent activity, retry"},
		{"bad conn", driver.ErrBadConn, http.StatusInternalServerError, "admin.storage_unavailable", "the configuration store is temporarily unavailable"},
		{"deadline exceeded", context.DeadlineExceeded, http.StatusInternalServerError, "admin.storage_unavailable", "the configuration store is temporarily unavailable"},
		{"context canceled", context.Canceled, http.StatusInternalServerError, "admin.storage_unavailable", "the configuration store is temporarily unavailable"},
		{"net op error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, http.StatusInternalServerError, "admin.storage_unavailable", "the configuration store is temporarily unavailable"},
		{"wrapped bad conn", fmt.Errorf("begin tx: %w", driver.ErrBadConn), http.StatusInternalServerError, "admin.storage_unavailable", "the configuration store is temporarily unavailable"},
		{"no rows", sql.ErrNoRows, http.StatusNotFound, "admin.not_found", "resource not found"},
		{"revision conflict", &conflictError{current: 9}, http.StatusConflict, "admin.revision_conflict", "config changed concurrently (current revision 9); refresh and retry"},
		{"unknown error hides internals", errors.New(leak), http.StatusInternalServerError, "admin.write_failed", "the write failed, see server logs"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/admin/api/v1/routes", nil)
			writeTxError(r, rec, tc.err)
			// No middleware ran: no request id header, no request_id key.
			assertEnvelopeShape(t, rec, false, tc.wantStatus, tc.wantCode, "", tc.wantCode == "admin.revision_conflict")
			_, errBody := decodeEnvelope(t, rec)
			if errBody["message"] != tc.wantMsg {
				t.Fatalf("message = %v, want %q", errBody["message"], tc.wantMsg)
			}
			if tc.wantCode == "admin.revision_conflict" {
				top, _ := decodeEnvelope(t, rec)
				if top["config_revision"] != float64(9) {
					t.Fatalf("config_revision = %v, want 9", top["config_revision"])
				}
			}
			if strings.Contains(rec.Body.String(), leak) {
				t.Fatal("raw error text leaked into the response")
			}
		})
	}
}

// --- mux 404/405 envelope ----------------------------------------------------

// TestAdminMux404Envelope: unknown paths outside the panel mount must answer
// with the JSON error envelope, not the net/http plain-text default.
func TestAdminMux404Envelope(t *testing.T) {
	s := &Server{limiter: newLoginLimiter()}
	h := s.Handler(nil)
	for _, path := range []string{"/nope", "/admin/api/v1/does-not-exist"} {
		rec := doRequest(h, http.MethodGet, path, "", nil)
		assertEnvelopeShape(t, rec, true, http.StatusNotFound, "admin.not_found", "", false)
		if strings.Contains(rec.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("%s: plain-text 404 leaked through", path)
		}
	}
}

// TestAdminMux405Envelope: a known API path with the wrong method must answer
// with the 405 envelope and keep the mux's Allow header.
func TestAdminMux405Envelope(t *testing.T) {
	s := &Server{limiter: newLoginLimiter()}
	h := s.Handler(nil)
	rec := doRequest(h, http.MethodDelete, "/admin/api/v1/routes", "", nil)
	assertEnvelopeShape(t, rec, true, http.StatusMethodNotAllowed, "admin.method_not_allowed", "", false)
	allow := rec.Header().Get("Allow")
	if !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
		t.Fatalf("Allow = %q, want the methods registered for the path (GET, POST)", allow)
	}
}

// TestAdminMuxPanelMountUntouched: the panel namespace keeps the native
// plain-text 404 — the envelope interception must not hijack the panel
// mount's responses. (Handler(nil) leaves /admin/ unmounted here; the mounted
// case needs the embedded FS and is covered by the same scoping rule.)
func TestAdminMuxPanelMountUntouched(t *testing.T) {
	s := &Server{limiter: newLoginLimiter()}
	h := s.Handler(nil)
	rec := doRequest(h, http.MethodGet, "/admin/some-panel-path", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want the untouched plain-text panel 404", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "404 page not found") {
		t.Fatalf("panel 404 body was rewritten: %q", body)
	}
}

// TestAdminMuxAPINamespaceBeatsSPAFallback: with a panel mounted (a stub that
// mimics the real SPA by answering 200 HTML for EVERYTHING it is asked), the
// API namespace must never leak through to the panel — unknown API paths get
// the 404 envelope and wrong-method requests the 405 envelope with Allow,
// while panel paths still get the SPA response.
func TestAdminMuxAPINamespaceBeatsSPAFallback(t *testing.T) {
	s := &Server{limiter: newLoginLimiter()}
	spa := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<!doctype html><html><body>RouteGate Admin</body></html>"))
	})
	h := s.Handler(spa)

	rec := doRequest(h, http.MethodGet, "/admin/api/v1/does-not-exist", "", nil)
	assertEnvelopeShape(t, rec, true, http.StatusNotFound, "admin.not_found", "", false)
	if strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("unknown API path served the SPA fallback: %q", rec.Body.String())
	}

	rec = doRequest(h, http.MethodGet, "/admin/api/v1/auth/login", "", nil)
	assertEnvelopeShape(t, rec, true, http.StatusMethodNotAllowed, "admin.method_not_allowed", "", false)
	if !strings.Contains(rec.Header().Get("Allow"), "POST") {
		t.Fatalf("Allow = %q, want POST", rec.Header().Get("Allow"))
	}

	rec = doRequest(h, http.MethodGet, "/admin/some-panel-route", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("panel route broken: status %d body %q", rec.Code, rec.Body.String())
	}
}

// --- request-ID middleware ---------------------------------------------------

// TestRequestIDMiddleware pins the three header behaviors on the
// unauthenticated 401 path (no DB involved): generate, echo, and
// reject-and-regenerate. The response body must always carry the same id as
// the header.
func TestRequestIDMiddleware(t *testing.T) {
	s := &Server{limiter: newLoginLimiter()}
	h := s.Handler(nil)

	assertID := func(t *testing.T, rec *httptest.ResponseRecorder, want string) string {
		t.Helper()
		id := rec.Header().Get("X-Request-Id")
		if id == "" {
			t.Fatal("X-Request-Id missing from response")
		}
		if want != "" && id != want {
			t.Fatalf("X-Request-Id = %q, want %q", id, want)
		}
		if want == "" {
			if _, err := hex.DecodeString(id); err != nil || len(id) != 32 {
				t.Fatalf("generated X-Request-Id = %q, want 32 hex chars", id)
			}
		}
		var body struct {
			Error struct {
				RequestID string `json:"request_id"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if body.Error.RequestID != id {
			t.Fatalf("body request_id = %q, header = %q", body.Error.RequestID, id)
		}
		return id
	}

	t.Run("generated when absent", func(t *testing.T) {
		assertID(t, doRequest(h, http.MethodGet, "/admin/api/v1/routes", "", nil), "")
	})

	t.Run("echoed when valid", func(t *testing.T) {
		const valid = "run-1.2_x-3"
		assertID(t, doRequest(h, http.MethodGet, "/admin/api/v1/routes", "",
			map[string]string{"X-Request-Id": valid}), valid)
	})

	t.Run("regenerated when invalid", func(t *testing.T) {
		for _, invalid := range []string{"bad id with spaces", "<script>", strings.Repeat("a", 129)} {
			id := assertID(t, doRequest(h, http.MethodGet, "/admin/api/v1/routes", "",
				map[string]string{"X-Request-Id": invalid}), "")
			if id == invalid {
				t.Fatalf("invalid request id %q was echoed back", invalid)
			}
		}
	})
}

// --- admin API tokens (mint / use / revoke lifecycle) -------------------------

// newBearerState returns a fake state whose driver resolves exactly one
// scripted token (raw) acting as the given live user row.
func newBearerState(raw string) *fakeDBState {
	return &fakeDBState{
		role:           "user",
		bearerRawToken: raw,
		bearerUserID:   9,
		bearerUsername: "ci-owner",
		bearerRole:     "user",
	}
}

// bearerUserScriptHash is the stored hash the mint test's insert must carry.
func TestAPITokenMintContract(t *testing.T) {
	state := &fakeDBState{role: "user", rev: 5, rowsAffected: 1}
	s := newContractServer(t, state)
	rec := doRequest(s.Handler(nil), http.MethodPost, "/admin/api/v1/auth/tokens",
		`{"name":"ci-bot","expires_in_days":30}`, map[string]string{"Cookie": sessionCookie})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID        float64 `json:"id"`
		Token     string  `json:"token"`
		Name      string  `json:"name"`
		Prefix    string  `json:"prefix"`
		ExpiresAt string  `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v; body=%s", err, rec.Body.String())
	}
	if !rawTokenShape.MatchString(body.Token) {
		t.Fatalf("raw token %q does not match ^rgt_[0-9a-f]{64}$", body.Token)
	}
	if body.Prefix != body.Token[:12] {
		t.Fatalf("prefix = %q, want the first 12 chars of the raw token", body.Prefix)
	}
	if body.Name != "ci-bot" {
		t.Fatalf("name = %q, want ci-bot", body.Name)
	}
	exp, err := time.Parse(time.RFC3339, body.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", body.ExpiresAt, err)
	}
	if d := time.Until(exp); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("expires_at = %v, want about 30 days out", exp)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	var insert, audit *execRecord
	for i := range state.execLog {
		switch q := state.execLog[i].query; {
		case strings.Contains(q, "INSERT INTO admin_api_tokens"):
			insert = &state.execLog[i]
		case strings.Contains(q, "INSERT INTO admin_audit"):
			audit = &state.execLog[i]
		}
	}
	if insert == nil {
		t.Fatalf("no admin_api_tokens insert recorded; execs=%d", len(state.execLog))
	}
	// args: user_id, name, token_hash, prefix, expires_at.
	if uid, _ := insert.args[0].(int64); uid != 1 {
		t.Fatalf("insert user_id = %v, want the session user id 1", insert.args[0])
	}
	hash, _ := insert.args[2].(string)
	if len(hash) != 64 || hash == body.Token {
		t.Fatalf("stored token_hash = %v, want the 64-hex SHA-256 (never the raw secret)", insert.args[2])
	}
	if hash != store.HashAPIToken(body.Token) {
		t.Fatal("stored hash is not the SHA-256 of the returned raw token")
	}
	if prefix, _ := insert.args[3].(string); prefix != body.Prefix {
		t.Fatalf("stored prefix = %v, want %q", insert.args[3], body.Prefix)
	}
	if audit == nil {
		t.Fatal("no admin_audit insert recorded")
	}
	if action, _ := audit.args[2].(string); action != "token.create" {
		t.Fatalf("audit action = %v, want token.create", audit.args[2])
	}
	if rtype, _ := audit.args[3].(string); rtype != "api_token" {
		t.Fatalf("audit resource_type = %v, want api_token", audit.args[3])
	}
	after, _ := audit.args[6].(string)
	if !strings.Contains(after, `"name":"ci-bot"`) || !strings.Contains(after, `"prefix":"`+body.Prefix+`"`) {
		t.Fatalf("audit after_state = %v, want name+prefix metadata", audit.args[6])
	}
	// The raw secret and its hash must appear nowhere in any persisted row.
	for _, e := range state.execLog {
		for _, a := range e.args {
			if s, ok := a.(string); ok && s == body.Token {
				t.Fatalf("raw token leaked into %s", e.query)
			}
		}
	}
}

// TestBearerAuthenticatesAsLiveUser: a Bearer token on a requireAuth endpoint
// runs the handler AS the token's user with the live user-row values, and the
// best-effort usage touch eventually lands (log-only on failure).
func TestBearerAuthenticatesAsLiveUser(t *testing.T) {
	raw, err := store.GenerateAPITokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	state := newBearerState(raw)
	s := newContractServer(t, state)
	rec := doRequest(s.Handler(nil), http.MethodGet, "/admin/api/v1/auth/me", "",
		map[string]string{"Authorization": "Bearer " + raw})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["username"] != "ci-owner" || body["role"] != "user" {
		t.Fatalf("me = %v, want the token's live user-row values", body)
	}
	// The touch fires on a detached context; poll briefly for its exec record.
	touched := func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		for _, e := range state.execLog {
			if strings.Contains(e.query, "UPDATE admin_api_tokens SET last_used_at") {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(2 * time.Second)
	for !touched() {
		if time.Now().After(deadline) {
			t.Fatal("TouchTokenUsage never fired for the bearer request")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBearerFailuresAreUniform401 pins the token failure contract: every
// resolution failure is the same 401 admin.token_invalid envelope with no
// fall-through to the session cookie.
func TestBearerFailuresAreUniform401(t *testing.T) {
	raw, err := store.GenerateAPITokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	garbage := "rgt_" + strings.Repeat("ff", 32) // well-formed but unknown hash
	cases := []struct {
		name    string
		state   *fakeDBState
		headers map[string]string
		want    string // expected error code
	}{
		{
			name: "expired token",
			state: func() *fakeDBState {
				st := newBearerState(raw)
				st.bearerExpiresAt = time.Now().Add(-time.Minute)
				return st
			}(),
			headers: map[string]string{"Authorization": "Bearer " + raw},
			want:    "admin.token_invalid",
		},
		{
			name: "revoked token",
			state: func() *fakeDBState {
				st := newBearerState(raw)
				st.bearerRevokedAt = sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true}
				return st
			}(),
			headers: map[string]string{"Authorization": "Bearer " + raw},
			want:    "admin.token_invalid",
		},
		{
			name:    "disabled user",
			state:   func() *fakeDBState { st := newBearerState(raw); st.bearerUserStatus = "disabled"; return st }(),
			headers: map[string]string{"Authorization": "Bearer " + raw},
			want:    "admin.token_invalid",
		},
		{
			name:    "garbage bearer",
			state:   newBearerState(raw),
			headers: map[string]string{"Authorization": "Bearer " + garbage},
			want:    "admin.token_invalid",
		},
		{
			name:    "malformed bearer",
			state:   newBearerState(raw),
			headers: map[string]string{"Authorization": "Bearer rgt_short"},
			want:    "admin.token_invalid",
		},
		{
			// A bearer attempt that fails must NOT fall through to the valid
			// session cookie the fake driver would happily accept.
			name:    "no cookie fall-through",
			state:   newBearerState(raw),
			headers: map[string]string{"Authorization": "Bearer " + garbage, "Cookie": sessionCookie},
			want:    "admin.token_invalid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newContractServer(t, tc.state)
			rec := doRequest(s.Handler(nil), http.MethodGet, "/admin/api/v1/auth/me", "", tc.headers)
			assertEnvelopeShape(t, rec, true, http.StatusUnauthorized, tc.want, "", false)
		})
	}
}

// TestMissingBearerLeavesCookieFlowUntouched: without an Authorization header
// the cookie path behaves exactly as before (valid cookie still authenticates,
// absent cookie is still admin.unauthenticated).
func TestMissingBearerLeavesCookieFlowUntouched(t *testing.T) {
	state := &fakeDBState{role: "user"}
	s := newContractServer(t, state)
	rec := doRequest(s.Handler(nil), http.MethodGet, "/admin/api/v1/auth/me", "",
		map[string]string{"Cookie": sessionCookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 via the unchanged cookie flow; body=%s", rec.Code, rec.Body.String())
	}
	rec = doRequest(s.Handler(nil), http.MethodGet, "/admin/api/v1/auth/me", "", nil)
	assertEnvelopeShape(t, rec, true, http.StatusUnauthorized, "admin.unauthenticated", "", false)
	// A non-Bearer Authorization scheme is not a token attempt either.
	rec = doRequest(s.Handler(nil), http.MethodGet, "/admin/api/v1/auth/me", "",
		map[string]string{"Authorization": "Basic dXNlcjpwYXNz"})
	assertEnvelopeShape(t, rec, true, http.StatusUnauthorized, "admin.unauthenticated", "", false)
}

// TestBearerOnSessionSemanticsEndpointsIgnored: login and logout ignore the
// Authorization header entirely.
func TestBearerOnSessionSemanticsEndpointsIgnored(t *testing.T) {
	raw, err := store.GenerateAPITokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	state := newBearerState(raw)
	s := newContractServer(t, state)
	// Login with a Bearer header and an empty body: the bearer branch must
	// not run — the response is the login body-validation 400, not a 401.
	rec := doRequest(s.Handler(nil), http.MethodPost, "/admin/api/v1/auth/login", "",
		map[string]string{"Authorization": "Bearer " + raw})
	assertEnvelopeShape(t, rec, true, http.StatusBadRequest, "admin.bad_request", "", false)
	// Logout with a Bearer header and no cookie: exempted from token
	// semantics, so the unchanged cookie flow answers unauthenticated.
	rec = doRequest(s.Handler(nil), http.MethodPost, "/admin/api/v1/auth/logout", "",
		map[string]string{"Authorization": "Bearer " + raw})
	assertEnvelopeShape(t, rec, true, http.StatusUnauthorized, "admin.unauthenticated", "", false)
}

// TestRevokeTokenContract: revoking an own token succeeds with an audit row;
// a foreign or missing id is the uniform 404.
func TestRevokeTokenContract(t *testing.T) {
	t.Run("own token", func(t *testing.T) {
		state := &fakeDBState{role: "user", rowsAffected: 1}
		s := newContractServer(t, state)
		rec := doRequest(s.Handler(nil), http.MethodDelete, "/admin/api/v1/auth/tokens/7", "",
			map[string]string{"Cookie": sessionCookie})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"status":"revoked"`) {
			t.Fatalf("body = %s, want the revoked status", rec.Body.String())
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		var audit *execRecord
		for i := range state.execLog {
			if strings.Contains(state.execLog[i].query, "INSERT INTO admin_audit") {
				audit = &state.execLog[i]
			}
		}
		if audit == nil {
			t.Fatal("no admin_audit insert recorded")
		}
		if action, _ := audit.args[2].(string); action != "token.revoke" {
			t.Fatalf("audit action = %v, want token.revoke", audit.args[2])
		}
		before, _ := audit.args[5].(string)
		if !strings.Contains(before, `"prefix":"rgt_01234567"`) {
			t.Fatalf("audit before_state = %v, want the token metadata", audit.args[5])
		}
	})
	t.Run("foreign or missing id", func(t *testing.T) {
		state := &fakeDBState{role: "user", rowsAffected: 0}
		s := newContractServer(t, state)
		rec := doRequest(s.Handler(nil), http.MethodDelete, "/admin/api/v1/auth/tokens/999", "",
			map[string]string{"Cookie": sessionCookie})
		assertEnvelopeShape(t, rec, true, http.StatusNotFound, "admin.not_found", "", false)
	})
}

// TestTokenListNeverLeaksSecrets: the list endpoint returns display metadata
// only — never the stored hash or any full raw secret.
func TestTokenListNeverLeaksSecrets(t *testing.T) {
	state := &fakeDBState{role: "user"}
	s := newContractServer(t, state)
	rec := doRequest(s.Handler(nil), http.MethodGet, "/admin/api/v1/auth/tokens", "",
		map[string]string{"Cookie": sessionCookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, banned := range []string{"token_hash", "hash", "secret"} {
		if strings.Contains(body, banned) {
			t.Fatalf("token list leaks %q: %s", banned, body)
		}
	}
	if m := rawTokenShape.FindString(body); m != "" {
		t.Fatalf("token list contains a full raw-token-shaped value: %s", m)
	}
	if !strings.Contains(body, `"prefix":"rgt_01234567"`) || !strings.Contains(body, `"revoked_at"`) {
		t.Fatalf("token list lost display metadata (prefix/revoked_at): %s", body)
	}
}

// TestTokenCreateValidation: friendly 400s for the name and expiry rules.
func TestTokenCreateValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty name", `{"name":"  ","expires_in_days":30}`, "a token name is required"},
		{"missing name", `{"expires_in_days":30}`, "a token name is required"},
		{"oversized name", `{"name":"` + strings.Repeat("x", 65) + `","expires_in_days":30}`, "a token name must be at most 64 characters"},
		{"zero expiry", `{"name":"ci-bot","expires_in_days":0}`, "expires_in_days must be between 1 and 365"},
		{"negative expiry", `{"name":"ci-bot","expires_in_days":-5}`, "expires_in_days must be between 1 and 365"},
		{"oversized expiry", `{"name":"ci-bot","expires_in_days":366}`, "expires_in_days must be between 1 and 365"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newContractServer(t, &fakeDBState{role: "user"})
			rec := doRequest(s.Handler(nil), http.MethodPost, "/admin/api/v1/auth/tokens", tc.body,
				map[string]string{"Cookie": sessionCookie})
			assertEnvelopeShape(t, rec, true, http.StatusBadRequest, "admin.invalid_token", "", false)
			if _, errBody := decodeEnvelope(t, rec); errBody["message"] != tc.want {
				t.Fatalf("message = %v, want %q", errBody["message"], tc.want)
			}
		})
	}
}
