// Package config loads sites.yaml and exposes it as a hot-reloadable snapshot.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// Dimension names usable in path_labels.
const (
	DimCountry  = "country"
	DimRegion   = "region"
	DimCity     = "city"
	DimASN      = "asn"
	DimReferrer = "referrer"
	DimSource   = "source"
	DimMedium   = "medium"
	DimCampaign = "campaign"
	DimBrowser  = "browser"
	DimOS       = "os"
	DimDevice   = "device"
	DimScreen   = "screen"
	DimEvent    = "event"
)

// AllDimensions is every family that can carry a path label.
var AllDimensions = []string{
	DimCountry, DimASN, DimReferrer, DimSource, DimMedium, DimCampaign,
	DimBrowser, DimOS, DimDevice, DimScreen, DimEvent,
}

// Defaults are the per-site knobs, overridable per site.
type Defaults struct {
	PathCap      int `yaml:"path_cap"`
	CrossPathCap int `yaml:"cross_path_cap"`
	ReferrerCap  int `yaml:"referrer_cap"`
	ASNCap       int `yaml:"asn_cap"`
	CampaignCap  int `yaml:"campaign_cap"`
	EventCap     int `yaml:"event_cap"`

	// PathLabels lists dimensions that carry a secondary path label.
	PathLabels []string `yaml:"path_labels"`

	// UniqueVisitorsByPath enables the coarse per-path 24h HLL.
	UniqueVisitorsByPath bool `yaml:"unique_visitors_by_path"`

	// AdmitMinCount is how often a value must be seen before it earns its own
	// series. Guards against a crawler squatting every slot on arrival.
	AdmitMinCount uint32 `yaml:"admit_min_count"`

	SessionTTL time.Duration `yaml:"session_ttl"`
	DecayAfter time.Duration `yaml:"decay_after"`

	RegionEnabled bool `yaml:"region_enabled"`
	CityEnabled   bool `yaml:"city_enabled"`
}

// Site is one tracked domain.
type Site struct {
	Domain  string   `yaml:"domain"`
	Origins []string `yaml:"origins"`

	// PathRewrites are applied in order after normalization.
	PathRewrites []Rewrite `yaml:"path_rewrites"`

	// HMACKey, if set, requires events to carry a valid signature.
	HMACKey string `yaml:"hmac_key"`

	Defaults `yaml:",inline"`

	originSet map[string]struct{}
}

// Rewrite is a regex path rewrite rule.
type Rewrite struct {
	Match   string `yaml:"match"`
	Replace string `yaml:"replace"`
	re      *regexp.Regexp
}

// Server holds listener and runtime settings.
type Server struct {
	IngestAddr  string        `yaml:"ingest_addr"`
	MetricsAddr string        `yaml:"metrics_addr"`
	QueueSize   int           `yaml:"queue_size"`
	Workers     int           `yaml:"workers"`
	MaxBodyD    int64         `yaml:"max_body_bytes"`
	RatePerMin  int           `yaml:"rate_per_min"`
	SaltRotate  time.Duration `yaml:"salt_rotate"`
	ServeJS     bool          `yaml:"serve_counter_js"`
	TrustProxy  bool          `yaml:"trust_proxy"`
}

// Geo selects the geolocation provider.
type Geo struct {
	Provider  string `yaml:"provider"` // dbip | maxmind | none
	CityDB    string `yaml:"city_db"`
	ASNDB     string `yaml:"asn_db"`
	CountryDB string `yaml:"country_db"`
}

// Config is the whole file.
type Config struct {
	Server   Server   `yaml:"server"`
	Geo      Geo      `yaml:"geo"`
	Defaults Defaults `yaml:"defaults"`
	Sites    []*Site  `yaml:"sites"`

	sites map[string]*Site
}

// Site returns the config for a domain, or nil if untracked.
func (c *Config) Site(domain string) *Site {
	return c.sites[strings.ToLower(domain)]
}

// AllowsOrigin reports whether origin may post events for this site.
//
// An empty allowlist denies everything rather than allowing everything: the
// loader already rejects a site with no origins, so reaching this state means
// something went wrong, and failing open would silently turn the allowlist off.
func (s *Site) AllowsOrigin(origin string) bool {
	if len(s.originSet) == 0 {
		return false
	}
	_, ok := s.originSet[strings.ToLower(strings.TrimSuffix(origin, "/"))]
	return ok
}

// HasPathLabel reports whether a dimension carries the secondary path label.
func (s *Site) HasPathLabel(dim string) bool {
	for _, d := range s.PathLabels {
		if d == dim {
			return true
		}
	}
	return false
}

// Rewrite applies the site's configured path rewrites.
func (s *Site) Rewrite(path string) string {
	for _, r := range s.PathRewrites {
		if r.re != nil {
			path = r.re.ReplaceAllString(path, r.Replace)
		}
	}
	return path
}

func defaults() Defaults {
	return Defaults{
		PathCap:       200,
		CrossPathCap:  50,
		ReferrerCap:   200,
		ASNCap:        200,
		CampaignCap:   100,
		EventCap:      50,
		AdmitMinCount: 3,
		SessionTTL:    30 * time.Minute,
		DecayAfter:    7 * 24 * time.Hour,
		PathLabels: []string{
			DimCountry, DimReferrer, DimSource, DimMedium, DimCampaign,
			DimBrowser, DimOS, DimDevice, DimScreen, DimEvent,
		},
	}
}

// newConfig returns a config populated with defaults and nothing else.
func newConfig() *Config {
	c := &Config{
		Server: Server{
			IngestAddr:  ":8080",
			MetricsAddr: "127.0.0.1:9090",
			QueueSize:   8192,
			Workers:     4,
			MaxBodyD:    4 << 10,
			RatePerMin:  600,
			SaltRotate:  24 * time.Hour,
			ServeJS:     true,
		},
		// dbip with no paths means "use whatever databases you can find".
		// The container ships DB-IP Lite, so this is what makes country and
		// ASN work out of the box; where no database exists the provider
		// degrades to the same empty result "none" would have produced.
		Geo:      Geo{Provider: "dbip"},
		Defaults: defaults(),
	}
	// Sentinel so we can tell "absent" from "explicitly empty" for PathLabels.
	c.Defaults.PathLabels = nil
	return c
}

// Load reads a config file and overlays environment variables on top of it.
// Environment always wins, so an image can ship a sensible YAML and a
// deployment can override any of it without editing files.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := newConfig()
	if err := yaml.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := applyEnv(c, os.Getenv); err != nil {
		return nil, err
	}
	if err := finalize(c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// finalize applies defaults, validates, and builds the lookup maps. Both the
// file and the environment path go through this, so neither can produce a
// config the other would have rejected.
func finalize(c *Config) error {
	if c.Defaults.PathLabels == nil {
		c.Defaults.PathLabels = defaults().PathLabels
	}
	fillZero(&c.Defaults, defaults())

	if len(c.Sites) == 0 {
		return fmt.Errorf("no sites configured (set %sSITES or list sites in the config file)", EnvPrefix)
	}

	c.sites = make(map[string]*Site, len(c.Sites))
	for _, s := range c.Sites {
		s.Domain = strings.ToLower(strings.TrimSpace(s.Domain))
		if s.Domain == "" {
			return fmt.Errorf("site with empty domain")
		}
		if _, dup := c.sites[s.Domain]; dup {
			return fmt.Errorf("duplicate site %q", s.Domain)
		}
		if s.PathLabels == nil {
			s.PathLabels = c.Defaults.PathLabels
		}
		fillZero(&s.Defaults, c.Defaults)

		for _, d := range s.PathLabels {
			if !validDimension(d) {
				return fmt.Errorf("site %s: unknown dimension %q in path_labels (valid: %s)",
					s.Domain, d, strings.Join(AllDimensions, ", "))
			}
		}
		if len(s.Origins) == 0 {
			return fmt.Errorf("site %s: no origins configured; an empty allowlist accepts events "+
				"from anywhere, which is almost never what you want in production", s.Domain)
		}
		s.originSet = make(map[string]struct{}, len(s.Origins))
		for _, o := range s.Origins {
			s.originSet[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(o), "/"))] = struct{}{}
		}
		for i := range s.PathRewrites {
			re, err := regexp.Compile(s.PathRewrites[i].Match)
			if err != nil {
				return fmt.Errorf("site %s: path_rewrite %q: %w", s.Domain, s.PathRewrites[i].Match, err)
			}
			s.PathRewrites[i].re = re
		}
		c.sites[s.Domain] = s
	}
	return nil
}

// fillZero copies defaults into any field left at its zero value.
func fillZero(d *Defaults, def Defaults) {
	if d.PathCap == 0 {
		d.PathCap = def.PathCap
	}
	if d.CrossPathCap == 0 {
		d.CrossPathCap = def.CrossPathCap
	}
	if d.ReferrerCap == 0 {
		d.ReferrerCap = def.ReferrerCap
	}
	if d.ASNCap == 0 {
		d.ASNCap = def.ASNCap
	}
	if d.CampaignCap == 0 {
		d.CampaignCap = def.CampaignCap
	}
	if d.EventCap == 0 {
		d.EventCap = def.EventCap
	}
	if d.AdmitMinCount == 0 {
		d.AdmitMinCount = def.AdmitMinCount
	}
	if d.SessionTTL == 0 {
		d.SessionTTL = def.SessionTTL
	}
	if d.DecayAfter == 0 {
		d.DecayAfter = def.DecayAfter
	}
}

func validDimension(d string) bool {
	for _, k := range AllDimensions {
		if k == d {
			return true
		}
	}
	return false
}

// Store holds the live config for hot reload.
type Store struct {
	path string
	cur  atomic.Pointer[Config]
}

// NewStore loads a reloadable config. An empty path means environment-only,
// which is the intended container deployment.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	c, err := s.load()
	if err != nil {
		return nil, err
	}
	s.cur.Store(c)
	return s, nil
}

func (s *Store) load() (*Config, error) {
	if s.path == "" {
		return LoadEnv()
	}
	return Load(s.path)
}

// Get returns the current config snapshot.
func (s *Store) Get() *Config { return s.cur.Load() }

// Reload re-reads the configuration. On error the previous config is retained,
// so a bad edit plus SIGHUP degrades to a log line rather than an outage.
func (s *Store) Reload() error {
	c, err := s.load()
	if err != nil {
		return err
	}
	s.cur.Store(c)
	return nil
}
