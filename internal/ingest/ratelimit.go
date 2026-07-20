package ingest

import (
	"sync"
	"time"
)

// Limiter is a token bucket keyed by string, with bounded memory.
type Limiter struct {
	rate  float64 // tokens per second
	burst float64
	max   int // maximum tracked keys

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLimiter allows perMin requests per key per minute, bursting to perMin.
func NewLimiter(perMin int, maxKeys int) *Limiter {
	if perMin < 1 {
		perMin = 1
	}
	return &Limiter{
		rate:    float64(perMin) / 60.0,
		burst:   float64(perMin),
		max:     maxKeys,
		buckets: make(map[string]*bucket),
	}
}

// Allow consumes a token for key.
func (l *Limiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		// The map itself is an attack surface: an attacker with many source
		// addresses could grow it without bound. Once full, refuse new keys
		// rather than allocating — failing closed is correct for a limiter.
		if len(l.buckets) >= l.max {
			return false
		}
		l.buckets[key] = &bucket{tokens: l.burst - 1, last: now}
		return true
	}

	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Sweep drops buckets that have been idle long enough to be fully refilled,
// since a full bucket is indistinguishable from a fresh one.
func (l *Limiter) Sweep(now time.Time) {
	idle := time.Duration(float64(time.Second) * (l.burst / l.rate))

	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if now.Sub(b.last) > idle {
			delete(l.buckets, k)
		}
	}
}

// Len reports tracked keys.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
