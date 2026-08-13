package ratelimit

import (
	"testing"
	"time"
)

func TestLimiterSeparatesKeysAndResetsWindow(t *testing.T) {
	l := New()
	now := time.Unix(100, 0)
	l.now = func() time.Time { return now }
	if !l.Allow("a", 2, time.Minute) || !l.Allow("a", 2, time.Minute) {
		t.Fatal("first two requests should pass")
	}
	if l.Allow("a", 2, time.Minute) {
		t.Fatal("third request should be limited")
	}
	if !l.Allow("b", 2, time.Minute) {
		t.Fatal("different key must have a separate budget")
	}
	now = now.Add(time.Minute)
	if !l.Allow("a", 2, time.Minute) {
		t.Fatal("budget should reset after its window")
	}
}

func TestCleanupNeverUsesAnotherKeysShorterWindow(t *testing.T) {
	l := New()
	now := time.Unix(100, 0)
	l.now = func() time.Time { return now }
	if !l.Allow("minute", 1, time.Minute) {
		t.Fatal("initial minute request denied")
	}
	now = now.Add(3 * time.Second)
	for i := 0; i < 1024; i++ {
		if !l.Allow("second-"+time.Unix(int64(i), 0).String(), 1, time.Second) {
			t.Fatal("unique key denied")
		}
	}
	if l.Allow("minute", 1, time.Minute) {
		t.Fatal("short-window cleanup deleted a live minute budget")
	}
}

func TestAllowPairDoesNotPartiallyConsumeWhenOneKeyIsExhausted(t *testing.T) {
	l := New()
	now := time.Unix(100, 0)
	l.now = func() time.Time { return now }
	if !l.Allow("shared-ip", 1, time.Minute) {
		t.Fatal("failed to prime shared IP budget")
	}
	if l.AllowPair("fresh-peer", "shared-ip", 1, time.Minute) {
		t.Fatal("pair should be rejected by exhausted IP budget")
	}
	if !l.Allow("fresh-peer", 1, time.Minute) {
		t.Fatal("rejected pair partially consumed the fresh peer budget")
	}
}

func TestLimiterBoundsUntrustedKeyCardinalityAndRecovers(t *testing.T) {
	l := New()
	l.maxEntries = 2
	now := time.Unix(100, 0)
	l.now = func() time.Time { return now }
	if !l.Allow("spoof-1", 1, time.Minute) || !l.Allow("spoof-2", 1, time.Minute) {
		t.Fatal("requests within capacity were denied")
	}
	if l.Allow("spoof-3", 1, time.Minute) {
		t.Fatal("new key should fail closed at the cardinality cap")
	}
	now = now.Add(time.Minute)
	if !l.Allow("legitimate", 1, time.Minute) {
		t.Fatal("expired attacker keys were not reclaimed")
	}
}
