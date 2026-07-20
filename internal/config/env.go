package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix is prepended to every configuration variable.
const EnvPrefix = "RIDICULYTICS_"

// env reads prefixed environment variables, accumulating errors so a container
// with three typos reports all three at once instead of one per restart.
type env struct {
	get  func(string) string
	errs []error
}

func (e *env) fail(key, val string, err error) {
	e.errs = append(e.errs, fmt.Errorf("%s%s=%q: %w", EnvPrefix, key, val, err))
}

// raw returns the value and whether it was set to something non-empty.
func (e *env) raw(key string) (string, bool) {
	v := strings.TrimSpace(e.get(EnvPrefix + key))
	return v, v != ""
}

func (e *env) str(key string, dst *string) {
	if v, ok := e.raw(key); ok {
		*dst = v
	}
}

func (e *env) int(key string, dst *int) {
	v, ok := e.raw(key)
	if !ok {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.fail(key, v, fmt.Errorf("want an integer"))
		return
	}
	*dst = n
}

func (e *env) int64(key string, dst *int64) {
	v, ok := e.raw(key)
	if !ok {
		return
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		e.fail(key, v, fmt.Errorf("want an integer"))
		return
	}
	*dst = n
}

func (e *env) uint32(key string, dst *uint32) {
	v, ok := e.raw(key)
	if !ok {
		return
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		e.fail(key, v, fmt.Errorf("want a non-negative integer"))
		return
	}
	*dst = uint32(n)
}

func (e *env) bool(key string, dst *bool) {
	v, ok := e.raw(key)
	if !ok {
		return
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.fail(key, v, fmt.Errorf("want true or false"))
		return
	}
	*dst = b
}

func (e *env) duration(key string, dst *time.Duration) {
	v, ok := e.raw(key)
	if !ok {
		return
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.fail(key, v, fmt.Errorf("want a Go duration such as 30m or 168h"))
		return
	}
	*dst = d
}

// list parses a comma-separated value. An explicit empty marker is honoured so
// a list can be cleared, which is otherwise impossible: an empty variable is
// indistinguishable from an unset one.
func (e *env) list(key string, dst *[]string) {
	v, ok := e.raw(key)
	if !ok {
		return
	}
	if v == "-" || strings.EqualFold(v, "none") {
		*dst = []string{}
		return
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	*dst = out
}

// siteKey converts a domain into its environment-variable infix:
// blog.example.co.uk -> BLOG_EXAMPLE_CO_UK
func siteKey(domain string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(domain)) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// applyEnv overlays environment variables onto an existing config. Environment
// always wins over the file, so a compose deployment can override anything
// baked into an image without editing YAML.
func applyEnv(c *Config, getenv func(string) string) error {
	e := &env{get: getenv}

	e.str("INGEST_ADDR", &c.Server.IngestAddr)
	e.str("METRICS_ADDR", &c.Server.MetricsAddr)
	e.int("QUEUE_SIZE", &c.Server.QueueSize)
	e.int("WORKERS", &c.Server.Workers)
	e.int64("MAX_BODY_BYTES", &c.Server.MaxBodyD)
	e.int("RATE_PER_MIN", &c.Server.RatePerMin)
	e.duration("SALT_ROTATE", &c.Server.SaltRotate)
	e.bool("SERVE_COUNTER_JS", &c.Server.ServeJS)
	e.bool("TRUST_PROXY", &c.Server.TrustProxy)

	e.str("GEO_PROVIDER", &c.Geo.Provider)
	e.str("GEO_CITY_DB", &c.Geo.CityDB)
	e.str("GEO_COUNTRY_DB", &c.Geo.CountryDB)
	e.str("GEO_ASN_DB", &c.Geo.ASNDB)

	applyEnvDefaults(e, "", &c.Defaults)

	// Sites listed in the environment are created if absent and overlaid if
	// already present in the file.
	var domains []string
	e.list("SITES", &domains)
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		s := findSite(c, d)
		if s == nil {
			s = &Site{Domain: d, Defaults: c.Defaults}
			c.Sites = append(c.Sites, s)
		}
		prefix := "SITE_" + siteKey(d) + "_"

		e.list(prefix+"ORIGINS", &s.Origins)
		e.str(prefix+"HMAC_KEY", &s.HMACKey)
		applyEnvDefaults(e, prefix, &s.Defaults)

		// Sensible default so a single-site deployment needs one variable.
		if len(s.Origins) == 0 {
			s.Origins = []string{"https://" + d, "https://www." + d}
		}
	}

	if len(e.errs) > 0 {
		msgs := make([]string, len(e.errs))
		for i, err := range e.errs {
			msgs[i] = err.Error()
		}
		return fmt.Errorf("invalid environment configuration:\n  %s", strings.Join(msgs, "\n  "))
	}
	return nil
}

// applyEnvDefaults overlays the tunables, either globally (prefix "") or for
// one site (prefix "SITE_EXAMPLE_COM_").
func applyEnvDefaults(e *env, prefix string, d *Defaults) {
	e.int(prefix+"PATH_CAP", &d.PathCap)
	e.int(prefix+"CROSS_PATH_CAP", &d.CrossPathCap)
	e.int(prefix+"REFERRER_CAP", &d.ReferrerCap)
	e.int(prefix+"ASN_CAP", &d.ASNCap)
	e.int(prefix+"CAMPAIGN_CAP", &d.CampaignCap)
	e.int(prefix+"EVENT_CAP", &d.EventCap)
	e.uint32(prefix+"ADMIT_MIN_COUNT", &d.AdmitMinCount)
	e.list(prefix+"PATH_LABELS", &d.PathLabels)
	e.bool(prefix+"UNIQUE_VISITORS_BY_PATH", &d.UniqueVisitorsByPath)
	e.bool(prefix+"REGION_ENABLED", &d.RegionEnabled)
	e.bool(prefix+"CITY_ENABLED", &d.CityEnabled)
	e.duration(prefix+"SESSION_TTL", &d.SessionTTL)
	e.duration(prefix+"DECAY_AFTER", &d.DecayAfter)
}

func findSite(c *Config, domain string) *Site {
	for _, s := range c.Sites {
		if strings.EqualFold(s.Domain, domain) {
			return s
		}
	}
	return nil
}

// LoadEnv builds a configuration entirely from environment variables, with no
// file involved. This is the intended path for container deployments.
func LoadEnv() (*Config, error) {
	c := newConfig()
	if err := applyEnv(c, os.Getenv); err != nil {
		return nil, err
	}
	if err := finalize(c); err != nil {
		return nil, err
	}
	return c, nil
}
