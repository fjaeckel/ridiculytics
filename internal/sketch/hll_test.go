package sketch

import (
	"math"
	"testing"
	"time"
)

func TestRollingCountsDistinctVisitors(t *testing.T) {
	r := NewRolling(time.Hour, 24, false)
	now := time.Now()

	const n = 10_000
	for i := 0; i < n; i++ {
		r.Insert(uint64(i), now)
	}
	// Re-inserting the same ids must not inflate the estimate.
	for i := 0; i < n; i++ {
		r.Insert(uint64(i), now)
	}

	got := r.Estimate(now)
	if relErr := math.Abs(float64(got)-n) / n; relErr > 0.02 {
		t.Errorf("estimate = %d for %d distinct ids (%.2f%% error, want <2%%)",
			got, n, relErr*100)
	}
}

func TestRollingIsEmptyBeforeAnyInsert(t *testing.T) {
	r := NewRolling(time.Minute, 5, false)
	if got := r.Estimate(time.Now()); got != 0 {
		t.Errorf("estimate = %d on a fresh sketch, want 0", got)
	}
}

// TestRollingWindowSlides is the property that makes these gauges meaningful:
// visitors must age out of the window rather than accumulating forever.
func TestRollingWindowSlides(t *testing.T) {
	r := NewRolling(time.Hour, 3, false) // 3-hour window
	base := time.Now().Truncate(time.Hour)

	r.Insert(1, base)
	r.Insert(2, base.Add(time.Hour))
	r.Insert(3, base.Add(2*time.Hour))

	if got := r.Estimate(base.Add(2 * time.Hour)); got != 3 {
		t.Errorf("all three should be inside the window, got %d", got)
	}

	// Four hours on, the first two buckets have fallen out.
	if got := r.Estimate(base.Add(4 * time.Hour)); got != 1 {
		t.Errorf("estimate = %d four hours later, want 1 (older buckets must expire)", got)
	}

	// Far enough ahead and everything has expired.
	if got := r.Estimate(base.Add(24 * time.Hour)); got != 0 {
		t.Errorf("estimate = %d a day later, want 0", got)
	}
}

// TestRollingRecyclesStaleBuckets guards a subtle ring-buffer bug: a slot
// reused a full cycle later must be cleared, not merged with its old contents.
func TestRollingRecyclesStaleBuckets(t *testing.T) {
	r := NewRolling(time.Hour, 3, false)
	base := time.Now().Truncate(time.Hour)

	for i := 0; i < 3; i++ {
		r.Insert(uint64(100+i), base.Add(time.Duration(i)*time.Hour))
	}
	// One full cycle later, the same ring slot comes round again.
	later := base.Add(3 * time.Hour)
	r.Insert(999, later)

	if got := r.Estimate(later); got != 3 {
		// Window covers hours 1,2,3: ids 101, 102 and 999.
		t.Errorf("estimate = %d, want 3; a recycled slot is leaking old ids", got)
	}
}

func TestRollingSparseUsesLessMemory(t *testing.T) {
	sparse := NewRolling(time.Hour, 24, true)
	now := time.Now()
	for i := 0; i < 500; i++ {
		sparse.Insert(uint64(i), now)
	}
	got := sparse.Estimate(now)
	if relErr := math.Abs(float64(got)-500) / 500; relErr > 0.05 {
		t.Errorf("sparse estimate = %d for 500 ids (%.2f%% error, want <5%%)", got, relErr*100)
	}
}

func TestWindowsInsertFeedsEveryWindow(t *testing.T) {
	w := NewWindows()
	now := time.Now()
	for i := 0; i < 100; i++ {
		w.Insert(uint64(i), now)
	}
	for name, r := range map[string]*Rolling{
		"active": w.Active, "1h": w.H1, "24h": w.D1, "7d": w.D7, "30d": w.D30,
	} {
		got := r.Estimate(now)
		if relErr := math.Abs(float64(got)-100) / 100; relErr > 0.05 {
			t.Errorf("window %s estimate = %d, want ~100", name, got)
		}
	}
}

func TestRollingWindowDuration(t *testing.T) {
	if got := NewRolling(time.Hour, 24, false).Window(); got != 24*time.Hour {
		t.Errorf("Window() = %v, want 24h", got)
	}
}

func TestRollingIsConcurrencySafe(t *testing.T) {
	r := NewRolling(time.Minute, 5, false)
	now := time.Now()
	done := make(chan struct{})

	for w := 0; w < 8; w++ {
		go func(base int) {
			for i := 0; i < 1000; i++ {
				r.Insert(uint64(base*1000+i), now)
				if i%100 == 0 {
					r.Estimate(now)
				}
			}
			done <- struct{}{}
		}(w)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	// 8000 distinct ids; assert it is in a believable range rather than exact.
	if got := r.Estimate(now); got < 7000 || got > 9000 {
		t.Errorf("estimate = %d after concurrent inserts, want ~8000", got)
	}
}
