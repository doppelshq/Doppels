package runsync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"doppels.so/cli/internal/registryclient"
	"doppels.so/cli/internal/runindex"
)

type fakeClient struct {
	calls int
	err   error
}

func (f *fakeClient) IngestRun(context.Context, string, string, string, registryclient.IngestPayload) error {
	f.calls++
	return f.err
}

func TestFlushAcksSuccessfulIngest(t *testing.T) {
	root := t.TempDir()
	runID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	reqID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	dir := filepath.Join(root, ".doppels", "runs", runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"apiVersion": "doppels.so/v1alpha1", "kind": "Request", "id": reqID,
		"createdAt": time.Now().UTC().Format(time.RFC3339Nano), "idempotencyKey": "local:" + reqID,
		"origin": "cli", "capability": map[string]any{"name": "greet", "version": "1.0.0", "manifestSha256": "aa", "schema": map[string]any{"id": "Capability", "sha256": "bb"}},
		"inputs": map[string]any{"name": "Ada"}, "requestedBy": map[string]any{"kind": "identity", "id": "tester"},
	}
	run := map[string]any{
		"apiVersion": "doppels.so/v1alpha1", "kind": "Run", "id": runID, "requestId": reqID,
		"createdAt":  time.Now().UTC().Format(time.RFC3339Nano),
		"capability": request["capability"], "inputs": request["inputs"],
		"executor": map[string]any{"kind": "identity", "id": "tester"},
	}
	writeJSON(t, filepath.Join(dir, "request.json"), request)
	writeJSON(t, filepath.Join(dir, "run.json"), run)
	events := []byte(`{"apiVersion":"doppels.so/v1alpha1","kind":"RunEvent","runId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","sequence":0,"occurredAt":"2026-08-02T12:00:00Z","type":"run_created"}` + "\n" +
		`{"apiVersion":"doppels.so/v1alpha1","kind":"RunEvent","runId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","sequence":1,"occurredAt":"2026-08-02T12:00:01Z","type":"run_succeeded","data":{"returns":{"message":"hi"}}}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), events, 0o600); err != nil {
		t.Fatal(err)
	}

	idx, err := runindex.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(runindex.Record{
		ID: runID, RequestID: reqID, Status: "succeeded", Source: runindex.SourceLocal,
		Capability: "greet@1.0.0", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), StateDir: dir,
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.EnqueueOutbox(runID, map[string]any{"id": runID}); err != nil {
		t.Fatal(err)
	}
	_ = idx.Close()

	client := &fakeClient{}
	synced, err := Flush(context.Background(), root, client, "token", "personal", "default")
	if err != nil {
		t.Fatal(err)
	}
	if synced != 1 || client.calls != 1 {
		t.Fatalf("synced=%d calls=%d", synced, client.calls)
	}
	idx, err = runindex.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	outbox, err := idx.ListOutbox()
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 0 {
		t.Fatalf("outbox = %#v", outbox)
	}
	record, err := idx.Get(runID)
	if err != nil {
		t.Fatal(err)
	}
	if record.SyncStatus != runindex.SyncSynced {
		t.Fatalf("sync_status = %q", record.SyncStatus)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
