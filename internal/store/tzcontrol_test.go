package store

import (
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// TestControlSessionDiesWithoutPin: WITHOUT the DSN pin (raw sql.Open), the
// same insert+GC kills the fresh session on a non-UTC server. This is the
// control proving TestSessionSurvivesGC actually detects the bug.
func TestControlSessionDiesWithoutPin(t *testing.T) {
	dsn := os.Getenv("ROUTEGATE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("needs ROUTEGATE_TEST_MYSQL_DSN")
	}
	db, err := sql.Open("mysql", dsn) // deliberately NOT via Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// On a server whose session clock already equals UTC the skew cannot
	// reproduce; skip instead of failing an inconclusive control.
	var skew string
	if err := db.QueryRow(`SELECT TIMEDIFF(NOW(), UTC_TIMESTAMP())`).Scan(&skew); err != nil {
		t.Fatal(err)
	}
	if skew == "00:00:00" {
		t.Skip("server session clock equals UTC; control cannot reproduce the skew here")
	}
	// A short sliding expiry makes the frame skew deterministic: without the
	// pin the written value is UTC(now+2s), which sits ~8h in the past of the
	// server's local-clock NOW(), so the GC deletes it immediately. With an
	// 8h expiry the skew leaves only a ~1s window (race-prone).
	now := time.Now()
	if _, err := db.Exec(
		`INSERT INTO sessions (user_id, token_hash, expires_at, absolute_expires_at, ip, user_agent)
		 VALUES (1, 'tz-control-hash', ?, ?, '127.0.0.1', 'tz-control')`,
		now.Add(2*time.Second), now.Add(1*time.Hour)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM sessions WHERE token_hash = 'tz-control-hash'`) })
	if _, err := db.Exec(`DELETE FROM sessions WHERE expires_at < NOW() OR absolute_expires_at < NOW() OR revoked_at IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = 'tz-control-hash'`).Scan(&n)
	if n != 0 {
		t.Fatal("control expected the fresh session to be GC-deleted without the pin, but it survived (server tz may be UTC — control inconclusive)")
	}
	t.Log("control: fresh session WAS deleted without the pin — bug reproduced")
}
