package accesslog

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizedHeadersExcludesSensitiveAndIP(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer super-secret")
	h.Set("Proxy-Authorization", "Basic zzz")
	h.Set("X-Forwarded-For", "1.2.3.4")
	h.Set("X-Forwarded-Proto", "https")
	h.Set("X-Real-IP", "5.6.7.8")
	h.Set("True-Client-IP", "9.9.9.9")
	h.Set("X-Gateway-Caller", "edge")
	h.Set("X-Authenticated-User-Id", "42")
	h.Set("X-Api-Key", "key-material")
	h.Set("X-Signature", "sig")
	h.Set("X-Csrf-Token", "csrf")
	h.Set("Cookie", "a=b")
	h.Set("Set-Cookie", "a=b")
	h.Set("Host", "routegate.local")
	h.Set("X-Custom-Trace", "keep-me")

	out, truncated := SanitizedHeaders(h, 0)
	if truncated {
		t.Fatal("unexpected truncation without budget")
	}
	for key := range out {
		if forbiddenDebugHeader(strings.ToLower(key)) {
			t.Errorf("sensitive header %q survived sanitization", key)
		}
	}
	if got := out["X-Custom-Trace"]; len(got) != 1 || got[0] != "keep-me" {
		t.Errorf("ordinary header lost: %v", got)
	}
}

func TestSanitizedHeadersBudget(t *testing.T) {
	h := http.Header{}
	h.Set("X-Big", strings.Repeat("x", 64))
	_, truncated := SanitizedHeaders(h, 8)
	if !truncated {
		t.Fatal("expected truncation with a tiny budget")
	}
}

// TestSanitizedHeadersDeterministicBudget: an over-budget header is skipped
// while smaller ones still fit, and repeated calls over fresh (randomized)
// maps produce the identical captured set.
func TestSanitizedHeadersDeterministicBudget(t *testing.T) {
	h := http.Header{}
	h.Set("X-Big", strings.Repeat("b", 40)) // sorts before the small headers, blows the budget
	h.Set("X-Small-1", "a")
	h.Set("X-Small-2", "c")

	out, truncated := SanitizedHeaders(h, 24)
	if !truncated {
		t.Fatal("expected the truncated flag")
	}
	if _, ok := out["X-Big"]; ok {
		t.Error("over-budget header must not be captured")
	}
	if len(out["X-Small-1"]) != 1 || len(out["X-Small-2"]) != 1 {
		t.Errorf("smaller headers must still be captured after the skip: %v", out)
	}

	// Fresh maps on every call randomize Go's map order; the captured set must
	// not depend on it.
	want := fmt.Sprint(out)
	for i := 0; i < 200; i++ {
		in := make(http.Header, len(h))
		for k, vs := range h {
			in[k] = append([]string(nil), vs...)
		}
		got, tr := SanitizedHeaders(in, 24)
		if tr != truncated || fmt.Sprint(got) != want {
			t.Fatalf("non-deterministic capture on iteration %d: %v vs %v", i, got, out)
		}
	}
}

func TestSanitizedCookies(t *testing.T) {
	out, _ := SanitizedCookies([]string{"session-id=abc123; theme=dark", "auth_token=xyz; lang=zh"}, 0)
	if out["session-id"] != "<redacted>" || out["auth_token"] != "<redacted>" {
		t.Errorf("credential cookies not redacted: %v", out)
	}
	if out["theme"] != "dark" || out["lang"] != "zh" {
		t.Errorf("ordinary cookies lost: %v", out)
	}
}

func TestEncodePayload(t *testing.T) {
	p := EncodePayload([]byte(`{"a":1}`), "application/json", "", 0)
	if p.Encoding != "text" || p.Data != `{"a":1}` || !p.Available {
		t.Errorf("json payload: %+v", p)
	}
	p2 := EncodePayload([]byte{0x00, 0x01, 0x02, 0xFF}, "application/octet-stream", "gzip", 0)
	if p2.Encoding != "base64" {
		t.Errorf("binary payload should be base64: %+v", p2)
	}
	dec, err := base64.StdEncoding.DecodeString(p2.Data)
	if err != nil || len(dec) != 4 {
		t.Errorf("base64 round-trip failed: %v %q", err, p2.Data)
	}
	p3 := EncodePayload(make([]byte, 100), "text/plain", "", 10)
	if !p3.Truncated || len(p3.Data) != 10 || p3.Bytes != 100 {
		t.Errorf("truncation metadata wrong: %+v", p3)
	}
}

// TestDebugSinkRoundTrip: records survive the asynchronous queue, rotation
// machinery, and JSON round-trip with all payload fields intact.
func TestDebugSinkRoundTrip(t *testing.T) {
	base := filepath.Join(t.TempDir(), "debug.log")
	sink, err := NewDebugFile(DebugFileOptions{
		FileOptions:   FileOptions{Path: base, MaxBytes: 1 << 20, RetainDays: 1},
		QueueCapacity: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := DebugRecord{
		TS: time.Now().UTC().Truncate(time.Millisecond), InstanceID: "inst", RequestID: "req-1",
		Caller: "caller-a", Route: "route-a", Method: "POST", Path: "/v1/x",
		MatchedPolicyID: 3, ConfigRevision: 9, Decision: "allow", Status: 201,
		DurationMS: 4, RequestBytes: 12, ResponseBytes: 34,
		RequestHeaders:          map[string][]string{"X-A": {"b"}},
		RequestHeadersTruncated: true,
		RequestCookies:          map[string]string{"k": "v"},
		RequestBody:             Payload{Available: true, Encoding: "text", Data: "hello"},
		ResponseBody:            Payload{Available: true, Encoding: "base64", Data: "aGk=", Truncated: true},
	}
	sink.EmitDebug(want)

	deadline := time.Now().Add(3 * time.Second)
	for {
		paths, err := DebugSegmentPaths(base)
		if err == nil {
			recs, err := ReadDebugRecordsBackward(paths, 10, nil)
			if err == nil && len(recs) == 1 {
				got := recs[0]
				if got.RequestID != want.RequestID || got.Caller != want.Caller || got.Status != want.Status ||
					got.Decision != want.Decision || got.MatchedPolicyID != want.MatchedPolicyID ||
					got.RequestBytes != want.RequestBytes || got.ResponseBytes != want.ResponseBytes ||
					got.RequestHeaders["X-A"][0] != "b" || got.RequestCookies["k"] != "v" ||
					!got.RequestHeadersTruncated || got.RequestBody.Data != "hello" ||
					got.ResponseBody.Data != "aGk=" || !got.ResponseBody.Truncated {
					t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, want)
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("debug record never reached the file")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDebugSinkNilFilterReadsAll(t *testing.T) {
	recs, err := ReadDebugRecordsBackward([]string{filepath.Join(t.TempDir(), "missing.log")}, 10, nil)
	if err != nil || len(recs) != 0 {
		t.Errorf("missing segment should yield empty output, got %d records, err %v", len(recs), err)
	}
}

// TestDebugSinkCloseRacesEmit: an in-flight EmitDebug racing Close must never
// panic in the caller's goroutine — sends either land before the drain or drop
// via the sink's closed branch (regression for the send-on-closed-channel
// panic window).
func TestDebugSinkCloseRacesEmit(t *testing.T) {
	base := filepath.Join(t.TempDir(), "debug.log")
	sink, err := NewDebugFile(DebugFileOptions{
		FileOptions:   FileOptions{Path: base, MaxBytes: 1 << 20, RetainDays: 1},
		QueueCapacity: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(50 * time.Millisecond)
		for i := 0; time.Now().Before(deadline); i++ {
			sink.EmitDebug(DebugRecord{RequestID: fmt.Sprintf("race-%d", i)})
		}
	}()
	time.Sleep(time.Millisecond)
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	_ = sink.Close() // idempotent
}
