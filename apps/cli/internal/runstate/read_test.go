package runstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/runindex"
)

func TestListEmptyProject(t *testing.T) {
	root := t.TempDir()
	items, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %#v", items)
	}
}

func TestLoadLogsAndListLocalRun(t *testing.T) {
	root := t.TempDir()
	runID := "run01local"
	reqID := "req01local"
	dir := filepath.Join(root, ".doppels", "runs", runID)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	writeJSON(t, filepath.Join(dir, "request.json"), execution.RequestRecord{
		APIVersion: execution.APIVersion, Kind: "Request", ID: reqID, CreatedAt: now,
		Capability: execution.DefinitionReference{Name: "greet", Version: "1.0.0"},
		Inputs:     map[string]any{"name": "Ada"},
	})
	writeJSON(t, filepath.Join(dir, "run.json"), execution.RunRecord{
		APIVersion: execution.APIVersion, Kind: "Run", ID: runID, RequestID: reqID, CreatedAt: now,
		Capability: execution.DefinitionReference{Name: "greet", Version: "1.0.0"},
		Recipe:     &execution.DefinitionReference{Name: "greet-shell", Version: "1.0.0"},
	})
	events := []execution.RunEvent{
		{APIVersion: execution.APIVersion, Kind: "RunEvent", RunID: runID, Sequence: 0, OccurredAt: now, Type: "run_started"},
		{APIVersion: execution.APIVersion, Kind: "RunEvent", RunID: runID, Sequence: 1, OccurredAt: now, Type: "step_log", Data: map[string]any{"stdout": "logs/greet.stdout.log"}},
		{APIVersion: execution.APIVersion, Kind: "RunEvent", RunID: runID, Sequence: 2, OccurredAt: now, Type: "run_succeeded"},
	}
	var lines []string
	for _, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(raw))
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "greet.stdout.log"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	idx, err := runindex.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(runindex.Record{
		ID: runID, RequestID: reqID, Status: "succeeded", Source: runindex.SourceLocal,
		Capability: "greet@1.0.0", Recipe: "greet-shell@1.0.0",
		CreatedAt: now.Format(time.RFC3339), StateDir: dir,
	}); err != nil {
		t.Fatal(err)
	}
	idx.Close()

	items, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != runID || items[0].Status != "succeeded" {
		t.Fatalf("list = %#v", items)
	}

	detail, err := Load(root, runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Summary.Recipe != "greet-shell@1.0.0" || detail.Summary.Status != "succeeded" {
		t.Fatalf("detail = %#v", detail.Summary)
	}

	logs, err := Logs(root, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Content != "hello\n" || logs[0].StepID != "greet" {
		t.Fatalf("logs = %#v", logs)
	}
}

// TestLogsRejectsSymlinkEscapingTheRunDirectory reproduces review finding 7:
// the safety check on a log path was purely lexical (filepath.Clean plus a
// string-prefix test), so a *symlink* placed inside logs/ — its name looks
// perfectly safe ("logs/whatever.log") — could point anywhere on disk and
// Logs would follow it via os.ReadFile, exfiltrating arbitrary file content
// a malicious or compromised Recipe step wrote into the Run's own logs/
// directory. Confinement must resolve the real path (mirroring the
// EvalSymlinks-based pattern already used by localstate.Store.Resume) and
// reject anything that escapes the Run directory, not just reject
// lexically-suspicious relative paths.
func TestLogsRejectsSymlinkEscapingTheRunDirectory(t *testing.T) {
	root := t.TempDir()
	runID := "run01symlink"
	reqID := "req01symlink"
	dir := filepath.Join(root, ".doppels", "runs", runID)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	writeJSON(t, filepath.Join(dir, "request.json"), execution.RequestRecord{
		APIVersion: execution.APIVersion, Kind: "Request", ID: reqID, CreatedAt: now,
		Capability: execution.DefinitionReference{Name: "greet", Version: "1.0.0"},
		Inputs:     map[string]any{"name": "Ada"},
	})
	writeJSON(t, filepath.Join(dir, "run.json"), execution.RunRecord{
		APIVersion: execution.APIVersion, Kind: "Run", ID: runID, RequestID: reqID, CreatedAt: now,
		Capability: execution.DefinitionReference{Name: "greet", Version: "1.0.0"},
	})
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("do not leak this"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No stdout/stderr in any event, so Logs() falls back to listing logs/
	// directly — exactly the path that only lexically validated names.
	if err := os.Symlink(secret, filepath.Join(dir, "logs", "escape.stdout.log")); err != nil {
		t.Fatal(err)
	}

	logs, err := Logs(root, runID)
	if err == nil {
		t.Fatalf("Logs followed a symlink out of the Run directory: %#v", logs)
	}
}

func TestLogFilesDetectsTruncationMarkerWithoutFullRead(t *testing.T) {
	root := t.TempDir()
	runID := "run01truncated"
	reqID := "req01truncated"
	dir := filepath.Join(root, ".doppels", "runs", runID)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	writeJSON(t, filepath.Join(dir, "request.json"), execution.RequestRecord{
		APIVersion: execution.APIVersion, Kind: "Request", ID: reqID, CreatedAt: now,
		Capability: execution.DefinitionReference{Name: "greet", Version: "1.0.0"},
	})
	writeJSON(t, filepath.Join(dir, "run.json"), execution.RunRecord{
		APIVersion: execution.APIVersion, Kind: "Run", ID: runID, RequestID: reqID, CreatedAt: now,
		Capability: execution.DefinitionReference{Name: "greet", Version: "1.0.0"},
	})
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("x", 1<<20) + "\n[doppels: truncated after 1.0MiB]\n"
	if err := os.WriteFile(filepath.Join(dir, "logs", "run.stdout.log"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "run.stderr.log"), []byte("clean output\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	refs, err := LogFiles(root, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %#v", refs)
	}
	for _, ref := range refs {
		wantTruncated := ref.Stream == "stdout"
		if ref.Truncated != wantTruncated {
			t.Fatalf("ref %#v truncated = %v, want %v", ref, ref.Truncated, wantTruncated)
		}
	}
}

// TestLogsRejectsSymlinkedIntermediateDirectory covers the other shape of
// the same escape: not the log file itself, but the "logs" directory
// component being a symlink to somewhere outside the Run directory.
// filepath.EvalSymlinks resolves every path component, not just the leaf, so
// the same confinement check must catch this too.
func TestLogsRejectsSymlinkedIntermediateDirectory(t *testing.T) {
	root := t.TempDir()
	runID := "run01symlinkdir"
	reqID := "req01symlinkdir"
	dir := filepath.Join(root, ".doppels", "runs", runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	writeJSON(t, filepath.Join(dir, "request.json"), execution.RequestRecord{
		APIVersion: execution.APIVersion, Kind: "Request", ID: reqID, CreatedAt: now,
		Capability: execution.DefinitionReference{Name: "greet", Version: "1.0.0"},
		Inputs:     map[string]any{"name": "Ada"},
	})
	writeJSON(t, filepath.Join(dir, "run.json"), execution.RunRecord{
		APIVersion: execution.APIVersion, Kind: "Run", ID: runID, RequestID: reqID, CreatedAt: now,
		Capability: execution.DefinitionReference{Name: "greet", Version: "1.0.0"},
	})
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "escape.stdout.log"), []byte("do not leak this"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "logs")); err != nil {
		t.Fatal(err)
	}

	logs, err := Logs(root, runID)
	if err == nil {
		t.Fatalf("Logs followed a symlinked logs/ directory out of the Run directory: %#v", logs)
	}
}

func TestLoadRejectsInvalidID(t *testing.T) {
	_, err := Load(t.TempDir(), "../escape")
	if err == nil || !strings.Contains(err.Error(), "invalid Run id") {
		t.Fatalf("err = %v", err)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
