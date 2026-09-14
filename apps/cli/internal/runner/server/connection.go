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

	mu              sync.Mutex
	initialized     bool
	clientName      string
	nodeSubscribed  bool
	nodeSubscribing bool
	pendingEvents   []proto.NodeEvent
	closed          bool
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
			// Protocol errors answer with id null and keep the connection
			// alive (§15: only framing corruption forces a close).
			c.enqueue(proto.NewErrorResponse(nil, protoErr), true)
			continue
		}
		if message.IsNotification() {
			// v1 defines no client→server notifications; ignore.
			continue
		}
		if !c.isInitialized() && message.Method != "v1/initialize" {
			c.enqueue(proto.NewErrorResponse(message.ID, &proto.Error{
				Code:    proto.CodeNotInitialized,
				Message: "initialize is required before any other method",
			}), true)
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

func (c *connection) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	close(c.stop)
	_ = c.conn.Close()
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
		conn.enqueue(proto.NewErrorResponse(message.ID, &proto.Error{
			Code:    proto.CodeMethodNotFound,
			Message: "unknown method: " + message.Method,
		}), true)
		return
	}
	if message.Method != "v1/shutdown" && message.Method != "v1/initialize" && s.isShuttingDown() {
		conn.enqueue(proto.NewErrorResponse(message.ID, &proto.Error{Code: proto.CodeBusy, Message: "runner is shutting down"}), true)
		return
	}
	result, protoErr := handler(conn, message.Params)
	if protoErr != nil {
		conn.enqueue(proto.NewErrorResponse(message.ID, protoErr), true)
		return
	}
	if message.Method == "v1/shutdown" {
		after := s.shutdownAfterAckCallback()
		if !conn.enqueueFrame(outboundFrame{value: proto.NewResponse(message.ID, result), after: after}, true) && after != nil {
			after()
		}
		return
	}
	queued := conn.enqueue(proto.NewResponse(message.ID, result), true)
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
