package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/authz"
	"github.com/BadmintonCode/routegate-gateway/internal/config"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

func testCfg() *config.Config {
	return &config.Config{
		ProxyEnabled: true, ClientAuth: config.ClientAuthCert,
		UpstreamKeepalive: true, InstanceID: "test",
		TrustedUserContextCallers: []string{"edge"},
	}
}

func testSnapshot() *snapshot.Snapshot {
	alpha := &snapshot.Route{
		ID: 1, Name: "alpha", PathPrefix: "/alpha", StripPrefix: true,
		Targets: []snapshot.Target{{Host: "127.0.0.1", Port: 1}},
	}
	beta := &snapshot.Route{
		ID: 2, Name: "beta", PathPrefix: "/beta", StripPrefix: true,
		Targets: []snapshot.Target{{Host: "127.0.0.1", Port: 1}},
	}
	snap := &snapshot.Snapshot{
		Revision: 7, Routes: []*snapshot.Route{beta, alpha}, // order irrelevant; loader sorts
		Policies: map[int64][]*snapshot.Policy{
			1: {
				{ID: 11, RouteID: 1, Caller: "demo-caller", Method: "GET", Segments: mustParse("/v1/**")},
				{ID: 12, RouteID: 1, Caller: "demo-caller", Method: "GET", Segments: mustParse("/admin/**"), Deny: true},
			},
			2: {
				{ID: 21, RouteID: 2, Caller: "demo-alpha", Method: "GET", Segments: mustParse("/v1/**")},
			},
		},
	}
	// Sort longest-prefix first like the loader does.
	snapshot.SortRoutes(snap)
	return snap
}

func mustParse(p string) []string {
	segs, err := authz.ParsePattern(p)
	if err != nil {
		panic(err)
	}
	return segs
}

func newTestServer(t *testing.T, snap *snapshot.Snapshot) *Server {
	t.Helper()
	holder := &snapshot.Holder{}
	holder.Store(snap)
	return NewServer(testCfg(), holder, accesslog.NewStdout(&strings.Builder{}), "test", accesslog.NewDebugManager(), store.NewBizFieldsHolder(), nil)
}

func TestEvaluateSemantics(t *testing.T) {
	snap := testSnapshot()
	alpha := snap.RouteByName("alpha")
	cases := []struct {
		caller, method, path string
		want                 string
	}{
		{"demo-caller", "GET", "/v1/ping", "allow"},
		{"demo-caller", "GET", "/v1/deep/nested", "allow"},
		{"demo-caller", "GET", "/admin/secret", "deny"}, // explicit deny
		{"demo-caller", "GET", "/other/x", "deny"},      // default deny
		{"demo-caller", "POST", "/v1/ping", "deny"},     // method mismatch
		{"nobody", "GET", "/v1/ping", "deny"},           // unknown caller
		{"demo-alpha", "GET", "/v1/ping", "deny"},       // no policy on alpha for this caller
	}
	for _, c := range cases {
		got, _ := evaluate(snap, alpha, c.caller, c.method, c.path)
		if got != c.want {
			t.Errorf("evaluate(%s %s %s) = %s, want %s", c.caller, c.method, c.path, got, c.want)
		}
	}
}

// TestPolicySeesNormalizedSubPath: dot-segment tricks in the raw path must not
// bypass a deny rule (ADR §17 acceptance).
func TestPolicySeesNormalizedSubPath(t *testing.T) {
	snap := testSnapshot()
	alpha := snap.RouteByName("alpha")
	// Raw request path /alpha/admin/../v1/ping normalizes to /alpha/v1/ping →
	// sub path /v1/ping → allow. And /alpha/v1/../admin/secret → /admin/secret → deny.
	if got, _ := evaluate(snap, alpha, "demo-caller", "GET", authz.NormalizePath("/alpha/admin/../v1/ping")[len("/alpha"):]); got != "allow" {
		t.Errorf("normalized allow path evaluated %q", got)
	}
	if got, _ := evaluate(snap, alpha, "demo-caller", "GET", authz.NormalizePath("/alpha/v1/../admin/secret")[len("/alpha"):]); got != "deny" {
		t.Errorf("normalized deny path evaluated %q", got)
	}
}

func TestStripedInboundHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("Forwarded", "for=1.2.3.4")
	r.Header.Set("X-Forwarded-Host", "evil.example")
	r.Header.Set("True-Client-IP", "1.2.3.4")
	r.Header.Set("X-Gateway-Caller", "attacker")
	r.Header.Set("X-Gateway-User-Id", "999")
	r.Header.Set("X-Authenticated-User-Id", "999")
	out := strippedHeaders(r)
	for _, h := range strippedInboundHeaders {
		if out.Get(h) != "" {
			t.Errorf("header %s not stripped", h)
		}
	}
}

// TestHeaderSmugglingStrippedEndToEnd: a client-supplied Forwarded /
// X-Forwarded-Host / True-Client-IP must never reach the upstream, while
// ordinary headers pass through untouched (ADR-0001 §11.4).
func TestHeaderSmugglingStrippedEndToEnd(t *testing.T) {
	var seen http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`ok`))
	}))
	defer up.Close()

	leaf, fp := callerCert(t)
	s, _, _, _ := newDebugTestServer(t, up.URL, fp, leaf, false, nil)

	req := httptest.NewRequest("GET", "/alpha/v1/echo", nil)
	req.Header.Set("Forwarded", "for=1.2.3.4")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("True-Client-IP", "1.2.3.4")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Gateway-Caller", "attacker")
	req.Header.Set("X-Custom-Trace", "keep-me")
	req.Header.Set("Accept", "application/json")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	s.ServeHTTP(httptest.NewRecorder(), req)

	if seen == nil {
		t.Fatal("request never reached the upstream")
	}
	for _, h := range []string{"Forwarded", "X-Forwarded-Host", "True-Client-Ip", "X-Forwarded-For"} {
		if got := seen.Get(h); got != "" {
			t.Errorf("smuggled header %s reached the upstream: %q", h, got)
		}
	}
	if got := seen.Get("X-Custom-Trace"); got != "keep-me" {
		t.Errorf("ordinary header X-Custom-Trace = %q, want keep-me", got)
	}
	if got := seen.Get("Accept"); got != "application/json" {
		t.Errorf("ordinary header Accept = %q, want application/json", got)
	}
}

func TestSanitizeRequestID(t *testing.T) {
	if got := sanitizeRequestID("req_ABC-123_.x"); got != "req_ABC-123_.x" {
		t.Errorf("valid id rejected: %q", got)
	}
	for _, bad := range []string{"", "with space", "<script>", strings.Repeat("x", 129), "a\nb"} {
		if got := sanitizeRequestID(bad); got != "" {
			t.Errorf("invalid id %q accepted as %q", bad, got)
		}
	}
}

func TestPickTargetWeighted(t *testing.T) {
	route := &snapshot.Route{Name: "w", Targets: []snapshot.Target{
		{Host: "a", Port: 1, Weight: 3},
		{Host: "b", Port: 1, Weight: 1},
	}}
	s := NewServer(testCfg(), &snapshot.Holder{}, accesslog.NewStdout(&strings.Builder{}), "t", accesslog.NewDebugManager(), store.NewBizFieldsHolder(), nil)
	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		tgt, err := s.pickTarget(route)
		if err != nil {
			t.Fatal(err)
		}
		counts[tgt.Host]++
	}
	if counts["a"] != 30 || counts["b"] != 10 {
		t.Errorf("weighted RR off: %v, want a=30 b=10", counts)
	}
}

func TestMatchPrefix(t *testing.T) {
	cases := []struct {
		prefix, path string
		want         bool
	}{
		{"/alpha", "/alpha", true},
		{"/alpha", "/alpha/v1", true},
		{"/alpha", "/alphabet", false}, // no partial-segment prefix
		{"/alpha", "/beta", false},
	}
	for _, c := range cases {
		if got := matchPrefix(c.prefix, c.path); got != c.want {
			t.Errorf("matchPrefix(%q,%q) = %v, want %v", c.prefix, c.path, got, c.want)
		}
	}
}

func TestServeHTTPFailClosedWithoutSnapshot(t *testing.T) {
	s := NewServer(testCfg(), &snapshot.Holder{}, accesslog.NewStdout(&strings.Builder{}), "t", accesslog.NewDebugManager(), store.NewBizFieldsHolder(), nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/alpha/v1/ping", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("empty snapshot: status = %d, want 503 (fail-closed)", w.Code)
	}
}

func TestServeHTTPRequiresTLS(t *testing.T) {
	s := newTestServer(t, testSnapshot())
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/alpha/v1/ping", nil) // r.TLS == nil
	s.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("plaintext to data plane: status = %d, want 400", w.Code)
	}
}
