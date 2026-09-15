package runs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"unicode/utf8"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/server"
)

type LiveLogSubscription struct {
	Active bool `json:"active"`
}

type liveLogStreamState struct {
	emitted   int
	truncated bool
	pending   []byte
}

type liveLogSubscriber struct {
	stepID string
	sub    server.RunLogSubscriber

	mu               sync.Mutex
	stopped          bool
	unregisterClosed func()
}

type liveLogBroadcaster struct {
	runID string

	mu          sync.Mutex
	stepID      string
	streams     map[string]*liveLogStreamState
	endedSteps  map[string]struct{}
	subscribers map[*liveLogSubscriber]struct{}
	closed      bool
}

func newLiveLogBroadcaster(runID string) *liveLogBroadcaster {
	return &liveLogBroadcaster{
		runID: runID, streams: make(map[string]*liveLogStreamState),
		endedSteps:  make(map[string]struct{}),
		subscribers: make(map[*liveLogSubscriber]struct{}),
	}
}

func (b *liveLogBroadcaster) observeEvent(event execution.RunEvent) {
	switch event.Type {
	case "step_started":
		b.stepStarted(event.StepID)
	case "step_succeeded", "step_failed":
		b.stepEnded(event.StepID)
	case "run_succeeded", "run_failed", "run_cancelled", "run_interrupted":
		b.close()
	}
}

func (b *liveLogBroadcaster) stepStarted(stepID string) {
	b.mu.Lock()
	b.stepID = stepID
	delete(b.endedSteps, stepID)
	b.mu.Unlock()
}

func (b *liveLogBroadcaster) stepEnded(stepID string) {
	b.mu.Lock()
	if b.stepID == stepID {
		b.stepID = ""
	}
	b.endedSteps[stepID] = struct{}{}
	var ended []*liveLogSubscriber
	for entry := range b.subscribers {
		if entry.stepID == stepID {
			delete(b.subscribers, entry)
			ended = append(ended, entry)
		}
	}
	b.mu.Unlock()
	for _, entry := range ended {
		entry.stop()
	}
}

func (b *liveLogBroadcaster) write(stream execution.LogStream, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	b.mu.Lock()
	if b.closed || b.stepID == "" {
		b.mu.Unlock()
		return
	}
	stepID := b.stepID
	stateKey := stepID + ":" + string(stream)
	state := b.streams[stateKey]
	if state == nil {
		state = &liveLogStreamState{}
		b.streams[stateKey] = state
	}
	if state.truncated {
		b.mu.Unlock()
		return
	}
	data := make([]byte, 0, len(state.pending)+len(chunk))
	data = append(data, state.pending...)
	data = append(data, chunk...)
	remaining := execution.DefaultLogStreamLimit - state.emitted
	take := len(data)
	capTruncated := false
	if take > remaining {
		take = remaining
		capTruncated = true
		state.truncated = true
	}
	take = completeUTF8Prefix(data[:take])
	state.emitted += take
	emitted := append([]byte(nil), data[:take]...)
	if capTruncated {
		state.pending = nil
	} else {
		state.pending = append(state.pending[:0], data[take:]...)
	}
	subscribers := make([]*liveLogSubscriber, 0, len(b.subscribers))
	for entry := range b.subscribers {
		if entry.stepID == "" || entry.stepID == stepID {
			subscribers = append(subscribers, entry)
		}
	}
	b.mu.Unlock()

	chunks := splitRunLogFrames(b.runID, stepID, string(stream), emitted, capTruncated)
	for _, entry := range subscribers {
		for _, frame := range chunks {
			if !entry.sub.DeliverRunLog(frame) {
				b.removeSubscriber(entry)
				break
			}
		}
	}
}

func splitRunLogFrames(runID, stepID, stream string, data []byte, capTruncated bool) []proto.RunLogChunk {
	if len(data) == 0 {
		if capTruncated {
			return []proto.RunLogChunk{{RunID: runID, StepID: stepID, Stream: stream, Truncated: true}}
		}
		return nil
	}
	var result []proto.RunLogChunk
	for len(data) > 0 {
		take := largestFittingLogPrefix(runID, stepID, stream, data)
		if take <= 0 {
			return result
		}
		result = append(result, proto.RunLogChunk{
			RunID: runID, StepID: stepID, Stream: stream, Data: string(data[:take]),
		})
		data = data[take:]
	}
	if len(result) > 1 || capTruncated {
		result[len(result)-1].Truncated = true
	}
	return result
}

func largestFittingLogPrefix(runID, stepID, stream string, data []byte) int {
	low, high := 1, len(data)
	best := 0
	for low <= high {
		middle := low + (high-low)/2
		effective := completeUTF8Prefix(data[:middle])
		chunk := proto.RunLogChunk{RunID: runID, StepID: stepID, Stream: stream, Data: string(data[:effective])}
		encoded, err := json.Marshal(proto.NewNotification("v1/runLog", chunk))
		if err == nil && len(encoded) <= proto.MaxFrameBytes {
			if effective > best {
				best = effective
			}
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	return best
}

func completeUTF8Prefix(data []byte) int {
	end := len(data)
	for end > 0 {
		r, size := utf8.DecodeLastRune(data[:end])
		if r != utf8.RuneError || size != 1 {
			return end
		}
		end--
	}
	return 0
}

func (b *liveLogBroadcaster) addSubscriber(stepID string, sub server.RunLogSubscriber) {
	entry := &liveLogSubscriber{
		stepID: stepID, sub: sub,
		unregisterClosed: func() {},
	}
	b.mu.Lock()
	_, stepEnded := b.endedSteps[stepID]
	if b.closed || (stepID != "" && stepEnded) {
		b.mu.Unlock()
		return
	}
	b.subscribers[entry] = struct{}{}
	b.mu.Unlock()
	unregister := sub.NotifyClosed(func() { b.removeSubscriber(entry) })
	entry.installUnregister(unregister)
}

func (b *liveLogBroadcaster) removeSubscriber(entry *liveLogSubscriber) {
	b.mu.Lock()
	_, found := b.subscribers[entry]
	delete(b.subscribers, entry)
	b.mu.Unlock()
	if found {
		entry.stop()
	}
}

func (b *liveLogBroadcaster) close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	entries := make([]*liveLogSubscriber, 0, len(b.subscribers))
	for entry := range b.subscribers {
		entries = append(entries, entry)
	}
	b.subscribers = make(map[*liveLogSubscriber]struct{})
	b.mu.Unlock()
	for _, entry := range entries {
		entry.stop()
	}
}

func (b *liveLogBroadcaster) isActive(stepID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, stepEnded := b.endedSteps[stepID]
	return !b.closed && (stepID == "" || !stepEnded)
}

func (b *liveLogBroadcaster) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

func (e *liveLogSubscriber) installUnregister(unregister func()) {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		unregister()
		return
	}
	e.unregisterClosed = unregister
	e.mu.Unlock()
}

func (e *liveLogSubscriber) stop() {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	e.stopped = true
	unregister := e.unregisterClosed
	e.mu.Unlock()
	unregister()
}

func (m *Manager) SubscribeRunLogs(runID, stepID string, sub server.RunLogSubscriber) (LiveLogSubscription, *proto.Error) {
	if !safeRunID(runID) {
		return LiveLogSubscription{}, &proto.Error{Code: proto.CodeRunNotFound, Message: "Run not found"}
	}
	_, _, _, err := m.findRecord(runID)
	if errors.Is(err, sql.ErrNoRows) {
		return LiveLogSubscription{}, &proto.Error{Code: proto.CodeRunNotFound, Message: "Run not found"}
	}
	if err != nil {
		return LiveLogSubscription{}, internalError(err)
	}
	m.mu.Lock()
	active := m.active[runID]
	m.mu.Unlock()
	if active == nil || active.logs == nil || !active.logs.isActive(stepID) {
		return LiveLogSubscription{Active: false}, nil
	}
	sub.Defer(func() { active.logs.addSubscriber(stepID, sub) }, func() {})
	return LiveLogSubscription{Active: true}, nil
}
