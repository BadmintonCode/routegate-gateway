package adminapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/assistant"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

// Server is the admin plane (ADR-0001 §7, §8). All write operations run in one
// transaction: business row + settings.config_revision bump + admin_audit row.
type Server struct {
	cfg        AdminConfig
	db         *sql.DB
	holder     *snapshot.Holder
	instanceID string
	version    string

	assistant *assistant.Engine

	limiter *loginLimiter
}

// AdminConfig carries what the admin plane needs from the process config.
type AdminConfig struct {
	BootstrapUsername string
	BootstrapPassword string
	OwnProxyPort      int                     // data-plane port, for route self-loop validation
	AccessLogFilePath string                  // file-sink base path; empty when the file sink is disabled
	Debug             *accesslog.DebugManager // panel-managed debug logging (settings + sink + files)
}

// New builds the admin server.
func New(cfg AdminConfig, db *sql.DB, holder *snapshot.Holder, instanceID, version string) *Server {
	return &Server{
		cfg: cfg, db: db, holder: holder, instanceID: instanceID, version: version,
		assistant: assistant.NewEngine(db, holder, cfg.AccessLogFilePath, instanceID),
		limiter:   newLoginLimiter(),
	}
}

type ctxKey string

const (
	ctxUserID    ctxKey = "user_id"
	ctxRole      ctxKey = "role"
	ctxUser      ctxKey = "username"
	ctxRequestID ctxKey = "request_id"
)

// requestIDPattern bounds and shapes accepted inbound X-Request-Id values:
// letters, digits, underscore, dot and hyphen, 1..128 chars. Anything else is
// discarded and regenerated so neither response headers nor audit rows can
// carry attacker-controlled junk.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,128}$`)

// newRequestID returns 16 random bytes as hex.
func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure should never happen on a supported platform;
		// fall back to a time-derived id so rows stay correlatable.
		return fmt.Sprintf("req-%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// requestIDFromHeader derives the request ID: echo the inbound X-Request-Id
// when it matches the pattern, generate a fresh one otherwise.
func requestIDFromHeader(v string) string {
	if v != "" && len(v) <= 128 && requestIDPattern.MatchString(v) {
		return v
	}
	return newRequestID()
}

// requestID returns the middleware-assigned request ID for r ("" when absent,
// e.g. in unit tests that bypass the middleware).
func requestID(r *http.Request) string {
	if r == nil {
		return ""
	}
	id, _ := r.Context().Value(ctxRequestID).(string)
	return id
}

// errorJSON renders the unified error envelope (every non-2xx JSON response):
// {"error":{"code":"<domain>.<reason>","message":"<stable English sentence>",
// "request_id":"..."}}. request_id appears only when the middleware assigned
// one. Cache-Control: no-store keeps intermediaries from caching errors.
func errorJSON(r *http.Request, w http.ResponseWriter, status int, code, message string) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope(r, code, message))
}

// errorEnvelope builds the error body. The revision-conflict response reuses
// it and adds a top-level config_revision next to error.
func errorEnvelope(r *http.Request, code, message string) map[string]any {
	e := map[string]any{"code": code, "message": message}
	if id := requestID(r); id != "" {
		e["request_id"] = id
	}
	return map[string]any{"error": e}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if status >= 400 {
		// Non-2xx JSON bodies follow the same no-store rule as errorJSON
		// (e.g. the 409 revision-conflict envelope).
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

const cookieName = "routegate-admin-session"

// Session lifetime policy (ADR-0001 §8.2): the DB-side sliding window bounds
// inactivity and the absolute expiry bounds total lifetime. The login cookie
// carries a MaxAge matching the absolute expiry so closing the browser no
// longer drops the session — the server-side checks remain the enforcement.
const (
	sessionSlidingWindow  = 8 * time.Hour
	sessionAbsoluteExpiry = 24 * time.Hour
)

// Handler returns the admin API + panel mount router.
func (s *Server) Handler(panel http.Handler) http.Handler {
	apiMux := http.NewServeMux()

	// Auth.
	apiMux.HandleFunc("POST /admin/api/v1/auth/login", s.handleLogin)
	apiMux.HandleFunc("POST /admin/api/v1/auth/logout", s.requireAuth(s.handleLogout))
	apiMux.HandleFunc("GET /admin/api/v1/auth/me", s.requireAuth(s.handleMe))
	apiMux.HandleFunc("POST /admin/api/v1/users/me/password", s.requireAuth(s.handlePasswordChange))

	// Self-service admin API tokens (user-scoped Bearer credentials for AI
	// and machine clients): any authenticated user manages their OWN tokens;
	// a token acts as its user with the role resolved live on every request.
	apiMux.HandleFunc("GET /admin/api/v1/auth/tokens", s.requireAuth(s.handleListTokens))
	apiMux.HandleFunc("POST /admin/api/v1/auth/tokens", s.requireAuth(s.handleCreateAPIToken))
	apiMux.HandleFunc("DELETE /admin/api/v1/auth/tokens/{id}", s.requireAuth(s.handleRevokeToken))

	// Routes.
	apiMux.HandleFunc("GET /admin/api/v1/routes", s.requireAuth(s.handleListRoutes))
	apiMux.HandleFunc("POST /admin/api/v1/routes", s.requireSuperadmin(s.handleCreateRoute))
	apiMux.HandleFunc("PUT /admin/api/v1/routes/{id}", s.requireSuperadmin(s.handleUpdateRoute))
	apiMux.HandleFunc("DELETE /admin/api/v1/routes/{id}", s.requireSuperadmin(s.handleDeleteRoute))

	// Registered operations of a route (managed registry).
	apiMux.HandleFunc("GET /admin/api/v1/routes/{id}/operations", s.requireAuth(s.handleListRouteOperations))
	apiMux.HandleFunc("POST /admin/api/v1/routes/{id}/operations", s.requireSuperadmin(s.handleCreateRouteOperation))
	apiMux.HandleFunc("PUT /admin/api/v1/operations/{id}", s.requireSuperadmin(s.handleUpdateOperation))
	apiMux.HandleFunc("DELETE /admin/api/v1/operations/{id}", s.requireSuperadmin(s.handleDeleteOperation))
	apiMux.HandleFunc("GET /admin/api/v1/routes/{id}/callers", s.requireAuth(s.handleListRouteCallers))

	// Policies.
	apiMux.HandleFunc("GET /admin/api/v1/policies", s.requireAuth(s.handleListPolicies))
	apiMux.HandleFunc("POST /admin/api/v1/policies", s.requireSuperadmin(s.handleCreatePolicy))
	apiMux.HandleFunc("PUT /admin/api/v1/policies/{id}", s.requireSuperadmin(s.handleUpdatePolicy))
	apiMux.HandleFunc("DELETE /admin/api/v1/policies/{id}", s.requireSuperadmin(s.handleDeletePolicy))
	apiMux.HandleFunc("GET /admin/api/v1/policies/expiring", s.requireAuth(s.handleExpiringPolicies))
	apiMux.HandleFunc("POST /admin/api/v1/policies/test", s.requireAuth(s.handlePolicyTest))

	// Identities.
	apiMux.HandleFunc("GET /admin/api/v1/identities", s.requireAuth(s.handleListIdentities))
	apiMux.HandleFunc("POST /admin/api/v1/identities", s.requireSuperadmin(s.handleCreateIdentity))
	apiMux.HandleFunc("POST /admin/api/v1/identities/validate", s.requireSuperadmin(s.handleValidateIdentity))
	apiMux.HandleFunc("POST /admin/api/v1/identities/generate", s.requireSuperadmin(s.handleGenerateIdentity))
	apiMux.HandleFunc("POST /admin/api/v1/identities/{id}/revoke", s.requireSuperadmin(s.handleRevokeIdentity))
	apiMux.HandleFunc("DELETE /admin/api/v1/identities/{id}", s.requireSuperadmin(s.handleDeleteIdentity))

	// Users (superadmin) + audit + gateway state.
	apiMux.HandleFunc("GET /admin/api/v1/users", s.requireSuperadmin(s.handleListUsers))
	apiMux.HandleFunc("POST /admin/api/v1/users", s.requireSuperadmin(s.handleCreateUser))
	apiMux.HandleFunc("PUT /admin/api/v1/users/{id}", s.requireSuperadmin(s.handleUpdateUser))

	apiMux.HandleFunc("GET /admin/api/v1/audit/admin", s.requireAuth(s.handleAdminAudit))
	apiMux.HandleFunc("GET /admin/api/v1/audit/access", s.requireAuth(s.handleAccessAudit))
	apiMux.HandleFunc("GET /admin/api/v1/logs/access", s.requireAuth(s.handleAccessLogFile))
	// Debug log: metadata list for all admin users; sensitive payload detail
	// is superadmin-only and audited (ADR-0001 §6.6).
	apiMux.HandleFunc("GET /admin/api/v1/logs/debug", s.requireAuth(s.handleDebugLogs))
	apiMux.HandleFunc("GET /admin/api/v1/logs/debug/{request_id}", s.requireSuperadmin(s.handleDebugLogDetail))

	apiMux.HandleFunc("GET /admin/api/v1/gateway/config", s.requireAuth(s.handleGatewayConfig))
	apiMux.HandleFunc("GET /admin/api/v1/gateway/instances", s.requireAuth(s.handleInstances))
	apiMux.HandleFunc("GET /admin/api/v1/gateway/snapshot", s.requireAuth(s.handleSnapshot))

	// Settings (panel 设置 page). Debug logging is sensitive: writes are
	// superadmin-only and audited; instances pick changes up on their next
	// sync tick (ADR-0001 §6.6).
	apiMux.HandleFunc("GET /admin/api/v1/settings/debug", s.requireAuth(s.handleGetDebugSettings))
	apiMux.HandleFunc("PUT /admin/api/v1/settings/debug", s.requireSuperadmin(s.handlePutDebugSettings))
	apiMux.HandleFunc("GET /admin/api/v1/settings/biz", s.requireAuth(s.handleGetBizSettings))
	apiMux.HandleFunc("PUT /admin/api/v1/settings/biz", s.requireSuperadmin(s.handlePutBizSettings))
	apiMux.HandleFunc("GET /admin/api/v1/alerts/channels", s.requireAuth(s.handleListAlertChannels))
	apiMux.HandleFunc("POST /admin/api/v1/alerts/channels", s.requireSuperadmin(s.handleCreateAlertChannel))
	apiMux.HandleFunc("PUT /admin/api/v1/alerts/channels/{id}", s.requireSuperadmin(s.handleUpdateAlertChannel))
	apiMux.HandleFunc("DELETE /admin/api/v1/alerts/channels/{id}", s.requireSuperadmin(s.handleDeleteAlertChannel))
	apiMux.HandleFunc("POST /admin/api/v1/alerts/channels/{id}/test", s.requireSuperadmin(s.handleTestAlertChannel))
	// Alert rules (§9.2): composable triggers scoped to routes, firing into
	// one channel; every firing is recorded in alert_events.
	apiMux.HandleFunc("GET /admin/api/v1/alerts/rules", s.requireAuth(s.handleListAlertRules))
	apiMux.HandleFunc("POST /admin/api/v1/alerts/rules", s.requireSuperadmin(s.handleCreateAlertRule))
	apiMux.HandleFunc("PUT /admin/api/v1/alerts/rules/{id}", s.requireSuperadmin(s.handleUpdateAlertRule))
	apiMux.HandleFunc("DELETE /admin/api/v1/alerts/rules/{id}", s.requireSuperadmin(s.handleDeleteAlertRule))
	apiMux.HandleFunc("GET /admin/api/v1/alerts/events", s.requireAuth(s.handleListAlertEvents))

	// AI assistant (§10): chat over read-only gateway data. Sessions are
	// user-owned; endpoint management is superadmin-only. The chat endpoint
	// streams SSE and the model's reachable surface is the read-only tool
	// whitelist in internal/assistant.
	apiMux.HandleFunc("GET /admin/api/v1/llm/endpoints", s.requireAuth(s.handleListLLMEndpoints))
	apiMux.HandleFunc("POST /admin/api/v1/llm/endpoints", s.requireSuperadmin(s.handleCreateLLMEndpoint))
	apiMux.HandleFunc("PUT /admin/api/v1/llm/endpoints/{id}", s.requireSuperadmin(s.handleUpdateLLMEndpoint))
	apiMux.HandleFunc("DELETE /admin/api/v1/llm/endpoints/{id}", s.requireSuperadmin(s.handleDeleteLLMEndpoint))
	apiMux.HandleFunc("POST /admin/api/v1/llm/endpoints/{endpoint_id}/models", s.requireSuperadmin(s.handleCreateLLMModel))
	apiMux.HandleFunc("POST /admin/api/v1/llm/endpoints/{endpoint_id}/models/discover", s.requireSuperadmin(s.handleDiscoverLLMModels))
	apiMux.HandleFunc("PUT /admin/api/v1/llm/models/{id}", s.requireSuperadmin(s.handleUpdateLLMModel))
	apiMux.HandleFunc("DELETE /admin/api/v1/llm/models/{id}", s.requireSuperadmin(s.handleDeleteLLMModel))
	apiMux.HandleFunc("POST /admin/api/v1/llm/models/{id}/test", s.requireSuperadmin(s.handleTestLLMModel))
	apiMux.HandleFunc("POST /admin/api/v1/assistant/sessions", s.requireAuth(s.handleCreateAssistantSession))
	apiMux.HandleFunc("GET /admin/api/v1/assistant/sessions", s.requireAuth(s.handleListAssistantSessions))
	apiMux.HandleFunc("PUT /admin/api/v1/assistant/sessions/{key}", s.requireAuth(s.handleUpdateAssistantSession))
	apiMux.HandleFunc("GET /admin/api/v1/assistant/sessions/{key}/messages", s.requireAuth(s.handleListAssistantMessages))
	apiMux.HandleFunc("DELETE /admin/api/v1/assistant/sessions/{key}", s.requireAuth(s.handleDeleteAssistantSession))
	apiMux.HandleFunc("POST /admin/api/v1/assistant/chat", s.requireAuth(s.handleAssistantChat))

	// Endpoint catalog (documentation backbone): meta/endpoints and
	// openapi.json render from the same hand-maintained list that
	// apicatalog_test.go pins against these registrations.
	//
	// PUBLIC (owner decision 2026-11-09): both serve static documentation —
	// no state, no secrets, no user data — and are reachable WITHOUT any
	// authentication so AI agents and integrators can discover the API and
	// verify connectivity before holding a credential.
	apiMux.HandleFunc("GET /admin/api/v1/meta/endpoints", s.handleMetaEndpoints)
	apiMux.HandleFunc("GET /admin/api/v1/openapi.json", s.handleOpenAPI)
	// The paste-ready AI prompt template ships embedded and serves as
	// text/markdown (PUBLIC: static content, no state, no secrets).
	apiMux.HandleFunc("GET /admin/api/v1/meta/prompt", s.handleAIPrompt)
	// The routegate-admin agent skill is self-hosted here (PUBLIC): an AI
	// agent can install the skill from the admin host alone — no repository
	// checkout. Whitelisted exact filenames only (see handleSkillFile).
	apiMux.HandleFunc("GET /admin/api/v1/meta/skill/SKILL.md", s.handleSkillFile)
	apiMux.HandleFunc("GET /admin/api/v1/meta/skill/ARCHITECTURE.md", s.handleSkillFile)
	apiMux.HandleFunc("GET /admin/api/v1/meta/skill/AI-PROMPT.md", s.handleSkillFile)

	// Panel + redirect. Mounted on its OWN mux: the SPA fallback must never
	// answer API requests — the api mux below serves /admin/api/* exclusively,
	// so unknown or wrongly-methoded API paths fall through to its native
	// 404/405, which envelopeWriter converts to the unified error envelope.
	rootMux := http.NewServeMux()
	if panel != nil {
		rootMux.Handle("GET /admin/", panel)
		rootMux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/admin/", http.StatusFound)
		})
	}

	// Request-ID middleware: every admin response carries X-Request-Id and
	// every error envelope and audit row can reference it. Both muxes are
	// additionally wrapped so the net/http default plain-text 404/405
	// responses come out as the unified error envelope (see envelopeWriter).
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := requestIDFromHeader(r.Header.Get("X-Request-Id"))
		w.Header().Set("X-Request-Id", reqID)
		r = r.WithContext(context.WithValue(r.Context(), ctxRequestID, reqID))
		ew := &envelopeWriter{ResponseWriter: w, req: r}
		defer ew.finish()
		if strings.HasPrefix(r.URL.Path, "/admin/api/") {
			apiMux.ServeHTTP(ew, r)
			return
		}
		rootMux.ServeHTTP(ew, r)
	})
}

// isPanelPath reports whether p belongs to the admin panel mount: /admin/...
// outside the /admin/api/ prefix. The panel (and the file server behind it)
// owns those responses, so the envelope interception leaves them untouched.
func isPanelPath(p string) bool {
	return strings.HasPrefix(p, "/admin/") && !strings.HasPrefix(p, "/admin/api/")
}

// envelopeWriter converts the net/http default plain-text 404 ("404 page not
// found") and 405 (Method Not Allowed) responses into the unified error
// envelope, keeping every header the mux already set (the 405 Allow header).
//
// This lives at the writer level because registering a "/" catch-all route
// instead would make every request match the mux and silently remove the
// native 405 behavior: wrong-method requests on known API paths would lose
// the Allow header. Responses whose Content-Type is already application/json
// pass through untouched (handlers write their own envelopes), and panel
// paths are excluded entirely.
type envelopeWriter struct {
	http.ResponseWriter
	req      *http.Request
	replaced int // intercepted plain-text status pending replacement (0 = none)
}

func (e *envelopeWriter) WriteHeader(status int) {
	if (status == http.StatusNotFound || status == http.StatusMethodNotAllowed) &&
		!strings.HasPrefix(e.Header().Get("Content-Type"), "application/json") &&
		!isPanelPath(e.req.URL.Path) {
		e.replaced = status
		return
	}
	e.ResponseWriter.WriteHeader(status)
}

func (e *envelopeWriter) Write(b []byte) (int, error) {
	if e.replaced == 0 {
		return e.ResponseWriter.Write(b)
	}
	status := e.replaced
	e.replaced = 0
	if status == http.StatusMethodNotAllowed {
		errorJSON(e.req, e.ResponseWriter, status, "admin.method_not_allowed", "the HTTP method is not allowed for this resource")
	} else {
		errorJSON(e.req, e.ResponseWriter, status, "admin.not_found", "the requested admin resource does not exist")
	}
	// The plain-text body is swallowed; its length is still reported so the
	// caller's write accounting stays consistent.
	return len(b), nil
}

// finish emits the pending envelope when an intercepted handler reported the
// status but never wrote a body.
func (e *envelopeWriter) finish() {
	if e.replaced != 0 {
		_, _ = e.Write(nil)
	}
}

// Bootstrap creates the first superadmin when the users table is empty
// (ADR-0001 §8.4).
func (s *Server) Bootstrap(ctx context.Context) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	username := s.cfg.BootstrapUsername
	password := s.cfg.BootstrapPassword
	random := false
	if username == "" {
		username = "admin"
	}
	if password == "" {
		pw, err := randomPassword()
		if err != nil {
			return err
		}
		password = pw
		random = true
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, display_name, role, must_change_password) VALUES (?, ?, 'Bootstrap admin', 'superadmin', 1)`,
		username, hash)
	if err != nil {
		return err
	}
	uid, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO admin_audit (actor_id, actor_name, action, resource_type, resource_id) VALUES (?, ?, 'bootstrap', 'user', ?)`,
		uid, username, username); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if random {
		log.Printf("ADMIN BOOTSTRAP: created superadmin %q with one-time password %q — change it at first login and never let this line reach logs aggregation", username, password)
	} else {
		log.Printf("admin bootstrap: created superadmin %q (password from environment, must change at first login)", username)
	}
	return nil
}

func randomPassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// --- sessions ---

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type sessionUser struct {
	id                 int64
	username, role     string
	mustChangePassword bool
}

// bearerToken extracts a Bearer credential from the Authorization header:
// the scheme is matched case-insensitively, the secret taken verbatim. ok is
// false when the header is absent, uses another scheme (Basic, …), or is
// empty — those requests fall through to the session-cookie path unchanged.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, value, _ := strings.Cut(h, " ")
	if !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(value) == "" {
		return "", false
	}
	return strings.TrimSpace(value), true
}

// bearerExemptPaths lists session-semantics endpoints where an Authorization
// header is IGNORED: login authenticates with username and password (it never
// reaches requireAuth, but is listed anyway so a future wiring change cannot
// silently give it token semantics), and logout revokes the cookie session
// only — a Bearer header there has no session to revoke.
var bearerExemptPaths = map[string]bool{
	"/admin/api/v1/auth/login":  true,
	"/admin/api/v1/auth/logout": true,
}

// requireAuth wraps a handler with authentication. A request carrying an
// Authorization: Bearer header resolves an admin API token EXCLUSIVELY — on
// any failure it answers the uniform 401 token envelope with no fall-through
// to the cookie — while every other request authenticates the session cookie
// as before. Both paths populate the same context keys with the live user-row
// values, so handlers and requireSuperadmin cannot tell them apart: a token
// acts AS its user and the role is resolved fresh on every request, so role
// changes and disabling the user take effect immediately.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tok, ok := bearerToken(r); ok && !bearerExemptPaths[r.URL.Path] {
			u, err := store.ResolveToken(r.Context(), s.db, tok)
			if err != nil {
				if !errors.Is(err, store.ErrTokenInvalid) {
					// Unexpected storage failure: log it server-side; the
					// client still gets the same uniform 401 (matching the
					// cookie path, which answers 401 on any error too).
					log.Printf("admin api error: %v", err)
				}
				errorJSON(r, w, http.StatusUnauthorized, "admin.token_invalid", "the API token is invalid, expired, or revoked")
				return
			}
			ctx := context.WithValue(r.Context(), ctxUserID, u.Token.UserID)
			ctx = context.WithValue(ctx, ctxRole, u.Role)
			ctx = context.WithValue(ctx, ctxUser, u.Username)
			s.touchTokenUsage(u.Token.ID)
			next(w, r.WithContext(ctx))
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil || c.Value == "" {
			errorJSON(r, w, http.StatusUnauthorized, "admin.unauthenticated", "login required")
			return
		}
		u, err := s.userForToken(r.Context(), c.Value)
		if err != nil {
			errorJSON(r, w, http.StatusUnauthorized, "admin.unauthenticated", "session invalid or expired")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserID, u.id)
		ctx = context.WithValue(ctx, ctxRole, u.role)
		ctx = context.WithValue(ctx, ctxUser, u.username)
		next(w, r.WithContext(ctx))
	}
}

// touchTokenUsage records API-token activity best-effort: a detached context
// (the request may finish first), a short timeout, and log-only failures —
// usage tracking must never influence the request outcome.
func (s *Server) touchTokenUsage(tokenID int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.TouchTokenUsage(ctx, s.db, tokenID); err != nil {
			log.Printf("admin api: token usage touch failed: %v", err)
		}
	}()
}

func (s *Server) userForToken(ctx context.Context, token string) (*sessionUser, error) {
	if token == "" || len(token) > 128 {
		return nil, fmt.Errorf("bad token")
	}
	var u sessionUser
	var expires, absolute sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT s.user_id, u.username, u.role, u.must_change_password, s.expires_at, s.absolute_expires_at
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = ? AND s.revoked_at IS NULL AND u.status = 'active'`,
		hashToken(token)).Scan(&u.id, &u.username, &u.role, &u.mustChangePassword, &expires, &absolute)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if !expires.Valid || !absolute.Valid || now.After(expires.Time) || now.After(absolute.Time) {
		return nil, fmt.Errorf("session expired")
	}
	// Sliding window: extend expires_at on activity (cap at absolute expiry).
	newExpiry := now.Add(8 * time.Hour)
	if newExpiry.After(absolute.Time) {
		newExpiry = absolute.Time
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE token_hash = ?`, newExpiry, hashToken(token))
	return &u, nil
}

// requireSuperadmin enforces the v1 RBAC matrix: writes are superadmin-only
// (ADR-0001 §8.1; regular users are read-only until the v1.5 review flow).
func (s *Server) requireSuperadmin(next http.HandlerFunc) http.HandlerFunc {
	wrapped := s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if role, _ := r.Context().Value(ctxRole).(string); role != "superadmin" {
			errorJSON(r, w, http.StatusForbidden, "admin.forbidden", "this operation requires the superadmin role")
			return
		}
		next(w, r)
	})
	return wrapped
}

// --- login hardening (ADR-0001 §8.3) ---

type loginLimiter struct {
	mu     sync.Mutex
	window map[string]*loginWindow
}

type loginWindow struct {
	count   int
	resetAt time.Time
}

// loginLimiterMaxEntries bounds the window map. Keys are ip|username pairs
// created per attempt and previously never removed once the key stopped
// logging in, so broad fake-login traffic grew the map forever. When a fresh
// entry pushes the map past the bound, a sweep evicts every expired window;
// returning keys are reset lazily on access.
const loginLimiterMaxEntries = 1024

func newLoginLimiter() *loginLimiter { return &loginLimiter{window: map[string]*loginWindow{}} }

func (l *loginLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.window[key]
	if !ok || now.After(w.resetAt) {
		l.window[key] = &loginWindow{count: 1, resetAt: now.Add(time.Minute)}
		if len(l.window) > loginLimiterMaxEntries {
			l.sweepExpiredLocked(now)
		}
		return true
	}
	w.count++
	return w.count <= 10
}

// sweepExpiredLocked deletes every expired window; the caller holds l.mu.
func (l *loginLimiter) sweepExpiredLocked(now time.Time) {
	for k, w := range l.window {
		if now.After(w.resetAt) {
			delete(l.window, k)
		}
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "username and password required")
		return
	}
	ip := clientIP(r)
	if !s.limiter.allow(ip + "|" + strings.ToLower(req.Username)) {
		s.auditSimple(r.Context(), 0, req.Username, "auth.login_rate_limited", "", "")
		errorJSON(r, w, http.StatusTooManyRequests, "admin.rate_limited", "too many login attempts; wait a minute")
		return
	}

	var u struct {
		id        int64
		hash      string
		status    string
		failed    int64
		lockUntil sql.NullTime
	}
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id, password_hash, status, failed_login_count, locked_until FROM users WHERE username = ?`,
		req.Username).Scan(&u.id, &u.hash, &u.status, &u.failed, &u.lockUntil)
	if err == sql.ErrNoRows {
		// Uniform failure response; burn a hash comparison to equalize timing.
		_, _ = VerifyPassword(req.Password, "$argon2id$v=19$m=65536,t=1,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		s.auditSimple(r.Context(), 0, req.Username, "auth.login_failed", "", ip)
		errorJSON(r, w, http.StatusUnauthorized, "admin.invalid_credentials", "invalid username or password")
		return
	}
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	now := time.Now()
	if u.status != "active" || (u.lockUntil.Valid && now.Before(u.lockUntil.Time)) {
		// Locked or disabled account: the user-facing message stays the uniform
		// invalid-credentials answer (no account enumeration), but the attempt
		// is still audited like every other failed login.
		s.auditSimple(r.Context(), u.id, req.Username, "auth.login_failed", "", ip)
		errorJSON(r, w, http.StatusUnauthorized, "admin.invalid_credentials", "invalid username or password")
		return
	}
	ok, verr := VerifyPassword(req.Password, u.hash)
	if verr != nil || !ok {
		failed := u.failed + 1
		lock := sql.NullBool{Valid: false}
		lockUntil := sql.NullTime{}
		lockMinutes := 0
		if failed >= 5 {
			lockMinutes = 15 << min64(failed-5, 4) // 15, 30, 60, 120, 240... capped
			lockUntil = sql.NullTime{Time: now.Add(time.Duration(lockMinutes) * time.Minute), Valid: true}
		}
		_, _ = s.db.ExecContext(r.Context(),
			`UPDATE users SET failed_login_count = ?, locked_until = ? WHERE id = ?`, failed, lockUntil, u.id)
		_ = lock
		s.auditSimple(r.Context(), u.id, req.Username, "auth.login_failed", "", ip)
		errorJSON(r, w, http.StatusUnauthorized, "admin.invalid_credentials", "invalid username or password")
		return
	}
	// Success: reset counters, create session.
	token, err := newToken()
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	now2 := time.Now()
	absolute := now2.Add(sessionAbsoluteExpiry)
	sliding := now2.Add(sessionSlidingWindow)
	if _, err := s.db.ExecContext(r.Context(),
		`UPDATE users SET failed_login_count = 0, locked_until = NULL, last_login_at = ?, last_login_ip = ? WHERE id = ?`,
		now2, ip, u.id); err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	if _, err := s.db.ExecContext(r.Context(),
		`INSERT INTO sessions (user_id, token_hash, expires_at, absolute_expires_at, ip, user_agent) VALUES (?, ?, ?, ?, ?, ?)`,
		u.id, hashToken(token), sliding, absolute, ip, r.UserAgent()); err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/",
		MaxAge:   int(sessionAbsoluteExpiry.Seconds()), // persists across browser restarts; the server still enforces sliding + absolute expiry
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	s.auditSimple(r.Context(), u.id, req.Username, "auth.login", "", ip)
	var mustChange bool
	_ = s.db.QueryRowContext(r.Context(), `SELECT must_change_password FROM users WHERE id = ?`, u.id).Scan(&mustChange)
	writeJSON(w, http.StatusOK, map[string]any{"username": req.Username, "must_change_password": mustChange})
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// tokenRand is the session-token randomness source; a variable so tests can
// inject a crypto/rand failure. Issuing a predictable (all-zero) token on RNG
// failure would be a session-fixation hazard, so the error is propagated and
// the login fails closed instead.
var tokenRand = rand.Read

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := tokenRand(b); err != nil {
		return "", fmt.Errorf("session token randomness: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		_, _ = s.db.ExecContext(r.Context(),
			`UPDATE sessions SET revoked_at = NOW() WHERE token_hash = ?`, hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"username":             r.Context().Value(ctxUser),
		"role":                 r.Context().Value(ctxRole),
		"must_change_password": s.mustChange(r),
	})
}

func (s *Server) mustChange(r *http.Request) bool {
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var m bool
	_ = s.db.QueryRowContext(r.Context(), `SELECT must_change_password FROM users WHERE id = ?`, uid).Scan(&m)
	return m
}

// handlePasswordChange serves both flows (ADR-0001 §8.4): the forced
// first-login change skips the current-password check — the caller just
// authenticated and the account is flagged must_change_password — while a
// voluntary change must present the current password.
func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var req struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "next (and current unless forced change) required")
		return
	}
	if len(req.Next) < 10 {
		errorJSON(r, w, http.StatusBadRequest, "admin.weak_password", "password must be at least 10 characters")
		return
	}
	var hash string
	var mustChange bool
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT password_hash, must_change_password FROM users WHERE id = ?`, uid).Scan(&hash, &mustChange); err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	if req.Current == "" {
		if !mustChange {
			errorJSON(r, w, http.StatusBadRequest, "admin.current_required", "current password is required")
			return
		}
		// Forced first-login change: session already authenticated this user.
	} else if ok, _ := VerifyPassword(req.Current, hash); !ok {
		errorJSON(r, w, http.StatusUnauthorized, "admin.invalid_credentials", "current password incorrect")
		return
	}
	newHash, err := HashPassword(req.Next)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	if _, err := s.db.ExecContext(r.Context(),
		`UPDATE users SET password_hash = ?, must_change_password = 0 WHERE id = ?`, newHash, uid); err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	// Invalidate all other sessions of this user (keep the current one).
	if c, cerr := r.Cookie(cookieName); cerr == nil {
		keep := hashToken(c.Value)
		_, _ = s.db.ExecContext(r.Context(),
			`UPDATE sessions SET revoked_at = NOW() WHERE user_id = ? AND token_hash <> ?`, uid, keep)
	}
	s.auditSimple(r.Context(), uid, r.Context().Value(ctxUser).(string), "user.password_changed", "", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]string{"status": "password_changed"})
}

// auditSimple records an audit row without before/after payloads. The
// middleware request ID (when present in ctx) lands in admin_audit.request_id.
func (s *Server) auditSimple(ctx context.Context, actorID int64, actor, action, resource, ip string) {
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO admin_audit (actor_id, actor_name, action, resource_type, resource_id, request_id, ip) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		actorID, actor, action, resourceTypeOf(action), resource, requestIDFromContext(ctx), ip)
}

// requestIDFromContext extracts the request ID from a context; unlike
// requestID it does not need the *http.Request (audit helpers only carry ctx).
func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

func resourceTypeOf(action string) string {
	switch {
	case strings.HasPrefix(action, "auth."):
		return "auth"
	case strings.HasPrefix(action, "route"):
		return "route"
	case strings.HasPrefix(action, "policy"):
		return "policy"
	case strings.HasPrefix(action, "identity"):
		return "identity"
	case strings.HasPrefix(action, "user"):
		return "user"
	case strings.HasPrefix(action, "alert.channel"):
		return "alert_channel"
	case strings.HasPrefix(action, "alert.rule"):
		return "alert_rule"
	case strings.HasPrefix(action, "debug."):
		return "debug_log"
	case strings.HasPrefix(action, "llm."):
		return "llm_endpoint"
	case strings.HasPrefix(action, "assistant."):
		return "assistant_session"
	default:
		return ""
	}
}

// Revision check helper shared by all write endpoints (optimistic concurrency,
// ADR-0001 §7.1): reads the current revision inside the caller's transaction.
func revisionMismatch(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, expected int64) (bool, int64, error) {
	var cur int64
	err := q.QueryRowContext(ctx,
		`SELECT CAST(JSON_EXTRACT(value,'$') AS UNSIGNED) FROM settings WHERE name='config_revision'`).Scan(&cur)
	if err != nil {
		return false, 0, err
	}
	return expected > 0 && expected != cur, cur, nil
}
