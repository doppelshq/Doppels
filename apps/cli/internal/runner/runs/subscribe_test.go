package runs

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"doppels.so/cli/internal/runner/proto"
)

type fakeSubscriber struct {
	mu     sync.Mutex
	events []proto.RunEventPayload
	gaps   []int
	notify chan struct{}
}

func newFakeSubscriber() *fakeSubscriber {
	return &fakeSubscriber{notify: make(chan struct{}, 256)}
}

func (f *fakeSubscriber) DeliverRunEvent(event proto.RunEventPayload) {
	f.mu.Lock()
	f.events = append(f.events, event)
	f.mu.Unlock()
	f.notify <- struct{}{}
}

func (f *fakeSubscriber) DeliverRunGap(runID string, fromSequence int) {
	f.mu.Lock()
	f.gaps = append(f.gaps, fromSequence)
	f.mu.Unlock()
	f.notify <- struct{}{}
}

func (f *fakeSubscriber) waitForEvents(t *testing.T, n int) []proto.RunEventPayload {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		got := len(f.events)
		f.mu.Unlock()
		if got >= n {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]proto.RunEventPayload(nil), f.events...)
		}
		select {
		case <-f.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for %d events, got %d", n, got)
		}
	}
}

func TestSubscribeReplaysThenStreamsLiveWithoutGapOrDuplicate(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	// A Recipe that blocks past the reservation+subscribe window, so the
	// terminal event is guaranteed to arrive on the live path rather than
	// already being present in the synchronous replay snapshot.
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
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"sub"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}

	sub := newFakeSubscriber()
	result, rpcErr := manager.Subscribe(started.RunID, 0, sub)
	if rpcErr != nil {
		t.Fatalf("Subscribe: %+v", rpcErr)
	}
	if len(result.Events) == 0 {
		t.Fatal("replay snapshot was empty; run_created should already be persisted")
	}

	waitForFile(t, filepath.Join(root, "started"))
	status, rpcErr := manager.Cancel(started.RunID, "")
	if rpcErr != nil || status != "interrupted" {
		t.Fatalf("Cancel = %q, %+v", status, rpcErr)
	}

	live := sub.waitForEvents(t, 1)

	seen := map[int]int{}
	for _, event := range result.Events {
		seen[event.Sequence]++
	}
	for _, event := range live {
		seen[event.Sequence]++
	}
	for sequence, count := range seen {
		if count != 1 {
			t.Fatalf("sequence %d delivered %d times (replay=%#v live=%#v)", sequence, count, result.Events, live)
		}
	}
	foundTerminal := false
	for _, event := range live {
		if event.Type == "run_interrupted" {
			foundTerminal = true
		}
	}
	if !foundTerminal {
		t.Fatalf("live events never included run_interrupted: %#v", live)
	}
}

func TestSubscribeOverflowGapsInsteadOfBlockingTheEngine(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"interactive","idempotencyKey":"sub-gap"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waitForStatus(t, root, started.RunID, "pending_manual")

	entry := addSubscriberForTest(manager, started.RunID)
	// Fill the bounded buffer without ever draining it (no forwarder
	// started), then push one more event past capacity: it must trigger a
	// gap instead of blocking the caller.
	for i := 0; i < subscriberEventBuffer; i++ {
		entry.deliver(proto.RunEventPayload{RunID: started.RunID, Sequence: i, Type: "run_interrupted"})
	}
	done := make(chan struct{})
	go func() {
		entry.deliver(proto.RunEventPayload{RunID: started.RunID, Sequence: subscriberEventBuffer, Type: "run_interrupted"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("deliver blocked the caller instead of gapping")
	}
	deadline := time.After(2 * time.Second)
	for {
		entry.mu.Lock()
		gapped := entry.stopped
		entry.mu.Unlock()
		if gapped {
			break
		}
		select {
		case <-deadline:
			t.Fatal("subscriber was never marked stopped after overflow")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func addSubscriberForTest(m *Manager, runID string) *runSubscriber {
	sub := newFakeSubscriber()
	entry := m.addSubscriber(runID, sub)
	entry.setReplayBoundary(-1)
	return entry
}

func TestSubscribeToUnknownRunReturnsRunNotFound(t *testing.T) {
	service, _ := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	sub := newFakeSubscriber()
	if _, rpcErr := manager.Subscribe("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", 0, sub); rpcErr == nil || rpcErr.Code != proto.CodeRunNotFound {
		t.Fatalf("Subscribe unknown run = %+v, want -32006", rpcErr)
	}
}

func TestSubscribeFromSequenceSkipsEarlierReplay(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"interactive","idempotencyKey":"sub-seq"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waitForStatus(t, root, started.RunID, "pending_manual")

	sub := newFakeSubscriber()
	result, rpcErr := manager.Subscribe(started.RunID, 1, sub)
	if rpcErr != nil {
		t.Fatalf("Subscribe: %+v", rpcErr)
	}
	for _, event := range result.Events {
		if event.Sequence < 1 {
			t.Fatalf("replay included sequence below fromSequence: %#v", event)
		}
	}
	if result.Status != "pendingManual" {
		t.Fatalf("status = %q, want pendingManual", result.Status)
	}
}
