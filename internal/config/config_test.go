package config

import (
	"strings"
	"testing"
)

func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ROUTEGATE_DB_DSN", "user:pw@tcp(host:3306)/db")
	t.Setenv("ROUTEGATE_PROXY_ENABLED", "false")
	t.Setenv("ROUTEGATE_ADMIN_ENABLED", "false")
	t.Setenv("ROUTEGATE_HEALTH_ENABLED", "false")
}

// TestDebugLoggingHasNoEnvKnobs: debug logging is panel-managed in the
// settings store (ADR-0001 §6.6) — the process config carries no debug fields,
// and the removed admin TLS variables are gone with them.
func TestDebugLoggingHasNoEnvKnobs(t *testing.T) {
	baseEnv(t)
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	// (The absence of the debug/admin-TLS fields is enforced by compilation —
	// any reference to them no longer type-checks.)
}

// TestAdminPlainHTTP: the admin listener never serves TLS again; unknown
// legacy env vars are simply ignored.
func TestAdminPlainHTTP(t *testing.T) {
	baseEnv(t)
	t.Setenv("ROUTEGATE_ADMIN_TLS_CERT", "cert.pem")
	t.Setenv("ROUTEGATE_ADMIN_TLS_KEY", "key.pem")
	if _, err := Load(); err != nil {
		t.Fatalf("legacy admin TLS env vars must not break startup: %v", err)
	}
}

// TestClientAuthModes: pure mTLS ("cert") is the only implemented identity in
// this release. The future modes stay parseable constants but must fail
// loudly at startup instead of silently running a different identity model.
func TestClientAuthModes(t *testing.T) {
	baseEnv(t)
	t.Setenv("ROUTEGATE_CLIENT_AUTH", "cert")
	if _, err := Load(); err != nil {
		t.Fatalf("mode cert must be accepted, got %v", err)
	}

	for _, mode := range []string{"cert_or_jwt", "jwt", "CERT_OR_JWT", "JWT"} {
		t.Setenv("ROUTEGATE_CLIENT_AUTH", mode)
		_, err := Load()
		if err == nil {
			t.Errorf("mode %q must be rejected", mode)
			continue
		}
		want := `ROUTEGATE_CLIENT_AUTH: mode "` + strings.ToLower(mode) + `" is not implemented in this release (only "cert")`
		if err.Error() != want {
			t.Errorf("mode %q error = %q, want %q", mode, err.Error(), want)
		}
	}

	baseEnv(t)
	t.Setenv("ROUTEGATE_CLIENT_AUTH", "hobbits")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "unknown mode") {
		t.Errorf("unknown mode error = %v, want \"unknown mode\"", err)
	}
}
