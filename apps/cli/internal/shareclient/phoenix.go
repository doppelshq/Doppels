package shareclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"doppels.so/cli/internal/execution"

	"github.com/coder/websocket"
)

type Socket interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Close(websocket.StatusCode, string) error
	CloseNow() error
}

type DialFunc func(context.Context, string) (Socket, error)

func coderDial(httpClient *http.Client) DialFunc {
	return func(ctx context.Context, endpoint string) (Socket, error) {
		connection, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: httpClient})
		if err != nil {
			if response != nil {
				if response.Body != nil {
					_ = response.Body.Close()
				}
				return nil, HTTPError{Operation: "connect Share channel", StatusCode: response.StatusCode}
			}
			return nil, err
		}
		connection.SetReadLimit(maxResponseBytes)
		return connection, nil
	}
}

type phoenixFrame struct {
	JoinRef *string
	Ref     *string
	Topic   string
	Event   string
	Payload json.RawMessage
}

func (frame phoenixFrame) MarshalJSON() ([]byte, error) {
	payload := frame.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	return json.Marshal([5]any{frame.JoinRef, frame.Ref, frame.Topic, frame.Event, payload})
}

func (frame *phoenixFrame) UnmarshalJSON(data []byte) error {
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	if len(values) != 5 {
		return fmt.Errorf("Phoenix V2 frame must contain five elements")
	}
	if string(values[0]) != "null" {
		if err := json.Unmarshal(values[0], &frame.JoinRef); err != nil {
			return err
		}
	}
	if string(values[1]) != "null" {
		if err := json.Unmarshal(values[1], &frame.Ref); err != nil {
			return err
		}
	}
	if err := json.Unmarshal(values[2], &frame.Topic); err != nil {
		return err
	}
	if err := json.Unmarshal(values[3], &frame.Event); err != nil {
		return err
	}
	frame.Payload = append(frame.Payload[:0], values[4]...)
	return nil
}

type phoenixReply struct {
	Status   string          `json:"status"`
	Response json.RawMessage `json:"response"`
}

type ReplyError struct{ Response json.RawMessage }

func (error ReplyError) Error() string { return "Cloud rejected the Phoenix Channel message" }

type phoenixConnection struct {
	socket    Socket
	topic     string
	joinRef   string
	refs      atomic.Uint64
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan replyResult
	inbound   chan ShareMessage
	failure   chan error
	done      chan struct{}
	closeOnce sync.Once
	heartbeat time.Duration
}

type replyResult struct {
	reply phoenixReply
	err   error
}

func newPhoenixConnection(ctx context.Context, socket Socket, topic string, heartbeat time.Duration) *phoenixConnection {
	connection := &phoenixConnection{
		socket: socket, topic: topic, pending: make(map[string]chan replyResult),
		inbound: make(chan ShareMessage, 64), failure: make(chan error, 1), done: make(chan struct{}), heartbeat: heartbeat,
	}
	go connection.readLoop(ctx)
	if heartbeat > 0 {
		go connection.heartbeatLoop(ctx)
	}
	return connection
}

func (connection *phoenixConnection) join(ctx context.Context, runnerToken string) (*PendingState, error) {
	reply, err := connection.push(ctx, connection.topic, "phx_join", map[string]string{"token": runnerToken}, true)
	if err != nil {
		return nil, err
	}
	var state PendingState
	if err := decodeRawJSON(reply.Response, &state); err != nil {
		return nil, fmt.Errorf("decode Channel join state: %w", err)
	}
	return &state, nil
}

func (connection *phoenixConnection) push(ctx context.Context, topic, event string, payload any, joined bool) (phoenixReply, error) {
	reference := strconv.FormatUint(connection.refs.Add(1), 10)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return phoenixReply{}, err
	}
	frame := phoenixFrame{Ref: &reference, Topic: topic, Event: event, Payload: encoded}
	if event == "phx_join" {
		connection.joinRef = reference
		frame.JoinRef = &connection.joinRef
	} else if joined {
		frame.JoinRef = &connection.joinRef
	}
	wait := make(chan replyResult, 1)
	connection.pendingMu.Lock()
	connection.pending[reference] = wait
	connection.pendingMu.Unlock()
	defer func() {
		connection.pendingMu.Lock()
		delete(connection.pending, reference)
		connection.pendingMu.Unlock()
	}()
	if err := connection.write(ctx, frame); err != nil {
		return phoenixReply{}, err
	}
	select {
	case result := <-wait:
		return result.reply, result.err
	case <-connection.done:
		return phoenixReply{}, errors.New("Phoenix Channel disconnected")
	case <-ctx.Done():
		return phoenixReply{}, ctx.Err()
	}
}

func (connection *phoenixConnection) send(ctx context.Context, topic, event string, payload any, joined bool) error {
	reference := strconv.FormatUint(connection.refs.Add(1), 10)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	frame := phoenixFrame{Ref: &reference, Topic: topic, Event: event, Payload: encoded}
	if joined {
		frame.JoinRef = &connection.joinRef
	}
	return connection.write(ctx, frame)
}

func (connection *phoenixConnection) write(ctx context.Context, frame phoenixFrame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	return connection.socket.Write(ctx, websocket.MessageText, data)
}

func (connection *phoenixConnection) readLoop(ctx context.Context) {
	for {
		messageType, data, err := connection.socket.Read(ctx)
		if err != nil {
			connection.fail(err)
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		var frame phoenixFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			connection.fail(fmt.Errorf("decode Phoenix frame: %w", err))
			return
		}
		if frame.Topic != connection.topic && frame.Topic != "phoenix" {
			continue
		}
		if frame.Event == "phx_reply" && frame.Ref != nil {
			var reply phoenixReply
			if err := json.Unmarshal(frame.Payload, &reply); err != nil {
				connection.resolve(*frame.Ref, replyResult{err: err})
				continue
			}
			if reply.Status != "ok" {
				connection.resolve(*frame.Ref, replyResult{reply: reply, err: ReplyError{Response: reply.Response}})
				continue
			}
			connection.resolve(*frame.Ref, replyResult{reply: reply})
			continue
		}
		if frame.Event == "phx_error" || frame.Event == "phx_close" {
			connection.fail(fmt.Errorf("Phoenix Channel closed with %s", frame.Event))
			return
		}
		var message ShareMessage
		if err := json.Unmarshal(frame.Payload, &message); err != nil {
			connection.fail(fmt.Errorf("decode ShareMessage: %w", err))
			return
		}
		expectedShareID := strings.TrimPrefix(connection.topic, "share:")
		if message.Kind != "ShareMessage" || message.ShareID != expectedShareID || message.MessageID == "" || message.Event != frame.Event {
			connection.fail(errors.New("received an invalid ShareMessage envelope"))
			return
		}
		// The runner receives its own persisted broadcasts before the push reply.
		// They are acknowledgements, not work items; queueing them can deadlock a
		// long Run once the bounded inbound buffer fills and phx_reply cannot be
		// read. SubmitRun/SubmitRunEvent validate the authoritative reply instead.
		switch message.Event {
		case "run_recorded", "run_event_recorded", "runner_status":
			continue
		case "request_available", "share_closed":
			// Forward the only two runner-side lifecycle notifications.
		default:
			connection.fail(fmt.Errorf("received unsupported Share event %q", message.Event))
			return
		}
		select {
		case connection.inbound <- message:
		case <-ctx.Done():
			return
		}
	}
}

func (connection *phoenixConnection) resolve(reference string, result replyResult) {
	connection.pendingMu.Lock()
	wait := connection.pending[reference]
	connection.pendingMu.Unlock()
	if wait != nil {
		wait <- result
	}
}

func (connection *phoenixConnection) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(connection.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			heartbeatCtx, cancel := context.WithTimeout(ctx, connection.heartbeat)
			err := connection.send(heartbeatCtx, "phoenix", "heartbeat", map[string]any{}, false)
			cancel()
			if err != nil {
				connection.fail(err)
				return
			}
		case <-connection.done:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (connection *phoenixConnection) fail(err error) {
	connection.closeOnce.Do(func() {
		select {
		case connection.failure <- err:
		default:
		}
		close(connection.done)
		_ = connection.socket.CloseNow()
	})
}

func (connection *phoenixConnection) close() {
	connection.closeOnce.Do(func() {
		close(connection.done)
		_ = connection.socket.Close(websocket.StatusNormalClosure, "share complete")
	})
}

func (connection *phoenixConnection) abort() {
	connection.closeOnce.Do(func() {
		close(connection.done)
		_ = connection.socket.CloseNow()
	})
}

type Channel struct {
	client      *Client
	shareID     string
	runnerToken string
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.RWMutex
	connection  *phoenixConnection
	reconnectMu sync.Mutex
	recoveries  []PendingState
	seenMu      sync.Mutex
	seen        map[string]struct{}
}

func (client *Client) OpenChannel(ctx context.Context, shareID, runnerToken string) (*Channel, *PendingState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !validUUID(shareID) || runnerToken == "" || containsControl(runnerToken) {
		return nil, nil, errors.New("valid share id and runner token are required")
	}
	channelCtx, cancel := context.WithCancel(ctx)
	channel := &Channel{client: client, shareID: shareID, runnerToken: runnerToken, ctx: channelCtx, cancel: cancel, seen: make(map[string]struct{})}
	connection, state, err := channel.connect(channelCtx)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	channel.connection = connection
	return channel, state, nil
}

func (channel *Channel) Close() {
	channel.cancel()
	channel.mu.Lock()
	if channel.connection != nil {
		channel.connection.abort()
	}
	channel.mu.Unlock()
}

func (channel *Channel) Next(ctx context.Context) (Update, error) {
	operationContext, cancel := channel.operationContext(ctx)
	defer cancel()
	for {
		channel.reconnectMu.Lock()
		if len(channel.recoveries) > 0 {
			recovery := channel.recoveries[0]
			channel.recoveries = channel.recoveries[1:]
			channel.reconnectMu.Unlock()
			return Update{Recovery: &recovery}, nil
		}
		channel.reconnectMu.Unlock()
		connection := channel.current()
		select {
		case message := <-connection.inbound:
			if channel.markSeen(message.MessageID) {
				return Update{Message: &message}, nil
			}
		case <-connection.failure:
			recovery, err := channel.reconnect(operationContext, connection, false)
			if err != nil {
				return Update{}, err
			}
			return Update{Recovery: recovery}, nil
		case <-operationContext.Done():
			return Update{}, operationContext.Err()
		}
	}
}

func (channel *Channel) SubmitRun(ctx context.Context, run execution.RunRecord) (*ShareMessage, error) {
	return channel.submit(ctx, "run_submitted", "run_recorded", run)
}

func (channel *Channel) SubmitRunEvent(ctx context.Context, event execution.RunEvent) (*ShareMessage, error) {
	return channel.submit(ctx, "run_event_submitted", "run_event_recorded", SanitizeRunEvent(event))
}

func (channel *Channel) submit(ctx context.Context, submittedEvent, recordedEvent string, value any) (*ShareMessage, error) {
	operationContext, cancel := channel.operationContext(ctx)
	defer cancel()
	envelope, err := newShareMessage(channel.shareID, submittedEvent, value, channel.client.now())
	if err != nil {
		return nil, err
	}
	for {
		connection := channel.current()
		reply, pushErr := connection.push(operationContext, connection.topic, submittedEvent, envelope, true)
		if pushErr == nil {
			var recorded ShareMessage
			if err := decodeRawJSON(reply.Response, &recorded); err != nil {
				return nil, fmt.Errorf("decode %s acknowledgement: %w", submittedEvent, err)
			}
			if err := validateAcknowledgement(envelope, recorded, recordedEvent); err != nil {
				return nil, err
			}
			channel.markSeen(recorded.MessageID)
			return &recorded, nil
		}
		var rejected ReplyError
		if errors.As(pushErr, &rejected) || errors.Is(pushErr, context.Canceled) || errors.Is(pushErr, context.DeadlineExceeded) {
			return nil, pushErr
		}
		if _, err := channel.reconnect(operationContext, connection, true); err != nil {
			return nil, err
		}
		// Retry the exact same envelope and messageId after recovery.
	}
}

func (channel *Channel) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	operationContext, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(channel.ctx, cancel)
	if channel.ctx.Err() != nil {
		cancel()
	}
	return operationContext, func() {
		stop()
		cancel()
	}
}

func (channel *Channel) connect(ctx context.Context) (*phoenixConnection, *PendingState, error) {
	endpoint := channel.client.websocketURL()
	socket, err := channel.client.dial(ctx, endpoint)
	if err != nil {
		var problem HTTPError
		if errors.As(err, &problem) {
			return nil, nil, problem
		}
		return nil, nil, fmt.Errorf("connect Share channel: %s", redact(err.Error(), channel.runnerToken))
	}
	connection := newPhoenixConnection(channel.ctx, socket, "share:"+channel.shareID, channel.client.heartbeatInterval)
	state, err := connection.join(ctx, channel.runnerToken)
	if err != nil {
		connection.close()
		return nil, nil, fmt.Errorf("join Share channel: %w", err)
	}
	if state.Share.ID != channel.shareID {
		connection.close()
		return nil, nil, errors.New("Channel joined a different Share")
	}
	return connection, state, nil
}

func (channel *Channel) reconnect(ctx context.Context, failed *phoenixConnection, queueRecovery bool) (*PendingState, error) {
	channel.reconnectMu.Lock()
	defer channel.reconnectMu.Unlock()
	if channel.current() != failed {
		if !queueRecovery && len(channel.recoveries) > 0 {
			recovery := channel.recoveries[0]
			channel.recoveries = channel.recoveries[1:]
			return &recovery, nil
		}
		pending, err := channel.client.Pending(ctx, channel.shareID, channel.runnerToken)
		if err == nil && queueRecovery {
			channel.recoveries = append(channel.recoveries, *pending)
		}
		return pending, err
	}
	failed.close()
	delay := channel.client.reconnectMin
	for {
		connection, _, err := channel.connect(ctx)
		if err == nil {
			pending, pendingErr := channel.client.Pending(ctx, channel.shareID, channel.runnerToken)
			if pendingErr == nil {
				channel.mu.Lock()
				channel.connection = connection
				channel.mu.Unlock()
				if queueRecovery {
					channel.recoveries = append(channel.recoveries, *pending)
				}
				return pending, nil
			}
			connection.close()
			err = pendingErr
		}
		var problem HTTPError
		if errors.As(err, &problem) && problem.Permanent() {
			return nil, err
		}
		var rejected ReplyError
		if errors.As(err, &rejected) {
			return nil, err
		}
		if err := waitContext(ctx, delay); err != nil {
			return nil, err
		}
		if delay == 0 {
			delay = time.Millisecond
		}
		delay *= 2
		if delay > channel.client.reconnectMax {
			delay = channel.client.reconnectMax
		}
	}
}

func (channel *Channel) current() *phoenixConnection {
	channel.mu.RLock()
	defer channel.mu.RUnlock()
	return channel.connection
}
func (channel *Channel) markSeen(messageID string) bool {
	channel.seenMu.Lock()
	defer channel.seenMu.Unlock()
	if _, exists := channel.seen[messageID]; exists {
		return false
	}
	channel.seen[messageID] = struct{}{}
	return true
}

func newShareMessage(shareID, event string, payload any, occurredAt time.Time) (ShareMessage, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ShareMessage{}, err
	}
	messageID, err := newUUID()
	if err != nil {
		return ShareMessage{}, err
	}
	return ShareMessage{APIVersion: APIVersion, Kind: "ShareMessage", MessageID: messageID, ShareID: shareID, OccurredAt: occurredAt.UTC(), Event: event, Payload: encoded}, nil
}

func newUUID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func validateAcknowledgement(submitted, recorded ShareMessage, expectedEvent string) error {
	if recorded.APIVersion != APIVersion || recorded.Kind != "ShareMessage" || recorded.Event != expectedEvent || recorded.ShareID != submitted.ShareID || recorded.MessageID == "" {
		return errors.New("Cloud returned an invalid persistence acknowledgement")
	}
	equal, err := acknowledgementPayloadsEqual(submitted.Event, submitted.Payload, recorded.Payload)
	if err != nil {
		return errors.New("Cloud returned an invalid acknowledgement payload")
	}
	if !equal {
		return errors.New("Cloud acknowledgement changed the submitted payload")
	}
	return nil
}

// Phoenix/Ash may render an equivalent timestamp with a different number of
// fractional digits. Compare domain records instead of their JSON spelling,
// while keeping every non-temporal field exact.
func acknowledgementPayloadsEqual(event string, submitted, recorded json.RawMessage) (bool, error) {
	switch event {
	case "run_submitted":
		var left, right execution.RunRecord
		if err := decodeRawJSON(submitted, &left); err != nil {
			return false, err
		}
		if err := decodeRawJSON(recorded, &right); err != nil {
			return false, err
		}
		return left.APIVersion == right.APIVersion && left.Kind == right.Kind && left.ID == right.ID &&
			left.RequestID == right.RequestID && left.CreatedAt.Equal(right.CreatedAt) &&
			left.Capability == right.Capability && definitionReferencesEqual(left.Recipe, right.Recipe) &&
			jsonValuesEqual(left.Inputs, right.Inputs) && left.Executor == right.Executor && left.NodeID == right.NodeID, nil
	case "run_event_submitted":
		var left, right execution.RunEvent
		if err := decodeRawJSON(submitted, &left); err != nil {
			return false, err
		}
		if err := decodeRawJSON(recorded, &right); err != nil {
			return false, err
		}
		return left.APIVersion == right.APIVersion && left.Kind == right.Kind && left.RunID == right.RunID &&
			left.Sequence == right.Sequence && left.OccurredAt.Equal(right.OccurredAt) &&
			left.Type == right.Type && left.StepID == right.StepID && jsonValuesEqual(left.Data, right.Data), nil
	default:
		return false, fmt.Errorf("unsupported submitted event %q", event)
	}
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func (client *Client) websocketURL() string {
	endpoint := *client.server
	if endpoint.Scheme == "https" {
		endpoint.Scheme = "wss"
	} else {
		endpoint.Scheme = "ws"
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/socket/websocket"
	query := url.Values{}
	query.Set("vsn", "2.0.0")
	endpoint.RawQuery = query.Encode()
	return endpoint.String()
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func redact(value, secret string) string {
	if secret == "" {
		return value
	}
	return strings.ReplaceAll(value, secret, "[REDACTED]")
}
