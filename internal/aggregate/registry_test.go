package aggregate

import (
	"fmt"
	"sort"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/fjaeckel/ridiculytics/internal/config"
)

func testConfig(t *testing.T, mutate func(*config.Site)) *config.Config {
	t.Helper()
	site := &config.Site{Domain: "example.com"}
	site.Defaults = config.Defaults{
		PathCap: 200, CrossPathCap: 50, ReferrerCap: 200, ASNCap: 200,
		CampaignCap: 100, EventCap: 50, AdmitMinCount: 1,
		SessionTTL: 30 * time.Minute, DecayAfter: 7 * 24 * time.Hour,
		PathLabels: config.AllDimensions,
	}
	if mutate != nil {
		mutate(site)
	}
	return &config.Config{Sites: []*config.Site{site}}
}

// gather collects every sample of a metric family, keyed by label map.
func gather(t *testing.T, r *Registry, name string) map[string]float64 {
	t.Helper()
	families, err := r.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			key := ""
			for _, l := range m.GetLabel() {
				key += l.GetName() + "=" + l.GetValue() + ","
			}
			out[key] = valueOf(m)
		}
	}
	return out
}

func valueOf(m *dto.Metric) float64 {
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	if g := m.GetGauge(); g != nil {
		return g.GetValue()
	}
	return 0
}

// key builds a lookup key in the same sorted order the Prometheus gatherer
// emits, so tests can state labels in any order.
func key(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for n := range labels {
		names = append(names, n)
	}
	sort.Strings(names)
	out := ""
	for _, n := range names {
		out += n + "=" + labels[n] + ","
	}
	return out
}

func total(m map[string]float64) float64 {
	var sum float64
	for _, v := range m {
		sum += v
	}
	return sum
}

// sumWhere sums samples whose label key contains the given fragment.
func sumWhere(m map[string]float64, fragment string) float64 {
	var sum float64
	for k, v := range m {
		if contains(k, fragment) {
			sum += v
		}
	}
	return sum
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestMarginalsRemainExactUnderCardinalityPressure is the load-bearing test
// for the whole design. Path and country admission are independent, so far
// more distinct paths are offered than cross_path_cap allows. The per-cell
// values may be folded into __other__, but:
//
//   - the grand total must equal the number of events, and
//   - the per-country marginal must be exact.
//
// If this ever fails, "sum by (country)" in a dashboard is lying.
func TestMarginalsRemainExactUnderCardinalityPressure(t *testing.T) {
	r := New(testConfig(t, nil))
	now := time.Now()

	countries := []string{"DE", "US", "FR", "JP"}
	const paths = 500 // ten times cross_path_cap
	const repeats = 4

	perCountry := map[string]float64{}
	events := 0
	for rep := 0; rep < repeats; rep++ {
		for i := 0; i < paths; i++ {
			c := countries[i%len(countries)]
			r.Record(Observation{
				Site: "example.com", Kind: KindPageview,
				Path: fmt.Sprintf("/page/%d", i), Country: c,
				VisitorID: uint64(i), At: now,
			})
			perCountry[c]++
			events++
		}
	}

	got := gather(t, r, "ridiculytics_pageviews_by_country_total")
	if g := total(got); g != float64(events) {
		t.Errorf("grand total = %v, want %v (events are being lost, not folded)", g, events)
	}
	for c, want := range perCountry {
		if g := sumWhere(got, "country="+c+","); g != want {
			t.Errorf("marginal for country %s = %v, want %v", c, g, want)
		}
	}

	// The folding must actually have happened, or the test proves nothing.
	if got[key(map[string]string{"site": "example.com", "country": "DE", "path": OtherLabel})] == 0 {
		t.Error("expected paths to be folded into __other__; cap did not engage")
	}

	pv := gather(t, r, "ridiculytics_pageviews_total")
	if g := total(pv); g != float64(events) {
		t.Errorf("pageviews_total = %v, want %v", g, events)
	}
}

// TestPathLabelDisabledUsesAllSentinel checks that turning a family's path
// label off keeps the label present, so one metric never has two label sets.
func TestPathLabelDisabledUsesAllSentinel(t *testing.T) {
	cfg := testConfig(t, func(s *config.Site) {
		s.PathLabels = []string{config.DimCountry} // device path label off
	})
	r := New(cfg)
	r.Record(Observation{
		Site: "example.com", Kind: KindPageview,
		Path: "/a", Country: "DE", Device: "mobile", At: time.Now(),
	})

	dev := gather(t, r, "ridiculytics_pageviews_by_device_total")
	if dev[key(map[string]string{"site": "example.com", "device": "mobile", "path": AllLabel})] != 1 {
		t.Errorf("device family should use __all__ path sentinel, got %v", dev)
	}
	ctry := gather(t, r, "ridiculytics_pageviews_by_country_total")
	if ctry[key(map[string]string{"site": "example.com", "country": "DE", "path": "/a"})] != 1 {
		t.Errorf("country family should carry the real path, got %v", ctry)
	}
}

// TestPathAdmissionIsSharedAcrossFamilies verifies that one path decision
// applies everywhere. If families disagreed about which paths exist, joining
// them in PromQL would silently drop rows.
func TestPathAdmissionIsSharedAcrossFamilies(t *testing.T) {
	r := New(testConfig(t, nil))
	now := time.Now()

	for i := 0; i < 300; i++ {
		r.Record(Observation{
			Site: "example.com", Kind: KindPageview,
			Path: fmt.Sprintf("/p%d", i), Country: "DE", Device: "mobile",
			Browser: "Firefox", At: now,
		})
	}

	countryPaths := labelSet(gather(t, r, "ridiculytics_pageviews_by_country_total"), "path=")
	devicePaths := labelSet(gather(t, r, "ridiculytics_pageviews_by_device_total"), "path=")
	browserPaths := labelSet(gather(t, r, "ridiculytics_pageviews_by_browser_total"), "path=")

	if len(countryPaths) != len(devicePaths) || len(countryPaths) != len(browserPaths) {
		t.Fatalf("families disagree on path set: country=%d device=%d browser=%d",
			len(countryPaths), len(devicePaths), len(browserPaths))
	}
	for p := range countryPaths {
		if !devicePaths[p] || !browserPaths[p] {
			t.Errorf("path %q present in country family but missing elsewhere", p)
		}
	}
}

func labelSet(m map[string]float64, prefix string) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		for i := 0; i+len(prefix) <= len(k); i++ {
			if k[i:i+len(prefix)] == prefix {
				rest := k[i+len(prefix):]
				for j := 0; j < len(rest); j++ {
					if rest[j] == ',' {
						out[rest[:j]] = true
						break
					}
				}
				break
			}
		}
	}
	return out
}

// TestASNFoldsOrgWithNumber guards a subtle blowup: if the ASN is capped but
// the organisation string is not, __other__ fans out into one series per org
// and the cap achieves nothing.
func TestASNFoldsOrgWithNumber(t *testing.T) {
	cfg := testConfig(t, func(s *config.Site) {
		s.ASNCap = 2
		s.AdmitMinCount = 1
		s.PathLabels = nil
	})
	r := New(cfg)
	now := time.Now()

	for i := 0; i < 50; i++ {
		r.Record(Observation{
			Site: "example.com", Kind: KindPageview,
			Path: "/", ASN: fmt.Sprintf("%d", 1000+i),
			ASOrg: fmt.Sprintf("Org %d", i), At: now,
		})
	}

	got := gather(t, r, "ridiculytics_pageviews_by_asn_total")
	if len(got) > 3 { // 2 admitted + 1 __other__
		t.Errorf("ASN cap leaked through as_org: %d series, want <= 3\n%v", len(got), got)
	}
	if got[key(map[string]string{"site": "example.com", "asn": OtherLabel, "as_org": OtherLabel, "path": AllLabel})] == 0 {
		t.Errorf("expected folded ASN to fold its org too, got %v", got)
	}
}

// TestSessionsAndBounces checks the session lifecycle end to end.
func TestSessionsAndBounces(t *testing.T) {
	r := New(testConfig(t, nil))
	base := time.Now()

	// Visitor 1 reads two pages: a session, not a bounce.
	r.Record(Observation{Site: "example.com", Kind: KindPageview, Path: "/", VisitorID: 1, At: base})
	r.Record(Observation{Site: "example.com", Kind: KindPageview, Path: "/next", VisitorID: 1, At: base.Add(time.Minute)})
	// Visitor 2 reads one page and leaves: a bounce.
	r.Record(Observation{Site: "example.com", Kind: KindPageview, Path: "/", VisitorID: 2, At: base})

	if s := total(gather(t, r, "ridiculytics_sessions_total")); s != 2 {
		t.Errorf("sessions = %v, want 2", s)
	}
	if e := gather(t, r, "ridiculytics_entries_total"); e[key(map[string]string{"site": "example.com", "path": "/"})] != 2 {
		t.Errorf("entries on / = %v, want 2", e[key(map[string]string{"site": "example.com", "path": "/"})])
	}

	// Expire both sessions.
	r.Maintain(base.Add(2 * time.Hour))

	if b := total(gather(t, r, "ridiculytics_bounces_total")); b != 1 {
		t.Errorf("bounces = %v, want 1", b)
	}
	exits := gather(t, r, "ridiculytics_exits_total")
	if exits[key(map[string]string{"site": "example.com", "path": "/next"})] != 1 {
		t.Errorf("expected exit on /next, got %v", exits)
	}
}

// TestCustomEventsDoNotInflatePageviews checks that a custom event is counted
// once, in the event family only.
func TestCustomEventsDoNotInflatePageviews(t *testing.T) {
	r := New(testConfig(t, nil))
	now := time.Now()

	r.Record(Observation{Site: "example.com", Kind: KindPageview, Path: "/pricing", Country: "DE", At: now})
	r.Record(Observation{Site: "example.com", Kind: KindCustom, Name: "signup", Path: "/pricing", At: now})

	if pv := total(gather(t, r, "ridiculytics_pageviews_total")); pv != 1 {
		t.Errorf("pageviews = %v, want 1", pv)
	}
	if ev := total(gather(t, r, "ridiculytics_events_total")); ev != 1 {
		t.Errorf("events = %v, want 1", ev)
	}
	if c := total(gather(t, r, "ridiculytics_pageviews_by_country_total")); c != 1 {
		t.Errorf("country family = %v, want 1 (custom events must not count as pageviews)", c)
	}
}

// TestDecayReleasesSeries verifies that quiet series are dropped so a site's
// metric shape follows current traffic.
func TestDecayReleasesSeries(t *testing.T) {
	r := New(testConfig(t, nil))
	base := time.Now()

	r.Record(Observation{Site: "example.com", Kind: KindPageview, Path: "/old", Country: "DE", At: base})
	if len(gather(t, r, "ridiculytics_pageviews_total")) != 1 {
		t.Fatal("expected one path series")
	}

	r.Maintain(base.Add(8 * 24 * time.Hour))

	if n := len(gather(t, r, "ridiculytics_pageviews_total")); n != 0 {
		t.Errorf("expected decayed path series to be removed, %d remain", n)
	}
	if n := len(gather(t, r, "ridiculytics_pageviews_by_country_total")); n != 0 {
		t.Errorf("expected decayed cross series to be removed, %d remain", n)
	}
}

// TestUnknownSiteIsRejected ensures traffic for an unconfigured domain cannot
// create series.
func TestUnknownSiteIsRejected(t *testing.T) {
	r := New(testConfig(t, nil))
	r.Record(Observation{Site: "evil.example", Kind: KindPageview, Path: "/", At: time.Now()})

	if n := len(gather(t, r, "ridiculytics_pageviews_total")); n != 0 {
		t.Errorf("unknown site created %d series", n)
	}
	ing := gather(t, r, "ridiculytics_ingest_events_total")
	if ing[key(map[string]string{"site": "evil.example", "result": "unknown_site"})] != 1 {
		t.Errorf("expected unknown_site to be counted, got %v", ing)
	}
}

// TestBoundedDimensionsAdmitOnFirstSight guards a data-loss bug: applying
// frequency-based admission to dimensions with a closed value space (browser,
// OS, device, screen, country) means a quiet site reports __other__ until its
// third pageview, and a rare-but-real browser may never clear the threshold.
// Those value spaces cannot be attacked, so they need no gate.
func TestBoundedDimensionsAdmitOnFirstSight(t *testing.T) {
	cfg := testConfig(t, func(s *config.Site) { s.AdmitMinCount = 3 })
	r := New(cfg)

	r.Record(Observation{
		Site: "example.com", Kind: KindPageview, Path: "/",
		Country: "DE", Browser: "Firefox", OS: "Linux", Device: "desktop", Screen: "xl",
		Referrer: "news.ycombinator.com", At: time.Now(),
	})

	bounded := map[string]map[string]string{
		"ridiculytics_pageviews_by_country_total": {"country": "DE"},
		"ridiculytics_pageviews_by_browser_total": {"browser": "Firefox"},
		"ridiculytics_pageviews_by_os_total":      {"os": "Linux"},
		"ridiculytics_pageviews_by_device_total":  {"device": "desktop"},
		"ridiculytics_pageviews_by_screen_total":  {"class": "xl"},
	}
	for metric, labels := range bounded {
		got := gather(t, r, metric)
		found := false
		for k, v := range got {
			for name, val := range labels {
				if contains(k, name+"="+val+",") && v == 1 {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%s: expected the real value on first sight, got %v", metric, got)
		}
	}

	// The open-ended dimensions must still be gated.
	ref := gather(t, r, "ridiculytics_pageviews_by_referrer_total")
	if sumWhere(ref, "referrer="+OtherLabel+",") != 1 {
		t.Errorf("referrer should stay gated until it proves itself, got %v", ref)
	}
}
