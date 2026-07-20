package sketch

import (
	"sync"
	"testing"
	"time"
)

// recorder captures session lifecycle events for assertions.
type recorder struct {
	mu      sync.Mutex
	started []string
	ended   []Ended
}

func (r *recorder) SessionStarted(_, entryPath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, entryPath)
}

func (r *recorder) SessionEnded(_ string, e Ended) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, e)
}

func (r *recorder) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started), len(r.ended)
}

func newTestSessions(t *testing.T, ttl time.Duration, capacity int) (*Sessions, *recorder) {
	t.Helper()
	rec := &recorder{}
	return NewSessions("example.com", ttl, capacity, rec), rec
}

func TestSessionStartsOnFirstPageview(t *testing.T) {
	s, rec := newTestSessions(t, 30*time.Minute, 100)
	now := time.Now()

	if !s.Touch(1, "/", now) {
		t.Error("first pageview should start a session")
	}
	if s.Touch(1, "/next", now.Add(time.Minute)) {
		t.Error("second pageview within the TTL should not start a new session")
	}
	if started, _ := rec.counts(); started != 1 {
		t.Errorf("started = %d, want 1", started)
	}
	if len(rec.started) > 0 && rec.started[0] != "/" {
		t.Errorf("entry path = %q, want /", rec.started[0])
	}
	if s.Len() != 1 {
		t.Errorf("live sessions = %d, want 1", s.Len())
	}
}

func TestSessionBounceAndExit(t *testing.T) {
	s, rec := newTestSessions(t, 30*time.Minute, 100)
	base := time.Now()

	s.Touch(1, "/landing", base)              // bounces
	s.Touch(2, "/a", base)                    // reads on
	s.Touch(2, "/b", base.Add(2*time.Minute)) //
	s.Sweep(base.Add(time.Hour))

	_, ended := rec.counts()
	if ended != 2 {
		t.Fatalf("ended = %d, want 2", ended)
	}

	byExit := map[string]Ended{}
	for _, e := range rec.ended {
		byExit[e.ExitPath] = e
	}

	bounce, ok := byExit["/landing"]
	if !ok {
		t.Fatal("missing the single-page session")
	}
	if !bounce.Bounced || bounce.PageCount != 1 {
		t.Errorf("single-pageview session should be a bounce, got %+v", bounce)
	}

	engaged, ok := byExit["/b"]
	if !ok {
		t.Fatal("missing the two-page session")
	}
	if engaged.Bounced {
		t.Error("two-pageview session must not be a bounce")
	}
	if engaged.PageCount != 2 {
		t.Errorf("page count = %d, want 2", engaged.PageCount)
	}
	if engaged.EntryPath != "/a" || engaged.ExitPath != "/b" {
		t.Errorf("entry/exit = %s/%s, want /a and /b", engaged.EntryPath, engaged.ExitPath)
	}
	if engaged.Duration != 2*time.Minute {
		t.Errorf("duration = %v, want 2m", engaged.Duration)
	}
	if s.Len() != 0 {
		t.Errorf("sweep should have emptied the map, %d remain", s.Len())
	}
}

// TestIdleVisitorStartsFreshSession checks that returning after the TTL opens a
// new session rather than silently resurrecting a stale one.
func TestIdleVisitorStartsFreshSession(t *testing.T) {
	s, rec := newTestSessions(t, 30*time.Minute, 100)
	base := time.Now()

	s.Touch(1, "/first", base)
	if !s.Touch(1, "/second", base.Add(2*time.Hour)) {
		t.Error("a visit after the idle TTL should start a new session")
	}

	started, ended := rec.counts()
	if started != 2 {
		t.Errorf("started = %d, want 2", started)
	}
	if ended != 1 {
		t.Errorf("ended = %d, want 1 (the stale session must be closed out)", ended)
	}
	if len(rec.ended) > 0 && !rec.ended[0].Bounced {
		t.Error("the abandoned single-page session should count as a bounce")
	}
}

func TestExtendKeepsSessionAlive(t *testing.T) {
	s, rec := newTestSessions(t, 30*time.Minute, 100)
	base := time.Now()

	s.Touch(1, "/article", base)
	// An engagement ping 20 minutes in pushes the idle deadline out.
	s.Extend(1, base.Add(20*time.Minute))
	s.Sweep(base.Add(40 * time.Minute))

	if _, ended := rec.counts(); ended != 0 {
		t.Error("an extended session must not be swept while still inside the TTL")
	}

	s.Sweep(base.Add(2 * time.Hour))
	if _, ended := rec.counts(); ended != 1 {
		t.Error("the session should be swept once genuinely idle")
	}
	// Duration should reflect the ping, not just the pageview.
	if got := rec.ended[0].Duration; got != 20*time.Minute {
		t.Errorf("duration = %v, want 20m (engagement should extend it)", got)
	}
}

// TestExtendIgnoresUnknownAndExpired guards against an engagement ping
// resurrecting a session that has already ended.
func TestExtendIgnoresUnknownAndExpired(t *testing.T) {
	s, _ := newTestSessions(t, 30*time.Minute, 100)
	now := time.Now()

	s.Extend(999, now) // never seen
	if s.Len() != 0 {
		t.Error("Extend must not create a session")
	}

	s.Touch(1, "/", now)
	s.Extend(1, now.Add(2*time.Hour)) // past the TTL
	s.Sweep(now.Add(3 * time.Hour))
	if s.Len() != 0 {
		t.Error("an expired session must still be sweepable")
	}
}

// TestCapEvictsOldestAndStillReports checks that memory pressure degrades
// totals gracefully: evicted sessions are reported, not silently dropped.
func TestCapEvictsOldestAndStillReports(t *testing.T) {
	s, rec := newTestSessions(t, 30*time.Minute, 5)
	base := time.Now()

	for i := 0; i < 20; i++ {
		s.Touch(uint64(i), "/p", base.Add(time.Duration(i)*time.Second))
	}

	if s.Len() > 5 {
		t.Errorf("live sessions = %d, want at most the cap of 5", s.Len())
	}
	if s.Evicted() == 0 {
		t.Error("expected evictions to be counted")
	}
	started, ended := rec.counts()
	if started != 20 {
		t.Errorf("started = %d, want 20", started)
	}
	// Every session must be accounted for: still live, or reported as ended.
	if ended+s.Len() != started {
		t.Errorf("%d ended + %d live != %d started; sessions are being lost silently",
			ended, s.Len(), started)
	}
}

func TestSessionsAreConcurrencySafe(t *testing.T) {
	s, _ := newTestSessions(t, 30*time.Minute, 1000)
	now := time.Now()

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				id := uint64(base*500 + i)
				s.Touch(id, "/p", now)
				s.Extend(id, now)
				if i%50 == 0 {
					s.Sweep(now)
					s.Len()
				}
			}
		}(w)
	}
	wg.Wait()
}
