package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAllowedPath(t *testing.T) {
	for _, path := range []string{"/alpha", "/alpha/v1/ping", "/beta", "/beta/v1/ping"} {
		if !allowedPath(path) {
			t.Errorf("allowedPath(%q) = false", path)
		}
	}
	for _, path := range []string{"/", "/gamma/v1/ping", "/alphabet/v1/ping", "/betaish"} {
		if allowedPath(path) {
			t.Errorf("allowedPath(%q) = true", path)
		}
	}
}

func TestCleanHeaders(t *testing.T) {
	in := http.Header{}
	in.Set("X-Gateway-Caller", "forged")
	in.Set("X-Gateway-User-Id", "forged-user")
	in.Set("X-Demo-User-Id", "user-42")
	in.Set("X-Request-Id", "req_test")
	out := cleanHeaders(in)
	if out.Get("X-Gateway-Caller") != "" || out.Get("X-Gateway-User-Id") != "" {
		t.Fatal("gateway identity headers must be stripped")
	}
	if out.Get("X-Demo-User-Id") != "user-42" || out.Get("X-Request-Id") != "req_test" {
		t.Fatal("non-gateway request headers should be preserved for controlled translation")
	}
}

func TestRequestIDFor(t *testing.T) {
	if got := requestIDFor("req_external-1"); got != "req_external-1" {
		t.Fatalf("valid request id changed: %q", got)
	}
	if got := requestIDFor("bad id"); !strings.HasPrefix(got, "edge_") {
		t.Fatalf("invalid request id not replaced: %q", got)
	}
}

func TestRestrictedProxyRejectsUnknownPath(t *testing.T) {
	called := false
	h := restrictedProxy(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/gamma/v1/ping", nil))
	if w.Code != http.StatusNotFound || called {
		t.Fatalf("unknown path: status=%d called=%v", w.Code, called)
	}
}

func TestRestrictedProxyAllowsKnownPath(t *testing.T) {
	called := false
	h := restrictedProxy(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/alpha/v1/ping", nil))
	if w.Code != http.StatusNoContent || !called {
		t.Fatalf("known path: status=%d called=%v", w.Code, called)
	}
}
