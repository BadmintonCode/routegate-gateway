package accesslog

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Payload is a bounded wire capture. Data is UTF-8 text when Encoding=text or
// base64 when Encoding=base64; it is never an unbounded string.
type Payload struct {
	Available       bool   `json:"available"`
	Unavailable     bool   `json:"unavailable,omitempty"`
	Data            string `json:"data,omitempty"`
	Encoding        string `json:"encoding,omitempty"` // text | base64
	ContentType     string `json:"content_type,omitempty"`
	ContentEncoding string `json:"content_encoding,omitempty"`
	Bytes           int64  `json:"bytes"`
	Truncated       bool   `json:"truncated,omitempty"`
}

// DebugRecord is separate from ordinary Record so normal audit sinks can
// never accidentally receive payloads or sensitive headers.
type DebugRecord struct {
	TS                       time.Time           `json:"ts"`
	InstanceID               string              `json:"instance_id"`
	RequestID                string              `json:"request_id"`
	Caller                   string              `json:"caller"`
	Route                    string              `json:"route"`
	Method                   string              `json:"method"`
	Path                     string              `json:"path"`
	MatchedPolicyID          int64               `json:"matched_policy_id,omitempty"`
	ConfigRevision           int64               `json:"config_revision"`
	Decision                 string              `json:"decision"`
	Outcome                  string              `json:"outcome,omitempty"`
	BizCode                  string              `json:"biz_code,omitempty"`
	BizMessage               string              `json:"biz_message,omitempty"`
	UpstreamHost             string              `json:"upstream_host,omitempty"`
	Status                   int                 `json:"status"`
	DurationMS               int64               `json:"duration_ms"`
	RequestBytes             int64               `json:"request_bytes"`
	ResponseBytes            int64               `json:"response_bytes"`
	RequestHeaders           map[string][]string `json:"request_headers,omitempty"`
	RequestHeadersTruncated  bool                `json:"request_headers_truncated,omitempty"`
	ResponseHeaders          map[string][]string `json:"response_headers,omitempty"`
	ResponseHeadersTruncated bool                `json:"response_headers_truncated,omitempty"`
	RequestCookies           map[string]string   `json:"request_cookies,omitempty"`
	ResponseCookies          map[string]string   `json:"response_cookies,omitempty"`
	RequestBody              Payload             `json:"request_body"`
	ResponseBody             Payload             `json:"response_body"`
}

// DebugFileOptions configures the independent sensitive debug sink.
type DebugFileOptions struct {
	FileOptions
	QueueCapacity int
}

// DebugSink is an asynchronous bounded sink. A full queue drops a debug
// record rather than slowing or failing the business request.
type DebugSink struct {
	base    *rotationFileSink
	queue   chan DebugRecord
	closed  chan struct{}
	done    chan struct{}
	dropped atomic.Uint64
}

// NewDebugFile creates a separate rotating debug file sink. It never shares a
// path or Record type with ordinary access logs. The base rotating sink is
// used bare: DebugSink provides the bounded queue itself, and the ordinary
// NewFile async wrapper stays reserved for the hot-path access sinks.
func NewDebugFile(opts DebugFileOptions) (*DebugSink, error) {
	base, err := newRotationFileSink(opts.FileOptions)
	if err != nil {
		return nil, err
	}
	cap := opts.QueueCapacity
	if cap < 1 {
		cap = 1024
	}
	s := &DebugSink{base: base, queue: make(chan DebugRecord, cap), closed: make(chan struct{}), done: make(chan struct{})}
	go s.run()
	return s, nil
}

// emit serializes and writes one record to the rotating file.
func (s *DebugSink) emit(rec DebugRecord) {
	b, err := json.Marshal(rec)
	if err == nil {
		s.base.emitLine(append(b, '\n'))
	}
}

// run consumes the queue until the sink is closed, then drains whatever was
// already buffered and exits. The queue channel is never closed: closing it
// would panic in-flight EmitDebug sends racing the shutdown (a send case on a
// closed channel panics even inside a select).
func (s *DebugSink) run() {
	defer close(s.done)
	for {
		select {
		case rec := <-s.queue:
			s.emit(rec)
		case <-s.closed:
			for {
				select {
				case rec := <-s.queue:
					s.emit(rec)
				default:
					return
				}
			}
		}
	}
}

// EmitDebug is intentionally non-blocking. After Close it drops: the closed
// branch makes the send safe against the shutdown race without locking.
func (s *DebugSink) EmitDebug(rec DebugRecord) {
	select {
	case s.queue <- rec:
	case <-s.closed:
		s.dropped.Add(1)
	default:
		s.dropped.Add(1)
	}
}

func (s *DebugSink) Dropped() uint64 { return s.dropped.Load() }

// Close stops the consumer, waits for the buffered records to drain, then
// closes the underlying file. Concurrent EmitDebug calls are safe: they either
// land in the buffer before the drain or drop via the closed branch.
func (s *DebugSink) Close() error {
	select {
	case <-s.closed:
		return nil
	default:
		close(s.closed)
	}
	<-s.done
	return s.base.Close()
}

// DebugSegmentPaths and ReadDebugRecordsBackward mirror the ordinary file
// reader while decoding the separate DebugRecord shape.
func DebugSegmentPaths(basePath string) ([]string, error) { return SegmentPaths(basePath) }

func ReadDebugRecordsBackward(paths []string, limit int, filter func(DebugRecord) bool) ([]DebugRecord, error) {
	if limit <= 0 {
		limit = 200
	}
	out := []DebugRecord{}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			var rec DebugRecord
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
			if filter != nil && !filter(rec) {
				continue
			}
			out = append(out, rec)
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// SanitizedHeaders applies the explicit-IP/credential exclusion contract.
// Cookie values are handled separately; Cookie and Set-Cookie are omitted.
// Keys are visited in sorted order and an over-budget header is skipped while
// smaller later headers still fit, so the same request always captures the
// same header set regardless of Go's randomized map order.
func SanitizedHeaders(in http.Header, maxBytes int) (map[string][]string, bool) {
	out := map[string][]string{}
	used := 0
	truncated := false
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		lower := strings.ToLower(key)
		if forbiddenDebugHeader(lower) {
			continue
		}
		for _, value := range in[key] {
			if maxBytes > 0 && used+len(key)+len(value) > maxBytes {
				// This header does not fit: set the truncated flag and keep
				// going with the remaining (smaller) headers instead of
				// dropping everything after it.
				truncated = true
				break
			}
			out[key] = append(out[key], value)
			used += len(key) + len(value)
		}
	}
	return out, truncated
}

func forbiddenDebugHeader(lower string) bool {
	if lower == "authorization" || lower == "proxy-authorization" || lower == "cookie" || lower == "set-cookie" {
		return true
	}
	for _, prefix := range []string{
		"forwarded", "x-forwarded-", "x-real-ip", "x-client-ip", "true-client-ip",
		"x-gateway-", "x-authenticated-", "x-api-key", "x-access-token", "x-refresh-token",
		"x-signature", "x-csrf", "proxy-",
	} {
		if lower == prefix || strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return lower == "host" || lower == "origin" || lower == "referer" || lower == "location"
}

// SanitizedCookies records cookie names and non-sensitive values; common
// session/token/auth cookies are represented by <redacted>.
func SanitizedCookies(values []string, maxBytes int) (map[string]string, bool) {
	out := map[string]string{}
	used := 0
	truncated := false
	for _, header := range values {
		for _, part := range strings.Split(header, ";") {
			pair := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(pair) != 2 || pair[0] == "" {
				continue
			}
			name, value := pair[0], pair[1]
			if sensitiveCookie(strings.ToLower(name)) {
				value = "<redacted>"
			}
			if maxBytes > 0 && used+len(name)+len(value) > maxBytes {
				truncated = true
				return out, truncated
			}
			out[name] = value
			used += len(name) + len(value)
		}
	}
	return out, truncated
}

func sensitiveCookie(name string) bool {
	for _, term := range []string{"session", "token", "auth", "secret", "csrf", "password", "key"} {
		if strings.Contains(name, term) {
			return true
		}
	}
	return false
}

// EncodePayload keeps textual content readable and uses base64 for binary or
// non-UTF-8 data.
func EncodePayload(data []byte, contentType, contentEncoding string, maxBytes int) Payload {
	p := Payload{Available: true, ContentType: contentType, ContentEncoding: contentEncoding, Bytes: int64(len(data))}
	if maxBytes > 0 && len(data) > maxBytes {
		p.Truncated = true
		data = data[:maxBytes]
	}
	if strings.HasPrefix(strings.ToLower(contentType), "text/") || strings.Contains(strings.ToLower(contentType), "json") || strings.Contains(strings.ToLower(contentType), "xml") {
		p.Encoding = "text"
		p.Data = string(data)
	} else {
		p.Encoding = "base64"
		p.Data = base64.StdEncoding.EncodeToString(data)
	}
	return p
}
