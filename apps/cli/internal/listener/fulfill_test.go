package listener

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/registryclient"
	"doppels.so/cli/internal/shareclient"
)

func writeSpaceProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	capDir := filepath.Join(root, ".doppels", "capabilities")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capDir, "sync-invoices.yaml"), []byte(`apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: sync-invoices, version: 1.1.0}
inputs: {}
outputs: {ok: {type: string}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	recipeDir := filepath.Join(root, ".doppels", "recipes")
	if err := os.MkdirAll(recipeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recipeDir, "sync-invoices.yaml"), []byte(`apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: sync-invoices, version: 1.1.0}
provides: [sync-invoices]
runtime: shell
requires:
  commands: [sh]
defaults: {approval: never}
steps:
  - id: run
    name: Mock
    run:
      shell: sh
      script: |
        export OK=ok
    produces:
      ok: {env: OK}
returns:
  ok: "{{ steps.run.ok }}"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func loadCatalog(t *testing.T, root string) *manifest.Catalog {
	t.Helper()
	paths, err := project.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	var documents []manifest.Loaded
	for _, path := range paths {
		loaded, err := manifest.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, loaded)
	}
	validation := manifest.Validate(documents, manifest.ValidationOptions{Root: root, CheckHost: false})
	if len(validation.Diagnostics) > 0 {
		for _, diagnostic := range validation.Diagnostics {
			t.Log(diagnostic.Error())
		}
		t.Fatalf("catalog validation failed")
	}
	return validation.Catalog
}

// recordingRegistry accepts decide and ingest calls, recording paths.
type recordingRegistry struct {
	mu      sync.Mutex
	decides []string
	ingests int
}

func (r *recordingRegistry) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case strings.HasSuffix(request.URL.Path, "/decide"):
		r.decides = append(r.decides, request.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"request": map[string]any{"id": "req-1"}})
	case strings.HasSuffix(request.URL.Path, "/runs/ingest"):
		r.ingests++
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"unexpected path"}`)
	}
}

func (r *recordingRegistry) decideCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.decides)
}

func (r *recordingRegistry) ingestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ingests
}

// autoPort auto-approves everything: the headless equivalent of --yes.
type autoPort struct{}

func (autoPort) DecideFulfillment(context.Context, Job, QueueInfo) (Decision, error) {
	return DecisionApprove, nil
}

func (autoPort) ApproveStep(context.Context, execution.ApprovalRequest) (bool, error) {
	return true, nil
}

func (autoPort) PickRecipe(_ context.Context, _ string, matches []manifest.RecipeDefinition) (manifest.RecipeDefinition, error) {
	return matches[0], nil
}

func (autoPort) FulfillManual(context.Context, execution.ManualRequest) (execution.ManualResult, error) {
	return execution.ManualResult{}, nil
}

func spaceJob(capability execution.DefinitionReference) Job {
	request := &execution.RequestRecord{
		APIVersion:     execution.APIVersion,
		Kind:           "Request",
		ID:             "8e7b1e94-9f3c-4a2b-8c21-0b1a2c3d4e5f",
		CreatedAt:      time.Now().UTC(),
		IdempotencyKey: "local:8e7b1e94-9f3c-4a2b-8c21-0b1a2c3d4e5f",
		Origin:         "api",
		Capability:     capability,
		Inputs:         map[string]any{},
		RequestedBy:    execution.ActorReference{Kind: "identity", ID: "identity-9"},
		Organization:   "acme",
		Space:          "infra",
	}
	return Job{Origin: "space", Request: request, Organization: "acme", Space: "infra"}
}

func TestFulfillSpaceRunsRecipeAndFlushesOutbox(t *testing.T) {
	root := writeSpaceProject(t)
	catalog := loadCatalog(t, root)

	registry := &recordingRegistry{}
	server := httptest.NewServer(registry)
	t.Cleanup(server.Close)
	reg, err := registryclient.New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}

	draft := spaceJob(execution.DefinitionReference{Name: "sync-invoices", Version: "1.1.0"})
	capability, recipe, resolveErr := ResolveSpaceFulfillment(catalog, *draft.Request, autoPort{}, true)
	if resolveErr != nil {
		t.Fatalf("resolve: %v", resolveErr)
	}
	if capability.Value == nil || capability.Value.Metadata.Name != "sync-invoices" {
		t.Fatalf("capability = %#v", capability.Value)
	}
	if recipe == nil || recipe.Value.Metadata.Name != "sync-invoices" {
		t.Fatalf("recipe = %#v", recipe)
	}

	var events []execution.RunEvent
	cfg := SpaceFulfillConfig{
		Registry:    reg,
		Token:       "token-1",
		Identity:    "identity-1",
		NodeID:      "node-1",
		Root:        root,
		Catalog:     catalog,
		Approvals:   autoPort{},
		ApproveAll:  true,
		Output:      io.Discard,
		Err:         io.Discard,
		Environment: os.Environ(),
		Now:         func() time.Time { return time.Now().UTC() },
		OnEvent: func(_ context.Context, event execution.RunEvent) error {
			events = append(events, event)
			return nil
		},
	}
	result, runErr := FulfillSpace(context.Background(), cfg, spaceJob(execution.ReferenceCapability(capability)), capability, recipe)
	if runErr != nil {
		t.Fatalf("fulfill: %v", runErr)
	}
	if result.Status != "succeeded" {
		t.Fatalf("status = %s (events=%v)", result.Status, eventTypes(events))
	}
	if result.Returns["ok"] != "ok" {
		t.Fatalf("returns = %#v", result.Returns)
	}
	if registry.decideCount() != 1 {
		t.Fatalf("decide calls = %d, want 1 (auto-approve before run)", registry.decideCount())
	}
	if registry.ingestCount() != 1 {
		t.Fatalf("ingest calls = %d, want 1 (outbox flush after run)", registry.ingestCount())
	}
}

func TestFulfillSpaceSkipsDecideWhenOperatorAlreadyApproved(t *testing.T) {
	root := writeSpaceProject(t)
	catalog := loadCatalog(t, root)

	registry := &recordingRegistry{}
	server := httptest.NewServer(registry)
	t.Cleanup(server.Close)
	reg, err := registryclient.New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}

	job := spaceJob(execution.DefinitionReference{Name: "sync-invoices", Version: "1.1.0"})
	job.Request.OperatorDecision = "approve"
	capability, recipe, resolveErr := ResolveSpaceFulfillment(catalog, *job.Request, autoPort{}, true)
	if resolveErr != nil {
		t.Fatalf("resolve: %v", resolveErr)
	}
	job.Request.Capability = execution.ReferenceCapability(capability)
	cfg := SpaceFulfillConfig{
		Registry: reg, Token: "token-1", Identity: "identity-1", NodeID: "node-1",
		Root: root, Catalog: catalog, Approvals: autoPort{}, ApproveAll: true,
		Output: io.Discard, Err: io.Discard, Environment: os.Environ(),
		Now: func() time.Time { return time.Now().UTC() },
	}
	if _, runErr := FulfillSpace(context.Background(), cfg, job, capability, recipe); runErr != nil {
		t.Fatalf("fulfill: %v", runErr)
	}
	if registry.decideCount() != 0 {
		t.Fatalf("decide calls = %d, want 0 (already approved)", registry.decideCount())
	}
}

func TestResolveSpaceFulfillmentMissingCapability(t *testing.T) {
	root := writeSpaceProject(t)
	catalog := loadCatalog(t, root)
	job := spaceJob(execution.DefinitionReference{Name: "unknown-cap", Version: "1.1.0"})
	job.Request.Capability.Name = "unknown-cap"
	if _, _, err := ResolveSpaceFulfillment(catalog, *job.Request, autoPort{}, true); err == nil {
		t.Fatal("expected error for missing capability")
	}
}

func TestResolveShareFulfillmentDigestMismatch(t *testing.T) {
	root := writeSpaceProject(t)
	catalog := loadCatalog(t, root)
	share := shareclient.Share{
		ID: "share-1",
		CapabilityRevision: execution.DefinitionReference{
			Name:           "sync-invoices",
			Version:        "1.1.0",
			ManifestSHA256: "deadbeef",
		},
	}
	if _, _, err := ResolveShareFulfillment(catalog, share, autoPort{}, true); err == nil {
		t.Fatal("expected digest mismatch error")
	}
}

func eventTypes(events []execution.RunEvent) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

// Compile-time: the real registry client satisfies SpaceFulfillConfig.Registry.
var _ SpaceRegistry = (*registryclient.Client)(nil)
