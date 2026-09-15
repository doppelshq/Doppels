package runs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runstate"
)

// concurrencyFanout is the default fan-out for the adversarial tests in this
// file; CI can raise it (e.g. via -run + a larger -count, or by editing this
// constant locally) to the plan's 128 without changing test logic.
const concurrencyFanout = 32

// TestConcurrentStartRunSameIdempotencyKeyConflictingInputsOneWinner
// reproduces RFC §12's idempotency contract under real adversarial
// pressure. runindex's idempotency table keys reservations on
// (capability, idempotency_key) together (internal/runindex/index.go's own
// `ON CONFLICT(capability, idempotency_key)`), so two different Capability
// names sharing one idempotencyKey never contend at all — each gets its own
// independent row. The actual conflict axis is the request fingerprint
// (capability + recipe + inputs) recorded under that one row: this test
// races concurrencyFanout goroutines against the *same* Capability and
// idempotencyKey but alternating input values, so every attempt except
// whichever ReserveStart call actually wins the race has a fingerprint that
// conflicts with the winner's. The first winner's (requestId, runId) must be
// the only pair ever returned successfully — including to every later
// attempt that happened to submit the exact same inputs as the winner,
// which must observe the winner's identifiers rather than minting its own —
// and every attempt with different inputs must be refused with -32602 (a
// fingerprint conflict), never silently coerced into a duplicate Run or a
// fabricated success.
func TestConcurrentStartRunSameIdempotencyKeyConflictingInputsOneWinner(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-concurrency", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	const key = "shared-idempotency-key"
	type outcome struct {
		result StartResult
		err    *proto.Error
	}
	results := make([]outcome, concurrencyFanout)
	var ready, done sync.WaitGroup
	ready.Add(concurrencyFanout)
	done.Add(concurrencyFanout)
	start := make(chan struct{})
	for i := 0; i < concurrencyFanout; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			count := i % 2 // two distinct input values -> two distinct fingerprints.
			params := []byte(fmt.Sprintf(`{"workspace":%s,"capability":"greet","inputs":{"count":%d},"approvalMode":"auto","idempotencyKey":%q}`, quote(root), count, key))
			result, rpcErr := manager.Start("cli", params)
			results[i] = outcome{result, rpcErr}
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	var winner *StartResult
	successes, conflicts := 0, 0
	for i, got := range results {
		switch {
		case got.err == nil:
			successes++
			if winner == nil {
				winner = &results[i].result
			} else if got.result != *winner {
				t.Fatalf("attempt %d succeeded with %#v, want the single winner %#v", i, got.result, *winner)
			}
		case got.err.Code == proto.CodeInvalidParams:
			conflicts++
		default:
			t.Fatalf("attempt %d: unexpected error %+v", i, got.err)
		}
	}
	if winner == nil {
		t.Fatal("no attempt ever won the reservation race")
	}
	if successes+conflicts != concurrencyFanout {
		t.Fatalf("successes(%d) + conflicts(%d) != fanout(%d)", successes, conflicts, concurrencyFanout)
	}
	waitForStatus(t, root, winner.RunID, "succeeded")
}

// TestConcurrentCancelSameRunExactlyOneTerminalEvent races concurrencyFanout
// goroutines all cancelling the same still-running Run. Manager.Cancel is
// documented as idempotent (runs/manager.go), so every caller is expected to
// receive success — the invariant under real concurrency is narrower and
// easy to violate with a naive read-then-write: exactly one terminal event
// may ever be appended to the Run's durable log, and every caller must
// observe the identical resulting status, never racing two different
// terminal outcomes (e.g. half the callers seeing "interrupted" and half
// fabricating a second, different terminal state).
func TestConcurrentCancelSameRunExactlyOneTerminalEvent(t *testing.T) {
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
    run: {shell: sh, script: "touch started; sleep 30; export VALUE=ok"}
    produces: {value: {env: VALUE}}
returns: {value: "{{ steps.run.value }}"}
`)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-concurrency", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"cancel-race"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waitForFile(t, filepath.Join(root, "started"))

	statuses := make([]string, concurrencyFanout)
	errs := make([]*proto.Error, concurrencyFanout)
	var ready, done sync.WaitGroup
	ready.Add(concurrencyFanout)
	done.Add(concurrencyFanout)
	start := make(chan struct{})
	for i := 0; i < concurrencyFanout; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			status, err := manager.Cancel(started.RunID, "concurrent cancel")
			statuses[i] = status
			errs[i] = err
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	for i := range statuses {
		if errs[i] != nil {
			t.Fatalf("Cancel[%d] = %+v, want success (Cancel is documented idempotent)", i, errs[i])
		}
		if statuses[i] != "interrupted" {
			t.Fatalf("Cancel[%d] status = %q, want interrupted (identical for every caller)", i, statuses[i])
		}
	}

	detail, err := runstate.Load(root, started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, event := range detail.Events {
		if isTerminalEventType(event.Type) {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events = %d, want exactly 1; events = %#v", terminals, detail.Events)
	}
}

// TestConcurrentSubscribersSeeIdenticalContiguousSequenceNoDuplicates
// verifies RFC §10's sequence contiguity invariant holds when
// concurrencyFanout independent subscribers register for the same Run at
// once, racing the engine's own broadcast goroutine: every subscriber must
// observe the exact same event sequence, gap-free and duplicate-free, from
// 0 through the terminal event, regardless of how the broadcaster's fan-out
// loop happened to interleave with each subscriber's own registration.
func TestConcurrentSubscribersSeeIdenticalContiguousSequenceNoDuplicates(t *testing.T) {
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
    run: {shell: sh, script: "touch started; sleep 1; export VALUE=ok"}
    produces: {value: {env: VALUE}}
returns: {value: "{{ steps.run.value }}"}
`)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-concurrency", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"subscribe-race"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waitForFile(t, filepath.Join(root, "started"))

	subscribers := make([]*fakeSubscriber, concurrencyFanout)
	replays := make([]SubscribeResult, concurrencyFanout)
	var ready sync.WaitGroup
	ready.Add(concurrencyFanout)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(concurrencyFanout)
	for i := 0; i < concurrencyFanout; i++ {
		go func(i int) {
			defer wg.Done()
			sub := newFakeSubscriber()
			subscribers[i] = sub
			ready.Done()
			<-start
			result, rpcErr := manager.Subscribe(started.RunID, 0, sub)
			if rpcErr != nil {
				t.Errorf("Subscribe[%d]: %+v", i, rpcErr)
				return
			}
			replays[i] = result
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	waitForStatus(t, root, started.RunID, "succeeded")
	detail, err := runstate.Load(root, started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	terminalSequence := detail.Events[len(detail.Events)-1].Sequence

	for i := range subscribers {
		combined := append([]proto.RunEventPayload(nil), replays[i].Events...)
		if len(combined) == 0 || combined[len(combined)-1].Sequence < terminalSequence {
			live := subscribers[i].waitForEvents(t, terminalSequence-len(combined)+1)
			combined = append(combined, live...)
		}
		seen := map[int]bool{}
		for _, event := range combined {
			if seen[event.Sequence] {
				t.Fatalf("subscriber %d saw duplicate sequence %d: %#v", i, event.Sequence, combined)
			}
			seen[event.Sequence] = true
		}
		for sequence := 0; sequence <= terminalSequence; sequence++ {
			if !seen[sequence] {
				t.Fatalf("subscriber %d never saw sequence %d (gap); combined = %#v", i, sequence, combined)
			}
		}
	}
}

// TestConcurrentStartRunDistinctCapabilitiesAllSucceedNoGoroutineLeak fires
// 100 simultaneous startRun calls for 100 distinct idempotency keys against
// the same Capability (each independent, no idempotency contention) and
// requires every one to reach "succeeded" — Manager's per-Run bookkeeping
// (m.active, m.subs) must not serialize or drop unrelated Runs under load —
// then asserts the engine's own goroutines (per-Run execute(), subscriber
// forwarders, live-log broadcasters) have all wound back down to within a
// small tolerance of the pre-test baseline instead of accumulating one
// leaked goroutine per Run.
func TestConcurrentStartRunDistinctCapabilitiesAllSucceedNoGoroutineLeak(t *testing.T) {
	const fanout = 100
	service, root := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-concurrency", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	baseline := runtime.NumGoroutine()
	var succeeded int64
	var wg sync.WaitGroup
	wg.Add(fanout)
	start := make(chan struct{})
	for i := 0; i < fanout; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			key := fmt.Sprintf("distinct-%d", i)
			params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"` + key + `"}`)
			result, rpcErr := manager.Start("cli", params)
			if rpcErr != nil {
				t.Errorf("Start[%d]: %+v", i, rpcErr)
				return
			}
			waitForStatus(t, root, result.RunID, "succeeded")
			atomic.AddInt64(&succeeded, 1)
		}(i)
	}
	close(start)
	wg.Wait()

	if int(succeeded) != fanout {
		t.Fatalf("succeeded = %d, want %d", succeeded, fanout)
	}

	const tolerance = 20
	deadline := time.Now().Add(5 * time.Second)
	for {
		current := runtime.NumGoroutine()
		if current <= baseline+tolerance {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count = %d, want <= baseline %d + tolerance %d after every Run finished", current, baseline, tolerance)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
