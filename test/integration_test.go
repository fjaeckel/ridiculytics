// Package test holds integration tests that wire the real components together
// the way cmd/ridiculytics does, and drive them over real HTTP listeners.
// Unit tests live next to the code they cover; these exist to catch the seams
// between packages.
package test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v3"

	"github.com/fjaeckel/ridiculytics/internal/aggregate"
	"github.com/fjaeckel/ridiculytics/internal/config"
	"github.com/fjaeckel/ridiculytics/internal/geo"
	"github.com/fjaeckel/ridiculytics/internal/ingest"
	"github.com/fjaeckel/ridiculytics/web"
)

const chromeUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0 Safari/537.36"

// stack is a running collector with both listeners bound to real ports.
type stack struct {
	ingestURL  string
	metricsURL string
	srv        *ingest.Server
	reg        *aggregate.Registry
}

// newStack boots the same wiring cmd/ridiculytics uses, configured purely from
// environment variables, on two ephemeral ports.
func newStack(t *testing.T, env map[string]string) *stack {
	t.Helper()

	for k, v := range env {
		t.Setenv(config.EnvPrefix+k, v)
	}
	store, err := config.NewStore("") // env-only, no file
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg := store.Get()

	reg := aggregate.New(cfg)
	salt, err := ingest.NewSalt(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv := ingest.NewServer(ingest.Options{
		Registry: reg, Config: store, Geo: geo.Null{}, Salt: salt,
	})

	ingestLn := listen(t)
	metricsLn := listen(t)

	ingestSrv := &http.Server{Handler: srv.Routes(web.CounterJS, true)}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(reg.Gatherer(), promhttp.HandlerOpts{}))
	metricsSrv := &http.Server{Handler: metricsMux}

	go ingestSrv.Serve(ingestLn)
	go metricsSrv.Serve(metricsLn)

	t.Cleanup(func() {
		ingestSrv.Close()
		metricsSrv.Close()
		srv.Close()
	})

	return &stack{
		ingestURL:  "http://" + ingestLn.Addr().String(),
		metricsURL: "http://" + metricsLn.Addr().String() + "/metrics",
		srv:        srv,
		reg:        reg,
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// send posts one event, returning the status code.
func (s *stack) send(t *testing.T, body string, headers map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.ingestURL+"/api/event", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("User-Agent", chromeUA)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// scrape fetches the metrics endpoint as text.
func (s *stack) scrape(t *testing.T) string {
	t.Helper()
	resp, err := http.Get(s.metricsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics returned %d", resp.StatusCode)
	}
	return string(b)
}

// settle waits for the async aggregation workers to drain.
func settle() { time.Sleep(150 * time.Millisecond) }

func mustContain(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("metrics output missing:\n  %s", want)
	}
}

// TestFullPipeline drives a pageview from HTTP ingest all the way to a
// Prometheus scrape, across every package.
func TestFullPipeline(t *testing.T) {
	s := newStack(t, map[string]string{
		"SITES":           "example.com",
		"ADMIT_MIN_COUNT": "1",
	})

	body := `{"n":"pageview","d":"example.com","u":"https://example.com/blog/post?utm_source=hn",` +
		`"r":"https://news.ycombinator.com/item?id=1","w":1920}`
	if code := s.send(t, body, map[string]string{"Origin": "https://example.com"}); code != http.StatusAccepted {
		t.Fatalf("ingest returned %d, want 202", code)
	}
	settle()

	out := s.scrape(t)
	for _, want := range []string{
		`ridiculytics_pageviews_total{path="/blog/post",site="example.com"} 1`,
		`referrer="news.ycombinator.com"`,
		`source="hn"`,
		`browser="Chrome"`,
		`os="macOS"`,
		`device="desktop"`,
		`class="xl"`,
		`ridiculytics_sessions_total{site="example.com"} 1`,
		`ridiculytics_unique_visitors{site="example.com",window="24h"} 1`,
	} {
		mustContain(t, out, want)
	}

	// The query string must not have reached a label.
	if strings.Contains(out, "utm_source=hn\"") {
		t.Error("query string leaked into a label value")
	}
}

// TestMetricsPortServesOnlyMetrics: the ingest and metrics listeners are
// separate on purpose. Nothing on the ingest port may read aggregates back
// out, and the metrics port accepts no events.
func TestListenersAreSeparate(t *testing.T) {
	s := newStack(t, map[string]string{"SITES": "example.com"})

	resp, err := http.Get(s.ingestURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/metrics on the ingest port returned %d, want 404", resp.StatusCode)
	}

	resp2, err := http.Post(s.metricsURL, "text/plain", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusAccepted {
		t.Error("the metrics port accepted an event; listeners are not separate")
	}
}

// TestEnvConfigReachesBehaviour proves the environment is actually wired to
// runtime behaviour, not just parsed into a struct.
func TestEnvConfigReachesBehaviour(t *testing.T) {
	s := newStack(t, map[string]string{
		"SITES":           "example.com",
		"ADMIT_MIN_COUNT": "1",
		"CROSS_PATH_CAP":  "2",
		"PATH_LABELS":     "country,referrer",
	})

	for i := 0; i < 6; i++ {
		s.send(t, fmt.Sprintf(
			`{"n":"pageview","d":"example.com","u":"https://example.com/p%d"}`, i),
			map[string]string{"Origin": "https://example.com"})
	}
	settle()
	out := s.scrape(t)

	// cross_path_cap=2 means paths beyond the first two fold into __other__.
	mustContain(t, out, `path="__other__"`)
	// path_labels excludes device, so that family carries the __all__ sentinel.
	mustContain(t, out, `ridiculytics_pageviews_by_device_total{device="desktop",path="__all__"`)
	// ...but includes country, which keeps a real path label.
	mustContain(t, out, `ridiculytics_pageviews_by_country_total{country="__none__",path="/p0"`)
}

// TestOriginEnforcementEndToEnd checks the allowlist through the real handler,
// including the inferred default origins.
func TestOriginEnforcementEndToEnd(t *testing.T) {
	s := newStack(t, map[string]string{"SITES": "example.com"})
	body := `{"n":"pageview","d":"example.com","u":"https://example.com/"}`

	cases := []struct {
		origin string
		want   int
	}{
		{"https://example.com", http.StatusAccepted},
		{"https://www.example.com", http.StatusAccepted}, // inferred
		{"https://evil.example", http.StatusForbidden},
		{"", http.StatusForbidden}, // no Origin at all
	}
	for _, c := range cases {
		h := map[string]string{}
		if c.origin != "" {
			h["Origin"] = c.origin
		}
		if got := s.send(t, body, h); got != c.want {
			t.Errorf("origin %q returned %d, want %d", c.origin, got, c.want)
		}
	}
}

// TestCounterJSIsServed covers the zero-third-party-request path.
func TestCounterJSIsServed(t *testing.T) {
	s := newStack(t, map[string]string{"SITES": "example.com"})

	resp, err := http.Get(s.ingestURL + "/counter.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("counter.js returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("content type = %q, want javascript", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "data-host") {
		t.Error("served script is not counter.js")
	}
}

// TestEngagementAndSessionLifecycle exercises pageview -> engagement ->
// session expiry -> bounce accounting through the real pipeline.
func TestEngagementAndSessionLifecycle(t *testing.T) {
	s := newStack(t, map[string]string{
		"SITES":           "example.com",
		"ADMIT_MIN_COUNT": "1",
		"SESSION_TTL":     "1s",
	})
	h := map[string]string{"Origin": "https://example.com"}

	s.send(t, `{"n":"pageview","d":"example.com","u":"https://example.com/post"}`, h)
	settle()
	s.send(t, `{"n":"engagement","d":"example.com","u":"https://example.com/post","e":42}`, h)
	settle()

	mustContain(t, s.scrape(t), `ridiculytics_time_on_page_seconds_sum{path="/post",site="example.com"} 42`)

	// Expire the session and confirm it lands as a bounce with an exit page.
	time.Sleep(1100 * time.Millisecond)
	s.reg.Maintain(time.Now())

	out := s.scrape(t)
	mustContain(t, out, `ridiculytics_bounces_total{site="example.com"} 1`)
	mustContain(t, out, `ridiculytics_exits_total{path="/post",site="example.com"} 1`)
}

// TestCustomEventEndToEnd covers goals travelling the full path.
func TestCustomEventEndToEnd(t *testing.T) {
	s := newStack(t, map[string]string{
		"SITES": "example.com", "ADMIT_MIN_COUNT": "1",
	})
	h := map[string]string{"Origin": "https://example.com"}

	s.send(t, `{"n":"pageview","d":"example.com","u":"https://example.com/pricing"}`, h)
	s.send(t, `{"n":"signup","d":"example.com","u":"https://example.com/pricing","p":{"plan":"free"}}`, h)
	settle()

	out := s.scrape(t)
	mustContain(t, out, `ridiculytics_events_total{name="signup",path="/pricing",site="example.com"} 1`)
	// A custom event must not inflate pageviews.
	mustContain(t, out, `ridiculytics_pageviews_total{path="/pricing",site="example.com"} 1`)
}

// TestMultiSiteIsolation: two sites must not bleed into each other's series,
// and each keeps its own origin allowlist.
func TestMultiSiteIsolation(t *testing.T) {
	s := newStack(t, map[string]string{
		"SITES":           "a.example,b.example",
		"ADMIT_MIN_COUNT": "1",
	})

	if got := s.send(t, `{"n":"pageview","d":"a.example","u":"https://a.example/x"}`,
		map[string]string{"Origin": "https://a.example"}); got != http.StatusAccepted {
		t.Fatalf("site a returned %d", got)
	}
	// b.example's origin must not be accepted for a.example's domain.
	if got := s.send(t, `{"n":"pageview","d":"a.example","u":"https://a.example/x"}`,
		map[string]string{"Origin": "https://b.example"}); got != http.StatusForbidden {
		t.Errorf("cross-site origin returned %d, want 403", got)
	}
	settle()

	out := s.scrape(t)
	mustContain(t, out, `ridiculytics_pageviews_total{path="/x",site="a.example"} 1`)
	if strings.Contains(out, `ridiculytics_pageviews_total{path="/x",site="b.example"}`) {
		t.Error("site b acquired series from site a's traffic")
	}
}

// TestReloadKeepsCountersAndAppliesNewSites: a SIGHUP-style reload must not
// reset live counters, or every dashboard gets a hole punched in it.
func TestReloadPreservesCounters(t *testing.T) {
	t.Setenv(config.EnvPrefix+"SITES", "example.com")
	t.Setenv(config.EnvPrefix+"ADMIT_MIN_COUNT", "1")

	store, err := config.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	reg := aggregate.New(store.Get())
	reg.Record(aggregate.Observation{
		Site: "example.com", Kind: aggregate.KindPageview, Path: "/", At: time.Now(),
	})

	// A reload that adds a site must keep the existing site's state.
	t.Setenv(config.EnvPrefix+"SITES", "example.com,new.example")
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	reg.Configure(store.Get())

	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	var found float64
	for _, f := range families {
		if f.GetName() != "ridiculytics_pageviews_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			found = m.GetCounter().GetValue()
		}
	}
	if found != 1 {
		t.Errorf("counter = %v after reload, want 1 (reload must not reset state)", found)
	}

	reg.Record(aggregate.Observation{
		Site: "new.example", Kind: aggregate.KindPageview, Path: "/", At: time.Now(),
	})
	// The newly added site must now be accepted rather than counted unknown.
	out := gatherText(t, reg)
	if !strings.Contains(out, `site="new.example"`) {
		t.Error("a site added by reload is not being recorded")
	}
}

func gatherText(t *testing.T, reg *aggregate.Registry) string {
	t.Helper()
	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				fmt.Fprintf(&b, "%s=%q ", l.GetName(), l.GetValue())
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// TestBadEnvFailsFast: a misconfigured deployment must refuse to start rather
// than come up silently wrong.
func TestBadEnvFailsFast(t *testing.T) {
	t.Setenv(config.EnvPrefix+"SITES", "example.com")
	t.Setenv(config.EnvPrefix+"WORKERS", "not-a-number")

	if _, err := config.NewStore(""); err == nil {
		t.Fatal("expected startup to fail on an invalid environment value")
	}
}

// repoFile reads a file from the repository root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestProdComposeRunsNoMonitoringStack pins a deployment requirement: the
// production stack is ridiculytics + nginx + optional certbot. Prometheus and
// Grafana belong to the end-to-end test stack only, and must never creep back
// into the file people actually deploy.
func TestProdComposeRunsNoMonitoringStack(t *testing.T) {
	prod := repoFile(t, "docker-compose.prod.yml")

	for _, banned := range []string{"prom/prometheus", "grafana/grafana"} {
		if strings.Contains(prod, banned) {
			t.Errorf("docker-compose.prod.yml runs %s; monitoring servers belong in the e2e stack", banned)
		}
	}
	for _, want := range []string{"ridiculytics:", "nginx:", "certbot:"} {
		if !strings.Contains(prod, want) {
			t.Errorf("docker-compose.prod.yml is missing the %s service", want)
		}
	}

	// The e2e stack is where the monitoring servers live.
	e2e := repoFile(t, "docker-compose.e2e.yml")
	for _, want := range []string{"prom/prometheus", "grafana/grafana"} {
		if !strings.Contains(e2e, want) {
			t.Errorf("docker-compose.e2e.yml should run %s", want)
		}
	}
}

// TestCertbotIsOptIn: certbot must not run unless the tls profile is enabled,
// so a deployment terminating TLS elsewhere never invokes it.
func TestCertbotIsOptIn(t *testing.T) {
	c := parseCompose(t, "docker-compose.prod.yml")

	for _, name := range []string{"certbot", "certbot-init"} {
		svc, ok := c.Services[name]
		if !ok {
			t.Errorf("service %s not found", name)
			continue
		}
		if len(svc.Profiles) == 0 {
			t.Errorf("%s has no profiles; certbot must be opt-in, not on by default", name)
			continue
		}
		found := false
		for _, p := range svc.Profiles {
			if p == "tls" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s profiles = %v, want the tls profile", name, svc.Profiles)
		}
	}

	// The services that must always run carry no profile.
	for _, name := range []string{"ridiculytics", "nginx"} {
		if svc, ok := c.Services[name]; !ok {
			t.Errorf("service %s missing from the production stack", name)
		} else if len(svc.Profiles) != 0 {
			t.Errorf("%s is behind profile %v but must always run", name, svc.Profiles)
		}
	}
}

// compose is the subset of a compose file these tests assert on.
type compose struct {
	Services map[string]struct {
		Image    string   `yaml:"image"`
		Profiles []string `yaml:"profiles"`
		Ports    []string `yaml:"ports"`
	} `yaml:"services"`
}

func parseCompose(t *testing.T, rel string) compose {
	t.Helper()
	var c compose
	if err := yaml.Unmarshal([]byte(repoFile(t, rel)), &c); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	if len(c.Services) == 0 {
		t.Fatalf("%s defines no services", rel)
	}
	return c
}

// TestMetricsPortNotPublishedPublicly: the metrics endpoint has no auth by
// design, so it must be bound to loopback in the production compose.
func TestMetricsPortBoundToLoopback(t *testing.T) {
	prod := repoFile(t, "docker-compose.prod.yml")
	if !strings.Contains(prod, `"127.0.0.1:${METRICS_PORT:-9090}:9090"`) {
		t.Error("metrics port must be published to 127.0.0.1 only; it has no authentication")
	}
}

// TestNginxNeverProxiesMetrics guards the same property one layer up.
func TestNginxNeverProxiesMetrics(t *testing.T) {
	snippet := repoFile(t, "deploy/nginx/snippets/ridiculytics-proxy.conf")
	if strings.Contains(snippet, "/metrics") && !strings.Contains(snippet, "# ") {
		t.Error("nginx appears to route /metrics")
	}
	// The catch-all must close everything not explicitly allowed.
	if !strings.Contains(snippet, "location / {") || !strings.Contains(snippet, "return 404;") {
		t.Error("nginx proxy snippet lacks a default-deny location block")
	}
}
