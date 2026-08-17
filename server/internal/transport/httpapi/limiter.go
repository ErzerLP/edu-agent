package httpapi

import (
	"sync"
	"time"
)

type fixedWindowEntry struct {
	windowStart time.Time
	count       int
}

type FixedWindowLimiter struct {
	mu         sync.Mutex
	limit      int
	window     time.Duration
	maxEntries int
	entries    map[string]fixedWindowEntry
	now        func() time.Time
}

func NewFixedWindowLimiter(limit int, window time.Duration) *FixedWindowLimiter {
	return &FixedWindowLimiter{limit: limit, window: window, maxEntries: 10000, entries: map[string]fixedWindowEntry{}, now: time.Now}
}

func (l *FixedWindowLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	entry, exists := l.entries[key]
	if !exists && len(l.entries) >= l.maxEntries {
		l.removeExpired(now)
		if len(l.entries) >= l.maxEntries {
			return false
		}
	}
	if entry.windowStart.IsZero() || now.Sub(entry.windowStart) >= l.window {
		entry = fixedWindowEntry{windowStart: now}
	}
	entry.count++
	l.entries[key] = entry
	return l.limit > 0 && entry.count <= l.limit
}

func (l *FixedWindowLimiter) removeExpired(now time.Time) {
	for candidate, value := range l.entries {
		if now.Sub(value.windowStart) >= l.window {
			delete(l.entries, candidate)
		}
	}
}
