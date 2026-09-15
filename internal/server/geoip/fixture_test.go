package geoip

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// fixturePath is the checked-in test database, shared with the httpapi and
// geoipupdate tests (../../geoip/testdata/GeoLite2-Country.mmdb). Those
// packages need a payload that survives real MMDB validation, and this package
// owns the repo's only MMDB writer.
const fixturePath = "testdata/GeoLite2-Country.mmdb"

// TestWriteFixture regenerates the checked-in test database:
//
//	FOBE_WRITE_FIXTURE=1 go test ./internal/server/geoip -run TestWriteFixture
//
// It is skipped by default so an ordinary `go test ./...` never rewrites a
// tracked file. The networks below are documentation ranges plus three public
// resolvers, so the fixture also documents which lookups the tests expect.
func TestWriteFixture(t *testing.T) {
	if os.Getenv("FOBE_WRITE_FIXTURE") != "1" {
		t.Skip("set FOBE_WRITE_FIXTURE=1 to regenerate testdata/GeoLite2-Country.mmdb")
	}
	data := buildMMDB(t, []netRecord{
		{addr: net.ParseIP("1.1.1.0").To4(), bits: 24, code: "AU"},
		{addr: net.ParseIP("8.8.8.0").To4(), bits: 24, code: "US"},
		{addr: net.ParseIP("223.5.5.0").To4(), bits: 24, code: "CN"},
	})
	if err := os.MkdirAll(filepath.Dir(fixturePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixturePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d bytes)", fixturePath, len(data))
}

// TestFixtureReadable keeps the tracked fixture honest: a regenerated file
// that no longer parses would otherwise only show up in the httpapi tests.
func TestFixtureReadable(t *testing.T) {
	if _, err := os.Stat(fixturePath); err != nil {
		t.Fatalf("testdata fixture missing (%v); regenerate with FOBE_WRITE_FIXTURE=1", err)
	}
	m := NewMMDB(fixturePath)
	defer m.Reload()
	if !m.Loaded() {
		t.Fatal("fixture does not load as an MMDB")
	}
	md := m.Metadata()
	if md.DatabaseType != "GeoLite2-Country" || md.NodeCount == 0 || md.BuildEpoch == 0 {
		t.Fatalf("fixture metadata = %+v, want a GeoLite2-Country database", md)
	}
	if code, ok := m.Country("8.8.8.8"); !ok || code != "US" {
		t.Fatalf("fixture lookup 8.8.8.8 = %q,%v, want US,true", code, ok)
	}
}
