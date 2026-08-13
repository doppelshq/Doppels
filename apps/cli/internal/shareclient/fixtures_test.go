package shareclient

import (
	"encoding/json"
	"strings"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
)

const (
	testShareID     = "aa222d6a-0fc8-4dc1-b9b5-0e4673a2d7de"
	testRequestID   = "3e5853b0-51ab-4fef-b402-486e02146591"
	testRunID       = "7b8b7f76-7cac-4b79-b879-2d32dd41c9e7"
	testRunnerToken = "runner_7bQ3cDjYs6dM2hV9pL4wXa8nK1eF5tR0"
)

var testNow = time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

func testCapability() *manifest.Capability {
	return &manifest.Capability{
		TypeMeta: manifest.TypeMeta{APIVersion: manifest.APIVersion, Kind: "Capability"},
		Metadata: manifest.Metadata{Name: "release-preview", Version: "1.0.0", Summary: "Build a public release."},
		Inputs: map[string]manifest.InputContract{
			"count": {Type: "integer", Required: true},
		},
		Outputs: map[string]manifest.OutputContract{
			"archive": {Type: "artifact", MediaType: "application/gzip"},
		},
	}
}

func testCapabilityReference() execution.DefinitionReference {
	return execution.DefinitionReference{
		Name: "release-preview", Version: "1.0.0", ManifestSHA256: strings.Repeat("a", 64),
		Schema: execution.SchemaReference{ID: manifest.CapabilitySchemaID, SHA256: manifest.CapabilitySchemaSHA256},
	}
}

func testRecipeReference() execution.DefinitionReference {
	return execution.DefinitionReference{
		Name: "release-shell", Version: "1.0.0", ManifestSHA256: strings.Repeat("b", 64),
		Schema: execution.SchemaReference{ID: manifest.RecipeSchemaID, SHA256: manifest.RecipeSchemaSHA256},
	}
}

func testShare() Share {
	recipe := testRecipeReference()
	return Share{
		APIVersion: manifest.APIVersion, Kind: "Share", ID: testShareID,
		CreatedAt: testNow, ExpiresAt: testNow.Add(time.Hour),
		CapabilityRevision: testCapabilityReference(), Capability: testCapability(), Recipe: &recipe,
		SharedBy: execution.ActorReference{Kind: "identity", ID: "owner@example.test"}, RequestLimit: 1,
	}
}

func testRequest() execution.RequestRecord {
	return execution.RequestRecord{
		APIVersion: manifest.APIVersion, Kind: "Request", ID: testRequestID, CreatedAt: testNow,
		IdempotencyKey: "share:test:request:1", Origin: "share", Capability: testCapabilityReference(),
		Inputs:      map[string]any{"count": json.Number("9007199254740993")},
		RequestedBy: execution.ActorReference{Kind: "identity", ID: "requester@example.test"}, ShareID: testShareID,
	}
}

func testRun() execution.RunRecord {
	recipe := testRecipeReference()
	return execution.RunRecord{
		APIVersion: manifest.APIVersion, Kind: "Run", ID: testRunID, RequestID: testRequestID, CreatedAt: testNow.Add(time.Second),
		Capability: testCapabilityReference(), Recipe: &recipe, Inputs: map[string]any{"count": json.Number("9007199254740993")},
		Executor: execution.ActorReference{Kind: "identity", ID: "owner@example.test"}, NodeID: "local",
	}
}

func testPending() PendingState {
	request := testRequest()
	return PendingState{Share: testShare(), Request: &request, Events: []execution.RunEvent{}}
}
