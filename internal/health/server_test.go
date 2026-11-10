package health

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql" // lazy driver: the DSN never connects in-process

	"github.com/BadmintonCode/routegate-gateway/internal/config"
	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
	"github.com/BadmintonCode/routegate-gateway/internal/syncworker"
)

// TestNoStoreHeaders: the readiness 503 states (not_ready, draining) and the
// instance-internals status document must never be cacheable. Bodies stay
// code-first JSON (probe contract) — only the header is asserted here.
func TestNoStoreHeaders(t *testing.T) {
	db, err := sql.Open("mysql", "u:p@tcp(127.0.0.1:1)/x")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	drain := &Drain{}
	h := Handler(&config.Config{ReadyStaleAfter: time.Minute}, db, &snapshot.Holder{}, &syncworker.Ready{}, Options{Drain: drain})

	get := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}

	if rec := get("/__ready"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/__ready = %d, want 503 not_ready (unreachable DB)", rec.Code)
	} else if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("/__ready not_ready Cache-Control = %q, want no-store", cc)
	}

	drain.Begin()
	if rec := get("/__ready"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/__ready = %d, want 503 draining", rec.Code)
	} else if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("/__ready draining Cache-Control = %q, want no-store", cc)
	}

	if rec := get("/__status"); rec.Code != http.StatusOK {
		t.Fatalf("/__status = %d, want 200", rec.Code)
	} else if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("/__status Cache-Control = %q, want no-store", cc)
	}
}
