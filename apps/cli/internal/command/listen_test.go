package command

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/registryclient"
	"doppels.so/cli/internal/shareclient"
)

func TestWriteListenBanner(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeListenBanner(&buf, listenScopeView{
		Header: listenHeader{
			Node:     "Turbo-2.local",
			Server:   "http://localhost:4000",
			Identity: "yuri",
		},
		Organization: "acme",
		Scopes: []registryclient.ListenScope{{
			Organization: "acme",
			Space:        "payments-prod",
			DisplayName:  strPtr("Payments Prod"),
		}},
		Capabilities: []listenCapabilityView{{
			Organization: "acme",
			Space:        "payments-prod",
			Name:         "greet",
			Version:      "1.0.0",
			Label:        "greet@1.0.0",
			HasRecipe:    true,
			RecipeName:   "greet-shell@1.0.0",
			Mode:         "recipe",
		}, {
			Organization: "acme",
			Space:        "payments-prod",
			Name:         "manual-review",
			Version:      "1.0.0",
			Label:        "manual-review@1.0.0",
			Mode:         "manual",
		}},
	})
	out := buf.String()
	for _, want := range []string{
		"Listening",
		"Node",
		"Turbo-2.local",
		"Org",
		"acme",
		"Spaces",
		"payments-prod — Payments Prod",
		"Ready to fulfill",
		"greet@1.0.0",
		"recipe",
		"greet-shell@1.0.0",
		"manual-review@1.0.0",
		"manual",
		"no Recipe",
		"Waiting for Requests in acme",
		"Press Ctrl-C to quit.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "greet@1.0.0") {
			continue
		}
		spaceIdx := strings.Index(line, "payments-prod")
		capIdx := strings.Index(line, "greet@1.0.0")
		if spaceIdx < 0 || capIdx < 0 || spaceIdx > capIdx {
			t.Fatalf("space should be first column in fulfill line: %q", line)
		}
	}
	if strings.Contains(out, "Capabilities (local)") {
		t.Fatalf("old capabilities label still present:\n%s", out)
	}
	if strings.Contains(out, "personal/") {
		t.Fatalf("unexpected multi-org noise:\n%s", out)
	}
}

func TestWriteListenBannerEmptyExplainsLocalMismatch(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeListenBanner(&buf, listenScopeView{
		Header: listenHeader{
			Node:     "Turbo-2.local",
			Server:   "http://localhost:4000",
			Identity: "yuri",
		},
		Organization:      "acme",
		Scopes:            []registryclient.ListenScope{{Organization: "acme", Space: "engineering"}},
		LocalCapabilities: []string{"greet@1.0.0", "manual-review@1.0.0"},
	})
	out := buf.String()
	for _, want := range []string{
		"Ready to fulfill",
		"None — local Capabilities are not registered in these Spaces.",
		"Local: greet@1.0.0, manual-review@1.0.0",
		"both here and in Org Spaces",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestBuildListenCapabilitiesMarksRecipe(t *testing.T) {
	t.Parallel()
	catalog := manifest.NewCatalog(".", []manifest.Loaded{
		{Document: &manifest.Capability{Metadata: manifest.Metadata{Name: "greet", Version: "1.0.0"}}},
		{Document: &manifest.Capability{Metadata: manifest.Metadata{Name: "manual-review", Version: "1.0.0"}}},
		{Document: &manifest.Recipe{
			Metadata: manifest.Metadata{Name: "greet-shell", Version: "1.0.0"},
			Provides: []string{"greet"},
		}},
	})
	views := buildListenCapabilities([]registryclient.ListenScope{{
		Organization: "acme",
		Space:        "platform",
		Capabilities: []registryclient.ListenCapability{
			{Name: "greet", Version: "1.0.0"},
			{Name: "manual-review", Version: "1.0.0"},
			{Name: "missing-elsewhere", Version: "1.0.0"},
		},
	}}, catalog)
	if len(views) != 2 {
		t.Fatalf("views = %#v", views)
	}
	byName := map[string]listenCapabilityView{}
	for _, view := range views {
		byName[view.Name] = view
	}
	if !byName["greet"].HasRecipe || byName["greet"].RecipeName != "greet-shell@1.0.0" {
		t.Fatalf("greet view = %#v", byName["greet"])
	}
	if byName["manual-review"].HasRecipe || byName["manual-review"].Mode != "manual" {
		t.Fatalf("manual view = %#v", byName["manual-review"])
	}
}

func TestResolveListenFiltersDefaultsOrg(t *testing.T) {
	t.Parallel()
	got, err := resolveListenFilters(nil, listenHeader{Organization: "acme"}, listenFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Organization != "acme" {
		t.Fatalf("got %#v", got)
	}
	_, err = resolveListenFilters(nil, listenHeader{}, listenFilters{})
	if err == nil {
		t.Fatal("expected error without org")
	}
}

func TestWriteListenDecisionPrompt(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	now := time.Date(2026, 8, 3, 16, 0, 0, 0, time.UTC)
	request := execution.RequestRecord{
		ID:          "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		RequestedBy: execution.ActorReference{Kind: "guest", ID: "guest-1"},
		Capability:  execution.DefinitionReference{Name: "greet", Version: "1.0.0"},
		Inputs:      map[string]any{"audience": "world", "dryRun": true, "count": float64(3)},
	}
	created := &shareclient.ShareCreated{
		Share: shareclient.Share{
			ID:        "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
			ExpiresAt: now.Add(45 * time.Minute),
		},
	}
	writeListenDecisionPrompt(&buf, request, created, 2, now)
	out := buf.String()
	for _, want := range []string{
		"Request  greet@1.0.0",
		"guest-1",
		"audience  world",
		"2 Requests queued",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func strPtr(value string) *string { return &value }
