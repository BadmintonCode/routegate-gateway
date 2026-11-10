// Package config parses and validates RouteGate environment configuration
// (ADR-0001 §3.3, §13). All variables carry the ROUTEGATE_ prefix.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ClientAuth selects the workload-identity credential form. All modes enforce
// authorization; there is no auth-free mode (ADR-0001 §5.1).
type ClientAuth string

const (
	ClientAuthCert      ClientAuth = "cert"
	ClientAuthCertOrJWT ClientAuth = "cert_or_jwt"
	ClientAuthJWT       ClientAuth = "jwt"
)

// Config is the resolved process configuration.
type Config struct {
	// Control plane.
	DBDSN            string
	AutoMigrate      bool
	SyncInterval     time.Duration
	ForceReloadEvery int
	ReadyStaleAfter  time.Duration // 0 => 3*SyncInterval
	BootMaxWait      time.Duration // 0 => retry forever
	DrainTimeout     time.Duration // SIGTERM grace: in-flight requests drain, bounded

	// Listeners.
	Listen        string
	ProxyEnabled  bool
	AdminListen   string
	AdminEnabled  bool
	HealthListen  string
	HealthEnabled bool

	// Data plane.
	ClientAuth                ClientAuth
	ServerTLSCert             string
	ServerTLSKey              string
	TrustedUserContextCallers []string

	// Admin plane (ADR-0001 §7, §8). The admin listener serves plain HTTP
	// (owner decision 2026-11-07): protect it at the deployment layer —
	// management network or a TLS-terminating front proxy.
	AdminBootstrapUsername string
	AdminBootstrapPassword string

	// Upstream connection behavior (ADR-0001 §5 step 5, §14).
	UpstreamKeepalive bool
	UpstreamDNSTTL    time.Duration

	// Access log sink (ADR-0001 §6.5).
	AccessLogSink       string // stdout | file | mysql
	AccessLogFile       string // file sink base path
	AccessLogMaxBytes   int64  // per-segment cap (rotation)
	AccessLogRetainDays int    // segment retention window (days)

	// Debug logging (ADR-0001 §6.6) is panel-managed in settings.debug_log —
	// deliberately no environment variables here.

	InstanceID string
}

const (
	defaultAccessLogMaxMB      = 2
	defaultAccessLogRetainDays = 7
)

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(env(key, "")) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw := env(key, "")
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: invalid duration %q", key, raw)
	}
	return d, nil
}

// Load builds a Config from the environment.
func Load() (*Config, error) {
	c := &Config{
		DBDSN:               env("ROUTEGATE_DB_DSN", ""),
		AutoMigrate:         envBool("ROUTEGATE_AUTO_MIGRATE", true),
		Listen:              env("ROUTEGATE_LISTEN", ":8080"),
		ProxyEnabled:        envBool("ROUTEGATE_PROXY_ENABLED", true),
		AdminListen:         env("ROUTEGATE_ADMIN_LISTEN", ":8081"),
		AdminEnabled:        envBool("ROUTEGATE_ADMIN_ENABLED", true),
		HealthListen:        env("ROUTEGATE_HEALTH_LISTEN", ":8082"),
		HealthEnabled:       envBool("ROUTEGATE_HEALTH_ENABLED", true),
		ServerTLSCert:       env("ROUTEGATE_SERVER_TLS_CERT", ""),
		ServerTLSKey:        env("ROUTEGATE_SERVER_TLS_KEY", ""),
		UpstreamKeepalive:   envBool("ROUTEGATE_UPSTREAM_KEEPALIVE", true),
		AccessLogSink:       strings.ToLower(env("ROUTEGATE_ACCESS_LOG_SINK", "stdout")),
		AccessLogFile:       env("ROUTEGATE_ACCESS_LOG_FILE", ""),
		AccessLogMaxBytes:   0, // set below
		AccessLogRetainDays: 0, // set below
		InstanceID:          env("ROUTEGATE_INSTANCE_ID", hostnameOrDefault()),

		AdminBootstrapUsername: env("ROUTEGATE_ADMIN_BOOTSTRAP_USERNAME", ""),
		AdminBootstrapPassword: env("ROUTEGATE_ADMIN_BOOTSTRAP_PASSWORD", ""),
	}

	var err error
	if c.SyncInterval, err = envDuration("ROUTEGATE_CONFIG_SYNC_INTERVAL", 60*time.Second); err != nil {
		return nil, err
	}
	if c.SyncInterval < time.Second {
		return nil, fmt.Errorf("ROUTEGATE_CONFIG_SYNC_INTERVAL: minimum is 1s, got %s", c.SyncInterval)
	}
	if c.ReadyStaleAfter, err = envDuration("ROUTEGATE_READY_STALE_AFTER", 3*c.SyncInterval); err != nil {
		return nil, err
	}
	if c.BootMaxWait, err = envDuration("ROUTEGATE_BOOT_MAX_WAIT", 0); err != nil {
		return nil, err
	}
	if c.DrainTimeout, err = envDuration("ROUTEGATE_DRAIN_TIMEOUT", 20*time.Second); err != nil {
		return nil, err
	}
	if c.UpstreamDNSTTL, err = envDuration("ROUTEGATE_UPSTREAM_DNS_TTL", 5*time.Second); err != nil {
		return nil, err
	}
	maxMB, err := strconv.Atoi(env("ROUTEGATE_ACCESS_LOG_MAX_MB", ""))
	if err != nil {
		maxMB = defaultAccessLogMaxMB
	}
	if maxMB < 1 {
		return nil, fmt.Errorf("ROUTEGATE_ACCESS_LOG_MAX_MB: must be >= 1, got %d", maxMB)
	}
	c.AccessLogMaxBytes = int64(maxMB) << 20
	retain, err := strconv.Atoi(env("ROUTEGATE_ACCESS_LOG_RETAIN_DAYS", ""))
	if err != nil {
		retain = defaultAccessLogRetainDays
	}
	if retain < 1 {
		return nil, fmt.Errorf("ROUTEGATE_ACCESS_LOG_RETAIN_DAYS: must be >= 1, got %d", retain)
	}
	c.AccessLogRetainDays = retain

	if raw := env("ROUTEGATE_FORCE_RELOAD_EVERY", "10"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("ROUTEGATE_FORCE_RELOAD_EVERY: positive integer required, got %q", raw)
		}
		c.ForceReloadEvery = n
	}

	switch raw := strings.ToLower(env("ROUTEGATE_CLIENT_AUTH", string(ClientAuthCert))); raw {
	case string(ClientAuthCert):
		c.ClientAuth = ClientAuthCert
	case string(ClientAuthCertOrJWT), string(ClientAuthJWT):
		// Parse the mode (the constants stay), but fail loudly: pure mTLS is
		// the only implemented identity in this release, and silently running
		// a different mode than configured is worse than refusing to start.
		return nil, fmt.Errorf("ROUTEGATE_CLIENT_AUTH: mode %q is not implemented in this release (only \"cert\")", raw)
	default:
		return nil, fmt.Errorf("ROUTEGATE_CLIENT_AUTH: unknown mode %q (want cert, cert_or_jwt, jwt)", raw)
	}

	if raw := env("ROUTEGATE_TRUSTED_USER_CONTEXT_CALLERS", ""); raw != "" {
		for _, name := range strings.Split(raw, ",") {
			if name = strings.TrimSpace(name); name != "" {
				c.TrustedUserContextCallers = append(c.TrustedUserContextCallers, name)
			}
		}
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.DBDSN == "" {
		return fmt.Errorf("ROUTEGATE_DB_DSN is required")
	}
	if c.ProxyEnabled && c.AdminEnabled && c.Listen == c.AdminListen {
		return fmt.Errorf("listener address conflict: ROUTEGATE_LISTEN == ROUTEGATE_ADMIN_LISTEN (%s)", c.Listen)
	}
	if c.ProxyEnabled && c.HealthEnabled && c.Listen == c.HealthListen {
		return fmt.Errorf("listener address conflict: ROUTEGATE_LISTEN == ROUTEGATE_HEALTH_LISTEN (%s)", c.Listen)
	}
	if c.AdminEnabled && c.HealthEnabled && c.AdminListen == c.HealthListen {
		return fmt.Errorf("listener address conflict: ROUTEGATE_ADMIN_LISTEN == ROUTEGATE_HEALTH_LISTEN (%s)", c.AdminListen)
	}
	if c.ProxyEnabled {
		if c.ServerTLSCert == "" || c.ServerTLSKey == "" {
			return fmt.Errorf("ROUTEGATE_SERVER_TLS_CERT and ROUTEGATE_SERVER_TLS_KEY are required when the proxy is enabled")
		}
	}

	switch c.AccessLogSink {
	case "stdout":
	case "file":
		if c.AccessLogFile == "" {
			return fmt.Errorf("ROUTEGATE_ACCESS_LOG_FILE is required when ROUTEGATE_ACCESS_LOG_SINK=file")
		}
	case "mysql":
		return fmt.Errorf("ROUTEGATE_ACCESS_LOG_SINK=mysql is not implemented in this milestone; use stdout or file")
	default:
		return fmt.Errorf("ROUTEGATE_ACCESS_LOG_SINK: unknown sink %q (want stdout, file, mysql)", c.AccessLogSink)
	}
	return nil
}

// ListenPort returns the numeric port of the data-plane listener, or 0 when it
// cannot be determined (used for the self-loop target validation, ADR-0001 §3.2).
func (c *Config) ListenPort() int {
	_, port, err := splitHostPort(c.Listen)
	if err != nil {
		return 0
	}
	return port
}

func splitHostPort(addr string) (string, int, error) {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return "", 0, fmt.Errorf("address %q has no port", addr)
	}
	port, err := strconv.Atoi(addr[idx+1:])
	if err != nil {
		return "", 0, fmt.Errorf("address %q has invalid port", addr)
	}
	return strings.Trim(addr[:idx], "[]"), port, nil
}

func hostnameOrDefault() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "routegate-unknown"
	}
	return h
}
