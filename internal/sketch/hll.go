// Package sketch holds the in-memory probabilistic structures: rolling
// HyperLogLog rings for unique visitors, and the TTL session map.
package sketch

import (
	"sync"
	"time"

	"github.com/axiomhq/hyperloglog"
)

// Rolling estimates distinct visitors over a sliding window using a ring of
// sub-sketches. Each bucket covers `res`; the window is res*len(buckets).
// Estimating merges the live buckets, so the window slides continuously
// instead of resetting on a boundary.
type Rolling struct {
	mu      sync.Mutex
	res     time.Duration
	buckets []*hyperloglog.Sketch
	stamps  []int64 // bucket index -> epoch slot it currently holds
	sparse  bool
}

// NewRolling builds a ring covering res*n with the given resolution.
// sparse=true selects the memory-efficient sparse representation, which is
// what makes a per-path sketch affordable.
func NewRolling(res time.Duration, n int, sparse bool) *Rolling {
	r := &Rolling{
		res:     res,
		buckets: make([]*hyperloglog.Sketch, n),
		stamps:  make([]int64, n),
		sparse:  sparse,
	}
	for i := range r.buckets {
		r.buckets[i] = r.newSketch()
		r.stamps[i] = -1
	}
	return r
}

func (r *Rolling) newSketch() *hyperloglog.Sketch {
	if r.sparse {
		// p=10: ~1KB dense, ~3% error. Enough to rank, not to report.
		s, err := hyperloglog.NewSketch(10, true)
		if err != nil {
			return hyperloglog.New14()
		}
		return s
	}
	return hyperloglog.New14()
}

// slot returns the epoch slot for t.
func (r *Rolling) slot(t time.Time) int64 { return t.UnixNano() / int64(r.res) }

// mix is the splitmix64 finalizer.
//
// HyperLogLog derives its estimate from the leading-zero count of what it
// assumes is an already-uniform hash. Feeding it structured input — sequential
// ids, counters, anything clustered — collapses the estimate to roughly
// log2(n) with no error or warning of any kind.
//
// Today's caller passes a truncated HMAC-SHA256, which is uniform, so this is
// insurance rather than a fix. It is worth the two nanoseconds: the failure
// mode is silent, produces plausible-looking small numbers, and would be
// extremely hard to spot in a dashboard.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// Insert records a visitor id at time t.
func (r *Rolling) Insert(id uint64, t time.Time) {
	s := r.slot(t)
	i := int(s % int64(len(r.buckets)))

	r.mu.Lock()
	defer r.mu.Unlock()
	// A stale stamp means this ring position belongs to an older window and
	// must be recycled before use.
	if r.stamps[i] != s {
		r.buckets[i] = r.newSketch()
		r.stamps[i] = s
	}
	r.buckets[i].InsertHash(mix(id))
}

// Estimate merges every bucket still inside the window ending at t.
func (r *Rolling) Estimate(t time.Time) uint64 {
	cur := r.slot(t)
	oldest := cur - int64(len(r.buckets)) + 1

	r.mu.Lock()
	defer r.mu.Unlock()

	acc := r.newSketch()
	any := false
	for i, st := range r.stamps {
		if st < oldest || st > cur {
			continue // expired or not yet populated
		}
		if err := acc.Merge(r.buckets[i]); err == nil {
			any = true
		}
	}
	if !any {
		return 0
	}
	return acc.Estimate()
}

// Window is the total span covered by the ring.
func (r *Rolling) Window() time.Duration { return r.res * time.Duration(len(r.buckets)) }

// Windows bundles the standard set of unique-visitor windows for one site.
type Windows struct {
	Active *Rolling // 5m, for "right now"
	H1     *Rolling
	D1     *Rolling
	D7     *Rolling
	D30    *Rolling
}

// NewWindows builds the standard ring set. Memory is roughly
// (12+24+24+7+30) x 12KB ~= 1.2MB per site.
func NewWindows() *Windows {
	return &Windows{
		Active: NewRolling(time.Minute, 5, false),
		H1:     NewRolling(5*time.Minute, 12, false),
		D1:     NewRolling(time.Hour, 24, false),
		D7:     NewRolling(24*time.Hour, 7, false),
		D30:    NewRolling(24*time.Hour, 30, false),
	}
}

// Insert feeds a visitor id into every window.
func (w *Windows) Insert(id uint64, t time.Time) {
	w.Active.Insert(id, t)
	w.H1.Insert(id, t)
	w.D1.Insert(id, t)
	w.D7.Insert(id, t)
	w.D30.Insert(id, t)
}
