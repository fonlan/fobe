// Package quota implements the traffic accounting semantics (design.md §8.2–8.3):
// rolling cycle anchors, timezone-split daily buckets, and the four usage modes.
package quota

import "time"

// PeriodStart computes the current quota-cycle start from the rolling anchor.
// Explicit cycle_days when set; otherwise calendar months keeping the anchor's
// day-of-month, clamped to month length (design §0.14 月末边界).
func PeriodStart(anchorAt, cycleDays *int64, tz string, now time.Time) int64 {
	loc := loadTZ(tz)
	now = now.In(loc)
	if anchorAt == nil || *anchorAt == 0 {
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc).Unix()
	}
	anchor := time.Unix(*anchorAt, 0).In(loc)
	if anchor.After(now) {
		anchor = now
	}
	if cycleDays != nil && *cycleDays > 0 {
		days := *cycleDays
		elapsed := int64(now.Sub(anchor).Hours() / 24)
		start := anchor.AddDate(0, 0, int(elapsed/days*days))
		return truncateToSec(start, loc)
	}
	// monthly: most recent occurrence of the anchor's day-of-month, clamped
	// to the month length ("Feb 31" must not normalize into March, §0.14)
	day := anchor.Day()
	cand := monthDay(now.Year(), now.Month(), day, anchor, loc)
	if cand.After(now) {
		y, m := now.Year(), now.Month()
		if m == time.January {
			y, m = y-1, time.December
		} else {
			m--
		}
		cand = monthDay(y, m, day, anchor, loc)
	}
	return cand.Unix()
}

// monthDay builds day `day` of (year, month) at the anchor's time of day,
// clamping to the month's last day.
func monthDay(year int, month time.Month, day int, anchor time.Time, loc *time.Location) time.Time {
	last := time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day() // day 0 = last of month
	if day > last {
		day = last
	}
	return time.Date(year, month, day, anchor.Hour(), anchor.Minute(), anchor.Second(), 0, loc)
}

// LocalDate renders ts as YYYY-MM-DD in the node's timezone (daily buckets
// follow the probe's own clock, §8.2.3).
func LocalDate(ts int64, tz string) string {
	return time.Unix(ts, 0).In(loadTZ(tz)).Format("2006-01-02")
}

func truncateToSec(t time.Time, loc *time.Location) int64 {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc).Unix()
}

func loadTZ(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil || loc == nil {
		return time.UTC
	}
	return loc
}

// Modes (design §8.3). The mode affects display and accounting only —
// raw rx/tx are always both stored.
const (
	ModeIn   = "in"
	ModeOut  = "out"
	ModeBoth = "both"
	ModeMax  = "max"
)

// Usage applies the mode: returns used bytes and the quota percentage.
// percent < 0 means "no quota configured" (§8.3: show totals only).
func Usage(mode string, in, out, quota int64) (used, percent float64) {
	switch mode {
	case ModeIn:
		used = float64(in)
	case ModeOut:
		used = float64(out)
	case ModeMax:
		used = max(float64(in), float64(out))
	default: // both
		used = float64(in) + float64(out)
	}
	if quota <= 0 {
		return used, -1
	}
	return used, used / float64(quota)
}
