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
			c.enqueue(proto.NewErrorResponse(nil, protoErr))
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
			}))
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
			if err := encoder.WriteFrame(frame); err != nil {
				return
			}
		case <-c.stop:
			return
		}
	}
}

func (c *connection) enqueue(value any) {
	select {
	case c.outbound <- value:
	default:
		// Subscriber too slow: drop rather than block the dispatch loop.
		c.server.config.Log("dropping outbound frame for slow client %q", c.clientName)
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
	c.enqueue(proto.NewNotification("v1/nodeEvent", event))
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
		}))
		return
	}
	result, protoErr := handler(conn, message.Params)
	if protoErr != nil {
		conn.enqueue(proto.NewErrorResponse(message.ID, protoErr))
		return
	}
	conn.enqueue(proto.NewResponse(message.ID, result))
}

// unmarshalParams decodes params ignoring unknown fields (RFC §5).
func unmarshalParams(params []byte, out any) error {
	return json.Unmarshal(params, out)
}
