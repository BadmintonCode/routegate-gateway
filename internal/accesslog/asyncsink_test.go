package accesslog

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSink records delivered records and can stall the writer goroutine so
// tests exercise the queue-full drop path deterministically.
type fakeSink struct {
	mu          sync.Mutex
	records     []Record
	entered     int           // Emit invocations, including the one currently blocked
	closes      int           // Close invocations
	block       chan struct{} // non-nil: every Emit blocks until it is closed
	releaseOnce sync.Once
}

func (f *fakeSink) Emit(r Record) {
	f.mu.Lock()
	f.entered++
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	f.records = append(f.records, r)
	f.mu.Unlock()
}

func (f *fakeSink) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeSink) delivered() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

func (f *fakeSink) enteredCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entered
}

// release unblocks the stalled writer exactly once, so tests can call it and
// still keep a panic-free deferred release for failure paths.
func (f *fakeSink) release() {
	if f.block == nil {
		return
	}
	f.releaseOnce.Do(func() { close(f.block) })
}

// TestAsyncSinkDelivers: records emitted before Close all reach the underlying
// sink, in order, with nothing dropped.
func TestAsyncSinkDelivers(t *testing.T) {
	base := &fakeSink{}
	s := newAsyncSink(base, asyncQueueCapacity)
	const n = 50
	for i := 0; i < n; i++ {
		s.Emit(rec(i + 1))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if base.delivered() != n {
		t.Fatalf("delivered = %d, want %d", base.delivered(), n)
	}
	if got := s.Dropped(); got != 0 {
		t.Fatalf("dropped = %d, want 0", got)
	}
	for i, r := range base.records {
		if r.RequestID != rec(i+1).RequestID {
			t.Fatalf("record %d out of order: got %s", i, r.RequestID)
		}
	}
}

// TestAsyncSinkFullQueueDropsWithoutBlocking: with the underlying sink stalled,
// the queue fills and further emits drop immediately instead of blocking the
// caller. Every record is either delivered or counted dropped.
func TestAsyncSinkFullQueueDropsWithoutBlocking(t *testing.T) {
	base := &fakeSink{block: make(chan struct{})}
	s := newAsyncSink(base, asyncQueueCapacity)
	defer func() {
		base.release() // release the writer if the test failed early
		_ = s.Close()
	}()

	// More records than the queue can hold while the writer is stuck in the
	// first Emit: the overflow must drop, never block.
	const n = 2*asyncQueueCapacity + 10
	start := time.Now()
	for i := 0; i < n; i++ {
		s.Emit(rec(i + 1))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("emit loop blocked for %s; Emit must be non-blocking", elapsed)
	}
	if got := s.Dropped(); got == 0 {
		t.Fatal("expected drops with a stalled sink and a full queue")
	}

	base.release()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := base.delivered() + int(s.Dropped()); got != n {
		t.Fatalf("delivered %d + dropped %d = %d, want %d", base.delivered(), s.Dropped(), got, n)
	}
	if base.delivered() < 1 {
		t.Fatal("the first queued record was never delivered")
	}
}

// TestAsyncSinkCloseDrains: records emitted while the writer is stalled are
// flushed by Close before the underlying sink is closed.
func TestAsyncSinkCloseDrains(t *testing.T) {
	base := &fakeSink{block: make(chan struct{})}
	s := newAsyncSink(base, asyncQueueCapacity)
	defer func() {
		base.release()
		_ = s.Close()
	}()

	s.Emit(rec(1))
	deadline := time.Now().Add(3 * time.Second)
	for base.enteredCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if base.enteredCount() == 0 {
		t.Fatal("writer goroutine never picked up the first record")
	}
	// The writer is stalled in Emit; these land in the queue.
	for i := 2; i <= 200; i++ {
		s.Emit(rec(i))
	}
	base.release()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := base.delivered(); got != 200 {
		t.Fatalf("Close delivered %d records, want 200 (it must drain the queue)", got)
	}
	if got := s.Dropped(); got != 0 {
		t.Fatalf("dropped = %d, want 0", got)
	}
	if base.closes != 1 {
		t.Fatalf("underlying Close called %d times, want 1", base.closes)
	}
}

// TestAsyncSinkCloseOnce: a second Close does not re-close the underlying sink
// and returns the same result.
func TestAsyncSinkCloseOnce(t *testing.T) {
	base := &fakeSink{}
	s := newAsyncSink(base, asyncQueueCapacity)
	s.Emit(rec(1))
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if base.closes != 1 {
		t.Fatalf("underlying Close called %d times, want 1", base.closes)
	}
	if base.delivered() != 1 {
		t.Fatalf("delivered = %d, want 1", base.delivered())
	}
}

// TestAsyncSinkEmitDuringClose: -race exercise of emits racing the shutdown.
// No record may appear after Close returns (the writer has exited), and every
// record is delivered or dropped.
func TestAsyncSinkEmitDuringClose(t *testing.T) {
	base := &fakeSink{}
	s := newAsyncSink(base, asyncQueueCapacity)
	const workers, perWorker = 8, 250
	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				s.Emit(rec(g*perWorker + i + 1))
			}
		}(g)
	}
	var closeErr error
	var closeOnce sync.Once
	go closeOnce.Do(func() { closeErr = s.Close() })
	wg.Wait()
	closeOnce.Do(func() { closeErr = s.Close() }) // ensure Close ran even if the goroutine lost the race
	if closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	total := workers * perWorker
	if got := base.delivered() + int(s.Dropped()); got > total {
		t.Fatalf("delivered %d + dropped %d = %d exceeds emitted %d", base.delivered(), s.Dropped(), got, total)
	}
	// The writer goroutine has exited with Close: the delivery count is final.
	settled := base.delivered()
	time.Sleep(20 * time.Millisecond)
	if base.delivered() != settled {
		t.Fatalf("records delivered after Close returned: %d -> %d", settled, base.delivered())
	}
}

// TestConstructorsReturnAsyncWrapper: NewStdout and NewFile hand the hot path
// the non-blocking wrapper, which satisfies Sink and DroppedReporter (the
// assertion /__metrics relies on) and exposes the underlying sink.
func TestConstructorsReturnAsyncWrapper(t *testing.T) {
	s := NewStdout(&strings.Builder{})
	var _ Sink = s
	as, ok := s.(*asyncSink)
	if !ok {
		t.Fatalf("NewStdout returned %T, want *asyncSink", s)
	}
	var _ DroppedReporter = as // /__metrics reads Dropped through this interface
	if _, ok := as.Underlying().(*stdoutSink); !ok {
		t.Errorf("stdout wrapper underlying = %T, want *stdoutSink", as.Underlying())
	}
	if as.Dropped() != 0 {
		t.Errorf("fresh wrapper dropped = %d, want 0", as.Dropped())
	}

	dir := t.TempDir()
	fs, err := NewFile(FileOptions{Path: dir + "/access.log", MaxBytes: 1 << 20, RetainDays: 7})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	var _ Sink = fs
	fa, ok := fs.(*asyncSink)
	if !ok {
		t.Fatalf("NewFile returned %T, want *asyncSink", fs)
	}
	var _ DroppedReporter = fa
	rot, ok := fa.Underlying().(*rotationFileSink)
	if !ok {
		t.Fatalf("file wrapper underlying = %T, want *rotationFileSink", fa.Underlying())
	}
	// Records flow through the wrapper into the rotation sink's segments.
	fs.Emit(rec(1))
	if err := fs.Close(); err != nil {
		t.Fatalf("file sink Close: %v", err)
	}
	paths, err := SegmentPaths(dir + "/access.log")
	if err != nil {
		t.Fatal(err)
	}
	if got := countLines(t, paths); got != 1 {
		t.Fatalf("segments carry %d lines, want 1", got)
	}
	// The wrapper reports its own drops; the rotation sink keeps its counter.
	if fa.Dropped() != 0 || rot.Dropped() != 0 {
		t.Errorf("healthy run dropped: wrapper %d, rotation %d", fa.Dropped(), rot.Dropped())
	}
}
