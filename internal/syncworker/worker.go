// Package syncworker periodically loads configuration from MySQL and swaps
// the data-plane snapshot (ADR-0001 §3). Behavior summary:
//
//   - tick: read settings.config_revision; full load when it changed, plus a
//     forced reload every N ticks (self-healing for break-glass direct DB
//     edits that forgot to bump the revision, ADR §3.1). The side settings
//     (debug log, biz fields, alert channels/rules) are re-read only when the
//     revision changed or the tick is forced — they change exclusively
//     through revision-bumping admin writes;
//   - side duties per tick: instance heartbeat upsert, stale-heartbeat GC
//     (>10 min, ADR §3.5), expired admin-session GC, retention deletes for
//     admin_audit and alert_events (90 days);
//   - DB unreachable: keep serving the last snapshot (fail-static) while
//     /__ready degrades after ReadyStaleAfter.
package syncworker

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/config"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

// Ready tracks the last successful sync for /__ready. The sync goroutine
// writes it and health-listener HTTP goroutines read it, so every field access
// is guarded by mu.
type Ready struct {
	mu        sync.Mutex
	lastOK    time.Time
	lastErr   string
	loadedRev int64
}

// OK reports whether the last successful sync tick is within the stale window.
func (r *Ready) OK(now time.Time, staleAfter time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.lastOK.IsZero() && now.Sub(r.lastOK) <= staleAfter
}

// LastOK returns the time of the last successful sync tick (zero when no tick
// has succeeded yet); /__metrics reports it as a timestamp gauge.
func (r *Ready) LastOK() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastOK
}

// markSuccess records a successful sync tick that (re)loaded the snapshot; rev
// is the revision now being served.
func (r *Ready) markSuccess(rev int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastOK = time.Now()
	r.lastErr = ""
	r.loadedRev = rev
}

// touchOK records a successful tick that did not reload (revision unchanged).
func (r *Ready) touchOK() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastOK = time.Now()
}

// markFailure records a failed sync tick and reports whether this was the
// first failure after a success (the caller logs only that transition).
func (r *Ready) markFailure(err error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	first := r.lastErr == ""
	r.lastErr = err.Error()
	return first
}

// loadedRevision returns the revision currently served (heartbeat upsert).
func (r *Ready) loadedRevision() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadedRev
}

// lastError returns the last sync failure message ("" when healthy).
func (r *Ready) lastError() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

// Worker runs the sync loop.
type Worker struct {
	cfg        *config.Config
	db         *sql.DB
	holder     *snapshot.Holder
	debug      *accesslog.DebugManager // panel-managed debug logging, applied every tick
	instanceID string
	version    string
	tickN      uint64

	bizFields  *store.BizFieldsHolder     // global business-envelope fallback, applied every tick
	alertCh    *store.AlertChannelsHolder // outbound alert channels, applied every tick
	alertRules *store.AlertRulesHolder    // alert rules, applied every tick

	// sideLoadsRev is the config revision the side settings (debug log, biz
	// fields, alert channels/rules) were last applied for; -1 until the first
	// applied tick. Those rows only change through revision-bumping admin
	// writes, so ticks whose revision probe reports unchanged skip the loads.
	sideLoadsRev int64

	// Ready is read by the health listener.
	Ready Ready
}

// New builds a worker. debug, bizFields and alertCh must be non-nil; the
// worker is their only writer.
func New(cfg *config.Config, db *sql.DB, holder *snapshot.Holder, instanceID, version string, debug *accesslog.DebugManager, bizFields *store.BizFieldsHolder, alertCh *store.AlertChannelsHolder, alertRules *store.AlertRulesHolder) *Worker {
	return &Worker{cfg: cfg, db: db, holder: holder, debug: debug, bizFields: bizFields, alertCh: alertCh, alertRules: alertRules, instanceID: instanceID, version: version, sideLoadsRev: -1}
}

// Run blocks until ctx is cancelled: boot (connect + migrate + first load),
// then the tick loop.
func (w *Worker) Run(ctx context.Context) error {
	// Boot: wait for the database, bounded by BootMaxWait when non-zero.
	deadline := time.Time{}
	if w.cfg.BootMaxWait > 0 {
		deadline = time.Now().Add(w.cfg.BootMaxWait)
	}
	for {
		err := w.db.PingContext(ctx)
		if err == nil {
			break
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}

	// Migrations are applied by the caller (cmd serve) before the worker
	// starts, so the admin-plane bootstrap can rely on the schema too.

	ticker := time.NewTicker(w.cfg.SyncInterval)
	defer ticker.Stop()

	w.tick(ctx, true)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.tickN++
			w.tick(ctx, false)
		}
	}
}

func (w *Worker) tick(ctx context.Context, boot bool) {
	rev, err := store.CurrentRevision(ctx, w.db)
	if err != nil {
		w.markSyncFailure(err)
		w.heartbeat(ctx, false, err)
		return
	}
	forced := w.cfg.ForceReloadEvery > 0 && w.tickN%uint64(w.cfg.ForceReloadEvery) == 0
	if boot || forced || rev != w.Ready.loadedRevision() {
		snap, errs, lerr := store.LoadSnapshot(ctx, w.db, w.cfg.ListenPort())
		if lerr != nil {
			w.markSyncFailure(lerr)
		} else {
			w.holder.Store(snap)
			w.Ready.markSuccess(snap.Revision)
			operationCount := 0
			for _, rt := range snap.Routes {
				operationCount += len(rt.Operations)
			}
			policyCount := 0
			for _, pols := range snap.Policies {
				policyCount += len(pols)
			}
			if errs.Total() > 0 {
				log.Printf("sync: revision %d loaded with %d invalid row(s): %+v", snap.Revision, errs.Total(), errs)
			} else {
				log.Printf("sync: revision %d loaded (%d routes, %d policies, %d operations, %d identities)",
					snap.Revision, len(snap.Routes), policyCount, operationCount, len(snap.Identities))
			}
		}
	} else if w.Ready.lastError() != "" {
		// Recovery: the revision probe succeeded and the revision is unchanged,
		// so the snapshot being served is current again. Without this, readiness
		// would stay degraded until the next forced reload (ADR-0001 §3.4).
		log.Printf("sync: recovered; revision %d still current", rev)
		w.Ready.markSuccess(rev)
	} else {
		w.Ready.touchOK()
	}
	w.heartbeat(ctx, w.Ready.lastError() == "", nil)
	if shouldApplySideLoads(rev, w.sideLoadsRev, forced) {
		w.applyDebugSettings(ctx)
		w.applyBizFields(ctx)
		w.applyAlertChannels(ctx)
		w.applyAlertRules(ctx)
		w.sideLoadsRev = rev
	}
	w.gcExpiredSessions(ctx)
	w.gcStaleInstances(ctx)
	w.gcRetention(ctx)
}

// shouldApplySideLoads reports whether this tick must re-read the side
// settings (debug log, biz fields, alert channels, alert rules). Those rows
// change only through revision-bumping admin writes, so an unchanged revision
// — the steady state — skips the loads. Forced ticks still apply them (they
// are the self-healing path for break-glass direct DB edits that forgot to
// bump the revision), and the first successful tick always does because
// lastAppliedRev starts at -1 while revisions are >= 0.
func shouldApplySideLoads(rev, lastAppliedRev int64, forced bool) bool {
	return forced || rev != lastAppliedRev
}

// applyBizFields loads the global business-envelope fallback every tick. A
// transient load error keeps the previous runtime state (fail-static): the
// holder is only updated when the row was actually read.
func (w *Worker) applyBizFields(ctx context.Context) {
	f, err := store.LoadBizFieldsSync(ctx, w.db)
	if err != nil {
		log.Printf("sync: load biz fields failed (keeping previous): %v", err)
		return
	}
	w.bizFields.Set(f)
}

// applyAlertChannels loads every configured alert channel per tick. A load
// failure keeps the previous runtime list (fail-static).
func (w *Worker) applyAlertChannels(ctx context.Context) {
	channels, err := store.LoadAlertChannels(ctx, w.db)
	if err != nil {
		log.Printf("sync: load alert channels failed (keeping previous): %v", err)
		return
	}
	w.alertCh.Set(channels)
}

// applyAlertRules loads every alert rule per tick.
func (w *Worker) applyAlertRules(ctx context.Context) {
	rules, err := store.LoadAlertRules(ctx, w.db)
	if err != nil {
		log.Printf("sync: load alert rules failed (keeping previous): %v", err)
		return
	}
	if len(rules) != len(w.alertRules.Get()) {
		log.Printf("sync: alert rules loaded: %d", len(rules))
	}
	w.alertRules.Set(rules)
}

// applyDebugSettings loads settings.debug_log every tick and hands it to the
// debug manager; unchanged settings are a no-op there. A transient load error
// keeps the previous runtime state (fail-static like everything else on DB
// loss): the manager is only touched when the row was actually read.
func (w *Worker) applyDebugSettings(ctx context.Context) {
	ds, err := store.LoadDebugSettingsSync(ctx, w.db)
	if err != nil {
		log.Printf("sync: load debug settings failed (keeping previous): %v", err)
		return
	}
	if err := w.debug.Apply(ds); err != nil {
		log.Printf("sync: apply debug settings failed (debug disabled): %v", err)
	}
}

func (w *Worker) markSyncFailure(err error) {
	if w.Ready.markFailure(err) {
		log.Printf("sync: load failed, serving last snapshot (fail-static): %v", err)
	}
}

func (w *Worker) heartbeat(ctx context.Context, ok bool, err error) {
	const q = `INSERT INTO instances (instance_id, version, loaded_revision, started_at, last_sync_at, last_sync_ok, updated_at)
		VALUES (?, ?, ?, NOW(), NOW(), ?, NOW())
		ON DUPLICATE KEY UPDATE version = VALUES(version), loaded_revision = VALUES(loaded_revision),
		last_sync_at = VALUES(last_sync_at), last_sync_ok = VALUES(last_sync_ok), updated_at = NOW()`
	if _, e := w.db.ExecContext(ctx, q, w.instanceID, w.version, w.Ready.loadedRevision(), ok); e != nil && err == nil {
		log.Printf("sync: heartbeat failed: %v", e)
	}
}

// gcStaleInstances removes heartbeats older than 10 minutes so dead instances
// do not pollute the propagation counters (ADR-0001 §3.5).
func (w *Worker) gcStaleInstances(ctx context.Context) {
	_, _ = w.db.ExecContext(ctx, `DELETE FROM instances WHERE updated_at < NOW() - INTERVAL 10 MINUTE`)
}

// gcExpiredSessions removes admin sessions past their sliding or absolute
// expiry, and revoked ones (ADR-0001 §8.2).
func (w *Worker) gcExpiredSessions(ctx context.Context) {
	_, _ = w.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at < NOW() OR absolute_expires_at < NOW() OR revoked_at IS NOT NULL`)
}

// auditRetentionDays bounds admin_audit and alert_events history. Both tables
// grow on every admin action and alert firing (firings are intentionally
// never suppressed, ADR §9.2), so without a window they grow unbounded.
// Operators keeping audits longer than 90 days must export them.
const auditRetentionDays = 90

// gcRetention deletes admin_audit and alert_events rows older than the
// retention window. Run every tick: both timestamp columns are indexed
// (idx_created), so the deletes are cheap range scans and the table never
// accumulates more than ~one tick of expired rows. A failure is logged and
// never fails the tick, mirroring the other GC steps.
func (w *Worker) gcRetention(ctx context.Context) {
	// Timestamp columns per migrations/0001 and 0007 (both DATETIME with
	// idx_created); identifiers are package constants, never user input.
	w.deleteRetention(ctx, "admin_audit", "created_at")
	w.deleteRetention(ctx, "alert_events", "created_at")
}

func (w *Worker) deleteRetention(ctx context.Context, table, column string) {
	res, err := w.db.ExecContext(ctx,
		"DELETE FROM "+table+" WHERE "+column+" < NOW() - INTERVAL ? DAY", auditRetentionDays)
	if err != nil {
		log.Printf("sync: %s retention delete failed: %v", table, err)
		return
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		log.Printf("sync: %s retention deleted %d row(s) older than %d days", table, n, auditRetentionDays)
	}
}
