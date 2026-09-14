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

	mu             sync.Mutex
	initialized    bool
	clientName     string
	nodeSubscribed bool
	closed         bool
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
				return
			}
			after()
		case <-c.stop:
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
			c.server.config.Log("outbound saturated; closing slow client %q", c.clientName)
			c.close()
			return false
		}
		c.server.config.Log("dropping outbound frame for slow client %q", c.clientName)
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

func (c *connection) isInitialized() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initialized
}

func (c *connection) setNodeSubscribed(subscribed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodeSubscribed = subscribed
}

// sendNodeEvent queues a v1/nodeEvent notification when subscribed.
func (c *connection) sendNodeEvent(event proto.NodeEvent) {
	c.mu.Lock()
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
	if conn.enqueue(proto.NewResponse(message.ID, result), true) && message.Method == "v1/subscribeNode" {
		// Activate only after the snapshot is in the connection FIFO. A
		// concurrent event can then never be queued ahead of that response.
		conn.setNodeSubscribed(true)
	}
}

// unmarshalParams decodes params ignoring unknown fields (RFC §5).
func unmarshalParams(params []byte, out any) error {
	return json.Unmarshal(params, out)
}
