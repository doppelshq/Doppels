package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/server"
)

type fakeRPCServer struct {
	handlers map[string]server.Handler
	events   []proto.NodeEvent
	onEvent  func(proto.NodeEvent)
}

type orderedRPCServer struct {
	handlers   map[string]server.Handler
	addEntered chan struct{}
	releaseAdd chan struct{}
	addOnce    sync.Once

	mu     sync.Mutex
	events []string
}

func (s *orderedRPCServer) Handle(method string, handler server.Handler) {
	if s.handlers == nil {
		s.handlers = make(map[string]server.Handler)
	}
	s.handlers[method] = handler
}

func (s *orderedRPCServer) EmitNodeEvent(event proto.NodeEvent) {
	if event.Kind == proto.NodeEventWorkspaceAdded {
		s.addOnce.Do(func() { close(s.addEntered) })
		<-s.releaseAdd
	}
	s.mu.Lock()
	s.events = append(s.events, event.Kind)
	s.mu.Unlock()
}

func (f *fakeRPCServer) Handle(method string, handler server.Handler) {
	if f.handlers == nil {
		f.handlers = make(map[string]server.Handler)
	}
	f.handlers[method] = handler
}

func (f *fakeRPCServer) EmitNodeEvent(event proto.NodeEvent) {
	if f.onEvent != nil {
		f.onEvent(event)
	}
	f.events = append(f.events, event)
}

func rpcParams(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRegisterRPCWorkspaceAndCapabilityFlow(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "space-a"))
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "v1.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: cap-a, version: 1.0.0}
inputs: {}
outputs: {ok: {type: string}}
`)
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "v2.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: cap-a, version: 2.0.0}
inputs: {}
outputs: {ok: {type: string}}
`)
	registryPath := filepath.Join(base, "workspaces.json")
	service := NewService(NewRegistry(registryPath), Deps{})
	target := &fakeRPCServer{}
	target.onEvent = func(event proto.NodeEvent) {
		persisted := NewRegistry(registryPath)
		if err := persisted.Load(); err != nil {
			t.Fatalf("event emitted before readable persistence: %v", err)
		}
		canonical, err := Canonicalize(root)
		if err != nil {
			t.Fatal(err)
		}
		switch event.Kind {
		case proto.NodeEventWorkspaceAdded:
			if !persisted.Contains(canonical) {
				t.Fatal("workspaceAdded emitted before add was persisted")
			}
		case proto.NodeEventWorkspaceRemoved:
			if persisted.Contains(canonical) {
				t.Fatal("workspaceRemoved emitted before remove was persisted")
			}
		}
	}

	RegisterRPC(target, service)
	for _, method := range []string{"v1/listWorkspaces", "v1/addWorkspace", "v1/removeWorkspace", "v1/listCapabilities", "v1/getCapability"} {
		if target.handlers[method] == nil {
			t.Fatalf("handler %s was not registered", method)
		}
	}

	result, rpcErr := target.handlers["v1/addWorkspace"](rpcParams(t, map[string]any{"root": root}))
	if rpcErr != nil {
		t.Fatalf("addWorkspace error = %+v", rpcErr)
	}
	summary, ok := result.(proto.WorkspaceSummary)
	if !ok || summary.Capabilities != 2 || !filepath.IsAbs(summary.Root) {
		t.Fatalf("addWorkspace result = %#v", result)
	}
	if len(target.events) != 1 || target.events[0].Kind != proto.NodeEventWorkspaceAdded {
		t.Fatalf("events = %+v, want one workspaceAdded", target.events)
	}

	if _, rpcErr := target.handlers["v1/addWorkspace"](rpcParams(t, map[string]any{"root": root})); rpcErr != nil {
		t.Fatalf("idempotent addWorkspace error = %+v", rpcErr)
	}
	if len(target.events) != 1 {
		t.Fatalf("idempotent add emitted event: %+v", target.events)
	}
	result, rpcErr = target.handlers["v1/listWorkspaces"](rpcParams(t, map[string]any{}))
	workspaces, ok := result.([]proto.WorkspaceSummary)
	if rpcErr != nil || !ok || len(workspaces) != 1 || workspaces[0].Root != summary.Root {
		t.Fatalf("listWorkspaces result = %#v, err = %+v", result, rpcErr)
	}

	result, rpcErr = target.handlers["v1/listCapabilities"](rpcParams(t, map[string]any{}))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	capabilities, ok := result.([]proto.CapabilitySummary)
	if !ok || len(capabilities) != 2 || capabilities[0].Version != "1.0.0" || capabilities[1].Version != "2.0.0" {
		t.Fatalf("listCapabilities result = %#v", result)
	}

	_, rpcErr = target.handlers["v1/getCapability"](rpcParams(t, map[string]any{"workspace": root, "name": "cap-a"}))
	if rpcErr == nil || rpcErr.Code != proto.CodeCapabilityNotFound {
		t.Fatalf("ambiguous getCapability error = %+v, want -32004", rpcErr)
	}
	data, ok := rpcErr.Data.(map[string]any)
	versions, versionsOK := data["versions"].([]string)
	if !ok || !versionsOK || len(versions) != 2 || versions[0] != "1.0.0" || versions[1] != "2.0.0" {
		t.Fatalf("ambiguity data = %#v, want sorted versions", rpcErr.Data)
	}

	result, rpcErr = target.handlers["v1/getCapability"](rpcParams(t, map[string]any{"workspace": root, "name": "cap-a", "version": "2.0.0"}))
	capability, ok := result.(Capability)
	if rpcErr != nil || !ok || capability.Manifest.Metadata.Version != "2.0.0" || capability.Inputs == nil || capability.Outputs == nil {
		t.Fatalf("getCapability result = %#v, err = %+v", result, rpcErr)
	}

	result, rpcErr = target.handlers["v1/removeWorkspace"](rpcParams(t, map[string]any{"root": root}))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if empty, ok := result.(map[string]any); !ok || len(empty) != 0 {
		t.Fatalf("removeWorkspace result = %#v, want {}", result)
	}
	if len(target.events) != 2 || target.events[1].Kind != proto.NodeEventWorkspaceRemoved {
		t.Fatalf("events = %+v, want workspaceRemoved", target.events)
	}
	if _, err := os.Stat(filepath.Join(root, ".doppels")); err != nil {
		t.Fatalf("removeWorkspace touched workspace contents: %v", err)
	}
}

func TestRegisterRPCMapsParamsAndDomainErrors(t *testing.T) {
	service, base := newService(t)
	root := initRoot(t, filepath.Join(base, "registered"))
	if _, _, err := service.AddWorkspace(root); err != nil {
		t.Fatal(err)
	}
	target := &fakeRPCServer{}
	RegisterRPC(target, service)

	tests := []struct {
		name   string
		method string
		params []byte
		code   int
	}{
		{name: "malformed params", method: "v1/addWorkspace", params: []byte(`{"root":`), code: proto.CodeInvalidParams},
		{name: "missing add root", method: "v1/addWorkspace", params: rpcParams(t, map[string]any{}), code: proto.CodeInvalidParams},
		{name: "invalid add root", method: "v1/addWorkspace", params: rpcParams(t, map[string]any{"root": filepath.Join(base, "missing")}), code: proto.CodeWorkspaceNotFound},
		{name: "unknown remove root", method: "v1/removeWorkspace", params: rpcParams(t, map[string]any{"root": filepath.Join(base, "missing")}), code: proto.CodeWorkspaceNotFound},
		{name: "unknown capability workspace", method: "v1/listCapabilities", params: rpcParams(t, map[string]any{"workspace": filepath.Join(base, "missing")}), code: proto.CodeWorkspaceNotFound},
		{name: "missing capability name", method: "v1/getCapability", params: rpcParams(t, map[string]any{"workspace": root}), code: proto.CodeInvalidParams},
		{name: "unknown capability version", method: "v1/getCapability", params: rpcParams(t, map[string]any{"workspace": root, "name": "missing", "version": "9.0.0"}), code: proto.CodeCapabilityNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, rpcErr := target.handlers[tt.method](tt.params)
			if rpcErr == nil || rpcErr.Code != tt.code {
				t.Fatalf("error = %+v, want code %d", rpcErr, tt.code)
			}
		})
	}
	if len(target.events) != 0 {
		t.Fatalf("failed mutations emitted events: %+v", target.events)
	}
}

func TestRegisterRPCMapsMissingRegisteredRootToWorkspaceNotFound(t *testing.T) {
	service, base := newService(t)
	root := initRoot(t, filepath.Join(base, "registered"))
	if _, _, err := service.AddWorkspace(root); err != nil {
		t.Fatal(err)
	}
	target := &fakeRPCServer{}
	RegisterRPC(target, service)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	for _, request := range []struct {
		method string
		params map[string]any
	}{
		{method: "v1/listCapabilities", params: map[string]any{"workspace": root}},
		{method: "v1/getCapability", params: map[string]any{"workspace": root, "name": "missing"}},
	} {
		_, rpcErr := target.handlers[request.method](rpcParams(t, request.params))
		if rpcErr == nil || rpcErr.Code != proto.CodeWorkspaceNotFound {
			t.Fatalf("%s error = %+v, want workspaceNotFound", request.method, rpcErr)
		}
	}
}

func TestRegisterRPCDoesNotEmitEventWhenPersistenceFails(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "workspace"))
	registryPath := filepath.Join(base, "workspaces.json")
	if err := os.Mkdir(registryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(registryPath)
	target := &fakeRPCServer{}
	RegisterRPC(target, NewService(registry, Deps{}))

	_, rpcErr := target.handlers["v1/addWorkspace"](rpcParams(t, map[string]any{"root": root}))
	if rpcErr == nil || rpcErr.Code != proto.CodeInternal {
		t.Fatalf("addWorkspace error = %+v, want internal persistence failure", rpcErr)
	}
	if len(target.events) != 0 || len(registry.Roots()) != 0 {
		t.Fatalf("failed persistence changed state: events=%+v roots=%v", target.events, registry.Roots())
	}
}

func TestRegisterRPCPublishesConcurrentMutationsInCommitOrder(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "workspace"))
	service := NewService(NewRegistry(filepath.Join(base, "workspaces.json")), Deps{})
	target := &orderedRPCServer{addEntered: make(chan struct{}), releaseAdd: make(chan struct{})}
	RegisterRPC(target, service)

	addDone := make(chan *proto.Error, 1)
	go func() {
		_, rpcErr := target.handlers["v1/addWorkspace"](rpcParams(t, map[string]any{"root": root}))
		addDone <- rpcErr
	}()
	select {
	case <-target.addEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("add did not reach blocked publication")
	}

	removeDone := make(chan *proto.Error, 1)
	go func() {
		_, rpcErr := target.handlers["v1/removeWorkspace"](rpcParams(t, map[string]any{"root": root}))
		removeDone <- rpcErr
	}()
	select {
	case rpcErr := <-removeDone:
		if rpcErr != nil {
			t.Fatalf("removeWorkspace error = %+v", rpcErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remove blocked while add notification was broadcasting")
	}
	close(target.releaseAdd)
	if rpcErr := <-addDone; rpcErr != nil {
		t.Fatalf("addWorkspace error = %+v", rpcErr)
	}

	target.mu.Lock()
	events := append([]string(nil), target.events...)
	target.mu.Unlock()
	if len(events) != 2 || events[0] != proto.NodeEventWorkspaceAdded || events[1] != proto.NodeEventWorkspaceRemoved {
		t.Fatalf("events = %v, want commit order [workspaceAdded workspaceRemoved]", events)
	}
}

func TestRegisterRPCEventPublicationAllowsReentrantMutation(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "workspace"))
	service := NewService(NewRegistry(filepath.Join(base, "workspaces.json")), Deps{})
	target := &fakeRPCServer{}
	RegisterRPC(target, service)

	var removeErr *proto.Error
	var once sync.Once
	target.onEvent = func(event proto.NodeEvent) {
		if event.Kind != proto.NodeEventWorkspaceAdded {
			return
		}
		once.Do(func() {
			_, removeErr = target.handlers["v1/removeWorkspace"](rpcParams(t, map[string]any{"root": root}))
		})
	}

	if _, rpcErr := target.handlers["v1/addWorkspace"](rpcParams(t, map[string]any{"root": root})); rpcErr != nil {
		t.Fatalf("addWorkspace error = %+v", rpcErr)
	}
	if removeErr != nil {
		t.Fatalf("reentrant removeWorkspace error = %+v", removeErr)
	}
	if len(target.events) != 2 || target.events[0].Kind != proto.NodeEventWorkspaceAdded || target.events[1].Kind != proto.NodeEventWorkspaceRemoved {
		t.Fatalf("events = %+v, want add then reentrant remove", target.events)
	}
	if roots := service.registry.Roots(); len(roots) != 0 {
		t.Fatalf("roots = %v, want reentrant removal persisted", roots)
	}
}
