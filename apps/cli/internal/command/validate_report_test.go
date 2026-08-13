package command

import (
	"strings"
	"testing"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/projectlock"
)

func TestValidateLockStatus(t *testing.T) {
	digest := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)
	locked := map[string]string{
		"capability/greet@1.0.0": digest,
	}
	cases := []struct {
		name   string
		locked map[string]string
		digest string
		want   string
	}{
		{name: "no-lock", locked: nil, digest: digest, want: "valid"},
		{name: "unchanged", locked: locked, digest: digest, want: "unchanged"},
		{name: "changed", locked: locked, digest: other, want: "changed"},
		{name: "new", locked: locked, digest: digest, want: "new"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, name, version := "Capability", "greet", "1.0.0"
			if tc.name == "new" {
				name = "other"
			}
			got := validateLockStatus(tc.locked, kind, name, version, tc.digest)
			if got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildValidateItemsReportsLockDrift(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", strings.Replace(
		runCapabilityFixture,
		"metadata: {name: greet, version: 1.0.0}",
		"metadata: {name: greet, version: 1.0.0, displayName: Greet}",
		1,
	))
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	paths, err := project.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	documents, diagnostics := load(paths)
	if len(diagnostics) != 0 {
		t.Fatalf("load diagnostics: %v", diagnostics)
	}
	result := manifest.Validate(documents, manifest.ValidationOptions{Root: root})
	if len(result.Diagnostics) != 0 {
		t.Fatalf("validate diagnostics: %v", result.Diagnostics)
	}

	capDef := result.Catalog.Capabilities["greet"][0]
	recipeDef := result.Catalog.Recipes["greet-shell"][0]
	capRef := execution.ReferenceCapability(capDef)
	recipeRef := execution.ReferenceRecipe(recipeDef)
	lock := projectlock.New([]projectlock.Entry{
		{Kind: "Capability", SourceAuthority: "manifest", Revision: capRef},
		{Kind: "Recipe", SourceAuthority: "manifest", Revision: withDigest(recipeRef, strings.Repeat("c", 64))},
	})

	items := buildValidateItems(root, documents, result.Catalog, &lock)
	byRef := map[string]validateItem{}
	for _, item := range items {
		byRef[validateResourceRef(item)] = item
	}
	if got := byRef["capability/greet@1.0.0"].Status; got != "unchanged" {
		t.Fatalf("capability status = %q, want unchanged", got)
	}
	if got := byRef["recipe/greet-shell@1.0.0"].Status; got != "changed" {
		t.Fatalf("recipe status = %q, want changed", got)
	}
	if byRef["capability/greet@1.0.0"].DisplayName != "Greet" {
		t.Fatalf("capability title = %q", byRef["capability/greet@1.0.0"].DisplayName)
	}
}

func TestWriteValidateReportNestsRecipesUnderCapability(t *testing.T) {
	var buf strings.Builder
	items := []validateItem{
		{Kind: "Capability", Name: "greet", Version: "1.0.0", DisplayName: "Greet", Status: "valid"},
		{Kind: "Recipe", Name: "greet-shell", Version: "1.0.0", DisplayName: "Greet shell", Status: "valid", Provides: []string{"greet"}},
		{Kind: "Recipe", Name: "greet-alt", Version: "1.0.0", DisplayName: "Greet alt", Status: "valid", Provides: []string{"greet"}},
	}
	docs := []manifest.Loaded{
		{Document: &manifest.Capability{}},
		{Document: &manifest.Recipe{}},
		{Document: &manifest.Recipe{}},
	}
	writeValidateReport(&buf, "/tmp/space", []string{"/tmp/space/capabilities/greet.yaml"}, docs, items)
	out := buf.String()
	if !strings.Contains(out, "capability/greet@1.0.0") {
		t.Fatalf("missing capability: %q", out)
	}
	if !strings.Contains(out, "├─ recipe/greet-alt@1.0.0") || !strings.Contains(out, "└─ recipe/greet-shell@1.0.0") {
		t.Fatalf("expected nested recipes: %q", out)
	}
	if !strings.Contains(out, "1 Capability") || !strings.Contains(out, "2 Recipes") {
		t.Fatalf("count line wrong: %q", out)
	}
	if strings.Contains(out, "\nrecipe/greet-shell@1.0.0") || strings.Contains(out, "\nrecipe/greet-alt@1.0.0") {
		t.Fatalf("recipe should be nested, not top-level: %q", out)
	}
}

func withDigest(ref execution.DefinitionReference, digest string) execution.DefinitionReference {
	ref.ManifestSHA256 = digest
	return ref
}
