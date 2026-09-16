package httpapi

import (
	"fmt"
	"testing"

	"github.com/fonlan/fobe/internal/server/store"
)

// The detail chart pulls every target enabled on the node in one request
// (target=all), bucket-averaged server-side. Targets not enabled on the node
// must stay out of the response even when they have samples, and the legacy
// single-target raw mode keeps working.
func TestNodeLatencyAllTargetsBucketed(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "latency", "m-lat-all", "198.51.100.8")

	tcpID, err := api.Store.CreateLatencyTarget("cloudflare", "tcp", "1.1.1.1", 443)
	if err != nil {
		t.Fatal(err)
	}
	icmpID, err := api.Store.CreateLatencyTarget("gateway", "icmp", "192.0.2.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Store.SetNodeLatencyTargets(nodeID, []int64{tcpID, icmpID}); err != nil {
		t.Fatal(err)
	}
	otherID, err := api.Store.CreateLatencyTarget("other", "tcp", "9.9.9.9", 53)
	if err != nil {
		t.Fatal(err)
	}

	// Keep every probe inside one aligned bucket so the averages are exact.
	bstart := ((nowUnix() - 3600) / 3600) * 3600
	var rows []store.LatencySampleRow
	for i := 0; i < 6; i++ {
		ts := bstart + 1 + int64(i)*10
		rows = append(rows,
			store.LatencySampleRow{TargetID: tcpID, TS: ts, ICMPMs: -1, TCPMs: float64(10 + i)},
			store.LatencySampleRow{TargetID: icmpID, TS: ts, ICMPMs: float64(20 + i), TCPMs: -1},
			store.LatencySampleRow{TargetID: otherID, TS: ts, ICMPMs: -1, TCPMs: 5},
		)
	}
	if err := api.Store.InsertLatencySamples(nodeID, rows); err != nil {
		t.Fatal(err)
	}

	_, body := authedGet(t, srv, cookie, fmt.Sprintf("/api/nodes/%s/latency?target=all&bucket=3600&from=%d", nodeID, bstart))
	samples, _ := body["samples"].([]any)
	seen := map[int64]int{}
	var tcpAvg, icmpAvg float64
	for _, raw := range samples {
		sm := raw.(map[string]any)
		id := int64(sm["target_id"].(float64))
		if id == otherID {
			t.Fatalf("target %d not enabled on the node leaked into target=all", otherID)
		}
		seen[id]++
		switch id {
		case tcpID:
			tcpAvg = sm["tcp_ms"].(float64)
			if sm["icmp_ms"].(float64) != -1 {
				t.Fatalf("tcp target bucket should carry icmp_ms=-1: %v", sm)
			}
		case icmpID:
			icmpAvg = sm["icmp_ms"].(float64)
		}
	}
	if seen[tcpID] != 1 || seen[icmpID] != 1 {
		t.Fatalf("want exactly one bucket per enabled target, got %v", seen)
	}
	if tcpAvg != 12.5 {
		t.Fatalf("tcp bucket avg = %v, want 12.5", tcpAvg)
	}
	if icmpAvg != 22.5 {
		t.Fatalf("icmp bucket avg = %v, want 22.5", icmpAvg)
	}

	_, body = authedGet(t, srv, cookie, fmt.Sprintf("/api/nodes/%s/latency?target=%d&from=%d", nodeID, tcpID, bstart))
	samples, _ = body["samples"].([]any)
	if len(samples) != 6 {
		t.Fatalf("raw single-target mode: want 6 raw points, got %d", len(samples))
	}
}
