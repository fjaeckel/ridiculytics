package geo

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

func TestNullProviderIsFullySupported(t *testing.T) {
	var p Provider = Null{}

	got := p.Lookup(netip.MustParseAddr("203.0.113.7"))
	if got != (Result{}) {
		t.Errorf("Null.Lookup = %+v, want a zero Result", got)
	}
	if p.DBAge() != 0 {
		t.Error("Null.DBAge should be zero")
	}
	// Running without geo is a supported state, so neither of these may error.
	if err := p.Reload(); err != nil {
		t.Errorf("Null.Reload = %v, want nil", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("Null.Close = %v, want nil", err)
	}
}

func TestOpenWithNoDatabasesSucceeds(t *testing.T) {
	m, err := Open("", "", "")
	if err != nil {
		t.Fatalf("opening with no databases should succeed, got %v", err)
	}
	defer m.Close()

	if got := m.Lookup(netip.MustParseAddr("203.0.113.7")); got != (Result{}) {
		t.Errorf("Lookup = %+v, want zero when no database is loaded", got)
	}
	if m.DBAge() <= 0 {
		t.Error("DBAge should be set once loaded, even with no databases")
	}
}

// TestOpenMissingFileFails: a configured-but-absent database is an operator
// error and must fail loudly at startup rather than silently disabling geo.
func TestOpenMissingFileFails(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "nope.mmdb"), "", "")
	if err == nil {
		t.Fatal("expected an error for a missing database file")
	}
}

func TestOpenCorruptFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.mmdb")
	if err := os.WriteFile(path, []byte("this is not an mmdb"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "", ""); err == nil {
		t.Fatal("expected an error for a corrupt database file")
	}
}

// TestFailedReloadKeepsPreviousDatabases: a bad file appearing on disk must not
// take geo down; the previously loaded databases stay in service.
func TestFailedReloadKeepsPreviousDatabases(t *testing.T) {
	m, err := Open("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	before := m.DBAge()

	m.cityPath = filepath.Join(t.TempDir(), "missing.mmdb")
	if err := m.Reload(); err == nil {
		t.Fatal("expected reload to fail")
	}
	if m.DBAge() < before {
		t.Error("a failed reload must not reset the load timestamp")
	}
	// Still usable rather than panicking on a nil reader.
	_ = m.Lookup(netip.MustParseAddr("203.0.113.7"))
}

func TestLookupInvalidAddress(t *testing.T) {
	m, err := Open("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if got := m.Lookup(netip.Addr{}); got != (Result{}) {
		t.Errorf("Lookup(invalid) = %+v, want zero", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	m, err := Open("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	// Lookup after Close must not panic; a shutting-down process may still
	// have in-flight requests.
	_ = m.Lookup(netip.MustParseAddr("203.0.113.7"))
}

func TestPickNamePrefersEnglish(t *testing.T) {
	if got := pickName(map[string]string{"de": "Köln", "en": "Cologne"}); got != "Cologne" {
		t.Errorf("pickName = %q, want Cologne", got)
	}
	if got := pickName(map[string]string{"de": "Köln"}); got != "Köln" {
		t.Errorf("pickName = %q, want the only available name", got)
	}
	if got := pickName(nil); got != "" {
		t.Errorf("pickName(nil) = %q, want empty", got)
	}
}

// TestProviderInterfaceIsSatisfied is a compile-time guard that the geo
// implementations remain swappable — the whole point of the interface is that
// nothing depends on a specific vendor's database being available.
func TestProviderInterfaceIsSatisfied(t *testing.T) {
	var _ Provider = Null{}
	var _ Provider = (*MMDB)(nil)
}
