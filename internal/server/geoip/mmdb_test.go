package geoip

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The tests assemble a minimal GeoLite2-Country format database by hand so
// the MMDB reader path is exercised without shipping a binary fixture.

const (
	mmdbRecordBytes = 3 // 24-bit node records
	mmdbDataFlag    = uint64(1) << 63
)

type netRecord struct {
	addr net.IP
	bits int
	code string
}

// buildMMDB serializes an IPv4-only database holding the given networks.
// Networks must not overlap (the format can't nest a record inside a node
// that also has children).
func buildMMDB(t *testing.T, records []netRecord) []byte {
	t.Helper()

	type node struct {
		child [2]uint64 // 0 = empty, dataFlag|idx = record, else child node id+1
	}
	nodes := []*node{{}}
	datas := []string{}

	for _, rec := range records {
		cur := 0
		for i := 0; i < rec.bits-1; i++ {
			bit := bitAt(rec.addr, i)
			next := nodes[cur].child[bit]
			if next == 0 {
				nodes = append(nodes, &node{})
				next = uint64(len(nodes))
				nodes[cur].child[bit] = next
			}
			cur = int(next - 1)
		}
		bit := bitAt(rec.addr, rec.bits-1)
		if nodes[cur].child[bit] != 0 {
			t.Fatalf("test networks overlap at %s/%d", rec.addr, rec.bits)
		}
		datas = append(datas, rec.code)
		nodes[cur].child[bit] = mmdbDataFlag | uint64(len(datas)-1)
	}

	dataSection := []byte{}
	offsets := make([]uint32, len(datas))
	for i, code := range datas {
		offsets[i] = uint32(len(dataSection))
		dataSection = append(dataSection, countryRecord(code)...)
	}

	tree := make([]byte, len(nodes)*2*mmdbRecordBytes)
	for i, nd := range nodes {
		for side := 0; side < 2; side++ {
			var val uint32
			switch c := nd.child[side]; {
			case c&mmdbDataFlag != 0:
				// data section pointer: separator (16) + offset, shifted by
				// the node count
				val = uint32(len(nodes)) + 16 + offsets[c&^mmdbDataFlag]
			case c != 0:
				val = uint32(c - 1) // plain node index
			}
			base := (i*2 + side) * mmdbRecordBytes
			tree[base] = byte(val >> 16)
			tree[base+1] = byte(val >> 8)
			tree[base+2] = byte(val)
		}
	}

	meta := encodeMap(9,
		encodeString("binary_format_major_version"), encodeUint(5, 2),
		encodeString("binary_format_minor_version"), encodeUint(5, 0),
		encodeString("build_epoch"), encodeUint(9, uint64(time.Now().Unix())),
		encodeString("database_type"), encodeString("GeoLite2-Country"),
		encodeString("description"),
		encodeMap(1, encodeString("en"), encodeString("fobe geoip test database")),
		encodeString("ip_version"), encodeUint(5, 4),
		encodeString("languages"), append([]byte{0x01, 0x04}, encodeString("en")...),
		encodeString("node_count"), encodeUint(6, uint64(len(nodes))),
		encodeString("record_size"), encodeUint(5, mmdbRecordBytes*8),
	)

	var buf bytes.Buffer
	buf.Write(tree)
	buf.Write(make([]byte, 16)) // data section separator
	buf.Write(dataSection)
	buf.Write([]byte{0xAB, 0xCD, 0xEF})
	buf.WriteString("MaxMind.com")
	buf.Write(meta)
	return buf.Bytes()
}

func bitAt(addr net.IP, i int) byte {
	return addr[i/8] >> (7 - i%8) & 1
}

// countryRecord encodes {"country":{"iso_code":code}}.
func countryRecord(code string) []byte {
	return encodeMap(1,
		encodeString("country"),
		encodeMap(1, encodeString("iso_code"), encodeString(code)),
	)
}

func encodeString(s string) []byte {
	return append([]byte{2<<5 | byte(len(s))}, s...) // test sizes stay < 29
}

func encodeMap(n int, kv ...[]byte) []byte {
	out := []byte{7<<5 | byte(n)}
	for _, b := range kv {
		out = append(out, b...)
	}
	return out
}

// encodeUint encodes an unsigned integer of the given MMDB type (5 = uint16,
// 6 = uint32, 9 = uint64). Types above 7 use the two-byte extended form:
// the first byte carries the size, the second the type number minus 7.
func encodeUint(typ byte, v uint64) []byte {
	size := byte(0)
	for x := v; x > 0; x >>= 8 {
		size++
	}
	var out []byte
	if typ > 7 {
		out = []byte{size, typ - 7}
	} else {
		out = []byte{typ<<5 | size}
	}
	for i := size; i > 0; i-- {
		out = append(out, byte(v>>((i-1)*8)))
	}
	return out
}

func writeMMDB(t *testing.T, records []netRecord) string {
	return writeMMDBAt(t, t.TempDir(), "GeoLite2-Country.mmdb", records)
}

func writeMMDBAt(t *testing.T, dir, name string, records []netRecord) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buildMMDB(t, records), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func testDB() []netRecord {
	return []netRecord{
		{addr: net.ParseIP("81.2.69.0").To4(), bits: 24, code: "GB"},
		{addr: net.ParseIP("203.0.113.0").To4(), bits: 24, code: "DE"},
	}
}

// swappedDB is the same shape as testDB (equal size on disk) but answers US.
func swappedDB(t *testing.T) []netRecord {
	return []netRecord{
		{addr: net.ParseIP("81.2.69.0").To4(), bits: 24, code: "US"},
		{addr: net.ParseIP("203.0.113.0").To4(), bits: 24, code: "DE"},
	}
}

func TestMMDBCountry(t *testing.T) {
	m := NewMMDB(writeMMDB(t, testDB()))
	for _, tc := range []struct {
		ip  string
		ok  bool
		now string
	}{
		{ip: "81.2.69.198", ok: true, now: "GB"}, // inside a network
		{ip: "81.2.69.1", ok: true, now: "GB"},
		{ip: "203.0.113.7", ok: true, now: "DE"},
		{ip: "8.8.8.8"},     // outside all networks
		{ip: "192.168.1.1"}, // LAN ranges carry no country in the db
		{ip: "not-an-ip"},   // unparseable
		{ip: ""},            // empty
		{ip: "2001:db8::1"}, // v6 in a v4-only database
	} {
		code, ok := m.Country(tc.ip)
		if ok != tc.ok || code != tc.now {
			t.Errorf("Country(%q) = (%q, %v), want (%q, %v)", tc.ip, code, ok, tc.now, tc.ok)
		}
	}
}

func TestMMDBMissingFileStaysOnMiss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.mmdb")
	m := NewMMDB(path)
	if _, ok := m.Country("81.2.69.1"); ok {
		t.Fatal("expected miss for a missing database")
	}
	// the file appears later: the next lookup picks it up without a restart
	if err := os.WriteFile(path, buildMMDB(t, testDB()), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, ok := m.Country("81.2.69.1"); !ok || code != "GB" {
		t.Fatalf("after the file appeared: Country = (%q, %v), want (GB, true)", code, ok)
	}
}

func TestMMDBAutoReloadOnChange(t *testing.T) {
	path := writeMMDB(t, testDB())
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	m := NewMMDB(path)
	if code, _ := m.Country("81.2.69.1"); code != "GB" {
		t.Fatalf("initial lookup = %q, want GB", code)
	}

	// swap in a database that answers US and stamp a newer mtime, like an
	// uploaded database would: the next Country call reloads on its own
	if err := os.WriteFile(path, buildMMDB(t, swappedDB(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	newer := old.Add(time.Hour)
	if err := os.Chtimes(path, newer, newer); err != nil {
		t.Fatal(err)
	}
	if code, ok := m.Country("81.2.69.1"); !ok || code != "US" {
		t.Fatalf("after reload: Country = (%q, %v), want (US, true)", code, ok)
	}
	// the second network keeps answering
	if code, ok := m.Country("203.0.113.7"); !ok || code != "DE" {
		t.Fatalf("after reload: Country = (%q, %v), want (DE, true)", code, ok)
	}
}

func TestMMDBReloadForcesReopen(t *testing.T) {
	dir := t.TempDir()
	path := writeMMDBAt(t, dir, "GeoLite2-Country.mmdb", testDB())
	// whole-second stamps so the stat comparison is exact regardless of the
	// filesystem's timestamp precision
	stamp := time.Unix(time.Now().Unix(), 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	m := NewMMDB(path)
	if code, _ := m.Country("81.2.69.1"); code != "GB" {
		t.Fatalf("initial lookup = %q, want GB", code)
	}

	// atomically replace the database with one of identical size and mtime
	// (as an upload would): the stat check can't notice the swap, only
	// Reload() forces the re-open
	tmp := writeMMDBAt(t, dir, "next.mmdb", swappedDB(t))
	if err := os.Chtimes(tmp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if code, _ := m.Country("81.2.69.1"); code != "GB" {
		t.Fatalf("lookup before Reload = %q, want stale GB", code)
	}
	if !m.Reload() {
		t.Fatal("Reload should report a loaded database")
	}
	if code, _ := m.Country("81.2.69.1"); code != "US" {
		t.Fatalf("lookup after Reload = %q, want US", code)
	}
}
