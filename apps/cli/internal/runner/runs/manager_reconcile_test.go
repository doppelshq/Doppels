package runs

import (
	"context"
	"os"
	"testing"

	"doppels.so/cli/internal/runindex"
	"doppels.so/cli/internal/runner/proto"
)

// TestStartNeverReservesDurablyAfterClose reproduces a deterministic race
// between Start and Close using a test-only barrier (no sleeps): Start is
// paused immediately before it would durably reserve, Close is driven to
// completion, and only then is Start released. Start must observe the
// manager as closed and refuse *before* committing anything durable — a
// reservation that outlives a Busy response can never be resumed (its
// goroutine never gets spawned), leaving the idempotency key permanently
// wedged on "running" with no execution behind it.
func TestStartNeverReservesDurablyAfterClose(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})

	reachedBarrier := make(chan struct{})
	releaseStart := make(chan struct{})
	manager.testBeforeReserve = func() {
		close(reachedBarrier)
		<-releaseStart
	}

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"race-close"}`)
	type outcome struct {
		result StartResult
		err    *proto.Error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := manager.Start("cli", params)
		done <- outcome{result, err}
	}()

	<-reachedBarrier
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(releaseStart)

	got := <-done
	if got.err == nil || got.err.Code != proto.CodeBusy {
		t.Fatalf("Start racing Close = %#v, %+v, want -32010 busy", got.result, got.err)
	}

	// No durable reservation may have leaked: reopening the index and
	// reserving the same (capability, key) from scratch must report this as
	// brand new.
	idx, err := runindex.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	fresh := runindex.Record{
		ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RequestID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		Status: "running", Source: "local", Capability: "greet@1.0.0", NodeID: "node-test",
		CreatedAt: "2026-09-14T00:00:00Z", StateDir: root,
	}
	_, created, err := idx.ReserveStart(fresh, "race-close", "irrelevant-fingerprint")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("a durable reservation leaked despite Start refusing due to shutdown")
	}
}

// TestOrdinaryRestartReconcilesOrphanedReservation simulates the narrow,
// PR6-scoped restart case: a Run row was durably reserved (ReserveStart
// committed) but the Runner exited before execute()'s goroutine reached
// runner.initialize(), so request.json/run.json were never written. This is
// NOT the kill -9 mid-Step recovery case (explicitly deferred to PR10,
// tracked in the desktop-first-runner plan): here nothing ever ran, so there
// is no partial Step work to preserve, only a stable reference to reconcile.
//
// A fresh Manager (simulating the next process) must resolve this to a
// coherent terminal Run: getRun must load it, and a durable retry with the
// same idempotency key must return the same stable identifiers instead of a
// reference that can never be loaded.
func TestOrdinaryRestartReconcilesOrphanedReservation(t *testing.T) {
	service, root := runnerWorkspace(t, true)

	idx, err := runindex.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	orphan := runindex.Record{
		ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", RequestID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		Status: "running", Source: "cli", Capability: "greet@1.0.0", Recipe: "greet-shell@1.0.0", NodeID: "node-test",
		CreatedAt: "2026-09-14T00:00:00Z", StateDir: root,
	}
	recipeRef := "greet-shell@1.0.0"
	fingerprint, err := requestFingerprint("greet@1.0.0", &recipeRef, []byte(`{"count":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := idx.ReserveStart(orphan, "orphan-key", fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	result, rpcErr := manager.GetRun(orphan.ID, true)
	if rpcErr != nil {
		t.Fatalf("GetRun on reconciled orphan: %+v", rpcErr)
	}
	if result.Summary.Status != "interrupted" {
		t.Fatalf("reconciled status = %q, want interrupted", result.Summary.Status)
	}
	if len(result.Events) == 0 || result.Events[len(result.Events)-1].Type != "run_interrupted" {
		t.Fatalf("reconciled events = %#v", result.Events)
	}

	// A durable retry with the original key must resolve to the same
	// identifiers rather than minting a new Run.
	retryParams := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"orphan-key"}`)
	retry, rpcErr := manager.Start("cli", retryParams)
	if rpcErr != nil {
		t.Fatalf("Start retry on reconciled orphan: %+v", rpcErr)
	}
	if retry.RunID != orphan.ID || retry.RequestID != orphan.RequestID {
		t.Fatalf("retry = %#v, want stable reference to %#v", retry, orphan)
	}
}
