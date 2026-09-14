package runindex

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
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
	if items[0].FinishedAt != "" {
		t.Fatalf("FinishedAt without occurredAt = %q, want empty", items[0].FinishedAt)
	}
}

// TestBackfillFromDiskCapturesNodeIDAndFinishedAt reproduces half of review
// finding 12: a fresh backfill (a row missing entirely from the index)
// never read run.json's nodeId at all, and never computed finishedAt from
// the terminal event's timestamp — both were silently left empty even
// though the authoritative disk state has them.
func TestBackfillFromDiskCapturesNodeIDAndFinishedAt(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".doppels", "runs", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	runJSON := []byte(`{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","requestId":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","createdAt":"2026-08-02T12:00:00Z","nodeId":"node-disk","capability":{"name":"greet","version":"1.0.0"}}`)
	if err := os.WriteFile(filepath.Join(dir, "run.json"), runJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"type":"run_succeeded","occurredAt":"2026-08-02T12:05:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	idx, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	record, err := idx.Get("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if record.NodeID != "node-disk" {
		t.Fatalf("NodeID = %q, want %q", record.NodeID, "node-disk")
	}
	if record.FinishedAt != "2026-08-02T12:05:00Z" {
		t.Fatalf("FinishedAt = %q, want %q", record.FinishedAt, "2026-08-02T12:05:00Z")
	}
}

// TestBackfillEnrichesExistingRowsMissingNodeIDOrFinishedAt reproduces the
// other half of finding 12: a row that already exists in the index (e.g.
// migrated from the pre-nodeId/finished_at schema, so both columns default
// to "") was skipped entirely by Backfill — it only ever handled rows
// missing outright. A present, non-empty value must never be overwritten
// (it may be more current than the disk snapshot), but a genuinely missing
// one should be filled from the authoritative on-disk state.
func TestBackfillEnrichesExistingRowsMissingNodeIDOrFinishedAt(t *testing.T) {
	root := t.TempDir()
	runID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	dir := filepath.Join(root, ".doppels", "runs", runID)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	runJSON := []byte(`{"id":"` + runID + `","requestId":"req-c","createdAt":"2026-08-02T12:00:00Z","nodeId":"node-disk","capability":{"name":"greet","version":"1.0.0"}}`)
	if err := os.WriteFile(filepath.Join(dir, "run.json"), runJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"type":"run_succeeded","occurredAt":"2026-08-02T12:05:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate a pre-existing index row (as if migrated from the old schema):
	// present in the index already, with finishedAt still at its
	// post-ALTER-TABLE default ("") and, deliberately, NodeID and Source
	// values a disk-derived record would not produce — proving Backfill
	// enriches only the missing field and leaves present values untouched.
	idx, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(Record{
		ID: runID, RequestID: "req-c", Status: "succeeded", Source: "cloud",
		Capability: "greet@1.0.0", NodeID: "node-index",
		CreatedAt: "2026-08-02T12:00:00Z", StateDir: dir,
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	record, err := reopened.Get(runID)
	if err != nil {
		t.Fatal(err)
	}
	if record.NodeID != "node-index" {
		t.Fatalf("NodeID = %q, want preserved index value %q", record.NodeID, "node-index")
	}
	if record.FinishedAt != "2026-08-02T12:05:00Z" {
		t.Fatalf("FinishedAt = %q, want enriched from disk %q", record.FinishedAt, "2026-08-02T12:05:00Z")
	}
	if record.Source != "cloud" {
		t.Fatalf("Source = %q, want unchanged %q (Backfill must not overwrite present fields)", record.Source, "cloud")
	}
}

// TestBackfillEnrichmentNeverClobbersConcurrentEngineUpsert pins the race
// between Backfill reading an incomplete row and writing its disk-derived
// enrichment. The engine may publish a newer full Record in that window;
// Backfill must fill only columns that are still empty at write time and
// leave every fresh engine-owned field untouched.
func TestBackfillEnrichmentNeverClobbersConcurrentEngineUpsert(t *testing.T) {
	root := t.TempDir()
	runID := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	dir := filepath.Join(root, ".doppels", "runs", runID)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), []byte(`{"id":"`+runID+`","requestId":"request-disk","createdAt":"2026-08-02T12:00:00Z","nodeId":"node-disk","capability":{"name":"disk","version":"1.0.0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"type":"run_succeeded","occurredAt":"2026-08-02T12:05:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	idx, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	stale := Record{
		ID: runID, RequestID: "request-stale", Status: "running", Source: SourceLocal,
		Capability: "disk@1.0.0", CreatedAt: "2026-08-02T12:00:00Z", StateDir: dir,
		SyncStatus: SyncNone,
	}
	if err := idx.Upsert(stale); err != nil {
		t.Fatal(err)
	}

	readComplete := make(chan struct{})
	releaseWrite := make(chan struct{})
	idx.testBeforeEnrichUpdate = func() {
		close(readComplete)
		<-releaseWrite
	}
	backfillDone := make(chan error, 1)
	go func() {
		backfillDone <- idx.Backfill()
	}()
	<-readComplete

	fresh := Record{
		ID: runID, RequestID: "request-engine", Status: "cancelled", Source: "desktop",
		Capability: "engine@2.0.0", Recipe: "engine-recipe@2.0.0", NodeID: "node-engine",
		CreatedAt: "2026-08-02T12:00:00Z", FinishedAt: "2026-08-02T12:06:00Z",
		StateDir: dir, SyncStatus: SyncPending,
	}
	if err := idx.Upsert(fresh); err != nil {
		t.Fatal(err)
	}
	close(releaseWrite)
	if err := <-backfillDone; err != nil {
		t.Fatal(err)
	}

	got, err := idx.Get(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got != fresh {
		t.Fatalf("record after concurrent enrichment = %#v, want fresh engine record %#v", got, fresh)
	}
}

func TestReserveStartIsAtomicAcrossIndexesAndSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}

	const callers = 24
	type outcome struct {
		reservation IdempotencyRecord
		created     bool
		err         error
	}
	outcomes := make(chan outcome, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		go func(i int) {
			ready.Done()
			<-start
			idx := first
			if i%2 == 1 {
				idx = second
			}
			record := Record{
				ID: "run-" + string(rune('a'+i)), RequestID: "request-" + string(rune('a'+i)),
				Status: "running", Capability: "greet@1.0.0", CreatedAt: "2026-09-14T12:00:00Z",
				StateDir: filepath.Join(root, ".doppels", "runs", "candidate"),
			}
			reservation, created, err := idx.ReserveStart(record, "same-key", "same-fingerprint", ReservationEvidence{})
			outcomes <- outcome{reservation: reservation, created: created, err: err}
		}(i)
	}
	ready.Wait()
	close(start)

	var winner IdempotencyRecord
	createdCount := 0
	all := make([]outcome, 0, callers)
	for i := 0; i < callers; i++ {
		got := <-outcomes
		all = append(all, got)
		if got.err != nil {
			t.Fatalf("ReserveStart: %v", got.err)
		}
		if got.created {
			createdCount++
			winner = got.reservation
		}
	}
	if createdCount != 1 {
		t.Fatalf("created reservations = %d, want 1", createdCount)
	}
	for _, got := range all {
		if got.reservation != winner {
			t.Fatalf("reservation = %#v, winner = %#v", got.reservation, winner)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	retry, created, err := reopened.ReserveStart(Record{
		ID: "other-run", RequestID: "other-request", Status: "running",
		Capability: "greet@1.0.0", CreatedAt: "2026-09-14T13:00:00Z", StateDir: root,
	}, "same-key", "same-fingerprint", ReservationEvidence{})
	if err != nil || created || retry != winner {
		t.Fatalf("retry = %#v, created=%v, err=%v; want original %#v", retry, created, err, winner)
	}
	if _, _, err := reopened.ReserveStart(Record{
		ID: "collision", RequestID: "collision-request", Status: "running",
		Capability: "greet@1.0.0", CreatedAt: "2026-09-14T13:00:00Z", StateDir: root,
	}, "same-key", "different-fingerprint", ReservationEvidence{}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("collision error = %v, want ErrIdempotencyConflict", err)
	}
	records, err := reopened.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != winner.RunID {
		t.Fatalf("records = %#v, want only winning Run", records)
	}
}

func TestListPageUsesStableCursorAndBoundedFilters(t *testing.T) {
	root := t.TempDir()
	idx, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	for _, record := range []Record{
		{ID: "run-b", RequestID: "req-b", Status: "failed", Capability: "other@1.0.0", CreatedAt: "2026-09-14T13:00:00Z", StateDir: root},
		{ID: "run-c", RequestID: "req-c", Status: "running", Capability: "greet@1.0.0", CreatedAt: "2026-09-14T14:00:00Z", StateDir: root},
		{ID: "run-b2", RequestID: "req-b2", Status: "failed", Capability: "greet@1.0.0", CreatedAt: "2026-09-14T13:00:00Z", StateDir: root},
		{ID: "run-a", RequestID: "req-a", Status: "succeeded", Capability: "greet@1.0.0", CreatedAt: "2026-09-14T13:00:00Z", StateDir: root},
		{ID: "run-d", RequestID: "req-d", Status: "running", Capability: "greet@1.0.0", CreatedAt: "2026-09-14T12:00:00Z", StateDir: root},
	} {
		if err := idx.Upsert(record); err != nil {
			t.Fatal(err)
		}
	}

	first, err := idx.ListPage(ListQuery{Capability: "greet@1.0.0", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertRunIDs(t, first.Records, "run-c", "run-a")
	if first.NextCursor == "" {
		t.Fatal("first page has no next cursor")
	}
	second, err := idx.ListPage(ListQuery{Capability: "greet@1.0.0", Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	assertRunIDs(t, second.Records, "run-b2", "run-d")
	if second.NextCursor != "" {
		t.Fatalf("last page cursor = %q, want empty", second.NextCursor)
	}

	failed, err := idx.ListPage(ListQuery{Status: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	assertRunIDs(t, failed.Records, "run-b", "run-b2")

	if _, err := idx.ListPage(ListQuery{Capability: "other@1.0.0", Limit: 2, Cursor: first.NextCursor}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cursor reused with different filter: %v, want ErrInvalidCursor", err)
	}
	if _, err := idx.ListPage(ListQuery{Cursor: "not-an-opaque-cursor"}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("malformed cursor: %v, want ErrInvalidCursor", err)
	}
	if _, err := idx.ListPage(ListQuery{Limit: MaxPageSize + 1}); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("oversize limit: %v, want ErrInvalidLimit", err)
	}
}

// TestUpsertNeverChangesCreatedAtAfterTheFirstInsert reproduces review
// finding 10: created_at is the sort key ListPage's keyset pagination
// relies on ("ORDER BY created_at DESC, id ASC"). A Run's row is first
// written at reservation time (ReserveStart, T0) and then Upserted again
// once the engine actually initializes it (T2, later) — if that second
// write were allowed to change created_at, a row could shift position (or
// vanish/duplicate across a client's page boundary) between two calls to
// ListPage. created_at must be assigned once and stay immutable across
// every subsequent Upsert, no matter what value a caller passes.
func TestUpsertNeverChangesCreatedAtAfterTheFirstInsert(t *testing.T) {
	root := t.TempDir()
	idx, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	original := Record{
		ID: "run-1", RequestID: "req-1", Status: "running", Capability: "greet@1.0.0",
		CreatedAt: "2026-09-14T12:00:00Z", StateDir: root,
	}
	if err := idx.Upsert(original); err != nil {
		t.Fatal(err)
	}
	later := original
	later.Status = "succeeded"
	later.CreatedAt = "2026-09-14T13:00:00Z" // a later Upsert (e.g. engine init) proposing a different value
	if err := idx.Upsert(later); err != nil {
		t.Fatal(err)
	}

	got, err := idx.Get("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt != original.CreatedAt {
		t.Fatalf("created_at = %q after a later Upsert, want unchanged %q", got.CreatedAt, original.CreatedAt)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status = %q, want succeeded (other columns must still update)", got.Status)
	}
}

func TestEncodeCursorResumesListPageAtExactBoundary(t *testing.T) {
	root := t.TempDir()
	idx, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	for _, record := range []Record{
		{ID: "run-a", RequestID: "req-a", Status: "running", Capability: "greet@1.0.0", CreatedAt: "2026-09-14T13:00:00Z", StateDir: root},
		{ID: "run-b", RequestID: "req-b", Status: "running", Capability: "greet@1.0.0", CreatedAt: "2026-09-14T12:00:00Z", StateDir: root},
	} {
		if err := idx.Upsert(record); err != nil {
			t.Fatal(err)
		}
	}
	cursor, err := EncodeCursor("greet@1.0.0", "", "2026-09-14T13:00:00Z", "run-a")
	if err != nil {
		t.Fatal(err)
	}
	page, err := idx.ListPage(ListQuery{Capability: "greet@1.0.0", Cursor: cursor})
	if err != nil {
		t.Fatal(err)
	}
	assertRunIDs(t, page.Records, "run-b")

	if _, err := EncodeCursor("greet@1.0.0", "", "not-a-timestamp", "run-a"); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("invalid timestamp error = %v, want ErrInvalidCursor", err)
	}
}

func assertRunIDs(t *testing.T, records []Record, want ...string) {
	t.Helper()
	if len(records) != len(want) {
		t.Fatalf("records = %#v, want ids %v", records, want)
	}
	for i := range want {
		if records[i].ID != want[i] {
			t.Fatalf("record %d id = %q, want %q (all = %#v)", i, records[i].ID, want[i], records)
		}
	}
}
