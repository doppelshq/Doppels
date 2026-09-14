package shareclient

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
)

func TestSanitizeRunEventNeverLeaksLocalExecutionData(t *testing.T) {
	event := execution.RunEvent{
		APIVersion: APIVersion, Kind: "RunEvent", RunID: testRunID, Sequence: 4, OccurredAt: testNow, Type: "run_succeeded",
		Data: map[string]any{
			"status": "succeeded", "error": "secret at /private/path", "evidence": map[string]any{
				"notes":  "reviewed manually",
				"proof":  execution.ArtifactReference{ID: "proof", Filename: "proof.txt", MediaType: "text/plain", SizeBytes: 5, SHA256: strings.Repeat("b", 64), LocalPath: "/private/proof.txt"},
				"unsafe": map[string]any{"secret": "value"},
			},
			"products": map[string]any{"intermediate": "secret"}, "stdoutPath": "/private/stdout", "stderrPath": "/private/stderr",
			"returns": map[string]any{
				"archive": execution.ArtifactReference{ID: "artifact", Filename: "release.tgz", MediaType: "application/gzip", SizeBytes: 12, SHA256: strings.Repeat("a", 64), LocalPath: "/private/release.tgz"},
				"url":     "https://example.test/release",
				"unsafe":  map[string]any{"secret": "value"},
			},
		},
	}
	public := SanitizeRunEvent(event)
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"secret", "/private", "products", "stdoutPath", "stderrPath", "unsafe"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("sanitized event leaks %q: %s", forbidden, text)
		}
	}
	for _, required := range []string{"run_succeeded", "release.tgz", "https://example.test/release", "evidence", "reviewed manually", "proof.txt"} {
		if !strings.Contains(text, required) {
			t.Errorf("sanitized event lost %q: %s", required, text)
		}
	}
}

// TestSanitizeRunEventTruncatesTimestampToMillisecond pins the wire format:
// the control plane stores microsecond timestamps and echoes them back in
// acknowledgements, so sub-millisecond digits would never round-trip and
// every submission would fail payload comparison on nanosecond clocks.
func TestSanitizeRunEventTruncatesTimestampToMillisecond(t *testing.T) {
	event := execution.RunEvent{
		APIVersion: APIVersion, Kind: "RunEvent", RunID: testRunID, Sequence: 0,
		OccurredAt: testNow.Add(333445556 * time.Nanosecond), Type: "run_created",
	}
	public := SanitizeRunEvent(event)
	if public.OccurredAt.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("occurredAt kept sub-millisecond digits: %v", public.OccurredAt)
	}
	want := testNow.Add(333 * time.Millisecond)
	if !public.OccurredAt.Equal(want) {
		t.Fatalf("occurredAt = %v, want %v", public.OccurredAt, want)
	}
}

// TestTruncateRunTimestampForWire pins the Run.CreatedAt truncation that
// SubmitRun performs before sending the envelope. The control plane
// (Postgres + Ash) stores microseconds and re-emits the same value in the
// `run_recorded` ack; payload comparison is exact (`time.Equal`), so the
// wire bytes must already be millisecond-aligned. A nanosecond clock
// would otherwise fail every submission with "Cloud acknowledgement
// changed the submitted payload" — the bug this truncation fixes.
func TestTruncateRunTimestampForWire(t *testing.T) {
	raw := testNow.Add(987654321 * time.Nanosecond)
	truncated := truncateRunTimestamp(raw)
	if truncated.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("truncated createdAt kept sub-millisecond digits: %v", truncated)
	}
	want := testNow.Add(987 * time.Millisecond)
	if !truncated.Equal(want) {
		t.Fatalf("createdAt = %v, want %v", truncated, want)
	}
	// Reapplying truncation is idempotent — a second pass leaves the
	// value unchanged, so a retry of the same envelope round-trips.
	if !truncateRunTimestamp(truncated).Equal(truncated) {
		t.Fatalf("truncate(truncate(x)) diverged")
	}
}

func TestSanitizeStepEventKeepsOnlySafeStatus(t *testing.T) {
	event := execution.RunEvent{Type: "step_failed", Data: map[string]any{"status": "failed", "exitCode": 2, "timedOut": false, "error": "contains secret", "products": map[string]any{"x": "y"}}}
	public := SanitizeRunEvent(event)
	if len(public.Data) != 3 || public.Data["status"] != "failed" || public.Data["exitCode"] != 2 || public.Data["timedOut"] != false {
		t.Fatalf("Data = %#v", public.Data)
	}
}
