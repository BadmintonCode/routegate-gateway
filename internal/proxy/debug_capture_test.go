package proxy

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/config"
	"github.com/BadmintonCode/routegate-gateway/internal/identity"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

// recCollector is a thread-safe ordinary Record sink for tests.
type recCollector struct {
	mu   sync.Mutex
	recs []accesslog.Record
}

func (c *recCollector) Emit(r accesslog.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r)
}

func (c *recCollector) Close() error { return nil }

func (c *recCollector) last() accesslog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.recs) == 0 {
		return accesslog.Record{}
	}
	return c.recs[len(c.recs)-1]
}

func debugTestCfg() *config.Config {
	return &config.Config{
		ProxyEnabled: true, ClientAuth: config.ClientAuthCert,
		UpstreamKeepalive: true, InstanceID: "test",
	}
}

// debugTestSettings mirrors the caps the capture assertions expect.
func debugTestSettings(file string) store.DebugSettings {
	return store.DebugSettings{
		Enabled:              true,
		File:                 file,
		MaxMB:                1,
		RetainDays:           1,
		RequestBodyMaxBytes:  16,
		ResponseBodyMaxBytes: 1024,
		HeadersMaxBytes:      4096,
		QueueCapacity:        16,
	}
}

// callerCert generates a self-signed leaf whose URI SAN matches the
// demo-caller identity, mirroring the pinned-certificate flow.
func callerCert(t *testing.T) (*x509.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "demo-caller"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		URIs:         []*url.URL{{Scheme: "https", Host: "routegate.local", Path: "/identities/demo-caller"}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return leaf, identity.Fingerprint(leaf)
}

func newDebugTestServer(t *testing.T, upstreamURL, fp string, leaf *x509.Certificate, debug bool, rule *snapshot.SuccessRule) (*Server, *recCollector, string, *store.BizFieldsHolder) {
	return newDebugTestServerBiz(t, upstreamURL, fp, leaf, debug, rule, "", "")
}

func newDebugTestServerBiz(t *testing.T, upstreamURL, fp string, leaf *x509.Certificate, debug bool, rule *snapshot.SuccessRule, bizCodePath, bizMsgPath string) (*Server, *recCollector, string, *store.BizFieldsHolder) {
	t.Helper()
	target, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, err := net.SplitHostPort(target.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	snap := &snapshot.Snapshot{
		Revision: 1,
		Routes: []*snapshot.Route{{
			ID: 1, Name: "alpha", PathPrefix: "/alpha", StripPrefix: true,
			Targets:        []snapshot.Target{{Host: host, Port: port, Weight: 1}},
			SuccessRule:    rule,
			BizCodePath:    bizCodePath,
			BizMessagePath: bizMsgPath,
		}},
		Policies: map[int64][]*snapshot.Policy{
			1: {
				{ID: 11, RouteID: 1, Caller: "demo-caller", Method: "GET", Segments: mustParse("/v1/**")},
				{ID: 12, RouteID: 1, Caller: "demo-caller", Method: "POST", Segments: mustParse("/v1/**")},
			},
		},
		Identities: map[string]*snapshot.Identity{
			fp: {Name: "demo-caller", Cert: leaf, Fingerprint: fp},
		},
	}
	snapshot.SortRoutes(snap)
	holder := &snapshot.Holder{}
	holder.Store(snap)

	logs := &recCollector{}
	base := filepath.Join(t.TempDir(), "debug.log")
	cfg := debugTestCfg()
	dbg := accesslog.NewDebugManager()
	if debug {
		if err := dbg.Apply(debugTestSettings(base)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = dbg.Close() })
	bizHolder := store.NewBizFieldsHolder()
	return NewServer(cfg, holder, logs, "test", dbg, bizHolder, nil), logs, base, bizHolder
}

// waitForDebugRecord polls the rotating debug file until a record with the
// given request ID appears (the sink is asynchronous by design).
func waitForDebugRecord(t *testing.T, base, reqID string) accesslog.DebugRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		paths, err := accesslog.DebugSegmentPaths(base)
		if err == nil {
			recs, _ := accesslog.ReadDebugRecordsBackward(paths, 50, func(r accesslog.DebugRecord) bool {
				return r.RequestID == reqID
			})
			if len(recs) > 0 {
				return recs[0]
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("debug record %q never reached the file", reqID)
	return accesslog.DebugRecord{}
}

// TestDebugCaptureAllowPath: with debug enabled, the allow path records
// sanitized headers/cookies and bounded payloads while the ordinary record
// gains real status/bytes; no explicit address or credential material leaks.
func TestDebugCaptureAllowPath(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Upstream-Header", "yes")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"echo":"` + string(b) + `"}`))
	}))
	defer up.Close()

	leaf, fp := callerCert(t)
	s, logs, base, _ := newDebugTestServer(t, up.URL, fp, leaf, true, nil)

	body := "hello-debug-body-0123456789-abcdefghijklmnopqrstuvwxyz" // 54 bytes > 16 cap
	req := httptest.NewRequest("POST", "/alpha/v1/echo", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Request-Id", "test-req-allow")
	req.Header.Set("Authorization", "Bearer super-secret")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Custom-Trace", "keep-me")
	req.Header.Set("Cookie", "session-id=abc123; theme=dark")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	got := logs.last()
	if got.Status != http.StatusOK || got.Decision != "allow" {
		t.Fatalf("ordinary record status/decision = %d/%s, want 200/allow", got.Status, got.Decision)
	}
	if got.RequestBytes != int64(len(body)) || got.ResponseBytes == 0 {
		t.Errorf("ordinary record bytes = req %d resp %d, want req %d and resp > 0", got.RequestBytes, got.ResponseBytes, len(body))
	}

	dbg := waitForDebugRecord(t, base, "test-req-allow")
	lineBytes, err := json.Marshal(dbg)
	if err != nil {
		t.Fatal(err)
	}
	line := string(lineBytes)
	for _, forbidden := range []string{"1.2.3.4", "super-secret", "Bearer", "abc123"} {
		if strings.Contains(line, forbidden) {
			t.Errorf("debug record leaks %q", forbidden)
		}
	}
	if gotHdr := dbg.RequestHeaders["X-Custom-Trace"]; len(gotHdr) != 1 || gotHdr[0] != "keep-me" {
		t.Errorf("ordinary request header not captured: %v", dbg.RequestHeaders)
	}
	if dbg.RequestCookies["theme"] != "dark" || dbg.RequestCookies["session-id"] != "<redacted>" {
		t.Errorf("cookie sanitization wrong: %v", dbg.RequestCookies)
	}
	// 54 bytes streamed through the tee, only the first 16 captured.
	if !dbg.RequestBody.Truncated || dbg.RequestBody.Bytes != int64(len(body)) || len(dbg.RequestBody.Data) != 16 {
		t.Errorf("request payload capture wrong: %+v", dbg.RequestBody)
	}
	// The full body reached the upstream (tee is transparent) and is echoed back.
	if !strings.Contains(dbg.ResponseBody.Data, body) {
		t.Errorf("response capture lost the echoed body: %q", dbg.ResponseBody.Data)
	}
	if gotHdr := dbg.ResponseHeaders["X-Upstream-Header"]; len(gotHdr) != 1 || gotHdr[0] != "yes" {
		t.Errorf("response headers not captured: %v", dbg.ResponseHeaders)
	}
	if dbg.ResponseBody.Unavailable || dbg.ResponseBody.Encoding != "text" {
		t.Errorf("response payload flags wrong: %+v", dbg.ResponseBody)
	}
	if dbg.RequestHeadersTruncated || dbg.ResponseHeadersTruncated {
		t.Errorf("unexpected header truncation: %+v", dbg)
	}
}

// TestDebugCaptureDenyNotConsumed: early rejects never read the body — the
// debug record must say "unavailable" rather than imply an empty payload —
// and the ordinary record carries the real 403 (previously always 0).
func TestDebugCaptureDenyNotConsumed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("deny path must not reach the upstream")
	}))
	defer up.Close()

	leaf, fp := callerCert(t)
	s, logs, base, _ := newDebugTestServer(t, up.URL, fp, leaf, true, nil)

	req := httptest.NewRequest("PUT", "/alpha/v1/echo", strings.NewReader("never-read-body"))
	req.Header.Set("X-Request-Id", "test-req-deny")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	got := logs.last()
	if got.Status != http.StatusForbidden || got.Decision != "deny" {
		t.Fatalf("ordinary record status/decision = %d/%s, want 403/deny", got.Status, got.Decision)
	}

	dbg := waitForDebugRecord(t, base, "test-req-deny")
	if dbg.Status != http.StatusForbidden || dbg.Decision != "deny" {
		t.Errorf("debug record status/decision = %d/%s", dbg.Status, dbg.Decision)
	}
	if !dbg.RequestBody.Unavailable || dbg.RequestBody.Data != "" || dbg.RequestBody.Bytes != 0 {
		t.Errorf("unconsumed body must be marked unavailable: %+v", dbg.RequestBody)
	}
	if !strings.Contains(dbg.ResponseBody.Data, "policy_denied") {
		t.Errorf("error envelope not captured: %q", dbg.ResponseBody.Data)
	}
}

// TestDebugDisabledNilSink: without the debug sink there is no capture and no
// debug file, while the ordinary record still gains status/bytes.
func TestDebugDisabledNilSink(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer up.Close()

	leaf, fp := callerCert(t)
	s, logs, base, _ := newDebugTestServer(t, up.URL, fp, leaf, false, nil)

	req := httptest.NewRequest("GET", "/alpha/v1/echo", nil)
	req.Header.Set("X-Request-Id", "test-req-off")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if got := logs.last(); got.Status != http.StatusOK || got.Decision != "allow" {
		t.Fatalf("ordinary record status/decision = %d/%s, want 200/allow", got.Status, got.Decision)
	}
	if paths, err := accesslog.DebugSegmentPaths(base); err == nil && len(paths) != 0 {
		t.Errorf("debug file written while disabled: %v", paths)
	}
}

// TestStatusRecorderCapture: implicit 200 on first Write, body bytes, and the
// hijack marker flow through the single response wrapper.
func TestStatusRecorderCapture(t *testing.T) {
	ds := debugTestSettings(filepath.Join(t.TempDir(), "debug.log"))
	cap := &debugCapture{ds: &ds}
	rw := &statusRecorder{ResponseWriter: httptest.NewRecorder(), cap: cap}

	n, err := rw.Write([]byte("implicit"))
	if err != nil || n != 8 {
		t.Fatalf("write = %d, %v", n, err)
	}
	if rw.status != http.StatusOK {
		t.Errorf("implicit status = %d, want 200", rw.status)
	}
	if cap.responseStatus != http.StatusOK || cap.responseBody.data.String() != "implicit" || rw.bytes != 8 {
		t.Errorf("capture state = status %d body %q bytes %d", cap.responseStatus, cap.responseBody.data.String(), rw.bytes)
	}

	// Hijack delegation marks payload capture unavailable.
	rw2 := &statusRecorder{ResponseWriter: &hijackableRecorder{httptest.NewRecorder()}, cap: cap}
	if _, _, err := rw2.Hijack(); err == nil {
		t.Error("expected ErrNotSupported-style error from the fake hijacker")
	}
	if !cap.responseHijacked {
		t.Error("hijack not marked on capture")
	}
	got := cap.record(accesslog.Record{Decision: "allow", Status: 200})
	if !got.ResponseBody.Unavailable || got.ResponseBody.Available {
		t.Errorf("hijacked response payload must be unavailable: %+v", got.ResponseBody)
	}
}

type hijackableRecorder struct {
	*httptest.ResponseRecorder
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("test hijacker")
}
