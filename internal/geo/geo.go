// Package geo resolves IP addresses to country, region, city and ASN.
//
// The provider is an interface on purpose: MaxMind's GeoLite2 EULA forbids
// redistribution and has been amended twice, so nothing here may depend on it
// being available. DB-IP Lite (CC-BY-4.0) is the redistributable default and
// the "none" provider must remain a fully supported state.
package geo

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

// Result is what enrichment gets back. Any field may be empty.
type Result struct {
	Country string // ISO-3166 alpha-2
	Region  string
	City    string
	ASN     string // decimal, no "AS" prefix
	ASOrg   string
}

// Provider looks up an IP. Implementations must be safe for concurrent use.
type Provider interface {
	Lookup(netip.Addr) Result
	// DBAge reports the age of the oldest loaded database.
	DBAge() time.Duration
	Reload() error
	Close() error
}

// Null is the no-geo provider. Every geo family simply goes absent.
type Null struct{}

func (Null) Lookup(netip.Addr) Result { return Result{} }
func (Null) DBAge() time.Duration     { return 0 }
func (Null) Reload() error            { return nil }
func (Null) Close() error             { return nil }

// MMDB reads MaxMind-format databases. Both DB-IP Lite and GeoLite2 ship in
// this format, so one implementation serves both providers.
type MMDB struct {
	cityPath, countryPath, asnPath string

	mu      sync.RWMutex
	city    *maxminddb.Reader
	country *maxminddb.Reader
	asn     *maxminddb.Reader
	loaded  time.Time
}

// Open loads whichever databases are configured. All paths are optional; an
// MMDB with no databases behaves like Null but still reports its age.
func Open(cityPath, countryPath, asnPath string) (*MMDB, error) {
	m := &MMDB{cityPath: cityPath, countryPath: countryPath, asnPath: asnPath}
	if err := m.Reload(); err != nil {
		return nil, err
	}
	return m, nil
}

// Reload re-opens the databases and swaps them in, closing the old readers
// only after the swap so in-flight lookups never touch a closed reader.
func (m *MMDB) Reload() error {
	var city, country, asn *maxminddb.Reader
	var err error

	open := func(p string) (*maxminddb.Reader, error) {
		if p == "" {
			return nil, nil
		}
		if _, statErr := os.Stat(p); statErr != nil {
			return nil, fmt.Errorf("geoip database %s: %w", p, statErr)
		}
		return maxminddb.Open(p)
	}

	if city, err = open(m.cityPath); err != nil {
		return err
	}
	if country, err = open(m.countryPath); err != nil {
		closeAll(city)
		return err
	}
	if asn, err = open(m.asnPath); err != nil {
		closeAll(city, country)
		return err
	}

	m.mu.Lock()
	old := []*maxminddb.Reader{m.city, m.country, m.asn}
	m.city, m.country, m.asn = city, country, asn
	m.loaded = time.Now()
	m.mu.Unlock()

	closeAll(old...)
	return nil
}

func closeAll(rs ...*maxminddb.Reader) {
	for _, r := range rs {
		if r != nil {
			_ = r.Close()
		}
	}
}

// cityRecord is the subset of the City schema we decode. Decoding only these
// fields keeps the hot path allocation-light.
type cityRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	Subdivisions []struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"subdivisions"`
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
}

type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

type asnRecord struct {
	Number uint   `maxminddb:"autonomous_system_number"`
	Org    string `maxminddb:"autonomous_system_organization"`
	// DB-IP Lite ASN uses different keys for the same data.
	AltNumber uint   `maxminddb:"as_number"`
	AltOrg    string `maxminddb:"as_organization"`
}

// Lookup resolves an address. Failures return zero values rather than errors:
// missing geo must never fail an ingest.
func (m *MMDB) Lookup(addr netip.Addr) Result {
	if !addr.IsValid() {
		return Result{}
	}
	ip := net.IP(addr.AsSlice())

	m.mu.RLock()
	city, country, asn := m.city, m.country, m.asn
	m.mu.RUnlock()

	var out Result
	switch {
	case city != nil:
		var rec cityRecord
		if err := city.Lookup(ip, &rec); err == nil {
			out.Country = rec.Country.ISOCode
			out.City = pickName(rec.City.Names)
			if len(rec.Subdivisions) > 0 {
				out.Region = pickName(rec.Subdivisions[0].Names)
			}
		}
	case country != nil:
		var rec countryRecord
		if err := country.Lookup(ip, &rec); err == nil {
			out.Country = rec.Country.ISOCode
		}
	}

	if asn != nil {
		var rec asnRecord
		if err := asn.Lookup(ip, &rec); err == nil {
			num, org := rec.Number, rec.Org
			if num == 0 {
				num, org = rec.AltNumber, rec.AltOrg
			}
			if num != 0 {
				out.ASN = strconv.FormatUint(uint64(num), 10)
				out.ASOrg = org
			}
		}
	}
	return out
}

// pickName prefers English, falling back to any available localization.
func pickName(names map[string]string) string {
	if n, ok := names["en"]; ok {
		return n
	}
	for _, n := range names {
		return n
	}
	return ""
}

func (m *MMDB) DBAge() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.loaded.IsZero() {
		return 0
	}
	return time.Since(m.loaded)
}

func (m *MMDB) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	closeAll(m.city, m.country, m.asn)
	m.city, m.country, m.asn = nil, nil, nil
	return nil
}
