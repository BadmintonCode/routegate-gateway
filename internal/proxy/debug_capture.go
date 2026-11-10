package proxy

import (
	"bytes"
	"io"
	"net/http"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

// debugCapture is request-scoped and only allocated when debug logging is
// explicitly enabled. It stores bounded, sanitized data; the original
// request/response bytes keep streaming normally.
type debugCapture struct {
	ds *store.DebugSettings

	requestHeaders map[string][]string
	requestCookies map[string]string

	requestBody  captureBuffer
	responseBody captureBuffer

	responseStatus   int
	responseHijacked bool

	responseHeaders map[string][]string
	responseCookies map[string]string

	headersTruncated      bool // request header/cookie budget exhausted
	responseHdrsTruncated bool

	requestContentType, requestContentEncoding   string
	responseContentType, responseContentEncoding string
}

func newDebugCapture(ds *store.DebugSettings, r *http.Request) *debugCapture {
	h, hTruncated := accesslog.SanitizedHeaders(r.Header, ds.HeadersMaxBytes)
	cookies, cTruncated := accesslog.SanitizedCookies(r.Header.Values("Cookie"), ds.HeadersMaxBytes)
	return &debugCapture{
		ds:                     ds,
		requestHeaders:         h,
		requestCookies:         cookies,
		headersTruncated:       hTruncated || cTruncated,
		requestContentType:     r.Header.Get("Content-Type"),
		requestContentEncoding: r.Header.Get("Content-Encoding"),
	}
}

type captureBuffer struct {
	data      bytes.Buffer
	bytes     int64
	truncated bool
}

func (b *captureBuffer) appendBytes(p []byte, max int) {
	b.bytes += int64(len(p))
	if b.truncated {
		return
	}
	remaining := max - b.data.Len()
	if remaining <= 0 {
		b.truncated = true
		return
	}
	if len(p) > remaining {
		_, _ = b.data.Write(p[:remaining])
		b.truncated = true
		return
	}
	_, _ = b.data.Write(p)
}

// debugRequestBody is a transparent bounded tee. It never returns capture
// errors to the caller and preserves Close semantics, so streaming requests
// (chunked, Expect: 100-continue, uploads) behave exactly as before.
type debugRequestBody struct {
	inner io.ReadCloser
	cap   *debugCapture
}

func (b *debugRequestBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if n > 0 {
		b.cap.requestBody.appendBytes(p[:n], b.cap.ds.RequestBodyMaxBytes)
	}
	return n, err
}

func (b *debugRequestBody) Close() error { return b.inner.Close() }

// header snapshots the final response headers. Called from statusRecorder
// immediately before the status is committed.
func (d *debugCapture) header(h http.Header) {
	clean, truncated := accesslog.SanitizedHeaders(h, d.ds.HeadersMaxBytes)
	d.responseHeaders = clean
	cookies, cookieTruncated := accesslog.SanitizedCookies(h.Values("Set-Cookie"), d.ds.HeadersMaxBytes)
	d.responseCookies = cookies
	d.responseHdrsTruncated = truncated || cookieTruncated
	d.responseContentType = h.Get("Content-Type")
	d.responseContentEncoding = h.Get("Content-Encoding")
}

func (d *debugCapture) writeHeader(status int) {
	if d.responseStatus == 0 {
		d.responseStatus = status
	}
}

func (d *debugCapture) write(p []byte) {
	d.responseBody.appendBytes(p, d.ds.ResponseBodyMaxBytes)
}

// hijacked marks an upgraded connection (WebSocket): the stream is no longer
// an HTTP response, so payload capture is explicitly unavailable.
func (d *debugCapture) hijacked() { d.responseHijacked = true }

// record assembles the DebugRecord from the sanitized captures. It receives
// the finished access Record so metadata stays identical between the two logs.
func (d *debugCapture) record(meta accesslog.Record) accesslog.DebugRecord {
	requestPayload := accesslog.EncodePayload(d.requestBody.data.Bytes(), d.requestContentType, d.requestContentEncoding, d.ds.RequestBodyMaxBytes)
	requestPayload.Bytes = d.requestBody.bytes
	requestPayload.Truncated = requestPayload.Truncated || d.requestBody.truncated
	// Early rejects (unauthenticated/route_not_found/deny) never read the
	// body: say so instead of implying an empty payload was seen.
	if meta.Decision != "allow" && d.requestBody.bytes == 0 {
		requestPayload.Unavailable = true
		requestPayload.Available = false
	}

	responsePayload := accesslog.EncodePayload(d.responseBody.data.Bytes(), d.responseContentType, d.responseContentEncoding, d.ds.ResponseBodyMaxBytes)
	responsePayload.Bytes = d.responseBody.bytes
	responsePayload.Unavailable = d.responseHijacked
	responsePayload.Available = !d.responseHijacked

	return accesslog.DebugRecord{
		TS: meta.TS, InstanceID: meta.InstanceID, RequestID: meta.RequestID,
		Caller: meta.Caller, Route: meta.Route, Method: meta.Method, Path: meta.Path,
		MatchedPolicyID: meta.MatchedPolicyID, ConfigRevision: meta.ConfigRevision,
		Decision: meta.Decision, Outcome: meta.Outcome, Status: meta.Status, DurationMS: meta.DurationMS,
		BizCode: meta.BizCode, BizMessage: meta.BizMessage,
		UpstreamHost: meta.UpstreamHost,
		RequestBytes: d.requestBody.bytes, ResponseBytes: d.responseBody.bytes,
		RequestHeaders:           d.requestHeaders,
		RequestHeadersTruncated:  d.headersTruncated,
		ResponseHeaders:          d.responseHeaders,
		ResponseHeadersTruncated: d.responseHdrsTruncated,
		RequestCookies:           d.requestCookies,
		ResponseCookies:          d.responseCookies,
		RequestBody:              requestPayload,
		ResponseBody:             responsePayload,
	}
}
