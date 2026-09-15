package runnerclient_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runnerclient"
)

func TestDialInitializesSuccessfully(t *testing.T) {
	ts := startTestServer(t)
	client := dialTest(t, ts.socketPath)

	if !client.HasCapability(proto.CapabilityLiveLogs) {
		t.Fatalf("expected liveLogs capability, got %v", client.Capabilities())
	}
	if client.NodeStatus().State != "online" {
		t.Fatalf("NodeStatus() = %+v, want state online", client.NodeStatus())
	}
}

func TestDialWrongTokenReturnsAuthFailed(t *testing.T) {
	ts := startTestServer(t)
	_, err := runnerclient.Dial(context.Background(), runnerclient.Options{
		SocketPath: ts.socketPath,
		Token:      "wrong-token",
	})
	if err == nil {
		t.Fatal("expected an error for a wrong token")
	}
	var rpcErr *proto.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want one wrapping *proto.Error", err)
	}
	if rpcErr.Code != proto.CodeAuthFailed {
		t.Fatalf("error code = %d, want %d", rpcErr.Code, proto.CodeAuthFailed)
	}
}

func TestDialNoSocketReturnsErrNotRunning(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.sock")
	_, err := runnerclient.Dial(context.Background(), runnerclient.Options{
		SocketPath: missing,
		Token:      "irrelevant",
	})
	if !errors.Is(err, runnerclient.ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}

func TestPingRoundTrip(t *testing.T) {
	ts := startTestServer(t)
	client := dialTest(t, ts.socketPath)

	pong, err := client.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if pong == "" {
		t.Fatal("Ping returned empty pong")
	}
}

func TestStartRunAndGetRun(t *testing.T) {
	ts := startTestServerWithRecipe(t)
	client := dialTest(t, ts.socketPath)
	ctx := context.Background()

	started, err := client.StartRun(ctx, runnerclient.StartRunParams{
		Workspace:      ts.root,
		Capability:     "greet",
		Inputs:         map[string]any{"count": 1},
		ApprovalMode:   "auto",
		IdempotencyKey: "test-start-1",
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if started.RunID == "" {
		t.Fatal("StartRun returned empty RunID")
	}

	deadline := time.Now().Add(5 * time.Second)
	var got runnerclient.GetRunResult
	for time.Now().Before(deadline) {
		got, err = client.GetRun(ctx, started.RunID, false)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Summary.Status == "succeeded" || got.Summary.Status == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Summary.Status != "succeeded" {
		t.Fatalf("Run status = %q, want succeeded (%+v)", got.Summary.Status, got.Summary)
	}
}

func TestNotificationDuringInFlightCallStillCompletes(t *testing.T) {
	ts := startTestServerWithRecipe(t)
	client := dialTest(t, ts.socketPath)
	ctx := context.Background()

	var mu sync.Mutex
	var notifications []runnerclient.Notification
	client.SetNotificationHandler(func(n runnerclient.Notification) {
		mu.Lock()
		notifications = append(notifications, n)
		mu.Unlock()
	})

	started, err := client.StartRun(ctx, runnerclient.StartRunParams{
		Workspace:      ts.root,
		Capability:     "greet",
		Inputs:         map[string]any{"count": 1},
		ApprovalMode:   "auto",
		IdempotencyKey: "test-notif-1",
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := client.SubscribeRun(ctx, started.RunID, 0); err != nil {
		t.Fatalf("SubscribeRun: %v", err)
	}

	// Interleave Pings with the run's own live event stream to exercise the
	// read-loop/pending-map correctness the RFC-8 spec calls out.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Ping(ctx); err != nil {
			t.Fatalf("Ping while notifications may be in flight: %v", err)
		}
		mu.Lock()
		n := len(notifications)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	n := len(notifications)
	mu.Unlock()
	if n == 0 {
		t.Fatal("expected at least one notification to have been dispatched")
	}
}

func TestConcurrentCallsMultiplexCorrectly(t *testing.T) {
	ts := startTestServer(t)
	client := dialTest(t, ts.socketPath)
	ctx := context.Background()

	const goroutines = 8
	const perGoroutine = 20
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*perGoroutine)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if _, err := client.Ping(ctx); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Ping failed: %v", err)
	}
}

func TestConcurrentGetRunCorrelatesResponses(t *testing.T) {
	ts := startTestServerWithRecipe(t)
	client := dialTest(t, ts.socketPath)
	ctx := context.Background()

	const runs = 6
	runIDs := make([]string, runs)
	for i := range runIDs {
		started, err := client.StartRun(ctx, runnerclient.StartRunParams{
			Workspace: ts.root, Capability: "greet",
			Inputs: map[string]any{"count": i}, ApprovalMode: "auto",
			IdempotencyKey: "concurrent-getrun-" + string(rune('a'+i)),
		})
		if err != nil {
			t.Fatalf("StartRun[%d]: %v", i, err)
		}
		runIDs[i] = started.RunID
	}

	var wg sync.WaitGroup
	errs := make(chan error, runs)
	for _, id := range runIDs {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := client.GetRun(ctx, id, false)
			if err != nil {
				errs <- err
				return
			}
			if got.Summary.RunID != id {
				errs <- errors.New("GetRun returned mismatched RunID: got " + got.Summary.RunID + " want " + id)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestErrorResponseSurfacesAndConnectionStaysUsable(t *testing.T) {
	ts := startTestServer(t)
	client := dialTest(t, ts.socketPath)
	ctx := context.Background()

	_, err := client.GetRun(ctx, "does-not-exist", false)
	if err == nil {
		t.Fatal("expected an error for an unknown runId")
	}
	var rpcErr *proto.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want *proto.Error", err)
	}
	if rpcErr.Code != proto.CodeRunNotFound {
		t.Fatalf("code = %d, want %d", rpcErr.Code, proto.CodeRunNotFound)
	}

	if _, err := client.Ping(ctx); err != nil {
		t.Fatalf("connection unusable after an error response: %v", err)
	}
}

func TestCloseFailsPendingCallsAndIsIdempotent(t *testing.T) {
	ts := startTestServer(t)
	client, err := runnerclient.Dial(context.Background(), runnerclient.Options{
		SocketPath: ts.socketPath, Token: testToken,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	_, err = client.Ping(context.Background())
	if !errors.Is(err, runnerclient.ErrClosed) {
		t.Fatalf("Ping after Close = %v, want ErrClosed", err)
	}
}
