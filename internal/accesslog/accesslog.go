// Package accesslog emits access-decision records (ADR-0001 §6.5). Records
// never contain credentials, tokens, or bodies. Sinks:
//
//   - stdout: JSON lines to the process stdout (default; collect with
//     Promtail/Filebeat into Loki for production).
//   - file:   daily-segmented rotating files under the directory of
//     ROUTEGATE_ACCESS_LOG_FILE, named <stem>-<YYYYMMDD>-<seq>.log. A segment
//     rolls when the day changes or the next record would exceed the size cap
//     (default 2 MiB). An hourly janitor (plus a sweep at startup) deletes
//     segments older than ROUTEGATE_ACCESS_LOG_RETAIN_DAYS (default 7), using
//     the date encoded in the filename.
package accesslog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Record is one access-decision entry.
type Record struct {
	TS              time.Time `json:"ts"`
	InstanceID      string    `json:"instance_id"`
	RequestID       string    `json:"request_id"`
	Caller          string    `json:"caller"`
	Route           string    `json:"route"`
	Method          string    `json:"method"`
	Path            string    `json:"path"`
	MatchedPolicyID int64     `json:"matched_policy_id,omitempty"`
	ConfigRevision  int64     `json:"config_revision"`
	Decision        string    `json:"decision"`              // allow | deny | unauthenticated | route_not_found | starting | tls_required | upstream_error
	Outcome         string    `json:"outcome,omitempty"`     // success | failure; routes with a success rule only (§6.7)
	BizCode         string    `json:"biz_code,omitempty"`    // business status code extracted from the response body (§6.7)
	BizMessage      string    `json:"biz_message,omitempty"` // business message extracted from the response body
	Status          int       `json:"status"`
	DurationMS      int64     `json:"duration_ms"`
	// RequestBytes counts bytes actually read from the client body while
	// forwarding (content-length and chunked alike). Early rejects before
	// forwarding keep 0: the body is never consumed on those paths.
	RequestBytes  int64  `json:"request_bytes"`
	ResponseBytes int64  `json:"response_bytes"`
	UpstreamHost  string `json:"upstream_host,omitempty"`
}

// Sink receives records; implementations must never block the hot path for
// long.
type Sink interface {
	Emit(Record)
	Close() error
}

// DroppedReporter is implemented by sinks that discard records instead of
// blocking the hot path; /__metrics reports the counters.
type DroppedReporter interface {
	Dropped() uint64
}

type stdoutSink struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewStdout returns the default JSON-lines sink writing to w (os.Stdout),
// wrapped in the asynchronous bounded-queue sink: Emit never blocks on the
// encoder; a full queue drops instead (ADR-0001 §5 [9]).
func NewStdout(w io.Writer) Sink {
	return newAsyncSink(&stdoutSink{enc: json.NewEncoder(w)}, asyncQueueCapacity)
}

func (s *stdoutSink) Emit(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(&r)
}

func (s *stdoutSink) Close() error { return nil }

// asyncQueueCapacity bounds the per-sink record queue (same default as the
// debug sink). A full queue drops the record instead of blocking the hot path
// (ADR-0001 §5 step [9]: the decision log is async, buffered, droppable).
const asyncQueueCapacity = 1024

// asyncSink wraps any Sink with a bounded queue and one writer goroutine, so
// Emit never blocks on the underlying sink's I/O or its lock — the same
// contract as DebugSink, applied to the ordinary access-log sinks. A full
// queue drops the record and counts it; a stalled underlying sink can delay
// delivery but never the request goroutines. NewStdout and NewFile install
// this wrapper by default.
type asyncSink struct {
	base      Sink
	queue     chan Record
	closed    chan struct{}
	done      chan struct{}
	dropped   atomic.Uint64 // this wrapper's drops; the wrapped sink keeps its own counter
	closeOnce sync.Once
	closeErr  error
}

func newAsyncSink(base Sink, capacity int) *asyncSink {
	if capacity < 1 {
		capacity = asyncQueueCapacity
	}
	s := &asyncSink{
		base:   base,
		queue:  make(chan Record, capacity),
		closed: make(chan struct{}),
		done:   make(chan struct{}),
	}
	go s.run()
	return s
}

// run consumes the queue until the sink is closed, then drains whatever was
// already buffered and exits. The queue channel is never closed: closing it
// would panic in-flight Emit sends racing the shutdown (same contract as
// DebugSink.run).
func (s *asyncSink) run() {
	defer close(s.done)
	for {
		select {
		case r := <-s.queue:
			s.base.Emit(r)
		case <-s.closed:
			for {
				select {
				case r := <-s.queue:
					s.base.Emit(r)
				default:
					return
				}
			}
		}
	}
}

// Emit is intentionally non-blocking: a full queue drops the record rather
// than stalling the request goroutine. After Close it also drops: the closed
// branch makes the send safe against the shutdown race without locking.
func (s *asyncSink) Emit(r Record) {
	select {
	case s.queue <- r:
	case <-s.closed:
		s.dropped.Add(1)
	default:
		s.dropped.Add(1)
	}
}

// Dropped reports the records this wrapper discarded (full queue, emit racing
// or after close). Implements DroppedReporter for /__metrics; the wrapped
// sink's own failure counter is reported by the wrapped sink itself.
func (s *asyncSink) Dropped() uint64 { return s.dropped.Load() }

// Underlying exposes the wrapped sink (tests reach rotation state through it).
func (s *asyncSink) Underlying() Sink { return s.base }

// Close stops the consumer, waits for the queued records to drain, then closes
// the underlying sink. Idempotent and safe to call concurrently: sync.Once
// serializes the shutdown, and every caller waits for the drain to finish.
func (s *asyncSink) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		<-s.done
		s.closeErr = s.base.Close()
	})
	<-s.done
	return s.closeErr
}

// FileOptions configures the rotating file sink.
type FileOptions struct {
	// Path is the base file path, e.g. /var/log/routegate/access.log. The
	// directory is created if missing; the stem becomes the segment prefix.
	Path       string
	MaxBytes   int64 // per-segment cap; default 2 MiB. A record larger than the cap still completes its own segment.
	RetainDays int   // delete segments older than this many days; default 7
}

const (
	dayLayout     = "20060102"
	seqWidth      = 4
	defaultMaxMBi = 2
	defaultDays   = 7
	janitorPeriod = time.Hour
)

// NewFile returns the rotating file sink wrapped in the asynchronous
// bounded-queue sink: Emit never blocks on rotation or disk I/O; a full queue
// drops instead (ADR-0001 §5 [9]). The hourly janitor is started here and
// stopped by Close.
func NewFile(opts FileOptions) (Sink, error) {
	base, err := newRotationFileSink(opts)
	if err != nil {
		return nil, err
	}
	return newAsyncSink(base, asyncQueueCapacity), nil
}

// newRotationFileSink builds the bare synchronous rotating sink. NewFile wraps
// it in asyncSink for the hot path; NewDebugFile uses it directly (DebugSink
// brings its own bounded queue), and rotation tests drive it synchronously.
func newRotationFileSink(opts FileOptions) (*rotationFileSink, error) {
	dir := filepath.Dir(opts.Path)
	stem := strings.TrimSuffix(filepath.Base(opts.Path), filepath.Ext(opts.Path))
	if stem == "" || stem == "." || stem == string(filepath.Separator) {
		stem = "access"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("accesslog: create dir %s: %w", dir, err)
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxMBi << 20
	}
	retain := opts.RetainDays
	if retain <= 0 {
		retain = defaultDays
	}
	s := &rotationFileSink{
		dir: dir, stem: stem,
		maxBytes: maxBytes, retain: retain,
		nowFn:   time.Now,
		janStop: make(chan struct{}),
	}
	if err := s.openCurrent(); err != nil {
		return nil, err
	}
	go s.janitorLoop()
	return s, nil
}

type rotationFileSink struct {
	mu       sync.Mutex
	dir      string
	stem     string
	maxBytes int64
	retain   int
	nowFn    func() time.Time // overridable in tests

	file    *os.File
	day     string
	seq     int
	size    int64
	closed  bool
	dropped atomic.Uint64 // records discarded instead of blocking the hot path

	janStop chan struct{}
	janOnce sync.Once
}

func (s *rotationFileSink) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

func (s *rotationFileSink) fileName(day string, seq int) string {
	return filepath.Join(s.dir, fmt.Sprintf("%s-%s-%0*d.log", s.stem, day, seqWidth, seq))
}

// openCurrent continues today's latest segment when it is still under the cap
// (restart-friendly, append mode), otherwise starts the next sequence.
func (s *rotationFileSink) openCurrent() error {
	day := s.now().Format(dayLayout)
	seq, size, found := s.latestWritable(day)
	if !found {
		seq, size = 1, 0
	}
	return s.openSeq(day, seq, size)
}

// latestWritable finds today's highest-numbered segment and reports it plus
// its size when it is still under the cap; otherwise the next sequence is
// returned with found=false.
func (s *rotationFileSink) latestWritable(day string) (seq int, size int64, found bool) {
	entries, _ := os.ReadDir(s.dir)
	prefix := s.stem + "-" + day + "-"
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".log") {
			continue
		}
		mid := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".log")
		var n int
		if _, err := fmt.Sscanf(mid, "%d", &n); err != nil || n <= 0 {
			continue
		}
		if n > seq {
			seq = n
		}
	}
	if seq == 0 {
		return 0, 0, false
	}
	info, err := os.Stat(s.fileName(day, seq))
	if err != nil || info.Size() >= s.maxBytes {
		return seq + 1, 0, false
	}
	return seq, info.Size(), true
}

func (s *rotationFileSink) openSeq(day string, seq int, size int64) error {
	if s.file != nil {
		s.file.Close()
		s.file = nil
	}
	f, err := os.OpenFile(s.fileName(day, seq), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	s.file, s.day, s.seq, s.size = f, day, seq, size
	return nil
}

func (s *rotationFileSink) Emit(r Record) {
	b, err := json.Marshal(r)
	if err != nil {
		s.dropped.Add(1)
		return
	}
	s.emitLine(append(b, '\n'))
}

// Dropped reports the records this sink discarded (rotation/open failures,
// emits after close, short writes). Implements DroppedReporter.
func (s *rotationFileSink) Dropped() uint64 { return s.dropped.Load() }

// emitLine writes one already-serialized JSON line under the same rotation
// rules. DebugSink uses this to share the safe daily/size/retention machinery
// without changing the ordinary Record/Sink contract.
func (s *rotationFileSink) emitLine(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.dropped.Add(1)
		return
	}
	now := s.now()
	day := now.Format(dayLayout)
	if s.day != day || s.size+int64(len(b)) > s.maxBytes {
		seq := 1
		if s.day == day {
			seq = s.seq + 1
		}
		if err := s.openSeq(day, seq, 0); err != nil {
			s.dropped.Add(1) // drop rather than block the hot path; /__metrics reports the loss
			return
		}
	}
	n, _ := s.file.Write(b)
	if n < len(b) {
		s.dropped.Add(1)
	}
	s.size += int64(n)
}

func (s *rotationFileSink) janitorLoop() {
	t := time.NewTicker(janitorPeriod)
	defer t.Stop()
	for {
		select {
		case <-s.janStop:
			return
		case <-t.C:
			s.sweep()
		}
	}
}

// sweep deletes segment files whose encoded date falls outside the retention
// window. Files not matching the naming pattern are left untouched.
func (s *rotationFileSink) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	cutoff := s.now().AddDate(0, 0, -s.retain)
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, s.stem+"-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		mid := strings.TrimSuffix(strings.TrimPrefix(name, s.stem+"-"), ".log")
		dayStr := mid
		if i := strings.IndexByte(mid, '-'); i >= 0 {
			dayStr = mid[:i]
		}
		d, err := time.Parse(dayLayout, dayStr)
		if err != nil {
			continue
		}
		if d.Before(cutoff) {
			os.Remove(filepath.Join(s.dir, name))
		}
	}
}

func (s *rotationFileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.janOnce.Do(func() { close(s.janStop) })
	if s.file != nil {
		err := s.file.Close()
		s.file = nil
		return err
	}
	return nil
}

// SegmentPaths returns the segment file paths for the given base path (the
// same value passed to NewFile), sorted newest segment first. Segment names
// embed date and sequence, so lexical descending order is chronological.
func SegmentPaths(basePath string) ([]string, error) {
	dir := filepath.Dir(basePath)
	stem := strings.TrimSuffix(filepath.Base(basePath), filepath.Ext(basePath))
	if stem == "" || stem == "." {
		stem = "access"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := stem + "-"
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".log") {
			out = append(out, filepath.Join(dir, name))
		}
	}
	// newest segment first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// RecordFilter selects which records to keep; nil keeps everything.
type RecordFilter func(Record) bool

// ReadRecordsBackward reads records starting from the newest segment and the
// newest line within it, applying filter until limit records are collected or
// the segments are exhausted. Malformed lines are skipped silently. When
// limit <= 0 it defaults to 200.
func ReadRecordsBackward(paths []string, limit int, filter RecordFilter) ([]Record, error) {
	if limit <= 0 {
		limit = 200
	}
	out := []Record{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // unreadable segment (rotation/permissions): skip
		}
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			var rec Record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
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
