package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/runner/proto"
)

type fakeLogSubscriber struct {
	mu     sync.Mutex
	chunks []proto.RunLogChunk
	notify chan struct{}
	closed func()
}

func newFakeLogSubscriber() *fakeLogSubscriber {
	return &fakeLogSubscriber{notify: make(chan struct{}, 64)}
}

func (f *fakeLogSubscriber) DeliverRunLog(chunk proto.RunLogChunk) bool {
	f.mu.Lock()
	f.chunks = append(f.chunks, chunk)
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
	return true
}

func (f *fakeLogSubscriber) Defer(onActivate func(), _ func()) { onActivate() }
func (f *fakeLogSubscriber) NotifyClosed(fn func()) func() {
	f.closed = fn
	return func() { f.closed = nil }
}

func (f *fakeLogSubscriber) snapshot() []proto.RunLogChunk {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proto.RunLogChunk(nil), f.chunks...)
}

func TestLiveLogStreamRedactsSecretsByteForByte(t *testing.T) {
	t.Setenv("RUNNER_LIVE_SECRET", "secret-suffix")
	service, root := runnerWorkspace(t, true)
	writeRunFixture(t, filepath.Join(root, ".doppels", "recipes", "greet.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: greet-shell, version: 1.0.0}
provides: [greet]
runtime: shell
requires:
  hostEnv: [RUNNER_LIVE_SECRET]
defaults: {approval: never}
steps:
  - id: run
    name: Run
    env:
      SECRET:
        from: host_env
        name: RUNNER_LIVE_SECRET
    run:
      shell: sh
      script: |
        touch live-log-ready
        sleep 0.2
        printf 'prefix-secr'
        sleep 0.05
        printf 'et-suffix\n'
        export VALUE=ok
    produces: {value: {env: VALUE}}
returns: {value: "{{ steps.run.value }}"}
`)
	manager := NewManager(context.Background(), service, Config{
		NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH"), "RUNNER_LIVE_SECRET=secret-suffix"},
	})
	defer manager.Close()
	started, rpcErr := manager.Start("desktop", []byte(`{"workspace":`+quote(root)+`,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"live-redaction"}`))
	if rpcErr != nil {
		t.Fatalf("Start: %+v", rpcErr)
	}
	waitForFile(t, filepath.Join(root, "live-log-ready"))
	sub := newFakeLogSubscriber()
	result, rpcErr := manager.SubscribeRunLogs(started.RunID, "run", sub)
	if rpcErr != nil || !result.Active {
		t.Fatalf("SubscribeRunLogs = %#v, %+v", result, rpcErr)
	}
	waitForStatus(t, root, started.RunID, "succeeded")

	var live bytes.Buffer
	for _, chunk := range sub.snapshot() {
		if chunk.Stream == string(execution.LogStreamStdout) {
			live.WriteString(chunk.Data)
		}
	}
	disk, err := os.ReadFile(filepath.Join(root, ".doppels", "runs", started.RunID, "logs", "run.stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(live.Bytes(), disk) {
		t.Fatalf("live = %q, disk = %q", live.Bytes(), disk)
	}
	if bytes.Contains(live.Bytes(), []byte("secret-suffix")) || !bytes.Contains(live.Bytes(), []byte("[REDACTED]")) {
		t.Fatalf("live stream was not redacted: %q", live.Bytes())
	}
}

func TestLiveLogStreamCapsAtSixteenMebibytes(t *testing.T) {
	broadcaster := newLiveLogBroadcaster("run-id")
	broadcaster.stepStarted("step-id")
	sub := newFakeLogSubscriber()
	broadcaster.addSubscriber("", sub)

	broadcaster.write(execution.LogStreamStdout, bytes.Repeat([]byte("x"), execution.DefaultLogStreamLimit+1024))
	before := sub.snapshot()
	broadcaster.write(execution.LogStreamStdout, []byte("must-be-dropped"))
	after := sub.snapshot()
	if len(after) != len(before) {
		t.Fatalf("frames after cap = %d, want %d", len(after), len(before))
	}
	var combined bytes.Buffer
	for _, chunk := range before {
		combined.WriteString(chunk.Data)
	}
	if combined.Len() != execution.DefaultLogStreamLimit {
		t.Fatalf("streamed bytes = %d, want %d", combined.Len(), execution.DefaultLogStreamLimit)
	}
	if len(before) == 0 || !before[len(before)-1].Truncated {
		t.Fatalf("last chunk = %#v, want truncated", before[len(before)-1])
	}
}

func TestLiveLogStreamSplitsOversizedFrames(t *testing.T) {
	broadcaster := newLiveLogBroadcaster("run-id")
	broadcaster.stepStarted("step-id")
	sub := newFakeLogSubscriber()
	broadcaster.addSubscriber("", sub)
	broadcaster.write(execution.LogStreamStdout, bytes.Repeat([]byte("x"), 6<<20))

	chunks := sub.snapshot()
	if len(chunks) != 2 {
		t.Fatalf("frames = %d, want 2", len(chunks))
	}
	for index, chunk := range chunks {
		frame, err := json.Marshal(proto.NewNotification("v1/runLog", chunk))
		if err != nil {
			t.Fatal(err)
		}
		if len(frame) > proto.MaxFrameBytes {
			t.Fatalf("frame %d = %d bytes, max %d", index, len(frame), proto.MaxFrameBytes)
		}
	}
	if chunks[0].Truncated || !chunks[1].Truncated {
		t.Fatalf("truncated flags = [%v %v], want [false true]", chunks[0].Truncated, chunks[1].Truncated)
	}
}

func TestLiveLogStreamPreservesUTF8SplitAcrossWrites(t *testing.T) {
	broadcaster := newLiveLogBroadcaster("run-id")
	broadcaster.stepStarted("step-id")
	sub := newFakeLogSubscriber()
	broadcaster.addSubscriber("", sub)

	want := []byte("😀")
	broadcaster.write(execution.LogStreamStdout, want[:2])
	broadcaster.write(execution.LogStreamStdout, want[2:])

	var got bytes.Buffer
	for _, chunk := range sub.snapshot() {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		var wireChunk proto.RunLogChunk
		if err := json.Unmarshal(encoded, &wireChunk); err != nil {
			t.Fatal(err)
		}
		got.WriteString(wireChunk.Data)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("wire bytes = %x, want original bytes %x", got.Bytes(), want)
	}
}

func TestLiveLogTrailingBytesFlushedOnStepEnd(t *testing.T) {
	broadcaster := newLiveLogBroadcaster("run-id")
	broadcaster.stepStarted("step-id")
	sub := newFakeLogSubscriber()
	broadcaster.addSubscriber("", sub)

	want := []byte("😀")
	broadcaster.write(execution.LogStreamStdout, want[:2])
	broadcaster.stepEnded("step-id")

	var got bytes.Buffer
	for _, chunk := range sub.snapshot() {
		if chunk.Stream != string(execution.LogStreamStdout) {
			continue
		}
		got.WriteString(chunk.Data)
	}
	if !bytes.Equal(got.Bytes(), want[:2]) {
		t.Fatalf("bytes delivered to subscriber after step end = %x, want %x", got.Bytes(), want[:2])
	}
}

func waitForLogSubscriberCount(t *testing.T, manager *Manager, runID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		manager.mu.Lock()
		active := manager.active[runID]
		manager.mu.Unlock()
		if active != nil && active.logs != nil && active.logs.subscriberCount() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d live-log subscribers", want)
}
