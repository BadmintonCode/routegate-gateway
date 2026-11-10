package health

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql" // lazy driver: the DSN never connects in-process

	"github.com/BadmintonCode/routegate-gateway/internal/config"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/syncworker"
)

// TestReadyDraining: before Begin the readiness path evaluates normally (the
// unreachable DB yields not_ready, never "draining"); after Begin, /__ready
// answers 503 {"code":"draining"} without touching the DB, while liveness
// (/__health) stays 200 so the orchestrator does not kill a draining instance.
func TestReadyDraining(t *testing.T) {
	db, err := sql.Open("mysql", "u:p@tcp(127.0.0.1:1)/x")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	drain := &Drain{}
	h := Handler(&config.Config{ReadyStaleAfter: time.Minute}, db, &snapshot.Holder{}, &syncworker.Ready{}, Options{Drain: drain})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__ready", nil))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "draining") {
		t.Fatalf("pre-drain /__ready = %d %s, want 503 not_ready", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-drain /__health = %d, want 200", rec.Code)
	}

	drain.Begin()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__ready", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "draining") {
		t.Fatalf("draining /__ready = %d %s, want 503 draining", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("draining /__health = %d, want 200", rec.Code)
	}
}
