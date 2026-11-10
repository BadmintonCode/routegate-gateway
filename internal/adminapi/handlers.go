package adminapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/alerts"
	"github.com/BadmintonCode/routegate-gateway/internal/authz"
	"github.com/BadmintonCode/routegate-gateway/internal/identity"
	"github.com/BadmintonCode/routegate-gateway/internal/proxy"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
	"github.com/go-sql-driver/mysql"
)

// --- write transaction (three-in-one: row + revision + audit, ADR §7.1) ---

// MySQL server error numbers used by the writeTxError classification (the
// go-sql-driver package does not export them as constants).
const (
	mysqlErrDupEntry     = 1062 // duplicate key
	mysqlErrLockDeadlock = 1213 // deadlock found
	mysqlErrLockWaitTO   = 1205 // lock wait timeout
)

type conflictError struct{ current int64 }

// conflictMessage renders the stable user-facing sentence for a revision
// conflict; the envelope never reads err.Error() so the wording cannot drift
// with internal wrapping.
func conflictMessage(current int64) string {
	return fmt.Sprintf("config changed concurrently (current revision %d); refresh and retry", current)
}

func (e *conflictError) Error() string {
	return conflictMessage(e.current)
}

// writeTx runs the three-in-one write transaction (business row +
// config_revision bump + admin_audit row). ip and requestID land in the audit
// row so every mutating action is attributable; callers pass clientIP(r) and
// requestID(r).
func (s *Server) writeTx(
	ctx context.Context,
	ip, requestID string,
	actorID int64, actorName, action, resourceType, resourceID string,
	expectedRevision int64,
	before, after any,
	fn func(tx *sql.Tx) error,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if mismatch, cur, err := revisionMismatch(ctx, tx, expectedRevision); err != nil {
		return err
	} else if mismatch {
		return &conflictError{current: cur}
	}

	if err := fn(tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE settings SET value = CAST(CAST(JSON_EXTRACT(value,'$') AS UNSIGNED) + 1 AS JSON) WHERE name='config_revision'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO admin_audit (actor_id, actor_name, action, resource_type, resource_id, before_state, after_state, request_id, ip)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		actorID, actorName, action, resourceType, resourceID, jsonOrNull(before), jsonOrNull(after), requestID, ip); err != nil {
		return err
	}
	return tx.Commit()
}

func jsonOrNull(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}

// writeTxError renders a write-transaction failure per the unified error
// contract: 409 for conflicts and duplicates, 404 for missing rows, 500 with
// stable friendly messages for infrastructure failures. Raw driver/DSN/Go
// error text never reaches the response — the details go to the server log.
func writeTxError(r *http.Request, w http.ResponseWriter, err error) {
	var c *conflictError
	var me *mysql.MySQLError
	var opErr *net.OpError
	switch {
	case asConflict(err, &c):
		body := errorEnvelope(r, "admin.revision_conflict", conflictMessage(c.current))
		body["config_revision"] = c.current
		writeJSON(w, http.StatusConflict, body)
	case errors.Is(err, sql.ErrNoRows):
		errorJSON(r, w, http.StatusNotFound, "admin.not_found", "resource not found")
	case errors.As(err, &me) && me.Number == mysqlErrDupEntry: // 1062
		errorJSON(r, w, http.StatusConflict, "admin.duplicate", "a record with the same unique value already exists")
	case errors.As(err, &me) && (me.Number == mysqlErrLockDeadlock || me.Number == mysqlErrLockWaitTO): // deadlock / lock wait timeout
		log.Printf("admin write failed (deadlock/lock timeout): %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.write_failed", "the write could not complete due to concurrent activity, retry")
	case errors.Is(err, driver.ErrBadConn),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled),
		errors.As(err, &opErr):
		log.Printf("admin write failed (storage unavailable): %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.storage_unavailable", "the configuration store is temporarily unavailable")
	default:
		log.Printf("admin write failed: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.write_failed", "the write failed, see server logs")
	}
}

func asConflict(err error, target **conflictError) bool {
	if c, ok := err.(*conflictError); ok {
		*target = c
		return true
	}
	return false
}

func mustAffectOne(res sql.Result, err error) error {
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

// --- routes ---

type routePayload struct {
	Name           string          `json:"name"`
	PathPrefix     string          `json:"path_prefix"`
	Methods        []string        `json:"methods,omitempty"`
	TargetsJSON    json.RawMessage `json:"targets"`
	StripPrefix    bool            `json:"strip_prefix"`
	Remark         string          `json:"remark"`
	SuccessRuleRaw json.RawMessage `json:"success_rule,omitempty"`
	BizCodePath    string          `json:"biz_code_path,omitempty"`
	BizMessagePath string          `json:"biz_message_path,omitempty"`
	ExpectedRev    int64           `json:"expected_revision,omitempty"`

	SuccessRule *snapshot.SuccessRule `json:"-"`
}

func (p *routePayload) validate(ownProxyPort int) error {
	if p.Name == "" || len(p.Name) > 128 {
		return fmt.Errorf("name is required (max 128 chars)")
	}
	targets, verr := store.ValidateRoute(p.PathPrefix, p.TargetsJSON, ownProxyPort)
	if verr != nil {
		return verr
	}
	if len(targets) == 0 {
		return fmt.Errorf("at least one target is required")
	}
	rule, rerr := store.ParseSuccessRule(p.SuccessRuleRaw)
	if rerr != nil {
		return rerr
	}
	p.SuccessRule = rule
	p.BizCodePath = strings.TrimSpace(p.BizCodePath)
	if p.BizCodePath != "" {
		if err := store.ValidateBodyPath(p.BizCodePath); err != nil {
			return fmt.Errorf("biz_code_path: %w", err)
		}
	}
	p.BizMessagePath = strings.TrimSpace(p.BizMessagePath)
	if p.BizMessagePath != "" {
		if err := store.ValidateBodyPath(p.BizMessagePath); err != nil {
			return fmt.Errorf("biz_message_path: %w", err)
		}
	}
	return nil
}

// nullIfEmpty maps an optional path to a nullable column value.
func nullIfEmpty(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

// successRuleColumn renders the rule for the routes INSERT/UPDATE: NULL when
// absent, the normalized JSON otherwise.
func (p *routePayload) successRuleColumn() any {
	if p.SuccessRule == nil {
		return nil
	}
	b, err := json.Marshal(p.SuccessRule)
	if err != nil {
		return nil
	}
	return string(b)
}

func methodsToString(methods []string) sql.NullString {
	if len(methods) == 0 {
		return sql.NullString{}
	}
	up := make([]string, 0, len(methods))
	for _, m := range methods {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m != "" {
			up = append(up, m)
		}
	}
	if len(up) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: strings.Join(up, ","), Valid: true}
}

func (s *Server) handleListRoutes(w http.ResponseWriter, r *http.Request) {
	items, err := s.loadRoutes(r.Context())
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// loadRoutes reads the full routes list. Shared by the list endpoint and
// handleGatewayConfig so a DB failure surfaces as a 500 envelope instead of
// being silently rendered as an empty document.
func (s *Server) loadRoutes(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, path_prefix, methods, targets, strip_prefix, remark, success_rule, biz_code_path, biz_message_path,
		        (SELECT COUNT(*) FROM route_operations ro WHERE ro.route_id = routes.id) AS ops_count
		 FROM routes ORDER BY path_prefix`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		m, err := routeFromRow(rows.Scan)
		if err != nil {
			continue
		}
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func routeFromRow(scan func(dest ...any) error) (map[string]any, error) {
	var id int64
	var name, prefix, remark string
	var methods sql.NullString
	var targets []byte
	var strip bool
	var opsCount int
	var ruleJSON []byte
	var bizCode, bizMsg sql.NullString
	if err := scan(&id, &name, &prefix, &methods, &targets, &strip, &remark, &ruleJSON, &bizCode, &bizMsg, &opsCount); err != nil {
		return nil, err
	}
	var methodList []string
	if methods.Valid && methods.String != "" {
		methodList = strings.Split(methods.String, ",")
	}
	return map[string]any{
		"id": id, "name": name, "path_prefix": prefix,
		"methods": methodList, "targets": json.RawMessage(targets),
		"ops_count":    opsCount,
		"strip_prefix": strip, "remark": remark,
		"success_rule":  json.RawMessage(ruleJSON),
		"biz_code_path": bizCode.String, "biz_message_path": bizMsg.String,
	}, nil
}

func (s *Server) routeByID(r *http.Request, id int64) map[string]any {
	row := s.db.QueryRowContext(r.Context(),
		`SELECT id, name, path_prefix, methods, targets, strip_prefix, remark,
		        (SELECT COUNT(*) FROM route_operations ro WHERE ro.route_id = routes.id)
		 FROM routes WHERE id=?`, id)
	m, err := routeFromRow(row.Scan)
	if err != nil {
		return nil
	}
	return m
}

func (s *Server) handleCreateRoute(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var p routePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	if err := p.validate(s.cfg.OwnProxyPort); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_route", err.Error())
		return
	}
	methods := methodsToString(p.Methods)
	err := s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "route.create", "route", p.Name, p.ExpectedRev, nil, p,
		func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(),
				`INSERT INTO routes (name, path_prefix, methods, targets, strip_prefix, remark, success_rule, biz_code_path, biz_message_path) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				p.Name, p.PathPrefix, methods, []byte(p.TargetsJSON), p.StripPrefix, p.Remark, p.successRuleColumn(), nullIfEmpty(p.BizCodePath), nullIfEmpty(p.BizMessagePath))
			return err
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "name": p.Name})
}

func (s *Server) handleUpdateRoute(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var p routePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	if err := p.validate(s.cfg.OwnProxyPort); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_route", err.Error())
		return
	}
	before := s.routeByID(r, id)
	methods := methodsToString(p.Methods)
	err := s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "route.update", "route", strconv.FormatInt(id, 10), p.ExpectedRev, before, p,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(r.Context(),
				`UPDATE routes SET name=?, path_prefix=?, methods=?, targets=?, strip_prefix=?, remark=?, success_rule=?, biz_code_path=?, biz_message_path=? WHERE id=?`,
				p.Name, p.PathPrefix, methods, []byte(p.TargetsJSON), p.StripPrefix, p.Remark, p.successRuleColumn(), nullIfEmpty(p.BizCodePath), nullIfEmpty(p.BizMessagePath), id)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated"})
}

func (s *Server) handleDeleteRoute(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		ExpectedRev int64 `json:"expected_revision"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	before := s.routeByID(r, id)
	err := s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "route.delete", "route", strconv.FormatInt(id, 10), body.ExpectedRev, before, nil,
		func(tx *sql.Tx) error {
			// Route-scoped policies and registered operations die with the route.
			if _, err := tx.ExecContext(r.Context(), `DELETE FROM policies WHERE route_id=?`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.Context(), `DELETE FROM route_operations WHERE route_id=?`, id); err != nil {
				return err
			}
			res, err := tx.ExecContext(r.Context(), `DELETE FROM routes WHERE id=?`, id)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// --- policies ---

type policyPayload struct {
	RouteID     int64  `json:"route_id"`
	Caller      string `json:"caller"`
	Method      string `json:"method"`
	PathPattern string `json:"path_pattern"`
	Effect      string `json:"effect"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	Remark      string `json:"remark"`
	ExpectedRev int64  `json:"expected_revision,omitempty"`
}

func (s *Server) validatePolicyPayload(r *http.Request, p *policyPayload) error {
	if p.Caller == "" || p.Effect == "" {
		return fmt.Errorf("caller and effect are required")
	}
	if p.Effect != "allow" && p.Effect != "deny" {
		return fmt.Errorf("effect must be allow or deny")
	}
	if p.Method == "" {
		p.Method = "*"
	}
	if p.Method != "*" {
		switch strings.ToUpper(p.Method) {
		case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS":
			p.Method = strings.ToUpper(p.Method)
		default:
			return fmt.Errorf("method must be * or a known HTTP method")
		}
	}
	if p.PathPattern == "" {
		p.PathPattern = "/**"
	}
	if _, err := authz.ParsePattern(p.PathPattern); err != nil {
		return err
	}
	// Write-time normalization rule (store.ValidateNormalizedPath): a pattern
	// that differs from its normalized form could never match a normalized
	// request path, so reject it with the shared friendly message.
	if err := store.ValidateNormalizedPath("path pattern", p.PathPattern); err != nil {
		return err
	}
	var routeExists int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM routes WHERE id=?`, p.RouteID).Scan(&routeExists); err != nil || routeExists == 0 {
		return fmt.Errorf("route %d does not exist", p.RouteID)
	}
	if (p.Caller == "*" || p.PathPattern == "/**") && p.ExpiresAt == "" {
		return fmt.Errorf("wide rules (caller='*' or pattern '/**') require expires_at (ADR §6.1)")
	}
	if p.Caller != "*" {
		var n int
		if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM identities WHERE name=? AND status='active'`, p.Caller).Scan(&n); err != nil || n == 0 {
			return fmt.Errorf("caller %q is not an active identity", p.Caller)
		}
	}
	return nil
}

func policyExpires(p *policyPayload) (sql.NullTime, error) {
	if p.ExpiresAt == "" {
		return sql.NullTime{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, p.ExpiresAt); err == nil {
			return sql.NullTime{Time: t, Valid: true}, nil
		}
	}
	return sql.NullTime{}, fmt.Errorf("expires_at must be RFC3339, 'YYYY-MM-DD HH:MM:SS', or 'YYYY-MM-DD'")
}

func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	items, err := s.loadPolicies(r.Context())
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// policyFromRow maps one policies JOIN routes row into the panel item shape.
func policyFromRow(scan func(dest ...any) error) (map[string]any, error) {
	var id, routeID int64
	var routeName, caller, method, pattern, effect, remark string
	var enabled bool
	var expires sql.NullTime
	if err := scan(&id, &routeID, &routeName, &caller, &method, &pattern, &effect, &expires, &enabled, &remark); err != nil {
		return nil, err
	}
	item := map[string]any{
		"id": id, "route_id": routeID, "route": routeName, "caller": caller,
		"method": method, "path_pattern": pattern, "effect": effect,
		"enabled": enabled, "remark": remark,
	}
	if expires.Valid {
		item["expires_at"] = expires.Time.UTC().Format(time.RFC3339)
		item["expiring"] = expires.Time.Before(time.Now().Add(7 * 24 * time.Hour))
	}
	return item, nil
}

// loadPolicies reads the full policy list; shared with handleGatewayConfig
// (same rationale as loadRoutes).
func (s *Server) loadPolicies(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.route_id, r.name, p.caller, p.method, p.path_pattern, p.effect, p.expires_at, p.enabled, p.remark
		 FROM policies p JOIN routes r ON r.id = p.route_id ORDER BY p.route_id, p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		item, err := policyFromRow(rows.Scan)
		if err != nil {
			continue
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Server) handleCreatePolicy(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var p policyPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	if err := s.validatePolicyPayload(r, &p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_policy", err.Error())
		return
	}
	expires, err := policyExpires(&p)
	if err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_policy", err.Error())
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "policy.create", "policy", strconv.FormatInt(p.RouteID, 10), p.ExpectedRev, nil, p,
		func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(),
				`INSERT INTO policies (route_id, caller, method, path_pattern, effect, expires_at, enabled, created_by, remark)
				 VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
				p.RouteID, p.Caller, p.Method, p.PathPattern, p.Effect, expires, uid, p.Remark)
			return err
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created"})
}

func (s *Server) handleUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var p policyPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	if err := s.validatePolicyPayload(r, &p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_policy", err.Error())
		return
	}
	expires, err := policyExpires(&p)
	if err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_policy", err.Error())
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "policy.update", "policy", strconv.FormatInt(id, 10), p.ExpectedRev, nil, p,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(r.Context(),
				`UPDATE policies SET route_id=?, caller=?, method=?, path_pattern=?, effect=?, expires_at=?, remark=? WHERE id=?`,
				p.RouteID, p.Caller, p.Method, p.PathPattern, p.Effect, expires, p.Remark, id)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated"})
}

// policyByID loads one policy row (with its route name) for the delete audit
// before-state, mirroring routeByID; nil when the row is missing or unreadable
// (the delete inside the transaction then fails with the 404 mapping).
func (s *Server) policyByID(r *http.Request, id int64) map[string]any {
	row := s.db.QueryRowContext(r.Context(),
		`SELECT p.id, p.route_id, r.name, p.caller, p.method, p.path_pattern, p.effect, p.expires_at, p.enabled, p.remark
		 FROM policies p JOIN routes r ON r.id = p.route_id WHERE p.id=?`, id)
	m, err := policyFromRow(row.Scan)
	if err != nil {
		return nil
	}
	return m
}

func (s *Server) handleDeletePolicy(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		ExpectedRev int64 `json:"expected_revision"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	before := s.policyByID(r, id)
	err := s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "policy.delete", "policy", strconv.FormatInt(id, 10), body.ExpectedRev, before, nil,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(r.Context(), `DELETE FROM policies WHERE id=?`, id)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

func (s *Server) handleExpiringPolicies(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT p.id, r.name, p.caller, p.path_pattern, p.effect, p.expires_at
		 FROM policies p JOIN routes r ON r.id = p.route_id
		 WHERE p.enabled=1 AND (p.expires_at IS NOT NULL OR p.caller='*' OR p.path_pattern='/**')
		 ORDER BY p.expires_at ASC LIMIT 100`)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var route, caller, pattern, effect string
		var expires sql.NullTime
		if err := rows.Scan(&id, &route, &caller, &pattern, &effect, &expires); err != nil {
			continue
		}
		item := map[string]any{"id": id, "route": route, "caller": caller, "path_pattern": pattern, "effect": effect}
		if expires.Valid {
			item["expires_at"] = expires.Time.UTC().Format(time.RFC3339)
			item["expiring"] = expires.Time.Before(time.Now().Add(7 * 24 * time.Hour))
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handlePolicyTest is the panel's policy tester (ADR-0001 §7.2): it runs the
// same evaluation path as the live data plane against the current snapshot.
func (s *Server) handlePolicyTest(w http.ResponseWriter, r *http.Request) {
	var req struct{ Caller, Method, Path string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Caller == "" || req.Path == "" {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "caller and path are required")
		return
	}
	if req.Method == "" {
		req.Method = "GET"
	}
	snap := s.holder.Load()
	route, decision, policyID, sub := proxy.TestAccess(snap, req.Caller, strings.ToUpper(req.Method), req.Path)
	body := map[string]any{
		"route": route, "decision": decision, "matched_policy_id": policyID,
		"sub_path": sub,
	}
	if snap != nil {
		body["config_revision"] = snap.Revision
	}
	writeJSON(w, http.StatusOK, body)
}

// --- identities ---

type identityPayload struct {
	Name        string `json:"name"`
	PEM         string `json:"pem"`
	Remark      string `json:"remark"`
	ExpectedRev int64  `json:"expected_revision,omitempty"`
}

func parseCertPEMString(pemStr string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no CERTIFICATE PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}

func (s *Server) handleCreateIdentity(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var p identityPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	if p.Name == "" || len(p.Name) > 128 {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "name is required (max 128 chars)")
		return
	}
	cert, err := parseCertPEMString(p.PEM)
	if err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_certificate", err.Error())
		return
	}
	if err := checkSANName(cert, p.Name); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_certificate", err.Error())
		return
	}
	fp := identity.Fingerprint(cert)
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "identity.create", "identity", p.Name, p.ExpectedRev, nil,
		map[string]any{"name": p.Name, "key_id": fp, "not_after": cert.NotAfter.UTC()},
		func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(),
				`INSERT INTO identities (name, key_id, credential_type, public_material, source, status, not_before, not_after, created_by, remark)
				 VALUES (?, ?, 'mtls_cert', ?, 'upload', 'active', ?, ?, ?, ?)`,
				p.Name, fp, strings.TrimSpace(p.PEM), cert.NotBefore.UTC(), cert.NotAfter.UTC(), uid, p.Remark)
			return err
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "key_id": fp})
}

// checkSANName enforces the ADR §6.3.2 rule: a URI SAN bound to
// routegate.local/identities/<x> must match the registered name.
func checkSANName(cert *x509.Certificate, name string) error {
	for _, san := range cert.URIs {
		if san.Scheme == "https" && san.Host == "routegate.local" &&
			strings.HasPrefix(san.Path, "/identities/") && len(san.Path) > len("/identities/") {
			if san.Path[len("/identities/"):] != name {
				return fmt.Errorf("certificate SAN identity %q does not match name %q", san.Path[len("/identities/"):], name)
			}
		}
	}
	return nil
}

func (s *Server) handleListIdentities(w http.ResponseWriter, r *http.Request) {
	items, err := s.loadIdentities(r.Context())
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// identityFromRow maps one identities row into the panel item shape.
func identityFromRow(scan func(dest ...any) error) (map[string]any, error) {
	var id int64
	var name, keyID, ctype, source, status, reason, remark string
	var nb, na, revoked sql.NullTime
	var policyCount int
	if err := scan(&id, &name, &keyID, &ctype, &source, &status, &nb, &na, &revoked, &reason, &remark, &policyCount); err != nil {
		return nil, err
	}
	item := map[string]any{
		"id": id, "name": name, "key_id": keyID, "credential_type": ctype,
		"source": source, "status": status, "policy_count": policyCount,
		"revoke_reason": reason, "remark": remark,
	}
	if nb.Valid {
		item["not_before"] = nb.Time.UTC().Format(time.RFC3339)
	}
	if na.Valid {
		item["not_after"] = na.Time.UTC().Format(time.RFC3339)
	}
	if revoked.Valid {
		item["revoked_at"] = revoked.Time.UTC().Format(time.RFC3339)
	}
	return item, nil
}

// loadIdentities reads the full identity list; shared with handleGatewayConfig
// (same rationale as loadRoutes).
func (s *Server) loadIdentities(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT i.id, i.name, i.key_id, i.credential_type, i.source, i.status, i.not_before, i.not_after, i.revoked_at, i.revoke_reason, i.remark,
		        (SELECT COUNT(*) FROM policies p WHERE p.caller = i.name AND p.enabled = 1)
		 FROM identities i ORDER BY i.name, i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		item, err := identityFromRow(rows.Scan)
		if err != nil {
			continue
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Server) handleValidateIdentity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PEM string `json:"pem"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	cert, err := parseCertPEMString(req.PEM)
	if err != nil {
		// Soft-fail 200 domain probe (panel route-pattern/cert tester). The
		// error is pure PEM/x509 parsing of user input — no store or driver
		// text can reach this response.
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid": true, "subject": cert.Subject.String(),
		"key_id":     identity.Fingerprint(cert),
		"not_before": cert.NotBefore.UTC().Format(time.RFC3339),
		"not_after":  cert.NotAfter.UTC().Format(time.RFC3339),
	})
}

// handleGenerateIdentity is the server-side generation path (ADR §6.3.2 B,
// backup option): the private key is returned exactly once and never stored.
// The panel prefers browser-side generation (WebCrypto), which never sends a
// private key to the server at all.
func (s *Server) handleGenerateIdentity(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var req struct {
		Name        string `json:"name"`
		Days        int    `json:"days"`
		Remark      string `json:"remark"`
		ExpectedRev int64  `json:"expected_revision,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "name is required")
		return
	}
	if req.Days <= 0 || req.Days > 365 {
		req.Days = 90
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "key generation failed")
		return
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: req.Name, Organization: []string{"routegate"}},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().AddDate(0, 0, req.Days),
		URIs:         parseSANURL(req.Name),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "certificate creation failed")
		return
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "certificate parse failed")
		return
	}
	fp := identity.Fingerprint(cert)
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "key encoding failed")
		return
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))

	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "identity.generate", "identity", req.Name, req.ExpectedRev, nil,
		map[string]any{"name": req.Name, "key_id": fp, "not_after": cert.NotAfter.UTC()},
		func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(),
				`INSERT INTO identities (name, key_id, credential_type, public_material, source, status, not_before, not_after, created_by, remark)
				 VALUES (?, ?, 'mtls_cert', ?, 'generated', 'active', ?, ?, ?, ?)`,
				req.Name, fp, certPEM, cert.NotBefore.UTC(), cert.NotAfter.UTC(), uid, req.Remark)
			return err
		})
	if err != nil {
		// Insert may conflict on (name, key_id) — vanishingly unlikely (random key).
		writeTxError(r, w, err)
		return
	}
	// One-time response: the private key crosses the wire once and is never
	// persisted, logged, or audited (audit carries only the fingerprint).
	writeJSON(w, http.StatusCreated, map[string]any{
		"status": "generated", "name": req.Name, "key_id": fp,
		"private_key": keyPEM, "certificate": certPEM,
		"not_after": cert.NotAfter.UTC().Format(time.RFC3339),
	})
}

func parseSANURL(name string) []*url.URL {
	u, err := url.Parse("https://routegate.local/identities/" + name)
	if err != nil {
		return nil
	}
	return []*url.URL{u}
}

func (s *Server) handleRevokeIdentity(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Reason      string `json:"reason"`
		ExpectedRev int64  `json:"expected_revision"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	err := s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "identity.revoke", "identity", strconv.FormatInt(id, 10), body.ExpectedRev, nil,
		map[string]any{"reason": body.Reason},
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(r.Context(),
				`UPDATE identities SET status='revoked', revoked_at=NOW(), revoke_reason=? WHERE id=? AND status='active'`,
				body.Reason, id)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked"})
}

func (s *Server) handleDeleteIdentity(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		ExpectedRev int64 `json:"expected_revision"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	err := s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "identity.delete", "identity", strconv.FormatInt(id, 10), body.ExpectedRev, nil, nil,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(r.Context(), `DELETE FROM identities WHERE id=?`, id)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// --- users ---

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, username, display_name, role, status, must_change_password, last_login_at FROM users ORDER BY id`)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var username, display, role, status string
		var mustChange bool
		var lastLogin sql.NullTime
		if err := rows.Scan(&id, &username, &display, &role, &status, &mustChange, &lastLogin); err != nil {
			continue
		}
		item := map[string]any{
			"id": id, "username": username, "display_name": display, "role": role,
			"status": status, "must_change_password": mustChange,
		}
		if lastLogin.Valid {
			item["last_login_at"] = lastLogin.Time.UTC().Format(time.RFC3339)
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var req struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	if req.Username == "" || len(req.Username) > 128 {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "username required (max 128 chars)")
		return
	}
	if len(req.Password) < 10 {
		errorJSON(r, w, http.StatusBadRequest, "admin.weak_password", "password must be at least 10 characters")
		return
	}
	if req.Role != "superadmin" && req.Role != "user" {
		req.Role = "user"
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "user.create", "user", req.Username, 0, nil,
		map[string]any{"username": req.Username, "role": req.Role},
		func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(),
				`INSERT INTO users (username, password_hash, display_name, role, must_change_password) VALUES (?, ?, ?, ?, 1)`,
				req.Username, hash, req.DisplayName, req.Role)
			return err
		})
	if err != nil {
		if isDuplicateKey(err) {
			errorJSON(r, w, http.StatusConflict, "admin.duplicate", "username already exists")
			return
		}
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "username": req.Username})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var req struct {
		Status      *string `json:"status,omitempty"`
		Role        *string `json:"role,omitempty"`
		NewPassword *string `json:"new_password,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	if id == uid && (req.Status != nil || req.Role != nil) {
		errorJSON(r, w, http.StatusBadRequest, "admin.self_lockout", "cannot change your own role or status")
		return
	}
	if req.NewPassword != nil && len(*req.NewPassword) < 10 {
		errorJSON(r, w, http.StatusBadRequest, "admin.weak_password", "password must be at least 10 characters")
		return
	}
	sets := map[string]any{}
	if req.Status != nil {
		if *req.Status != "active" && *req.Status != "disabled" {
			errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "status must be active or disabled")
			return
		}
		sets["status"] = *req.Status
	}
	if req.Role != nil {
		if *req.Role != "superadmin" && *req.Role != "user" {
			errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "role must be superadmin or user")
			return
		}
		sets["role"] = *req.Role
	}
	if req.NewPassword != nil {
		hash, err := HashPassword(*req.NewPassword)
		if err != nil {
			log.Printf("admin api error: %v", err)
			errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
			return
		}
		sets["password_hash"] = hash
		sets["must_change_password"] = true
	}
	if len(sets) == 0 {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "nothing to update")
		return
	}
	err := s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "user.update", "user", strconv.FormatInt(id, 10), 0, nil, sets,
		func(tx *sql.Tx) error {
			assigns, args := []string{}, []any{}
			for col, val := range sets {
				assigns = append(assigns, col+"=?")
				args = append(args, val)
			}
			args = append(args, id)
			res, err := tx.ExecContext(r.Context(), `UPDATE users SET `+strings.Join(assigns, ", ")+` WHERE id=?`, args...)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	// Disabling a user revokes their sessions immediately.
	if req.Status != nil && *req.Status == "disabled" {
		_, _ = s.db.ExecContext(r.Context(),
			`UPDATE sessions s JOIN users u ON u.id = s.user_id SET s.revoked_at = NOW() WHERE u.id = ?`, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated"})
}

// --- audit ---

func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	page, pageSize := pageParams(r)
	offset := (page - 1) * pageSize
	where, args := []string{"1=1"}, []any{}
	if a := r.URL.Query().Get("actor"); a != "" {
		where = append(where, "actor_name = ?")
		args = append(args, a)
	}
	if a := r.URL.Query().Get("action"); a != "" {
		where = append(where, "action LIKE ?")
		args = append(args, a+"%")
	}
	whereSQL := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM admin_audit WHERE `+whereSQL, args...).Scan(&total); err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	q := `SELECT id, actor_id, actor_name, action, resource_type, resource_id, before_state, after_state, ip, created_at
	      FROM admin_audit WHERE ` + whereSQL + ` ORDER BY id DESC LIMIT ? OFFSET ?`
	args = append(args, pageSize, offset)
	rows, err := s.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, actorID int64
		var actor, action, rtype, rid, ip string
		var before, after sql.NullString
		var created time.Time
		if err := rows.Scan(&id, &actorID, &actor, &action, &rtype, &rid, &before, &after, &ip, &created); err != nil {
			continue
		}
		item := map[string]any{
			"id": id, "actor_id": actorID, "actor": actor, "action": action,
			"resource_type": rtype, "resource_id": rid, "ip": ip,
			"created_at": created.UTC().Format(time.RFC3339),
		}
		if before.Valid {
			item["before"] = json.RawMessage(before.String)
		}
		if after.Valid {
			item["after"] = json.RawMessage(after.String)
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func (s *Server) handleAccessAudit(w http.ResponseWriter, r *http.Request) {
	// The access_log table is populated by the MySQL sink; with the stdout
	// sink in use it stays empty (ADR-0001 §6.5).
	var total int
	_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM access_log`).Scan(&total)
	writeJSON(w, http.StatusOK, map[string]any{
		"items": []map[string]any{}, "total": total, "page": 1, "page_size": 50,
		"note": "access_log rows require ROUTEGATE_ACCESS_LOG_SINK=mysql; the stdout sink does not persist here",
	})
}

func pageParams(r *http.Request) (page, pageSize int) {
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ = strconv.Atoi(r.URL.Query().Get("page_size"))
	if pageSize < 1 || pageSize > 100 {
		pageSize = 50
	}
	return page, pageSize
}

// --- gateway state ---

// handleGatewayConfig renders the panel's deployed-configuration view. It
// queries the rows directly through the shared loaders: a DB failure yields a
// 500 envelope instead of the previous capture-recorder hack, which silently
// rendered zero routes/policies/identities with a 200 on a DB blip.
func (s *Server) handleGatewayConfig(w http.ResponseWriter, r *http.Request) {
	rev, err := storeCurrentRevision(r.Context(), s.db)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	routes, err := s.loadRoutes(r.Context())
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	policies, err := s.loadPolicies(r.Context())
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	identities, err := s.loadIdentities(r.Context())
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config_revision": rev,
		"routes":          routes,
		"policies":        policies,
		"identities":      identities,
	})
}

func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT instance_id, version, loaded_revision, started_at, last_sync_at, last_sync_ok, updated_at
		 FROM instances ORDER BY instance_id`)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, version string
		var rev int64
		var started, lastSync, updated sql.NullTime
		var ok bool
		if err := rows.Scan(&id, &version, &rev, &started, &lastSync, &ok, &updated); err != nil {
			continue
		}
		item := map[string]any{
			"instance_id": id, "version": version, "loaded_revision": rev,
			"last_sync_ok": ok, "stale": !updated.Valid || time.Since(updated.Time) > 10*time.Minute,
		}
		if started.Valid {
			item["started_at"] = started.Time.UTC().Format(time.RFC3339)
		}
		if lastSync.Valid {
			item["last_sync_at"] = lastSync.Time.UTC().Format(time.RFC3339)
		}
		if updated.Valid {
			item["updated_at"] = updated.Time.UTC().Format(time.RFC3339)
		}
		items = append(items, item)
	}
	rev, _ := storeCurrentRevision(r.Context(), s.db)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "config_revision": rev})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, renderSnapshot(s.holder.Load()))
}

// renderSnapshot serializes the loaded holder view into the complete
// deterministic status document: routes (with registered operations), loaded
// policies, and identity metadata. It reads ONLY the holder snapshot — never
// the database — so the response is exactly what this instance enforces.
func renderSnapshot(snap *snapshot.Snapshot) map[string]any {
	if snap == nil {
		return map[string]any{"loaded": false}
	}

	routes := []map[string]any{}
	policies := []map[string]any{}
	for _, rt := range snap.Routes {
		targets := []map[string]any{}
		for _, t := range rt.Targets {
			targets = append(targets, map[string]any{"host": t.Host, "port": t.Port, "weight": t.Weight})
		}
		operations := []map[string]any{}
		for _, op := range rt.Operations {
			operations = append(operations, map[string]any{
				"id": op.ID, "route_id": op.RouteID, "method": op.Method,
				"path": op.Path, "name": op.Name, "remark": op.Remark,
			})
		}
		methods := rt.Methods
		if methods == nil {
			methods = []string{}
		}
		routeItem := map[string]any{
			"id": rt.ID, "name": rt.Name, "path_prefix": rt.PathPrefix,
			"methods": methods, "targets": targets,
			"operations":   operations,
			"strip_prefix": rt.StripPrefix,
		}
		if rt.SuccessRule != nil {
			routeItem["success_rule"] = rt.SuccessRule
		}
		if rt.BizCodePath != "" {
			routeItem["biz_code_path"] = rt.BizCodePath
		}
		if rt.BizMessagePath != "" {
			routeItem["biz_message_path"] = rt.BizMessagePath
		}
		routes = append(routes, routeItem)
		// Correlate policies with their route name; policies are grouped by
		// route id and appended in id order (loader ORDER BY), so per-route
		// slices are already deterministic.
		for _, p := range snap.Policies[rt.ID] {
			item := map[string]any{
				"id": p.ID, "route_id": p.RouteID, "route": rt.Name,
				"caller": p.Caller, "method": p.Method,
				"path_pattern": p.PathPattern, "effect": p.Effect,
				"state":  map[string]any{"allow": !p.Deny, "deny": p.Deny},
				"remark": p.Remark,
			}
			if p.ExpiresAt != nil {
				item["expires_at"] = p.ExpiresAt.UTC().Format(time.RFC3339)
			}
			policies = append(policies, item)
		}
	}

	// Identities render sorted by name (then key id for determinism): build
	// the ordered slice once instead of rescanning the map per name.
	sorted := make([]*snapshot.Identity, 0, len(snap.Identities))
	for _, id := range snap.Identities {
		sorted = append(sorted, id)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Name != sorted[j].Name {
			return sorted[i].Name < sorted[j].Name
		}
		return sorted[i].KeyID < sorted[j].KeyID
	})
	identities := []map[string]any{}
	names := make([]string, 0, len(sorted))
	for _, id := range sorted {
		names = append(names, id.Name)
		item := map[string]any{
			"name": id.Name, "key_id": id.KeyID,
			"credential_type": "mtls_cert",
		}
		if !id.NotBefore.IsZero() {
			item["not_before"] = id.NotBefore.UTC().Format(time.RFC3339)
		}
		if !id.NotAfter.IsZero() {
			item["not_after"] = id.NotAfter.UTC().Format(time.RFC3339)
		}
		identities = append(identities, item)
	}

	// operationCount sums the registered operations across routes.
	operationCount := 0
	for _, rt := range snap.Routes {
		operationCount += len(rt.Operations)
	}

	return map[string]any{
		"loaded": true, "config_revision": snap.Revision,
		"loaded_at":        snap.LoadedAt.UTC().Format(time.RFC3339),
		"routes":           routes,
		"operations_count": operationCount,
		"policies":         policies,
		"identities":       identities,
		"identity_names":   names,
	}
}

// storeCurrentRevision reads settings.config_revision through the store
// helper: missing row maps to 0 and an unparsable value surfaces as an error
// instead of being silently scanned to 0 by fmt.Sscanf.
func storeCurrentRevision(ctx context.Context, db *sql.DB) (int64, error) {
	return store.CurrentRevision(ctx, db)
}

// --- route operations (managed registry, Round 2) ---

type operationPayload struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Name        string `json:"name"`
	Remark      string `json:"remark"`
	ExpectedRev int64  `json:"expected_revision,omitempty"`
}

func (s *Server) handleListRouteOperations(w http.ResponseWriter, r *http.Request) {
	routeID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, method, path, name, remark FROM route_operations WHERE route_id=? ORDER BY method, path`, routeID)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var method, path, name, remark string
		if err := rows.Scan(&id, &method, &path, &name, &remark); err != nil {
			continue
		}
		items = append(items, map[string]any{
			"id": id, "route_id": routeID, "method": method,
			"path": path, "name": name, "remark": remark,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleCreateRouteOperation(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	routeID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var p operationPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	op, err := store.ValidateOperation(p.Method, p.Path)
	if err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_operation", err.Error())
		return
	}
	var routeExists int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM routes WHERE id=?`, routeID).Scan(&routeExists); err != nil || routeExists == 0 {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_operation", fmt.Sprintf("route %d does not exist", routeID))
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "route.operation.create", "route_operation",
		fmt.Sprintf("%d:%s %s", routeID, op.Method, op.Path), p.ExpectedRev, nil, p,
		func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(),
				`INSERT INTO route_operations (route_id, method, path, name, remark) VALUES (?, ?, ?, ?, ?)`,
				routeID, op.Method, op.Path, p.Name, p.Remark)
			return err
		})
	if err != nil {
		writeOperationError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created"})
}

func (s *Server) handleUpdateOperation(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var p operationPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	op, err := store.ValidateOperation(p.Method, p.Path)
	if err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_operation", err.Error())
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "route.operation.update", "route_operation",
		strconv.FormatInt(id, 10), p.ExpectedRev, nil, p,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(r.Context(),
				`UPDATE route_operations SET method=?, path=?, name=?, remark=? WHERE id=?`,
				op.Method, op.Path, p.Name, p.Remark, id)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeOperationError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated"})
}

func (s *Server) handleDeleteOperation(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		ExpectedRev int64 `json:"expected_revision"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	err := s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "route.operation.delete", "route_operation",
		strconv.FormatInt(id, 10), body.ExpectedRev, nil, nil,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(r.Context(), `DELETE FROM route_operations WHERE id=?`, id)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeOperationError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// writeOperationError maps duplicate-key violations on route_operations to a
// friendly 409 (the unique key is route+method+path); everything else follows
// the shared writeTxError classification.
func writeOperationError(r *http.Request, w http.ResponseWriter, err error) {
	if isDuplicateKey(err) {
		errorJSON(r, w, http.StatusConflict, "admin.duplicate", "this method + path is already registered on the route")
		return
	}
	writeTxError(r, w, err)
}

// isDuplicateKey reports a MySQL duplicate-key (1062) violation, wrapped or
// not. This is the single duplicate-key check for the admin plane.
func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlErrDupEntry
}

// handleListRouteCallers backs the route drawer's certificate section (owner
// request Round 3): the distinct callers referenced by the route's enabled
// policies, each joined with the identity's certificate status and expiry.
// A caller with no identity row is reported as not_registered — a policy can
// exist before its certificate is registered.
func (s *Server) handleListRouteCallers(w http.ResponseWriter, r *http.Request) {
	routeID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT p.caller,
		       (SELECT i.status FROM identities i WHERE i.name = p.caller ORDER BY i.id DESC LIMIT 1),
		       (SELECT i.not_after FROM identities i WHERE i.name = p.caller AND i.status = 'active' ORDER BY i.id DESC LIMIT 1)
		FROM (SELECT DISTINCT caller FROM policies WHERE route_id = ? AND enabled = 1) p
		ORDER BY p.caller`, routeID)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var caller string
		var status sql.NullString
		var notAfter sql.NullTime
		if err := rows.Scan(&caller, &status, &notAfter); err != nil {
			continue
		}
		item := map[string]any{"caller": caller}
		if !status.Valid {
			item["registered"] = false
			item["status"] = "not_registered"
		} else {
			item["registered"] = true
			item["status"] = status.String
			if notAfter.Valid {
				item["not_after"] = notAfter.Time.UTC().Format(time.RFC3339)
			}
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleAccessLogFile serves access-decision records from the rotating file
// sink segments (newest first) with optional caller/decision/route exact
// filters and a record limit. It never touches the database; when the file
// sink is not enabled it explains how to enable persistence.
func (s *Server) handleAccessLogFile(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AccessLogFilePath == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []map[string]any{},
			"note":  "access log file sink is not enabled (set ROUTEGATE_ACCESS_LOG_SINK=file and ROUTEGATE_ACCESS_LOG_FILE)",
		})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	qCaller := r.URL.Query().Get("caller")
	qDecision := r.URL.Query().Get("decision")
	qRoute := r.URL.Query().Get("route")
	qOutcome := r.URL.Query().Get("outcome")

	paths, err := accesslog.SegmentPaths(s.cfg.AccessLogFilePath)
	if err != nil {
		// Filesystem errors embed server paths — log them, keep the note generic.
		log.Printf("admin access-log listing failed: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []map[string]any{}, "count": 0, "limit": limit,
			"note": "no access log segments found on this instance",
		})
		return
	}
	var filter accesslog.RecordFilter
	if qCaller != "" || qDecision != "" || qRoute != "" || qOutcome != "" {
		filter = func(rec accesslog.Record) bool {
			return (qCaller == "" || rec.Caller == qCaller) &&
				(qDecision == "" || rec.Decision == qDecision) &&
				(qRoute == "" || rec.Route == qRoute) &&
				(qOutcome == "" || rec.Outcome == qOutcome)
		}
	}
	recs, err := accesslog.ReadRecordsBackward(paths, limit, filter)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	items := []map[string]any{}
	for _, rec := range recs {
		items = append(items, map[string]any{
			"ts": rec.TS.UTC().Format(time.RFC3339), "instance_id": rec.InstanceID,
			"request_id": rec.RequestID, "caller": rec.Caller, "route": rec.Route,
			"method": rec.Method, "path": rec.Path,
			"matched_policy_id": rec.MatchedPolicyID, "config_revision": rec.ConfigRevision,
			"decision": rec.Decision, "status": rec.Status, "duration_ms": rec.DurationMS,
			"request_bytes": rec.RequestBytes, "response_bytes": rec.ResponseBytes,
			"upstream_host": rec.UpstreamHost, "outcome": rec.Outcome,
			"biz_code": rec.BizCode, "biz_message": rec.BizMessage,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items), "limit": limit})
}

// --- debug log (opt-in sensitive payloads, ADR-0001 §6.6) ---

// debugMeta projects a DebugRecord to metadata-only fields. The list endpoint
// never returns headers, cookies, or payloads — those live behind the
// superadmin-only detail endpoint.
func debugMeta(rec accesslog.DebugRecord) map[string]any {
	return map[string]any{
		"ts": rec.TS.UTC().Format(time.RFC3339), "instance_id": rec.InstanceID,
		"request_id": rec.RequestID, "caller": rec.Caller, "route": rec.Route,
		"method": rec.Method, "path": rec.Path,
		"matched_policy_id": rec.MatchedPolicyID, "config_revision": rec.ConfigRevision,
		"decision": rec.Decision, "status": rec.Status, "duration_ms": rec.DurationMS,
		"request_bytes": rec.RequestBytes, "response_bytes": rec.ResponseBytes,
		"upstream_host": rec.UpstreamHost,
		"outcome":       rec.Outcome,
		"biz_code":      rec.BizCode, "biz_message": rec.BizMessage,
	}
}

// handleDebugLogs serves debug-record metadata from the rotating debug sink
// segments (newest first). Available to every admin user; payloads are not.
func (s *Server) handleDebugLogs(w http.ResponseWriter, r *http.Request) {
	ds := s.cfg.Debug.Settings()
	if ds == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []map[string]any{}, "enabled": false, "count": 0,
			"note": "debug logging is not enabled (enable it on the Settings page)",
		})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	qCaller := r.URL.Query().Get("caller")
	qDecision := r.URL.Query().Get("decision")
	qRoute := r.URL.Query().Get("route")

	paths, err := accesslog.DebugSegmentPaths(ds.File)
	if err != nil {
		// Filesystem errors embed server paths — log them, keep the note generic.
		log.Printf("admin debug-log listing failed: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []map[string]any{}, "enabled": true, "count": 0, "limit": limit,
			"note": "no debug log segments found on this instance",
		})
		return
	}
	var filter func(accesslog.DebugRecord) bool
	if qCaller != "" || qDecision != "" || qRoute != "" {
		filter = func(rec accesslog.DebugRecord) bool {
			return (qCaller == "" || rec.Caller == qCaller) &&
				(qDecision == "" || rec.Decision == qDecision) &&
				(qRoute == "" || rec.Route == qRoute)
		}
	}
	recs, err := accesslog.ReadDebugRecordsBackward(paths, limit, filter)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	items := []map[string]any{}
	for _, rec := range recs {
		items = append(items, debugMeta(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "enabled": true, "count": len(items), "limit": limit})
}

// handleDebugLogDetail returns one bounded debug record — sanitized headers,
// cookies, and payload captures — for superadmin troubleshooting. The response
// is no-store so nothing between the panel and the gateway caches sensitive
// payloads, and viewing it is itself audited (the audit row holds only the
// request ID, never the payload).
func (s *Server) handleDebugLogDetail(w http.ResponseWriter, r *http.Request) {
	ds := s.cfg.Debug.Settings()
	if ds == nil {
		errorJSON(r, w, http.StatusServiceUnavailable, "admin.debug_disabled", "debug logging is not enabled on this instance")
		return
	}
	reqID := r.PathValue("request_id")
	if reqID == "" || len(reqID) > 128 {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request_id required")
		return
	}
	paths, err := accesslog.DebugSegmentPaths(ds.File)
	if err != nil {
		errorJSON(r, w, http.StatusNotFound, "admin.debug_not_found", "no debug log segments exist for this instance")
		return
	}
	recs, err := accesslog.ReadDebugRecordsBackward(paths, 500, func(rec accesslog.DebugRecord) bool {
		return rec.RequestID == reqID
	})
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	if len(recs) == 0 {
		errorJSON(r, w, http.StatusNotFound, "admin.debug_not_found", "no debug record matches this request ID (records roll over and may have been retained out)")
		return
	}
	uid, _ := r.Context().Value(ctxUserID).(int64)
	user, _ := r.Context().Value(ctxUser).(string)
	s.auditSimple(r.Context(), uid, user, "debug.log_viewed", reqID, clientIP(r))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, recs[0])
}

// --- debug settings (panel 设置 page, ADR-0001 §6.6) ---

// handleGetDebugSettings returns the current settings.debug_log values plus
// the config revision for optimistic concurrency. Readable by every admin
// user; writes are superadmin-only.
func (s *Server) handleGetDebugSettings(w http.ResponseWriter, r *http.Request) {
	ds, err := store.LoadDebugSettingsSync(r.Context(), s.db)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	rev, err := storeCurrentRevision(r.Context(), s.db)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":                 ds.Enabled,
		"file":                    ds.File,
		"max_mb":                  ds.MaxMB,
		"retain_days":             ds.RetainDays,
		"request_body_max_bytes":  ds.RequestBodyMaxBytes,
		"response_body_max_bytes": ds.ResponseBodyMaxBytes,
		"headers_max_bytes":       ds.HeadersMaxBytes,
		"queue_capacity":          ds.QueueCapacity,
		"running":                 s.cfg.Debug.Settings() != nil,
		"config_revision":         rev,
	})
}

// handlePutDebugSettings updates settings.debug_log inside the standard
// three-in-one transaction (settings row + config_revision bump + audit).
// Instances apply the change on their next sync tick.
func (s *Server) handlePutDebugSettings(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var req store.DebugSettings
	type revPayload struct {
		ExpectedRev int64 `json:"expected_revision"`
	}
	var revReq revPayload
	body, err := io.ReadAll(r.Body)
	if err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "could not read the request body")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	_ = json.Unmarshal(body, &revReq) // expected_revision optional
	if err := req.Validate(); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_debug_settings", err.Error())
		return
	}
	before, err := store.LoadDebugSettingsSync(r.Context(), s.db)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "debug.settings_updated", "settings", "debug_log", revReq.ExpectedRev, before, req,
		func(tx *sql.Tx) error {
			return store.SaveDebugSettings(r.Context(), tx, req)
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated", "note": "instances apply the change on their next sync tick"})
}

// --- global business-envelope fallback (panel settings, ADR-0001 §6.7) ---

// handleGetBizSettings returns the global biz code/message paths plus the
// config revision for optimistic concurrency. Readable by every admin user.
func (s *Server) handleGetBizSettings(w http.ResponseWriter, r *http.Request) {
	f, err := store.LoadBizFieldsSync(r.Context(), s.db)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	rev, err := storeCurrentRevision(r.Context(), s.db)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code_path":       f.CodePath,
		"message_path":    f.MessagePath,
		"config_revision": rev,
	})
}

// handlePutBizSettings updates settings.biz_fields inside the standard
// three-in-one transaction. Instances apply the values on their next tick;
// routes that define their own paths take precedence per field.
func (s *Server) handlePutBizSettings(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var req store.BizFields
	type revPayload struct {
		ExpectedRev int64 `json:"expected_revision"`
	}
	var revReq revPayload
	body, err := io.ReadAll(r.Body)
	if err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "could not read the request body")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	_ = json.Unmarshal(body, &revReq)
	if req.CodePath != "" {
		if err := store.ValidateBodyPath(req.CodePath); err != nil {
			errorJSON(r, w, http.StatusBadRequest, "admin.invalid_biz_settings", "code_path: "+err.Error())
			return
		}
	}
	if req.MessagePath != "" {
		if err := store.ValidateBodyPath(req.MessagePath); err != nil {
			errorJSON(r, w, http.StatusBadRequest, "admin.invalid_biz_settings", "message_path: "+err.Error())
			return
		}
	}
	before, err := store.LoadBizFieldsSync(r.Context(), s.db)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "biz.settings_updated", "settings", "biz_fields", revReq.ExpectedRev, before, req,
		func(tx *sql.Tx) error {
			return store.SaveBizFields(r.Context(), tx, req)
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated", "note": "instances apply the change on their next sync tick"})
}

// --- alert channels (Feishu/Lark bot webhooks, ADR-0001 §9.1) ---

// alertNotFound marks a missing channel row for the 404 mapping.
var alertNotFound = errors.New("alert channel not found")

// alertChannelAudit redacts the webhook token before a payload reaches an
// audit row — the token is a credential.
func alertChannelAudit(c store.AlertChannel) store.AlertChannel {
	c.WebhookToken = store.MaskToken(c.WebhookToken)
	return c
}

// handleListAlertChannels returns every configured channel with the webhook
// token masked. Readable by every admin user.
func (s *Server) handleListAlertChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := store.LoadAlertChannels(r.Context(), s.db)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	items := []map[string]any{}
	for _, c := range channels {
		items = append(items, map[string]any{
			"id": c.ID, "name": c.Name, "enabled": c.Enabled,
			"platform": c.Platform, "webhook_masked": store.MaskToken(c.WebhookToken),
			"has_token":  c.WebhookToken != "",
			"updated_at": c.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleCreateAlertChannel adds a channel. Audit rows carry masked tokens.
func (s *Server) handleCreateAlertChannel(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var req store.AlertChannel
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	req.WebhookToken = strings.TrimSpace(req.WebhookToken)
	if err := store.ValidateAlertChannel(req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_alert_channel", err.Error())
		return
	}
	if req.WebhookToken == "" || len(req.WebhookToken) > 256 {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_alert_channel", "webhook token is required (max 256 chars)")
		return
	}
	err := s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "alert.channel_created", "alert_channel", req.Name, 0, nil, alertChannelAudit(req),
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(r.Context(),
				`INSERT INTO alert_channels (name, enabled, platform, webhook_token) VALUES (?, ?, ?, ?)`,
				strings.TrimSpace(req.Name), req.Enabled, req.Platform, req.WebhookToken)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeAlertTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "name": req.Name})
}

// handleUpdateAlertChannel updates one channel. An empty webhook_token keeps
// the stored credential. Audit rows carry masked tokens.
func (s *Server) handleUpdateAlertChannel(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var req store.AlertChannel
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	current, err := alertChannelByID(r, s.db, id)
	if err != nil {
		writeAlertTxError(r, w, err)
		return
	}
	req.WebhookToken = strings.TrimSpace(req.WebhookToken)
	if req.WebhookToken == "" {
		req.WebhookToken = current.WebhookToken // keep the stored credential
	}
	if err := store.ValidateAlertChannel(req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_alert_channel", err.Error())
		return
	}
	if req.Enabled && req.WebhookToken == "" {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_alert_channel", "enabled alert channel requires a webhook token")
		return
	}
	before := alertChannelAudit(current)
	after := alertChannelAudit(req)
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "alert.channel_updated", "alert_channel", fmt.Sprintf("%d", id), 0, before, after,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(r.Context(),
				`UPDATE alert_channels SET name=?, enabled=?, platform=?, webhook_token=? WHERE id=?`,
				strings.TrimSpace(req.Name), req.Enabled, req.Platform, req.WebhookToken, id)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeAlertTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated"})
}

// handleDeleteAlertChannel removes one channel. Audit rows carry masked
// tokens.
func (s *Server) handleDeleteAlertChannel(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	current, err := alertChannelByID(r, s.db, id)
	if err != nil {
		writeAlertTxError(r, w, err)
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "alert.channel_deleted", "alert_channel", fmt.Sprintf("%d", id), 0, alertChannelAudit(current), nil,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(r.Context(), `DELETE FROM alert_channels WHERE id=?`, id)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeAlertTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// handleTestAlertChannel sends a test message through one channel. The
// response is bounded and never contains the token.
func (s *Server) handleTestAlertChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c, err := alertChannelByID(r, s.db, id)
	if err != nil {
		writeAlertTxError(r, w, err)
		return
	}
	if !c.Enabled || c.WebhookToken == "" {
		errorJSON(r, w, http.StatusBadRequest, "admin.alert_not_configured", "alert channel is not enabled")
		return
	}
	url, err := c.WebhookURL()
	if err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_alert_channel", err.Error())
		return
	}
	b, _ := json.Marshal(alerts.TestMessageCard(s.instanceID, c.Name, c.Platform, time.Now()))
	transport := http.DefaultTransport.(*http.Transport).Clone() // honors HTTPS_PROXY env
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		// The Go client error embeds the full webhook URL (which contains the
		// channel token). Never echo it into the response — a fixed soft-fail
		// message keeps the delivery detail out entirely.
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "the webhook request failed"})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status":   resp.StatusCode,
		"response": strings.TrimSpace(string(respBody)),
	})
}

// alertChannelByID loads one row (with the plaintext token — handlers only,
// never responses) and maps missing rows to 404.
func alertChannelByID(r *http.Request, db *sql.DB, id int64) (store.AlertChannel, error) {
	var c store.AlertChannel
	var updated sql.NullTime
	err := db.QueryRowContext(r.Context(),
		`SELECT id, name, enabled, platform, webhook_token, updated_at FROM alert_channels WHERE id=?`, id).
		Scan(&c.ID, &c.Name, &c.Enabled, &c.Platform, &c.WebhookToken, &updated)
	if err == sql.ErrNoRows {
		return c, alertNotFound
	}
	if updated.Valid {
		c.UpdatedAt = updated.Time
	}
	return c, err
}

// writeAlertTxError wraps writeTxError with a friendlier not-found message:
// conflictError without a revision carries the not-found semantics here.
// Duplicates and infrastructure failures get the same 409/500 discipline as
// every other write (channels/rules run through this wrapper).
func writeAlertTxError(r *http.Request, w http.ResponseWriter, err error) {
	if errors.Is(err, alertNotFound) {
		errorJSON(r, w, http.StatusNotFound, "admin.alert_not_found", "alert channel not found")
		return
	}
	if isDuplicateKey(err) {
		errorJSON(r, w, http.StatusConflict, "admin.duplicate", "a channel with this name already exists")
		return
	}
	writeTxError(r, w, err)
}

// --- alert rules (ADR-0001 §9.2) ---

// handleListAlertRules returns every rule with its parsed conditions.
func (s *Server) handleListAlertRules(w http.ResponseWriter, r *http.Request) {
	rules, err := store.LoadAlertRules(r.Context(), s.db)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	items := []map[string]any{}
	for _, r := range rules {
		match := r.Match
		if match == nil {
			match = &store.AlertMatchNode{Mode: r.MatchMode}
		}
		doc, _ := json.Marshal(match)
		items = append(items, map[string]any{
			"id": r.ID, "name": r.Name, "enabled": r.Enabled,
			"scope_type": r.ScopeType, "route_ids": r.RouteIDs,
			"channel_id": r.ChannelID,
			"match":      json.RawMessage(doc),
			"updated_at": r.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// alertRulePayload is the create/update body.
type alertRulePayload struct {
	Name       string          `json:"name"`
	Enabled    bool            `json:"enabled"`
	ScopeType  string          `json:"scope_type"`
	RouteIDs   []int64         `json:"route_ids"`
	ChannelID  int64           `json:"channel_id"`
	MatchMode  string          `json:"match_mode"`
	Conditions json.RawMessage `json:"conditions"`
}

func (p *alertRulePayload) validate() (*store.AlertMatchNode, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 64 {
		return nil, fmt.Errorf("name is required (max 64 chars)")
	}
	if p.ScopeType != "all" && p.ScopeType != "routes" {
		return nil, fmt.Errorf("scope_type must be all or routes")
	}
	if p.ScopeType == "routes" && len(p.RouteIDs) == 0 {
		return nil, fmt.Errorf("scope_type=routes requires at least one route id")
	}
	if p.ChannelID < 1 {
		return nil, fmt.Errorf("channel_id is required")
	}
	if p.MatchMode == "" {
		p.MatchMode = "all"
	}
	if p.MatchMode != "all" && p.MatchMode != "any" {
		return nil, fmt.Errorf("match_mode must be all or any")
	}
	match, err := store.ParseAlertMatch(p.Conditions)
	if err != nil {
		return nil, err
	}
	return match, nil
}

// handleCreateAlertRule adds one rule. Conditions are validated before the
// write transaction; the channel must exist.
func (s *Server) handleCreateAlertRule(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var p alertRulePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	match, verr := p.validate()
	if verr != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_alert_rule", verr.Error())
		return
	}
	var channelExists int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM alert_channels WHERE id=?`, p.ChannelID).Scan(&channelExists); err != nil || channelExists == 0 {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_alert_rule", "channel_id does not exist")
		return
	}
	routeIDs, _ := json.Marshal(p.RouteIDs)
	condBytes, _ := json.Marshal(match)
	err := s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "alert.rule_created", "alert_rule", p.Name, 0, nil, p,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(r.Context(),
				`INSERT INTO alert_rules (name, enabled, scope_type, route_ids, channel_id, match_mode, conditions) VALUES (?, ?, ?, CAST(? AS JSON), ?, ?, CAST(? AS JSON))`,
				p.Name, p.Enabled, p.ScopeType, routeIDs, p.ChannelID, p.MatchMode, condBytes)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeAlertTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "name": p.Name})
}

// handleUpdateAlertRule updates one rule.
func (s *Server) handleUpdateAlertRule(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var p alertRulePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	match, verr := p.validate()
	if verr != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_alert_rule", verr.Error())
		return
	}
	var channelExists int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM alert_channels WHERE id=?`, p.ChannelID).Scan(&channelExists); err != nil || channelExists == 0 {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_alert_rule", "channel_id does not exist")
		return
	}
	before, err := alertRuleByID(r, s.db, id)
	if err != nil {
		writeAlertTxError(r, w, err)
		return
	}
	routeIDs, _ := json.Marshal(p.RouteIDs)
	condBytes, _ := json.Marshal(match)
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "alert.rule_updated", "alert_rule", fmt.Sprintf("%d", id), 0, before, p,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(r.Context(),
				`UPDATE alert_rules SET name=?, enabled=?, scope_type=?, route_ids=CAST(? AS JSON), channel_id=?, match_mode=?, conditions=CAST(? AS JSON) WHERE id=?`,
				p.Name, p.Enabled, p.ScopeType, routeIDs, p.ChannelID, p.MatchMode, condBytes, id)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeAlertTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated"})
}

// handleDeleteAlertRule removes one rule.
func (s *Server) handleDeleteAlertRule(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	before, err := alertRuleByID(r, s.db, id)
	if err != nil {
		writeAlertTxError(r, w, err)
		return
	}
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "alert.rule_deleted", "alert_rule", fmt.Sprintf("%d", id), 0, before, nil,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(r.Context(), `DELETE FROM alert_rules WHERE id=?`, id)
			return mustAffectOne(res, err)
		})
	if err != nil {
		writeAlertTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// alertRuleByID loads one rule row for audit before/after payloads.
func alertRuleByID(r *http.Request, db *sql.DB, id int64) (store.AlertRule, error) {
	rules, err := store.LoadAlertRules(r.Context(), db)
	if err != nil {
		return store.AlertRule{}, err
	}
	for _, rule := range rules {
		if rule.ID == id {
			return rule, nil
		}
	}
	return store.AlertRule{}, alertNotFound
}

// handleListAlertEvents serves the newest firings for the panel.
func (s *Server) handleListAlertEvents(w http.ResponseWriter, r *http.Request) {
	page, pageSize := pageParams(r)
	limit := pageSize
	ruleID, _ := strconv.ParseInt(r.URL.Query().Get("rule_id"), 10, 64)
	events, err := store.ListAlertEvents(r.Context(), s.db, ruleID, limit)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	items := []map[string]any{}
	for _, e := range events {
		items = append(items, map[string]any{
			"id": e.ID, "rule_id": e.RuleID, "rule_name": e.RuleName,
			"route_name": e.RouteName, "request_id": e.RequestID,
			"status": e.Status, "outcome": e.Outcome, "biz_code": e.BizCode,
			"biz_message": e.BizMessage,
			"sent":        e.Sent, "send_error": e.SendError,
			"created_at": e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "page": page, "page_size": pageSize})
}
