package runs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/server"
	"doppels.so/cli/internal/runner/transport"
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

func (f *fakeSubscriber) DeliverRunEvent(event proto.RunEventPayload) bool {
	f.mu.Lock()
	f.events = append(f.events, event)
	f.mu.Unlock()
	f.notify <- struct{}{}
	return true
}

func (f *fakeSubscriber) DeliverRunGap(runID string, fromSequence int) {
	f.mu.Lock()
	f.gaps = append(f.gaps, fromSequence)
	f.mu.Unlock()
	f.notify <- struct{}{}
}

func (f *fakeSubscriber) Defer(fn func())     { fn() }
func (f *fakeSubscriber) NotifyClosed(func()) func() { return func() {} }

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

// TestRunEventGapIsLastNotificationForRunOverRealSocket reproduces the
// remaining finding-5 ordering bug with the real JSON-RPC transport. Once a
// gap is visible on the wire, no event from the discarded backlog may follow
// it for that Run, and fromSequence must name the first discarded event.
func TestRunEventGapIsLastNotificationForRunOverRealSocket(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	forwardStarted := make(chan struct{})
	releaseForward := make(chan struct{})
	forwardDone := make(chan struct{})
	manager.testBeforeForward = func() {
		close(forwardStarted)
		<-releaseForward
	}
	manager.testAfterForward = func() { close(forwardDone) }

	srv := server.New(server.Config{
		Token: integrationToken, RunnerVersion: "0.0.1-test",
		NodeStatus: func() proto.NodeStatus { return proto.NodeStatus{} },
		Log:        func(string, ...any) {},
	})
	RegisterRPC(srv, manager)
	socketPath := filepath.Join(t.TempDir(), "runner.sock")
	listener, err := transport.Unix{}.Listen(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	go srv.Serve(serveCtx, listener)
	t.Cleanup(srv.Close)

	client := dialIntegrationClient(t, socketPath)
	startResponse := client.call("start-gap", "v1/startRun", map[string]any{
		"workspace": root, "capability": "greet", "inputs": map[string]any{"count": 1},
		"approvalMode": "interactive", "idempotencyKey": "wire-gap-order",
	})
	if startResponse.Err != nil {
		t.Fatalf("startRun: %+v", startResponse.Err)
	}
	var started StartResult
	if err := json.Unmarshal(rawResult(t, startResponse), &started); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, root, started.RunID, "pending_manual")

	subResponse := client.call("sub-gap", "v1/subscribeRun", map[string]any{"runId": started.RunID})
	if subResponse.Err != nil {
		t.Fatalf("subscribeRun: %+v", subResponse.Err)
	}
	var snapshot SubscribeResult
	if err := json.Unmarshal(rawResult(t, subResponse), &snapshot); err != nil {
		t.Fatal(err)
	}
	<-forwardStarted
	firstLive := len(snapshot.Events)
	for sequence := firstLive; sequence < firstLive+subscriberEventBuffer+1; sequence++ {
		manager.broadcast(started.RunID, proto.RunEventPayload{
			RunID: started.RunID, Sequence: sequence, Type: "step_started",
		})
	}

	method, params := client.readNotification(t)
	if method != "v1/nodeEvent" {
		t.Fatalf("first post-overflow notification = %q, want v1/nodeEvent gap", method)
	}
	var gap proto.NodeEvent
	if err := json.Unmarshal(params, &gap); err != nil {
		t.Fatal(err)
	}
	close(releaseForward)
	<-forwardDone
	if gap.Kind != proto.NodeEventRunEventGap {
		t.Fatalf("node event kind = %q, want %q", gap.Kind, proto.NodeEventRunEventGap)
	}
	rawPayload, err := json.Marshal(gap.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var gapPayload struct {
		RunID        string `json:"runId"`
		FromSequence int    `json:"fromSequence"`
	}
	if err := json.Unmarshal(rawPayload, &gapPayload); err != nil {
		t.Fatal(err)
	}
	if gapPayload.RunID != started.RunID || gapPayload.FromSequence != firstLive {
		t.Fatalf("gap payload = %#v, want runId %q fromSequence %d", gapPayload, started.RunID, firstLive)
	}

	// The ping response is a deterministic FIFO fence: once observed, every
	// frame the completed forwarder could enqueue has already been read.
	if err := client.encoder.WriteFrame(map[string]any{"jsonrpc": "2.0", "id": "gap-fence", "method": "v1/ping", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	for {
		frame, err := client.decoder.ReadFrame()
		if err != nil {
			t.Fatalf("read post-gap frame: %v", err)
		}
		var envelope struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Method == "v1/runEvent" {
			var event proto.RunEventPayload
			if err := json.Unmarshal(envelope.Params, &event); err != nil {
				t.Fatal(err)
			}
			if event.RunID == started.RunID {
				t.Fatalf("runEvent sequence %d appeared after runEventGap for Run %s", event.Sequence, event.RunID)
			}
		}
		if envelope.ID == "gap-fence" {
			break
		}
	}
}

// rejectingSubscriber's DeliverRunEvent reports failure (simulating a
// saturated connection outbound queue) starting from a configured sequence.
type rejectingSubscriber struct {
	mu        sync.Mutex
	rejectAt  int
	delivered []proto.RunEventPayload
	gaps      []int
	notify    chan struct{}
}

func newRejectingSubscriber(rejectAt int) *rejectingSubscriber {
	return &rejectingSubscriber{rejectAt: rejectAt, notify: make(chan struct{}, 256)}
}

func (r *rejectingSubscriber) DeliverRunEvent(event proto.RunEventPayload) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if event.Sequence >= r.rejectAt {
		return false
	}
	r.delivered = append(r.delivered, event)
	r.notify <- struct{}{}
	return true
}

func (r *rejectingSubscriber) DeliverRunGap(_ string, fromSequence int) {
	r.mu.Lock()
	r.gaps = append(r.gaps, fromSequence)
	r.mu.Unlock()
	r.notify <- struct{}{}
}

func (r *rejectingSubscriber) Defer(fn func())     { fn() }
func (r *rejectingSubscriber) NotifyClosed(func()) func() { return func() {} }

func (r *rejectingSubscriber) waitForGap(t *testing.T) int {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		r.mu.Lock()
		if len(r.gaps) > 0 {
			gap := r.gaps[0]
			r.mu.Unlock()
			return gap
		}
		r.mu.Unlock()
		select {
		case <-r.notify:
		case <-deadline:
			t.Fatal("timed out waiting for a gap notification")
		}
	}
}

// TestForwardTreatsAFailedDeliveryAsAGapAndStopsForwarding reproduces review
// finding 3's runs-side half: the forwarder ignored DeliverRunEvent's
// success/failure, so a connection that silently dropped a frame (saturated
// outbound queue) left the Manager believing delivery succeeded — the
// client would then see a gap in sequence with no runEventGap to explain it.
func TestForwardTreatsAFailedDeliveryAsAGapAndStopsForwarding(t *testing.T) {
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
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"forward-gap"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}

	// Reject every live event from sequence 0 onward: the replay snapshot
	// (read synchronously below) already covers sequence 0, so this
	// exercises exactly the live path.
	sub := newRejectingSubscriber(0)
	result, rpcErr := manager.Subscribe(started.RunID, 0, sub)
	if rpcErr != nil {
		t.Fatalf("Subscribe: %+v", rpcErr)
	}
	if len(result.Events) == 0 {
		t.Fatal("replay snapshot was empty")
	}

	waitForFile(t, filepath.Join(root, "started"))
	if _, rpcErr := manager.Cancel(started.RunID, ""); rpcErr != nil {
		t.Fatalf("Cancel: %+v", rpcErr)
	}

	gapSequence := sub.waitForGap(t)
	if gapSequence != len(result.Events) {
		t.Fatalf("gap fromSequence = %d, want %d (first live sequence)", gapSequence, len(result.Events))
	}

	sub.mu.Lock()
	delivered := len(sub.delivered)
	sub.mu.Unlock()
	if delivered != 0 {
		t.Fatalf("forwarder delivered %d events after the first rejected send, want 0", delivered)
	}
}

// captureDeferSubscriber records whatever RunEventSubscriber.Defer hands it
// instead of running it immediately, so a test can control exactly when
// live delivery activates.
type captureDeferSubscriber struct {
	mu           sync.Mutex
	deferred     func()
	events       []proto.RunEventPayload
	notify       chan struct{}
	closedBy     func()
	unregistered atomic.Int32
}

func newCaptureDeferSubscriber() *captureDeferSubscriber {
	return &captureDeferSubscriber{notify: make(chan struct{}, 256)}
}

func (c *captureDeferSubscriber) DeliverRunEvent(event proto.RunEventPayload) bool {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
	c.notify <- struct{}{}
	return true
}

func (c *captureDeferSubscriber) DeliverRunGap(string, int) {}

func (c *captureDeferSubscriber) Defer(fn func()) {
	c.mu.Lock()
	c.deferred = fn
	c.mu.Unlock()
}

func (c *captureDeferSubscriber) NotifyClosed(fn func()) func() {
	c.mu.Lock()
	c.closedBy = fn
	c.mu.Unlock()
	return func() { c.unregistered.Add(1) }
}

func (c *captureDeferSubscriber) takeDeferred() func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deferred
}

// TestSubscribeDefersLiveForwardingViaSubDefer reproduces review finding 4's
// first half at the runs-package level: Manager.Subscribe must hand off
// activation of live delivery through sub.Defer instead of starting the
// forwarder immediately. Starting it immediately risks a live event racing
// the RPC response itself onto a connection's outbound queue, breaking the
// "replay first, then live" ordering guarantee (proved at the wire level in
// server_test.go; this test proves the Manager-side contract).
func TestSubscribeDefersLiveForwardingViaSubDefer(t *testing.T) {
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
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"defer-activation"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}

	sub := newCaptureDeferSubscriber()
	if _, rpcErr := manager.Subscribe(started.RunID, 0, sub); rpcErr != nil {
		t.Fatalf("Subscribe: %+v", rpcErr)
	}
	deferred := sub.takeDeferred()
	if deferred == nil {
		t.Fatal("Manager.Subscribe did not hand off activation via sub.Defer — live delivery could start before the RPC response is enqueued")
	}

	waitForFile(t, filepath.Join(root, "started"))
	if _, rpcErr := manager.Cancel(started.RunID, ""); rpcErr != nil {
		t.Fatalf("Cancel: %+v", rpcErr)
	}

	select {
	case <-sub.notify:
		t.Fatal("an event was delivered before the deferred activation ran")
	case <-time.After(200 * time.Millisecond):
	}

	deferred()

	select {
	case <-sub.notify:
	case <-time.After(5 * time.Second):
		t.Fatal("no event delivered after activation ran")
	}
}

// TestSubscribeUnsubscribesOnConnectionClose reproduces review finding 4's
// second half: Manager.Subscribe must register cleanup via sub.NotifyClosed
// so a disconnected client's subscription (and its forwarder goroutine)
// does not leak forever.
func TestSubscribeUnsubscribesOnConnectionClose(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"interactive","idempotencyKey":"unsub-close"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waitForStatus(t, root, started.RunID, "pending_manual")

	sub := newCaptureDeferSubscriber()
	if _, rpcErr := manager.Subscribe(started.RunID, 0, sub); rpcErr != nil {
		t.Fatalf("Subscribe: %+v", rpcErr)
	}

	manager.subsMu.Lock()
	before := len(manager.subs[started.RunID])
	manager.subsMu.Unlock()
	if before == 0 {
		t.Fatal("subscriber was never registered")
	}

	sub.mu.Lock()
	closedBy := sub.closedBy
	sub.mu.Unlock()
	if closedBy == nil {
		t.Fatal("Manager.Subscribe did not register cleanup via sub.NotifyClosed")
	}
	closedBy() // simulate the underlying connection disconnecting

	manager.subsMu.Lock()
	after := len(manager.subs[started.RunID])
	manager.subsMu.Unlock()
	if after != 0 {
		t.Fatalf("subscriber list for %s still has %d entries after disconnect, want 0", started.RunID, after)
	}
}

// TestByteBudgetTracksBacklogNotLifetimeTotal reproduces review finding 5:
// deliver() added to e.bytes on every send but forward() never subtracted
// after dequeuing, so the "4 MiB per subscriber" budget (RFC §10) measured
// cumulative lifetime traffic instead of actual pending backlog. A
// subscriber drained promptly must never be gapped merely because it has
// received more than 4 MiB in total over its lifetime.
func TestByteBudgetTracksBacklogNotLifetimeTotal(t *testing.T) {
	m := &Manager{subs: make(map[string][]*runSubscriber)}
	sub := newFakeSubscriber()
	entry := m.addSubscriber("run-x", sub)
	entry.setReplayBoundary(-1)
	entry.start()

	payload := make([]byte, 64<<10) // 64 KiB per event
	for i := range payload {
		payload[i] = 'x'
	}
	const events = 200 // 200 * 64 KiB = 12.5 MiB total, well over the 4 MiB budget
	for i := 0; i < events; i++ {
		entry.deliver(proto.RunEventPayload{
			RunID: "run-x", Sequence: i, Type: "step_started",
			Data: map[string]any{"blob": string(payload)},
		})
		// Pace to the consumer so the *pending backlog* never approaches the
		// budget: this isolates "cumulative lifetime total" (the bug) from
		// "genuine backlog overflow" (tested separately below), which would
		// otherwise legitimately gap if the producer outran the forwarder.
		<-sub.notify
	}

	delivered := sub.waitForEvents(t, events)
	if len(delivered) != events {
		t.Fatalf("delivered = %d, want %d", len(delivered), events)
	}
	sub.mu.Lock()
	gaps := len(sub.gaps)
	sub.mu.Unlock()
	if gaps != 0 {
		t.Fatalf("subscriber was gapped despite being drained promptly: %d gaps", gaps)
	}
}

// TestByteBudgetStillGapsOnRealBacklogOverflow guards the other side of
// finding 5's fix: an undrained subscriber whose *pending* backlog exceeds
// the byte budget must still be gapped, and must receive nothing delivered
// after the point of overflow.
func TestByteBudgetStillGapsOnRealBacklogOverflow(t *testing.T) {
	m := &Manager{subs: make(map[string][]*runSubscriber)}
	sub := newFakeSubscriber()
	entry := m.addSubscriber("run-x", sub)
	entry.setReplayBoundary(-1)
	// Deliberately never call entry.start(): the backlog must accumulate
	// undrained so the byte budget alone forces a gap.

	payload := make([]byte, 512<<10) // 512 KiB per event; 4 MiB / 512 KiB = 8 events
	for i := range payload {
		payload[i] = 'x'
	}
	for i := 0; i < 20; i++ {
		entry.deliver(proto.RunEventPayload{
			RunID: "run-x", Sequence: i, Type: "step_started",
			Data: map[string]any{"blob": string(payload)},
		})
	}
	entry.mu.Lock()
	stopped := entry.stopped
	entry.mu.Unlock()
	if !stopped {
		t.Fatal("subscriber was never gapped despite a backlog far exceeding the 4 MiB budget")
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

// TestSubscribeDegradesToRunningSnapshotForFreshlyReservedRun reproduces a
// race exposed while stabilizing this suite: startRun durably reserves a Run
// in the index (and hands its id back to the caller) before the engine
// goroutine that writes request.json/run.json has even started. A caller
// that subscribes immediately after startRun returns — exactly what a
// well-behaved client does — could lose that race and get a spurious
// internal error for a Run id it was just validly handed. testStopAfterReserve
// makes the "goroutine never got to write the files" side of that race
// deterministic instead of timing-dependent: it returns right after the
// reservation, before request.json/run.json are ever written.
func TestSubscribeDegradesToRunningSnapshotForFreshlyReservedRun(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()
	manager.testStopAfterReserve = true

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"interactive","idempotencyKey":"subscribe-before-materialize"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}

	sub := newFakeSubscriber()
	result, rpcErr := manager.Subscribe(started.RunID, 0, sub)
	if rpcErr != nil {
		t.Fatalf("Subscribe: %+v", rpcErr)
	}
	if result.Status != "running" {
		t.Fatalf("status = %q, want running", result.Status)
	}
	if len(result.Events) != 0 {
		t.Fatalf("events = %#v, want empty (nothing durable yet)", result.Events)
	}
}
