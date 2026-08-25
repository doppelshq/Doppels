package command

import (
	"bytes"
	"io"
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
			Organization:   "acme",
			Space:          "payments-prod",
			Name:           "greet",
			Version:        "1.0.0",
			Label:          "greet@1.0.0",
			HasRecipe:      true,
			RecipeName:     "greet-shell@1.0.0",
			Mode:           "recipe",
			CapabilityPath: "capabilities/greet.yaml",
			RecipePath:     "recipes/greet-shell.yaml",
		}, {
			Organization:   "acme",
			Space:          "payments-prod",
			Name:           "manual-review",
			Version:        "1.0.0",
			Label:          "manual-review@1.0.0",
			Mode:           "manual",
			CapabilityPath: "capabilities/manual-review.yaml",
		}},
		Inbox: []listenInboxItemView{{
			ID:         "req_01aaaaaaaa",
			Capability: "greet@1.0.0",
			From:       "guest",
			Age:        "2m ago",
		}},
	})
	out := buf.String()
	for _, want := range []string{
		"Node online",
		"Node",
		"Turbo-2.local",
		"Identity",
		"yuri",
		"Scope",
		"acme",
		"Applied",
		"payments-prod — Payments Prod",
		"greet@1.0.0",
		"greet-shell@1.0.0",
		"capabilities/greet.yaml",
		"recipes/greet-shell.yaml",
		"manual-review@1.0.0",
		"Inbox",
		"req_01",
		"guest",
		"[a]pprove",
		"[r]eject",
		"[s]kip",
		"[b]ackground",
		"Ctrl-C stops this Node",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "└── payments-prod") && !strings.Contains(out, "├── payments-prod") {
		t.Fatalf("catalog space should be a tree parent:\n%s", out)
	}
	if !strings.Contains(out, "├── greet@1.0.0") && !strings.Contains(out, "└── greet@1.0.0") {
		t.Fatalf("catalog capability should be a tree leaf:\n%s", out)
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
		LocalTrees: []listenLocalTreeView{{
			Path:     "/Users/turbo/work/acme-app",
			Space:    "engineering",
			Branch:   "main",
			Worktree: "primary",
			Capabilities: []listenLocalCapabilityView{{
				Label: "greet@1.0.0",
				Path:  "capabilities/greet.yaml",
				Recipes: []listenLocalRecipeView{{
					Label: "greet-shell@1.0.0",
					Path:  "recipes/greet.yaml",
				}},
			}, {
				Label: "manual-review@1.0.0",
				Path:  "capabilities/manual-review.yaml",
			}},
		}, {
			Path:     "/Users/turbo/work/acme-app/.worktrees/hotfix",
			Space:    "engineering",
			Branch:   "hotfix/export",
			Worktree: "/Users/turbo/work/acme-app",
		}},
	})
	out := buf.String()
	for _, want := range []string{
		"Applied",
		"None of these local Capabilities are applied here.",
		"Local",
		"├── engineering",
		"├── greet@1.0.0",
		"└── manual-review@1.0.0",
		"└── engineering",
		"hotfix/export",
		"capabilities/greet.yaml",
		"recipes/greet.yaml",
		"Status",
		"Apply these Spaces into acme",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Local trees") {
		t.Fatalf("old trees heading still present:\n%s", out)
	}
	if strings.Contains(out, "space     engineering") {
		t.Fatalf("nested tree fields still present:\n%s", out)
	}
}

func TestWriteListenLocalTreesAnnotatesCatalog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeListenLocalTrees(&buf, newTermStyle(&buf), []listenLocalTreeView{{
		Path:  "/ws/private",
		Space: "private",
		Capabilities: []listenLocalCapabilityView{{
			Label:  "greet@1.0.0",
			Path:   "capabilities/greet.yaml",
			Origin: "pinned",
			Recipes: []listenLocalRecipeView{{
				Label:   "greet-with-approval@1.0.0",
				Path:    "recipes/greet-with-approval.yaml",
				Origin:  "stale",
				Runtime: "shell",
				Checked: true,
				Ready:   true,
			}, {
				Label:   "greet-with-shell@1.0.0",
				Path:    "recipes/greet.yaml",
				Origin:  "pinned",
				Runtime: "shell",
				Checked: true,
				Ready:   true,
			}},
		}, {
			Label:  "manual-review@1.0.0",
			Path:   "capabilities/manual-review.yaml",
			Origin: "pinned",
		}, {
			Label:  "release-pipeline@1.0.0",
			Path:   "capabilities/release-pipeline.yaml",
			Origin: "pinned",
			Recipes: []listenLocalRecipeView{{
				Label:   "release-pipeline-shell@1.0.0",
				Path:    "recipes/release-pipeline.yaml",
				Origin:  "pinned",
				Runtime: "shell",
				Checked: true,
				Ready:   false,
				Missing: []string{"command helm"},
			}},
		}},
	}})
	out := buf.String()
	for _, want := range []string{
		"private",
		"3 caps · 3 recipes · 1 blocked",
		"! greet-with-approval@1.0.0",
		"stale",
		"manual",
		"manual-review@1.0.0",
		"shell",
		"✓ greet-with-shell@1.0.0",
		"✗ release-pipeline-shell@1.0.0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "manual-review") && !strings.Contains(line, "manual") {
			t.Fatalf("manual Capability missing mode:\n%s", line)
		}
		if strings.Contains(line, "greet-with-approval") && !strings.Contains(line, "!") {
			t.Fatalf("stale Recipe missing warn mark:\n%s", line)
		}
		if strings.Contains(line, "greet-with-shell@1.0.0") && !strings.Contains(line, "shell") {
			t.Fatalf("Recipe missing runtime:\n%s", line)
		}
		if strings.Contains(line, "greet-with-shell@1.0.0") && strings.Contains(line, "!") {
			t.Fatalf("pinned Recipe should not warn:\n%s", line)
		}
	}
}

func TestWriteListenBannerEmptyOrgListsLocalCatalog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeListenBanner(&buf, listenScopeView{
		Header: listenHeader{
			Node:     "Turbo-2.local",
			Server:   "http://127.0.0.1:4000",
			Identity: "local-developer",
		},
		Organization: "local",
		LocalTrees: []listenLocalTreeView{{
			Path:     "/ws/finance",
			Space:    "finance",
			Branch:   "main",
			Worktree: "primary",
			Capabilities: []listenLocalCapabilityView{{
				Label: "close-month@1.1.0",
				Path:  "capabilities/close-month.yaml",
				Recipes: []listenLocalRecipeView{{
					Label:   "close-month-ledger@1.1.0",
					Path:    "recipes/close-month-ledger.yaml",
					Checked: true,
					Ready:   true,
				}},
			}, {
				Label: "sync-invoices@1.1.0",
				Path:  "capabilities/sync-invoices.yaml",
				Recipes: []listenLocalRecipeView{{
					Label:   "sync-invoices-stripe@1.1.0",
					Path:    "recipes/sync-invoices-stripe.yaml",
					Checked: true,
					Ready:   false,
					Missing: []string{"command stripe", "env STRIPE_API_KEY"},
				}},
			}},
		}},
	})
	out := buf.String()
	for _, want := range []string{
		"Local",
		"└── finance",
		"2 caps · 2 recipes · 1 blocked",
		"├── close-month@1.1.0",
		"capabilities/close-month.yaml",
		"✓",
		"close-month-ledger@1.1.0",
		"recipes/close-month-ledger.yaml",
		"✗",
		"command stripe",
		"env STRIPE_API_KEY",
		"└── sync-invoices@1.1.0",
		"Status",
		"not ready",
		"sync-invoices-stripe@1.1.0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	for _, not := range []string{
		"None in this Organization.",
		"Catalog empty",
		"doppels org use",
		"Applied",
		"not registered",
	} {
		if strings.Contains(out, not) {
			t.Fatalf("org local should not scare about an empty applied catalog; found %q in:\n%s", not, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "├──") && !strings.Contains(line, "└──") {
			continue
		}
		if strings.Contains(line, "sync-invoices-stripe") && strings.Contains(line, "command stripe") {
			t.Fatalf("missing requires should be nested, not inline: %q", line)
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
	}}, catalog, nil)
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
	writeListenDecisionPrompt(&buf, request, created, listenPromptQueue{
		Index:  1,
		Total:  3,
		Queued: []string{"release-build", "export-db"},
	}, now)
	out := buf.String()
	for _, want := range []string{
		"[1/3]",
		"greet@1.0.0",
		"guest-1",
		"audience  world",
		"queue  release-build · export-db",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestDecideFulfillmentAcceptsSkipAndBackground(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  fulfillDecision
	}{
		{"a\n", fulfillApprove},
		{"r\n", fulfillReject},
		{"s\n", fulfillSkip},
		{"b\n", fulfillBackground},
	}
	for _, tc := range cases {
		interaction := newInteraction(strings.NewReader(tc.input), io.Discard)
		got, err := interaction.decideFulfillment()
		if err != nil {
			t.Fatalf("input %q: %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("input %q: got %v want %v", tc.input, got, tc.want)
		}
	}
}

func strPtr(value string) *string { return &value }
