package runs

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/runindex"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runstate"
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
	_, created, err := idx.ReserveStart(fresh, "race-close", "irrelevant-fingerprint", runindex.ReservationEvidence{})
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
	if _, _, err := idx.ReserveStart(orphan, "orphan-key", fingerprint, runindex.ReservationEvidence{
		RequestJSON: `{"apiVersion":"doppels.so/v1alpha1","kind":"Request","id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","createdAt":"2026-09-14T00:00:00Z","idempotencyKey":"orphan-key","origin":"cli","capability":{"name":"greet","version":"1.0.0"},"inputs":{},"requestedBy":{"kind":"identity","id":"local-operator"}}`,
		RunJSON:     `{"apiVersion":"doppels.so/v1alpha1","kind":"Run","id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","requestId":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","createdAt":"2026-09-14T00:00:00Z","capability":{"name":"greet","version":"1.0.0"},"recipe":{"name":"greet-shell","version":"1.0.0"},"inputs":{},"executor":{"kind":"service","id":"doppels-runner"},"nodeId":"node-test"}`,
	}); err != nil {
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

// TestRestartReconcilesPreInitializationCrashesWithoutFabricatingEvidence
// reproduces the remaining finding-1 data-loss bug at both pre-initialization
// crash boundaries. Recovery must preserve the exact canonical evidence that
// the accepted startRun represented; synthesizing empty inputs, a different
// key, missing manifest hashes, or a different Space is dishonest history.
func TestRestartReconcilesPreInitializationCrashesWithoutFabricatingEvidence(t *testing.T) {
	for _, boundary := range []string{"post-reserve", "post-request"} {
		t.Run(boundary, func(t *testing.T) {
			service, root := runnerWorkspace(t, true)
			if _, _, err := project.WriteSpaceManifest(root, "review-space"); err != nil {
				t.Fatal(err)
			}
			resolved, err := service.ResolveExecution(root, "greet", "greet-shell")
			if err != nil {
				t.Fatal(err)
			}
			capabilityRef := execution.ReferenceCapability(resolved.Capability)
			recipeRef := execution.ReferenceRecipe(*resolved.Recipe)
			for name, digest := range map[string]string{
				"capability": capabilityRef.ManifestSHA256,
				"recipe":     recipeRef.ManifestSHA256,
			} {
				if len(digest) != 64 || digest == strings.Repeat("0", 64) {
					t.Fatalf("%s manifest digest = %q, want non-trivial SHA-256", name, digest)
				}
			}

			createdAt := time.Date(2026, 9, 14, 23, 0, 0, 123_000_000, time.UTC)
			manager := NewManager(context.Background(), service, Config{
				NodeID: "node-review", Now: func() time.Time { return createdAt },
				Environment: []string{"PATH=" + os.Getenv("PATH")},
			})
			requestPersisted := make(chan struct{}, 1)
			switch boundary {
			case "post-reserve":
				manager.testStopAfterReserve = true
			case "post-request":
				manager.testAfterRequestPersisted = func() error {
					requestPersisted <- struct{}{}
					return os.ErrClosed
				}
			}

			key := "honest-" + boundary
			params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","recipe":"greet-shell","inputs":{"count":7},"approvalMode":"auto","idempotencyKey":"` + key + `"}`)
			started, rpcErr := manager.Start("doppels-desktop", params)
			if rpcErr != nil {
				t.Fatalf("Start: %+v", rpcErr)
			}
			if boundary == "post-request" {
				<-requestPersisted
			}
			if err := manager.Close(); err != nil {
				t.Fatal(err)
			}

			fingerprintBefore := storedFingerprint(t, root, started.RunID)
			expectedFingerprint, err := requestFingerprint("greet@1.0.0", ptr("greet-shell@1.0.0"), []byte(`{"count":7}`))
			if err != nil {
				t.Fatal(err)
			}
			if fingerprintBefore != expectedFingerprint {
				t.Fatalf("stored fingerprint = %q, want %q", fingerprintBefore, expectedFingerprint)
			}

			expectedRequest := execution.RequestRecord{
				APIVersion: execution.APIVersion, Kind: "Request", ID: started.RequestID,
				CreatedAt: createdAt, IdempotencyKey: key, Origin: "cli",
				Capability: capabilityRef, Inputs: map[string]any{"count": float64(7)},
				RequestedBy: execution.ActorReference{Kind: "identity", ID: "local-operator"},
				Space:       "review-space",
			}
			expectedRun := execution.RunRecord{
				APIVersion: execution.APIVersion, Kind: "Run", ID: started.RunID,
				RequestID: started.RequestID, CreatedAt: createdAt,
				Capability: capabilityRef, Recipe: &recipeRef,
				Inputs:   map[string]any{"count": float64(7)},
				Executor: execution.ActorReference{Kind: "service", ID: "doppels-runner"},
				NodeID:   "node-review",
			}

			restarted := NewManager(context.Background(), service, Config{NodeID: "node-review", Now: func() time.Time { return createdAt.Add(time.Second) }})
			defer restarted.Close()
			result, rpcErr := restarted.GetRun(started.RunID, true)
			if rpcErr != nil {
				t.Fatalf("GetRun after restart: %+v", rpcErr)
			}
			if !reflect.DeepEqual(result.Request, expectedRequest) {
				t.Fatalf("recovered Request = %#v, want exact %#v", result.Request, expectedRequest)
			}
			detail, err := runstate.Load(root, started.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(detail.Run, expectedRun) {
				t.Fatalf("recovered Run = %#v, want exact %#v", detail.Run, expectedRun)
			}
			if result.Summary.Source != "desktop" || result.Summary.Status != "interrupted" {
				t.Fatalf("recovered summary = %#v, want desktop/interrupted", result.Summary)
			}
			if len(detail.Events) != 2 || detail.Events[0].Type != "run_created" || detail.Events[1].Type != "run_interrupted" {
				t.Fatalf("recovered events = %#v, want honest pre-execution terminal history", detail.Events)
			}

			retry, rpcErr := restarted.Start("doppels-desktop", params)
			if rpcErr != nil || retry != started {
				t.Fatalf("retry = %#v, %+v; want stable IDs %#v", retry, rpcErr, started)
			}
			if fingerprintAfter := storedFingerprint(t, root, started.RunID); fingerprintAfter != fingerprintBefore {
				t.Fatalf("fingerprint after restart = %q, want unchanged %q", fingerprintAfter, fingerprintBefore)
			}
		})
	}
}

func storedFingerprint(t *testing.T, root, runID string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, ".doppels", "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var fingerprint string
	if err := db.QueryRow(`SELECT request_fingerprint FROM idempotency WHERE run_id = ?`, runID).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func ptr(value string) *string { return &value }
