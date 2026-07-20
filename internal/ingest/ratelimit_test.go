package ingest

import (
	"sync"
	"testing"
	"time"
)

func TestLimiterAllowsBurstThenBlocks(t *testing.T) {
	l := NewLimiter(60, 1000) // 60/min = 1/s, burst 60
	now := time.Now()

	allowed := 0
	for i := 0; i < 100; i++ {
		if l.Allow("k", now) {
			allowed++
		}
	}
	if allowed != 60 {
		t.Errorf("allowed %d in a burst, want the full burst of 60", allowed)
	}
	if l.Allow("k", now) {
		t.Error("bucket should be empty")
	}
}

func TestLimiterRefillsOverTime(t *testing.T) {
	l := NewLimiter(60, 1000) // 1 token per second
	now := time.Now()

	for i := 0; i < 60; i++ {
		l.Allow("k", now)
	}
	if l.Allow("k", now) {
		t.Fatal("bucket should be drained")
	}

	if !l.Allow("k", now.Add(time.Second)) {
		t.Error("a token should have refilled after one second")
	}
	if l.Allow("k", now.Add(time.Second)) {
		t.Error("only one token should have refilled")
	}
}

func TestLimiterRefillIsCappedAtBurst(t *testing.T) {
	l := NewLimiter(60, 1000)
	now := time.Now()

	l.Allow("k", now)
	// A very long idle period must not let the bucket overfill.
	allowed := 0
	for i := 0; i < 200; i++ {
		if l.Allow("k", now.Add(time.Hour)) {
			allowed++
		}
	}
	if allowed > 60 {
		t.Errorf("allowed %d after a long idle, want at most the burst of 60", allowed)
	}
}

func TestLimiterKeysAreIndependent(t *testing.T) {
	l := NewLimiter(5, 1000)
	now := time.Now()

	for i := 0; i < 5; i++ {
		l.Allow("a", now)
	}
	if l.Allow("a", now) {
		t.Fatal("key a should be exhausted")
	}
	if !l.Allow("b", now) {
		t.Error("key b must have its own bucket")
	}
}

// TestLimiterFailsClosedWhenFull: the bucket map is itself an attack surface.
// An attacker with many source addresses could otherwise grow it without
// bound, so once full the limiter must refuse rather than allocate.
func TestLimiterFailsClosedWhenFull(t *testing.T) {
	l := NewLimiter(600, 3)
	now := time.Now()

	for _, k := range []string{"a", "b", "c"} {
		if !l.Allow(k, now) {
			t.Fatalf("key %s should fit within the map cap", k)
		}
	}
	if l.Allow("d", now) {
		t.Error("a new key past the map cap must be refused, not allocated")
	}
	if l.Len() != 3 {
		t.Errorf("tracked keys = %d, want 3", l.Len())
	}
}

func TestLimiterSweepReclaimsIdleKeys(t *testing.T) {
	l := NewLimiter(60, 1000)
	now := time.Now()

	l.Allow("a", now)
	l.Allow("b", now)
	if l.Len() != 2 {
		t.Fatalf("tracked = %d, want 2", l.Len())
	}

	// Not yet fully refilled: still worth tracking.
	l.Sweep(now.Add(time.Second))
	if l.Len() != 2 {
		t.Errorf("tracked = %d, want 2 (buckets are not yet idle)", l.Len())
	}

	// Long enough to be indistinguishable from a fresh bucket.
	l.Sweep(now.Add(2 * time.Hour))
	if l.Len() != 0 {
		t.Errorf("tracked = %d after a long idle, want 0", l.Len())
	}
}

func TestLimiterIsConcurrencySafe(t *testing.T) {
	l := NewLimiter(6000, 10000)
	now := time.Now()

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				l.Allow("k", now)
				if i%50 == 0 {
					l.Sweep(now)
					l.Len()
				}
			}
		}(w)
	}
	wg.Wait()
}

func TestLimiterRejectsNonsenseRate(t *testing.T) {
	l := NewLimiter(0, 10) // clamped to a minimum of 1
	if !l.Allow("k", time.Now()) {
		t.Error("a zero rate should clamp to something usable, not deny everything")
	}
}
