package store

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/BadmintonCode/routegate-gateway/internal/authz"
)

func TestValidateOperationValid(t *testing.T) {
	cases := []struct{ method, path, wantMethod string }{
		{"GET", "/v1/users/**", "GET"},
		{"post", "/v1/users", "POST"},
		{"", "/health", "*"},
		{"*", "/v1/*", "*"},
	}
	for _, c := range cases {
		op, err := ValidateOperation(c.method, c.path)
		if err != nil {
			t.Fatalf("ValidateOperation(%q,%q) error: %v", c.method, c.path, err)
		}
		if op.Method != c.wantMethod {
			t.Errorf("ValidateOperation(%q,%q) method = %q, want %q", c.method, c.path, op.Method, c.wantMethod)
		}
	}
}

func TestValidateOperationInvalid(t *testing.T) {
	cases := []struct{ method, path string }{
		{"FETCH", "/v1"},
		{"GET", ""},
		{"GET", "/v1/**/x"},     // '**' not last
		{"GET", "/v1/te*t"},     // wildcard inside literal
		{"GET", "v1"},           // must start with '/'
		{"GET", "/"},            // must match at least one segment
		{"GET", "/v1/x/"},       // trailing slash: normalized form differs
		{"GET", "/v1/%2e%2e/x"}, // percent-encoded dot segment
	}
	for _, c := range cases {
		if _, err := ValidateOperation(c.method, c.path); err == nil {
			t.Errorf("ValidateOperation(%q,%q) expected error, got none", c.method, c.path)
		}
	}
}

// TestValidateNormalizedPath: the write-time grammar rule — a stored prefix,
// pattern, or operation path must already equal its normalized form, because
// the matcher only ever compares normalized request paths. '**' segments stay
// intact, so clean wildcard patterns pass.
func TestValidateNormalizedPath(t *testing.T) {
	for _, ok := range []string{"/v1/users", "/v1/users/123", "/v1/**", "/v1/*/orders", "/health"} {
		if err := ValidateNormalizedPath("path prefix", ok); err != nil {
			t.Errorf("ValidateNormalizedPath(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"/v1/users/",     // trailing slash
		"//v1/users",     // empty segment
		"/v1/./users",    // current-dir segment
		"/v1/../users",   // parent segment
		"/v1/%2e%2e/etc", // percent-encoded dot segment
		"/v1/us%65rs",    // percent-encoded letter: decoded form differs
	} {
		err := ValidateNormalizedPath("path prefix", bad)
		if err == nil {
			t.Errorf("ValidateNormalizedPath(%q) = nil, want a normalization error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "must be normalized") || !strings.Contains(err.Error(), "remove trailing slashes") {
			t.Errorf("ValidateNormalizedPath(%q) error %q is not the friendly message", bad, err)
		}
	}
}

// TestValidateRouteNormalizedPrefix: un-normalized prefixes are rejected with
// the got/want message; a clean prefix is accepted.
func TestValidateRouteNormalizedPrefix(t *testing.T) {
	targets := []byte(`[{"host":"svc","port":8080,"weight":1}]`)
	for _, bad := range []string{"/v1/users/", "/v1/users/../admin", "/v1/%2e%2e/etc"} {
		_, verr := ValidateRoute(bad, targets, 0)
		if verr == nil {
			t.Errorf("ValidateRoute(%q) accepted an un-normalized prefix", bad)
			continue
		}
		if !verr.BadPrefix || verr.PrefixWant == "" {
			t.Errorf("ValidateRoute(%q) = %+v, want BadPrefix with got/want", bad, verr)
		}
		want := authz.NormalizePath(bad)
		if verr.PrefixWant != want {
			t.Errorf("ValidateRoute(%q) PrefixWant = %q, want %q", bad, verr.PrefixWant, want)
		}
	}
	if _, verr := ValidateRoute("/v1/users", targets, 0); verr != nil {
		t.Errorf("clean prefix rejected: %v", verr)
	}
}

// TestNormalizeLegacyPrefix: the loader tolerates rows written before
// write-time normalization by loading the normalized form and logging it; a
// clean prefix passes through untouched and silently.
func TestNormalizeLegacyPrefix(t *testing.T) {
	var buf bytes.Buffer
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	}()

	if got := normalizeLegacyPrefix(7, "legacy", "/v1/x/"); got != "/v1/x" {
		t.Errorf("legacy prefix loaded as %q, want %q", got, "/v1/x")
	}
	if !strings.Contains(buf.String(), `route 7 (legacy) has a non-normalized path prefix "/v1/x/"; loading as "/v1/x"`) {
		t.Errorf("tolerant load did not log: %q", buf.String())
	}

	buf.Reset()
	if got := normalizeLegacyPrefix(8, "clean", "/v1/users"); got != "/v1/users" {
		t.Errorf("clean prefix changed: %q", got)
	}
	if buf.Len() != 0 {
		t.Errorf("clean prefix must not log, got %q", buf.String())
	}
}

// TestCleanPrefixMatchesNormalizedRequests: end-to-end shape of the fix — a
// route saved with a clean prefix is accepted and matches the normalized
// request paths the proxy matcher sees (the same prefix/suffix test
// proxy.matchPrefix performs).
func TestCleanPrefixMatchesNormalizedRequests(t *testing.T) {
	const prefix = "/v1/users"
	targets := []byte(`[{"host":"svc","port":8080,"weight":1}]`)
	if _, verr := ValidateRoute(prefix, targets, 0); verr != nil {
		t.Fatalf("clean prefix rejected: %v", verr)
	}
	for _, raw := range []string{"/v1/users", "/v1/users/123", "//v1//users/9/"} {
		norm := authz.NormalizePath(raw)
		if !(norm == prefix || strings.HasPrefix(norm, prefix+"/")) {
			t.Errorf("clean prefix %q does not match normalized request %q", prefix, norm)
		}
	}
	// The rejected un-normalized spelling is exactly the one that would never
	// have matched anything: normalized request paths carry no trailing slash
	// and no empty segments, so matchPrefix against "/v1/users/" is always
	// false.
	stale := "/v1/users/"
	for _, raw := range []string{"/v1/users", "/v1/users/123"} {
		norm := authz.NormalizePath(raw)
		if stale == norm || strings.HasPrefix(norm, stale+"/") {
			t.Errorf("un-normalized prefix %q unexpectedly matched normalized request %q", stale, norm)
		}
	}
}

// TestPinSessionTimezone: the DSN gains time_zone='+00:00' so NOW() agrees
// with the UTC-written DATETIMEs; an existing time_zone is respected, a
// non-UTC loc is left untouched (warned), and unparsable DSNs pass through.
func TestPinSessionTimezone(t *testing.T) {
	pinned := pinSessionTimezone("user:pw@tcp(host:3306)/db")
	cfg, err := mysql.ParseDSN(pinned)
	if err != nil {
		t.Fatalf("pinned DSN unparsable: %v", err)
	}
	if got := cfg.Params["time_zone"]; got != "'+00:00'" {
		t.Errorf("time_zone param = %q, want '+00:00'", got)
	}

	respected := pinSessionTimezone("user:pw@tcp(host:3306)/db?time_zone=%27%2B08%3A00%27")
	if respected != "user:pw@tcp(host:3306)/db?time_zone=%27%2B08%3A00%27" {
		t.Errorf("explicit time_zone must not be touched, got %q", respected)
	}

	local := pinSessionTimezone("user:pw@tcp(host:3306)/db?loc=Local")
	if local != "user:pw@tcp(host:3306)/db?loc=Local" {
		t.Errorf("explicit non-UTC loc must not be touched, got %q", local)
	}

	if got := pinSessionTimezone("not-a-dsn"); got != "not-a-dsn" {
		t.Errorf("unparsable DSN must pass through, got %q", got)
	}

	// UTC loc stays consistent with the pin.
	utc := pinSessionTimezone("user:pw@tcp(host:3306)/db?parseTime=true&loc=UTC")
	cfg2, err := mysql.ParseDSN(utc)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Params["time_zone"] != "'+00:00'" {
		t.Errorf("UTC loc should still pin time_zone, got %q", cfg2.Params["time_zone"])
	}
}

// TestSessionSurvivesGC is the regression test for the timezone bug: a
// session inserted the same way the admin plane does it must survive the GC
// query even when the server's session time zone is not UTC. Skipped unless
// ROUTEGATE_TEST_MYSQL_DSN points at a scratch database.
func TestSessionSurvivesGC(t *testing.T) {
	dsn := os.Getenv("ROUTEGATE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("ROUTEGATE_TEST_MYSQL_DSN not set; requires a scratch MySQL database")
	}
	db, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Prove the server session zone is what the fix is about.
	var tz string
	if err := db.QueryRow(`SELECT @@session.time_zone`).Scan(&tz); err != nil {
		t.Fatal(err)
	}
	if tz != "+00:00" {
		t.Logf("note: server session time_zone is %q (fix pins the DSN to +00:00)", tz)
	}

	now := time.Now()
	_, err = db.Exec(
		`INSERT INTO sessions (user_id, token_hash, expires_at, absolute_expires_at, ip, user_agent)
		 VALUES (1, 'tz-fix-test-hash', ?, ?, '127.0.0.1', 'tz-fix-test')`,
		now.Add(8*time.Hour), now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM sessions WHERE token_hash = 'tz-fix-test-hash'`)
	})

	// Frame alignment: with the session zone pinned to UTC, the 8h sliding
	// expiry sits ~28800s ahead of NOW() — not the ~1s skew of the bug.
	var diff int64
	if err := db.QueryRow(`SELECT TIMESTAMPDIFF(SECOND, NOW(), expires_at) FROM sessions WHERE token_hash = 'tz-fix-test-hash'`).Scan(&diff); err != nil {
		t.Fatal(err)
	}
	if diff < 28800-10 || diff > 28800+10 {
		t.Fatalf("expires_at is %ds ahead of NOW(), want ~28800s (session zone not aligned with written values)", diff)
	}

	// The exact GC predicate from the sync worker.
	if _, err := db.Exec(`DELETE FROM sessions WHERE expires_at < NOW() OR absolute_expires_at < NOW() OR revoked_at IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = 'tz-fix-test-hash'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("fresh session was deleted by the GC predicate — timezone pin ineffective")
	}
}

// TestLoadSnapshotWithBizFields is a live regression for the loader's column
// wiring: a migrated scratch DB with one route (rule + biz paths) must load
// into a snapshot with everything intact. Skipped unless
// ROUTEGATE_TEST_MYSQL_DSN points at an EMPTY scratch database (the test
// migrates it and cleans up).
func TestLoadSnapshotWithBizFields(t *testing.T) {
	dsn := os.Getenv("ROUTEGATE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("ROUTEGATE_TEST_MYSQL_DSN not set; requires an empty scratch MySQL database")
	}
	db, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := t.Context()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM routes`)
	})
	if _, err := db.Exec(`INSERT INTO routes (name, path_prefix, targets, strip_prefix, success_rule, biz_code_path, biz_message_path)
		VALUES ('r1', '/r1', '[{"host":"127.0.0.1","port":18099,"weight":1}]', 1,
		'{"mode":"all","conditions":[{"source":"status","op":"in","values":["200"]}]}', 'code', 'message')`); err != nil {
		t.Fatal(err)
	}
	snap, errs, err := LoadSnapshot(ctx, db, 8080)
	if err != nil {
		t.Fatal(err)
	}
	if errs.Total() != 0 {
		t.Fatalf("load errors: %+v", errs)
	}
	if len(snap.Routes) != 1 {
		t.Fatalf("routes = %d, want 1", len(snap.Routes))
	}
	r := snap.Routes[0]
	if r.SuccessRule == nil || r.SuccessRule.Mode != "all" {
		t.Errorf("success rule lost: %+v", r.SuccessRule)
	}
	if r.BizCodePath != "code" || r.BizMessagePath != "message" {
		t.Errorf("biz paths lost: %q / %q", r.BizCodePath, r.BizMessagePath)
	}
}

// TestLoadSnapshotNormalizesLegacyPrefix is the live regression for tolerant
// loading: a row stored with a trailing-slash prefix (pre-normalization data)
// must load with the clean prefix and no load errors. Skipped unless
// ROUTEGATE_TEST_MYSQL_DSN points at an EMPTY scratch database (the test
// migrates it and cleans up).
func TestLoadSnapshotNormalizesLegacyPrefix(t *testing.T) {
	dsn := os.Getenv("ROUTEGATE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("ROUTEGATE_TEST_MYSQL_DSN not set; requires an empty scratch MySQL database")
	}
	db, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := t.Context()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM routes`)
	})
	if _, err := db.Exec(`INSERT INTO routes (name, path_prefix, targets, strip_prefix)
		VALUES ('legacy', '/v1/x/', '[{"host":"127.0.0.1","port":18099,"weight":1}]', 1)`); err != nil {
		t.Fatal(err)
	}
	snap, errs, err := LoadSnapshot(ctx, db, 8080)
	if err != nil {
		t.Fatal(err)
	}
	if errs.Total() != 0 {
		t.Fatalf("legacy route was dropped at load: %+v", errs)
	}
	if len(snap.Routes) != 1 {
		t.Fatalf("routes = %d, want 1", len(snap.Routes))
	}
	if got := snap.Routes[0].PathPrefix; got != "/v1/x" {
		t.Errorf("legacy prefix loaded as %q, want normalized %q", got, "/v1/x")
	}
}
