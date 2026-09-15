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

func TestPeriodStartMonthlyAnchor(t *testing.T) {
	// anchor on the 15th; on Jan 20 the period started Jan 15
	anchor := int64(mustUTC(t, "2026-01-15T00:00:00Z").Unix())
	got := PeriodStart(&anchor, nil, "UTC", mustUTC(t, "2026-01-20T10:00:00Z"))
	want := mustUTC(t, "2026-01-15T00:00:00Z").Unix()
	if got != want {
		t.Fatalf("got %d want %d", got, want)
	}
	// before the 15th of the next month, the period is still the Jan cycle
	got = PeriodStart(&anchor, nil, "UTC", mustUTC(t, "2026-02-10T00:00:00Z"))
	want = mustUTC(t, "2026-01-15T00:00:00Z").Unix()
	if got != want {
		t.Fatalf("got %d want %d", got, want)
	}
	// after Jan 15 next cycle starts Feb 15
	got = PeriodStart(&anchor, nil, "UTC", mustUTC(t, "2026-02-15T01:00:00Z"))
	want = mustUTC(t, "2026-02-15T01:00:00Z").Unix() - 3600
	// Feb 15 anchor lands at 00:00; 01:00 is one hour into the new cycle
	start := mustUTC(t, "2026-02-15T00:00:00Z").Unix()
	if got != start {
		t.Fatalf("got %d want %d", got, start)
	}
	_ = want
}

func TestPeriodStartMonthEndClamp(t *testing.T) {
	// anchor on Jan 31; February has no 31st → clamp to Feb 28
	anchor := int64(mustUTC(t, "2026-01-31T12:00:00Z").Unix())
	got := PeriodStart(&anchor, nil, "UTC", mustUTC(t, "2026-02-28T23:00:00Z"))
	want := mustUTC(t, "2026-02-28T12:00:00Z").Unix()
	if got != want {
		t.Fatalf("got %d want %d", got, want)
	}
}

func TestPeriodStartCycleDays(t *testing.T) {
	// 7-day cycle anchored Jan 1; on Jan 15 the period started Jan 15
	anchor := int64(mustUTC(t, "2026-01-01T00:00:00Z").Unix())
	days := int64(7)
	got := PeriodStart(&anchor, &days, "UTC", mustUTC(t, "2026-01-17T00:00:00Z"))
	want := mustUTC(t, "2026-01-15T00:00:00Z").Unix()
	if got != want {
		t.Fatalf("got %d want %d", got, want)
	}
}

func TestPeriodStartNoAnchor(t *testing.T) {
	// no anchor → calendar month start
	got := PeriodStart(nil, nil, "UTC", mustUTC(t, "2026-02-10T00:00:00Z"))
	want := mustUTC(t, "2026-02-01T00:00:00Z").Unix()
	if got != want {
		t.Fatalf("got %d want %d", got, want)
	}
}

func TestPeriodStartTimezone(t *testing.T) {
	// node in Asia/Shanghai (UTC+8): local Feb 1 00:00 = Jan 31 16:00 UTC
	// a UTC-month-start would be wrong by 8 hours; local date must win
	anchor := int64(mustUTC(t, "2025-12-31T16:00:00Z").Unix()) // local Jan 1 00:00 +08
	got := PeriodStart(&anchor, nil, "Asia/Shanghai", mustUTC(t, "2026-01-31T17:00:00Z"))
	// local time is Feb 1 01:00 → anchor for Jan (local Jan 1 00:00) is in the past,
	// and the Feb cycle starts at local Feb 1 00:00 = Jan 31 16:00 UTC
	want := mustUTC(t, "2026-01-31T16:00:00Z").Unix()
	if got != want {
		t.Fatalf("got %d want %d", got, want)
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
		{ModeBoth, 300, 100, 0, 400, -1}, // no quota → pct -1
	}
	for _, c := range cases {
		used, pct := Usage(c.mode, c.in, c.out, c.quota)
		if used != c.wantUsed || pct != c.wantPct {
			t.Errorf("Usage(%s,%d,%d,%d) = %v,%v want %v,%v",
				c.mode, c.in, c.out, c.quota, used, pct, c.wantUsed, c.wantPct)
		}
	}
}

func TestLocalDateTZ(t *testing.T) {
	// 2026-01-31 17:00 UTC is already Feb 1 in Shanghai
	got := LocalDate(mustUTC(t, "2026-01-31T17:00:00Z").Unix(), "Asia/Shanghai")
	if got != "2026-02-01" {
		t.Fatalf("got %s", got)
	}
	if LocalDate(mustUTC(t, "2026-01-31T17:00:00Z").Unix(), "UTC") != "2026-01-31" {
		t.Fatal("utc date wrong")
	}
}
