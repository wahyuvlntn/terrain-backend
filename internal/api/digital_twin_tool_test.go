package api

import "testing"

// TestExpectedRunS: a run in process takes the tool's typical time for the step,
// else the route's ideal time (a batch its lot time, other tools the wafer time
// shared by the chambers in use plus the lot overhead); unknown without either.
func TestExpectedRunS(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	s := func(v string) *string { return &v }
	cases := []struct {
		name            string
		typical, wafer  *float64
		lot             *float64
		wafers          int64
		chambers        *string
		batch           bool
		want            float64
		unknown         bool
	}{
		{name: "typical wins", typical: f(900), wafer: f(60), lot: f(300), wafers: 25, want: 900},
		{name: "single chamber", wafer: f(60), lot: f(300), wafers: 25, want: 1800},
		{name: "three chambers", wafer: f(60), lot: f(300), wafers: 24, chambers: s("PM1,PM2,PM3"), want: 780},
		{name: "batch", wafer: f(0), lot: f(14400), wafers: 75, batch: true, want: 14400},
		{name: "no route times", wafers: 25, unknown: true},
	}
	for _, c := range cases {
		got := expectedRunS(c.typical, c.wafer, c.lot, c.wafers, c.chambers, c.batch)
		if c.unknown {
			if got != nil {
				t.Errorf("%s: got %v, want unknown", c.name, *got)
			}
			continue
		}
		if got == nil || *got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
