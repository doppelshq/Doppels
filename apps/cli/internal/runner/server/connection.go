package server

import (
	"encoding/json"
	"net"
	"sync"
	"time"

	"doppels.so/cli/internal/runner/proto"
)

// outboundBufferSize bounds queued notifications per subscriber (RFC §10:
// 1024 events). Overflowing subscribers drop frames instead of stalling the
// server; run-event streams add explicit gap signalling on top.
const outboundBufferSize = 1024

type connection struct {
	server   *Server
	conn     net.Conn
	outbound chan any
	stop     chan struct{}

	mu                sync.Mutex
	initialized       bool
	clientName        string
	nodeSubscribed    bool
	nodeSubscribing   bool
	pendingEvents     []proto.NodeEvent
	closed            bool
	pendingActivation func()
	pendingAbort      func()
	onClose           map[int]func()
	nextOnCloseID     int
}

type outboundFrame struct {
	value any
	after func()
}

func newConnection(server *Server, conn net.Conn) *connection {
	return &connection{
		server:   server,
		conn:     conn,
		outbound: make(chan any, outboundBufferSize),
		stop:     make(chan struct{}),
	}
}

func (c *connection) serve() {
	defer c.server.removeConnection(c)

	writerDone := make(chan struct{})
	go c.writeLoop(writerDone)
	defer func() {
		c.close()
		<-writerDone
	}()

	// A connection must handshake within the timeout or gets closed (§6).
	_ = c.conn.SetReadDeadline(time.Now().Add(c.server.config.HandshakeTimeout))

	decoder := proto.NewDecoder(c.conn)
	for {
		// Frames already buffered by the reader must not be executed once the
		// connection is gone: a client that pipelines shutdown plus a Run
		// request would otherwise have the Run started by a dead connection.
		if c.isClosed() {
			return
		}
		frame, err := decoder.ReadFrame()
		if err != nil {
			return
		}
		message, protoErr := proto.DecodeMessage(frame)
		if protoErr != nil {
			// DecodeMessage returns a non-nil message (carrying the
			// already-validated id) for every error where that id can
			// still be trusted enough to correlate — only a frame whose id
			// itself couldn't be determined or validated answers with id
			// null (§15: only framing corruption forces a close; the
			// connection otherwise stays alive either way).
			var id any
			if message != nil {
				id = message.ID
			}
			c.enqueueError(id, protoErr, true)
			continue
		}
		if message.IsNotification() {
			// v1 defines no client→server notifications; ignore.
			continue
		}
		if !c.isInitialized() && message.Method != "v1/initialize" {
			c.enqueueError(message.ID, &proto.Error{
				Code:    proto.CodeNotInitialized,
				Message: "initialize is required before any other method",
			}, true)
			continue
		}
		if c.isClosed() {
			return
		}
		c.server.dispatch(c, message)
		if message.Method == "v1/initialize" && c.isInitialized() {
			_ = c.conn.SetReadDeadline(time.Time{})
		}
	}
}

func (c *connection) writeLoop(done chan struct{}) {
	defer close(done)
	encoder := proto.NewEncoder(c.conn)
	for {
		select {
		case frame := <-c.outbound:
			value := frame
			after := func() {}
			if deferred, ok := frame.(outboundFrame); ok {
				value, after = deferred.value, deferred.after
				if after == nil {
					after = func() {}
				}
			}
			if err := encoder.WriteFrame(value); err != nil {
				c.server.config.Log("runner client writer failed: %v", err)
				c.close()
				// The ack is evidence for the client, not a precondition of
				// the shutdown itself: run the callback anyway or the server
				// stays draining forever.
				after()
				c.drainAfters()
				return
			}
			after()
		case <-c.stop:
			c.drainAfters()
			return
		}
	}
}

// drainAfters runs the deferred callbacks of frames that will never be
// written. Only shutdown carries one today; losing it would leave the server
// refusing operations while never stopping.
func (c *connection) drainAfters() {
	for {
		select {
		case frame := <-c.outbound:
			if deferred, ok := frame.(outboundFrame); ok && deferred.after != nil {
				deferred.after()
			}
		default:
			return
		}
	}
}

// enqueue queues a frame for the writer goroutine. Required frames are RPC
// responses the client is waiting on: if the outbound queue is saturated,
// dropping them would leave the client hanging until the read deadline, so
// we close the connection instead (RFC §15: clients must reconnect after
// framing issues). Best-effort frames (notifications) drop with a log.
func (c *connection) enqueue(value any, required bool) bool {
	return c.enqueueFrame(outboundFrame{value: value}, required)
}

// oversizedErrorMessage is the fixed fallback text enqueueError uses once
// neither the full error nor a Data-stripped one fits: short enough that,
// combined with id (bounded at parse time, MaxIDBytes) and any Code, the
// resulting envelope always fits MaxFrameBytes.
const oversizedErrorMessage = "error diagnostics exceed maximum frame size"

// enqueueError builds a JSON-RPC error response and, unlike a bare
// conn.enqueue(proto.NewErrorResponse(...)), actually verifies it fits
// MaxFrameBytes before queuing it — the same guarantee dispatch already
// gives every success response. id is bounded at parse time (DecodeMessage,
// MaxIDBytes), so it alone can never be the reason an envelope doesn't fit;
// either err.Data (arbitrary handler-supplied diagnostics) or err.Message
// (which is not always a short fixed string — e.g. a "workspace not found"
// error echoes the raw, unbounded request parameter) can be. If the full
// error doesn't fit, retry with Data dropped; if that still doesn't fit,
// replace Message with a fixed, bounded diagnostic too. id is preserved at
// every step: it was already validated and already fits, so there is never
// a reason to fall back to id null here (that fallback is reserved for
// DecodeMessage's own "the id itself couldn't be trusted" case).
func (c *connection) enqueueError(id any, err *proto.Error, required bool) bool {
	if fits(id, err) {
		return c.enqueue(proto.NewErrorResponse(id, err), required)
	}
	if err.Data != nil {
		trimmed := &proto.Error{Code: err.Code, Message: err.Message}
		if fits(id, trimmed) {
			return c.enqueue(proto.NewErrorResponse(id, trimmed), required)
		}
		err = trimmed
	}
	fallback := &proto.Error{Code: err.Code, Message: oversizedErrorMessage}
	return c.enqueue(proto.NewErrorResponse(id, fallback), required)
}

func fits(id any, err *proto.Error) bool {
	encoded, marshalErr := json.Marshal(proto.NewErrorResponse(id, err))
	return marshalErr == nil && len(encoded) <= proto.MaxFrameBytes
}

func (c *connection) enqueueFrame(frame outboundFrame, required bool) bool {
	select {
	case c.outbound <- frame:
		return true
	default:
		if required {
			c.server.config.Log("outbound saturated; closing slow client %q", c.name())
			c.close()
			return false
		}
		c.server.config.Log("dropping outbound frame for slow client %q", c.name())
		return false
	}
}

func (c *connection) initialize(clientName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initialized = true
	if clientName != "" {
		c.clientName = clientName
	}
}

// name returns the client name for diagnostics. Read under the lock: the
// overflow path logs from whichever goroutine is emitting while the reader
// may still be running the handshake.
func (c *connection) name() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clientName
}

func (c *connection) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *connection) isInitialized() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initialized
}

// beginNodeSubscription opens the window around the snapshot: events raised
// while subscribeNode computes the node status are buffered rather than
// dropped (they predate the subscription) or queued ahead of the response
// (they would break the snapshot-first ordering of RFC §10).
func (c *connection) beginNodeSubscription() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nodeSubscribed {
		return
	}
	c.nodeSubscribing = true
	c.pendingEvents = nil
}

// flushNodeSubscription activates the subscription once the snapshot
// response is already in the outbound FIFO, then queues whatever arrived in
// between. An event overlapping the snapshot may be delivered twice; it is
// never lost.
func (c *connection) flushNodeSubscription() {
	c.mu.Lock()
	pending := c.pendingEvents
	c.pendingEvents = nil
	c.nodeSubscribing = false
	c.nodeSubscribed = true
	c.mu.Unlock()
	for _, event := range pending {
		c.enqueue(proto.NewNotification("v1/nodeEvent", event), false)
	}
}

// cancelNodeSubscription undoes beginNodeSubscription when the snapshot
// response never made it into the queue.
func (c *connection) cancelNodeSubscription() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodeSubscribing = false
	c.pendingEvents = nil
}

// sendNodeEvent queues a v1/nodeEvent notification when subscribed.
func (c *connection) sendNodeEvent(event proto.NodeEvent) {
	c.mu.Lock()
	if c.nodeSubscribing {
		// Bounded like the outbound queue: a subscriber whose snapshot never
		// completes must not grow the buffer without limit.
		if len(c.pendingEvents) < outboundBufferSize {
			c.pendingEvents = append(c.pendingEvents, event)
		}
		c.mu.Unlock()
		return
	}
	subscribed := c.nodeSubscribed
	c.mu.Unlock()
	if !subscribed {
		return
	}
	c.enqueue(proto.NewNotification("v1/nodeEvent", event), false)
}

// DeliverRunEvent sends a v1/runEvent notification targeted at this
// connection only. Domain callers (e.g. runs.Manager) push events straight to
// a specific subscriber instead of the server-wide broadcast EmitNodeEvent
// uses, since a Run subscription is per-connection, not per-server.
//
// It reports whether the frame was actually queued. A saturated outbound
// queue drops the frame (best-effort, like any notification) but the caller
// must not treat that as success: a dropped event is a gap, and the caller
// is responsible for reacting (see DeliverRunGap) — silently losing an event
// with no signal at all would violate the no-gap contract.
func (c *connection) DeliverRunEvent(event proto.RunEventPayload) bool {
	return c.enqueue(proto.NewNotification("v1/runEvent", event), false)
}

// DeliverRunGap tells this connection's subscriber that it fell behind and
// must resynchronize (RFC §10): the Runner stops emitting that Run to it.
// This is the client's only signal to resync, so it is never a silent,
// best-effort drop: a saturated outbound queue closes the connection
// instead, which is itself an unambiguous signal to reconnect and
// re-subscribe (RFC §10: "Desconexión = unsubscribe implícito ... Reconexión
// = nuevo initialize + re-subscripciones").
func (c *connection) DeliverRunGap(runID string, fromSequence int) {
	c.enqueue(proto.NewNotification("v1/nodeEvent", proto.NodeEvent{
		Kind:    proto.NodeEventRunEventGap,
		Payload: map[string]any{"runId": runID, "fromSequence": fromSequence},
	}), true)
}

// Defer implements RunEventSubscriber: onActivate runs once the response to
// the in-flight RPC call has been enqueued (see dispatch's
// consumePendingActivation), never before and never if that response could
// not be enqueued at all. onAbort runs instead, exactly once, whenever
// onActivate would not: a saturated outbound queue, a handler error, or —
// notably — a successful handler whose response still turned out to exceed
// MaxFrameBytes once encoded (dispatch discovers this only after the
// handler, and Subscribe's own bookkeeping registration already happened
// by then). Without onAbort a caller like runs.Manager, which registers a
// subscriber list entry and a connection onClose callback before deferring
// activation, would leak both for a subscription whose activation never
// ran and therefore never has another chance to clean up. At most one
// activation is ever pending: methods run on a connection's single reading
// goroutine, so dispatch always consumes it before the next call.
func (c *connection) Defer(onActivate func(), onAbort func()) {
	c.mu.Lock()
	c.pendingActivation = onActivate
	c.pendingAbort = onAbort
	c.mu.Unlock()
}

// consumePendingActivation clears whatever Defer registered during the
// current dispatch and runs onActivate if succeeded, else onAbort — exactly
// one of the two, exactly once.
func (c *connection) consumePendingActivation(succeeded bool) {
	c.mu.Lock()
	activate := c.pendingActivation
	abort := c.pendingAbort
	c.pendingActivation = nil
	c.pendingAbort = nil
	c.mu.Unlock()
	if succeeded {
		if activate != nil {
			activate()
		}
		return
	}
	if abort != nil {
		abort()
	}
}

// NotifyClosed implements RunEventSubscriber: fn runs when this connection
// closes, or immediately if it already has. Domain subscribers (e.g.
// runs.Manager) use this to unsubscribe on disconnect instead of leaking a
// subscription (and its forwarder goroutine) forever. The returned func
// unregisters fn if the subscription ends on its own (Run terminal, gap,
// error) before the connection ever closes — without it, a long-lived
// connection subscribing to many Runs over its lifetime would accumulate one
// stale callback per past subscription forever. Unregistering is a no-op
// once the connection has already closed or already unregistered.
func (c *connection) NotifyClosed(fn func()) func() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		fn()
		return func() {}
	}
	if c.onClose == nil {
		c.onClose = make(map[int]func())
	}
	id := c.nextOnCloseID
	c.nextOnCloseID++
	c.onClose[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.onClose, id)
		c.mu.Unlock()
	}
}

func (c *connection) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	callbacks := c.onClose
	c.onClose = nil
	c.mu.Unlock()
	close(c.stop)
	_ = c.conn.Close()
	for _, fn := range callbacks {
		fn()
	}
}

func (s *Server) removeConnection(conn *connection) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
	conn.close()
}

func (s *Server) dispatch(conn *connection, message *proto.Message) {
	s.mu.Lock()
	handler, found := s.handlers[message.Method]
	s.mu.Unlock()
	if !found {
		conn.enqueueError(message.ID, &proto.Error{
			Code:    proto.CodeMethodNotFound,
			Message: "unknown method: " + message.Method,
		}, true)
		return
	}
	if message.Method != "v1/shutdown" && message.Method != "v1/initialize" && s.isShuttingDown() {
		conn.enqueueError(message.ID, &proto.Error{Code: proto.CodeBusy, Message: "runner is shutting down"}, true)
		return
	}
	result, protoErr := handler(conn, message.Params)
	if protoErr != nil {
		// A handler that errored never reaches the success path below, so
		// any Defer it may have registered (it shouldn't, but defensively)
		// must not fire for a response that was never sent.
		conn.consumePendingActivation(false)
		conn.enqueueError(message.ID, protoErr, true)
		return
	}
	if message.Method == "v1/shutdown" {
		conn.consumePendingActivation(false)
		after := s.shutdownAfterAckCallback()
		if !conn.enqueueFrame(outboundFrame{value: proto.NewResponse(message.ID, result), after: after}, true) && after != nil {
			after()
		}
		return
	}
	if fitter, ok := result.(ResponseFrameFitter); ok {
		if fitErr := fitter.FitResponseFrame(message.ID, proto.MaxFrameBytes); fitErr != nil {
			conn.consumePendingActivation(false)
			conn.enqueueError(message.ID, fitErr, true)
			return
		}
	}
	response := proto.NewResponse(message.ID, result)
	encoded, encodeErr := json.Marshal(response)
	if encodeErr != nil || len(encoded) > proto.MaxFrameBytes {
		conn.consumePendingActivation(false)
		conn.enqueueError(message.ID, &proto.Error{Code: proto.CodeInternal, Message: "response exceeds maximum frame size"}, true)
		return
	}
	queued := conn.enqueue(response, true)
	// Runs any RunEventSubscriber.Defer callback the handler registered
	// (e.g. v1/subscribeRun activating live delivery) only now that the
	// response is ahead of it in the outbound FIFO — and never at all if the
	// response could not be enqueued.
	conn.consumePendingActivation(queued)
	if message.Method == "v1/subscribeNode" {
		// Activate only after the snapshot is in the connection FIFO. A
		// concurrent event can then never be queued ahead of that response,
		// and the events buffered during the snapshot follow it.
		if queued {
			conn.flushNodeSubscription()
		} else {
			conn.cancelNodeSubscription()
		}
	}
}

// unmarshalParams decodes params ignoring unknown fields (RFC §5).
func unmarshalParams(params []byte, out any) error {
	return json.Unmarshal(params, out)
}
