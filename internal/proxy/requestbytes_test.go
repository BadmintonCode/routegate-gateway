package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// RequestBytes must be counted for every forwarded request, not only when the
// debug tee is active. The counting reader sits innermost (the tee wraps it),
// so each byte counts exactly once; early rejects keep 0.

// TestRequestBytesContentLength: a forwarded request with a content-length
// body counts the body bytes in the ordinary record without debug capture.
func TestRequestBytesContentLength(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer up.Close()

	leaf, fp := callerCert(t)
	s, logs, _, _ := newDebugTestServer(t, up.URL, fp, leaf, false, nil)

	body := strings.Repeat("x", 40)
	req := httptest.NewRequest("POST", "/alpha/v1/echo", strings.NewReader(body))
	req.Header.Set("X-Request-Id", "reqbytes-cl")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	s.ServeHTTP(httptest.NewRecorder(), req)

	got := logs.last()
	if got.Decision != "allow" {
		t.Fatalf("decision = %s, want allow", got.Decision)
	}
	if got.RequestBytes != int64(len(body)) {
		t.Errorf("RequestBytes = %d, want %d", got.RequestBytes, len(body))
	}
}

// TestRequestBytesChunked: an unknown-length (streamed/chunked) body counts
// every byte read until EOF.
func TestRequestBytesChunked(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer up.Close()

	leaf, fp := callerCert(t)
	s, logs, _, _ := newDebugTestServer(t, up.URL, fp, leaf, false, nil)

	body := strings.Repeat("y", 33)
	// A plain io.Reader (not *strings.Reader) leaves ContentLength 0, which is
	// how the server presents a chunked/streamed client body to the handler.
	req := httptest.NewRequest("POST", "/alpha/v1/echo", io.NopCloser(strings.NewReader(body)))
	req.Header.Set("X-Request-Id", "reqbytes-chunked")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	s.ServeHTTP(httptest.NewRecorder(), req)

	got := logs.last()
	if got.Decision != "allow" {
		t.Fatalf("decision = %s, want allow", got.Decision)
	}
	if got.RequestBytes != int64(len(body)) {
		t.Errorf("RequestBytes = %d, want %d", got.RequestBytes, len(body))
	}
}

// TestRequestBytesEarlyRejectZero: a policy-deny before forwarding never
// consumes the body, so RequestBytes stays 0.
func TestRequestBytesEarlyRejectZero(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("deny path must not reach the upstream")
	}))
	defer up.Close()

	leaf, fp := callerCert(t)
	s, logs, _, _ := newDebugTestServer(t, up.URL, fp, leaf, false, nil)

	req := httptest.NewRequest("PUT", "/alpha/v1/echo", strings.NewReader("never-read"))
	req.Header.Set("X-Request-Id", "reqbytes-deny")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	s.ServeHTTP(httptest.NewRecorder(), req)

	got := logs.last()
	if got.Decision != "deny" || got.Status != http.StatusForbidden {
		t.Fatalf("decision/status = %s/%d, want deny/403", got.Decision, got.Status)
	}
	if got.RequestBytes != 0 {
		t.Errorf("RequestBytes = %d, want 0 for an early reject", got.RequestBytes)
	}
}

// TestRequestBytesDebugNotDoubleCounted: with debug capture active the tee
// wraps the counter, so the ordinary record and the debug record both carry
// the body size once.
func TestRequestBytesDebugNotDoubleCounted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer up.Close()

	leaf, fp := callerCert(t)
	s, logs, base, _ := newDebugTestServer(t, up.URL, fp, leaf, true, nil)

	body := strings.Repeat("z", 21)
	req := httptest.NewRequest("POST", "/alpha/v1/echo", strings.NewReader(body))
	req.Header.Set("X-Request-Id", "reqbytes-debug")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	s.ServeHTTP(httptest.NewRecorder(), req)

	if got := logs.last(); got.RequestBytes != int64(len(body)) {
		t.Errorf("ordinary RequestBytes = %d, want %d (double counting?)", got.RequestBytes, len(body))
	}
	dbg := waitForDebugRecord(t, base, "reqbytes-debug")
	if dbg.RequestBytes != int64(len(body)) {
		t.Errorf("debug RequestBytes = %d, want %d", dbg.RequestBytes, len(body))
	}
}
