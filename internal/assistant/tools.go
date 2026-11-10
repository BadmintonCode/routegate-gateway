// Package assistant implements the panel's troubleshooting chat agent
// (ADR-0001 §10). The security boundary is the tool whitelist in this
// package: the model can only invoke the functions registered here, and
// every one of them is a parameterized SELECT, an in-memory snapshot read,
// or a read over the access-log file sink. No write operation exists in the
// model's reachable surface; the system prompt (agent.go) is guidance, not
// the enforcement layer.
package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/proxy"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
)

// ToolCallRecord is the audit entry for one read-only tool invocation. Args
// carry the model-produced input, bounded to keep chat rows and audit
// payloads small.
type ToolCallRecord struct {
	Tool       string `json:"tool"`
	Args       string `json:"args,omitempty"`
	OK         bool   `json:"ok"`
	DurationMS int64  `json:"duration_ms"`
}

const maxArgJSON = 512

// recorder collects the tool invocations of one chat turn. Tool functions
// close over it; the react agent loop calls them from a single goroutine per
// step, but the mutex keeps the record race-free regardless.
type recorder struct {
	mu    sync.Mutex
	calls []ToolCallRecord
}

func (r *recorder) add(c ToolCallRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *recorder) snapshot() []ToolCallRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ToolCallRecord, len(r.calls))
	copy(out, r.calls)
	return out
}

// boundedJSON marshals the tool input for the audit trail, truncating
// oversized values rather than failing the invocation.
func boundedJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	if len(b) > maxArgJSON {
		b = append(b[:maxArgJSON-3], "..."...)
	}
	return string(b)
}

// --- tool inputs (JSON schema is inferred from these struct tags) ---

type policySimulateIn struct {
	Caller string `json:"caller" jsonschema:"caller identity name, e.g. edge-gw or service-a"`
	Method string `json:"method" jsonschema:"HTTP method, defaults to GET"`
	Path   string `json:"path" jsonschema:"request path to evaluate, e.g. /alpha/v1/ping"`
}

type routeListIn struct {
	Name string `json:"name,omitempty" jsonschema:"optional route name substring filter"`
}

type policyListIn struct {
	RouteName string `json:"route_name,omitempty" jsonschema:"optional route name to list that route's policies only"`
}

type identityListIn struct {
	Name string `json:"name,omitempty" jsonschema:"optional identity name substring filter"`
}

type instanceListIn struct{}

type auditQueryIn struct {
	ActionPrefix string `json:"action_prefix,omitempty" jsonschema:"action prefix filter, e.g. route. or policy."`
	Resource     string `json:"resource,omitempty" jsonschema:"resource name substring filter"`
	Limit        int    `json:"limit,omitempty" jsonschema:"max rows, default 20, cap 50"`
}

type alertEventQueryIn struct {
	RouteName string `json:"route_name,omitempty" jsonschema:"optional route name filter"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max rows, default 20, cap 50"`
}

type accessLogQueryIn struct {
	Caller   string `json:"caller,omitempty" jsonschema:"exact caller identity filter"`
	Route    string `json:"route,omitempty" jsonschema:"exact route name filter"`
	Decision string `json:"decision,omitempty" jsonschema:"decision filter: allow, deny, unauthenticated, route_not_found or upstream_error"`
	Outcome  string `json:"outcome,omitempty" jsonschema:"success verdict filter: success or failure (routes with a success rule only)"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max rows, default 20, cap 50 (newest first)"`
}

const toolLimitCap = 50

func clampLimit(n int) int {
	if n <= 0 {
		return 20
	}
	if n > toolLimitCap {
		return toolLimitCap
	}
	return n
}

// engine is the tool execution context: read-only dependencies shared by all
// invocations of one server process.
type engine struct {
	db            *sql.DB
	holder        *snapshot.Holder
	accessLogPath string
}

// --- individual tool implementations (plain functions, testable) ---

func (e *engine) simulatePolicy(_ context.Context, in policySimulateIn) (map[string]any, error) {
	if in.Caller == "" || in.Path == "" {
		return nil, fmt.Errorf("caller and path are required")
	}
	if in.Method == "" {
		in.Method = "GET"
	}
	snap := e.holder.Load()
	route, decision, policyID, sub := proxy.TestAccess(snap, in.Caller, strings.ToUpper(in.Method), in.Path)
	out := map[string]any{
		"route": route, "decision": decision, "matched_policy_id": policyID, "sub_path": sub,
	}
	if snap != nil {
		out["config_revision"] = snap.Revision
	}
	return out, nil
}

func (e *engine) listRoutes(_ context.Context, in routeListIn) ([]map[string]any, error) {
	snap := e.holder.Load()
	if snap == nil {
		return []map[string]any{}, nil
	}
	out := []map[string]any{}
	for _, r := range snap.Routes {
		if in.Name != "" && !strings.Contains(strings.ToLower(r.Name), strings.ToLower(in.Name)) {
			continue
		}
		targets := []string{}
		for _, t := range r.Targets {
			targets = append(targets, fmt.Sprintf("%s:%d(w%d)", t.Host, t.Port, t.Weight))
		}
		methods := r.Methods
		if len(methods) == 0 {
			methods = []string{"*"}
		}
		out = append(out, map[string]any{
			"name": r.Name, "path_prefix": r.PathPrefix, "methods": methods,
			"targets": targets, "strip_prefix": r.StripPrefix,
			"has_success_rule": r.SuccessRule != nil,
		})
	}
	return out, nil
}

func (e *engine) listPolicies(_ context.Context, in policyListIn) ([]map[string]any, error) {
	snap := e.holder.Load()
	if snap == nil {
		return []map[string]any{}, nil
	}
	routeByID := map[int64]string{}
	for _, r := range snap.Routes {
		routeByID[r.ID] = r.Name
	}
	out := []map[string]any{}
	for routeID, pols := range snap.Policies {
		rname := routeByID[routeID]
		if in.RouteName != "" && rname != in.RouteName {
			continue
		}
		for _, p := range pols {
			effect := "allow"
			if p.Deny {
				effect = "deny"
			}
			out = append(out, map[string]any{
				"id": p.ID, "route": rname, "caller": p.Caller, "method": p.Method,
				"path_pattern": p.PathPattern, "effect": effect, "expires_at": p.ExpiresAt,
			})
		}
	}
	return out, nil
}

func (e *engine) listIdentities(_ context.Context, in identityListIn) ([]map[string]any, error) {
	snap := e.holder.Load()
	if snap == nil {
		return []map[string]any{}, nil
	}
	out := []map[string]any{}
	for _, id := range snap.Identities {
		if in.Name != "" && !strings.Contains(strings.ToLower(id.Name), strings.ToLower(in.Name)) {
			continue
		}
		fpr := id.Fingerprint
		if len(fpr) > 16 {
			fpr = fpr[:16] + "…"
		}
		out = append(out, map[string]any{
			"name": id.Name, "key_id": id.KeyID, "fingerprint": fpr,
			"not_before": id.NotBefore.UTC().Format(time.RFC3339),
			"not_after":  id.NotAfter.UTC().Format(time.RFC3339),
			"expired":    time.Now().After(id.NotAfter),
		})
	}
	return out, nil
}

func (e *engine) listInstances(ctx context.Context, _ instanceListIn) ([]map[string]any, error) {
	rows, err := e.db.QueryContext(ctx,
		`SELECT instance_id, version, loaded_revision, started_at, last_sync_at, last_sync_ok, updated_at
		 FROM instances ORDER BY instance_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, version string
		var rev int64
		var started, lastSync, updated sql.NullTime
		var ok bool
		if err := rows.Scan(&id, &version, &rev, &started, &lastSync, &ok, &updated); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"instance_id": id, "version": version, "loaded_revision": rev,
			"last_sync_ok": ok,
			"stale":        !updated.Valid || time.Since(updated.Time) > 10*time.Minute,
			"last_sync_at": nullTimeString(lastSync),
		})
	}
	return out, rows.Err()
}

func (e *engine) queryAdminAudit(ctx context.Context, in auditQueryIn) ([]map[string]any, error) {
	limit := clampLimit(in.Limit)
	query := `SELECT actor_name, action, resource_type, resource_id, created_at FROM admin_audit`
	var args []any
	var conds []string
	if in.ActionPrefix != "" {
		conds = append(conds, "action LIKE ?")
		args = append(args, strings.TrimSuffix(in.ActionPrefix, "%")+"%")
	}
	if in.Resource != "" {
		conds = append(conds, "resource_id LIKE ?")
		args = append(args, "%"+in.Resource+"%")
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var actor, action, rtype, rid string
		var created sql.NullTime
		if err := rows.Scan(&actor, &action, &rtype, &rid, &created); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"actor": actor, "action": action, "resource_type": rtype,
			"resource_id": rid, "created_at": nullTimeString(created),
		})
	}
	return out, rows.Err()
}

func (e *engine) queryAlertEvents(ctx context.Context, in alertEventQueryIn) ([]map[string]any, error) {
	limit := clampLimit(in.Limit)
	query := `SELECT rule_name, route_name, request_id, http_status, outcome, biz_code, sent, created_at FROM alert_events`
	var args []any
	if in.RouteName != "" {
		query += " WHERE route_name = ?"
		args = append(args, in.RouteName)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var rule, route, reqID, outcome, bizCode string
		var status int
		var sent bool
		var created sql.NullTime
		if err := rows.Scan(&rule, &route, &reqID, &status, &outcome, &bizCode, &sent, &created); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"rule": rule, "route": route, "request_id": reqID, "http_status": status,
			"outcome": outcome, "biz_code": bizCode, "sent": sent,
			"created_at": nullTimeString(created),
		})
	}
	return out, rows.Err()
}

// queryAccessLogs reads the newest matching records from the access-log file
// sink. When the file sink is disabled the tool answers with an explicit
// "unavailable" note instead of an empty list, so the model can tell the
// operator what to enable.
func (e *engine) queryAccessLogs(_ context.Context, in accessLogQueryIn) (map[string]any, error) {
	if e.accessLogPath == "" {
		return map[string]any{
			"items": []map[string]any{},
			"note":  "access log file sink is not enabled on this instance; set ROUTEGATE_ACCESS_LOG_SINK=file to capture records",
		}, nil
	}
	limit := clampLimit(in.Limit)
	paths, err := accesslog.SegmentPaths(e.accessLogPath)
	if err != nil {
		return map[string]any{"items": []map[string]any{}, "note": "no access log segments found on this instance"}, nil
	}
	var filter accesslog.RecordFilter
	if in.Caller != "" || in.Route != "" || in.Decision != "" || in.Outcome != "" {
		filter = func(rec accesslog.Record) bool {
			return (in.Caller == "" || rec.Caller == in.Caller) &&
				(in.Route == "" || rec.Route == in.Route) &&
				(in.Decision == "" || rec.Decision == in.Decision) &&
				(in.Outcome == "" || rec.Outcome == in.Outcome)
		}
	}
	recs, err := accesslog.ReadRecordsBackward(paths, limit, filter)
	if err != nil {
		return nil, err
	}
	items := []map[string]any{}
	for _, rec := range recs {
		items = append(items, map[string]any{
			"ts": rec.TS.UTC().Format(time.RFC3339), "request_id": rec.RequestID,
			"caller": rec.Caller, "route": rec.Route, "method": rec.Method,
			"path": rec.Path, "decision": rec.Decision, "status": rec.Status,
			"duration_ms": rec.DurationMS, "upstream_host": rec.UpstreamHost,
			"outcome": rec.Outcome, "biz_code": rec.BizCode, "biz_message": rec.BizMessage,
		})
	}
	return map[string]any{"items": items, "count": len(items)}, nil
}

func nullTimeString(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}
