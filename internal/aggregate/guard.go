package aggregate

import (
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
)

// Label sentinels.
const (
	// OtherLabel absorbs every value that has not earned its own series.
	OtherLabel = "__other__"
	// AllLabel is used for the path label on families where path is disabled,
	// so a metric keeps a consistent label set across sites.
	AllLabel = "__all__"
	// NoneLabel marks a dimension that is genuinely absent (no referrer, no
	// geo database), which is different from "capped away".
	NoneLabel = "__none__"
)

// counter is a Count-Min Sketch over label values.
//
// Its purpose is admission control, not accuracy: a naive "first N values win"
// cap lets one crawler hitting /?id=<uuid> permanently occupy every slot in
// the first second. Requiring a minimum observed frequency means real traffic
// earns series and one-shot noise stays folded into __other__.
type counter struct {
	mu    sync.Mutex
	depth int
	width uint64
	cells []uint32

	// adds since the last halving, and the interval at which to halve.
	adds  uint64
	epoch uint64
}

func newCounter(depth int, width uint64) *counter {
	return &counter{
		depth: depth,
		width: width,
		cells: make([]uint32, uint64(depth)*width),
		// Halve once every `width` additions. The noise floor of a Count-Min
		// Sketch is roughly (additions / width), so this keeps it near 1 and
		// well under any sane minimum count.
		epoch: width,
	}
}

// halve ages every counter, turning the sketch into an estimate of *recent*
// frequency rather than an all-time tally.
//
// Without this the sketch is defeated by the very attack it exists to stop: a
// flood of unique values raises every cell through hash collisions until the
// minimum estimate for a never-before-seen value exceeds the admission
// threshold, at which point one-shot garbage sails straight into the cap.
// Halving rather than zeroing lets a genuinely popular value carry its history
// across epochs at reduced weight.
//
// Caller must hold the mutex.
func (c *counter) halve() {
	for i := range c.cells {
		c.cells[i] >>= 1
	}
	c.adds = 0
}

// add increments the value and returns its estimated count.
//
// This uses conservative update: only the cells already at the minimum are
// incremented. A plain Count-Min raises every cell on every add, so cells
// shared with a heavy hitter keep inflating and unrelated rare values inherit
// that inflation — which is precisely how a flood of one-shot values sneaks
// past the admission threshold. Conservative update never over-counts more
// than plain CMS and in practice is dramatically tighter.
//
// The estimate remains an upper bound, which is the right direction to err: it
// can admit a rare value early, never suppress a genuinely frequent one.
func (c *counter) add(v string) uint32 {
	h := xxhash.Sum64String(v)
	h1, h2 := h, h>>32|h<<32

	c.mu.Lock()
	defer c.mu.Unlock()

	c.adds++
	if c.adds >= c.epoch {
		c.halve()
	}

	var idx [8]uint64
	depth := c.depth
	if depth > len(idx) {
		depth = len(idx)
	}

	min := ^uint32(0)
	for i := 0; i < depth; i++ {
		idx[i] = uint64(i)*c.width + (h1+uint64(i)*h2)%c.width
		if v := c.cells[idx[i]]; v < min {
			min = v
		}
	}
	if min == ^uint32(0) {
		return min // saturated; refuse to wrap
	}
	for i := 0; i < depth; i++ {
		if c.cells[idx[i]] == min {
			c.cells[idx[i]] = min + 1
		}
	}
	return min + 1
}

// reset clears the sketch. Called alongside decay so that a value which has
// gone quiet cannot coast forever on counts accumulated months ago.
func (c *counter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.cells {
		c.cells[i] = 0
	}
}

// Guard bounds the distinct label values a single dimension may produce.
type Guard struct {
	cap      int
	minCount uint32
	cnt      *counter

	mu       sync.Mutex
	admitted map[string]time.Time // value -> last seen
	capped   uint64
}

// NewGuard creates a guard with its own frequency counter.
func NewGuard(capacity int, minCount uint32) *Guard {
	return NewGuardWith(capacity, minCount, newCounter(4, 4096))
}

// NewGuardWith creates a guard sharing an existing frequency counter.
//
// This is how path admission stays global: one counter and one decision feed
// every family that carries a path label. Per-family admission would let /a be
// tracked in by_country but folded in by_device, so the families would
// disagree about which paths exist — confusing to query and pointless to
// compute.
func NewGuardWith(capacity int, minCount uint32, c *counter) *Guard {
	if capacity < 1 {
		capacity = 1
	}
	if minCount < 1 {
		minCount = 1
	}
	return &Guard{
		cap:      capacity,
		minCount: minCount,
		cnt:      c,
		admitted: make(map[string]time.Time, capacity),
	}
}

// Admit maps a raw value to the label value that should be used. It returns
// the value itself if admitted, or OtherLabel if it was folded.
//
// Counting is unconditional and happens before the admitted-set check, so a
// value's frequency keeps accruing while it is folded and it can be promoted
// once it proves itself.
func (g *Guard) Admit(v string, now time.Time) string {
	if v == "" {
		return NoneLabel
	}
	est := g.cnt.add(v)

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.admitted[v]; ok {
		g.admitted[v] = now
		return v
	}
	if est >= g.minCount && len(g.admitted) < g.cap {
		g.admitted[v] = now
		return v
	}
	g.capped++
	return OtherLabel
}

// Peek reports the label a value would map to without recording anything.
// Used by lookups that must not influence admission, such as deciding whether
// a per-path sketch already exists.
func (g *Guard) Peek(v string) (string, bool) {
	if v == "" {
		return NoneLabel, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.admitted[v]; ok {
		return v, true
	}
	return OtherLabel, false
}

// Decay removes values not seen within the retention window and returns them
// so the caller can delete the corresponding series. Freeing slots lets a
// site's metric shape follow its actual current traffic instead of freezing
// on whatever it looked like at boot.
func (g *Guard) Decay(now time.Time, after time.Duration) []string {
	g.mu.Lock()
	var dropped []string
	for v, seen := range g.admitted {
		if now.Sub(seen) > after {
			dropped = append(dropped, v)
			delete(g.admitted, v)
		}
	}
	g.mu.Unlock()

	if len(dropped) > 0 {
		g.cnt.reset()
	}
	return dropped
}

// Len reports admitted values; Capped reports folded observations.
func (g *Guard) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.admitted)
}

func (g *Guard) Capped() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.capped
}
