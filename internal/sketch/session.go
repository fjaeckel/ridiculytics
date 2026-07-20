package sketch

import (
	"sync"
	"time"
)

// Session is one visitor's in-flight activity. It exists only between the
// first pageview and the idle timeout; nothing is persisted.
type Session struct {
	Start     time.Time
	LastSeen  time.Time
	PageCount int
	EntryPath string
	ExitPath  string
}

// Ended describes a session at the moment it times out.
type Ended struct {
	Duration  time.Duration
	PageCount int
	EntryPath string
	ExitPath  string
	Bounced   bool
}

// Sink receives session lifecycle events so the aggregate layer can turn them
// into metrics. Implementations must be safe for concurrent use.
type Sink interface {
	SessionStarted(site, entryPath string)
	SessionEnded(site string, e Ended)
}

// Sessions is the TTL-bounded session map for one site.
type Sessions struct {
	site string
	ttl  time.Duration
	cap  int
	sink Sink

	mu sync.Mutex
	m  map[uint64]*Session

	evicted uint64
}

// NewSessions creates a session tracker. cap bounds memory; exceeding it
// evicts the least-recently-seen sessions, which are also the ones whose
// data matters least.
func NewSessions(site string, ttl time.Duration, capacity int, sink Sink) *Sessions {
	return &Sessions{
		site: site,
		ttl:  ttl,
		cap:  capacity,
		sink: sink,
		m:    make(map[uint64]*Session),
	}
}

// Touch records a pageview for a visitor. It returns true if this started a
// new session. Callers must supply the already-normalized path.
func (s *Sessions) Touch(id uint64, path string, now time.Time) bool {
	s.mu.Lock()

	if cur, ok := s.m[id]; ok && now.Sub(cur.LastSeen) < s.ttl {
		cur.PageCount++
		cur.LastSeen = now
		cur.ExitPath = path
		s.mu.Unlock()
		return false
	} else if ok {
		// Idle past the TTL: close the old one and open a fresh session,
		// rather than silently resurrecting a stale one.
		ended := finish(cur)
		delete(s.m, id)
		s.mu.Unlock()
		s.sink.SessionEnded(s.site, ended)
		s.mu.Lock()
	}

	s.m[id] = &Session{
		Start: now, LastSeen: now, PageCount: 1,
		EntryPath: path, ExitPath: path,
	}
	over := len(s.m) > s.cap
	s.mu.Unlock()

	s.sink.SessionStarted(s.site, path)
	if over {
		s.evictOldest(now)
	}
	return true
}

// Extend applies a client-reported engagement ping, keeping a session alive
// and letting duration reflect real time on the last page.
func (s *Sessions) Extend(id uint64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.m[id]; ok && now.Sub(cur.LastSeen) < s.ttl {
		cur.LastSeen = now
	}
}

// Sweep closes every session idle for longer than the TTL.
func (s *Sessions) Sweep(now time.Time) int {
	var done []Ended

	s.mu.Lock()
	for id, cur := range s.m {
		if now.Sub(cur.LastSeen) >= s.ttl {
			done = append(done, finish(cur))
			delete(s.m, id)
		}
	}
	s.mu.Unlock()

	for _, e := range done {
		s.sink.SessionEnded(s.site, e)
	}
	return len(done)
}

// evictOldest drops the least-recently-seen sessions until back under cap.
// Evicted sessions are still reported, so totals stay believable under
// pressure rather than silently sagging.
func (s *Sessions) evictOldest(now time.Time) {
	var done []Ended

	s.mu.Lock()
	for len(s.m) > s.cap {
		var oldestID uint64
		var oldest time.Time
		first := true
		for id, cur := range s.m {
			if first || cur.LastSeen.Before(oldest) {
				oldestID, oldest, first = id, cur.LastSeen, false
			}
		}
		if first {
			break
		}
		done = append(done, finish(s.m[oldestID]))
		delete(s.m, oldestID)
		s.evicted++
	}
	s.mu.Unlock()

	for _, e := range done {
		s.sink.SessionEnded(s.site, e)
	}
}

// Len reports live sessions.
func (s *Sessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// Evicted reports how many sessions were dropped for capacity.
func (s *Sessions) Evicted() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evicted
}

func finish(c *Session) Ended {
	return Ended{
		Duration:  c.LastSeen.Sub(c.Start),
		PageCount: c.PageCount,
		EntryPath: c.EntryPath,
		ExitPath:  c.ExitPath,
		Bounced:   c.PageCount == 1,
	}
}
