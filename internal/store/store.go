// Package store owns MySQL access: embedded migrations, config-revision
// reads/writes, and the consistent snapshot load that feeds the data plane
// (ADR-0001 §3.2, §4).
package store

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/BadmintonCode/routegate-gateway/internal/authz"
	"github.com/BadmintonCode/routegate-gateway/internal/identity"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/migrations"
)

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Open connects to MySQL with sane driver defaults.
func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", pinSessionTimezone(dsn))
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}

// pinSessionTimezone appends the time_zone='+00:00' system variable to the
// DSN unless the DSN already pins one (ADR-0001 §13 DSN guidance).
//
// Why: the driver writes DATETIME parameters as UTC wall-clock strings
// (default loc=UTC), while MySQL evaluates NOW() in the session time zone.
// On a server whose session zone is not UTC (e.g. SYSTEM=+08:00),
// `expires_at < NOW()` in the admin-session GC deletes freshly created
// sessions within one sync tick, and policy expiry checks skew the same way.
// Pinning the session zone to UTC makes NOW() agree with the written values.
// An explicit non-UTC loc is respected but warned about, since it reintroduces
// the skew unless time_zone is pinned to match.
func pinSessionTimezone(dsn string) string {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return dsn // keep the original DSN; Open reports the parse error
	}
	if _, ok := cfg.Params["time_zone"]; ok {
		return dsn
	}
	if cfg.Loc != nil && cfg.Loc != time.UTC {
		log.Printf("store: DSN sets loc=%s without time_zone; NOW() runs in the server session zone while values are written in %s — pin time_zone in the DSN to avoid expiring fresh sessions", cfg.Loc, cfg.Loc)
		return dsn
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+00:00'"
	return cfg.FormatDSN()
}

// Migrate applies the embedded migrations in order, tracked in
// schema_migrations.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version VARCHAR(32) NOT NULL PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		return fmt.Errorf("store: ensure schema_migrations: %w", err)
	}
	applied := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("store: list migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	names, err := migrations.Names()
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		if applied[name] {
			continue
		}
		raw, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("store: read %s: %w", name, err)
		}
		body := string(raw)
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, stmt := range splitStatements(body) {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("store: apply %s: %w", name, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version) VALUES (?)`, name); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: record %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// CurrentRevision reads settings.config_revision; missing row => 0.
func CurrentRevision(ctx context.Context, q querier) (int64, error) {
	var raw sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT JSON_UNQUOTE(JSON_EXTRACT(value, '$')) FROM settings WHERE name = 'config_revision'`).Scan(&raw)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var rev int64
	if raw.Valid {
		if _, err := fmt.Sscanf(raw.String, "%d", &rev); err != nil {
			return 0, fmt.Errorf("store: config_revision unparsable: %q", raw.String)
		}
	}
	return rev, nil
}

// BumpRevision increments config_revision by one (break-glass `config bump`).
func BumpRevision(ctx context.Context, db *sql.DB) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO settings (name, value) VALUES ('config_revision', CAST(1 AS JSON))
		 ON DUPLICATE KEY UPDATE value = CAST(CAST(JSON_EXTRACT(value, '$') AS UNSIGNED) + 1 AS JSON)`); err != nil {
		return 0, err
	}
	rev, err := CurrentRevision(ctx, tx)
	if err != nil {
		return 0, err
	}
	return rev, tx.Commit()
}

// LoadErrors counts rows dropped during a snapshot load.
type LoadErrors struct {
	BadTargets     int
	Dangling       int // policy referencing a missing/disabled route
	BadPattern     int
	MissingExpiry  int
	BadIdentity    int
	SelfLoop       int
	BadOperations  int // malformed or orphaned route_operations row
	BadSuccessRule int // malformed routes.success_rule JSON
}

func (e LoadErrors) Total() int {
	return e.BadTargets + e.Dangling + e.BadPattern + e.MissingExpiry + e.BadIdentity + e.SelfLoop + e.BadOperations + e.BadSuccessRule
}

// LoadSnapshot reads routes, policies, and identities inside one transaction
// (InnoDB REPEATABLE READ supplies a consistent view), validates each row, and
// returns a fully built snapshot. Invalid rows are dropped and counted — a
// single dirty row never blocks the whole snapshot (ADR-0001 §3.2).
func LoadSnapshot(ctx context.Context, db *sql.DB, ownProxyPort int) (*snapshot.Snapshot, LoadErrors, error) {
	var errs LoadErrors
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errs, err
	}
	defer tx.Rollback()
	now := time.Now()
	snap := &snapshot.Snapshot{
		Policies:   map[int64][]*snapshot.Policy{},
		Identities: map[string]*snapshot.Identity{},
		LoadedAt:   now,
	}

	// Routes.
	routeRows, err := tx.QueryContext(ctx,
		`SELECT id, name, path_prefix, methods, targets, strip_prefix, success_rule, biz_code_path, biz_message_path FROM routes WHERE enabled = 1`)
	if err != nil {
		return nil, errs, err
	}
	routeByID := map[int64]*snapshot.Route{}
	for routeRows.Next() {
		var (
			id          int64
			name        string
			prefix      string
			methods     sql.NullString
			targetsJSON []byte
			strip       bool
			ruleJSON    []byte
			bizCode     sql.NullString
			bizMsg      sql.NullString
		)
		if err := routeRows.Scan(&id, &name, &prefix, &methods, &targetsJSON, &strip, &ruleJSON, &bizCode, &bizMsg); err != nil {
			routeRows.Close()
			return nil, errs, err
		}
		// Legacy rows may predate write-time normalization: load them with the
		// normalized prefix and say so, so existing databases keep working; a
		// panel re-save then pins the clean form.
		prefix = normalizeLegacyPrefix(id, name, prefix)
		targets, rerr := ValidateRoute(prefix, targetsJSON, ownProxyPort)
		if rerr != nil {
			errs.BadTargets++
			continue
		}
		r := &snapshot.Route{
			ID: id, Name: name, PathPrefix: prefix,
			Targets: targets, StripPrefix: strip,
		}
		if rule, rerr := ParseSuccessRule(ruleJSON); rerr != nil {
			errs.BadSuccessRule++ // route still serves; it just has no verdict
		} else {
			r.SuccessRule = rule
		}
		// Business-envelope paths: written paths were validated on insert;
		// defensively drop anything malformed that reached the DB.
		if bizCode.Valid && validateBodyPath(bizCode.String) == nil {
			r.BizCodePath = bizCode.String
		}
		if bizMsg.Valid && validateBodyPath(bizMsg.String) == nil {
			r.BizMessagePath = bizMsg.String
		}
		if methods.Valid && strings.TrimSpace(methods.String) != "" {
			for _, m := range strings.Split(methods.String, ",") {
				if m = strings.ToUpper(strings.TrimSpace(m)); m != "" {
					r.Methods = append(r.Methods, m)
				}
			}
		}
		routeByID[id] = r
	}
	routeRows.Close()
	if err := routeRows.Err(); err != nil {
		return nil, errs, err
	}
	for _, r := range routeByID {
		snap.Routes = append(snap.Routes, r)
	}
	sort.Slice(snap.Routes, func(i, j int) bool {
		if len(snap.Routes[i].PathPrefix) != len(snap.Routes[j].PathPrefix) {
			return len(snap.Routes[i].PathPrefix) > len(snap.Routes[j].PathPrefix)
		}
		return snap.Routes[i].PathPrefix < snap.Routes[j].PathPrefix
	})

	// Policies (ordered by id for deterministic snapshots).
	polRows, err := tx.QueryContext(ctx,
		`SELECT id, route_id, caller, method, path_pattern, effect, expires_at, remark
		 FROM policies WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, errs, err
	}
	for polRows.Next() {
		var (
			id, routeID    int64
			caller, method string
			pattern        string
			effect         string
			expires        sql.NullTime
			remark         string
		)
		if err := polRows.Scan(&id, &routeID, &caller, &method, &pattern, &effect, &expires, &remark); err != nil {
			polRows.Close()
			return nil, errs, err
		}
		if _, ok := routeByID[routeID]; !ok {
			errs.Dangling++
			continue
		}
		segs, perr := authz.ParsePattern(pattern)
		if perr != nil {
			errs.BadPattern++
			continue
		}
		temporary := caller == "*" || (len(segs) == 1 && segs[0] == "**")
		if temporary && (!expires.Valid || !expires.Time.After(now)) {
			// Wide rules must carry a future expiry (ADR-0001 §6.1 discipline).
			errs.MissingExpiry++
			continue
		}
		if expires.Valid && !expires.Time.After(now) {
			continue // expired: not loaded; the panel marks it stale
		}
		pol := &snapshot.Policy{
			ID: id, RouteID: routeID, Caller: caller,
			Method: strings.ToUpper(method), Segments: segs,
			Deny:        effect == "deny",
			PathPattern: pattern, Effect: effect, Remark: remark,
		}
		if expires.Valid {
			exp := expires.Time
			pol.ExpiresAt = &exp
		}
		snap.Policies[routeID] = append(snap.Policies[routeID], pol)
	}
	polRows.Close()
	if err := polRows.Err(); err != nil {
		return nil, errs, err
	}

	// Registered operations of enabled routes (authoring metadata; inert for
	// proxy matching). Ordered for deterministic snapshots; malformed or
	// orphaned direct-DB rows are dropped and counted.
	opRows, err := tx.QueryContext(ctx,
		`SELECT id, route_id, method, path, name, remark
		 FROM route_operations ORDER BY route_id, method, path, id`)
	if err != nil {
		return nil, errs, err
	}
	for opRows.Next() {
		var (
			id, routeID  int64
			method, path string
			name, remark string
		)
		if err := opRows.Scan(&id, &routeID, &method, &path, &name, &remark); err != nil {
			opRows.Close()
			return nil, errs, err
		}
		route, ok := routeByID[routeID]
		if !ok {
			errs.BadOperations++ // orphan: route missing or disabled
			continue
		}
		if _, verr := ValidateOperation(method, path); verr != nil {
			errs.BadOperations++
			continue
		}
		route.Operations = append(route.Operations, &snapshot.Operation{
			ID: id, RouteID: routeID,
			Method: strings.ToUpper(method), Path: path,
			Name: name, Remark: remark,
		})
	}
	opRows.Close()
	if err := opRows.Err(); err != nil {
		return nil, errs, err
	}

	// Identities (active mtls_cert only; JWT public keys join with M2).
	idRows, err := tx.QueryContext(ctx,
		`SELECT name, key_id, credential_type, public_material, not_before, not_after
		 FROM identities WHERE status = 'active'`)
	if err != nil {
		return nil, errs, err
	}
	for idRows.Next() {
		var (
			name, keyID, ctype string
			pemStr             string
			nb, na             sql.NullTime
		)
		if err := idRows.Scan(&name, &keyID, &ctype, &pemStr, &nb, &na); err != nil {
			idRows.Close()
			return nil, errs, err
		}
		if ctype != "mtls_cert" {
			continue
		}
		cert, cerr := parseCertPEM([]byte(pemStr))
		if cerr != nil {
			errs.BadIdentity++
			continue
		}
		fp := identity.Fingerprint(cert)
		entry := &snapshot.Identity{Name: name, KeyID: keyID, Cert: cert, Fingerprint: fp}
		if nb.Valid {
			entry.NotBefore = nb.Time
		}
		if na.Valid {
			entry.NotAfter = na.Time
		}
		snap.Identities[fp] = entry
	}
	idRows.Close()
	if err := idRows.Err(); err != nil {
		return nil, errs, err
	}

	snap.Revision, err = CurrentRevision(ctx, tx)
	if err != nil {
		return nil, errs, err
	}
	return snap, errs, tx.Commit()
}

// RouteValidation classifies why a route row was rejected.
type RouteValidation struct {
	BadPrefix  bool
	PrefixGot  string // set when BadPrefix is a normalization violation
	PrefixWant string
	BadTargets bool
	SelfLoop   bool
}

func (v *RouteValidation) Error() string {
	switch {
	case v.SelfLoop:
		return "route targets the gateway's own listener (loop)"
	case v.BadPrefix && v.PrefixWant != "":
		return fmt.Sprintf("path prefix must be normalized: got %q, want %q — remove trailing slashes and dot segments", v.PrefixGot, v.PrefixWant)
	case v.BadPrefix:
		return "path prefix must start with '/', must not contain wildcards, and cannot be '/'"
	default:
		return "targets must be valid JSON like [{\"host\":\"svc\",\"port\":8080,\"weight\":1}]"
	}
}

// Operation is one registered operation of a route (owner request 2026-11-06):
// the concrete method + path pattern the upstream supports, offered as
// selectable options when authoring policies. Enforcement stays with
// policies; this registry is an authoring affordance stored in its own
// route_operations table.
type Operation struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Name   string `json:"name,omitempty"`
	Remark string `json:"remark,omitempty"`
}

// ValidateOperation normalizes and validates a single registered operation:
// method defaults to '*' and must be '*' or a known HTTP method; path must
// satisfy the segment pattern grammar (authz.ParsePattern — wildcards like
// '/v1/users/*' and '/v1/**' allowed, '**' only as the last segment).
// Uniqueness within a route is enforced by the route_operations unique key.
func ValidateOperation(method, path string) (*Operation, error) {
	if method == "" {
		method = "*"
	}
	method = strings.ToUpper(method)
	if method != "*" {
		switch method {
		case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS":
		default:
			return nil, fmt.Errorf("method must be '*' or a known HTTP method, got %q", method)
		}
	}
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	if _, err := authz.ParsePattern(path); err != nil {
		return nil, err
	}
	// Same write-time rule as route prefixes: the stored path must equal its
	// normalized form, otherwise it can never match a normalized request path.
	if err := ValidateNormalizedPath("path", path); err != nil {
		return nil, err
	}
	return &Operation{Method: method, Path: path}, nil
}

// ValidateNormalizedPath enforces the write-time rule that stored path
// prefixes, patterns, and operation paths are already in normalized form
// (authz.NormalizePath: one percent-decode pass, dot segments removed, empty
// segments collapsed; '**' wildcard segments stay intact). The matcher only
// ever compares normalized request paths, so a stored value that differs from
// its normalized form would silently never match — reject it instead of
// accepting a route that can never fire.
func ValidateNormalizedPath(kind, raw string) error {
	if norm := authz.NormalizePath(raw); norm != raw {
		return fmt.Errorf("%s must be normalized: got %q, want %q — remove trailing slashes and dot segments", kind, raw, norm)
	}
	return nil
}

// normalizeLegacyPrefix returns the prefix to load for a stored route row.
// Rows written before write-time normalization may carry a trailing slash or
// dot segments, which the matcher (over normalized request paths) would never
// match; such rows load normalized, with a log line, so existing databases
// keep working. A panel re-save then pins the clean form.
func normalizeLegacyPrefix(id int64, name, prefix string) string {
	norm := authz.NormalizePath(prefix)
	if norm != prefix {
		log.Printf("store: route %d (%s) has a non-normalized path prefix %q; loading as %q (re-save the route in the panel to pin the normalized form)", id, name, prefix, norm)
	}
	return norm
}

// ValidateRoute applies the shared route rules (ADR-0001 §3.2, §7.2): explicit
// prefix whitelist plus target JSON parsing and self-loop rejection. Used by
// both the snapshot loader and the Admin API so the rules cannot drift.
func ValidateRoute(prefix string, targetsJSON []byte, ownProxyPort int) ([]snapshot.Target, *RouteValidation) {
	if prefix == "" || prefix == "/" || !strings.HasPrefix(prefix, "/") ||
		strings.ContainsAny(prefix, "*?") {
		return nil, &RouteValidation{BadPrefix: true}
	}
	if err := ValidateNormalizedPath("path prefix", prefix); err != nil {
		return nil, &RouteValidation{
			BadPrefix:  true,
			PrefixGot:  prefix,
			PrefixWant: authz.NormalizePath(prefix),
		}
	}
	targets, err := parseTargets(targetsJSON)
	if err != nil {
		return nil, &RouteValidation{BadTargets: true}
	}
	if isSelfLoop(targets, ownProxyPort) {
		return nil, &RouteValidation{SelfLoop: true}
	}
	return targets, nil
}

func parseTargets(raw []byte) ([]snapshot.Target, error) {
	var targets []snapshot.Target
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, fmt.Errorf("empty targets")
	}
	if err := json.Unmarshal([]byte(trimmed), &targets); err != nil {
		return nil, err
	}
	for i := range targets {
		if targets[i].Host == "" || targets[i].Port <= 0 || targets[i].Port > 65535 {
			return nil, fmt.Errorf("invalid target %d", i)
		}
		if targets[i].Weight <= 0 {
			targets[i].Weight = 1
		}
	}
	return targets, nil
}

func parseCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	rest := pemBytes
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			return nil, fmt.Errorf("no CERTIFICATE block")
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
		rest = remaining
	}
}

// isSelfLoop rejects targets that point back at this gateway's own data-plane
// listener (ADR-0001 §3.2 loop protection).
func isSelfLoop(targets []snapshot.Target, ownPort int) bool {
	if ownPort == 0 {
		return false
	}
	for _, t := range targets {
		if t.Port != ownPort {
			continue
		}
		h := strings.ToLower(t.Host)
		if h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "0.0.0.0" ||
			strings.HasPrefix(h, "127.") {
			return true
		}
	}
	return false
}

// splitStatements splits a migration script into individual statements on
// semicolons that terminate a statement, ignoring semicolons inside quoted
// strings and stripping comment-only fragments. The embedded migrations use
// no procedures or triggers, so this simple scanner is sufficient and avoids
// requiring multiStatements=true on the DSN.
func splitStatements(script string) []string {
	var stmts []string
	var cur strings.Builder
	inSquote, inDquote, inBacktick, inComment, inLineComment := false, false, false, false, false
	for i := 0; i < len(script); i++ {
		c := script[i]
		next := byte(0)
		if i+1 < len(script) {
			next = script[i+1]
		}
		switch {
		case inLineComment:
			if c == '\n' {
				inLineComment = false
				cur.WriteByte(c)
			}
		case inComment:
			if c == '*' && next == '/' {
				inComment = false
				i++
			}
		case inSquote:
			if c == '\\' {
				i++
			} else if c == '\'' {
				inSquote = false
			}
			cur.WriteByte(c)
		case inDquote:
			if c == '\\' {
				i++
			} else if c == '"' {
				inDquote = false
			}
			cur.WriteByte(c)
		case inBacktick:
			if c == '`' {
				inBacktick = false
			}
			cur.WriteByte(c)
		case c == '-' && next == '-':
			inLineComment = true
			i++
		case c == '/' && next == '*':
			inComment = true
			i++
		case c == '\'':
			inSquote = true
			cur.WriteByte(c)
		case c == '"':
			inDquote = true
			cur.WriteByte(c)
		case c == '`':
			inBacktick = true
			cur.WriteByte(c)
		case c == ';':
			stmts = append(stmts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	stmts = append(stmts, cur.String())
	// Drop fragments that are pure comments/whitespace.
	out := make([]string, 0, len(stmts))
	for _, s := range stmts {
		s = stripComments(s)
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func stripComments(s string) string {
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "--") || strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "*") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}
