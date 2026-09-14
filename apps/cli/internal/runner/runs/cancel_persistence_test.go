package runs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"doppels.so/cli/internal/runindex"
	"doppels.so/cli/internal/runner/proto"
)

// gatingUpsertIndex wraps a real runIndex. Once armed, the next Upsert call
// blocks on a channel until the test releases it, letting a test
// deterministically prove an ordering invariant (e.g. "no notification
// before persistence") instead of racing an async subscriber goroutine
// against Upsert's completion. Unarmed, Upsert passes straight through so
// setup writes (e.g. the initial "running"/"pending_manual" index rows) are
// unaffected.
type gatingUpsertIndex struct {
	runIndex
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *gatingUpsertIndex) Upsert(record runindex.Record) error {
	if g.armed.Load() {
		close(g.entered)
		<-g.release
	}
	return g.runIndex.Upsert(record)
}

// failingUpsertIndex wraps a real runIndex and can be told to fail every
// Upsert call, letting tests reproduce a persistence failure deterministically
// instead of via timing.
type failingUpsertIndex struct {
	runIndex
	failUpsert atomic.Bool
}

func (f *failingUpsertIndex) Upsert(record runindex.Record) error {
	if f.failUpsert.Load() {
		return errors.New("injected Upsert failure")
	}
	return f.runIndex.Upsert(record)
}

// newFailingUpsertManager builds a Manager whose openIndex wraps the real
// runIndex in a decorator that can be told to fail every Upsert call. The
// decorator is only constructed once the Manager actually opens the
// workspace's index (e.g. on the first Start), so callers read *wrapped
// after that happens rather than the returned pointer's value at call time.
func newFailingUpsertManager(t *testing.T) (manager *Manager, root string, wrapped **failingUpsertIndex) {
	t.Helper()
	service, root := runnerWorkspace(t, false)
	var captured *failingUpsertIndex
	manager = NewManager(context.Background(), service, Config{NodeID: "node-test"})
	manager.openIndex = func(r string) (runIndex, error) {
		real, err := runindex.Open(r)
		if err != nil {
			return nil, err
		}
		captured = &failingUpsertIndex{runIndex: real}
		return captured, nil
	}
	return manager, root, &captured
}

// TestCancelPendingManualPersistsIndexBeforeBroadcast reproduces review
// finding 2: cancelRun's pendingManual path appended the terminal event to
// events.jsonl, broadcast it to subscribers, and only THEN updated the
// SQLite index status. A subscriber that reacts to the terminal notification
// by immediately querying the index (e.g. via listRuns) could observe a
// stale, non-terminal status. The index write must complete before any
// notification goes out.
func TestCancelPendingManualPersistsIndexBeforeBroadcast(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	var gate *gatingUpsertIndex
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	manager.openIndex = func(r string) (runIndex, error) {
		real, err := runindex.Open(r)
		if err != nil {
			return nil, err
		}
		gate = &gatingUpsertIndex{runIndex: real, entered: make(chan struct{}), release: make(chan struct{})}
		return gate, nil
	}
	defer manager.Close()

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"interactive","idempotencyKey":"cancel-persist"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waitForStatus(t, root, started.RunID, "pending_manual")

	notified := make(chan struct{}, 1)
	sub := &signalingSubscriber{notified: notified}
	if _, rpcErr := manager.Subscribe(started.RunID, 0, sub); rpcErr != nil {
		t.Fatalf("Subscribe: %+v", rpcErr)
	}

	gate.armed.Store(true)
	cancelDone := make(chan cancelOutcome, 1)
	go func() {
		status, err := manager.Cancel(started.RunID, "")
		cancelDone <- cancelOutcome{status: status, err: err}
	}()

	// Cancel must be blocked inside Upsert (persisting) before any
	// notification can possibly have gone out. If a notification arrives
	// here, broadcast ran before persistence — the bug.
	select {
	case <-gate.entered:
	case <-notified:
		t.Fatal("subscriber was notified before the index Upsert call even began")
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel never reached the index Upsert call")
	}
	select {
	case <-notified:
		t.Fatal("subscriber was notified while the index Upsert call was still blocked (not yet persisted)")
	case <-time.After(50 * time.Millisecond):
	}

	close(gate.release)
	select {
	case outcome := <-cancelDone:
		if outcome.err != nil || outcome.status != "cancelled" {
			t.Fatalf("Cancel = %q, %+v", outcome.status, outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel never returned after the Upsert was released")
	}
	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber was never notified after Cancel completed")
	}
}

type cancelOutcome struct {
	status string
	err    *proto.Error
}

type signalingSubscriber struct {
	notified chan struct{}
}

func (s *signalingSubscriber) DeliverRunEvent(event proto.RunEventPayload) bool {
	if event.Type == "run_cancelled" || event.Type == "run_interrupted" {
		s.notified <- struct{}{}
	}
	return true
}

func (s *signalingSubscriber) DeliverRunGap(string, int) {}
func (s *signalingSubscriber) Defer(fn func())           { fn() }
func (s *signalingSubscriber) NotifyClosed(func())       {}

// TestCancelPendingManualRetriesAfterFailedUpsertWithoutPrematureBroadcast
// injects an Upsert failure on the first Cancel attempt: the event must
// still be durable in events.jsonl (JSONL is authoritative for status), no
// notification may go out for the state the index failed to persist, Cancel
// must report the error, and a subsequent Cancel call must self-heal the
// stale index row without emitting a second terminal event.
func TestCancelPendingManualRetriesAfterFailedUpsertWithoutPrematureBroadcast(t *testing.T) {
	manager, root, wrapped := newFailingUpsertManager(t)
	defer manager.Close()

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"interactive","idempotencyKey":"cancel-fault"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waitForStatus(t, root, started.RunID, "pending_manual")

	events := make(chan struct{}, 8)
	sub := &countingSubscriber{events: events}
	if _, rpcErr := manager.Subscribe(started.RunID, 0, sub); rpcErr != nil {
		t.Fatalf("Subscribe: %+v", rpcErr)
	}

	(*wrapped).failUpsert.Store(true)
	if _, rpcErr := manager.Cancel(started.RunID, ""); rpcErr == nil {
		t.Fatal("Cancel with injected Upsert failure should have errored")
	}
	select {
	case <-events:
		t.Fatal("broadcast fired despite the index write failing")
	case <-time.After(200 * time.Millisecond):
	}
	_, _, record, err := manager.findRecord(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "pending_manual" {
		t.Fatalf("index status after failed Upsert = %q, want pending_manual (unchanged)", record.Status)
	}

	(*wrapped).failUpsert.Store(false)
	status, rpcErr := manager.Cancel(started.RunID, "")
	if rpcErr != nil || status != "cancelled" {
		t.Fatalf("Cancel retry = %q, %+v", status, rpcErr)
	}
	select {
	case <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber was never notified after the successful retry")
	}
	select {
	case <-events:
		t.Fatal("a duplicate terminal event was broadcast")
	case <-time.After(200 * time.Millisecond):
	}
	_, _, record, err = manager.findRecord(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "cancelled" {
		t.Fatalf("index status after retry = %q, want cancelled", record.Status)
	}
}

type countingSubscriber struct {
	events chan struct{}
}

func (c *countingSubscriber) DeliverRunEvent(proto.RunEventPayload) bool {
	c.events <- struct{}{}
	return true
}
func (c *countingSubscriber) DeliverRunGap(string, int) {}
func (c *countingSubscriber) Defer(fn func())           { fn() }
func (c *countingSubscriber) NotifyClosed(func())       {}
