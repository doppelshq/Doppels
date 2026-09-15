// Package conformance drives the Doppels Runner v1 IPC (RFC 001) as an
// external client would: raw NDJSON frames over the real Unix Domain Socket
// transport, never through runnerclient. It exists to catch wire-contract
// regressions (envelope shape, error codes, version negotiation) that a
// same-process unit test exercising Manager/Service methods directly cannot
// see, and to harden the daemon lifecycle (SIGKILL recovery) and its
// concurrency invariants under adversarial load.
package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/runs"
	"doppels.so/cli/internal/runner/server"
	"doppels.so/cli/internal/runner/transport"
	"doppels.so/cli/internal/runner/workspace"
)

const conformanceToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"

// harness boots the real Runner stack (workspace.Service + runs.Manager +
// server.Server) behind a real Unix Domain Socket listener, exactly as
// cmd/doppels-runner wires it, so conformance tests exercise the same
// dispatch path a real client connects to.
type harness struct {
	t          *testing.T
	service    *workspace.Service
	manager    *runs.Manager
	srv        *server.Server
	socketPath string
	root       string
}

func newHarness(t *testing.T, withRecipe bool) *harness {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, ".doppels", "capabilities", "greet.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: greet, version: 1.0.0}
inputs:
  count: {type: integer, required: true}
outputs:
  value: {type: string}
`)
	if withRecipe {
		writeFixture(t, filepath.Join(root, ".doppels", "recipes", "greet.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: greet-shell, version: 1.0.0}
provides: [greet]
runtime: shell
defaults: {approval: never}
steps:
  - id: run
    name: Run
    run:
      shell: sh
      script: |
        export VALUE="ok:$DOPPEL_INPUT_COUNT"
    produces:
      value: {env: VALUE}
returns:
  value: "{{ steps.run.value }}"
`)
	}
	registry := workspace.NewRegistry(filepath.Join(base, "workspaces.json"))
	service := workspace.NewService(registry, workspace.Deps{Host: manifest.OSHost{}})
	if _, _, err := service.AddWorkspace(root); err != nil {
		t.Fatal(err)
	}

	manager := runs.NewManager(nil, service, runs.Config{
		NodeID: "node-conformance", Environment: []string{"PATH=" + os.Getenv("PATH")},
	})
	t.Cleanup(func() { _ = manager.Close() })

	srv := server.New(server.Config{
		Token: conformanceToken, RunnerVersion: "0.0.1-test",
		NodeStatus: func() proto.NodeStatus { return proto.NodeStatus{} },
		Log:        func(string, ...any) {},
	})
	workspace.RegisterRPC(srv, service)
	runs.RegisterRPC(srv, manager)

	socketPath := filepath.Join(t.TempDir(), "runner.sock")
	listener, err := transport.Unix{}.Listen(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(t.Context(), listener) }()
	t.Cleanup(srv.Close)

	return &harness{t: t, service: service, manager: manager, srv: srv, socketPath: socketPath, root: root}
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rawClient speaks the wire protocol exactly as an independent client
// implementation would: it never assumes anything about ordering beyond what
// the RFC promises, and it exposes both a convenience call() (request/response
// correlation, buffering out-of-band notifications) and direct frame-level
// access for malformed-frame tests.
type rawClient struct {
	t       *testing.T
	conn    interface{ Close() error }
	encoder *proto.Encoder
	decoder *proto.Decoder

	queued []queuedFrame
}

type queuedFrame struct {
	method string
	params json.RawMessage
}

func dial(t *testing.T, h *harness) *rawClient {
	t.Helper()
	conn, err := transport.Unix{}.Dial(h.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &rawClient{t: t, conn: conn, encoder: proto.NewEncoder(conn), decoder: proto.NewDecoder(conn)}
}

// dialInitialized dials and completes the v1 handshake with the conformance
// token, returning the client positioned to call any other method.
func dialInitialized(t *testing.T, h *harness) *rawClient {
	t.Helper()
	client := dial(t, h)
	response := client.call("init", "v1/initialize", map[string]any{
		"token":  conformanceToken,
		"client": map[string]any{"name": "conformance", "version": "0.0.1"},
	})
	if response.Err != nil {
		t.Fatalf("initialize: %+v", response.Err)
	}
	return client
}

func (c *rawClient) writeRaw(line string) {
	c.t.Helper()
	raw := c.conn.(interface{ Write([]byte) (int, error) })
	if _, err := raw.Write([]byte(line + "\n")); err != nil {
		c.t.Fatalf("write raw frame: %v", err)
	}
}

func (c *rawClient) readRawResponse() *proto.Response {
	c.t.Helper()
	frame, err := c.decoder.ReadFrame()
	if err != nil {
		c.t.Fatalf("read response: %v", err)
	}
	var response proto.Response
	if err := json.Unmarshal(frame, &response); err != nil {
		c.t.Fatalf("decode response %s: %v", frame, err)
	}
	return &response
}

func (c *rawClient) call(id, method string, params any) *proto.Response {
	c.t.Helper()
	if err := c.encoder.WriteFrame(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.t.Fatalf("write request: %v", err)
	}
	for {
		frame, err := c.decoder.ReadFrame()
		if err != nil {
			c.t.Fatalf("read response: %v", err)
		}
		var envelope struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			c.t.Fatalf("decode frame %s: %v", frame, err)
		}
		if envelope.Method != "" {
			c.queued = append(c.queued, queuedFrame{method: envelope.Method, params: envelope.Params})
			continue
		}
		var response proto.Response
		if err := json.Unmarshal(frame, &response); err != nil {
			c.t.Fatalf("decode response %s: %v", frame, err)
		}
		return &response
	}
}

// tryCall behaves like call but returns a nil response instead of failing
// the test on a read/write error — for races where the connection dying
// mid-flight (e.g. a server-wide shutdown) is itself a legitimate, expected
// outcome that must not be reported as a test failure by a goroutine other
// than the one running the test.
func (c *rawClient) tryCall(id, method string, params any) *proto.Response {
	if err := c.encoder.WriteFrame(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil
	}
	for {
		frame, err := c.decoder.ReadFrame()
		if err != nil {
			return nil
		}
		var envelope struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil
		}
		if envelope.Method != "" {
			continue
		}
		var response proto.Response
		if err := json.Unmarshal(frame, &response); err != nil {
			return nil
		}
		return &response
	}
}

func (c *rawClient) readNotification() (string, json.RawMessage) {
	c.t.Helper()
	if len(c.queued) > 0 {
		next := c.queued[0]
		c.queued = c.queued[1:]
		return next.method, next.params
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		frame, err := c.decoder.ReadFrame()
		if err != nil {
			c.t.Fatalf("read notification: %v", err)
		}
		var envelope struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			c.t.Fatal(err)
		}
		if envelope.Method == "" {
			continue
		}
		return envelope.Method, envelope.Params
	}
	c.t.Fatal("timed out waiting for a notification")
	return "", nil
}

func rawResult(t *testing.T, response *proto.Response) json.RawMessage {
	t.Helper()
	if response.Err != nil {
		t.Fatalf("unexpected error response: %+v", response.Err)
	}
	raw, ok := response.Result.(json.RawMessage)
	if !ok {
		t.Fatalf("response.Result = %#v, want json.RawMessage", response.Result)
	}
	return raw
}

// redactKeys names object keys whose values are inherently non-deterministic
// (temp-dir paths, generated ids, wall-clock timestamps) and therefore must
// be normalized to a fixed placeholder before two payloads can be compared
// byte-for-byte.
var redactKeys = map[string]bool{
	"runId": true, "requestId": true, "createdAt": true, "occurredAt": true,
	"finishedAt": true, "startedAt": true, "requestedAt": true,
	"workspace": true, "root": true, "path": true, "manifestSha256": true,
	"pong": true, "gitBranch": true,
}

func redact(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if redactKeys[key] {
				out[key] = "<REDACTED>"
				continue
			}
			out[key] = redact(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redact(item)
		}
		return out
	default:
		return v
	}
}

// goldenJSON canonicalizes raw (redacting non-deterministic fields, then
// re-encoding with encoding/json's map-key sort) so two independently
// produced payloads compare byte-for-byte regardless of the volatile ids,
// paths, and timestamps a live Runner necessarily produces.
func goldenJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode payload %s: %v", raw, err)
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(redact(value)); err != nil {
		t.Fatalf("re-encode canonical payload: %v", err)
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}

func assertGolden(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	if got := goldenJSON(t, raw); got != want {
		t.Fatalf("golden mismatch:\n got:  %s\n want: %s", got, want)
	}
}

func assertErrorResponse(t *testing.T, response *proto.Response, wantCode int) {
	t.Helper()
	if response.Err == nil {
		t.Fatalf("expected error, got success result %#v", response.Result)
	}
	if response.Err.Code != wantCode {
		t.Fatalf("error code = %d, want %d (%+v)", response.Err.Code, wantCode, response.Err)
	}
}

func idString(t *testing.T, response *proto.Response) string {
	t.Helper()
	encoded, err := json.Marshal(response.ID)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func waitForStatusOverSocket(t *testing.T, client *rawClient, runID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response := client.call(fmt.Sprintf("poll-%d", time.Now().UnixNano()), "v1/getRun", map[string]any{"runId": runID})
		if response.Err == nil {
			var result runs.GetRunResult
			if err := json.Unmarshal(rawResult(t, response), &result); err == nil && result.Summary.Status == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach status %s", runID, want)
}
