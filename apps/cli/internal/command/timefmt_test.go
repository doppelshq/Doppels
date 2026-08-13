package command

import (
	"testing"
	"time"
)

func TestFormatRelativeTimeUnder24Hours(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		at   time.Time
		want string
	}{
		{now.Add(-30 * time.Second), "30 secs ago"},
		{now.Add(-time.Second), "just now"},
		{now.Add(-6 * time.Minute), "6 mins ago"},
		{now.Add(-time.Minute), "1 min ago"},
		{now.Add(-5*time.Hour - 22*time.Minute), "5 hours 22 mins ago"},
		{now.Add(-2 * time.Hour), "2 hours ago"},
		{now.Add(-time.Hour), "1 hour ago"},
		{now.Add(30 * time.Second), "in 30 secs"},
		{now.Add(6 * time.Minute), "in 6 mins"},
		{now.Add(time.Hour), "in 1 hour"},
		{now.Add(2*time.Hour + 15*time.Minute), "in 2 hours 15 mins"},
		{now.Add(-24 * time.Hour), "2026-08-02T12:00:00Z"},
		{now.Add(-25 * time.Hour), "2026-08-02T11:00:00Z"},
		{now.Add(25 * time.Hour), "2026-08-04T13:00:00Z"},
	}
	for _, tc := range cases {
		if got := formatRelativeTime(now, tc.at); got != tc.want {
			t.Fatalf("formatRelativeTime(%s) = %q, want %q", tc.at, got, tc.want)
		}
	}
}

func TestFormatDisplayTimeParsesCreatedStrings(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	got := formatDisplayTime(now, "2026-08-03T11:54:00Z")
	if got != "6 mins ago" {
		t.Fatalf("got %q", got)
	}
	if got := formatDisplayTime(now, "not-a-date"); got != "not-a-date" {
		t.Fatalf("passthrough = %q", got)
	}
}
