package maint

import (
	"testing"
	"time"
)

func TestAddMonthsClamped(t *testing.T) {
	jan31 := time.Date(2026, 1, 31, 9, 30, 0, 0, time.UTC)
	cases := map[int]string{1: "2026-02-28", 2: "2026-03-31", 3: "2026-04-30", 13: "2027-02-28", 25: "2028-02-29"}
	for months, want := range cases {
		if got := addMonthsClamped(jan31, months).Format("2006-01-02"); got != want {
			t.Errorf("Jan 31 + %d months = %s, want %s", months, got, want)
		}
	}
	if got := addMonthsClamped(jan31, 1); got.Hour() != 9 || got.Minute() != 30 {
		t.Errorf("time of day not kept: %v", got)
	}
}

func TestSeriesStarts(t *testing.T) {
	sg := time.FixedZone("SGT", 8*3600)
	s := &Service{loc: sg}
	// 08:00 local on 31 Jan, monthly.
	anchor := time.Date(2026, 1, 31, 8, 0, 0, 0, sg)
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, sg)
	starts := s.seriesStarts(anchor.UTC(), 3, &end, nil, anchor)
	want := []string{"2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30", "2026-05-31"}
	if len(starts) != len(want) {
		t.Fatalf("got %d occurrences: %v", len(starts), starts)
	}
	for i, st := range starts {
		local := st.In(sg)
		if local.Format("2006-01-02") != want[i] || local.Hour() != 8 {
			t.Errorf("occurrence %d = %v, want %s 08:00", i, local, want[i])
		}
	}
	// Daily series across a DST-free zone keep the local hour; "after"
	// continues strictly after the given time.
	daily := s.seriesStarts(anchor.UTC(), 1, nil, &starts[0], anchor)
	if len(daily) == 0 || !daily[0].Equal(anchor.AddDate(0, 0, 1)) {
		t.Fatalf("daily continuation: %v", daily[:1])
	}
	if len(daily) > maxOccurrences {
		t.Fatalf("occurrences not bounded: %d", len(daily))
	}
	// Open-ended series stop at the rolling horizon.
	last := daily[len(daily)-1]
	if last.After(anchor.Add(horizon)) {
		t.Fatalf("occurrence beyond horizon: %v", last)
	}
	// Weekly and yearly.
	weekly := s.seriesStarts(anchor.UTC(), 2, &end, nil, anchor)
	if !weekly[1].Equal(anchor.AddDate(0, 0, 7)) {
		t.Fatalf("weekly: %v", weekly[:2])
	}
	yearEnd := time.Date(2029, 1, 1, 0, 0, 0, 0, sg)
	leap := time.Date(2028, 2, 29, 8, 0, 0, 0, sg)
	yearly := s.seriesStarts(leap.UTC(), 4, &yearEnd, nil, leap)
	if len(yearly) != 1 {
		t.Fatalf("yearly: %v", yearly)
	}
	// A long running series resumes near "after" instead of iterating from the anchor.
	old := time.Date(2020, 1, 1, 8, 0, 0, 0, sg)
	now := time.Date(2026, 10, 6, 8, 0, 0, 0, sg)
	resumed := s.seriesStarts(old.UTC(), 1, nil, &now, now)
	if len(resumed) == 0 || !resumed[0].Equal(now.AddDate(0, 0, 1)) {
		t.Fatalf("resume after: %v", resumed[:1])
	}
}
