package accesslog

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

func testDebugSettings(file string, bodyCap int) store.DebugSettings {
	return store.DebugSettings{
		Enabled: true, File: file, MaxMB: 1, RetainDays: 1,
		RequestBodyMaxBytes: bodyCap, ResponseBodyMaxBytes: 1024,
		HeadersMaxBytes: 1024, QueueCapacity: 8,
	}
}

func waitForRecord(t *testing.T, m *DebugManager, reqID string) DebugRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ds := m.Settings(); ds != nil {
			if paths, err := DebugSegmentPaths(ds.File); err == nil {
				recs, _ := ReadDebugRecordsBackward(paths, 10, func(r DebugRecord) bool { return r.RequestID == reqID })
				if len(recs) == 1 {
					return recs[0]
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("debug record %q never reached the file", reqID)
	return DebugRecord{}
}

// TestDebugManagerLifecycle: apply → capture, disabled → no capture, settings
// change → new caps take effect without losing the manager.
func TestDebugManagerLifecycle(t *testing.T) {
	m := NewDebugManager()
	defer m.Close()
	if m.Settings() != nil {
		t.Fatal("manager must start disabled")
	}
	m.EmitDebug(DebugRecord{RequestID: "x"}) // no sink: must not panic

	base := filepath.Join(t.TempDir(), "debug.log")
	if err := m.Apply(testDebugSettings(base, 1024)); err != nil {
		t.Fatal(err)
	}
	if m.Settings() == nil {
		t.Fatal("settings not applied")
	}
	m.EmitDebug(DebugRecord{RequestID: "on-1"})
	got := waitForRecord(t, m, "on-1")
	if got.RequestID != "on-1" {
		t.Fatal("wrong record")
	}

	// Disable: settings go nil; the sink drains and stops.
	if err := m.Apply(store.DefaultDebugSettings()); err != nil {
		t.Fatal(err)
	}
	if m.Settings() != nil {
		t.Fatal("disable did not clear settings")
	}
	m.EmitDebug(DebugRecord{RequestID: "off-1"}) // must not panic

	// Re-enable with a fresh file.
	base2 := filepath.Join(t.TempDir(), "debug2.log")
	if err := m.Apply(testDebugSettings(base2, 8)); err != nil {
		t.Fatal(err)
	}
	m.EmitDebug(DebugRecord{RequestID: "on-2"})
	got = waitForRecord(t, m, "on-2")
	if got.RequestID != "on-2" {
		t.Fatal("wrong record after re-enable")
	}
}

// TestDebugManagerInvalidApply: enabled without a path is rejected and leaves
// the manager disabled; unchanged settings are a cheap no-op.
func TestDebugManagerInvalidApply(t *testing.T) {
	m := NewDebugManager()
	defer m.Close()
	if err := m.Apply(store.DebugSettings{Enabled: true, File: ""}); err == nil {
		t.Fatal("enabled without file must be rejected")
	}
	if m.Settings() != nil {
		t.Fatal("failed apply must leave the manager disabled")
	}

	base := filepath.Join(t.TempDir(), "debug.log")
	s := testDebugSettings(base, 64)
	if err := m.Apply(s); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(s); err != nil {
		t.Fatalf("unchanged settings must be a no-op, got %v", err)
	}
	if m.Settings() == nil || m.Settings() != m.Settings() {
		t.Fatal("settings pointer should be stable across no-op applies")
	}
}

// TestDebugManagerSettingsChangeRetiresOldSink: applying changed settings with
// a new file must (a) route new records into the new file and (b) stop writing
// to the previous file — the old sink is closed, not leaked (regression for
// the Apply swap that orphaned the previous sink's writer/janitor/handle).
func TestDebugManagerSettingsChangeRetiresOldSink(t *testing.T) {
	m := NewDebugManager()
	defer m.Close()

	fileA := filepath.Join(t.TempDir(), "debug-a.log")
	if err := m.Apply(testDebugSettings(fileA, 1024)); err != nil {
		t.Fatal(err)
	}
	m.EmitDebug(DebugRecord{RequestID: "a-1"})
	if got := waitForRecord(t, m, "a-1"); got.RequestID != "a-1" {
		t.Fatal("record a-1 never reached file A")
	}

	fileB := filepath.Join(t.TempDir(), "debug-b.log")
	if err := m.Apply(testDebugSettings(fileB, 1024)); err != nil {
		t.Fatal(err)
	}
	m.EmitDebug(DebugRecord{RequestID: "b-1"})
	if got := waitForRecord(t, m, "b-1"); got.RequestID != "b-1" {
		t.Fatal("record b-1 never reached file B")
	}

	// Give the retired sink's asynchronous close time to finish, then verify
	// nothing written after the change landed in file A.
	time.Sleep(100 * time.Millisecond)
	pathsA, err := DebugSegmentPaths(fileA)
	if err != nil {
		t.Fatal(err)
	}
	recsA, err := ReadDebugRecordsBackward(pathsA, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recsA {
		if r.RequestID == "b-1" {
			t.Errorf("record b-1 leaked into the retired file A")
		}
	}
	if len(recsA) != 1 || recsA[0].RequestID != "a-1" {
		t.Errorf("file A contents = %v, want only a-1", recsA)
	}
}

// TestDebugManagerDisableRacesEmit: concurrent EmitDebug around Apply(disabled)
// must never panic — the manager swaps the sink away before the old one closes,
// and the sink's closed branch absorbs the racing sends.
func TestDebugManagerDisableRacesEmit(t *testing.T) {
	m := NewDebugManager()
	defer m.Close()
	base := filepath.Join(t.TempDir(), "debug.log")
	if err := m.Apply(testDebugSettings(base, 8)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(50 * time.Millisecond)
		for i := 0; time.Now().Before(deadline); i++ {
			m.EmitDebug(DebugRecord{RequestID: fmt.Sprintf("disable-race-%d", i)})
		}
	}()
	time.Sleep(time.Millisecond)
	if err := m.Apply(store.DefaultDebugSettings()); err != nil {
		t.Fatal(err)
	}
	<-done
}
