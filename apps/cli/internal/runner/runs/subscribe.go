package runs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/server"
	"doppels.so/cli/internal/runstate"
)

// RFC §10: buffer per subscriber is bounded at 1024 events / 4 MiB. A
// subscriber that would overflow either bound is gapped instead of stalling
// the engine goroutine that is broadcasting the event.
const (
	subscriberEventBuffer = 1024
	subscriberByteBudget  = 4 << 20
)

// runSubscriber fans a single Run's events out to one connection-scoped
// server.RunEventSubscriber. lastReplay is the highest sequence already
// returned by the synchronous subscribeRun snapshot; forward() drops
// anything at or below it so replay and live delivery never overlap or gap.
type runSubscriber struct {
	manager *Manager
	runID   string
	sub     server.RunEventSubscriber
	events  chan proto.RunEventPayload

	mu         sync.Mutex
	lastReplay int
	bytes      int
	stopped    bool
}

// addSubscriber registers entry so broadcast() starts queuing events for it
// immediately, but deliberately does not start its forwarder goroutine yet:
// until setReplayBoundary+start runs, the boundary distinguishing "already in
// the replay batch" from "live" is unknown, and delivering early would risk
// a duplicate against the caller's synchronous replay read.
func (m *Manager) addSubscriber(runID string, sub server.RunEventSubscriber) *runSubscriber {
	entry := &runSubscriber{
		manager: m, runID: runID, sub: sub,
		events:     make(chan proto.RunEventPayload, subscriberEventBuffer),
		lastReplay: -1,
	}
	m.subsMu.Lock()
	m.subs[runID] = append(m.subs[runID], entry)
	m.subsMu.Unlock()
	return entry
}

func (e *runSubscriber) setReplayBoundary(lastSequence int) {
	e.mu.Lock()
	e.lastReplay = lastSequence
	e.mu.Unlock()
}

// start begins forwarding buffered and future events. Must be called only
// after setReplayBoundary so no event is ever evaluated before the replay
// cutoff is known.
func (e *runSubscriber) start() {
	go e.forward()
}

func (e *runSubscriber) forward() {
	for event := range e.events {
		e.mu.Lock()
		skip := event.Sequence <= e.lastReplay
		e.mu.Unlock()
		if skip {
			continue
		}
		// A connection can drop a frame on its own outbound queue
		// independently of this subscriber's byte/count budget (e.g. a
		// stuck client). That is still a gap: report it as one instead of
		// silently treating the drop as a successful delivery, and stop
		// forwarding further events to a subscriber whose connection has
		// already proven it cannot keep up.
		if !e.sub.DeliverRunEvent(event) {
			e.close()
			e.manager.removeSubscriber(e.runID, e)
			e.sub.DeliverRunGap(e.runID, event.Sequence)
			return
		}
	}
}

// deliver enqueues event without ever blocking the broadcasting goroutine: a
// full or over-budget subscriber is gapped and dropped instead.
func (e *runSubscriber) deliver(event proto.RunEventPayload) {
	size := approxEventSize(event)
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	if e.bytes+size > subscriberByteBudget {
		e.gapLocked(event.Sequence)
		return
	}
	select {
	case e.events <- event:
		e.bytes += size
		e.mu.Unlock()
	default:
		e.gapLocked(event.Sequence)
	}
}

// gapLocked must be called with e.mu held. It stops delivery permanently and
// asynchronously notifies the subscriber and unregisters it, since both can
// block or re-enter the manager.
func (e *runSubscriber) gapLocked(fromSequence int) {
	e.stopped = true
	close(e.events)
	e.mu.Unlock()
	go func() {
		e.manager.removeSubscriber(e.runID, e)
		e.sub.DeliverRunGap(e.runID, fromSequence)
	}()
}

func (e *runSubscriber) close() {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	e.stopped = true
	e.mu.Unlock()
	close(e.events)
}

func approxEventSize(event proto.RunEventPayload) int {
	data, _ := json.Marshal(event)
	return len(data)
}

func (m *Manager) removeSubscriber(runID string, target *runSubscriber) {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	list := m.subs[runID]
	for index, candidate := range list {
		if candidate == target {
			m.subs[runID] = append(list[:index:index], list[index+1:]...)
			break
		}
	}
	if len(m.subs[runID]) == 0 {
		delete(m.subs, runID)
	}
}

// broadcast fans a persisted event out to every current subscriber of runID.
// Callers must persist the event first (RFC §10: persistence before
// notification); broadcast itself never touches disk.
func (m *Manager) broadcast(runID string, event proto.RunEventPayload) {
	m.subsMu.Lock()
	list := append([]*runSubscriber(nil), m.subs[runID]...)
	m.subsMu.Unlock()
	for _, entry := range list {
		entry.deliver(event)
	}
	if isTerminalEventType(event.Type) {
		m.closeSubscribers(runID)
	}
}

func (m *Manager) closeSubscribers(runID string) {
	m.subsMu.Lock()
	list := m.subs[runID]
	delete(m.subs, runID)
	m.subsMu.Unlock()
	for _, entry := range list {
		entry.close()
	}
}

func isTerminalEventType(eventType string) bool {
	switch eventType {
	case "run_succeeded", "run_failed", "run_cancelled", "run_interrupted":
		return true
	default:
		return false
	}
}

func payloadFromEvent(event execution.RunEvent) proto.RunEventPayload {
	return proto.RunEventPayload{
		RunID: event.RunID, Sequence: event.Sequence, Type: event.Type,
		StepID: event.StepID, Data: event.Data,
		OccurredAt: event.OccurredAt.UTC().Format(time.RFC3339Nano),
	}
}

// SubscribeResult is the synchronous v1/subscribeRun response: the replay
// batch from fromSequence plus the Run's current status. Live events follow
// as v1/runEvent notifications on the same connection.
type SubscribeResult struct {
	Events []proto.RunEventPayload `json:"events"`
	Status string                  `json:"status"`
}

// Subscribe registers sub for runID's future events, then returns the replay
// batch from fromSequence. Registration happens before the replay read so no
// event can be missed; forward() deduplicates against lastReplay so no event
// already in the replay batch is delivered twice.
func (m *Manager) Subscribe(runID string, fromSequence int, sub server.RunEventSubscriber) (SubscribeResult, *proto.Error) {
	if !safeRunID(runID) {
		return SubscribeResult{}, &proto.Error{Code: proto.CodeRunNotFound, Message: "Run not found"}
	}
	if fromSequence < 0 {
		return SubscribeResult{}, invalidParams("fromSequence must be >= 0")
	}
	root, idx, _, err := m.findRecord(runID)
	if err != nil {
		return SubscribeResult{}, subscribeFindError(err)
	}
	entry := m.addSubscriber(runID, sub)
	detail, err := runstate.LoadWithIndex(root, runID, idx)
	if err != nil {
		entry.close()
		m.removeSubscriber(runID, entry)
		return SubscribeResult{}, internalError(err)
	}
	lastSequence := len(detail.Events) - 1
	entry.setReplayBoundary(lastSequence)
	entry.start()

	events := make([]proto.RunEventPayload, 0, len(detail.Events))
	for _, event := range detail.Events {
		if event.Sequence < fromSequence {
			continue
		}
		events = append(events, payloadFromEvent(event))
	}
	status := wireStatus(detail.Summary.Status)
	if isTerminal(detail.Summary.Status) {
		entry.close()
		m.removeSubscriber(runID, entry)
	}
	return SubscribeResult{Events: events, Status: status}, nil
}

func subscribeFindError(err error) *proto.Error {
	if errors.Is(err, sql.ErrNoRows) {
		return &proto.Error{Code: proto.CodeRunNotFound, Message: "Run not found"}
	}
	return internalError(err)
}
