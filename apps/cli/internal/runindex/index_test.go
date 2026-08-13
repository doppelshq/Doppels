package runindex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUpsertListAndOutbox(t *testing.T) {
	root := t.TempDir()
	idx, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	record := Record{
		ID: "run-1", RequestID: "req-1", Status: "succeeded", Source: SourceLocal,
		Capability: "greet@1.0.0", Recipe: "greet-shell@1.0.0",
		CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		StateDir:   filepath.Join(root, ".doppels", "runs", "run-1"),
		SyncStatus: SyncNone,
	}
	if err := idx.Upsert(record); err != nil {
		t.Fatal(err)
	}
	if err := idx.EnqueueOutbox(record.ID, map[string]any{"id": record.ID, "status": record.Status}); err != nil {
		t.Fatal(err)
	}

	items, err := idx.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Source != SourceLocal || items[0].SyncStatus != SyncPending {
		t.Fatalf("items = %#v", items)
	}
	outbox, err := idx.ListOutbox()
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 1 || outbox[0].RunID != "run-1" {
		t.Fatalf("outbox = %#v", outbox)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(outbox[0].Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["id"] != "run-1" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestBackfillFromDisk(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".doppels", "runs", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	runJSON := []byte(`{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","requestId":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","createdAt":"2026-08-02T12:00:00Z","capability":{"name":"greet","version":"1.0.0"},"recipe":{"name":"greet-shell","version":"1.0.0"}}`)
	if err := os.WriteFile(filepath.Join(dir, "run.json"), runJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"type":"run_succeeded"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	idx, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	items, err := idx.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "succeeded" || items[0].Capability != "greet@1.0.0" {
		t.Fatalf("backfill items = %#v", items)
	}
}
