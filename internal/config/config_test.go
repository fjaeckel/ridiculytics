package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sites.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadYAML(t *testing.T) {
	p := writeConfig(t, `
server:
  ingest_addr: ":9000"
  trust_proxy: true
geo:
  provider: dbip
  city_db: /geo/city.mmdb
defaults:
  cross_path_cap: 25
  path_labels: [country, referrer]
sites:
  - domain: Example.COM
    origins: ["https://example.com/", "https://www.example.com"]
  - domain: other.example
    origins: ["https://other.example"]
    cross_path_cap: 100
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}

	if c.Server.IngestAddr != ":9000" || !c.Server.TrustProxy {
		t.Errorf("server = %+v", c.Server)
	}
	if c.Geo.Provider != "dbip" || c.Geo.CityDB != "/geo/city.mmdb" {
		t.Errorf("geo = %+v", c.Geo)
	}

	// Domains are normalized, so lookups are case-insensitive.
	s := c.Site("example.com")
	if s == nil {
		t.Fatal("site lookup should be case-insensitive")
	}
	if s.CrossPathCap != 25 {
		t.Errorf("cross path cap = %d, want the global 25", s.CrossPathCap)
	}
	if !s.HasPathLabel(DimCountry) || s.HasPathLabel(DimDevice) {
		t.Errorf("path labels = %v, want only country and referrer", s.PathLabels)
	}
	// Trailing slashes are stripped when matching origins.
	if !s.AllowsOrigin("https://example.com") {
		t.Error("origin with a trailing slash in config should still match")
	}

	if o := c.Site("other.example"); o.CrossPathCap != 100 {
		t.Errorf("per-site cap = %d, want 100", o.CrossPathCap)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	p := writeConfig(t, `
sites:
  - domain: example.com
    origins: ["https://example.com"]
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s := c.Site("example.com")
	if s.PathCap != 200 || s.CrossPathCap != 50 || s.AdmitMinCount != 3 {
		t.Errorf("defaults not applied: %+v", s.Defaults)
	}
	if s.SessionTTL != 30*time.Minute || s.DecayAfter != 7*24*time.Hour {
		t.Errorf("duration defaults not applied: %+v", s.Defaults)
	}
	if len(s.PathLabels) == 0 {
		t.Error("default path labels should be populated")
	}
	if s.HasPathLabel(DimASN) {
		t.Error("asn should not carry a path label by default")
	}
	// The container ships DB-IP Lite and the binary discovers it by path, so
	// the default provider has to be dbip. Defaulting to none would mean the
	// databases in the image are never opened.
	if c.Geo.Provider != "dbip" {
		t.Errorf("default geo provider = %q, want dbip", c.Geo.Provider)
	}
}

func TestLoadRejectsBadConfigs(t *testing.T) {
	cases := map[string]string{
		"no sites":       `server: {workers: 2}`,
		"empty domain":   "sites:\n  - domain: \"\"\n    origins: [\"https://x\"]",
		"duplicate site": "sites:\n  - {domain: a.example, origins: [\"https://a\"]}\n  - {domain: a.example, origins: [\"https://b\"]}",
		"no origins":     "sites:\n  - domain: a.example",
		"bad dimension":  "defaults:\n  path_labels: [country, nope]\nsites:\n  - {domain: a.example, origins: [\"https://a\"]}",
		"bad rewrite":    "sites:\n  - domain: a.example\n    origins: [\"https://a\"]\n    path_rewrites:\n      - match: \"[\"\n        replace: x",
		"malformed yaml": "sites: [oops",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

func TestPathRewrites(t *testing.T) {
	p := writeConfig(t, `
sites:
  - domain: example.com
    origins: ["https://example.com"]
    path_rewrites:
      - match: "^/products/[^/]+$"
        replace: "/products/:slug"
      - match: "^/u/[^/]+/posts$"
        replace: "/u/:user/posts"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s := c.Site("example.com")

	cases := map[string]string{
		"/products/blue-widget": "/products/:slug",
		"/u/alice/posts":        "/u/:user/posts",
		"/about":                "/about", // untouched
	}
	for in, want := range cases {
		if got := s.Rewrite(in); got != want {
			t.Errorf("Rewrite(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnknownSiteReturnsNil(t *testing.T) {
	p := writeConfig(t, "sites:\n  - {domain: a.example, origins: [\"https://a\"]}")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Site("unknown.example") != nil {
		t.Error("an unconfigured domain must not resolve")
	}
}

func TestStoreReloadKeepsPreviousOnError(t *testing.T) {
	p := writeConfig(t, `
server: {ingest_addr: ":8080"}
sites:
  - {domain: a.example, origins: ["https://a.example"]}
`)
	store, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if store.Get().Server.IngestAddr != ":8080" {
		t.Fatal("setup")
	}

	// A broken edit plus a reload must be a logged error, not an outage.
	if err := os.WriteFile(p, []byte("sites: [oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Reload(); err == nil {
		t.Error("expected reload to fail on malformed YAML")
	}
	if got := store.Get().Server.IngestAddr; got != ":8080" {
		t.Errorf("ingest addr = %q after a failed reload, want the previous value", got)
	}
	if store.Get().Site("a.example") == nil {
		t.Error("previous sites must survive a failed reload")
	}
}

func TestStoreReloadAppliesValidChanges(t *testing.T) {
	p := writeConfig(t, "sites:\n  - {domain: a.example, origins: [\"https://a.example\"]}")
	store, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}

	body := "sites:\n  - {domain: a.example, origins: [\"https://a.example\"]}\n" +
		"  - {domain: b.example, origins: [\"https://b.example\"]}"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	if store.Get().Site("b.example") == nil {
		t.Error("a site added by reload should be live")
	}
}

func TestAllowsOriginIsCaseInsensitive(t *testing.T) {
	p := writeConfig(t, "sites:\n  - {domain: a.example, origins: [\"https://A.Example\"]}")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Site("a.example").AllowsOrigin("https://a.example") {
		t.Error("origin matching should be case-insensitive")
	}
}

func TestErrorMentionsFile(t *testing.T) {
	p := writeConfig(t, "sites:\n  - domain: a.example")
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "sites.yaml") {
		t.Errorf("error should name the offending file, got: %v", err)
	}
}
