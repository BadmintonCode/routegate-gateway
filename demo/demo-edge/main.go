// demo-edge is a deliberately small public-facing API gateway example.
//
// External callers use plain HTTP here. The edge itself is a RouteGate
// workload: it calls RouteGate over HTTPS with its pinned client certificate.
// Only /alpha/* and /beta/* are proxied; /healthz is local and everything else
// is rejected so this cannot become an open proxy.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,128}$`)

func main() {
	gateway := envOr("GATEWAY_URL", "https://routegate:8080")
	certFile := envOr("CLIENT_CERT", "/certs/edge.crt")
	keyFile := envOr("CLIENT_KEY", "/certs/edge.key")
	caFile := envOr("GATEWAY_CA", "/certs/gateway.crt")
	listen := envOr("LISTEN_ADDR", ":8080")

	transport, err := newTransport(certFile, keyFile, caFile)
	if err != nil {
		log.Fatalf("demo-edge: %v", err)
	}
	base, err := url.Parse(gateway)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		log.Fatalf("demo-edge: GATEWAY_URL must be an https URL, got %q", gateway)
	}

	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: 100 * time.Millisecond,
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Keep the original /alpha or /beta path: RouteGate owns prefix
			// matching and strips it before forwarding to the upstream.
			pr.Out.URL = &url.URL{
				Scheme: base.Scheme, Host: base.Host,
				Path: pr.In.URL.Path, RawPath: pr.In.URL.RawPath,
				RawQuery: pr.In.URL.RawQuery,
			}
			pr.Out.Host = base.Host
			out := cleanHeaders(pr.In.Header)
			requestID := requestIDFor(pr.In.Header.Get("X-Request-Id"))
			out.Set("X-Request-Id", requestID)
			if user := cleanUserID(pr.In.Header.Get("X-Demo-User-Id")); user != "" {
				// RouteGate accepts this only because the authenticated caller is
				// demo-edge (ROUTEGATE_TRUSTED_USER_CONTEXT_CALLERS).
				out.Set("X-Gateway-User-Id", user)
			}
			pr.Out.Header = out
			pr.Out.Header.Del("X-Demo-User-Id")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]string{"code": "demo_edge.upstream_unavailable", "message": err.Error()},
			})
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"code": "ok", "service": "demo-edge"})
	})
	mux.Handle("/alpha", restrictedProxy(proxy))
	mux.Handle("/alpha/", restrictedProxy(proxy))
	mux.Handle("/beta", restrictedProxy(proxy))
	mux.Handle("/beta/", restrictedProxy(proxy))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]map[string]string{
			"error": {"code": "demo_edge.route_not_found", "message": "only /alpha/* and /beta/* are exposed"},
		})
	})

	log.Printf("demo-edge listening on %s; outbound RouteGate=%s", listen, gateway)
	if err := http.ListenAndServe(listen, mux); err != nil {
		log.Fatal(err)
	}
}

// restrictedProxy is an extra defense-in-depth guard: ServeMux patterns above
// already constrain paths, but this check makes the boundary explicit and
// testable if the handler is reused later.
func restrictedProxy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedPath(r.URL.Path) {
			writeJSON(w, http.StatusNotFound, map[string]map[string]string{
				"error": {"code": "demo_edge.route_not_found", "message": "path is not exposed by demo-edge"},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func allowedPath(path string) bool {
	return path == "/alpha" || strings.HasPrefix(path, "/alpha/") ||
		path == "/beta" || strings.HasPrefix(path, "/beta/")
}

func cleanHeaders(in http.Header) http.Header {
	out := in.Clone()
	for _, key := range []string{
		"X-Gateway-Caller", "X-Gateway-Request-Id", "X-Gateway-Cert-Subject",
		"X-Gateway-User-Id", "X-Authenticated-User-Id", "X-Forwarded-For",
		"X-Forwarded-Proto", "X-Real-IP", "X-Client-Ip",
	} {
		out.Del(key)
	}
	return out
}

func requestIDFor(value string) string {
	value = strings.TrimSpace(value)
	if requestIDPattern.MatchString(value) {
		return value
	}
	return fmt.Sprintf("edge_%x", time.Now().UnixNano())
}

func cleanUserID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

func newTransport(certFile, keyFile, caFile string) (*http.Transport, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load edge client certificate: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read RouteGate certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("RouteGate certificate contains no PEM certificate")
	}
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS12,
		},
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// Keep io referenced in the demo package's dependency surface for future
// streaming endpoints; the current proxy streams through ReverseProxy.
var _ = io.EOF
