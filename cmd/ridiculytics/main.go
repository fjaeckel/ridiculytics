// Command ridiculytics is a privacy-first web analytics collector that stores
// nothing and exposes everything as Prometheus metrics.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/fjaeckel/ridiculytics/internal/aggregate"
	"github.com/fjaeckel/ridiculytics/internal/config"
	"github.com/fjaeckel/ridiculytics/internal/geo"
	"github.com/fjaeckel/ridiculytics/internal/ingest"
	"github.com/fjaeckel/ridiculytics/web"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ridiculytics:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "",
		"path to a YAML config file; omit to configure entirely from "+config.EnvPrefix+"* environment variables")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("ridiculytics", version)
		return nil
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	store, err := config.NewStore(*cfgPath)
	if err != nil {
		return err
	}
	cfg := store.Get()

	geoProvider, err := openGeo(cfg.Geo, log)
	if err != nil {
		return err
	}
	defer func() { _ = geoProvider.Close() }()

	reg := aggregate.New(cfg)
	reg.SetGeoAgeFunc(geoProvider.DBAge)

	salt, err := ingest.NewSalt(cfg.Server.SaltRotate)
	if err != nil {
		return fmt.Errorf("seed salt: %w", err)
	}

	srv := ingest.NewServer(ingest.Options{
		Registry: reg, Config: store, Geo: geoProvider, Salt: salt, Logger: log,
	})
	defer srv.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Two listeners on two ports. The metrics port never needs to face the
	// internet, and nothing on the ingest port can read aggregates back out.
	ingestSrv := &http.Server{
		Addr:              cfg.Server.IngestAddr,
		Handler:           srv.Routes(web.CounterJS, cfg.Server.ServeJS),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(reg.Gatherer(), promhttp.HandlerOpts{
		ErrorHandling:     promhttp.ContinueOnError,
		EnableOpenMetrics: true,
	}))
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	metricsSrv := &http.Server{
		Addr:              cfg.Server.MetricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 2)
	go serve(ingestSrv, "ingest", log, errCh)
	go serve(metricsSrv, "metrics", log, errCh)

	go maintain(ctx, reg, srv, salt, log)
	go reload(ctx, store, reg, geoProvider, log)

	log.Info("ridiculytics started",
		"version", version,
		"ingest", cfg.Server.IngestAddr,
		"metrics", cfg.Server.MetricsAddr,
		"sites", len(cfg.Sites),
		"geo", cfg.Geo.Provider)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = ingestSrv.Shutdown(shutCtx)
	_ = metricsSrv.Shutdown(shutCtx)
	return nil
}

func serve(s *http.Server, name string, log *slog.Logger, errCh chan<- error) {
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listener failed", "listener", name, "addr", s.Addr, "err", err)
		errCh <- fmt.Errorf("%s listener: %w", name, err)
	}
}

// geoSearchDirs are scanned when a provider is selected but no database paths
// are given. The mount point is searched first: a database an operator mounted
// is refreshable, while one baked into the image is frozen at build time.
var geoSearchDirs = []string{
	"/var/lib/ridiculytics",
	"/usr/share/ridiculytics/geo",
}

// geoCandidates maps a database kind to the filenames it is published under,
// DB-IP Lite first and GeoLite2 second, so a mounted directory from either
// source is picked up without configuration.
var geoCandidates = map[string][]string{
	"city":    {"dbip-city-lite.mmdb", "GeoLite2-City.mmdb"},
	"country": {"dbip-country-lite.mmdb", "GeoLite2-Country.mmdb"},
	"asn":     {"dbip-asn-lite.mmdb", "GeoLite2-ASN.mmdb"},
}

// discoverGeo returns the first existing file for each database kind. Any of
// the results may be empty; geo.Open treats every path as optional.
func discoverGeo(dirs []string) (city, country, asn string) {
	find := func(kind string) string {
		for _, dir := range dirs {
			for _, name := range geoCandidates[kind] {
				p := filepath.Join(dir, name)
				if st, err := os.Stat(p); err == nil && !st.IsDir() {
					return p
				}
			}
		}
		return ""
	}
	return find("city"), find("country"), find("asn")
}

func openGeo(g config.Geo, log *slog.Logger) (geo.Provider, error) {
	switch g.Provider {
	case "", "none":
		log.Info("geolocation disabled; geo metric families will be absent")
		return geo.Null{}, nil
	case "dbip", "maxmind":
		city, country, asn := g.CityDB, g.CountryDB, g.ASNDB
		// Explicit configuration is never second-guessed: if any path is set,
		// those are the databases, and a missing one is a startup failure
		// rather than a silent fallback to whatever else is lying around.
		if city == "" && country == "" && asn == "" {
			city, country, asn = discoverGeo(geoSearchDirs)
			if city == "" && country == "" && asn == "" {
				log.Info("no geo databases found; geo metric families will be absent",
					"searched", geoSearchDirs)
			} else {
				log.Info("discovered geo databases",
					"city", city, "country", country, "asn", asn)
			}
		}
		m, err := geo.Open(city, country, asn)
		if err != nil {
			return nil, fmt.Errorf("geo provider %s: %w", g.Provider, err)
		}
		return m, nil
	default:
		return nil, fmt.Errorf("unknown geo provider %q (want dbip, maxmind or none)", g.Provider)
	}
}

// maintain runs the periodic housekeeping: expiring sessions, decaying quiet
// series, rotating the salt, and sweeping the rate limiter.
func maintain(ctx context.Context, reg *aggregate.Registry, srv *ingest.Server, salt *ingest.Salt, log *slog.Logger) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			reg.Maintain(now)
			srv.Sweep(now)
			if salt.RotateIfDue(now) {
				log.Info("visitor salt rotated")
			}
		}
	}
}

// reload re-reads the config and geo databases on SIGHUP. A failed reload
// keeps the previous config, so a bad edit is a log line rather than an outage.
func reload(ctx context.Context, store *config.Store, reg *aggregate.Registry, g geo.Provider, log *slog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if err := store.Reload(); err != nil {
				log.Error("config reload failed; keeping previous config", "err", err)
				continue
			}
			reg.Configure(store.Get())
			if err := g.Reload(); err != nil {
				log.Error("geo reload failed; keeping previous databases", "err", err)
			}
			log.Info("configuration reloaded", "sites", len(store.Get().Sites))
		}
	}
}
