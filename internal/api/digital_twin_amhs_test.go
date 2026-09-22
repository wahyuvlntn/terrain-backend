package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStripLots: without the production permission the transport state keeps
// the vehicles' moves and the FOUPs on the ports but no lot codes or products.
func TestStripLots(t *testing.T) {
	raw := `{"now": "2026-10-13T06:00:00Z", "vehicles": [{"id": "V01", "segs": [
		{"t0": "2026-10-13T06:00:00Z", "t1": "2026-10-13T06:00:05Z", "track": "IB", "s0": 1, "s1": 11},
		{"t0": "2026-10-13T06:00:05Z", "t1": "2026-10-13T06:00:15Z", "track": "photo", "s0": 3, "s1": 3, "hoist": "pick", "port": "TRK_01:1", "lot": "M261013.01"}]}],
		"ports": [{"port": "ETCH_01:2", "lot": "W261012.03", "product": "WSOC40", "hot": true}], "stockers": {"STK-01": 4}}`
	var st map[string]any
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	stripLots(st)
	b, _ := json.Marshal(st)
	out := string(b)
	for _, s := range []string{"M261013", "W261012", "WSOC40", `"hot"`} {
		if strings.Contains(out, s) {
			t.Errorf("stripped state still has %s: %s", s, out)
		}
	}
	for _, s := range []string{`"port":"TRK_01:1"`, `"hoist":"pick"`, `"port":"ETCH_01:2"`, `"STK-01":4`} {
		if !strings.Contains(out, s) {
			t.Errorf("stripped state lost %s: %s", s, out)
		}
	}
	if strings.Contains(out, `"s1":11,"t0":"2026-10-13T06:00:00Z","t1":"2026-10-13T06:00:05Z","track":"IB","lot"`) {
		t.Errorf("an empty run got a lot: %s", out)
	}
}
