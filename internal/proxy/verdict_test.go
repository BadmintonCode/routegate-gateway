package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

func rule(mode string, conditions ...*snapshot.SuccessCondition) *snapshot.SuccessRule {
	return &snapshot.SuccessRule{Mode: mode, Conditions: conditions}
}

func cond(source, path, op string, values ...string) *snapshot.SuccessCondition {
	return &snapshot.SuccessCondition{Source: source, Path: path, Op: op, Values: values}
}

func TestEvaluateVerdict(t *testing.T) {
	cases := []struct {
		name   string
		rule   *snapshot.SuccessRule
		status int
		body   string
		want   string
	}{
		{"status in matches", rule("all", cond("status", "", "in", "200", "201")), 200, "", "success"},
		{"status in rejects", rule("all", cond("status", "", "in", "200")), 500, "", "failure"},
		{"status not_in passes", rule("all", cond("status", "", "not_in", "500")), 200, "", "success"},
		{"status not_in rejects", rule("all", cond("status", "", "not_in", "500")), 500, "", "failure"},
		{"body code in matches number", rule("all", cond("body", "code", "in", "0")), 200, `{"code":0}`, "success"},
		{"body code in rejects other", rule("all", cond("body", "code", "in", "0")), 200, `{"code":1}`, "failure"},
		{"body code missing fails in", rule("all", cond("body", "code", "in", "0")), 200, `{}`, "failure"},
		{"body non-json fails in", rule("all", cond("body", "code", "in", "0")), 200, `Service Unavailable`, "failure"},
		{"body missing passes not_in", rule("all", cond("body", "code", "not_in", "500")), 200, `{}`, "success"},
		{"body not_in rejects", rule("all", cond("body", "code", "not_in", "500")), 200, `{"code":"500"}`, "failure"},
		{"nested path string", rule("all", cond("body", "data.status", "in", "OK")), 200, `{"data":{"status":"OK"}}`, "success"},
		{"array index path", rule("all", cond("body", "items.0.name", "in", "n")), 200, `{"items":[{"name":"n"}]}`, "success"},
		{"array leaf compares as json", rule("all", cond("body", "tags", "in", `["a"]`)), 200, `{"tags":["a"]}`, "success"},
		{"all mode both must hold", rule("all", cond("status", "", "in", "200"), cond("body", "code", "in", "0")), 200, `{"code":1}`, "failure"},
		{"all mode success", rule("all", cond("status", "", "in", "200"), cond("body", "code", "in", "0")), 200, `{"code":0}`, "success"},
		{"any mode second holds", rule("any", cond("status", "", "in", "200"), cond("body", "code", "in", "0")), 500, `{"code":0}`, "success"},
		{"any mode none holds", rule("any", cond("status", "", "in", "200"), cond("body", "code", "in", "0")), 500, `{"code":1}`, "failure"},
		{"empty body stringifies null", rule("all", cond("body", "code", "not_in", "null")), 200, `{"code":null}`, "failure"},
		{"eq matches", rule("all", cond("body", "code", "eq", "0")), 200, `{"code":0}`, "success"},
		{"eq missing fails", rule("all", cond("body", "code", "eq", "0")), 200, `{}`, "failure"},
		{"neq missing passes", rule("all", cond("body", "code", "neq", "500")), 200, `{}`, "success"},
		{"neq different passes", rule("all", cond("body", "code", "neq", "500")), 200, `{"code":1}`, "success"},
		{"neq same fails", rule("all", cond("body", "code", "neq", "1")), 200, `{"code":1}`, "failure"},
		{"gt numeric passes", rule("all", cond("body", "elapsed_ms", "gt", "100")), 200, `{"elapsed_ms":150}`, "success"},
		{"gt numeric fails", rule("all", cond("body", "elapsed_ms", "gt", "100")), 200, `{"elapsed_ms":50}`, "failure"},
		{"gt non-numeric fails", rule("all", cond("body", "elapsed_ms", "gt", "100")), 200, `{"elapsed_ms":"fast"}`, "failure"},
		{"lte passes", rule("all", cond("status", "", "lte", "299")), 299, "", "success"},
		{"contains passes", rule("all", cond("body", "msg", "contains", "timeout")), 200, `{"msg":"upstream timeout occurred"}`, "success"},
		{"contains missing fails", rule("all", cond("body", "msg", "contains", "timeout")), 200, `{}`, "failure"},
		{"not_contains passes", rule("all", cond("body", "msg", "not_contains", "timeout")), 200, `{"msg":"ok"}`, "success"},
		{"not_contains missing passes", rule("all", cond("body", "msg", "not_contains", "timeout")), 200, `{}`, "success"},
		{"bool matches", rule("all", cond("body", "enabled", "eq", "true")), 200, `{"enabled":true}`, "success"},
		{"bool string matches too", rule("all", cond("body", "enabled", "eq", "true")), 200, `{"enabled":"true"}`, "success"},
		{"snowflake id exact", rule("all", cond("body", "order_id", "eq", "1789432093549168641")), 200, `{"order_id":1789432093549168641}`, "success"},
		{"decimal keeps literal", rule("all", cond("body", "amount", "eq", "1.50")), 200, `{"amount":1.50}`, "success"},
		{"decimal trailing zero literal", rule("all", cond("body", "amount", "eq", "0.30000000000000004")), 200, `{"amount":0.30000000000000004}`, "success"},
	}
	for _, c := range cases {
		if got := evaluateVerdict(c.rule, c.status, DecodeJSONBody([]byte(c.body))); got != c.want {
			t.Errorf("%s: evaluateVerdict(%d, %s) = %s, want %s", c.name, c.status, c.body, got, c.want)
		}
	}
}

// TestDecodeJSONBodyOnce: the single decode shared by the verdict and the
// business-envelope extraction keeps the UseNumber semantics (numeric
// literals verbatim) and yields nil for non-JSON bodies.
func TestDecodeJSONBodyOnce(t *testing.T) {
	if root := DecodeJSONBody([]byte(`{"amount":1.50,"id":1789432093549168641}`)); root == nil {
		t.Fatal("valid JSON decoded to nil")
	} else {
		if s, ok := lookupJSONPath(root, "amount"); !ok || s != "1.50" {
			t.Errorf("amount = %q, want literal 1.50", s)
		}
		if s, ok := lookupJSONPath(root, "id"); !ok || s != "1789432093549168641" {
			t.Errorf("id = %q, want the exact source literal", s)
		}
	}
	for _, body := range []string{"", "   ", "not json", `{"truncated`} {
		if root := DecodeJSONBody([]byte(body)); root != nil {
			t.Errorf("DecodeJSONBody(%q) = %v, want nil", body, root)
		}
	}
}

// TestExtractBizValueFromRoot: extraction walks the pre-decoded root exactly
// like the old re-decoding path, truncating rune-safely and returning "" for
// missing fields and nil roots.
func TestExtractBizValueFromRoot(t *testing.T) {
	root := DecodeJSONBody([]byte(`{"code":1,"message":"余额不足很多很多字","nested":{"msg":"deep"}}`))
	if got := extractBizValue("code", root, 128); got != "1" {
		t.Errorf("code = %q, want 1", got)
	}
	if got := extractBizValue("nested.msg", root, 128); got != "deep" {
		t.Errorf("nested.msg = %q, want deep", got)
	}
	if got := extractBizValue("missing", root, 128); got != "" {
		t.Errorf("missing field = %q, want empty", got)
	}
	// Rune-safe truncation at 6 bytes keeps the first two CJK runes.
	if got := extractBizValue("message", root, 6); got != "余额" {
		t.Errorf("truncated message = %q, want 余额", got)
	}
	if got := extractBizValue("code", nil, 128); got != "" {
		t.Errorf("nil root = %q, want empty", got)
	}
}

// TestBizFieldExtraction: configured envelope paths surface the business
// code and message in the access record even without a success rule.
func TestBizFieldExtraction(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":1,"message":"余额不足","amount":1.50}`))
	}))
	defer up.Close()

	leaf, fp := callerCert(t)
	s, logs, _, _ := newDebugTestServerBiz(t, up.URL, fp, leaf, false, nil, "code", "message")

	req := httptest.NewRequest("GET", "/alpha/v1/echo", nil)
	req.Header.Set("X-Request-Id", "biz-extract-1")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	s.ServeHTTP(httptest.NewRecorder(), req)

	got := logs.last()
	if got.BizCode != "1" || got.BizMessage != "余额不足" {
		t.Errorf("biz extraction = %q / %q, want 1 / 余额不足", got.BizCode, got.BizMessage)
	}
	if got.Outcome != "" {
		t.Errorf("no rule configured, outcome = %q, want empty", got.Outcome)
	}
}

// TestBizFieldGlobalFallback: when the route defines no envelope paths, the
// holder's global fields apply; a route-level path wins per field.
func TestBizFieldGlobalFallback(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":7,"message":"global-hit","extra":"x"}`))
	}))
	defer up.Close()

	leaf, fp := callerCert(t)
	s, logs, _, biz := newDebugTestServer(t, up.URL, fp, leaf, false, nil)

	biz.Set(store.BizFields{CodePath: "code", MessagePath: "message"})
	req := httptest.NewRequest("GET", "/alpha/v1/echo", nil)
	req.Header.Set("X-Request-Id", "biz-global-1")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	s.ServeHTTP(httptest.NewRecorder(), req)
	got := logs.last()
	if got.BizCode != "7" || got.BizMessage != "global-hit" {
		t.Errorf("global fallback = %q / %q, want 7 / global-hit", got.BizCode, got.BizMessage)
	}

	// Fallback is per UNSET field: the route configures only the code path —
	// a configured-but-missing path yields empty (no cross-field guessing),
	// while the message falls back to the global setting.
	s2, logs2, _, biz2 := newDebugTestServerBiz(t, up.URL, fp, leaf, false, nil, "nope", "")
	biz2.Set(store.BizFields{MessagePath: "message"})
	req2 := httptest.NewRequest("GET", "/alpha/v1/echo", nil)
	req2.Header.Set("X-Request-Id", "biz-global-2")
	req2.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	s2.ServeHTTP(httptest.NewRecorder(), req2)
	got2 := logs2.last()
	if got2.BizCode != "" || got2.BizMessage != "global-hit" {
		t.Errorf("per-field fallback = %q / %q, want '' (route set but missed) / global-hit", got2.BizCode, got2.BizMessage)
	}
}

// TestVerdictEndToEnd: a route with a success rule produces outcome values in
// the ordinary access record across success, business-failure, upstream
// failure, and early-reject paths.
func TestVerdictEndToEnd(t *testing.T) {
	upStatus := http.StatusOK
	upBody := `{"code":0,"echo":"ok"}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upStatus)
		_, _ = w.Write([]byte(upBody))
	}))
	defer up.Close()

	leaf, fp := callerCert(t)
	s, logs, _, _ := newDebugTestServer(t, up.URL, fp, leaf, false,
		rule("all", cond("status", "", "in", "200"), cond("body", "code", "in", "0")))

	send := func(method, path string) {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Request-Id", "verdict-"+method+"-"+path)
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
		s.ServeHTTP(httptest.NewRecorder(), req)
	}

	// success: 200 + code=0
	send("GET", "/alpha/v1/echo")
	if got := logs.last(); got.Outcome != "success" {
		t.Errorf("200+code0 outcome = %q, want success", got.Outcome)
	}
	// business failure: 200 + code=1
	upBody = `{"code":1,"echo":"biz-fail"}`
	send("GET", "/alpha/v1/echo")
	if got := logs.last(); got.Outcome != "failure" {
		t.Errorf("200+code1 outcome = %q, want failure", got.Outcome)
	}
	// upstream failure: 500 with non-JSON body
	upStatus = http.StatusInternalServerError
	upBody = `boom`
	send("GET", "/alpha/v1/echo")
	if got := logs.last(); got.Outcome != "failure" || got.Status != 500 {
		t.Errorf("500 outcome = %q status %d, want failure/500", got.Outcome, got.Status)
	}
	// early reject: PUT is not allowed → no verdict
	upStatus, upBody = http.StatusOK, `{"code":0}`
	send("PUT", "/alpha/v1/echo")
	if got := logs.last(); got.Outcome != "" {
		t.Errorf("deny path outcome = %q, want empty", got.Outcome)
	}
}

// largeEnvelope builds a valid common.success envelope bigger than the
// verdict capture limit, shaped like the dictionaries response that exposed
// the truncation false negative (envelope fields first, bulky data after).
func largeEnvelope() string {
	chunk := `{"id":1,"name":"dictionary-entry","values":["alpha","beta","gamma"]},`
	return `{"code":"common.success","message":"ok","data":{"items":[` +
		strings.Repeat(chunk, 200) + `{"id":0}]}}`
}

// TestDecodeJSONBodyTruncatedPrefix: a body cut at the capture limit is not
// valid JSON as a whole, but every top-level member completed before the cut
// is real data. The decode keeps those members (envelope code/message sit at
// the body start) instead of discarding everything; genuinely malformed or
// empty prefixes still yield nil.
func TestDecodeJSONBodyTruncatedPrefix(t *testing.T) {
	big := largeEnvelope()
	cut := big[:verdictCaptureMaxBytes]
	cases := []struct {
		name, body, path, want string
	}{
		{"envelope code survives the cut", cut, "code", "common.success"},
		{"envelope message survives the cut", cut, "message", "ok"},
		{"complete nested member kept", `{"code":"x","meta":{"a":1},"data":[`, "meta.a", "1"},
		{"array root keeps complete elements", `[{"a":1},{"a":2},{"a":`, "1.a", "2"},
		{"number literal kept verbatim", `{"id":1789432093549168641,"data":[`, "id", "1789432093549168641"},
	}
	for _, c := range cases {
		root := DecodeJSONBody([]byte(c.body))
		if root == nil {
			t.Errorf("%s: DecodeJSONBody returned nil", c.name)
			continue
		}
		if got, found := lookupJSONPath(root, c.path); !found || got != c.want {
			t.Errorf("%s: %s = %q (found=%v), want %q", c.name, c.path, got, found, c.want)
		}
	}
	// The member cut mid-value stays missing — no guessing past the cut.
	if root := DecodeJSONBody([]byte(cut)); root == nil {
		t.Error("truncated envelope decoded to nil")
	} else if _, found := lookupJSONPath(root, "data"); found {
		t.Error("incomplete data member must stay missing")
	}
	// The production rule shape judges the truncated prefix correctly.
	r := rule("all", cond("status", "", "eq", "200"), cond("body", "code", "eq", "common.success"))
	if got := evaluateVerdict(r, 200, DecodeJSONBody([]byte(cut))); got != "success" {
		t.Errorf("verdict on truncated success envelope = %q, want success", got)
	}
	for _, body := range []string{
		``, `   `, `not json`, `{"truncated`, `{"co`, `"unterminated`, `[`,
		`{"code":"x", garbage}`, `{"code":"x",}`,
	} {
		if root := DecodeJSONBody([]byte(body)); root != nil {
			t.Errorf("DecodeJSONBody(%q) = %v, want nil", body, root)
		}
	}
}

// TestVerdictLargeResponseBody: an upstream response larger than the verdict
// capture limit still reaches the client intact and is judged on its envelope
// fields — the regression for the dictionaries request recorded as failure
// with empty business fields despite HTTP 200 + common.success.
func TestVerdictLargeResponseBody(t *testing.T) {
	body := largeEnvelope()
	if len(body) <= verdictCaptureMaxBytes {
		t.Fatalf("test body must exceed the capture limit: %d bytes", len(body))
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer up.Close()

	leaf, fp := callerCert(t)
	s, logs, _, biz := newDebugTestServer(t, up.URL, fp, leaf, false,
		rule("all", cond("status", "", "eq", "200"), cond("body", "code", "eq", "common.success")))
	biz.Set(store.BizFields{CodePath: "code", MessagePath: "message"})

	req := httptest.NewRequest("GET", "/alpha/v1/dictionaries", nil)
	req.Header.Set("X-Request-Id", "verdict-large-1")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)

	got := logs.last()
	if got.Outcome != "success" {
		t.Errorf("large success envelope outcome = %q, want success", got.Outcome)
	}
	if got.BizCode != "common.success" || got.BizMessage != "ok" {
		t.Errorf("biz fields = %q / %q, want common.success / ok", got.BizCode, got.BizMessage)
	}
	if rr.Body.String() != body {
		t.Errorf("client body = %d bytes, want the full %d-byte response", rr.Body.Len(), len(body))
	}
	if got.ResponseBytes != int64(len(body)) {
		t.Errorf("response bytes = %d, want %d", got.ResponseBytes, len(body))
	}
}
