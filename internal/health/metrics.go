// /__metrics renders the Prometheus text exposition format (v0.0.4) for the
// operational state this process already keeps in memory: config snapshot
// freshness, sync readiness, alert/debug sink drop counters, and a few Go
// runtime gauges. It lives on the plaintext health listener next to
// /__status and exposes only counters and status values — no request data,
// no credentials, and no DB I/O on scrape (freshness is covered by the sync
// timestamps, so a scrape can never stall on the database).
package health

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/alerts"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
	"github.com/BadmintonCode/routegate-gateway/internal/syncworker"
)

// MetricsDeps carries the runtime state sources /__metrics reads. Every field
// is optional (nil = the group renders zeros or is skipped) so management-only
// instances and tests render a meaningful subset.
type MetricsDeps struct {
	Version    string
	InstanceID string
	Start      time.Time

	Holder     *snapshot.Holder
	Ready      *syncworker.Ready
	StaleAfter time.Duration

	AlertRules    *store.AlertRulesHolder
	AlertChannels *store.AlertChannelsHolder
	Alerts        *alerts.Engine

	Debug     *accesslog.DebugManager
	AccessLog accesslog.DroppedReporter // optional: only the file sink drops
}

const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

func serveMetrics(w http.ResponseWriter, _ *http.Request, d *MetricsDeps) {
	w.Header().Set("Content-Type", metricsContentType)
	d.render(w)
}

// render writes the exposition body. HELP/TYPE pairs are emitted per group
// exactly once; samples follow the `name{labels} value` layout.
func (d *MetricsDeps) render(w io.Writer) {
	gauge := func(name, help string, value float64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", name, help, name, name, formatValue(value))
	}
	counter := func(name, help string, value float64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %s\n", name, help, name, name, formatValue(value))
	}

	// Build info and process basics.
	version, instance := d.Version, d.InstanceID
	fmt.Fprintf(w, "# HELP routegate_build_info Build metadata.\n# TYPE routegate_build_info gauge\nroutegate_build_info{version=\"%s\",instance=\"%s\"} 1\n",
		escapeLabel(version), escapeLabel(instance))
	if !d.Start.IsZero() {
		gauge("routegate_process_uptime_seconds", "Seconds since process start.", time.Since(d.Start).Seconds())
	}
	gauge("go_goroutines", "Number of goroutines that currently exist.", float64(runtime.NumGoroutine()))
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	gauge("go_memstats_heap_alloc_bytes", "Number of heap bytes allocated and still in use.", float64(ms.HeapAlloc))
	gauge("go_memstats_sys_bytes", "Number of bytes obtained from system.", float64(ms.Sys))

	// Config snapshot (nil until the first successful sync; the whole group
	// is skipped when no holder is wired).
	if d.Holder != nil {
		if snap := d.Holder.Load(); snap != nil {
			gauge("routegate_config_loaded", "1 when a configuration snapshot is loaded.", 1)
			gauge("routegate_config_revision", "Configuration revision currently served.", float64(snap.Revision))
			gauge("routegate_config_routes", "Routes in the loaded snapshot.", float64(len(snap.Routes)))
			gauge("routegate_config_identities", "Workload identities in the loaded snapshot.", float64(len(snap.Identities)))
			gauge("routegate_config_loaded_at_timestamp", "Unix time of the last snapshot load.", unixSeconds(snap.LoadedAt))
		} else {
			gauge("routegate_config_loaded", "1 when a configuration snapshot is loaded.", 0)
			gauge("routegate_config_revision", "Configuration revision currently served.", 0)
		}
	}

	// Sync worker readiness.
	if d.Ready != nil {
		gauge("routegate_sync_ready", "1 when the last sync is within the stale window.", b2f(d.Ready.OK(time.Now(), d.StaleAfter)))
		gauge("routegate_sync_last_success_timestamp", "Unix time of the last successful sync tick.", unixSeconds(d.Ready.LastOK()))
	}

	// Alert rules/channels and dispatch-queue drops.
	if d.AlertRules != nil {
		total, enabled := 0, 0
		for _, r := range d.AlertRules.Get() {
			total++
			if r.Enabled {
				enabled++
			}
		}
		gauge("routegate_alert_rules", "Alert rules loaded.", float64(total))
		gauge("routegate_alert_rules_enabled", "Alert rules loaded and enabled.", float64(enabled))
	}
	if d.AlertChannels != nil {
		total, enabled := 0, 0
		for _, c := range d.AlertChannels.Get() {
			total++
			if c.Enabled {
				enabled++
			}
		}
		gauge("routegate_alert_channels", "Alert channels loaded.", float64(total))
		gauge("routegate_alert_channels_enabled", "Alert channels loaded and enabled.", float64(enabled))
	}
	if d.Alerts != nil {
		counter("routegate_alert_queue_dropped_total", "Alert firings dropped because the dispatch queue was full.", float64(d.Alerts.Dropped()))
	}

	// Debug capture state and sink drops.
	if d.Debug != nil {
		gauge("routegate_debug_enabled", "1 when debug capture is active.", b2f(d.Debug.Settings() != nil))
		counter("routegate_debug_dropped_total", "Debug records dropped because the sink queue was full.", float64(d.Debug.Dropped()))
	}
	if d.AccessLog != nil {
		counter("routegate_accesslog_dropped_total", "Access-log records dropped by the sink (rotation or write failures).", float64(d.AccessLog.Dropped()))
	}
}

func formatValue(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.Unix())
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// escapeLabel escapes a label value per the text exposition format.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, `"\n`) {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}
