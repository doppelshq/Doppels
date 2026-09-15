package runs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
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
	flushes := b.drainPendingLocked(stepID)
	var ended []*liveLogSubscriber
	for entry := range b.subscribers {
		if entry.stepID == stepID {
			delete(b.subscribers, entry)
			ended = append(ended, entry)
		}
	}
	subscribers := make([]*liveLogSubscriber, 0, len(b.subscribers))
	for entry := range b.subscribers {
		if entry.stepID == "" || entry.stepID == stepID {
			subscribers = append(subscribers, entry)
		}
	}
	b.mu.Unlock()

	for _, flush := range flushes {
		for _, entry := range subscribers {
			if !entry.sub.DeliverRunLog(flush) {
				b.removeSubscriber(entry)
			}
		}
	}
	for _, entry := range ended {
		for _, flush := range flushes {
			entry.sub.DeliverRunLog(flush)
		}
		entry.stop()
	}
}

// drainPendingLocked flushes any buffered incomplete UTF-8 suffix for streams
// belonging to stepID so the live wire keeps byte-for-byte parity with the
// on-disk log once the step can no longer receive further writes. Must be
// called with b.mu held.
func (b *liveLogBroadcaster) drainPendingLocked(stepID string) []proto.RunLogChunk {
	var flushes []proto.RunLogChunk
	for key, state := range b.streams {
		if len(state.pending) == 0 || state.truncated {
			continue
		}
		keyStepID, stream, ok := splitStreamKey(key)
		if !ok || keyStepID != stepID {
			continue
		}
		flushes = append(flushes, splitDrainFrames(b.runID, stepID, stream, state.pending)...)
		state.emitted += len(state.pending)
		state.pending = nil
	}
	return flushes
}

// splitDrainFrames emits a final truncated frame from bytes that the step
// emitted but never completed. Unlike splitRunLogFrames it does NOT retreat
// to a UTF-8 rune boundary: the disk log retains these bytes verbatim, so
// the live wire must too even when they are an incomplete or invalid UTF-8
// suffix. The frame is still bounded to MaxFrameBytes; larger buffers are
// split into multiple truncated frames.
func splitDrainFrames(runID, stepID, stream string, data []byte) []proto.RunLogChunk {
	if len(data) == 0 {
		return nil
	}
	// envelopeOverhead is the size the JSON-RPC envelope + RunLogChunk
	// scaffolding add on top of Data. Measure with an empty Data so the
	// largestFittingLogPrefix binary search below has a hard upper bound on
	// how many bytes can fit; using len(encoded) directly per probe would
	// need a json.Marshal per step, which is fine but the binary search
	// only narrows by 1 byte at a time on miss and can stall when every
	// candidate exceeds the limit (see test regression that motivated this
	// comment).
	overheadProbe := proto.RunLogChunk{RunID: runID, StepID: stepID, Stream: stream, Data: ""}
	overhead, err := json.Marshal(proto.NewNotification("v1/runLog", overheadProbe))
	if err != nil {
		return nil
	}
	budget := proto.MaxFrameBytes - len(overhead)
	if budget <= 0 {
		return nil
	}

	var result []proto.RunLogChunk
	for len(data) > 0 {
		take := len(data)
		if take > budget {
			take = budget
		}
		result = append(result, proto.RunLogChunk{
			RunID: runID, StepID: stepID, Stream: stream, Data: string(data[:take]),
		})
		data = data[take:]
	}
	result[len(result)-1].Truncated = true
	return result
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

func splitStreamKey(key string) (stepID, stream string, ok bool) {
	stepID, stream, found := strings.Cut(key, ":")
	return stepID, stream, found
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
	flushes := b.drainAllPendingLocked()
	entries := make([]*liveLogSubscriber, 0, len(b.subscribers))
	for entry := range b.subscribers {
		entries = append(entries, entry)
	}
	b.subscribers = make(map[*liveLogSubscriber]struct{})
	b.mu.Unlock()
	for _, entry := range entries {
		for _, flush := range flushes {
			entry.sub.DeliverRunLog(flush)
		}
		entry.stop()
	}
}

// drainAllPendingLocked flushes every stream's buffered incomplete UTF-8
// suffix when the run itself ends. Must be called with b.mu held.
func (b *liveLogBroadcaster) drainAllPendingLocked() []proto.RunLogChunk {
	var flushes []proto.RunLogChunk
	for key, state := range b.streams {
		if len(state.pending) == 0 || state.truncated {
			continue
		}
		stepID, stream, ok := splitStreamKey(key)
		if !ok {
			continue
		}
		flushes = append(flushes, splitDrainFrames(b.runID, stepID, stream, state.pending)...)
		state.emitted += len(state.pending)
		state.pending = nil
	}
	return flushes
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
