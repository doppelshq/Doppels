// Package server implements the Doppels Runner JSON-RPC dispatch loop
// (RFC 001): handshake enforcement, token auth, subscriptions, and the
// v1 method surface. Domain behavior is injected through Config so the
// server core stays wire-protocol-only.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/transport"
)

// Config wires the domain into the wire protocol.
type Config struct {
	// Token authenticates clients (socket permissions are the other half).
	Token string
	// RunnerVersion is reported by initialize/getNodeStatus.
	RunnerVersion string
	// NodeStatus snapshots the current node state.
	NodeStatus func() proto.NodeStatus
	// Capabilities advertises optional features (RFC §7).
	Capabilities []string
	// OnShutdown runs after the shutdown ack is flushed.
	OnShutdown func()
	// HandshakeTimeout bounds how long a connection may live without
	// initialize (default 10s per RFC §6).
	HandshakeTimeout time.Duration
	// Log receives one-line diagnostics (nil-safe).
	Log func(format string, args ...any)
}

type internalHandler func(conn *connection, params []byte) (any, *proto.Error)

// Handler is the public extension seam for domain methods. Connection
// lifecycle and subscription state stay private to the server package.
type Handler func(params []byte) (any, *proto.Error)

// RunEventSubscriber is the narrow, connection-scoped seam a domain method
// uses to push v1/runEvent notifications (and the runEventGap escape hatch)
// to the one connection that called it. Unlike EmitNodeEvent, delivery is
// never broadcast: a Run subscription belongs to a single client connection.
type RunEventSubscriber interface {
	// DeliverRunEvent reports whether the event was actually queued for
	// delivery. false means the caller must treat this as a gap (see
	// DeliverRunGap) — it must never be silently ignored.
	DeliverRunEvent(event proto.RunEventPayload) bool
	DeliverRunGap(runID string, fromSequence int)
	// Defer runs fn once the RPC response currently being handled has been
	// enqueued ahead of anything fn might send — never before, and never at
	// all if that response could not be enqueued (e.g. the connection died).
	// A subscribeRun handler must use this to activate live delivery, or a
	// live event can race the synchronous RPC response onto the wire and
	// arrive first, breaking the "replay, then live" ordering guarantee.
	Defer(fn func())
	// NotifyClosed runs fn when the underlying connection is closed (or
	// immediately, if it already is). Domain subscribers use it to
	// unsubscribe on disconnect instead of leaking a subscription forever.
	// The returned func unregisters fn if the subscription ends on its own
	// (before the connection closes) so a long-lived connection does not
	// accumulate one stale callback per past subscription.
	NotifyClosed(fn func()) func()
}

// ResponseFrameFitter lets a paginated result shrink itself using the real
// JSON-RPC id before the response is queued. Implementations must measure the
// complete proto.Response, not only their result payload.
type ResponseFrameFitter interface {
	FitResponseFrame(id any, maxBytes int) *proto.Error
}

// SubscribeHandler is the extension seam for methods that must address their
// own calling connection to satisfy a later, out-of-band notification
// (v1/subscribeRun). The RunEventSubscriber is only ever the connection that
// invoked the method.
type SubscribeHandler func(sub RunEventSubscriber, params []byte) (any, *proto.Error)

// ClientHandler is the extension seam for methods that need the calling
// connection's handshake client name (RFC §9: a Run's source is derived from
// client.name), without exposing any other connection internals.
type ClientHandler func(clientName string, params []byte) (any, *proto.Error)

// Server dispatches v1 methods over accepted connections.
type Server struct {
	config   Config
	handlers map[string]internalHandler

	// active tracks the per-connection goroutines so Serve can only return
	// once no handler is still running.
	active sync.WaitGroup

	mu               sync.Mutex
	conns            map[*connection]struct{}
	shutdown         bool
	shutdownAfterAck bool
	closed           chan struct{}
	closeOne         sync.Once
	listener         transport.Listener
}

// New builds a Server with the core v1 method surface. Domain methods
// (workspaces, capabilities, runs, approvals) attach via Handle.
func New(config Config) *Server {
	if config.HandshakeTimeout <= 0 {
		config.HandshakeTimeout = 10 * time.Second
	}
	// An empty token would silently authenticate any client that omits the
	// field: reject at boot so the misconfiguration cannot escape.
	if config.Token == "" {
		panic("runner server: empty Token in Config; refusing to authenticate without one")
	}
	if config.NodeStatus == nil {
		config.NodeStatus = func() proto.NodeStatus {
			return proto.NodeStatus{State: "degraded", Workspaces: []proto.WorkspaceSummary{}}
		}
	}
	if config.Log == nil {
		config.Log = func(string, ...any) {}
	}
	server := &Server{
		config:   config,
		handlers: make(map[string]internalHandler),
		conns:    make(map[*connection]struct{}),
		closed:   make(chan struct{}),
	}
	server.handlers["v1/initialize"] = server.handleInitialize
	server.handlers["v1/ping"] = server.handlePing
	server.handlers["v1/getNodeStatus"] = server.handleGetNodeStatus
	server.handlers["v1/subscribeNode"] = server.handleSubscribeNode
	server.handlers["v1/shutdown"] = server.handleShutdown
	return server
}

// Handle registers (or replaces) a v1 method. Methods run on the reading
// goroutine of their connection; long work must spawn its own goroutine.
func (s *Server) Handle(method string, run Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = func(_ *connection, params []byte) (any, *proto.Error) {
		return run(params)
	}
}

// HandleSubscribe registers (or replaces) a v1 method that needs to address
// its own calling connection, e.g. to register it for later out-of-band
// notifications. See SubscribeHandler.
func (s *Server) HandleSubscribe(method string, run SubscribeHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = func(conn *connection, params []byte) (any, *proto.Error) {
		return run(conn, params)
	}
}

// HandleWithClient registers (or replaces) a v1 method that needs the
// calling connection's handshake client name. See ClientHandler.
func (s *Server) HandleWithClient(method string, run ClientHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = func(conn *connection, params []byte) (any, *proto.Error) {
		return run(conn.name(), params)
	}
}

// Serve accepts connections until ctx ends, the listener dies, or a client
// requests shutdown. It always closes the listener.
func (s *Server) Serve(ctx context.Context, listener transport.Listener) error {
	// Wait for in-flight handlers before returning: a supervisor that sees
	// the process exit while a handler is still touching disk state gets
	// corruption, not a graceful shutdown.
	defer s.active.Wait()
	defer listener.Close()
	s.mu.Lock()
	s.listener = listener
	stopping := s.shutdown
	select {
	case <-s.closed:
		stopping = true
	default:
	}
	s.mu.Unlock()
	if stopping {
		return nil
	}
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-s.closed:
		}
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		connection := newConnection(s, conn)
		s.mu.Lock()
		if s.shutdown {
			s.mu.Unlock()
			conn.Close()
			continue
		}
		s.conns[connection] = struct{}{}
		s.mu.Unlock()
		s.active.Add(1)
		go func() {
			defer s.active.Done()
			connection.serve()
		}()
	}
}

// Close stops accepting, closes every connection, and signals Serve to
// return. Safe to call multiple times.
func (s *Server) Close() {
	s.closeOne.Do(func() {
		close(s.closed)
	})
	s.mu.Lock()
	listener := s.listener
	s.listener = nil
	for conn := range s.conns {
		conn.close()
	}
	s.conns = make(map[*connection]struct{})
	s.mu.Unlock()
	if listener != nil {
		listener.Close()
	}
}

// EmitNodeEvent delivers a v1/nodeEvent notification to every subscribed
// connection. Delivery is best-effort: a stalled subscriber drops events.
func (s *Server) EmitNodeEvent(event proto.NodeEvent) {
	s.mu.Lock()
	connections := make([]*connection, 0, len(s.conns))
	for conn := range s.conns {
		connections = append(connections, conn)
	}
	s.mu.Unlock()
	for _, conn := range connections {
		conn.sendNodeEvent(event)
	}
}

func (s *Server) nodeStatusSnapshot() proto.NodeStatus {
	status := s.config.NodeStatus()
	if status.ProtocolVersion == 0 {
		status.ProtocolVersion = proto.ProtocolVersion
	}
	if status.RunnerVersion == "" {
		status.RunnerVersion = s.config.RunnerVersion
	}
	if status.Workspaces == nil {
		status.Workspaces = []proto.WorkspaceSummary{}
	}
	return status
}

func (s *Server) handleInitialize(conn *connection, params []byte) (any, *proto.Error) {
	var request proto.InitializeParams
	if err := decodeParams(params, &request); err != nil {
		return nil, err
	}
	if request.Token != s.config.Token {
		return nil, &proto.Error{Code: proto.CodeAuthFailed, Message: "invalid runner token"}
	}
	var versioned struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	_ = decodeParams(params, &versioned)
	if versioned.ProtocolVersion != 0 && versioned.ProtocolVersion != proto.ProtocolVersion {
		return nil, &proto.Error{
			Code:    proto.CodeVersionMismatch,
			Message: "protocol version not supported",
			Data:    map[string]int{"expected": versioned.ProtocolVersion, "supported": proto.ProtocolVersion},
		}
	}
	conn.initialize(request.Client.Name)
	capabilities := s.config.Capabilities
	if capabilities == nil {
		capabilities = []string{}
	}
	return proto.InitializeResult{
		ProtocolVersion: proto.ProtocolVersion,
		RunnerVersion:   s.config.RunnerVersion,
		Capabilities:    capabilities,
		NodeStatus:      s.nodeStatusSnapshot(),
	}, nil
}

func (s *Server) handlePing(*connection, []byte) (any, *proto.Error) {
	return map[string]string{"pong": time.Now().UTC().Format(time.RFC3339Nano)}, nil
}

func (s *Server) handleGetNodeStatus(*connection, []byte) (any, *proto.Error) {
	return s.nodeStatusSnapshot(), nil
}

func (s *Server) handleSubscribeNode(conn *connection, _ []byte) (any, *proto.Error) {
	// Open the buffering window before reading the status: an event raised
	// while the snapshot is being built belongs to the subscriber.
	conn.beginNodeSubscription()
	return s.nodeStatusSnapshot(), nil
}

func (s *Server) handleShutdown(conn *connection, params []byte) (any, *proto.Error) {
	var request struct {
		Reason string `json:"reason"`
	}
	if len(params) > 0 {
		_ = decodeParams(params, &request)
	}
	s.config.Log("shutdown requested: %s", request.Reason)
	s.mu.Lock()
	already := s.shutdown
	s.shutdown = true
	if !already {
		s.shutdownAfterAck = true
	}
	s.mu.Unlock()
	return map[string]any{}, nil
}

func (s *Server) isShuttingDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdown
}

func (s *Server) shutdownAfterAckCallback() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.shutdownAfterAck {
		return nil
	}
	s.shutdownAfterAck = false
	return func() {
		if s.config.OnShutdown != nil {
			s.config.OnShutdown()
		}
		s.Close()
	}
}

func decodeParams(params []byte, out any) *proto.Error {
	if len(params) == 0 {
		return nil
	}
	if err := unmarshalParams(params, out); err != nil {
		return &proto.Error{Code: proto.CodeInvalidParams, Message: err.Error()}
	}
	return nil
}
