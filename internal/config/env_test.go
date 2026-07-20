package config

import (
	"strings"
	"testing"
	"time"
)

// fakeEnv builds a getenv func from a map, so tests never touch real env state.
func fakeEnv(vars map[string]string) func(string) string {
	return func(k string) string { return vars[strings.TrimPrefix(k, "")] }
}

func loadEnv(t *testing.T, vars map[string]string) (*Config, error) {
	t.Helper()
	full := make(map[string]string, len(vars))
	for k, v := range vars {
		full[EnvPrefix+k] = v
	}
	c := newConfig()
	if err := applyEnv(c, fakeEnv(full)); err != nil {
		return nil, err
	}
	if err := finalize(c); err != nil {
		return nil, err
	}
	return c, nil
}

func TestMinimalEnvConfig(t *testing.T) {
	c, err := loadEnv(t, map[string]string{"SITES": "example.com"})
	if err != nil {
		t.Fatalf("a single SITES variable should be enough: %v", err)
	}
	s := c.Site("example.com")
	if s == nil {
		t.Fatal("site not registered")
	}
	// Origins should be inferred so one variable is genuinely enough.
	if !s.AllowsOrigin("https://example.com") || !s.AllowsOrigin("https://www.example.com") {
		t.Errorf("expected inferred origins, got %v", s.Origins)
	}
	if s.AllowsOrigin("https://evil.example") {
		t.Error("inferred origins must not accept a foreign origin")
	}
	if s.CrossPathCap != 50 || s.PathCap != 200 {
		t.Errorf("defaults not applied: cross=%d path=%d", s.CrossPathCap, s.PathCap)
	}
}

func TestEnvOverridesServerAndDefaults(t *testing.T) {
	c, err := loadEnv(t, map[string]string{
		"SITES":          "example.com",
		"INGEST_ADDR":    "0.0.0.0:9999",
		"METRICS_ADDR":   "127.0.0.1:1234",
		"TRUST_PROXY":    "true",
		"WORKERS":        "8",
		"RATE_PER_MIN":   "120",
		"MAX_BODY_BYTES": "2048",
		"SALT_ROTATE":    "12h",
		"CROSS_PATH_CAP": "25",
		"SESSION_TTL":    "15m",
		"GEO_PROVIDER":   "dbip",
		"GEO_CITY_DB":    "/geo/city.mmdb",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.IngestAddr != "0.0.0.0:9999" || c.Server.MetricsAddr != "127.0.0.1:1234" {
		t.Errorf("listener addresses not applied: %+v", c.Server)
	}
	if !c.Server.TrustProxy || c.Server.Workers != 8 || c.Server.RatePerMin != 120 {
		t.Errorf("server tunables not applied: %+v", c.Server)
	}
	if c.Server.MaxBodyD != 2048 || c.Server.SaltRotate != 12*time.Hour {
		t.Errorf("body cap / salt rotation not applied: %+v", c.Server)
	}
	if c.Geo.Provider != "dbip" || c.Geo.CityDB != "/geo/city.mmdb" {
		t.Errorf("geo not applied: %+v", c.Geo)
	}
	if s := c.Site("example.com"); s.CrossPathCap != 25 || s.SessionTTL != 15*time.Minute {
		t.Errorf("global defaults did not reach the site: cross=%d ttl=%v", s.CrossPathCap, s.SessionTTL)
	}
}

func TestPerSiteEnvOverrides(t *testing.T) {
	c, err := loadEnv(t, map[string]string{
		"SITES":          "example.com,blog.example.co.uk",
		"CROSS_PATH_CAP": "50",

		"SITE_EXAMPLE_COM_ORIGINS":        "https://example.com,https://staging.example.com",
		"SITE_EXAMPLE_COM_CROSS_PATH_CAP": "10",
		"SITE_EXAMPLE_COM_HMAC_KEY":       "s3cret",

		// Dots become underscores, so a multi-label domain is addressable.
		"SITE_BLOG_EXAMPLE_CO_UK_PATH_LABELS":             "country,referrer",
		"SITE_BLOG_EXAMPLE_CO_UK_UNIQUE_VISITORS_BY_PATH": "true",
	})
	if err != nil {
		t.Fatal(err)
	}

	a := c.Site("example.com")
	if a.CrossPathCap != 10 {
		t.Errorf("per-site cap = %d, want 10 (site must win over global)", a.CrossPathCap)
	}
	if a.HMACKey != "s3cret" {
		t.Errorf("hmac key = %q", a.HMACKey)
	}
	if !a.AllowsOrigin("https://staging.example.com") {
		t.Error("explicit origins not applied")
	}
	if a.AllowsOrigin("https://www.example.com") {
		t.Error("explicit origins must replace the inferred ones, not extend them")
	}

	b := c.Site("blog.example.co.uk")
	if b.CrossPathCap != 50 {
		t.Errorf("site without an override should inherit global, got %d", b.CrossPathCap)
	}
	if len(b.PathLabels) != 2 || !b.HasPathLabel(DimCountry) || !b.HasPathLabel(DimReferrer) {
		t.Errorf("path labels = %v, want [country referrer]", b.PathLabels)
	}
	if b.HasPathLabel(DimBrowser) {
		t.Error("path_labels must replace the default list, not add to it")
	}
	if !b.UniqueVisitorsByPath {
		t.Error("per-site bool not applied")
	}
}

// TestPathLabelsCanBeDisabled covers the case an empty variable cannot express:
// an unset variable and an empty one are indistinguishable, so clearing a list
// needs an explicit marker.
func TestPathLabelsCanBeDisabled(t *testing.T) {
	for _, marker := range []string{"-", "none", "NONE"} {
		c, err := loadEnv(t, map[string]string{
			"SITES":       "example.com",
			"PATH_LABELS": marker,
		})
		if err != nil {
			t.Fatalf("marker %q: %v", marker, err)
		}
		if got := c.Site("example.com").PathLabels; len(got) != 0 {
			t.Errorf("marker %q left path labels %v, want none", marker, got)
		}
	}
}

// TestInvalidEnvReportsEveryError at once, so a misconfigured container does
// not need one restart per typo.
func TestInvalidEnvReportsEveryError(t *testing.T) {
	_, err := loadEnv(t, map[string]string{
		"SITES":          "example.com",
		"WORKERS":        "lots",
		"TRUST_PROXY":    "yes-please",
		"SESSION_TTL":    "half an hour",
		"CROSS_PATH_CAP": "50",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"WORKERS", "TRUST_PROXY", "SESSION_TTL"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %s; got:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "CROSS_PATH_CAP") {
		t.Errorf("valid variable should not be reported:\n%s", msg)
	}
}

func TestUnknownPathLabelIsRejected(t *testing.T) {
	_, err := loadEnv(t, map[string]string{
		"SITES":       "example.com",
		"PATH_LABELS": "country,contry",
	})
	if err == nil || !strings.Contains(err.Error(), "contry") {
		t.Fatalf("expected a typo'd dimension to be rejected, got %v", err)
	}
}

func TestNoSitesIsAnError(t *testing.T) {
	if _, err := loadEnv(t, map[string]string{"WORKERS": "4"}); err == nil {
		t.Fatal("a config with no sites must fail rather than start deaf")
	}
}

func TestSiteKey(t *testing.T) {
	cases := map[string]string{
		"example.com":         "EXAMPLE_COM",
		"blog.example.co.uk":  "BLOG_EXAMPLE_CO_UK",
		"my-site.example.org": "MY_SITE_EXAMPLE_ORG",
	}
	for in, want := range cases {
		if got := siteKey(in); got != want {
			t.Errorf("siteKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEnvOverridesYAML pins the precedence rule: a deployment must be able to
// override anything baked into an image without editing files.
func TestEnvOverridesYAML(t *testing.T) {
	c := newConfig()
	c.Sites = []*Site{{Domain: "example.com", Origins: []string{"https://example.com"}}}
	c.Server.IngestAddr = ":8080"
	c.Defaults.CrossPathCap = 200

	err := applyEnv(c, fakeEnv(map[string]string{
		EnvPrefix + "INGEST_ADDR":    ":9000",
		EnvPrefix + "CROSS_PATH_CAP": "25",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := finalize(c); err != nil {
		t.Fatal(err)
	}
	if c.Server.IngestAddr != ":9000" {
		t.Errorf("ingest addr = %q, want the environment value", c.Server.IngestAddr)
	}
	if c.Site("example.com").CrossPathCap != 25 {
		t.Errorf("cross path cap = %d, want the environment value",
			c.Site("example.com").CrossPathCap)
	}
}

// TestEmptyOriginsRejected guards a fail-open hazard: a site whose allowlist
// is empty would otherwise accept events from anywhere.
func TestEmptyOriginsRejected(t *testing.T) {
	c := newConfig()
	c.Sites = []*Site{{Domain: "example.com"}}
	if err := finalize(c); err == nil {
		t.Fatal("a site with no origins must be rejected")
	}

	var s Site
	if s.AllowsOrigin("https://anything.example") {
		t.Error("an empty allowlist must deny, not allow")
	}
}
