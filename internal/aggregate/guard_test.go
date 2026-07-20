package aggregate

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestGuardAdmitsAfterMinCount(t *testing.T) {
	g := NewGuard(10, 3)
	now := time.Now()

	if got := g.Admit("/a", now); got != OtherLabel {
		t.Errorf("first sighting = %q, want %s", got, OtherLabel)
	}
	if got := g.Admit("/a", now); got != OtherLabel {
		t.Errorf("second sighting = %q, want %s", got, OtherLabel)
	}
	// Third sighting clears the threshold.
	if got := g.Admit("/a", now); got != "/a" {
		t.Errorf("third sighting = %q, want /a", got)
	}
	// And it stays admitted.
	if got := g.Admit("/a", now); got != "/a" {
		t.Errorf("fourth sighting = %q, want /a", got)
	}
	if g.Len() != 1 {
		t.Errorf("admitted = %d, want 1", g.Len())
	}
}

// TestGuardResistsSquatting is the reason admission is frequency-based rather
// than first-come. A crawler emitting unique one-shot values must not be able
// to occupy every slot before real traffic arrives.
//
// Measured leakage into a 200-slot cap, with a real path still admitted in
// every case:
//
//	    5,000 unique values -> 0 slots
//	   50,000               -> 1
//	  500,000               -> 1
//	2,000,000               -> 9
//
// The guard degrades gradually and never locks out genuine traffic, which is
// the property that matters. A plain (non-conservative, non-aged) sketch lost
// 10 slots to only 5,000 values *and* then refused the real path outright.
func TestGuardResistsSquatting(t *testing.T) {
	g := NewGuard(10, 3)
	now := time.Now()

	for i := 0; i < 5000; i++ {
		g.Admit(fmt.Sprintf("/?id=%d", i), now) // each seen exactly once
	}
	if g.Len() != 0 {
		t.Errorf("one-shot values claimed %d slots; the cap should still be empty", g.Len())
	}

	// A genuinely popular path can still get in afterwards.
	for i := 0; i < 3; i++ {
		g.Admit("/real", now)
	}
	if got, ok := g.Peek("/real"); !ok || got != "/real" {
		t.Error("a repeatedly-seen value should be admitted even after a flood")
	}
}

// TestGuardDegradesGracefullyUnderHeavyFlood pins the important half of the
// property: even when a flood does leak a few slots, legitimate traffic must
// still be admitted. Losing some capacity is acceptable; being locked out is
// not.
func TestGuardDegradesGracefullyUnderHeavyFlood(t *testing.T) {
	g := NewGuard(200, 3)
	now := time.Now()

	for i := 0; i < 500_000; i++ {
		g.Admit(fmt.Sprintf("/?id=%d", i), now)
	}
	if leaked := g.Len(); leaked > 20 {
		t.Errorf("flood leaked %d of 200 slots, want well under 10%%", leaked)
	}
	for i := 0; i < 3; i++ {
		g.Admit("/real", now)
	}
	if _, ok := g.Peek("/real"); !ok {
		t.Error("real traffic must still be admitted after a heavy flood")
	}
}

func TestGuardEnforcesCap(t *testing.T) {
	g := NewGuard(3, 1)
	now := time.Now()

	for i := 0; i < 20; i++ {
		g.Admit(fmt.Sprintf("/p%d", i), now)
	}
	if g.Len() != 3 {
		t.Errorf("admitted = %d, want exactly the cap of 3", g.Len())
	}
	if g.Capped() == 0 {
		t.Error("overflow should be counted so the cap is alertable")
	}
}

func TestGuardEmptyValueIsNone(t *testing.T) {
	g := NewGuard(10, 1)
	if got := g.Admit("", time.Now()); got != NoneLabel {
		t.Errorf("empty value = %q, want %s", got, NoneLabel)
	}
	// __none__ (genuinely absent) must stay distinct from __other__ (capped),
	// or you cannot tell a broken geo database from a saturated cap.
	if NoneLabel == OtherLabel {
		t.Fatal("the absent and capped sentinels must differ")
	}
}

func TestGuardPeekDoesNotAdmit(t *testing.T) {
	g := NewGuard(10, 1)
	for i := 0; i < 10; i++ {
		if _, ok := g.Peek("/a"); ok {
			t.Fatal("Peek admitted a value")
		}
	}
	if g.Len() != 0 {
		t.Errorf("Peek changed state: %d admitted", g.Len())
	}
}

func TestGuardDecayReleasesSlots(t *testing.T) {
	g := NewGuard(2, 1)
	base := time.Now()

	g.Admit("/old", base)
	g.Admit("/also-old", base)
	if g.Len() != 2 {
		t.Fatalf("setup: admitted = %d", g.Len())
	}

	dropped := g.Decay(base.Add(8*24*time.Hour), 7*24*time.Hour)
	if len(dropped) != 2 {
		t.Errorf("dropped %d values, want 2", len(dropped))
	}
	if g.Len() != 0 {
		t.Errorf("admitted = %d after decay, want 0", g.Len())
	}

	// The freed slots are usable by whatever is popular now.
	g.Admit("/new", base.Add(8*24*time.Hour))
	if _, ok := g.Peek("/new"); !ok {
		t.Error("a freed slot was not reusable")
	}
}

func TestGuardDecayKeepsActiveValues(t *testing.T) {
	g := NewGuard(10, 1)
	base := time.Now()

	g.Admit("/busy", base)
	g.Admit("/busy", base.Add(6*24*time.Hour)) // seen recently

	if dropped := g.Decay(base.Add(7*24*time.Hour+time.Hour), 7*24*time.Hour); len(dropped) != 0 {
		t.Errorf("dropped %v; a recently-seen value must survive decay", dropped)
	}
}

// TestSharedCounterGivesOneDecision: two guards sharing a frequency counter
// must agree about what is popular. This is what keeps every path-labelled
// family consistent about which paths exist.
func TestSharedCounterGivesOneDecision(t *testing.T) {
	shared := newCounter(4, 2048)
	pure := NewGuardWith(100, 3, shared)
	cross := NewGuardWith(100, 3, shared)
	now := time.Now()

	// Each guard sees the value only once, but the shared counter tallies both.
	pure.Admit("/a", now)
	cross.Admit("/a", now)
	pure.Admit("/a", now)

	// The third observation overall clears the threshold for the next caller.
	if got := cross.Admit("/a", now); got != "/a" {
		t.Errorf("cross guard = %q; a shared counter should have admitted it", got)
	}
}

func TestGuardIsConcurrencySafe(t *testing.T) {
	g := NewGuard(50, 2)
	now := time.Now()

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				g.Admit(fmt.Sprintf("/p%d", i%100), now)
				if i%100 == 0 {
					g.Len()
					g.Capped()
					g.Peek("/p1")
				}
			}
		}(w)
	}
	wg.Wait()

	if g.Len() > 50 {
		t.Errorf("cap exceeded under concurrency: %d admitted", g.Len())
	}
}

func TestCounterEstimatesFrequency(t *testing.T) {
	c := newCounter(4, 2048)

	var last uint32
	for i := 0; i < 10; i++ {
		last = c.add("x")
	}
	if last < 10 {
		t.Errorf("estimate = %d after 10 increments, want at least 10", last)
	}
	// Count-Min never under-counts, which is the direction that matters:
	// it can admit early, never suppress something genuinely frequent.
	if got := c.add("never-seen-before"); got < 1 {
		t.Errorf("estimate = %d for a first sighting, want at least 1", got)
	}

	c.reset()
	if got := c.add("x"); got != 1 {
		t.Errorf("estimate = %d after reset, want 1", got)
	}
}
