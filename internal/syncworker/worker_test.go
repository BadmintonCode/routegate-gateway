package syncworker

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/config"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

// fakeSettingsConnector backs a *sql.DB that answers the settings SELECT with
// one canned row (or an injected failure), so the apply* helpers can be tested
// without a database.
type fakeSettingsConnector struct {
	fail     bool
	noRows   bool
	value    string
	queryLog *[]string // when non-nil, records every QueryContext query (single-goroutine use)
}

func (c fakeSettingsConnector) Connect(context.Context) (driver.Conn, error) {
	return fakeSettingsConn{c}, nil
}
func (c fakeSettingsConnector) Driver() driver.Driver { return fakeSettingsDriver{} }

type fakeSettingsDriver struct{}

func (fakeSettingsDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use sql.OpenDB")
}

type fakeSettingsConn struct{ cfg fakeSettingsConnector }

func (c fakeSettingsConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements unsupported by the fake")
}
func (c fakeSettingsConn) Close() error { return nil }
func (c fakeSettingsConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions unsupported by the fake")
}

func (c fakeSettingsConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.cfg.queryLog != nil {
		*c.cfg.queryLog = append(*c.cfg.queryLog, query)
	}
	if c.cfg.fail {
		return nil, errors.New("db unavailable (fake)")
	}
	if c.cfg.noRows {
		return &fakeSettingsRows{}, nil
	}
	return &fakeSettingsRows{cols: []string{"value"}, rows: [][]driver.Value{{c.cfg.value}}}, nil
}

type fakeSettingsRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeSettingsRows) Columns() []string { return r.cols }
func (r *fakeSettingsRows) Close() error      { return nil }
func (r *fakeSettingsRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

// TestReadyConcurrentAccess: the sync goroutine writes Ready while
// health-listener HTTP goroutines read it. Under -race, any unsynchronized
// field access fails this test. The writer exercises exactly the writes the
// tick path performs (markSuccess / touchOK / markFailure).
func TestReadyConcurrentAccess(t *testing.T) {
	var r Ready
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				if r.OK(time.Now(), time.Minute) {
					_ = r.LastOK()
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 1000; j++ {
			r.markSuccess(int64(j))
			r.touchOK()
			r.markFailure(errors.New("transient (test)"))
			_ = r.loadedRevision()
			_ = r.lastError()
		}
	}()
	wg.Wait()
}

// TestReadyTransitions covers the read accessors' semantics that /__ready and
// /__metrics rely on.
func TestReadyTransitions(t *testing.T) {
	var r Ready
	if r.OK(time.Now(), time.Minute) {
		t.Error("zero Ready must not be ready")
	}
	if !r.LastOK().IsZero() {
		t.Error("zero Ready must report a zero LastOK")
	}
	r.markSuccess(42)
	if got := r.loadedRevision(); got != 42 {
		t.Errorf("loadedRevision = %d, want 42", got)
	}
	if got := r.lastError(); got != "" {
		t.Errorf("lastError = %q, want empty after success", got)
	}
	if !r.OK(time.Now(), time.Minute) {
		t.Error("fresh successful tick must be ready")
	}
	if first := r.markFailure(errors.New("boom")); !first {
		t.Error("first failure after success must be reported as a transition")
	}
	if got := r.lastError(); got != "boom" {
		t.Errorf("lastError = %q, want boom", got)
	}
	if first := r.markFailure(errors.New("boom again")); first {
		t.Error("repeat failure must not be reported as a transition")
	}
}

func newTestWorker(db *sql.DB) (*Worker, *store.BizFieldsHolder, *accesslog.DebugManager) {
	biz := store.NewBizFieldsHolder()
	dbg := accesslog.NewDebugManager()
	w := New(&config.Config{}, db, &snapshot.Holder{}, "inst", "test", dbg, biz,
		store.NewAlertChannelsHolder(), store.NewAlertRulesHolder())
	return w, biz, dbg
}

// TestApplyBizFieldsKeepsPreviousOnError: a transient load failure must leave
// the holder untouched (fail-static) instead of wiping the global fallback.
func TestApplyBizFieldsKeepsPreviousOnError(t *testing.T) {
	db := sql.OpenDB(fakeSettingsConnector{fail: true})
	defer db.Close()
	w, biz, _ := newTestWorker(db)
	biz.Set(store.BizFields{CodePath: "keep.code", MessagePath: "keep.msg"})
	w.applyBizFields(t.Context())
	if got := biz.Get(); got.CodePath != "keep.code" || got.MessagePath != "keep.msg" {
		t.Errorf("failed tick wiped the holder: %+v", got)
	}
}

// TestApplyBizFieldsAppliesRow: a successful read still updates the holder.
func TestApplyBizFieldsAppliesRow(t *testing.T) {
	db := sql.OpenDB(fakeSettingsConnector{value: `{"code_path":"code","message_path":"message"}`})
	defer db.Close()
	w, biz, _ := newTestWorker(db)
	w.applyBizFields(t.Context())
	if got := biz.Get(); got.CodePath != "code" || got.MessagePath != "message" {
		t.Errorf("holder = %+v, want code/message", got)
	}
}

// TestApplyDebugSettingsKeepsPreviousOnError: a transient load failure must
// not disable an enabled debug capture (fail-static).
func TestApplyDebugSettingsKeepsPreviousOnError(t *testing.T) {
	db := sql.OpenDB(fakeSettingsConnector{fail: true})
	defer db.Close()
	w, _, dbg := newTestWorker(db)
	enabled := store.DebugSettings{
		Enabled: true, File: filepath.Join(t.TempDir(), "debug.log"),
		MaxMB: 1, RetainDays: 1, RequestBodyMaxBytes: 64, ResponseBodyMaxBytes: 64,
		HeadersMaxBytes: 64, QueueCapacity: 8,
	}
	if err := dbg.Apply(enabled); err != nil {
		t.Fatal(err)
	}
	w.applyDebugSettings(t.Context())
	if got := dbg.Settings(); got == nil || got.File != enabled.File {
		t.Errorf("failed tick changed debug settings: %+v", got)
	}
	if err := dbg.Apply(store.DefaultDebugSettings()); err != nil {
		t.Fatal(err)
	}
}

// TestApplyDebugSettingsAppliesRow: a successful read still reaches the debug
// manager.
func TestApplyDebugSettingsAppliesRow(t *testing.T) {
	db := sql.OpenDB(fakeSettingsConnector{value: `{"enabled":true,"file":"` + filepath.Join(t.TempDir(), "debug.log") + `","max_mb":1,"retain_days":1,"request_body_max_bytes":64,"response_body_max_bytes":64,"headers_max_bytes":64,"queue_capacity":8}`})
	defer db.Close()
	w, _, dbg := newTestWorker(db)
	w.applyDebugSettings(t.Context())
	if got := dbg.Settings(); got == nil || !got.Enabled {
		t.Errorf("enabled row did not reach the manager: %+v", got)
	}
	if err := dbg.Apply(store.DefaultDebugSettings()); err != nil {
		t.Fatal(err)
	}
}

// TestShouldApplySideLoads pins the per-tick gating decision for the side
// settings (debug log, biz fields, alert channels/rules): an unchanged
// revision skips the loads, a changed revision or a forced tick applies them,
// and the first tick always applies (lastAppliedRev starts at -1).
func TestShouldApplySideLoads(t *testing.T) {
	cases := []struct {
		name             string
		rev, lastApplied int64
		forced           bool
		want             bool
	}{
		{"first tick always applies", 7, -1, false, true},
		{"first tick with revision 0 applies", 0, -1, false, true},
		{"unchanged revision skips", 7, 7, false, false},
		{"revision 0 unchanged skips", 0, 0, false, false},
		{"changed revision applies", 8, 7, false, true},
		{"forced tick applies despite unchanged revision", 7, 7, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldApplySideLoads(tc.rev, tc.lastApplied, tc.forced); got != tc.want {
				t.Errorf("shouldApplySideLoads(%d, %d, %v) = %v, want %v", tc.rev, tc.lastApplied, tc.forced, got, tc.want)
			}
		})
	}
}

// sideLoadQuery reports whether a query belongs to one of the four side
// settings loads (debug/biz read settings by name; the alert loaders read
// their own tables). The revision probe and the snapshot load never match.
func sideLoadQuery(query string) bool {
	return strings.Contains(query, "FROM settings WHERE name = ?") ||
		strings.Contains(query, "FROM alert_channels") ||
		strings.Contains(query, "FROM alert_rules")
}

// TestTickSkipsSideLoadsOnUnchangedRevision exercises the worker wiring: a
// boot tick applies the four side loads, the next tick with an unchanged
// revision and no force reload must not issue a single side-load query, and a
// forced tick applies them again. The snapshot load always fails against the
// one-column fake (keeping loadedRevision at 0), which is irrelevant to the
// side-load gate: it compares against the revision the loads were applied
// for, not the served snapshot's revision.
func TestTickSkipsSideLoadsOnUnchangedRevision(t *testing.T) {
	var log []string
	conn := fakeSettingsConnector{value: "7", queryLog: &log}
	db := sql.OpenDB(conn)
	defer db.Close()
	w := New(&config.Config{ForceReloadEvery: 2}, db, &snapshot.Holder{}, "inst", "test",
		accesslog.NewDebugManager(), store.NewBizFieldsHolder(),
		store.NewAlertChannelsHolder(), store.NewAlertRulesHolder())
	ctx := t.Context()

	sideLoadsSince := func(mark int) int {
		n := 0
		for _, q := range log[mark:] {
			if sideLoadQuery(q) {
				n++
			}
		}
		return n
	}

	w.tick(ctx, true) // boot: revision 7 becomes the applied revision
	if w.sideLoadsRev != 7 {
		t.Fatalf("boot tick sideLoadsRev = %d, want 7", w.sideLoadsRev)
	}
	if n := sideLoadsSince(0); n != 4 {
		t.Fatalf("boot tick issued %d side-load queries, want 4", n)
	}

	mark := len(log)
	w.tickN++ // steady state: same revision, not a forced tick
	w.tick(ctx, false)
	if n := sideLoadsSince(mark); n != 0 {
		t.Fatalf("steady tick issued %d side-load queries, want 0 (gating skipped them)", n)
	}
	if w.sideLoadsRev != 7 {
		t.Fatalf("steady tick sideLoadsRev = %d, want unchanged 7", w.sideLoadsRev)
	}

	mark = len(log)
	w.tickN++ // 2 % 2 == 0 with ForceReloadEvery 2: forced tick re-applies
	w.tick(ctx, false)
	if n := sideLoadsSince(mark); n != 4 {
		t.Fatalf("forced tick issued %d side-load queries, want 4", n)
	}
	if w.sideLoadsRev != 7 {
		t.Fatalf("forced tick sideLoadsRev = %d, want 7", w.sideLoadsRev)
	}
}
