package runnerclient

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"doppels.so/cli/internal/runner/proto"
)

// TestCloseFailsInFlightPendingCall is a white-box test (package
// runnerclient, not runnerclient_test): it constructs a Client directly over
// a net.Pipe whose "server" side never answers, so Call is guaranteed to
// still be pending in c.pending when Close runs. The public integration
// tests in client_test.go cover Close against a real server, but a real
// server always answers fast enough that a genuinely in-flight Call is hard
// to guarantee deterministically.
func TestCloseFailsInFlightPendingCall(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()

	client := &Client{
		conn:     clientConn,
		encoder:  proto.NewEncoder(clientConn),
		decoder:  proto.NewDecoder(clientConn),
		pending:  make(map[string]chan pendingResult),
		readDone: make(chan struct{}),
	}
	go client.readLoop()

	// Drain (never answer) whatever the client writes.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		buf := make([]byte, 4096)
		for {
			if _, err := serverConn.Read(buf); err != nil {
				return
			}
		}
	}()

	callDone := make(chan error, 1)
	go func() {
		callDone <- client.Call(context.Background(), "v1/ping", map[string]any{}, nil)
	}()

	select {
	case <-callDone:
		t.Fatal("Call returned before Close; it never had a chance to be pending")
	case <-time.After(50 * time.Millisecond):
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	select {
	case err := <-callDone:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("in-flight Call error = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight Call never returned after Close")
	}
	<-drained
}
