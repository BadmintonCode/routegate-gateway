package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/alerts"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
	"github.com/BadmintonCode/routegate-gateway/internal/syncworker"
)

type fakeDropped struct{ n uint64 }

func (f fakeDropped) Dropped() uint64 { return f.n }

// TestMetricsEndpointGating: the endpoint exists only when Metrics deps are
// provided, and answers with the Prometheus content type.
func TestMetricsEndpointGating(t *testing.T) {
	with := Handler(nil, nil, &snapshot.Holder{}, &syncworker.Ready{}, Options{Metrics: &MetricsDeps{}})
	rec := httptest.NewRecorder()
	with.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /__metrics with deps = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != metricsContentType {
		t.Fatalf("content type = %q, want %q", ct, metricsContentType)
	}

	without := Handler(nil, nil, &snapshot.Holder{}, &syncworker.Ready{}, Options{})
	rec = httptest.NewRecorder()
	without.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /__metrics without deps = %d, want 404", rec.Code)
	}
}

// TestMetricsRenderEmpty: zero deps must render without panicking, report an
// unloaded config, and skip the groups whose sources are absent.
func TestMetricsRenderEmpty(t *testing.T) {
	d := &MetricsDeps{Version: "test", InstanceID: "inst"}
	out := renderToString(t, d)
	for _, want := range []string{"routegate_build_info", "go_goroutines"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{
		"routegate_sync_ready", "routegate_alert_rules", "routegate_accesslog_dropped_total",
		"routegate_config_loaded", "routegate_process_uptime_seconds", "routegate_debug_enabled",
	} {
		if strings.Contains(out, absent) {
			t.Errorf("absent source %q must not render:\n%s", absent, out)
		}
	}
}

// TestMetricsRenderLoaded: populated sources produce the expected gauges.
func TestMetricsRenderLoaded(t *testing.T) {
	holder := &snapshot.Holder{}
	holder.Store(&snapshot.Snapshot{
		Revision:   7,
		LoadedAt:   time.Unix(1700000000, 0),
		Routes:     []*snapshot.Route{{}, {}},
		Identities: map[string]*snapshot.Identity{"a": {}, "b": {}, "c": {}},
	})
	rules := store.NewAlertRulesHolder()
	rules.Set([]store.AlertRule{{ID: 1, Enabled: true}, {ID: 2}})
	channels := store.NewAlertChannelsHolder()
	channels.Set([]store.AlertChannel{{ID: 1}})

	engine := alerts.NewEngine(nil, nil, nil, "inst")
	defer engine.Close()

	d := &MetricsDeps{
		Version: "test", InstanceID: "inst",
		Holder: holder, Ready: &syncworker.Ready{}, StaleAfter: time.Minute,
		AlertRules: rules, AlertChannels: channels, Alerts: engine,
		Debug:     accesslog.NewDebugManager(),
		AccessLog: fakeDropped{n: 42},
	}
	out := renderToString(t, d)
	for _, want := range []string{
		`routegate_config_loaded 1`,
		`routegate_config_revision 7`,
		`routegate_config_routes 2`,
		`routegate_config_identities 3`,
		`routegate_sync_ready 0`, // Ready never ticked
		`routegate_sync_last_success_timestamp 0`,
		`routegate_alert_rules 2`,
		`routegate_alert_rules_enabled 1`,
		`routegate_alert_channels 1`,
		`routegate_alert_channels_enabled 0`,
		`routegate_alert_queue_dropped_total 0`,
		`routegate_debug_enabled 0`,
		`routegate_debug_dropped_total 0`,
		`routegate_accesslog_dropped_total 42`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestMetricsLabelEscaping: label values with quotes, backslashes, and
// newlines are escaped per the exposition format.
func TestMetricsLabelEscaping(t *testing.T) {
	d := &MetricsDeps{Version: "1.0 \"x\"\n\\z", InstanceID: "inst"}
	out := renderToString(t, d)
	if !strings.Contains(out, `version="1.0 \"x\"\n\\z"`) {
		t.Errorf("label value not escaped:\n%s", out)
	}
}

func renderToString(t *testing.T, d *MetricsDeps) string {
	t.Helper()
	rec := httptest.NewRecorder()
	serveMetrics(rec, httptest.NewRequest(http.MethodGet, "/__metrics", nil), d)
	return rec.Body.String()
}
