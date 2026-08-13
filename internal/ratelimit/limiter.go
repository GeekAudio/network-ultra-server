// Package ratelimit provides small, in-process fixed-window limiters.
package ratelimit

import (
	"sync"
	"time"
)

const defaultMaxEntries = 65536

type entry struct {
	resetAt time.Time
	count   int
}

// Limiter is safe for concurrent use. It intentionally fails open when limit
// is zero so callers can disable one policy explicitly.
type Limiter struct {
	mu         sync.Mutex
	entries    map[string]entry
	now        func() time.Time
	calls      uint64
	maxEntries int
}

func New() *Limiter {
	return &Limiter{entries: make(map[string]entry), now: time.Now, maxEntries: defaultMaxEntries}
}

// Allow consumes one unit for key in window and reports whether it fit.
func (l *Limiter) Allow(key string, limit int, window time.Duration) bool {
	if limit <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	key = normalizeKey(key)
	e, exists := l.currentEntry(key, now, window)
	if !exists && !l.ensureCapacity(now, 1) {
		return false
	}
	if e.count >= limit {
		return false
	}
	e.count++
	l.entries[key] = e
	l.afterConsume(now)
	return true
}

// AllowPair atomically consumes one unit from both keys. If either budget is
// exhausted, neither budget is charged. This prevents a shared-IP rejection
// from silently burning a fresh peer's own budget.
func (l *Limiter) AllowPair(first, second string, limit int, window time.Duration) bool {
	if limit <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	first, second = normalizeKey(first), normalizeKey(second)
	a, aExists := l.currentEntry(first, now, window)
	if first == second {
		if a.count >= limit || (!aExists && !l.ensureCapacity(now, 1)) {
			return false
		}
		a.count++
		l.entries[first] = a
		l.afterConsume(now)
		return true
	}
	b, bExists := l.currentEntry(second, now, window)
	if a.count >= limit || b.count >= limit {
		return false
	}
	needed := 0
	if !aExists {
		needed++
	}
	if !bExists {
		needed++
	}
	if !l.ensureCapacity(now, needed) {
		return false
	}
	a.count++
	l.entries[first] = a
	b.count++
	l.entries[second] = b
	l.afterConsume(now)
	return true
}

func normalizeKey(key string) string {
	if key == "" {
		return "unknown"
	}
	return key
}

func (l *Limiter) currentEntry(key string, now time.Time, window time.Duration) (entry, bool) {
	e, exists := l.entries[key]
	if exists && !now.Before(e.resetAt) {
		delete(l.entries, key)
		exists = false
	}
	if !exists {
		e = entry{resetAt: now.Add(window)}
	}
	return e, exists
}

func (l *Limiter) ensureCapacity(now time.Time, needed int) bool {
	if needed == 0 || len(l.entries)+needed <= l.maxEntries {
		return true
	}
	l.cleanupExpired(now)
	return len(l.entries)+needed <= l.maxEntries
}

func (l *Limiter) afterConsume(now time.Time) {
	l.calls++
	if l.calls%1024 == 0 {
		l.cleanupExpired(now)
	}
}

func (l *Limiter) cleanupExpired(now time.Time) {
	for k, candidate := range l.entries {
		if !now.Before(candidate.resetAt) {
			delete(l.entries, k)
		}
	}
}
