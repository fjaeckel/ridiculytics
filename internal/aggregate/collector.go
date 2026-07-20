package aggregate

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// stateCollector reports everything that is computed at scrape time rather
// than incremented on the hot path: HLL estimates, live map sizes and guard
// occupancy. Sketches cannot be represented as counters, so they are gauges
// sampled by Prometheus.
type stateCollector struct{ r *Registry }

var (
	descUnique = prometheus.NewDesc(
		ns+"_unique_visitors",
		"Estimated distinct visitors in a sliding window (HyperLogLog, ~0.8% error).",
		[]string{"site", "window"}, nil)
	descActive = prometheus.NewDesc(
		ns+"_visitors_active",
		"Estimated distinct visitors in the last 5 minutes.",
		[]string{"site"}, nil)
	descUniquePath = prometheus.NewDesc(
		ns+"_unique_visitors_by_path",
		"Estimated distinct visitors per path over 24h (coarse sketch, ~3% error).",
		[]string{"site", "path"}, nil)
	descSessionsLive = prometheus.NewDesc(
		ns+"_sessions_live",
		"Sessions currently in flight.",
		[]string{"site"}, nil)
	descLabelValues = prometheus.NewDesc(
		ns+"_label_values",
		"Distinct label values currently admitted for a dimension.",
		[]string{"site", "dimension"}, nil)
	descCapped = prometheus.NewDesc(
		ns+"_cardinality_capped_total",
		"Observations folded into __other__ because a cap was reached.",
		[]string{"site", "dimension"}, nil)
	descEvicted = prometheus.NewDesc(
		ns+"_sessions_evicted_total",
		"Sessions dropped because the per-site session cap was reached.",
		[]string{"site"}, nil)
	descGeoAge = prometheus.NewDesc(
		ns+"_geoip_db_age_seconds",
		"Age of the loaded GeoIP database. Alert on this: stale geo is silent geo.",
		nil, nil)
)

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descUnique
	ch <- descActive
	ch <- descUniquePath
	ch <- descSessionsLive
	ch <- descLabelValues
	ch <- descCapped
	ch <- descEvicted
	ch <- descGeoAge
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()

	c.r.mu.RLock()
	states := make([]*siteState, 0, len(c.r.sites))
	for _, st := range c.r.sites {
		states = append(states, st)
	}
	c.r.mu.RUnlock()

	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	counter := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, labels...)
	}

	for _, st := range states {
		site := st.cfg.Domain
		w := st.windows

		gauge(descActive, float64(w.Active.Estimate(now)), site)
		gauge(descUnique, float64(w.H1.Estimate(now)), site, "1h")
		gauge(descUnique, float64(w.D1.Estimate(now)), site, "24h")
		gauge(descUnique, float64(w.D7.Estimate(now)), site, "7d")
		gauge(descUnique, float64(w.D30.Estimate(now)), site, "30d")

		gauge(descSessionsLive, float64(st.sessions.Len()), site)
		counter(descEvicted, float64(st.sessions.Evicted()), site)

		gauge(descLabelValues, float64(st.purePath.Len()), site, "path")
		gauge(descLabelValues, float64(st.crossPath.Len()), site, "path_cross")
		counter(descCapped, float64(st.purePath.Capped()), site, "path")
		counter(descCapped, float64(st.crossPath.Capped()), site, "path_cross")

		for _, d := range dimensions {
			g := st.guards[d.dim]
			gauge(descLabelValues, float64(g.Len()), site, d.dim)
			counter(descCapped, float64(g.Capped()), site, d.dim)
		}

		if st.cfg.UniqueVisitorsByPath {
			st.pathUniqMu.Lock()
			paths := make(map[string]uint64, len(st.pathUniques))
			for p, roll := range st.pathUniques {
				paths[p] = roll.Estimate(now)
			}
			st.pathUniqMu.Unlock()
			for p, v := range paths {
				gauge(descUniquePath, float64(v), site, p)
			}
		}
	}

	if c.r.geoAge != nil {
		gauge(descGeoAge, c.r.geoAge().Seconds())
	}
}
