package ingest

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fjaeckel/ridiculytics/internal/aggregate"
	"github.com/fjaeckel/ridiculytics/internal/config"
	"github.com/fjaeckel/ridiculytics/internal/geo"
)

const testYAML = `
server:
  ingest_addr: ":0"
  metrics_addr: ":0"
  rate_per_min: 1000
  workers: 1
defaults:
  admit_min_count: 1
sites:
  - domain: example.com
    origins: ["https://example.com"]
`

func newTestServer(t *testing.T, yaml string) (*Server, *aggregate.Registry) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sites.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	reg := aggregate.New(store.Get())
	salt, err := NewSalt(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Options{Registry: reg, Config: store, Geo: geo.Null{}, Salt: salt})
	t.Cleanup(srv.Close)
	return srv, reg
}

func post(t *testing.T, srv *Server, body, origin, ua string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/event", strings.NewReader(body))
	r.Header.Set("Content-Type", "text/plain")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.Header.Set("User-Agent", ua)
	r.RemoteAddr = "203.0.113.7:54321"

	w := httptest.NewRecorder()
	srv.Routes(nil, false).ServeHTTP(w, r)
	return w
}

const chromeUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0 Safari/537.36"

// drain waits for the async workers to finish aggregating.
func drain(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(srv.queue) == 0 {
			time.Sleep(20 * time.Millisecond)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("queue did not drain")
}

func metricValue(t *testing.T, reg *aggregate.Registry, name string, want map[string]string) float64 {
	t.Helper()
	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	metric:
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			for k, v := range want {
				if labels[k] != v {
					continue metric
				}
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
			if g := m.GetGauge(); g != nil {
				return g.GetValue()
			}
		}
	}
	return 0
}

func TestIngestPageviewEndToEnd(t *testing.T) {
	srv, reg := newTestServer(t, testYAML)

	body := `{"n":"pageview","d":"example.com","u":"https://example.com/blog/post?utm_source=hn",` +
		`"r":"https://news.ycombinator.com/item?id=1","w":1920}`
	if w := post(t, srv, body, "https://example.com", chromeUA); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	drain(t, srv)

	checks := []struct {
		metric string
		labels map[string]string
	}{
		{"ridiculytics_pageviews_total", map[string]string{"path": "/blog/post"}},
		{"ridiculytics_pageviews_by_referrer_total", map[string]string{"referrer": "news.ycombinator.com"}},
		{"ridiculytics_pageviews_by_source_total", map[string]string{"source": "hn"}},
		{"ridiculytics_pageviews_by_browser_total", map[string]string{"browser": "Chrome"}},
		{"ridiculytics_pageviews_by_os_total", map[string]string{"os": "macOS"}},
		{"ridiculytics_pageviews_by_device_total", map[string]string{"device": "desktop"}},
		{"ridiculytics_pageviews_by_screen_total", map[string]string{"class": "xl"}},
		{"ridiculytics_sessions_total", map[string]string{"site": "example.com"}},
	}
	for _, c := range checks {
		if v := metricValue(t, reg, c.metric, c.labels); v != 1 {
			t.Errorf("%s%v = %v, want 1", c.metric, c.labels, v)
		}
	}

	// The query string must not have survived into the path label.
	if v := metricValue(t, reg, "ridiculytics_pageviews_total",
		map[string]string{"path": "/blog/post?utm_source=hn"}); v != 0 {
		t.Error("query string leaked into the path label")
	}
}

func TestOriginAllowlistIsEnforced(t *testing.T) {
	srv, reg := newTestServer(t, testYAML)
	body := `{"n":"pageview","d":"example.com","u":"https://example.com/"}`

	if w := post(t, srv, body, "https://evil.example", chromeUA); w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	drain(t, srv)

	if v := metricValue(t, reg, "ridiculytics_ingest_events_total",
		map[string]string{"result": "rejected_origin"}); v != 1 {
		t.Errorf("rejected_origin count = %v, want 1", v)
	}
	if v := metricValue(t, reg, "ridiculytics_pageviews_total", map[string]string{"path": "/"}); v != 0 {
		t.Error("event from disallowed origin was counted")
	}
}

func TestUnknownSiteIsRejected(t *testing.T) {
	srv, _ := newTestServer(t, testYAML)
	body := `{"n":"pageview","d":"notmine.example","u":"https://notmine.example/"}`
	if w := post(t, srv, body, "https://notmine.example", chromeUA); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestBotsAreCountedNotSilentlyDropped(t *testing.T) {
	srv, reg := newTestServer(t, testYAML)
	body := `{"n":"pageview","d":"example.com","u":"https://example.com/"}`

	post(t, srv, body, "https://example.com", "Mozilla/5.0 (compatible; Googlebot/2.1)")
	drain(t, srv)

	if v := metricValue(t, reg, "ridiculytics_ingest_events_total",
		map[string]string{"result": "bot"}); v != 1 {
		t.Errorf("bot count = %v, want 1 (silent drops make traffic gaps undebuggable)", v)
	}
	if v := metricValue(t, reg, "ridiculytics_pageviews_total", map[string]string{"path": "/"}); v != 0 {
		t.Error("bot traffic was counted as a pageview")
	}
}

func TestMalformedPayloadsAreRejected(t *testing.T) {
	srv, _ := newTestServer(t, testYAML)
	cases := []struct {
		name, body string
		want       int
	}{
		{"not json", `{{{`, http.StatusBadRequest},
		{"missing url", `{"n":"pageview","d":"example.com"}`, http.StatusBadRequest},
		{"missing name", `{"d":"example.com","u":"https://example.com/"}`, http.StatusBadRequest},
		{"unknown field", `{"n":"pageview","d":"example.com","u":"https://example.com/","x":1}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if w := post(t, srv, c.body, "https://example.com", chromeUA); w.Code != c.want {
				t.Errorf("status = %d, want %d", w.Code, c.want)
			}
		})
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	srv, _ := newTestServer(t, testYAML)
	huge := `{"n":"pageview","d":"example.com","u":"https://example.com/` + strings.Repeat("a", 8192) + `"}`
	if w := post(t, srv, huge, "https://example.com", chromeUA); w.Code < 400 {
		t.Errorf("status = %d, want a 4xx for an oversized body", w.Code)
	}
}

func TestRateLimiting(t *testing.T) {
	yaml := strings.Replace(testYAML, "rate_per_min: 1000", "rate_per_min: 5", 1)
	srv, reg := newTestServer(t, yaml)
	body := `{"n":"pageview","d":"example.com","u":"https://example.com/"}`

	limited := 0
	for i := 0; i < 20; i++ {
		if w := post(t, srv, body, "https://example.com", chromeUA); w.Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("rate limiter never engaged")
	}
	drain(t, srv)
	if v := metricValue(t, reg, "ridiculytics_ingest_events_total",
		map[string]string{"result": "rate_limited"}); v != float64(limited) {
		t.Errorf("rate_limited metric = %v, want %d", v, limited)
	}
}

// TestCORSPreflight verifies the browser can actually talk to us.
func TestCORSPreflight(t *testing.T) {
	srv, _ := newTestServer(t, testYAML)
	r := httptest.NewRequest(http.MethodOptions, "/api/event", nil)
	r.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	srv.Routes(nil, false).ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("ACAO = %q, want the request origin", got)
	}
}

// TestVisitorIDIsStableWithinASaltAndUnlinkableAcross checks the privacy
// primitive: same visitor hashes consistently today, and rotating the salt
// makes yesterday's identifiers unlinkable.
func TestVisitorIDIsStableWithinASaltAndUnlinkableAcross(t *testing.T) {
	salt, err := NewSalt(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ip := mustAddr(t, "203.0.113.7")

	a := salt.VisitorID(ip, chromeUA, "example.com")
	if b := salt.VisitorID(ip, chromeUA, "example.com"); a != b {
		t.Error("same visitor produced different ids within one salt")
	}
	if b := salt.VisitorID(mustAddr(t, "203.0.113.8"), chromeUA, "example.com"); a == b {
		t.Error("different IPs collided")
	}
	// Same person, different site: ids must not be cross-linkable.
	if b := salt.VisitorID(ip, chromeUA, "other.example"); a == b {
		t.Error("visitor id is linkable across sites")
	}

	if err := salt.Rotate(); err != nil {
		t.Fatal(err)
	}
	if b := salt.VisitorID(ip, chromeUA, "example.com"); a == b {
		t.Error("id survived salt rotation; yesterday's visitors stay linkable")
	}
}

// TestForwardedForIgnoredWithoutTrustProxy guards against clients forging
// their own geolocation and evading rate limits with a header.
func TestForwardedForIgnoredWithoutTrustProxy(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/event", nil)
	r.RemoteAddr = "203.0.113.7:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := ClientIP(r, false); got.String() != "203.0.113.7" {
		t.Errorf("ClientIP = %v, want the socket address when trust_proxy is off", got)
	}
	if got := ClientIP(r, true); got.String() != "1.2.3.4" {
		t.Errorf("ClientIP = %v, want the XFF address when trust_proxy is on", got)
	}
}

func TestRateKeyBucketsByPrefix(t *testing.T) {
	a := rateKey(mustAddr(t, "203.0.113.7"))
	b := rateKey(mustAddr(t, "203.0.113.200"))
	if a != b {
		t.Errorf("addresses in one /24 got different keys: %s vs %s", a, b)
	}
	if c := rateKey(mustAddr(t, "203.0.114.7")); c == a {
		t.Error("addresses in different /24s share a key")
	}
}

func TestEngagementRecordsTimeOnPage(t *testing.T) {
	srv, reg := newTestServer(t, testYAML)

	post(t, srv, `{"n":"pageview","d":"example.com","u":"https://example.com/post"}`,
		"https://example.com", chromeUA)
	drain(t, srv)
	post(t, srv, `{"n":"engagement","d":"example.com","u":"https://example.com/post","e":42}`,
		"https://example.com", chromeUA)
	drain(t, srv)

	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "ridiculytics_time_on_page_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			if h := m.GetHistogram(); h != nil && h.GetSampleCount() == 1 {
				if got := h.GetSampleSum(); got != 42 {
					t.Errorf("time on page sum = %v, want 42", got)
				}
				return
			}
		}
	}
	t.Error("no time_on_page observation recorded")
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return a
}
