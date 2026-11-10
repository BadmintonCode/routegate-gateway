package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

// fakeSettingsConnector backs a *sql.DB that answers the settings SELECT with
// one canned row (or an injected failure), so the settings loaders can be
// tested without a database. It satisfies the querier interface via *sql.DB.
type fakeSettingsConnector struct {
	fail   bool
	noRows bool
	value  string
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

func TestDefaultDebugSettings(t *testing.T) {
	d := DefaultDebugSettings()
	if d.Enabled || d.File != "" {
		t.Errorf("debug must default to disabled without a file: %+v", d)
	}
	if d.MaxMB != 2 || d.RetainDays != 7 || d.RequestBodyMaxBytes != 64*1024 ||
		d.ResponseBodyMaxBytes != 64*1024 || d.HeadersMaxBytes != 32*1024 || d.QueueCapacity != 1024 {
		t.Errorf("unexpected defaults: %+v", d)
	}
}

func TestDebugSettingsValidate(t *testing.T) {
	valid := DefaultDebugSettings()
	valid.Enabled = true
	valid.File = "/var/log/routegate/debug.log"
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	noFile := valid
	noFile.File = ""
	if err := noFile.Validate(); err == nil {
		t.Fatal("enabled without file must be rejected")
	}
	disabled := valid
	disabled.Enabled = false
	disabled.File = ""
	if err := disabled.Validate(); err != nil {
		t.Fatalf("disabled settings need no file: %v", err)
	}
	badCap := valid
	badCap.QueueCapacity = 0
	if err := badCap.Validate(); err == nil {
		t.Fatal("zero cap must be rejected")
	}
}

// TestSanitizeDebugSettings: a hand-edited or partially-broken settings row is
// clamped to safe defaults instead of producing a zero-cap or runaway sink.
func TestSanitizeDebugSettings(t *testing.T) {
	s := DebugSettings{Enabled: true, File: "/tmp/x.log", MaxMB: 0, RetainDays: -3,
		RequestBodyMaxBytes: 0, ResponseBodyMaxBytes: -1, HeadersMaxBytes: 0, QueueCapacity: 0}
	got := sanitizeDebugSettings(s)
	d := DefaultDebugSettings()
	if got.MaxMB != d.MaxMB || got.RetainDays != d.RetainDays ||
		got.RequestBodyMaxBytes != d.RequestBodyMaxBytes || got.ResponseBodyMaxBytes != d.ResponseBodyMaxBytes ||
		got.HeadersMaxBytes != d.HeadersMaxBytes || got.QueueCapacity != d.QueueCapacity {
		t.Errorf("invalid caps not clamped to defaults: %+v", got)
	}
	enabledNoPath := DebugSettings{Enabled: true, File: ""}
	if got := sanitizeDebugSettings(enabledNoPath); got.Enabled {
		t.Error("enabled without a path must be treated as disabled")
	}
}

func TestDebugSettingsJSONRoundTrip(t *testing.T) {
	s := DefaultDebugSettings()
	s.Enabled = true
	s.File = "/var/log/routegate/debug.log"
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back DebugSettings
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != s {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", back, s)
	}
}

// TestLoadDebugSettingsSyncFailStatic: the sync loader must distinguish "row
// absent / row broken" (defaults, no error) from "transient read failure"
// (defaults AND error) so the sync worker can keep the previous runtime state
// instead of silently disabling debug capture.
func TestLoadDebugSettingsSyncFailStatic(t *testing.T) {
	ctx := context.Background()

	// Missing row: defaults, no error.
	db := sql.OpenDB(fakeSettingsConnector{noRows: true})
	defer db.Close()
	ds, err := LoadDebugSettingsSync(ctx, db)
	if err != nil {
		t.Fatalf("missing row returned an error: %v", err)
	}
	if ds != DefaultDebugSettings() {
		t.Errorf("missing row yielded %+v, want defaults", ds)
	}

	// Injected transient error: defaults AND the error.
	dbFail := sql.OpenDB(fakeSettingsConnector{fail: true})
	defer dbFail.Close()
	if ds, err := LoadDebugSettingsSync(ctx, dbFail); err == nil {
		t.Error("transient read error must be propagated, got nil")
	} else if ds != DefaultDebugSettings() {
		t.Errorf("error path yielded %+v, want defaults", ds)
	}

	// Unparsable row: sanitized defaults, no error.
	dbGarbage := sql.OpenDB(fakeSettingsConnector{value: `{not json`})
	defer dbGarbage.Close()
	ds, err = LoadDebugSettingsSync(ctx, dbGarbage)
	if err != nil {
		t.Fatalf("unparsable row returned an error: %v", err)
	}
	if ds != DefaultDebugSettings() {
		t.Errorf("unparsable row yielded %+v, want defaults", ds)
	}

	// Valid row: parsed and sanitized.
	dbValid := sql.OpenDB(fakeSettingsConnector{value: `{"enabled":true,"file":"/tmp/d.log","max_mb":0,"queue_capacity":0}`})
	defer dbValid.Close()
	ds, err = LoadDebugSettingsSync(ctx, dbValid)
	if err != nil {
		t.Fatalf("valid row returned an error: %v", err)
	}
	if !ds.Enabled || ds.File != "/tmp/d.log" || ds.MaxMB != 2 || ds.QueueCapacity != 1024 {
		t.Errorf("valid row not parsed/sanitized: %+v", ds)
	}
}
