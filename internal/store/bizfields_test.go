package store

import (
	"context"
	"database/sql"
	"testing"
)

// TestLoadBizFieldsSyncFailStatic: the sync loader must distinguish "row
// absent / row broken" (empty fallback, no error) from "transient read
// failure" (empty fallback AND error) so the sync worker can keep the
// previous global envelope fallback instead of wiping it for a tick.
func TestLoadBizFieldsSyncFailStatic(t *testing.T) {
	ctx := context.Background()

	// Missing row: empty fallback, no error.
	db := sql.OpenDB(fakeSettingsConnector{noRows: true})
	defer db.Close()
	f, err := LoadBizFieldsSync(ctx, db)
	if err != nil {
		t.Fatalf("missing row returned an error: %v", err)
	}
	if f != (BizFields{}) {
		t.Errorf("missing row yielded %+v, want the empty fallback", f)
	}

	// Injected transient error: empty fallback AND the error.
	dbFail := sql.OpenDB(fakeSettingsConnector{fail: true})
	defer dbFail.Close()
	if f, err := LoadBizFieldsSync(ctx, dbFail); err == nil {
		t.Error("transient read error must be propagated, got nil")
	} else if f != (BizFields{}) {
		t.Errorf("error path yielded %+v, want the empty fallback", f)
	}

	// Unparsable row: empty fallback, no error.
	dbGarbage := sql.OpenDB(fakeSettingsConnector{value: `not json at all`})
	defer dbGarbage.Close()
	f, err = LoadBizFieldsSync(ctx, dbGarbage)
	if err != nil {
		t.Fatalf("unparsable row returned an error: %v", err)
	}
	if f != (BizFields{}) {
		t.Errorf("unparsable row yielded %+v, want the empty fallback", f)
	}

	// Valid row: parsed and sanitized (an invalid path is dropped).
	dbValid := sql.OpenDB(fakeSettingsConnector{value: `{"code_path":"code","message_path":" message "}`})
	defer dbValid.Close()
	f, err = LoadBizFieldsSync(ctx, dbValid)
	if err != nil {
		t.Fatalf("valid row returned an error: %v", err)
	}
	if f.CodePath != "code" || f.MessagePath != "message" {
		t.Errorf("valid row not parsed/sanitized: %+v", f)
	}
}
