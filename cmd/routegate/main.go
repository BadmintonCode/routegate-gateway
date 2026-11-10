// Command routegate is the RouteGate gateway binary.
//
// RouteGate is a MySQL-backed service-to-service gateway with mTLS workload
// identity, default-deny authorization policies, and a built-in admin panel.
// See docs/adr/0001-routegate-design.md for the founding design.
//
// Milestone status: serve (data plane + health listener), migrate, config
// bump, and version are implemented. The Admin API and web panel arrive with
// M3.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/accesslog"
	"github.com/BadmintonCode/routegate-gateway/internal/adminapi"
	"github.com/BadmintonCode/routegate-gateway/internal/alerts"
	"github.com/BadmintonCode/routegate-gateway/internal/config"
	"github.com/BadmintonCode/routegate-gateway/internal/health"
	"github.com/BadmintonCode/routegate-gateway/internal/proxy"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/store"
	"github.com/BadmintonCode/routegate-gateway/internal/syncworker"
	"github.com/BadmintonCode/routegate-gateway/internal/web"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

const usage = `routegate - a MySQL-backed service-to-service gateway

Usage:
  routegate serve        Run the gateway (data plane + health listener)
  routegate migrate      Apply embedded database migrations, then exit
  routegate config bump  Increment config_revision (break-glass direct DB edits)
  routegate version      Print version information

Configuration: environment variables (ROUTEGATE_*), see docs/adr/0001-routegate-design.md §13.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "version":
		fmt.Printf("routegate %s\n", version)
	case "serve":
		err = cmdServe()
	case "migrate":
		err = cmdMigrate()
	case "config":
		if len(os.Args) >= 3 && os.Args[2] == "bump" {
			err = cmdConfigBump()
			break
		}
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func loadConfig() (*config.Config, error) {
	return config.Load()
}

func cmdServe() error {
	start := time.Now()
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DBDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	holder := &snapshot.Holder{}
	var sink accesslog.Sink
	switch cfg.AccessLogSink {
	case "file":
		sink, err = accesslog.NewFile(accesslog.FileOptions{
			Path:       cfg.AccessLogFile,
			MaxBytes:   cfg.AccessLogMaxBytes,
			RetainDays: cfg.AccessLogRetainDays,
		})
		if err != nil {
			return err
		}
	default:
		sink = accesslog.NewStdout(os.Stdout)
	}
	defer sink.Close()
	adminLogFile := ""
	if cfg.AccessLogSink == "file" {
		adminLogFile = cfg.AccessLogFile
	}

	// Debug logging (ADR-0001 §6.6): panel-managed via settings.debug_log.
	// The manager starts disabled; the sync worker applies the settings on
	// every tick (create/stop the sink, update capture caps atomically).
	debugMgr := accesslog.NewDebugManager()
	defer debugMgr.Close()
	bizFields := store.NewBizFieldsHolder()
	alertCh := store.NewAlertChannelsHolder()
	alertRules := store.NewAlertRulesHolder()
	alertEngine := alerts.NewEngine(alertRules, alertCh, db, cfg.InstanceID)
	defer alertEngine.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	drain := &health.Drain{}

	var dataSrv, adminSrv, healthSrv *http.Server // shutdown order: data → admin → health

	// Migrate synchronously before anything depends on the schema (the sync
	// worker and the admin-plane bootstrap both assume it).
	if cfg.AutoMigrate {
		if err := store.Migrate(ctx, db); err != nil {
			return err
		}
	}

	worker := syncworker.New(cfg, db, holder, cfg.InstanceID, version, debugMgr, bizFields, alertCh, alertRules)
	go func() {
		if err := worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatalf("sync worker: %v", err)
		}
	}()

	// Health listener: plaintext, no client certs, internal networks only
	// (ADR-0001 §2.2). The data plane cannot serve probes: it requires client
	// certificates at the TLS layer.
	if cfg.HealthEnabled {
		metrics := &health.MetricsDeps{
			Version: version, InstanceID: cfg.InstanceID, Start: start,
			Holder: holder, Ready: &worker.Ready, StaleAfter: cfg.ReadyStaleAfter,
			AlertRules: alertRules, AlertChannels: alertCh, Alerts: alertEngine,
			Debug: debugMgr,
		}
		if dr, ok := sink.(accesslog.DroppedReporter); ok {
			metrics.AccessLog = dr
		}
		h := &http.Server{
			Addr:              cfg.HealthListen,
			Handler:           health.Handler(cfg, db, holder, &worker.Ready, health.Options{Version: version, ClientAuth: string(cfg.ClientAuth), Metrics: metrics, Drain: drain}),
			ReadHeaderTimeout: 5 * time.Second,
		}
		healthSrv = h
		go func() {
			log.Printf("health listener on %s (/__health /__ready /__status /__metrics)", cfg.HealthListen)
			if err := h.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("health listener: %v", err)
			}
		}()
		defer h.Close()
	}

	// Admin plane (ADR-0001 §7, §8): Admin API + panel, management networks
	// only. Plain HTTP by owner decision — production deployments should
	// restrict the listener to a management network or front it with TLS.
	if cfg.AdminEnabled {
		admin := adminapi.New(adminapi.AdminConfig{
			BootstrapUsername: cfg.AdminBootstrapUsername,
			BootstrapPassword: cfg.AdminBootstrapPassword,
			OwnProxyPort:      cfg.ListenPort(),
			AccessLogFilePath: adminLogFile,
			Debug:             debugMgr,
		}, db, holder, cfg.InstanceID, version)
		if err := admin.Bootstrap(ctx); err != nil {
			return fmt.Errorf("admin bootstrap: %w", err)
		}
		a := &http.Server{
			Addr:              cfg.AdminListen,
			Handler:           admin.Handler(web.Panel()),
			ReadHeaderTimeout: 10 * time.Second,
		}
		adminSrv = a
		go func() {
			log.Printf("admin listener on %s (plain HTTP, panel at /admin/)", cfg.AdminListen)
			if err := a.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("admin listener: %v", err)
			}
		}()
		defer a.Close()
	}

	if !cfg.ProxyEnabled {
		log.Printf("routegate %s running (management-only instance; proxy disabled)", version)
		<-ctx.Done()
		drain.Begin()
		shutdownServer("admin", adminSrv, 5*time.Second)
		shutdownServer("health", healthSrv, time.Second)
		return nil
	}

	tlsCfg, err := proxy.TLSConfig(cfg.ServerTLSCert, cfg.ServerTLSKey)
	if err != nil {
		return err
	}
	dataSrv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           proxy.NewServer(cfg, holder, sink, cfg.InstanceID, debugMgr, bizFields, alertEngine),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- dataSrv.ListenAndServeTLS("", "") }()
	log.Printf("routegate %s data plane on %s (mTLS required, client_auth=%s, sync=%s)",
		version, cfg.Listen, cfg.ClientAuth, cfg.SyncInterval)

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	// Graceful drain (ADR-0001 §14, owner-approved 2026-11-08): /__ready
	// flips 503 immediately so the LB removes this instance, then in-flight
	// requests finish under a bounded deadline. The management listeners
	// close after the data plane so the health listener keeps answering
	// (503) while probes observe the drain.
	drain.Begin()
	log.Printf("SIGTERM: draining (grace %s), /__ready answers 503", cfg.DrainTimeout)
	shutdownServer("data plane", dataSrv, cfg.DrainTimeout)
	shutdownServer("admin", adminSrv, 5*time.Second)
	shutdownServer("health", healthSrv, time.Second)
	return nil
}

// shutdownServer drains one listener under its grace; on expiry it closes
// immediately, cutting in-flight work (the bounded-drain contract).
func shutdownServer(name string, srv *http.Server, grace time.Duration) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
		log.Printf("%s shutdown: %v (closed)", name, err)
	}
}

func cmdMigrate() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DBDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, db); err != nil {
		return err
	}
	fmt.Println("migrations applied")
	return nil
}

func cmdConfigBump() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DBDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rev, err := store.BumpRevision(ctx, db)
	if err != nil {
		return err
	}
	fmt.Printf("config_revision bumped to %d\n", rev)
	return nil
}
