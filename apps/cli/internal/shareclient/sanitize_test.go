package shareclient

import (
	"encoding/json"
	"strings"
	"testing"

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

func TestSanitizeStepEventKeepsOnlySafeStatus(t *testing.T) {
	event := execution.RunEvent{Type: "step_failed", Data: map[string]any{"status": "failed", "exitCode": 2, "timedOut": false, "error": "contains secret", "products": map[string]any{"x": "y"}}}
	public := SanitizeRunEvent(event)
	if len(public.Data) != 3 || public.Data["status"] != "failed" || public.Data["exitCode"] != 2 || public.Data["timedOut"] != false {
		t.Fatalf("Data = %#v", public.Data)
	}
}
