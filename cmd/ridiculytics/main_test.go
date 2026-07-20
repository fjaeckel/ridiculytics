package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fjaeckel/ridiculytics/internal/config"
	"github.com/fjaeckel/ridiculytics/internal/geo"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestOpenGeoProviders(t *testing.T) {
	log := quietLogger()

	t.Run("none", func(t *testing.T) {
		p, err := openGeo(config.Geo{Provider: "none"}, log)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		if _, ok := p.(geo.Null); !ok {
			t.Errorf("provider = %T, want geo.Null", p)
		}
	})

	// An unset provider must behave like "none" rather than failing: running
	// without geolocation is a supported configuration.
	t.Run("empty defaults to none", func(t *testing.T) {
		p, err := openGeo(config.Geo{}, log)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		if _, ok := p.(geo.Null); !ok {
			t.Errorf("provider = %T, want geo.Null", p)
		}
	})

	t.Run("unknown provider is rejected", func(t *testing.T) {
		_, err := openGeo(config.Geo{Provider: "geocities"}, log)
		if err == nil {
			t.Fatal("expected an error for an unknown provider")
		}
		// The message should say what is actually valid.
		for _, want := range []string{"dbip", "maxmind", "none"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should list %q as valid; got: %v", want, err)
			}
		}
	})

	t.Run("missing database fails loudly", func(t *testing.T) {
		_, err := openGeo(config.Geo{
			Provider: "dbip",
			CityDB:   filepath.Join(t.TempDir(), "absent.mmdb"),
		}, log)
		if err == nil {
			t.Fatal("a configured-but-absent database must fail at startup, not silently disable geo")
		}
	})

	// Selecting a provider with no database paths is allowed: it lets an
	// operator enable geo before the download lands.
	t.Run("provider without databases", func(t *testing.T) {
		p, err := openGeo(config.Geo{Provider: "dbip"}, log)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
	})
}

// TestConfigFlagDefaultsToEnv pins the container-first behaviour: with no
// -config flag the process must be configurable entirely from the environment.
func TestConfigFlagDefaultsToEnv(t *testing.T) {
	t.Setenv(config.EnvPrefix+"SITES", "example.com")

	store, err := config.NewStore("") // what main does with an empty -config
	if err != nil {
		t.Fatalf("env-only startup failed: %v", err)
	}
	if store.Get().Site("example.com") == nil {
		t.Error("site from the environment was not registered")
	}
}

// TestYAMLPathStillWorks keeps the file-based path from bit-rotting now that
// the environment is the primary interface.
func TestYAMLPathStillWorks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sites.yaml")
	body := "sites:\n  - {domain: file.example, origins: [\"https://file.example\"]}\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := config.NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if store.Get().Site("file.example") == nil {
		t.Error("site from the YAML file was not registered")
	}
}
