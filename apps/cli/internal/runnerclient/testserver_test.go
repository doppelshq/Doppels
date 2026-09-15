package runnerclient_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/runs"
	"doppels.so/cli/internal/runner/server"
	"doppels.so/cli/internal/runner/transport"
	"doppels.so/cli/internal/runner/workspace"
	"doppels.so/cli/internal/runnerclient"
)

const testToken = "runnerclient-test-token"

// testServer bundles a real server.Server + runs.Manager + Unix socket
// listener, matching the pattern in internal/runner/runs/rpc_integration_test.go.
type testServer struct {
	socketPath string
	root       string
	manager    *runs.Manager
	srv        *server.Server
	cancel     context.CancelFunc
}

func startTestServer(t *testing.T) *testServer {
	t.Helper()
	return startTestServerWith(t, false)
}

func startTestServerWithRecipe(t *testing.T) *testServer {
	t.Helper()
	return startTestServerWith(t, true)
}

func startTestServerWith(t *testing.T, withRecipe bool) *testServer {
	t.Helper()
	service, root := testWorkspace(t, withRecipe)
	manager := runs.NewManager(context.Background(), service, runs.Config{NodeID: "node-test"})

	srv := server.New(server.Config{
		Token: testToken, RunnerVersion: "0.0.1-test",
		Capabilities: []string{},
		NodeStatus:   func() proto.NodeStatus { return proto.NodeStatus{State: "online"} },
		Log:          func(string, ...any) {},
	})
	runs.RegisterRPC(srv, manager)

	socketPath := filepath.Join(t.TempDir(), "runner.sock")
	listener, err := transport.Unix{}.Listen(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx, listener)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { manager.Close() })

	return &testServer{socketPath: socketPath, root: root, manager: manager, srv: srv, cancel: cancel}
}

// testWorkspace mirrors runs.runnerWorkspace: a fresh .doppels/ project with
// a "greet" Capability, optionally backed by a shell Recipe.
func testWorkspace(t *testing.T, withRecipe bool) (*workspace.Service, string) {
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
	return service, root
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

func dialTest(t *testing.T, socketPath string) *runnerclient.Client {
	t.Helper()
	client, err := runnerclient.Dial(context.Background(), runnerclient.Options{
		SocketPath: socketPath,
		Token:      testToken,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}
