// Package aggregate turns enriched observations into bounded Prometheus
// metrics. Every family carries at most two dimensions: its own, plus path.
package aggregate

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/fjaeckel/ridiculytics/internal/config"
	"github.com/fjaeckel/ridiculytics/internal/sketch"
)

const ns = "ridiculytics"

// Kind distinguishes the three things a client can report.
type Kind int

const (
	KindPageview Kind = iota
	KindCustom
	KindEngagement
)

// Observation is a fully enriched event, ready to be counted. Enrichment
// happens upstream so this package never touches an IP or a User-Agent.
type Observation struct {
	Site string
	Kind Kind
	Name string // custom event name

	Path     string // already normalized
	Referrer string
	Source   string
	Medium   string
	Campaign string

	Country string
	Region  string
	City    string
	ASN     string
	ASOrg   string

	Browser string
	OS      string
	Device  string
	Screen  string

	VisitorID  uint64
	TimeOnPage time.Duration
	At         time.Time
}

// dimension describes one metric family that can carry a path label.
type dimension struct {
	dim    string   // config dimension name
	metric string   // metric name suffix
	labels []string // value label names, excluding site and path
}

var dimensions = []dimension{
	{config.DimCountry, "pageviews_by_country_total", []string{"country"}},
	{config.DimASN, "pageviews_by_asn_total", []string{"asn", "as_org"}},
	{config.DimReferrer, "pageviews_by_referrer_total", []string{"referrer"}},
	{config.DimSource, "pageviews_by_source_total", []string{"source"}},
	{config.DimMedium, "pageviews_by_medium_total", []string{"medium"}},
	{config.DimCampaign, "pageviews_by_campaign_total", []string{"campaign"}},
	{config.DimBrowser, "pageviews_by_browser_total", []string{"browser"}},
	{config.DimOS, "pageviews_by_os_total", []string{"os"}},
	{config.DimDevice, "pageviews_by_device_total", []string{"device"}},
	{config.DimScreen, "pageviews_by_screen_total", []string{"class"}},
	{config.DimEvent, "events_total", []string{"name"}},
}

// siteState is the per-site guard and sketch set.
type siteState struct {
	cfg *config.Site

	// pathCounter is shared by both path guards so a path's frequency is
	// judged once, from one stream of observations.
	pathCounter *counter
	purePath    *Guard // path_cap, for path-primary families
	crossPath   *Guard // cross_path_cap, shared by every path-labelled family

	guards map[string]*Guard

	windows     *sketch.Windows
	sessions    *sketch.Sessions
	pathUniques map[string]*sketch.Rolling
	pathUniqMu  sync.Mutex
}

// Registry owns every metric and all per-site state.
type Registry struct {
	prom *prometheus.Registry

	mu    sync.RWMutex
	sites map[string]*siteState

	// Traffic
	pageviews *prometheus.CounterVec // site, path
	entries   *prometheus.CounterVec // site, path
	exits     *prometheus.CounterVec // site, path

	// Dimension families, keyed by config dimension name.
	dims map[string]*prometheus.CounterVec

	// Opt-in geo detail, without a path label.
	regions *prometheus.CounterVec // site, region
	cities  *prometheus.CounterVec // site, city

	// Sessions
	sessionsTotal *prometheus.CounterVec // site
	bounces       *prometheus.CounterVec // site
	sessionDur    *prometheus.HistogramVec
	timeOnPage    *prometheus.HistogramVec // site, path

	// Self-observability. Guard occupancy, capped totals and session-eviction
	// counts are emitted by stateCollector instead, since they are read from
	// live state rather than incremented on the hot path.
	IngestEvents  *prometheus.CounterVec // site, result
	IngestLatency prometheus.Histogram
	QueueDepth    prometheus.Gauge
	GeoLookups    *prometheus.CounterVec // result
	geoAge        func() time.Duration
}

// New builds the registry from config. Metrics are registered on a private
// prometheus.Registry so the metrics port exposes exactly what we intend.
func New(cfg *config.Config) *Registry {
	r := &Registry{
		prom:  prometheus.NewRegistry(),
		sites: make(map[string]*siteState),
		dims:  make(map[string]*prometheus.CounterVec),
	}

	counterVec := func(name, help string, labels ...string) *prometheus.CounterVec {
		v := prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: ns, Name: name, Help: help}, labels)
		r.prom.MustRegister(v)
		return v
	}

	r.pageviews = counterVec("pageviews_total", "Pageviews by page path.", "site", "path")
	r.entries = counterVec("entries_total", "Sessions that began on this path.", "site", "path")
	r.exits = counterVec("exits_total", "Sessions that ended on this path.", "site", "path")

	for _, d := range dimensions {
		labels := append([]string{"site"}, d.labels...)
		labels = append(labels, "path")
		help := "Pageviews by " + d.dim + ", optionally broken down by path."
		if d.dim == config.DimEvent {
			help = "Custom events by name, optionally broken down by path."
		}
		r.dims[d.dim] = counterVec(d.metric, help, labels...)
	}

	r.regions = counterVec("pageviews_by_region_total", "Pageviews by region (opt-in).", "site", "region")
	r.cities = counterVec("pageviews_by_city_total", "Pageviews by city (opt-in).", "site", "city")

	r.sessionsTotal = counterVec("sessions_total", "Sessions started.", "site")
	r.bounces = counterVec("bounces_total", "Sessions that ended after a single pageview.", "site")

	r.sessionDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns, Name: "session_duration_seconds",
		Help:    "Session duration.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800},
	}, []string{"site"})
	r.prom.MustRegister(r.sessionDur)

	// Path-labelled histogram uses cross_path_cap, not path_cap: a histogram
	// series costs ~12 samples, so 200 paths here would outweigh every
	// counter family combined.
	r.timeOnPage = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns, Name: "time_on_page_seconds",
		Help:    "Client-reported time on page.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600},
	}, []string{"site", "path"})
	r.prom.MustRegister(r.timeOnPage)

	r.IngestEvents = counterVec("ingest_events_total",
		"Ingest outcomes: accepted, rejected_origin, rate_limited, malformed, bot, unknown_site, queue_full.",
		"site", "result")
	r.GeoLookups = counterVec("geoip_lookups_total", "GeoIP lookup outcomes.", "result")

	r.IngestLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns, Name: "ingest_duration_seconds",
		Help:    "Time to accept an event on the HTTP handler.",
		Buckets: prometheus.ExponentialBuckets(0.00005, 3, 8),
	})
	r.QueueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "queue_depth", Help: "Events waiting to be aggregated.",
	})
	r.prom.MustRegister(r.IngestLatency, r.QueueDepth)
	r.prom.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	r.prom.MustRegister(&stateCollector{r: r})

	r.Configure(cfg)
	return r
}

// Configure creates state for any newly configured site. Called at boot and
// on reload; existing sites keep their counters and sketches so a reload
// never blows a hole in live dashboards.
func (r *Registry) Configure(cfg *config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, sc := range cfg.Sites {
		if st, ok := r.sites[sc.Domain]; ok {
			st.cfg = sc
			continue
		}
		st := &siteState{
			cfg:         sc,
			pathCounter: newCounter(4, 4096),
			guards:      make(map[string]*Guard),
			windows:     sketch.NewWindows(),
			pathUniques: make(map[string]*sketch.Rolling),
		}
		st.purePath = NewGuardWith(sc.PathCap, sc.AdmitMinCount, st.pathCounter)
		st.crossPath = NewGuardWith(sc.CrossPathCap, sc.AdmitMinCount, st.pathCounter)

		for _, d := range dimensions {
			st.guards[d.dim] = NewGuard(capFor(sc, d.dim), minCountFor(sc, d.dim))
		}
		st.sessions = sketch.NewSessions(sc.Domain, sc.SessionTTL, 200_000, &sessionSink{r: r})
		r.sites[sc.Domain] = st
	}
}

// minCountFor decides whether a dimension needs frequency-based admission.
//
// The gate exists to stop an attacker inventing label values faster than the
// cap can absorb them. That threat only exists where the value space is open.
// Browser, OS, device and screen values come from our own parser's fixed
// vocabulary, and country is closed by ISO-3166 — nothing a client sends can
// produce a novel value, and the dimension is naturally bounded well below its
// cap. Gating those would only mean a quiet site shows __other__ until its
// third pageview, and a rare-but-real browser might never clear the threshold
// at all. So they are admitted on first sight.
func minCountFor(s *config.Site, dim string) uint32 {
	switch dim {
	case config.DimBrowser, config.DimOS, config.DimDevice, config.DimScreen, config.DimCountry:
		return 1
	default:
		// path, referrer, source, medium, campaign, event name, ASN: open or
		// large value spaces, where a single crawler could otherwise squat
		// every slot in the first second.
		return s.AdmitMinCount
	}
}

func capFor(s *config.Site, dim string) int {
	switch dim {
	case config.DimReferrer:
		return s.ReferrerCap
	case config.DimASN:
		return s.ASNCap
	case config.DimCampaign, config.DimSource, config.DimMedium:
		return s.CampaignCap
	case config.DimEvent:
		return s.EventCap
	case config.DimCountry:
		return 300 // ISO-3166 is naturally bounded
	default:
		return 40 // browser, os, device, screen
	}
}

// Gatherer exposes the private registry for the metrics handler.
func (r *Registry) Gatherer() prometheus.Gatherer { return r.prom }

// SetGeoAgeFunc wires the geo database age gauge.
func (r *Registry) SetGeoAgeFunc(f func() time.Duration) { r.geoAge = f }

func (r *Registry) site(domain string) *siteState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sites[domain]
}

// Record folds one observation into the metrics.
func (r *Registry) Record(o Observation) {
	st := r.site(o.Site)
	if st == nil {
		r.IngestEvents.WithLabelValues(o.Site, "unknown_site").Inc()
		return
	}
	if o.At.IsZero() {
		o.At = time.Now()
	}

	if o.Kind == KindEngagement {
		r.recordEngagement(st, o)
		return
	}

	// Path admission is decided once, on the shared counter, and the same
	// answer is used by every path-labelled family.
	purePath := st.purePath.Admit(o.Path, o.At)
	crossPath := st.crossPath.Admit(o.Path, o.At)

	if o.Kind == KindPageview {
		r.pageviews.WithLabelValues(o.Site, purePath).Inc()
		if st.sessions.Touch(o.VisitorID, purePath, o.At) {
			r.entries.WithLabelValues(o.Site, purePath).Inc()
		}
		st.windows.Insert(o.VisitorID, o.At)
		r.recordPathUnique(st, crossPath, o)
	}

	for _, d := range dimensions {
		vec := r.dims[d.dim]
		g := st.guards[d.dim]

		pathLabel := AllLabel
		if st.cfg.HasPathLabel(d.dim) {
			pathLabel = crossPath
		}

		switch d.dim {
		case config.DimEvent:
			// The event family counts custom events only; pageviews are
			// already covered by pageviews_total.
			if o.Kind != KindCustom {
				continue
			}
			name := g.Admit(o.Name, o.At)
			vec.WithLabelValues(o.Site, name, pathLabel).Inc()

		case config.DimASN:
			if o.Kind != KindPageview {
				continue
			}
			asn := g.Admit(o.ASN, o.At)
			org := o.ASOrg
			// Org is 1:1 with ASN, so it costs nothing — but once the ASN is
			// folded, the org must fold too or __other__ fans out into one
			// series per organisation.
			if asn == OtherLabel || asn == NoneLabel {
				org = asn
			} else if org == "" {
				org = NoneLabel
			}
			vec.WithLabelValues(o.Site, asn, org, pathLabel).Inc()

		default:
			if o.Kind != KindPageview {
				continue
			}
			v := g.Admit(dimValue(&o, d.dim), o.At)
			vec.WithLabelValues(o.Site, v, pathLabel).Inc()
		}
	}

	if o.Kind == KindPageview {
		if st.cfg.RegionEnabled && o.Region != "" {
			r.regions.WithLabelValues(o.Site, o.Region).Inc()
		}
		if st.cfg.CityEnabled && o.City != "" {
			r.cities.WithLabelValues(o.Site, o.City).Inc()
		}
	}
}

func (r *Registry) recordEngagement(st *siteState, o Observation) {
	st.sessions.Extend(o.VisitorID, o.At)
	if o.TimeOnPage <= 0 {
		return
	}
	path, _ := st.crossPath.Peek(o.Path)
	r.timeOnPage.WithLabelValues(o.Site, path).Observe(o.TimeOnPage.Seconds())
}

// recordPathUnique maintains the opt-in coarse per-path unique sketch. Only
// admitted paths get one, so the map is bounded by cross_path_cap.
func (r *Registry) recordPathUnique(st *siteState, path string, o Observation) {
	if !st.cfg.UniqueVisitorsByPath || path == OtherLabel || path == NoneLabel {
		return
	}
	st.pathUniqMu.Lock()
	roll, ok := st.pathUniques[path]
	if !ok {
		roll = sketch.NewRolling(time.Hour, 24, true)
		st.pathUniques[path] = roll
	}
	st.pathUniqMu.Unlock()
	roll.Insert(o.VisitorID, o.At)
}

func dimValue(o *Observation, dim string) string {
	switch dim {
	case config.DimCountry:
		return o.Country
	case config.DimReferrer:
		return o.Referrer
	case config.DimSource:
		return o.Source
	case config.DimMedium:
		return o.Medium
	case config.DimCampaign:
		return o.Campaign
	case config.DimBrowser:
		return o.Browser
	case config.DimOS:
		return o.OS
	case config.DimDevice:
		return o.Device
	case config.DimScreen:
		return o.Screen
	}
	return ""
}

// sessionSink turns session lifecycle events into metrics.
type sessionSink struct{ r *Registry }

func (s *sessionSink) SessionStarted(site, entryPath string) {
	s.r.sessionsTotal.WithLabelValues(site).Inc()
}

func (s *sessionSink) SessionEnded(site string, e sketch.Ended) {
	s.r.sessionDur.WithLabelValues(site).Observe(e.Duration.Seconds())
	s.r.exits.WithLabelValues(site, e.ExitPath).Inc()
	if e.Bounced {
		s.r.bounces.WithLabelValues(site).Inc()
	}
}

// Maintain runs the periodic sweep: expiring sessions and decaying series
// whose label values have gone quiet.
func (r *Registry) Maintain(now time.Time) {
	r.mu.RLock()
	states := make([]*siteState, 0, len(r.sites))
	for _, st := range r.sites {
		states = append(states, st)
	}
	r.mu.RUnlock()

	for _, st := range states {
		site := st.cfg.Domain
		st.sessions.Sweep(now)

		for _, p := range st.purePath.Decay(now, st.cfg.DecayAfter) {
			r.pageviews.DeleteLabelValues(site, p)
			r.entries.DeleteLabelValues(site, p)
			r.exits.DeleteLabelValues(site, p)
		}
		// A path leaving the cross guard must be removed from every family
		// that could have used it as a secondary label.
		for _, p := range st.crossPath.Decay(now, st.cfg.DecayAfter) {
			m := prometheus.Labels{"site": site, "path": p}
			for _, vec := range r.dims {
				vec.DeletePartialMatch(m)
			}
			r.timeOnPage.DeletePartialMatch(m)

			st.pathUniqMu.Lock()
			delete(st.pathUniques, p)
			st.pathUniqMu.Unlock()
		}
		for _, d := range dimensions {
			g := st.guards[d.dim]
			primary := d.labels[0]
			for _, v := range g.Decay(now, st.cfg.DecayAfter) {
				r.dims[d.dim].DeletePartialMatch(prometheus.Labels{"site": site, primary: v})
			}
		}
	}
}
