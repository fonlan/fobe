package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Quarter is a first-class traffic cycle (§14, 2026-09-18 revision). The
// window math lives in package quota; this pins the other gate: the API must
// accept the value and still reject unknown strings without clobbering the
// stored cycle.
func TestTrafficCycleAcceptsQuarter(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "quarter", "m-quarter", "198.51.100.9")
	reset := time.Now().AddDate(0, 4, 0).Unix()

	body := fmt.Sprintf(`{"traffic_cycle":{"cycle_type":"quarter","next_reset_at":%d}}`, reset)
	if resp, raw := doAuthed(t, http.MethodPatch, srv.URL+"/api/nodes/"+nodeID, cookie, []byte(body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("quarter cycle rejected: %d %s", resp.StatusCode, raw)
	}

	_, out := authedGet(t, srv, cookie, "/api/nodes/"+nodeID)
	tc, ok := out["traffic_cycle"].(map[string]any)
	if !ok {
		t.Fatalf("traffic_cycle missing from node detail: %#v", out["traffic_cycle"])
	}
	if tc["cycle_type"] != "quarter" || tc["next_reset_at"] == nil {
		t.Fatalf("quarter cycle not persisted: %#v", tc)
	}

	bad := `{"traffic_cycle":{"cycle_type":"fortnight","next_reset_at":1234567890}}`
	resp, raw := doAuthed(t, http.MethodPatch, srv.URL+"/api/nodes/"+nodeID, cookie, []byte(bad))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown cycle accepted: %d %s", resp.StatusCode, raw)
	}
	var errBody map[string]any
	if err := json.Unmarshal(raw, &errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if code := errorCode(errBody); code != "invalid_cycle_type" {
		t.Fatalf("error code = %q, want invalid_cycle_type", code)
	}

	// The rejected request must not have overwritten the stored cycle.
	_, out = authedGet(t, srv, cookie, "/api/nodes/"+nodeID)
	tc, _ = out["traffic_cycle"].(map[string]any)
	if tc["cycle_type"] != "quarter" {
		t.Fatalf("rejected update clobbered the cycle: %#v", tc)
	}
}
