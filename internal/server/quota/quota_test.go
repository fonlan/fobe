package quota

import (
	"testing"
	"time"
)

func mustUTC(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return tm
}

func TestPeriodStartDisabledCycleIsLifetime(t *testing.T) {
	if got := PeriodStart("none", nil, "UTC", mustUTC(t, "2026-02-10T00:00:00Z")); got != 0 {
		t.Fatalf("got %d, want lifetime start 0", got)
	}
	if got := NextReset("none", nil, "UTC", time.Now()); got != nil {
		t.Fatalf("disabled cycle reset = %v, want nil", got)
	}
}

func TestMonthlyCycleRollsFromNextResetAndPreservesSeconds(t *testing.T) {
	reset := mustUTC(t, "2026-01-31T12:34:56Z").Unix()
	start := PeriodStart("month", &reset, "UTC", mustUTC(t, "2026-02-28T13:00:00Z"))
	wantStart := mustUTC(t, "2026-02-28T12:34:56Z").Unix()
	if start != wantStart {
		t.Fatalf("start=%d, want %d", start, wantStart)
	}
	next := NextReset("month", &reset, "UTC", mustUTC(t, "2026-02-28T13:00:00Z"))
	wantNext := mustUTC(t, "2026-03-31T12:34:56Z").Unix()
	if next == nil || *next != wantNext {
		t.Fatalf("next=%v, want %d", next, wantNext)
	}
}

func TestYearlyCycleClampsLeapDay(t *testing.T) {
	reset := mustUTC(t, "2024-02-29T01:02:03Z").Unix()
	start := PeriodStart("year", &reset, "UTC", mustUTC(t, "2025-03-01T00:00:00Z"))
	wantStart := mustUTC(t, "2025-02-28T01:02:03Z").Unix()
	if start != wantStart {
		t.Fatalf("start=%d, want %d", start, wantStart)
	}
	next := NextReset("year", &reset, "UTC", mustUTC(t, "2025-03-01T00:00:00Z"))
	wantNext := mustUTC(t, "2026-02-28T01:02:03Z").Unix()
	if next == nil || *next != wantNext {
		t.Fatalf("next=%v, want %d", next, wantNext)
	}
}

func TestCycleTimezone(t *testing.T) {
	// Local Feb 1 00:00 Asia/Shanghai = Jan 31 16:00 UTC.
	reset := mustUTC(t, "2026-01-31T16:00:07Z").Unix()
	start := PeriodStart("month", &reset, "Asia/Shanghai", mustUTC(t, "2026-02-01T00:00:08Z"))
	if start != reset {
		t.Fatalf("start=%d, want %d", start, reset)
	}
}

func TestUsageModes(t *testing.T) {
	cases := []struct {
		mode     string
		in, out  int64
		quota    int64
		wantUsed float64
		wantPct  float64
	}{
		{ModeIn, 300, 100, 1000, 300, 0.3},
		{ModeOut, 300, 100, 1000, 100, 0.1},
		{ModeBoth, 300, 100, 1000, 400, 0.4},
		{ModeMax, 300, 100, 1000, 300, 0.3},
		{ModeMax, 100, 300, 1000, 300, 0.3},
		{ModeBoth, 300, 100, 0, 400, -1},
	}
	for _, c := range cases {
		used, pct := Usage(c.mode, c.in, c.out, c.quota)
		if used != c.wantUsed || pct != c.wantPct {
			t.Errorf("Usage(%s,%d,%d,%d) = %v,%v want %v,%v", c.mode, c.in, c.out, c.quota, used, pct, c.wantUsed, c.wantPct)
		}
	}
}

func TestLocalDateTZ(t *testing.T) {
	if got := LocalDate(mustUTC(t, "2026-01-31T17:00:00Z").Unix(), "Asia/Shanghai"); got != "2026-02-01" {
		t.Fatalf("got %s", got)
	}
}
