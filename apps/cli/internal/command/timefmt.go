package command

import (
	"fmt"
	"strings"
	"time"
)

func formatDisplayTime(now time.Time, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	at, ok := parseDisplayTime(raw)
	if !ok {
		return raw
	}
	return formatRelativeTime(now, at)
}

func parseDisplayTime(raw string) (time.Time, bool) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05.000Z07:00",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// formatRelativeTime renders past ("N mins ago") or future ("in N mins").
// Events ≥24h away fall back to RFC3339.
func formatRelativeTime(now, at time.Time) string {
	if at.IsZero() {
		return ""
	}
	now = now.UTC()
	at = at.UTC()
	delta := at.Sub(now)
	future := delta > 0
	if delta < 0 {
		delta = -delta
	}
	if delta >= 24*time.Hour {
		return at.Format(time.RFC3339)
	}
	unit := englishDuration(delta)
	if future {
		return "in " + unit
	}
	if delta < time.Minute {
		if int(delta.Seconds()) <= 1 {
			return "just now"
		}
		return unit + " ago"
	}
	return unit + " ago"
}

func englishDuration(delta time.Duration) string {
	if delta < time.Minute {
		secs := int(delta.Seconds())
		if secs <= 1 {
			return "1 sec"
		}
		return fmt.Sprintf("%d secs", secs)
	}
	totalMins := int(delta.Minutes())
	hours := totalMins / 60
	mins := totalMins % 60
	switch {
	case hours == 0:
		return englishCount(mins, "min", "mins")
	case mins == 0:
		return englishCount(hours, "hour", "hours")
	default:
		return englishCount(hours, "hour", "hours") + " " + englishCount(mins, "min", "mins")
	}
}

func englishCount(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}
