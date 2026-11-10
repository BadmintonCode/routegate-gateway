package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// sseEmitter writes Server-Sent Events for the assistant chat stream. Writes
// are best-effort: once the client is gone every send reports false and the
// handler keeps persisting the turn server-side.
type sseEmitter struct {
	mu    sync.Mutex
	w     http.ResponseWriter
	flush http.Flusher
	ok    bool
}

// newSSEEmitter sets the SSE response headers and flushes the preamble so
// intermediaries see the stream start immediately.
func newSSEEmitter(w http.ResponseWriter) *sseEmitter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-store")
	h.Set("X-Accel-Buffering", "no")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	f, canFlush := w.(http.Flusher)
	em := &sseEmitter{w: w, flush: f, ok: true}
	if canFlush {
		f.Flush()
	}
	return em
}

// send writes one named event with a JSON payload; false means the write
// failed (client disconnected) and no further sends are useful.
func (e *sseEmitter) send(event string, payload any) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.ok {
		return false
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		e.ok = false
		return false
	}
	if e.flush != nil {
		e.flush.Flush()
	}
	return true
}

// startHeartbeat writes an SSE comment line every interval so reasoning
// phases and long-running tools do not look like a stalled connection to the
// browser or any intermediary. Comment lines are ignored by EventSource
// parsers and by our own block parser (they carry no "data:" line). The
// returned func stops the ticker; it is safe to call multiple times.
func (e *sseEmitter) startHeartbeat(interval time.Duration) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				e.mu.Lock()
				if e.ok {
					// Best-effort: a failed write flips ok and every later
					// send becomes a no-op, so the goroutine stays cheap.
					_, _ = fmt.Fprint(e.w, ": ping\n\n")
					if e.flush != nil {
						e.flush.Flush()
					}
				}
				e.mu.Unlock()
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}
