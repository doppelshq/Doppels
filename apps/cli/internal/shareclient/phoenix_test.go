package shareclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"

	"github.com/coder/websocket"
)

func readPhoenix(ctx context.Context, connection *websocket.Conn) (phoenixFrame, error) {
	messageType, data, err := connection.Read(ctx)
	if err != nil {
		return phoenixFrame{}, err
	}
	if messageType != websocket.MessageText {
		return phoenixFrame{}, fmt.Errorf("message type = %v", messageType)
	}
	var frame phoenixFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return phoenixFrame{}, err
	}
	return frame, nil
}

func writePhoenix(ctx context.Context, connection *websocket.Conn, frame phoenixFrame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return connection.Write(ctx, websocket.MessageText, data)
}

func writePhoenixReply(ctx context.Context, connection *websocket.Conn, request phoenixFrame, response any) error {
	responseJSON, err := json.Marshal(response)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(phoenixReply{Status: "ok", Response: responseJSON})
	if err != nil {
		return err
	}
	return writePhoenix(ctx, connection, phoenixFrame{JoinRef: request.JoinRef, Ref: request.Ref, Topic: request.Topic, Event: "phx_reply", Payload: payload})
}

func writeBroadcast(ctx context.Context, connection *websocket.Conn, message ShareMessage) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return writePhoenix(ctx, connection, phoenixFrame{Topic: "share:" + message.ShareID, Event: message.Event, Payload: payload})
}

func joinServer(ctx context.Context, connection *websocket.Conn) (phoenixFrame, error) {
	join, err := readPhoenix(ctx, connection)
	if err != nil {
		return phoenixFrame{}, err
	}
	if join.Event != "phx_join" || join.Ref == nil || join.JoinRef == nil || *join.Ref != *join.JoinRef {
		return phoenixFrame{}, fmt.Errorf("invalid join frame: %#v", join)
	}
	var payload map[string]string
	if err := json.Unmarshal(join.Payload, &payload); err != nil {
		return phoenixFrame{}, err
	}
	if payload["token"] != testRunnerToken {
		return phoenixFrame{}, fmt.Errorf("join token missing")
	}
	if err := writePhoenixReply(ctx, connection, join, testPending()); err != nil {
		return phoenixFrame{}, err
	}
	return join, nil
}

func TestPhoenixV2JoinHeartbeatAndMessageDedupe(t *testing.T) {
	heartbeatSeen := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/socket/websocket" || request.URL.Query().Get("vsn") != "2.0.0" {
			heartbeatSeen <- fmt.Errorf("socket URL = %s", request.URL.String())
			return
		}
		if request.URL.Query().Has("token") || strings.Contains(request.URL.String(), testRunnerToken) {
			heartbeatSeen <- errors.New("runner token leaked in URL")
			return
		}
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			heartbeatSeen <- err
			return
		}
		defer connection.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := joinServer(ctx, connection); err != nil {
			heartbeatSeen <- err
			return
		}

		requestPayload, _ := json.Marshal(testRequest())
		available := ShareMessage{APIVersion: APIVersion, Kind: "ShareMessage", MessageID: "message-request", ShareID: testShareID, OccurredAt: testNow, Event: "request_available", Payload: requestPayload}
		closedPayload := json.RawMessage(`{"reason":"completed"}`)
		closed := ShareMessage{APIVersion: APIVersion, Kind: "ShareMessage", MessageID: "message-closed", ShareID: testShareID, OccurredAt: testNow, Event: "share_closed", Payload: closedPayload}
		if err := writeBroadcast(ctx, connection, available); err != nil {
			heartbeatSeen <- err
			return
		}
		if err := writeBroadcast(ctx, connection, available); err != nil {
			heartbeatSeen <- err
			return
		}
		if err := writeBroadcast(ctx, connection, closed); err != nil {
			heartbeatSeen <- err
			return
		}
		for {
			frame, err := readPhoenix(ctx, connection)
			if err != nil {
				heartbeatSeen <- err
				return
			}
			if frame.Event == "heartbeat" {
				if frame.Topic != "phoenix" || frame.JoinRef != nil {
					heartbeatSeen <- fmt.Errorf("invalid heartbeat: %#v", frame)
					return
				}
				heartbeatSeen <- writePhoenixReply(ctx, connection, frame, map[string]any{})
				return
			}
		}
	}))
	defer server.Close()
	client, err := New(Options{Server: server.URL, HTTPClient: server.Client(), HeartbeatInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	channel, state, err := client.OpenChannel(ctx, testShareID, testRunnerToken)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	if state.Share.ID != testShareID {
		t.Fatalf("join state Share = %q", state.Share.ID)
	}
	first, err := channel.Next(ctx)
	if err != nil || first.Message == nil || first.Message.Event != "request_available" {
		t.Fatalf("first update = %#v, %v", first, err)
	}
	request, err := first.Message.Request()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := request.Inputs["count"].(json.Number); !ok {
		t.Fatalf("Channel integer = %#v (%T)", request.Inputs["count"], request.Inputs["count"])
	}
	second, err := channel.Next(ctx)
	if err != nil || second.Message == nil || second.Message.Event != "share_closed" {
		t.Fatalf("deduped update = %#v, %v", second, err)
	}
	if err := <-heartbeatSeen; err != nil {
		t.Fatal(err)
	}
}

func TestSubmitRunReconnectsRecoversAndRetriesIdenticalEnvelope(t *testing.T) {
	var connections atomic.Int32
	var pendingCalls atomic.Int32
	var firstEnvelope ShareMessage
	var envelopeMu sync.Mutex
	serverErrors := make(chan error, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/shares/"+testShareID+"/pending", func(writer http.ResponseWriter, request *http.Request) {
		pendingCalls.Add(1)
		if request.Header.Get("Authorization") != "Bearer "+testRunnerToken {
			serverErrors <- errors.New("pending auth missing")
		}
		_ = json.NewEncoder(writer).Encode(testPending())
	})
	mux.HandleFunc("/socket/websocket", func(writer http.ResponseWriter, request *http.Request) {
		connectionNumber := connections.Add(1)
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer connection.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := joinServer(ctx, connection); err != nil {
			serverErrors <- err
			return
		}
		push, err := readPhoenix(ctx, connection)
		if err != nil {
			serverErrors <- err
			return
		}
		if push.Event != "run_submitted" {
			serverErrors <- fmt.Errorf("push event = %s", push.Event)
			return
		}
		var submitted ShareMessage
		if err := json.Unmarshal(push.Payload, &submitted); err != nil {
			serverErrors <- err
			return
		}
		if connectionNumber == 1 {
			envelopeMu.Lock()
			firstEnvelope = submitted
			envelopeMu.Unlock()
			_ = connection.CloseNow()
			return
		}
		envelopeMu.Lock()
		identical := firstEnvelope.MessageID == submitted.MessageID && jsonValuesEqual(firstEnvelope.Payload, submitted.Payload)
		envelopeMu.Unlock()
		if !identical {
			serverErrors <- errors.New("retry changed messageId or payload")
			return
		}
		recorded := submitted
		recorded.MessageID = "recorded-message"
		recorded.Event = "run_recorded"
		if err := writePhoenixReply(ctx, connection, push, recorded); err != nil {
			serverErrors <- err
			return
		}
		_, _, _ = connection.Read(ctx)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, _ := New(Options{Server: server.URL, HTTPClient: server.Client(), HeartbeatInterval: time.Hour, ReconnectMin: time.Millisecond, ReconnectMax: 2 * time.Millisecond, Now: func() time.Time { return testNow }})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	channel, _, err := client.OpenChannel(ctx, testShareID, testRunnerToken)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	ack, err := channel.SubmitRun(ctx, testRun())
	if err != nil {
		t.Fatal(err)
	}
	if ack.Event != "run_recorded" || connections.Load() != 2 || pendingCalls.Load() != 1 {
		t.Fatalf("ack=%#v connections=%d pending=%d", ack, connections.Load(), pendingCalls.Load())
	}
	recovery, err := channel.Next(ctx)
	if err != nil || recovery.Recovery == nil || recovery.Recovery.Share.ID != testShareID {
		t.Fatalf("recovery = %#v, %v", recovery, err)
	}
	select {
	case err := <-serverErrors:
		t.Fatal(err)
	default:
	}
}

func TestNextAndSubmitRunAreConcurrentSafe(t *testing.T) {
	serverErrors := make(chan error, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer connection.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := joinServer(ctx, connection); err != nil {
			serverErrors <- err
			return
		}
		push, err := readPhoenix(ctx, connection)
		if err != nil {
			serverErrors <- err
			return
		}
		payload, _ := json.Marshal(testRequest())
		message := ShareMessage{APIVersion: APIVersion, Kind: "ShareMessage", MessageID: "concurrent-request", ShareID: testShareID, OccurredAt: testNow, Event: "request_available", Payload: payload}
		if err := writeBroadcast(ctx, connection, message); err != nil {
			serverErrors <- err
			return
		}
		var submitted ShareMessage
		if err := json.Unmarshal(push.Payload, &submitted); err != nil {
			serverErrors <- err
			return
		}
		recorded := submitted
		recorded.MessageID = "concurrent-ack"
		recorded.Event = "run_recorded"
		if err := writePhoenixReply(ctx, connection, push, recorded); err != nil {
			serverErrors <- err
			return
		}
		_, _, _ = connection.Read(ctx)
	}))
	defer server.Close()
	client, _ := New(Options{Server: server.URL, HTTPClient: server.Client(), HeartbeatInterval: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	channel, _, err := client.OpenChannel(ctx, testShareID, testRunnerToken)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	var wait sync.WaitGroup
	wait.Add(2)
	var update Update
	var acknowledged *ShareMessage
	var nextErr, submitErr error
	go func() { defer wait.Done(); update, nextErr = channel.Next(ctx) }()
	go func() { defer wait.Done(); acknowledged, submitErr = channel.SubmitRun(ctx, testRun()) }()
	wait.Wait()
	if nextErr != nil || submitErr != nil || update.Message == nil || acknowledged == nil {
		t.Fatalf("next=%#v/%v ack=%#v/%v", update, nextErr, acknowledged, submitErr)
	}
	select {
	case err := <-serverErrors:
		t.Fatal(err)
	default:
	}
}

func TestSubmitDoesNotQueueItsOwnPersistedBroadcasts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := joinServer(ctx, connection); err != nil {
			return
		}
		push, err := readPhoenix(ctx, connection)
		if err != nil {
			return
		}
		var submitted ShareMessage
		if err := json.Unmarshal(push.Payload, &submitted); err != nil {
			return
		}
		for index := 0; index < 100; index++ {
			broadcast := submitted
			broadcast.MessageID = fmt.Sprintf("self-ack-%d", index)
			broadcast.Event = "run_recorded"
			if err := writeBroadcast(ctx, connection, broadcast); err != nil {
				return
			}
		}
		recorded := submitted
		recorded.MessageID = "reply-after-broadcasts"
		recorded.Event = "run_recorded"
		_ = writePhoenixReply(ctx, connection, push, recorded)
	}))
	defer server.Close()
	client, _ := New(Options{Server: server.URL, HTTPClient: server.Client(), HeartbeatInterval: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	channel, _, err := client.OpenChannel(ctx, testShareID, testRunnerToken)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	if _, err := channel.SubmitRun(ctx, testRun()); err != nil {
		t.Fatalf("SubmitRun blocked behind self broadcasts: %v", err)
	}
}

func TestReconnectStopsOnPermanentPendingError(t *testing.T) {
	var connections atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/shares/"+testShareID+"/pending", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusGone) })
	mux.HandleFunc("/socket/websocket", func(writer http.ResponseWriter, request *http.Request) {
		number := connections.Add(1)
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := joinServer(ctx, connection); err != nil {
			_ = connection.CloseNow()
			return
		}
		if number == 1 {
			_ = connection.CloseNow()
			return
		}
		defer connection.CloseNow()
		_, _, _ = connection.Read(ctx)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, _ := New(Options{Server: server.URL, HTTPClient: server.Client(), HeartbeatInterval: time.Hour, ReconnectMin: 200 * time.Millisecond, ReconnectMax: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	channel, _, err := client.OpenChannel(ctx, testShareID, testRunnerToken)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	started := time.Now()
	_, err = channel.Next(ctx)
	var problem HTTPError
	if !errors.As(err, &problem) || problem.StatusCode != http.StatusGone {
		t.Fatalf("error = %#v", err)
	}
	if time.Since(started) >= 200*time.Millisecond {
		t.Fatalf("permanent error was retried for %s", time.Since(started))
	}
}

func TestChannelCloseInterruptsReconnectBackoff(t *testing.T) {
	attempts := make(chan int, 4)
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		number := int(connections.Add(1))
		attempts <- number
		if number > 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := joinServer(ctx, connection); err != nil {
			_ = connection.CloseNow()
			return
		}
		_ = connection.CloseNow()
	}))
	defer server.Close()
	client, err := New(Options{
		Server: server.URL, HTTPClient: server.Client(), HeartbeatInterval: time.Hour,
		ReconnectMin: 30 * time.Second, ReconnectMax: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	openContext, cancelOpen := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelOpen()
	channel, _, err := client.OpenChannel(openContext, testShareID, testRunnerToken)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := channel.Next(context.Background())
		result <- err
	}()

	for connections.Load() < 2 {
		select {
		case <-attempts:
		case <-time.After(2 * time.Second):
			t.Fatal("Channel did not enter reconnect")
		}
	}
	started := time.Now()
	channel.Close()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Next error = %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Close did not interrupt reconnect backoff")
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("Close took %s", elapsed)
	}
}

func TestValidateAcknowledgementRejectsChangedPayload(t *testing.T) {
	submitted, _ := newShareMessage(testShareID, "run_submitted", testRun(), testNow)
	recorded := submitted
	recorded.MessageID = "ack"
	recorded.Event = "run_recorded"
	changed := testRun()
	changed.NodeID = "other"
	recorded.Payload, _ = json.Marshal(changed)
	if err := validateAcknowledgement(submitted, recorded, "run_recorded"); err == nil {
		t.Fatal("changed acknowledgement accepted")
	}
	if reflect.DeepEqual(submitted.Payload, recorded.Payload) {
		t.Fatal("test payload did not change")
	}
}

func TestValidateAcknowledgementAcceptsEquivalentTimestampFormatting(t *testing.T) {
	occurredAt := time.Date(2026, 8, 1, 10, 0, 48, 215_780_000, time.UTC)
	event := execution.RunEvent{
		APIVersion: APIVersion, Kind: "RunEvent", RunID: testRunID,
		Sequence: 0, OccurredAt: occurredAt, Type: "run_created",
	}
	submitted, err := newShareMessage(testShareID, "run_event_submitted", event, testNow)
	if err != nil {
		t.Fatal(err)
	}
	recorded := submitted
	recorded.MessageID = "ack"
	recorded.Event = "run_event_recorded"
	recorded.Payload = json.RawMessage(strings.Replace(string(recorded.Payload), ".21578Z", ".215780Z", 1))
	if reflect.DeepEqual(submitted.Payload, recorded.Payload) {
		t.Fatal("test did not change the JSON timestamp spelling")
	}
	if err := validateAcknowledgement(submitted, recorded, "run_event_recorded"); err != nil {
		t.Fatalf("equivalent timestamp was rejected: %v", err)
	}
}

func TestSubmitRunEventSanitizesPayloadBeforeTransport(t *testing.T) {
	serverErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer connection.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := joinServer(ctx, connection); err != nil {
			serverErrors <- err
			return
		}
		push, err := readPhoenix(ctx, connection)
		if err != nil {
			serverErrors <- err
			return
		}
		var submitted ShareMessage
		if err := json.Unmarshal(push.Payload, &submitted); err != nil {
			serverErrors <- err
			return
		}
		text := string(submitted.Payload)
		for _, forbidden := range []string{"secret", "/private", "products", "stderrPath", "error"} {
			if strings.Contains(text, forbidden) {
				serverErrors <- fmt.Errorf("payload leaks %q: %s", forbidden, text)
				return
			}
		}
		recorded := submitted
		recorded.MessageID = "event-recorded"
		recorded.Event = "run_event_recorded"
		if err := writePhoenixReply(ctx, connection, push, recorded); err != nil {
			serverErrors <- err
			return
		}
		_, _, _ = connection.Read(ctx)
	}))
	defer server.Close()
	client, _ := New(Options{Server: server.URL, HTTPClient: server.Client(), HeartbeatInterval: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	channel, _, err := client.OpenChannel(ctx, testShareID, testRunnerToken)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	event := execution.RunEvent{APIVersion: APIVersion, Kind: "RunEvent", RunID: testRunID, Sequence: 1, OccurredAt: testNow, Type: "step_failed", StepID: "build", Data: map[string]any{"status": "failed", "error": "secret /private/file", "products": map[string]any{"token": "secret"}, "stderrPath": "/private/stderr"}}
	if _, err := channel.SubmitRunEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serverErrors:
		t.Fatal(err)
	default:
	}
}
