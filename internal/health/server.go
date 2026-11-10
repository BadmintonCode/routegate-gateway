// Package health serves the plaintext health listener (ADR-0001 §2.2, §10):
// GET /__health (liveness), GET /__ready (readiness), GET /__status (JSON
// diagnostics), GET /__metrics (Prometheus exposition). It exists because the
// data plane requires client certificates at the TLS layer, which LBs/Docker/
// K8s probes cannot present; the data plane itself carries no health endpoints.
package health

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/config"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/syncworker"
)

// Options carries what /__status reports and the /__metrics state sources.
type Options struct {
	Version    string
	ClientAuth string
	// Metrics enables GET /__metrics when non-nil (nil keeps the endpoint
	// unregistered, e.g. in tests that build a bare handler).
	Metrics *MetricsDeps
	// Drain flips /__ready to 503 when the shutdown sequence begins (nil =
	// never draining, e.g. bare test handlers).
	Drain *Drain
}

// Handler builds the health listener mux.
func Handler(cfg *config.Config, db *sql.DB, holder *snapshot.Holder, ready *syncworker.Ready, opts Options) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /__health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "ok"})
	})

	mux.HandleFunc("GET /__ready", func(w http.ResponseWriter, r *http.Request) {
		// Draining beats everything: answer 503 without the DB ping so LBs
		// remove the instance while in-flight requests finish.
		if opts.Drain != nil && opts.Drain.Active() {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			// Probe bodies stay code-first JSON (contract exemption is
			// documented in the ADR/docs task); no-store matches the admin
			// error contract so intermediaries never cache a stale state.
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "draining"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		dbOK := db.PingContext(ctx) == nil
		syncOK := ready.OK(time.Now(), cfg.ReadyStaleAfter)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if dbOK && syncOK {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "ok", "db": true, "sync": true})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "not_ready", "db": dbOK, "sync": syncOK})
	})

	if opts.Metrics != nil {
		mux.HandleFunc("GET /__metrics", func(w http.ResponseWriter, r *http.Request) {
			serveMetrics(w, r, opts.Metrics)
		})
	}

	mux.HandleFunc("GET /__status", func(w http.ResponseWriter, r *http.Request) {
		snap := holder.Load()
		body := map[string]any{
			"version":       opts.Version,
			"instance_id":   cfg.InstanceID,
			"client_auth":   opts.ClientAuth,
			"sync_interval": cfg.SyncInterval.String(),
		}
		if snap != nil {
			body["config_revision"] = snap.Revision
			body["routes"] = len(snap.Routes)
			body["identities"] = len(snap.Identities)
			body["loaded_at"] = snap.LoadedAt.UTC().Format(time.RFC3339)
		} else {
			body["config_revision"] = 0
			body["loaded"] = false
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// /__status reports instance internals — never cache it.
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(body)
	})

	return mux
}
