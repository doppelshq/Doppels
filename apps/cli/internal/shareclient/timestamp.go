package shareclient

import "time"

// truncateRunTimestamp aligns a timestamp to milliseconds UTC so the wire
// envelope survives Postgres's microsecond rounding without tripping the
// ack comparison. SubmitRun applies this before pushing the envelope; the
// roundtrip test in sanitize_test.go pins the idempotency.
func truncateRunTimestamp(t time.Time) time.Time {
	return t.UTC().Truncate(time.Millisecond)
}
