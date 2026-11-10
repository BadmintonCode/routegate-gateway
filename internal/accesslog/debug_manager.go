package accesslog

import (
	"sync"
	"sync/atomic"

	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

// DebugManager owns the debug-logging runtime state: the settings the data
// plane reads per request (atomic pointer; nil = disabled) and the sink
// lifecycle. The sync worker calls Apply on every tick with the freshly loaded
// settings; unchanged settings are a no-op.
type DebugManager struct {
	cur  atomic.Pointer[store.DebugSettings] // nil = debug disabled
	sink atomic.Pointer[DebugSink]
	mu   sync.Mutex // serializes Apply/Close
}

// NewDebugManager starts disabled. It is always constructed — with debug
// disabled it costs two atomic loads per request and nothing else.
func NewDebugManager() *DebugManager {
	return &DebugManager{}
}

// Settings returns the active debug settings, or nil when disabled.
func (m *DebugManager) Settings() *store.DebugSettings { return m.cur.Load() }

// EmitDebug forwards one record to the active sink, if any. Non-blocking by
// the sink's contract.
func (m *DebugManager) EmitDebug(rec DebugRecord) {
	if s := m.sink.Load(); s != nil {
		s.EmitDebug(rec)
	}
}

// Dropped reports the dropped-record count of the active sink.
func (m *DebugManager) Dropped() uint64 {
	if s := m.sink.Load(); s != nil {
		return s.Dropped()
	}
	return 0
}

// Apply transitions the runtime to the given settings:
//
//   - disabled → stop the sink, hot path sees nil;
//   - invalid (enabled without a path, non-positive caps) → stop the sink and
//     return the validation error for the caller to log;
//   - enabled with unchanged settings → no-op;
//   - enabled with changed settings → close the old sink and open a new one
//     (rotation-safe; records in flight are lost only if Close raced, which is
//     acceptable for a troubleshooting sink).
//
// An open failure disables debug capture and returns the error for the caller
// to log; the next Apply (next tick with changed settings) retries.
func (m *DebugManager) Apply(s store.DebugSettings) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !s.Enabled {
		m.stopLocked()
		return nil
	}
	if err := s.Validate(); err != nil {
		m.stopLocked()
		return err
	}
	if cur := m.cur.Load(); cur != nil && *cur == s {
		return nil // unchanged
	}
	sink, err := NewDebugFile(DebugFileOptions{
		FileOptions: FileOptions{
			Path:       s.File,
			MaxBytes:   s.MaxMB << 20,
			RetainDays: s.RetainDays,
		},
		QueueCapacity: s.QueueCapacity,
	})
	if err != nil {
		m.stopLocked()
		return err
	}
	// Swap before starting writes: EmitDebug reads the sink pointer, and the
	// settings pointer gates the capture work in ServeHTTP.
	old := m.sink.Swap(sink)
	m.cur.Store(&s)
	if old != nil {
		// Close drains the queue, so it must not run on the sync tick's
		// goroutine: the previous sink is retired asynchronously. In-flight
		// EmitDebug sends either land before the drain or drop via the
		// sink's closed branch (see DebugSink.Close).
		go old.Close()
	}
	return nil
}

func (m *DebugManager) stopLocked() {
	if old := m.sink.Swap(nil); old != nil {
		_ = old.Close() // drains the queue
	}
	m.cur.Store(nil)
}

// Close stops the sink (process shutdown).
func (m *DebugManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.sink.Swap(nil); old != nil {
		return old.Close()
	}
	return nil
}
