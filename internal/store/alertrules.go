package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
)

// AlertRule is one alert rule (ADR-0001 §9.2): a composable response match —
// the same condition shape as route success rules plus an `outcome` source
// fed by the route's own success verdict — scoped to all or selected routes,
// firing into exactly one alert channel. The struct is marshaled into
// admin_audit rows, so the JSON keys must be snake_case.
type AlertRule struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Enabled   bool            `json:"enabled"`
	ScopeType string          `json:"scope_type"` // all | routes
	RouteIDs  []int64         `json:"route_ids"`
	ChannelID int64           `json:"channel_id"`
	MatchMode string          `json:"match_mode"` // display form of the root mode; the authoritative logic lives in Match
	Match     *AlertMatchNode `json:"match"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// AlertRulesHolder carries the parsed rules between the sync worker (writer)
// and the alert engine (reader).
type AlertRulesHolder struct {
	p atomic.Pointer[[]AlertRule]
}

// NewAlertRulesHolder starts empty.
func NewAlertRulesHolder() *AlertRulesHolder { return &AlertRulesHolder{} }

func (h *AlertRulesHolder) Get() []AlertRule {
	if r := h.p.Load(); r != nil {
		return *r
	}
	return nil
}

func (h *AlertRulesHolder) Set(r []AlertRule) { h.p.Store(&r) }

// LoadAlertRules reads and parses every rule. Unparsable/malformed rules are
// dropped with a log line (fail-safe: a broken rule never fires).
func LoadAlertRules(ctx context.Context, db *sql.DB) ([]AlertRule, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, name, enabled, scope_type, route_ids, channel_id, match_mode, conditions, updated_at
		 FROM alert_rules ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertRule
	for rows.Next() {
		var r AlertRule
		var routeIDs, conditions []byte
		var updated sql.NullTime
		if err := rows.Scan(&r.ID, &r.Name, &r.Enabled, &r.ScopeType, &routeIDs, &r.ChannelID, &r.MatchMode, &conditions, &updated); err != nil {
			log.Printf("sync: alert rule row unreadable: %v", err)
			continue
		}
		if updated.Valid {
			r.UpdatedAt = updated.Time
		}
		if r.ScopeType == "routes" && len(routeIDs) > 0 {
			_ = json.Unmarshal(routeIDs, &r.RouteIDs)
		}
		match, perr := ParseAlertMatch(conditions)
		if perr != nil {
			// Legacy rows may carry the flat conditions array as the root
			// document; wrap it into a group node.
			var legacy []snapshot.SuccessCondition
			if lerr := json.Unmarshal(conditions, &legacy); lerr == nil && len(legacy) > 0 {
				match = parseMatchLeafCompat(legacy, r.MatchMode)
			}
			if match == nil {
				log.Printf("sync: alert rule %q has invalid match document, dropped: %v", r.Name, perr)
				continue
			}
		}
		r.Match = match
		if r.ScopeType != "all" && r.ScopeType != "routes" {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ParseAlertConditions validates the shared condition document for alert
// rules: sources status | body | outcome (the verdict produced by the
// route's own success rule).
func ParseAlertConditions(raw []byte) ([]snapshot.SuccessCondition, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("conditions are required")
	}
	if len(trimmed) > 8192 {
		return nil, fmt.Errorf("conditions exceed 8 KiB")
	}
	var doc struct {
		Mode       string                `json:"mode"`
		Conditions []successConditionRaw `json:"conditions"`
	}
	if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
		return nil, fmt.Errorf("conditions are not valid JSON: %w", err)
	}
	switch doc.Mode {
	case "":
		doc.Mode = "all"
	case "all", "any":
	default:
		return nil, fmt.Errorf("match mode must be all or any, got %q", doc.Mode)
	}
	if len(doc.Conditions) == 0 {
		return nil, fmt.Errorf("at least one condition is required")
	}
	if len(doc.Conditions) > 16 {
		return nil, fmt.Errorf("at most 16 conditions")
	}
	out := make([]snapshot.SuccessCondition, 0, len(doc.Conditions))
	for i := range doc.Conditions {
		c := &doc.Conditions[i]
		switch c.Source {
		case "status", "outcome":
		case "body":
			if err := validateBodyPath(c.Path); err != nil {
				return nil, fmt.Errorf("condition %d: %w", i+1, err)
			}
		default:
			return nil, fmt.Errorf("condition %d: source must be status, body or outcome, got %q", i+1, c.Source)
		}
		if c.Source == "outcome" && c.Op != "in" && c.Op != "not_in" {
			// The verdict is a two-value enum: only membership operators are
			// meaningful (大于/包含 etc. are rejected).
			return nil, fmt.Errorf("condition %d: outcome source only supports in or not_in, got %q", i+1, c.Op)
		}
		switch c.Op {
		case "in", "not_in", "contains", "not_contains":
			if len(c.Values) > 64 {
				return nil, fmt.Errorf("condition %d: at most 64 values", i+1)
			}
		case "eq", "neq", "gt", "gte", "lt", "lte":
			if len(c.Values) != 1 {
				return nil, fmt.Errorf("condition %d: op %q takes exactly one value", i+1, c.Op)
			}
		default:
			return nil, fmt.Errorf("condition %d: unknown op %q", i+1, c.Op)
		}
		if len(c.Values) == 0 {
			return nil, fmt.Errorf("condition %d: values list is empty", i+1)
		}
		numericOnly := c.Op == "gt" || c.Op == "gte" || c.Op == "lt" || c.Op == "lte"
		normalized := make([]string, 0, len(c.Values))
		for _, rawVal := range c.Values {
			v := strings.TrimSpace(string(rawVal))
			if strings.HasPrefix(v, `"`) { // JSON string literal → unquote
				var sv string
				if json.Unmarshal(rawVal, &sv) != nil {
					return nil, fmt.Errorf("condition %d: value %s is not a valid string", i+1, v)
				}
				v = sv
			}
			if v == "" || len(v) > 256 {
				return nil, fmt.Errorf("condition %d: value must be 1..256 chars", i+1)
			}
			if c.Source == "status" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 100 || n > 599 {
					return nil, fmt.Errorf("condition %d: status value %q must be an HTTP status (100-599)", i+1, v)
				}
				v = strconv.Itoa(n)
			}
			if c.Source == "outcome" {
				v = strings.ToLower(v)
				if v != "success" && v != "failure" {
					return nil, fmt.Errorf("condition %d: outcome value %q must be success or failure", i+1, v)
				}
			}
			if numericOnly {
				if _, err := strconv.ParseFloat(v, 64); err != nil {
					return nil, fmt.Errorf("condition %d: op %q needs a numeric value, got %q", i+1, c.Op, v)
				}
			}
			normalized = append(normalized, v)
		}
		out = append(out, snapshot.SuccessCondition{
			Source: c.Source, Path: c.Path, Op: c.Op, Values: normalized,
		})
	}
	return out, nil
}

// InsertAlertEvent records one firing inside the caller's transaction.
func InsertAlertEvent(ctx context.Context, tx *sql.Tx, e AlertEvent) error {
	detail := "{}"
	if len(e.Detail) > 0 {
		detail = string(e.Detail)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO alert_events (rule_id, rule_name, channel_id, route_name, request_id, http_status, outcome, biz_code, biz_message, detail, sent, send_error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.RuleID, e.RuleName, e.ChannelID, e.RouteName, e.RequestID, e.Status, e.Outcome, e.BizCode, e.BizMessage, detail, e.Sent, e.SendError)
	return err
}

// ListAlertEvents returns the newest firings for the panel.
func ListAlertEvents(ctx context.Context, db *sql.DB, ruleID int64, limit int) ([]AlertEvent, error) {
	q := `SELECT id, rule_id, rule_name, channel_id, route_name, request_id, http_status, outcome, biz_code, biz_message, detail, sent, send_error, created_at
	      FROM alert_events`
	args := []any{}
	if ruleID > 0 {
		q += ` WHERE rule_id = ?`
		args = append(args, ruleID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertEvent
	for rows.Next() {
		var e AlertEvent
		var detail []byte
		var created sql.NullTime
		if err := rows.Scan(&e.ID, &e.RuleID, &e.RuleName, &e.ChannelID, &e.RouteName, &e.RequestID, &e.Status, &e.Outcome, &e.BizCode, &e.BizMessage, &detail, &e.Sent, &e.SendError, &created); err != nil {
			continue
		}
		if created.Valid {
			e.CreatedAt = created.Time
		}
		e.Detail = detail
		out = append(out, e)
	}
	return out, rows.Err()
}

// AlertEvent is one firing of an alert rule. The struct is marshaled into
// admin_audit rows, so the JSON keys must be snake_case.
type AlertEvent struct {
	ID         int64     `json:"id"`
	RuleID     int64     `json:"rule_id"`
	RuleName   string    `json:"rule_name"`
	ChannelID  int64     `json:"channel_id"`
	RouteName  string    `json:"route_name"`
	RequestID  string    `json:"request_id"`
	Status     int       `json:"http_status"`
	Outcome    string    `json:"outcome"`
	BizCode    string    `json:"biz_code"`
	BizMessage string    `json:"biz_message"`
	Detail     []byte    `json:"detail"`
	Sent       bool      `json:"sent"`
	SendError  string    `json:"send_error"`
	CreatedAt  time.Time `json:"created_at"`
}
