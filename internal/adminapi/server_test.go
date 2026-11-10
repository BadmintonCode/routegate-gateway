package adminapi

import (
	"fmt"
	"testing"
	"time"
)

// TestLoginLimiterBlocksEleventhAttempt pins the base window behavior the
// eviction work must preserve: 10 attempts per ip|username per minute.
func TestLoginLimiterBlocksEleventhAttempt(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < 10; i++ {
		if !l.allow("10.0.0.1|alice") {
			t.Fatalf("attempt %d blocked, want allowed", i+1)
		}
	}
	if l.allow("10.0.0.1|alice") {
		t.Fatal("11th attempt allowed, want blocked")
	}
	// A different key has its own window.
	if !l.allow("10.0.0.2|alice") {
		t.Fatal("independent key blocked, want allowed")
	}
}

// TestLoginLimiterLazyResetOnAccess: an expired window accessed again starts
// fresh instead of staying blocked.
func TestLoginLimiterLazyResetOnAccess(t *testing.T) {
	l := newLoginLimiter()
	l.window["10.0.0.9|bob"] = &loginWindow{count: 10, resetAt: time.Now().Add(-time.Second)}
	for i := 0; i < 10; i++ {
		if !l.allow("10.0.0.9|bob") {
			t.Fatalf("attempt %d after expiry blocked, want a fresh window", i+1)
		}
	}
	if l.allow("10.0.0.9|bob") {
		t.Fatal("11th attempt in the fresh window allowed, want blocked")
	}
}

// TestLoginLimiterSweepEvictsExpiredKeepsActive: crossing the size bound
// sweeps expired windows while keeping active ones.
func TestLoginLimiterSweepEvictsExpiredKeepsActive(t *testing.T) {
	l := newLoginLimiter()
	now := time.Now()
	for i := 0; i < 1100; i++ {
		l.window[fmt.Sprintf("10.0.0.%d|u", i)] = &loginWindow{count: 3, resetAt: now.Add(-time.Minute)}
	}
	for i := 0; i < 50; i++ {
		l.window[fmt.Sprintf("10.1.0.%d|u", i)] = &loginWindow{count: 3, resetAt: now.Add(time.Minute)}
	}
	if !l.allow("10.2.0.1|u") {
		t.Fatal("fresh key blocked, want allowed")
	}
	if got := len(l.window); got != 51 {
		t.Fatalf("window size = %d, want 51 (expired entries evicted, active kept, new key added)", got)
	}
	for i := 0; i < 50; i++ {
		if _, ok := l.window[fmt.Sprintf("10.1.0.%d|u", i)]; !ok {
			t.Fatalf("active entry 10.1.0.%d was evicted", i)
		}
	}
	for i := 0; i < 1100; i++ {
		if _, ok := l.window[fmt.Sprintf("10.0.0.%d|u", i)]; ok {
			t.Fatalf("expired entry 10.0.0.%d survived the sweep", i)
		}
	}
}

// TestLoginLimiterBoundedUnderChurn: bursts of one-shot keys across time do
// not grow the map without bound. Aging every window between batches stands
// in for the minute passing in production.
func TestLoginLimiterBoundedUnderChurn(t *testing.T) {
	l := newLoginLimiter()
	for batch := 0; batch < 5; batch++ {
		if batch > 0 {
			// Age every window before the batch so the churn's sweep has
			// something to evict — the stand-in for the minute passing in
			// production.
			for _, w := range l.window {
				w.resetAt = time.Now().Add(-time.Minute)
			}
		}
		for i := 0; i < 800; i++ {
			if !l.allow(fmt.Sprintf("10.%d.%d.1|u", batch, i)) {
				t.Fatalf("batch %d key %d blocked on a fresh window", batch, i)
			}
		}
	}
	if got := len(l.window); got > 2*loginLimiterMaxEntries {
		t.Fatalf("window size = %d after 4000 churned keys, want bounded near %d", got, loginLimiterMaxEntries)
	}
	for k, w := range l.window {
		if time.Now().After(w.resetAt) {
			t.Fatalf("expired entry %q survived the sweeps", k)
		}
	}
}
