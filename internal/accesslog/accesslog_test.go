package accesslog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestSink builds the bare rotating sink (no async wrapper) so rotation
// tests drive Emit synchronously, exactly as the async writer goroutine does.
func newTestSink(t *testing.T, dir string, maxBytes int64, now time.Time) *rotationFileSink {
	t.Helper()
	s, err := newRotationFileSink(FileOptions{Path: filepath.Join(dir, "access.log"), MaxBytes: maxBytes, RetainDays: 7})
	if err != nil {
		t.Fatalf("newRotationFileSink: %v", err)
	}
	s.mu.Lock()
	s.nowFn = func() time.Time { return now }
	s.mu.Unlock()
	return s
}

func rec(id int) Record {
	return Record{TS: time.Now(), RequestID: fmt.Sprintf("req-%04d-with-some-padding", id), Caller: "demo-caller", Route: "demo-alpha", Method: "GET", Path: "/v1/ping", Decision: "allow"}
}

func segmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "access-") && strings.HasSuffix(e.Name(), ".log") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

func countLines(t *testing.T, files []string) int {
	t.Helper()
	total := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		total += strings.Count(string(b), "\n")
	}
	return total
}

// TestRotationOnSize: with a tiny cap, writes roll into multiple segments and
// every record survives.
func TestRotationOnSize(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	s := newTestSink(t, dir, 400, now)
	defer s.Close()

	for i := 1; i <= 30; i++ {
		s.Emit(rec(i))
	}
	files := segmentFiles(t, dir)
	if len(files) < 2 {
		t.Fatalf("expected >=2 segments with 400B cap, got %d", len(files))
	}
	if got := countLines(t, files); got != 30 {
		t.Fatalf("lines across segments = %d, want 30", got)
	}
	for _, f := range files {
		if st, err := os.Stat(f); err == nil && st.Size() > 400+250 {
			t.Fatalf("segment %s exceeds cap+record: %d", f, st.Size())
		}
	}
}

// TestDailyRollover: a day change starts a new segment named with the new
// date; the previous day's segment is preserved.
func TestDailyRollover(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Date(2026, 11, 6, 23, 59, 0, 0, time.UTC)
	s := newTestSink(t, dir, 1<<20, day1)
	defer s.Close()

	s.Emit(rec(1))
	s.nowFn = func() time.Time { return day1.AddDate(0, 0, 1) }
	s.Emit(rec(2))

	f1 := filepath.Join(dir, "access-20261106-0001.log")
	f2 := filepath.Join(dir, "access-20261107-0001.log")
	b1, err1 := os.ReadFile(f1)
	b2, err2 := os.ReadFile(f2)
	if err1 != nil || err2 != nil {
		t.Fatalf("daily segments missing: %v %v", err1, err2)
	}
	if strings.Count(string(b1), "\n") != 1 || strings.Count(string(b2), "\n") != 1 {
		t.Fatalf("unexpected line distribution: %d/%d", strings.Count(string(b1), "\n"), strings.Count(string(b2), "\n"))
	}
}

// TestRestartContinuesTodaySegment: reopening continues the latest today
// segment when under the cap.
func TestRestartContinuesTodaySegment(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	s1 := newTestSink(t, dir, 1<<20, now)
	s1.Emit(rec(1))
	s1.Close()

	s2 := newTestSink(t, dir, 1<<20, now)
	defer s2.Close()
	s2.Emit(rec(2))

	f := filepath.Join(dir, "access-20261106-0001.log")
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "\n") != 2 {
		t.Fatalf("restart did not append to today's segment: %d lines", strings.Count(string(b), "\n"))
	}
}

// TestRetentionSweep: segments with an encoded date older than the retention
// window are deleted; recent and non-matching files are kept.
func TestRetentionSweep(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	s := newTestSink(t, dir, 1<<20, now)
	defer s.Close()

	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("access-20261001-0001.log", "ancient\n")  // > 7 days  -> delete
	write("access-20261031-0001.log", "within7d\n") // within 7 days -> keep
	write("access-20261105-0001.log", "recent\n")   // keep
	write("other-prefix.log", "keep me\n")          // not a segment -> keep

	s.sweep()

	if _, err := os.Stat(filepath.Join(dir, "access-20261001-0001.log")); !os.IsNotExist(err) {
		t.Error("ancient segment was not deleted")
	}
	for _, keep := range []string{"access-20261031-0001.log", "access-20261105-0001.log", "other-prefix.log"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("file %s should have been kept: %v", keep, err)
		}
	}
}

// TestEmitAfterClose: a closed sink drops records instead of panicking.
func TestEmitAfterClose(t *testing.T) {
	dir := t.TempDir()
	s := newTestSink(t, dir, 1<<20, time.Now())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.Emit(rec(1)) // must not panic or write
	if files := segmentFiles(t, dir); len(files) != 1 {
		t.Fatalf("unexpected files after close: %d", len(files))
	}
}

// TestDroppedCounter: healthy emits drop nothing; emits after close and
// rotation open-failures drop-and-count instead of blocking the hot path.
// The sink also satisfies the DroppedReporter interface /__metrics reads.
func TestDroppedCounter(t *testing.T) {
	dir := t.TempDir()
	s := newTestSink(t, dir, 1<<20, time.Now())
	s.Emit(rec(1))
	if got := s.Dropped(); got != 0 {
		t.Fatalf("healthy emit: dropped = %d, want 0", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.Emit(rec(2))
	if got := s.Dropped(); got != 1 {
		t.Fatalf("emit after close: dropped = %d, want 1", got)
	}

	// Force repeated rolling with a 1-byte cap, then remove the directory so
	// the next required roll fails to open its segment.
	dir2 := t.TempDir()
	s2, err := newRotationFileSink(FileOptions{Path: filepath.Join(dir2, "access.log"), MaxBytes: 1, RetainDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	var _ DroppedReporter = s2
	s2.Emit(rec(3)) // rolls and succeeds while the directory exists
	if err := os.RemoveAll(dir2); err != nil {
		t.Fatal(err)
	}
	s2.Emit(rec(4))
	if got := s2.Dropped(); got != 1 {
		t.Fatalf("rotation failure: dropped = %d, want 1", got)
	}
}

// TestSegmentPathsAndReadBackward: segments are returned newest-first and
// records are read newest-line-first with limit and filter respected;
// malformed lines are skipped.
func TestSegmentPathsAndReadBackward(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "access.log")
	day := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	s, err := newRotationFileSink(FileOptions{Path: base, MaxBytes: 1 << 20, RetainDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	s.nowFn = func() time.Time { return day }
	// NewFile opened a segment for the real current day before the clock was
	// mocked; remove that stray (empty) segment so the assertions below stay
	// independent of the wall-clock date the test happens to run on.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !strings.Contains(e.Name(), day.Format("20060102")) {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	for i := 1; i <= 6; i++ {
		s.Emit(Record{TS: day, RequestID: fmt.Sprintf("req-%d", i), Caller: "svc-a", Route: "demo-alpha", Method: "GET", Path: "/v1/ping", Decision: "allow"})
	}
	// force a second segment with a deny + a malformed line
	s.mu.Lock()
	if err := s.openSeq(day.Format("20060102"), 2, 0); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	s.Emit(Record{TS: day, RequestID: "req-deny", Caller: "svc-a", Route: "demo-alpha", Method: "GET", Path: "/admin/secret", Decision: "deny"})
	f2 := s.fileName(day.Format("20060102"), 2)
	f, _ := os.OpenFile(f2, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("not-json-line\n")
	f.Close()
	s.Close()

	paths, err := SegmentPaths(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "0002.log") {
		t.Fatalf("SegmentPaths order wrong: %v", paths)
	}

	// newest first, limit 2: expect req-deny then req-6
	recs, err := ReadRecordsBackward(paths, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].RequestID != "req-deny" || recs[1].RequestID != "req-6" {
		t.Fatalf("backward read wrong: %+v", recs)
	}

	// filter decision=deny finds only the deny record
	recs, err = ReadRecordsBackward(paths, 0, func(r Record) bool { return r.Decision == "deny" })
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Decision != "deny" {
		t.Fatalf("filter wrong: %+v", recs)
	}
}
