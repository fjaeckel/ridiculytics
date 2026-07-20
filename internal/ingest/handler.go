// Package ingest accepts events over HTTP, enriches them, and hands them to
// the aggregate layer. Everything it receives is treated as hostile.
package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/fjaeckel/ridiculytics/internal/aggregate"
	"github.com/fjaeckel/ridiculytics/internal/config"
	"github.com/fjaeckel/ridiculytics/internal/enrich"
	"github.com/fjaeckel/ridiculytics/internal/geo"
)

// payload is the wire format. It deliberately matches the single-letter JSON
// shape that cookieless analytics collectors have converged on, so third-party
// scripts, SDKs and CMS plugins written against that shape work by changing
// one hostname.
type payload struct {
	Name     string          `json:"n"` // "pageview", "engagement", or a custom name
	Domain   string          `json:"d"`
	URL      string          `json:"u"`
	Referrer string          `json:"r"`
	Width    int             `json:"w"`
	Engaged  float64         `json:"e"` // seconds on page, engagement only
	Props    json.RawMessage `json:"p"` // accepted and ignored; see below
}

// raw is an accepted event awaiting enrichment on a worker.
type raw struct {
	site *config.Site
	p    payload
	ip   netip.Addr
	ua   string
	at   time.Time
}

// Options configures the ingest server.
type Options struct {
	Registry *aggregate.Registry
	Config   *config.Store
	Geo      geo.Provider
	Salt     *Salt
	Logger   *slog.Logger
}

// Server accepts events and aggregates them asynchronously.
type Server struct {
	opt     Options
	queue   chan raw
	limiter *Limiter
	wg      sync.WaitGroup
	stop    chan struct{}
	once    sync.Once
}

// NewServer builds the ingest server and starts its workers.
func NewServer(opt Options) *Server {
	cfg := opt.Config.Get()
	s := &Server{
		opt:     opt,
		queue:   make(chan raw, cfg.Server.QueueSize),
		limiter: NewLimiter(cfg.Server.RatePerMin, 200_000),
		stop:    make(chan struct{}),
	}
	workers := cfg.Server.Workers
	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	return s
}

// Close drains the queue and stops the workers.
func (s *Server) Close() {
	s.once.Do(func() {
		close(s.stop)
		close(s.queue)
		s.wg.Wait()
	})
}

// Sweep drops idle rate-limiter buckets.
func (s *Server) Sweep(now time.Time) { s.limiter.Sweep(now) }

// Routes returns the public ingest mux. Nothing here can read aggregates back
// out — that lives on the metrics listener.
func (s *Server) Routes(counterJS []byte, serveJS bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/event", s.handleEvent)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	if serveJS && len(counterJS) > 0 {
		mux.HandleFunc("/counter.js", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			http.ServeContent(w, r, "counter.js", time.Time{}, strings.NewReader(string(counterJS)))
		})
	}
	return mux
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() { s.opt.Registry.IngestLatency.Observe(time.Since(start).Seconds()) }()

	origin := r.Header.Get("Origin")

	if r.Method == http.MethodOptions {
		s.writeCORS(w, origin)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := s.opt.Config.Get()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cfg.Server.MaxBodyD))
	if err != nil {
		s.reject(w, origin, "", "malformed", http.StatusRequestEntityTooLarge)
		return
	}

	var p payload
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		s.reject(w, origin, "", "malformed", http.StatusBadRequest)
		return
	}

	p.Domain = strings.ToLower(strings.TrimSpace(p.Domain))
	site := cfg.Site(p.Domain)
	if site == nil {
		s.reject(w, origin, p.Domain, "unknown_site", http.StatusNotFound)
		return
	}
	if !site.AllowsOrigin(origin) {
		s.reject(w, origin, site.Domain, "rejected_origin", http.StatusForbidden)
		return
	}
	if site.HMACKey != "" && !validSignature(site.HMACKey, body, r.Header.Get("X-Signature")) {
		s.reject(w, origin, site.Domain, "bad_signature", http.StatusForbidden)
		return
	}
	if err := validate(&p); err != nil {
		s.reject(w, origin, site.Domain, "malformed", http.StatusBadRequest)
		return
	}

	ua := r.Header.Get("User-Agent")
	if enrich.ParseUA(ua).Bot {
		// Counted, not silently dropped: silent drops are how you spend a
		// Saturday debugging traffic that never arrives.
		s.reject(w, origin, site.Domain, "bot", http.StatusAccepted)
		return
	}

	ip := ClientIP(r, cfg.Server.TrustProxy)
	now := time.Now()
	if !s.limiter.Allow(rateKey(ip), now) {
		s.reject(w, origin, site.Domain, "rate_limited", http.StatusTooManyRequests)
		return
	}

	ev := raw{site: site, p: p, ip: ip, ua: ua, at: now}
	select {
	case s.queue <- ev:
		s.opt.Registry.IngestEvents.WithLabelValues(site.Domain, "accepted").Inc()
	default:
		// Never block the browser on our own backlog.
		s.opt.Registry.IngestEvents.WithLabelValues(site.Domain, "queue_full").Inc()
	}
	s.opt.Registry.QueueDepth.Set(float64(len(s.queue)))

	s.writeCORS(w, origin)
	w.WriteHeader(http.StatusAccepted)
}

// reject records the outcome and answers. Note that most rejections still
// return 2xx-or-4xx quickly and never reveal why in the body — the endpoint
// is public, and detailed errors are a probing oracle.
func (s *Server) reject(w http.ResponseWriter, origin, site, reason string, code int) {
	if site == "" {
		site = "unknown"
	}
	s.opt.Registry.IngestEvents.WithLabelValues(site, reason).Inc()
	s.writeCORS(w, origin)
	w.WriteHeader(code)
}

func (s *Server) writeCORS(w http.ResponseWriter, origin string) {
	if origin == "" {
		origin = "*"
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, X-Signature")
	h.Set("Access-Control-Max-Age", "86400")
	h.Set("Vary", "Origin")
}

var errBadPayload = errors.New("bad payload")

func validate(p *payload) error {
	p.Name = strings.ToLower(strings.TrimSpace(p.Name))
	if p.Name == "" || len(p.Name) > 64 {
		return errBadPayload
	}
	if p.URL == "" || len(p.URL) > 2048 {
		return errBadPayload
	}
	if len(p.Referrer) > 2048 {
		p.Referrer = ""
	}
	if p.Width < 0 || p.Width > 30000 {
		p.Width = 0
	}
	// Engagement over an hour is a stuck tab, not a reader.
	if p.Engaged < 0 || p.Engaged > 3600 {
		p.Engaged = 0
	}
	return nil
}

func validSignature(key string, body []byte, sig string) bool {
	want := hmac.New(sha256.New, []byte(key))
	want.Write(body)
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	return hmac.Equal(want.Sum(nil), got)
}

// worker enriches and records queued events.
func (s *Server) worker() {
	defer s.wg.Done()
	for ev := range s.queue {
		s.opt.Registry.Record(s.enrichEvent(ev))
	}
}

// enrichEvent turns a raw event into a fully resolved observation. This is
// the only place an IP or User-Agent is read, and neither survives the call.
func (s *Server) enrichEvent(ev raw) aggregate.Observation {
	site := ev.site

	path := site.Rewrite(enrich.NormalizePath(ev.p.URL))
	o := aggregate.Observation{
		Site:      site.Domain,
		Name:      ev.p.Name,
		Path:      path,
		VisitorID: s.opt.Salt.VisitorID(ev.ip, ev.ua, site.Domain),
		At:        ev.at,
	}

	switch ev.p.Name {
	case "pageview":
		o.Kind = aggregate.KindPageview
	case "engagement":
		o.Kind = aggregate.KindEngagement
		o.TimeOnPage = time.Duration(ev.p.Engaged * float64(time.Second))
		return o
	default:
		o.Kind = aggregate.KindCustom
		return o
	}

	utm := enrich.ParseUTM(ev.p.URL)
	o.Source, o.Medium, o.Campaign = utm.Source, utm.Medium, utm.Campaign
	o.Referrer = enrich.ReferrerHost(ev.p.Referrer, site.Domain)
	// A referrer with no campaign tag is itself the acquisition source.
	if o.Source == "" && o.Referrer != "" {
		o.Source = o.Referrer
	}

	c := enrich.ParseUA(ev.ua)
	o.Browser, o.OS, o.Device = c.Browser, c.OS, c.Device
	o.Screen = enrich.ScreenClass(ev.p.Width)

	if s.opt.Geo != nil {
		g := s.opt.Geo.Lookup(ev.ip)
		o.Country, o.Region, o.City = g.Country, g.Region, g.City
		o.ASN, o.ASOrg = g.ASN, g.ASOrg
		if g.Country == "" && g.ASN == "" {
			s.opt.Registry.GeoLookups.WithLabelValues("miss").Inc()
		} else {
			s.opt.Registry.GeoLookups.WithLabelValues("hit").Inc()
		}
	}
	return o
}
