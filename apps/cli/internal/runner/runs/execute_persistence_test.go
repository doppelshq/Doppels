package runs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/runindex"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runstate"
)

// failingCommitTerminalIndex wraps a real runIndex and can be told to fail
// every CommitTerminal call, letting tests reproduce a terminal-persistence
// failure on the automatic (non-manual) execution path deterministically.
type failingCommitTerminalIndex struct {
	runIndex
	failCommit atomic.Bool
}

func (f *failingCommitTerminalIndex) CommitTerminal(record runindex.Record, payload any) (bool, error) {
	if f.failCommit.Load() {
		return false, errors.New("injected CommitTerminal failure")
	}
	return f.runIndex.CommitTerminal(record, payload)
}

func waitForEventType(t *testing.T, root, runID, eventType string) execution.RunEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		detail, err := runstate.Load(root, runID)
		if err == nil {
			for _, event := range detail.Events {
				if event.Type == eventType {
					return event
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never observed event %s for run %s", eventType, runID)
	return execution.RunEvent{}
}

// TestExecuteDoesNotEmitOnFinishedWhenCommitTerminalFailsThenRepairsOnce
// reproduces a review finding: the automatic (approvalMode=auto) execution
// path treats execution.Execute's returned Result.Status as authoritative
// for firing OnFinished, even when the Run's own terminal CommitTerminal
// call failed. The engine already appended the terminal event to
// events.jsonl and set Result.Status to "succeeded" before that write was
// attempted (see execution/runner.go), so a naive "status looks terminal"
// check fires the node-event callback for a Run whose index/outbox never
// actually landed. persistence must complete — and be durably verified —
// before notification, exactly like the RunEvent broadcast path already
// requires.
//
// A subsequent Cancel call (idempotent on an already-terminal Run) is the
// existing reconciliation path: it repairs the stale index row via
// CommitTerminal and must then fire OnFinished exactly once.
func TestExecuteDoesNotEmitOnFinishedWhenCommitTerminalFailsThenRepairsOnce(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	writeRunFixture(t, filepath.Join(root, ".doppels", "recipes", "greet.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: greet-shell, version: 1.0.0}
provides: [greet]
runtime: shell
defaults: {approval: never}
steps:
  - id: run
    name: Run
    run: {shell: sh, script: "export VALUE=ok"}
    produces: {value: {env: VALUE}}
returns: {value: "{{ steps.run.value }}"}
`)

	var wrapped *failingCommitTerminalIndex
	var finished atomic.Int32
	manager := NewManager(context.Background(), service, Config{
		NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")},
		OnFinished: func(proto.RunSummary) { finished.Add(1) },
	})
	defer manager.Close()
	manager.openIndex = func(r string) (runIndex, error) {
		real, err := runindex.Open(r)
		if err != nil {
			return nil, err
		}
		wrapped = &failingCommitTerminalIndex{runIndex: real}
		return wrapped, nil
	}

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"execute-commit-fault"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	// openIndex only runs lazily on first use; Start already forced that.
	if wrapped == nil {
		t.Fatal("openIndex was never invoked")
	}
	wrapped.failCommit.Store(true)

	terminal := waitForEventType(t, root, started.RunID, "run_succeeded")

	// Give any (buggy) premature OnFinished call a chance to land before
	// asserting its absence.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if finished.Load() != 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := finished.Load(); got != 0 {
		t.Fatalf("OnFinished calls before terminal persistence succeeded = %d, want 0", got)
	}
	_, _, record, err := manager.findRecord(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status == "succeeded" {
		t.Fatalf("index status = %q, want still non-terminal since CommitTerminal failed", record.Status)
	}

	wrapped.failCommit.Store(false)
	status, rpcErr := manager.Cancel(started.RunID, "")
	if rpcErr != nil || status != "succeeded" {
		t.Fatalf("Cancel (reconcile) = %q, %+v", status, rpcErr)
	}
	if got := finished.Load(); got != 1 {
		t.Fatalf("OnFinished calls after reconciliation = %d, want 1", got)
	}
	_, _, record, err = manager.findRecord(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "succeeded" || record.FinishedAt != terminal.OccurredAt.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("repaired record = %#v, want succeeded finishedAt %q", record, terminal.OccurredAt)
	}

	// Idempotent retry must not emit a second OnFinished.
	status, rpcErr = manager.Cancel(started.RunID, "")
	if rpcErr != nil || status != "succeeded" {
		t.Fatalf("idempotent Cancel = %q, %+v", status, rpcErr)
	}
	if got := finished.Load(); got != 1 {
		t.Fatalf("OnFinished calls after idempotent retry = %d, want 1", got)
	}
}
