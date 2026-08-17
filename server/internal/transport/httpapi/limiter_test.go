package httpapi

import (
	"testing"
	"time"
)

func TestFixedWindowLimiterBoundsDistinctKeys(t *testing.T) {
	now := time.Now()
	limiter := NewFixedWindowLimiter(2, time.Minute)
	limiter.maxEntries = 2
	limiter.now = func() time.Time { return now }
	if !limiter.Allow("one") || !limiter.Allow("two") {
		t.Fatal("expected entries within the bound to be allowed")
	}
	if limiter.Allow("three") || len(limiter.entries) != 2 {
		t.Fatalf("new keys must be rejected at capacity: %d", len(limiter.entries))
	}
	now = now.Add(time.Minute)
	if !limiter.Allow("three") || len(limiter.entries) != 1 {
		t.Fatalf("expired entries should be evicted before rejecting: %d", len(limiter.entries))
	}
}

func TestFixedWindowLimiterResets(t *testing.T) {
	now := time.Now()
	limiter := NewFixedWindowLimiter(2, time.Minute)
	limiter.now = func() time.Time { return now }
	if !limiter.Allow("client") || !limiter.Allow("client") || limiter.Allow("client") {
		t.Fatal("limiter did not enforce fixed window")
	}
	now = now.Add(time.Minute)
	if !limiter.Allow("client") {
		t.Fatal("limiter did not reset")
	}
}
