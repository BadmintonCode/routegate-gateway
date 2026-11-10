package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/authz"
	"github.com/BadmintonCode/routegate-gateway/internal/config"
	"github.com/BadmintonCode/routegate-gateway/internal/identity"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

// Headers stripped from every inbound request before forwarding (ADR-0001 §11.4).
// Forwarded, X-Forwarded-Host and True-Client-Ip are included with the rest of
// the forwarding family: a caller must never be able to smuggle an upstream
// identity or routing hint past the gateway. Del canonicalizes the names, so
// list casing is tolerant.
var strippedInboundHeaders = []string{
	"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Real-IP",
	"X-Client-Ip", "True-Client-Ip", "Forwarded",
	"X-Authenticated-User-Id", "X-Gateway-Caller", "X-Gateway-Request-Id",
	"X-Gateway-Cert-Subject", "X-Gateway-User-Id",
}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,128}$`)

// Server is the data-plane HTTP handler.
type Server struct {
	cfg        *config.Config
	holder     *snapshot.Holder
	log        accesslog.Sink
	debug      *accesslog.DebugManager // panel-managed debug capture; never nil, disabled until settings say otherwise
	bizFields  *store.BizFieldsHolder  // global business-envelope fallback (route paths win per field)
	alerts     AlertSink               // alert-rule evaluation; nil when no engine is wired
	transport  *http.Transport
	dns        *dnsCache
	rr         sync.Map // route name -> *atomic.Uint64 (weighted round-robin counter)
	instanceID string
}

// AlertSink evaluates alert rules for one finished request. Implemented by
// the alerts engine; nil disables alert evaluation.
type AlertSink interface {
	HasRules() bool
	Evaluate(routeID int64, rec accesslog.Record, status int, body []byte)
}

// NewServer builds the data-plane handler. debug and bizFields must be
// non-nil; the sync worker applies their settings on every tick, so the hot
// path here just reads the current atomic snapshots. alerts may be nil.
func NewServer(cfg *config.Config, holder *snapshot.Holder, log accesslog.Sink, instanceID string, debug *accesslog.DebugManager, bizFields *store.BizFieldsHolder, alerts AlertSink) *Server {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	resolver := net.DefaultResolver
	cache := newDNSCache(cfg.UpstreamDNSTTL)
	transport := &http.Transport{
		Proxy: nil, // internal mesh: never via an HTTP proxy
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return dialer.DialContext(ctx, network, addr)
			}
			addrs, lerr := cache.lookup(ctx, resolver, host)
			if lerr != nil || len(addrs) == 0 {
				return dialer.DialContext(ctx, network, addr)
			}
			// Rotate across cached IPs so multi-A records share load.
			var lastErr error
			for _, a := range addrs {
				conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(a, port))
				if derr == nil {
					return conn, nil
				}
				lastErr = derr
			}
			return nil, lastErr
		},
		DisableKeepAlives:     !cfg.UpstreamKeepalive,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ForceAttemptHTTP2:     false, // upstream h2 not committed for v1 (ADR §5.2)
	}
	return &Server{
		cfg: cfg, holder: holder, log: log, debug: debug, bizFields: bizFields, alerts: alerts,
		transport: transport, dns: cache, instanceID: instanceID,
	}
}

// errorf writes the data-plane error envelope. Status codes follow REST
// semantics; the legacy always-200 envelope is intentionally NOT used in
// the open-source data plane (ADR-0001 §1.5).
func errorf(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{
		"error": {"code": code, "message": message},
	})
}

// statusRecorder captures the response status and byte count for the access
// record. When debug capture is present it also feeds the sanitized response
// snapshot (headers on first WriteHeader, bounded body bytes on Write).
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	cap    *debugCapture
	vbuf   *captureBuffer // bounded response capture for the success verdict; nil when the route has no rule
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
		if s.cap != nil {
			s.cap.header(s.Header())
			s.cap.writeHeader(code)
		}
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.WriteHeader(http.StatusOK)
	}
	if s.cap != nil {
		s.cap.write(p)
	}
	if s.vbuf != nil {
		s.vbuf.appendBytes(p, verdictCaptureMaxBytes)
	}
	s.bytes += int64(len(p))
	return s.ResponseWriter.Write(p)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates for upgraded connections (WebSocket) and marks the debug
// payload capture unavailable: the stream is no longer an HTTP response.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	if s.cap != nil {
		s.cap.hijacked()
	}
	return h.Hijack()
}

// Unwrap lets http.ResponseController reach the underlying writer's optional
// interfaces through the wrapper.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// countingBody wraps a request body and counts every byte consumed, so
// rec.RequestBytes reports bytes actually read from the client for every
// forwarded request — not only when debug capture tees the body. It is
// installed as the innermost wrapper (the debug tee wraps this counter), so
// each byte is counted exactly once.
type countingBody struct {
	inner io.ReadCloser
	n     atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	if n > 0 {
		c.n.Add(int64(n))
	}
	return n, err
}

func (c *countingBody) Close() error { return c.inner.Close() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := accesslog.Record{
		TS: start, InstanceID: s.instanceID,
		Method: r.Method, Path: r.URL.Path,
	}

	// Request-byte accounting is always on: wrap the body as the innermost
	// reader. Early rejects below never consume the body, so they keep 0 by
	// construction; forwarded requests count what the forwarding path read.
	var bodyCount *countingBody
	if r.Body != nil {
		bodyCount = &countingBody{inner: r.Body}
		r.Body = bodyCount
	}
	// Debug capture is panel-managed (ADR-0001 §6.6): a bounded tee on the
	// request body and sanitized response hooks. Disabled (the default) means
	// one atomic load and zero extra work. The tee wraps the counter above, so
	// bytes are never double counted.
	var cap *debugCapture
	if ds := s.debug.Settings(); ds != nil {
		cap = newDebugCapture(ds, r)
		if r.Body != nil {
			r.Body = &debugRequestBody{inner: r.Body, cap: cap}
		}
	}
	rw := &statusRecorder{ResponseWriter: w, cap: cap}
	// Every downstream write — error envelopes and proxied responses alike —
	// goes through the recorder so statuses and debug capture are complete.
	w = rw
	// Declared here so the deferred verdict evaluation can read the matched
	// route; nil until [3] below.
	var route *snapshot.Route
	defer func() {
		rec.DurationMS = time.Since(start).Milliseconds()
		if rec.Status == 0 {
			rec.Status = rw.status
		}
		rec.ResponseBytes = rw.bytes
		if bodyCount != nil {
			// Counted at the innermost reader: every byte the forwarding (or
			// debug tee) path consumed, exactly once. Early rejects never read
			// the body, so they stay 0 — the documented Record semantics.
			rec.RequestBytes = bodyCount.n.Load()
		}
		if rw.vbuf != nil && rw.status != 0 &&
			(rec.Decision == "allow" || rec.Decision == "upstream_error") {
			// Gateway-generated early rejects get no verdict and no envelope
			// extraction: the interface was never reached. Proxied responses
			// and upstream failures are judged and extracted like any other.
			// The captured body is decoded exactly once and shared by the
			// verdict and the business-envelope extraction.
			root := DecodeJSONBody(rw.vbuf.data.Bytes())
			if route.SuccessRule != nil {
				rec.Outcome = evaluateVerdict(route.SuccessRule, rw.status, root)
			}
			// Per-field fallback: the route path wins, the global default
			// fills in when the route does not define that field.
			codePath, msgPath := route.BizCodePath, route.BizMessagePath
			if codePath == "" || msgPath == "" {
				gf := s.bizFields.Get()
				if codePath == "" {
					codePath = gf.CodePath
				}
				if msgPath == "" {
					msgPath = gf.MessagePath
				}
			}
			rec.BizCode = extractBizValue(codePath, root, 128)
			rec.BizMessage = extractBizValue(msgPath, root, 512)
		}
		if s.alerts != nil && route != nil {
			var body []byte
			if rw.vbuf != nil {
				body = rw.vbuf.data.Bytes()
			}
			s.alerts.Evaluate(route.ID, rec, rw.status, body)
		}
		s.log.Emit(rec)
		if cap != nil {
			s.debug.EmitDebug(cap.record(rec))
		}
	}()

	// [1] Request ID: reuse a well-formed inbound value, else generate one.
	reqID := sanitizeRequestID(r.Header.Get("X-Request-Id"))
	if reqID == "" {
		reqID = generateRequestID()
	}
	rec.RequestID = reqID

	snap := s.holder.Load()
	if snap == nil {
		// No snapshot ever loaded: fail closed (ADR-0001 §3.4).
		errorf(w, http.StatusServiceUnavailable, "gateway.starting", "gateway has not loaded configuration yet")
		rec.Decision = "starting"
		return
	}
	rec.ConfigRevision = snap.Revision

	// [2] Caller identity (always enforced; no auth-free mode, ADR §5).
	if r.TLS == nil {
		errorf(w, http.StatusBadRequest, "gateway.tls_required", "the data plane requires HTTPS with a client certificate")
		rec.Decision = "tls_required"
		return
	}
	caller, err := identity.Resolve(r.TLS, snap, time.Now())
	if err != nil {
		errorf(w, http.StatusUnauthorized, "identity.unauthenticated", "a valid, registered client certificate is required")
		rec.Decision = "unauthenticated"
		return
	}
	rec.Caller = caller

	// Inbound end-user context: captured only for trusted callers, then all
	// inbound identity/proxy headers are stripped (ADR §6.3.4).
	inboundUserID := r.Header.Get("X-Gateway-User-Id")
	trustedUserContext := false
	for _, t := range s.cfg.TrustedUserContextCallers {
		if t == caller {
			trustedUserContext = true
			break
		}
	}
	var userID string
	if trustedUserContext && sanitizeUserID(inboundUserID) != "" {
		userID = sanitizeUserID(inboundUserID)
	}

	// [3] Route match: longest explicit prefix over the normalized path. The
	// snapshot sorts routes longest-prefix-first, and the first route whose
	// method set allows the request wins. Method filtering therefore falls
	// through: a more specific prefix whose methods exclude the request does
	// NOT answer 405 — the scan continues with the next shorter prefix, and if
	// no prefix+method pair matches, the request 404s (route_not_found). This
	// fall-through is load-bearing: deployed layouts rely on a shorter
	// wildcard-style route catching methods the specific route does not list,
	// so do not turn it into a 405 without revisiting those routes.
	normalized := authz.NormalizePath(r.URL.Path)
	for _, cand := range snap.Routes {
		if matchPrefix(cand.PathPrefix, normalized) && methodAllowed(cand, r.Method) {
			route = cand
			break
		}
	}
	if route == nil {
		errorf(w, http.StatusNotFound, "gateway.route_not_found", "no route matches this path")
		rec.Decision = "route_not_found"
		return
	}
	rec.Route = route.Name

	// [4] Policy evaluation: deny > allow > default deny (ADR §6.1).
	sub := normalized
	if route.StripPrefix {
		sub = strings.TrimPrefix(normalized, route.PathPrefix)
		if sub == "" {
			sub = "/"
		}
	}
	rec.Path = sub
	decision, policyID := evaluate(snap, route, caller, r.Method, sub)
	rec.MatchedPolicyID = policyID
	if route.SuccessRule != nil || route.BizCodePath != "" || route.BizMessagePath != "" || s.hasGlobalBizFields() ||
		(s.alerts != nil && s.alerts.HasRules()) {
		// Bounded response capture for the success verdict and/or the
		// business-envelope extraction (§6.7). Install it only now: early
		// rejects never reach the interface and get neither.
		rw.vbuf = &captureBuffer{}
	}
	if decision != "allow" {
		errorf(w, http.StatusForbidden, "gateway.policy_denied", "the caller is not allowed to perform this operation")
		rec.Decision = "deny"
		return
	}
	rec.Decision = "allow"

	// [5] Weighted target selection.
	target, terr := s.pickTarget(route)
	if terr != nil {
		errorf(w, http.StatusBadGateway, "gateway.no_targets", "the route has no usable upstream target")
		rec.Decision = "upstream_error"
		return
	}
	rec.UpstreamHost = fmt.Sprintf("%s:%d", target.Host, target.Port)

	// [6][7] Forward with header hygiene. The surgery happens on pr.Out inside
	// Rewrite — that is the header set actually forwarded. X-Gateway-Cert-Subject
	// is stripped but not rebuilt in v1: the caller name in X-Gateway-Caller
	// carries the verified identity downstream.
	scheme, host := "http", fmt.Sprintf("%s:%d", target.Host, target.Port)
	proxy := &httputil.ReverseProxy{
		Transport:     s.transport,
		FlushInterval: 100 * time.Millisecond, // SSE/streaming: never batch (ADR §5.2)
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: scheme, Host: host})
			pr.Out.URL.Path = sub
			pr.Out.URL.RawPath = ""
			pr.Out.Host = r.Host

			outbound := strippedHeaders(pr.In)
			outbound.Set("X-Gateway-Caller", caller)
			outbound.Set("X-Gateway-Request-Id", reqID)
			outbound.Set("X-Request-Id", reqID)
			if userID != "" {
				outbound.Set("X-Gateway-User-Id", userID)
			}
			pr.Out.Header = outbound
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if isTimeout(err) {
				errorf(w, http.StatusGatewayTimeout, "gateway.upstream_timeout", "the upstream did not respond in time")
			} else {
				errorf(w, http.StatusBadGateway, "gateway.upstream_unavailable", "the upstream could not be reached")
			}
			rec.Decision = "upstream_error"
		},
	}
	proxy.ServeHTTP(w, r.WithContext(r.Context()))
}

// strippedHeaders clones the request headers minus the inbound identity/proxy
// set. The gateway re-asserts the identity it verified: X-Gateway-Caller (and
// optionally X-Gateway-User-Id) are set by the caller of Rewrite. In v1
// X-Gateway-Cert-Subject is stripped but NOT rebuilt — the caller name in
// X-Gateway-Caller is the only downstream identity signal. Hop-by-hop headers
// (Connection, Upgrade, ...) are left to httputil, whose handling preserves
// WebSocket upgrades.
func strippedHeaders(r *http.Request) http.Header {
	out := r.Header.Clone()
	for _, h := range strippedInboundHeaders {
		out.Del(h)
	}
	return out
}

func matchPrefix(prefix, path string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func methodAllowed(r *snapshot.Route, method string) bool {
	if len(r.Methods) == 0 {
		return true
	}
	for _, m := range r.Methods {
		if m == strings.ToUpper(method) {
			return true
		}
	}
	return false
}

// evaluate implements deny > allow > default deny over the route's policies.
func evaluate(snap *snapshot.Snapshot, route *snapshot.Route, caller, method, subPath string) (string, int64) {
	policies := snap.Policies[route.ID]
	if len(policies) == 0 {
		return "deny", 0
	}
	segs := splitSegments(subPath)
	allowed := false
	var allowID int64
	for _, p := range policies {
		if !authz.CallerMatches(p.Caller, caller) || !authz.MethodMatches(p.Method, method) {
			continue
		}
		if !authz.MatchSegments(p.Segments, segs) {
			continue
		}
		if p.Deny {
			return "deny", p.ID
		}
		if !allowed {
			allowed, allowID = true, p.ID
		}
	}
	if allowed {
		return "allow", allowID
	}
	return "deny", 0
}

func splitSegments(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// pickTarget selects a target by weighted round-robin (ADR §5 step 5).
func (s *Server) pickTarget(route *snapshot.Route) (snapshot.Target, error) {
	if len(route.Targets) == 0 {
		return snapshot.Target{}, fmt.Errorf("no targets")
	}
	total := 0
	for _, t := range route.Targets {
		total += t.Weight
	}
	counterAny, _ := s.rr.LoadOrStore(route.Name, &atomic.Uint64{})
	counter := counterAny.(*atomic.Uint64)
	n := counter.Add(1) % uint64(total)
	for _, t := range route.Targets {
		if n < uint64(t.Weight) {
			return t, nil
		}
		n -= uint64(t.Weight)
	}
	return route.Targets[0], nil
}

func sanitizeRequestID(v string) string {
	v = strings.TrimSpace(v)
	if !requestIDPattern.MatchString(v) {
		return ""
	}
	return v
}

func generateRequestID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("req_%x_%x", time.Now().UnixNano(), b)
}

func sanitizeUserID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 128 {
		return ""
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return v
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if ne, ok := err.(net.Error); ok {
		return ne.Timeout()
	}
	return strings.Contains(strings.ToLower(err.Error()), "timeout")
}

// TLSConfig builds the data-plane server TLS configuration: a server
// certificate plus mandatory client certificates (verified by fingerprint at
// the HTTP layer, ADR §6.3.2).
func TLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("proxy: load server keypair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		// Request (not Require) a client certificate: the single enforcement
		// point is the HTTP layer (identity.Resolve), so a missing or unknown
		// certificate yields the ADR §5 semantics — an HTTP 401 — instead of a
		// TLS handshake failure. Chain trust is intentionally not enforced
		// server-side; identity is pinned by fingerprint.
		ClientAuth: tls.RequestClientCert,
		MinVersion: tls.VersionTLS12,
	}, nil
}
