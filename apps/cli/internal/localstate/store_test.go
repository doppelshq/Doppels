package localstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorePersistsDocumentsEventsLogsAndArtifact(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteRequest(map[string]any{"kind": "Request", "id": "request"}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteRun(map[string]any{"kind": "Run", "id": "run"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(map[string]any{"sequence": 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(map[string]any{"sequence": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteLog("build", "stdout", []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "result.txt")
	if err := os.WriteFile(source, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CopyArtifact("22222222-2222-4222-8222-222222222222", "result.txt", source)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"request.json", "run.json", filepath.Join("logs", "build.stdout.log")} {
		if _, err := os.Stat(filepath.Join(store.Dir(), name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	events, err := os.ReadFile(filepath.Join(store.Dir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(events)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d event lines", len(lines))
	}
	for _, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Errorf("invalid event JSON: %v", err)
		}
	}
	if data, err := os.ReadFile(snapshot); err != nil || string(data) != "artifact" {
		t.Fatalf("artifact snapshot = %q, %v", data, err)
	}
}

func TestStoreRejectsUnsafeIdentifiers(t *testing.T) {
	if _, err := Open(t.TempDir(), "../escape"); err == nil {
		t.Fatal("expected unsafe run id to fail")
	}
	store, err := Open(t.TempDir(), "safe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteLog("../escape", "stdout", nil); err == nil {
		t.Fatal("expected unsafe step id to fail")
	}
}

func TestStoreNeverReopensAnExistingRun(t *testing.T) {
	root := t.TempDir()
	if _, err := Open(root, "same-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, "same-run"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second Open error = %v", err)
	}
}
