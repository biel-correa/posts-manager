package scheduler

import (
	"fmt"
	"time"
)

// CurrentWeekKey returns the canonical YYYY-MM-DD date string of the most recent Sunday
// (00:00:00 in loc). Because Go's time.Weekday starts with Sunday == 0, every day from
// Sunday 00:00 through Saturday 23:59:59 maps to the exact same week_key.
func CurrentWeekKey(now time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	sunday := midnight.AddDate(0, 0, -int(midnight.Weekday()))
	return sunday.Format("2006-01-02")
}

// FormatWeekRange formats a YYYY-MM-DD Sunday week_key into a human-friendly label,
// e.g. "Nov 1 – Nov 7, 2026".
func FormatWeekRange(weekKey string, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	start, err := time.ParseInLocation("2006-01-02", weekKey, loc)
	if err != nil {
		return weekKey
	}
	end := start.AddDate(0, 0, 6)
	if start.Year() == end.Year() {
		return fmt.Sprintf("%s – %s, %d", start.Format("Jan 2"), end.Format("Jan 2"), end.Year())
	}
	return fmt.Sprintf("%s – %s", start.Format("Jan 2, 2006"), end.Format("Jan 2, 2006"))
}

// SuggestedDayForSlot picks a suggested posting day label (1-indexed slot).
func SuggestedDayForSlot(slotIndex int, preferredDays []string) string {
	if len(preferredDays) == 0 {
		preferredDays = []string{"Tuesday", "Wednesday", "Thursday"}
	}
	idx := (slotIndex - 1) % len(preferredDays)
	if idx < 0 {
		idx = 0
	}
	return preferredDays[idx]
}
