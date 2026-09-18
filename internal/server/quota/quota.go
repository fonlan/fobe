// Package quota implements the traffic accounting semantics (design.md §8.2–8.3):
// rolling cycle anchors, timezone-split daily buckets, and the four usage modes.
package quota

import "time"

// PeriodStart computes the current traffic-cycle start from the configured
// next reset. A disabled cycle intentionally uses the all-time start (0): a
// quota without a reset is a lifetime quota, not an implicit calendar month.
func PeriodStart(cycleType string, nextResetAt *int64, tz string, now time.Time) int64 {
	start, _ := CycleWindow(cycleType, nextResetAt, tz, now)
	return start
}

// NextReset computes the effective future reset time for a configured cycle.
// The stored next reset is an anchor: past values roll forward automatically.
func NextReset(cycleType string, nextResetAt *int64, tz string, now time.Time) *int64 {
	_, next := CycleWindow(cycleType, nextResetAt, tz, now)
	return next
}

// CycleWindow returns the beginning of the active window and the effective
// upcoming reset. Calendar arithmetic preserves the original day-of-month and
// time-of-day, clamping short months without permanently changing later months.
func CycleWindow(cycleType string, nextResetAt *int64, tz string, now time.Time) (int64, *int64) {
	loc := loadTZ(tz)
	now = now.In(loc)
	if (cycleType != "month" && cycleType != "quarter" && cycleType != "year") || nextResetAt == nil || *nextResetAt <= 0 {
		return 0, nil
	}
	anchor := time.Unix(*nextResetAt, 0).In(loc)
	if anchor.After(now) {
		next := truncateToSec(anchor, loc)
		return truncateToSec(cycleAt(anchor, cycleType, -1), loc), &next
	}

	step := 0
	for candidate := cycleAt(anchor, cycleType, step); !candidate.After(now); candidate = cycleAt(anchor, cycleType, step) {
		step++
	}
	next := truncateToSec(cycleAt(anchor, cycleType, step), loc)
	start := truncateToSec(cycleAt(anchor, cycleType, step-1), loc)
	return start, &next
}

func cycleAt(anchor time.Time, cycleType string, step int) time.Time {
	loc := anchor.Location()
	year, month := anchor.Year(), anchor.Month()
	// step counts whole cycles; months is the calendar jump it stands for.
	// year/quarter are just 12- and 3-month steps, so month-end and leap-day
	// clamping (monthDay) behaves identically for every cycle type.
	months := step
	switch cycleType {
	case "year":
		months = step * 12
	case "quarter":
		months = step * 3
	}
	totalMonths := year*12 + int(month) - 1 + months
	year = totalMonths / 12
	month = time.Month(totalMonths%12 + 1)
	return monthDay(year, month, anchor.Day(), anchor, loc)
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
