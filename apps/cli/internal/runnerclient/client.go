package runnerclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"syscall"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/transport"
)

// Notification is one incoming v1 notification (a JSON-RPC request without
// an id): v1/runEvent, v1/nodeEvent, v1/runLog, or any future method this
// client does not yet know about (RFC §7: unknown notifications must be
// ignored gracefully, not treated as errors).
type Notification struct {
	Method string
	Params json.RawMessage
}

// Client is a single-connection, multiplexed JSON-RPC client for the Runner
// IPC protocol. See the package doc comment for the concurrency model. A
// Client must not be copied after use.
type Client struct {
	conn    net.Conn
	encoder *proto.Encoder
	decoder *proto.Decoder

	capabilities []string
	nodeStatus   proto.NodeStatus

	nextID int64

	pendingMu sync.Mutex
	pending   map[string]chan pendingResult
	closed    bool

	notifyHandler atomic.Pointer[func(Notification)]

	closeOnce     sync.Once
	closeErr      error
	closing       chan struct{}
	readDone      chan struct{}
	notifications chan Notification
}

type pendingResult struct {
	result json.RawMessage
	err    *proto.Error
}

// Dial connects to the Runner's socket, performs v1/initialize, and starts
// the background read loop. On success the returned Client owns conn until
// Close.
//
// Dial does not retry and does not reconnect: a dropped connection later on
// is the caller's problem to detect (via a Call error) and re-Dial.
func Dial(ctx context.Context, opts Options) (*Client, error) {
	resolved, err := opts.withDefaults()
	if err != nil {
		return nil, fmt.Errorf("runnerclient: resolve options: %w", err)
	}

	conn, err := transport.Unix{}.Dial(resolved.SocketPath)
	if err != nil {
		if isNotRunning(err) {
			return nil, ErrNotRunning
		}
		return nil, fmt.Errorf("runnerclient: dial: %w", err)
	}

	client := &Client{
		conn:          conn,
		encoder:       proto.NewEncoder(conn),
		decoder:       proto.NewDecoder(conn),
		pending:       make(map[string]chan pendingResult),
		closing:       make(chan struct{}),
		readDone:      make(chan struct{}),
		notifications: make(chan Notification, 256),
	}
	if resolved.OnNotification != nil {
		client.SetNotificationHandler(resolved.OnNotification)
	}
	go client.readLoop()
	go client.notificationLoop()

	var params proto.InitializeParams
	params.Token = resolved.Token
	params.Client.Name = resolved.ClientName
	params.Client.Version = ClientVersion
	// The wire params also need protocolVersion, which InitializeParams does
	// not carry (server.go reads it via a second decode pass); send it
	// through a superset struct so the server can see it.
	wireParams := struct {
		proto.InitializeParams
		ProtocolVersion int `json:"protocolVersion"`
	}{InitializeParams: params, ProtocolVersion: resolved.ProtocolVersion}

	var result proto.InitializeResult
	dialCtx, cancel := context.WithTimeout(ctx, resolved.HandshakeTimeout)
	defer cancel()
	if err := client.Call(dialCtx, "v1/initialize", wireParams, &result); err != nil {
		var rpcErr *proto.Error
		if errors.As(err, &rpcErr) && rpcErr.Code == proto.CodeVersionMismatch {
			expected := resolved.ProtocolVersion
			supported := resolved.ProtocolVersion
			if data, ok := rpcErr.Data.(map[string]any); ok {
				if value, ok := protocolVersionNumber(data["expected"]); ok {
					expected = value
				}
				if value, ok := protocolVersionNumber(data["supported"]); ok {
					supported = value
				}
			}
			_ = client.Close()
			return nil, &VersionMismatchError{Expected: expected, Supported: supported}
		}
		_ = client.Close()
		return nil, fmt.Errorf("runnerclient: initialize: %w", err)
	}
	if result.ProtocolVersion != 0 && result.ProtocolVersion != resolved.ProtocolVersion {
		_ = client.Close()
		return nil, &VersionMismatchError{Expected: resolved.ProtocolVersion, Supported: result.ProtocolVersion}
	}

	client.capabilities = result.Capabilities
	client.nodeStatus = result.NodeStatus
	return client, nil
}

func protocolVersionNumber(value any) (int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	if err != nil {
		return 0, false
	}
	return int(parsed), true
}

// isNotRunning classifies a dial failure as "no daemon listening" vs. some
// other unexpected error (e.g. a permission problem, which should surface as
// a real error instead of silently triggering standalone fallback).
func isNotRunning(err error) bool {
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if errors.Is(opErr.Err, syscall.ENOENT) || errors.Is(opErr.Err, syscall.ECONNREFUSED) {
			return true
		}
	}
	return false
}

// Capabilities returns the optional features the Runner advertised at
// handshake (RFC §7).
func (c *Client) Capabilities() []string { return c.capabilities }

// HasCapability reports whether the Runner advertised name at handshake.
func (c *Client) HasCapability(name string) bool {
	for _, capability := range c.capabilities {
		if capability == name {
			return true
		}
	}
	return false
}

// NodeStatus returns the node status snapshot taken at handshake time. It is
// not refreshed automatically; call GetNodeStatus for a live view.
func (c *Client) NodeStatus() proto.NodeStatus { return c.nodeStatus }

// SetNotificationHandler installs (or replaces) the callback invoked for
// every subsequently dispatched notification. Safe to call at any time,
// including concurrently with an active read loop.
func (c *Client) SetNotificationHandler(handler func(Notification)) {
	if handler == nil {
		c.notifyHandler.Store(nil)
		return
	}
	c.notifyHandler.Store(&handler)
}

// Call issues one JSON-RPC request and blocks for its matching response (or
// ctx cancellation, or Close). Multiple goroutines may call Call
// concurrently on the same Client.
//
// A *proto.Error response is returned as-is (as an error) so callers can
// type-assert it and switch on .Code; any other failure (encoding, closed
// client, context) is returned as a plain error.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	idNum := atomic.AddInt64(&c.nextID, 1)
	id := fmt.Sprintf("%d", idNum)
	ch := make(chan pendingResult, 1)

	c.pendingMu.Lock()
	if c.closed {
		c.pendingMu.Unlock()
		return ErrClosed
	}
	c.pending[id] = ch
	c.pendingMu.Unlock()

	unregister := func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}

	frame := map[string]any{
		"jsonrpc": "2.0",
		"id":      idNum,
		"method":  method,
		"params":  params,
	}
	if err := c.encoder.WriteFrame(frame); err != nil {
		unregister()
		return fmt.Errorf("runnerclient: write %s: %w", method, err)
	}

	select {
	case res, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if res.err != nil {
			return res.err
		}
		if result != nil && len(res.result) > 0 {
			if err := json.Unmarshal(res.result, result); err != nil {
				return fmt.Errorf("runnerclient: decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		unregister()
		return ctx.Err()
	}
}

// readLoop is the sole reader of the connection: it demultiplexes incoming
// frames to either a pending Call (by id) or the notification handler.
//
// Frames are classified by presence of "method": the server only ever sends
// two shapes on this connection — a notification (method, no id) or an RPC
// response (id, no method; RFC §5, §10). proto.DecodeMessage cannot be
// reused here: it validates the server's *inbound* contract, where a
// message without a method is always malformed, which would silently drop
// every RPC response.
func (c *Client) readLoop() {
	defer func() {
		close(c.notifications)
		close(c.readDone)
	}()
	for {
		frame, err := c.decoder.ReadFrame()
		if err != nil {
			c.failAllPending()
			return
		}
		var envelope struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			// A malformed frame from the server is not something this
			// client can resynchronize from (RFC §4): close the connection.
			c.failAllPending()
			return
		}
		if envelope.Method != "" {
			select {
			case c.notifications <- Notification{Method: envelope.Method, Params: envelope.Params}:
			case <-c.closing:
				c.failAllPending()
				return
			}
			continue
		}
		// It's an RPC response.
		var response proto.Response
		if err := json.Unmarshal(frame, &response); err != nil {
			continue
		}
		id := response.ID.(proto.ID).String()
		c.pendingMu.Lock()
		ch, found := c.pending[id]
		if found {
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()
		if !found {
			continue
		}
		result, _ := response.Result.(json.RawMessage)
		ch <- pendingResult{result: result, err: response.Err}
	}
}

func (c *Client) notificationLoop() {
	for {
		select {
		case <-c.closing:
			return
		case notification, ok := <-c.notifications:
			if !ok {
				return
			}
			select {
			case <-c.closing:
				return
			default:
			}
			c.dispatchNotification(notification.Method, notification.Params)
		}
	}
}

func (c *Client) dispatchNotification(method string, params json.RawMessage) {
	handlerPtr := c.notifyHandler.Load()
	if handlerPtr == nil {
		return
	}
	(*handlerPtr)(Notification{Method: method, Params: params})
}

func (c *Client) failAllPending() {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan pendingResult)
	c.closed = true
	c.pendingMu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
}

// Close closes the underlying connection and fails every pending Call with
// ErrClosed. Idempotent.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closing)
		c.closeErr = c.conn.Close()
		<-c.readDone
		// readLoop's own failAllPending already fires as ReadFrame returns
		// an error from the closed conn, but close it defensively here too
		// in case readLoop already exited for another reason first.
		c.pendingMu.Lock()
		pending := c.pending
		c.pending = make(map[string]chan pendingResult)
		c.closed = true
		c.pendingMu.Unlock()
		for _, ch := range pending {
			close(ch)
		}
	})
	return c.closeErr
}
