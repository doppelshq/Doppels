package manifest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeHost struct {
	commands map[string]string
	versions map[string]string
	env      map[string]string
	stat     func(string) (fs.FileInfo, error)
}

func (h fakeHost) LookupCommand(name string) (string, error) {
	path, ok := h.commands[name]
	if !ok {
		return "", errors.New("not found")
	}
	return path, nil
}
func (h fakeHost) LookupEnv(name string) (string, bool) { value, ok := h.env[name]; return value, ok }
func (h fakeHost) Stat(path string) (fs.FileInfo, error) {
	if h.stat != nil {
		return h.stat(path)
	}
	return os.Stat(path)
}
func (h fakeHost) CommandVersion(path string) (string, error) { return h.versions[path], nil }

func mustLoaded(t *testing.T, path, source string) Loaded {
	t.Helper()
	document, err := Decode([]byte(source))
	if err != nil {
		t.Fatalf("Decode(%s): %v", path, err)
	}
	return Loaded{Path: path, Directory: filepath.Dir(path), SHA256: strings.Repeat("a", 64), Document: document}
}

func codes(diagnostics []Diagnostic) map[string]bool {
	result := map[string]bool{}
	for _, diagnostic := range diagnostics {
		result[diagnostic.Code] = true
	}
	return result
}

func TestValidateCapabilityRecipeAndHost(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	documents := []Loaded{
		mustLoaded(t, filepath.Join(root, "capability.yaml"), validCapabilityYAML),
		mustLoaded(t, filepath.Join(root, "recipe.yaml"), validRecipeYAML),
	}
	host := fakeHost{
		commands: map[string]string{"tar": "/bin/tar", "sh": "/bin/sh"},
		versions: map[string]string{"/bin/tar": "tar 1.35.0"},
		env:      map[string]string{"SIGNING_KEY": "secret"},
	}
	result := Validate(documents, ValidationOptions{Root: root, CheckHost: true, Host: host})
	if len(result.Diagnostics) != 0 {
		t.Fatalf("Validate() diagnostics:\n%v", result.Diagnostics)
	}
	if result.Catalog.Root != root || len(result.Catalog.RecipesForCapability("release-build")) != 1 {
		t.Fatalf("unexpected Catalog: %#v", result.Catalog)
	}
}

func TestRecipeSemanticFailures(t *testing.T) {
	tests := []struct {
		name   string
		change func(string) string
		code   string
	}{
		{"unknown capability", func(value string) string { return strings.Replace(value, "[release-build]", "[missing]", 1) }, "recipe.unknown-capability"},
		{"approval unresolved", func(value string) string { return strings.Replace(value, "defaults:\n  approval: never\n", "", 1) }, "approval.unresolved"},
		{"script interpolation", func(value string) string {
			return strings.Replace(value, `tar -czf "release-$VERSION.tgz"`, `tar -czf "release-{{ inputs.version }}.tgz"`, 1)
		}, "script.interpolation"},
		{"unknown input", func(value string) string { return strings.Replace(value, "inputs.version", "inputs.channel", 1) }, "expression.unknown-input"},
		{"missing output", func(value string) string {
			return strings.Replace(value, `  checksum: "{{ steps.build.checksum }}"`+"\n", "", 1)
		}, "return.missing-output"},
		{"wrong artifact product", func(value string) string {
			return strings.Replace(value, "file: \"release-{{ inputs.version }}.tgz\"", "env: ARCHIVE", 1)
		}, "return.artifact"},
		{"undeclared host env", func(value string) string { return strings.Replace(value, "  hostEnv: [SIGNING_KEY]\n", "", 1) }, "environment.undeclared-host-env"},
		{"obsolete expression", func(value string) string {
			return strings.Replace(value, "{{ inputs.version }}", "${{ inputs.version }}", 1)
		}, "expression.obsolete"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			documents := []Loaded{
				mustLoaded(t, "capability.yaml", validCapabilityYAML),
				mustLoaded(t, "recipe.yaml", test.change(validRecipeYAML)),
			}
			result := Validate(documents, ValidationOptions{Root: "."})
			if !codes(result.Diagnostics)[test.code] {
				t.Fatalf("missing diagnostic %q in %#v", test.code, result.Diagnostics)
			}
		})
	}
}

func TestStepReferencesMustPointBackward(t *testing.T) {
	recipe := strings.Replace(validRecipeYAML, "steps:\n", `steps:
  - id: prepare
    name: Prepare
    env:
      FUTURE: "{{ steps.build.checksum }}"
    run:
      shell: sh
      script: echo prepare
`, 1)
	result := Validate([]Loaded{
		mustLoaded(t, "capability.yaml", validCapabilityYAML),
		mustLoaded(t, "recipe.yaml", recipe),
	}, ValidationOptions{Root: "."})
	if !codes(result.Diagnostics)["expression.unavailable-step"] {
		t.Fatalf("expected forward-reference diagnostic, got %#v", result.Diagnostics)
	}
}

func TestValidateManualRecipeAndCapabilityWithoutRecipe(t *testing.T) {
	manual := `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: release-human, version: 1.0.0}
provides: [release-build]
runtime: manual
procedure: {readme: ./runbook.md}
evidence:
  notes: {type: string}
`
	for name, documents := range map[string][]Loaded{
		"without recipe": {mustLoaded(t, "capability.yaml", validCapabilityYAML)},
		"manual recipe":  {mustLoaded(t, "capability.yaml", validCapabilityYAML), mustLoaded(t, "manual.yaml", manual)},
	} {
		t.Run(name, func(t *testing.T) {
			result := Validate(documents, ValidationOptions{Root: "."})
			if len(result.Diagnostics) != 0 {
				t.Fatalf("diagnostics = %#v", result.Diagnostics)
			}
		})
	}
}

func TestCapabilityRejectsNullDefaultAndEmptyEnum(t *testing.T) {
	capability := strings.Replace(validCapabilityYAML, "    required: true", "    required: true\n    default: null\n    enum: []", 1)
	if _, err := Decode([]byte(capability)); err == nil || !strings.Contains(err.Error(), "/inputs/version/default") || !strings.Contains(err.Error(), "/inputs/version/enum") {
		t.Fatalf("expected exact schema errors for null default and empty enum, got %v", err)
	}
}

func TestHostRequirementFailures(t *testing.T) {
	host := fakeHost{
		commands: map[string]string{"tar": "/bin/tar", "sh": "/bin/sh"},
		versions: map[string]string{"/bin/tar": "tar 2.5.0"},
		env:      map[string]string{},
		stat:     func(string) (fs.FileInfo, error) { return nil, os.ErrNotExist },
	}
	result := Validate([]Loaded{
		mustLoaded(t, "capability.yaml", validCapabilityYAML),
		mustLoaded(t, "recipe.yaml", validRecipeYAML),
	}, ValidationOptions{Root: ".", CheckHost: true, Host: host})
	got := codes(result.Diagnostics)
	for _, code := range []string{"host.version-mismatch", "host.env-missing", "host.file-missing"} {
		if !got[code] {
			t.Errorf("missing %s in %#v", code, result.Diagnostics)
		}
	}
}

func TestMultipleCapabilitiesRequireCommonCompatibleContract(t *testing.T) {
	secondCapability := strings.NewReplacer(
		"name: release-build", "name: release-publish",
		"required: true", "required: false",
	).Replace(validCapabilityYAML)
	recipe := strings.Replace(validRecipeYAML, "[release-build]", "[release-build, release-publish]", 1)
	result := Validate([]Loaded{
		mustLoaded(t, "first.yaml", validCapabilityYAML),
		mustLoaded(t, "second.yaml", secondCapability),
		mustLoaded(t, "recipe.yaml", recipe),
	}, ValidationOptions{Root: "."})
	got := codes(result.Diagnostics)
	if !got["contract.incompatible-input"] || !got["expression.unknown-input"] {
		t.Fatalf("expected incompatible shared input diagnostics, got %#v", result.Diagnostics)
	}
}

func TestCatalogRecipeResolutionIsDeterministic(t *testing.T) {
	capability := mustLoaded(t, "capability.yaml", validCapabilityYAML)
	recipeA := mustLoaded(t, "a.yaml", validRecipeYAML)
	recipeBSource := strings.Replace(validRecipeYAML, "name: release-shell", "name: release-bash", 1)
	recipeB := mustLoaded(t, "b.yaml", recipeBSource)

	catalog := NewCatalog(".", []Loaded{capability})
	if _, err := catalog.ResolveRecipe("release-build", ""); !errors.Is(err, ErrRecipeNotFound) {
		t.Fatalf("ResolveRecipe() error = %v, want ErrRecipeNotFound", err)
	}
	catalog = NewCatalog(".", []Loaded{capability, recipeA})
	if selected, err := catalog.ResolveRecipe("release-build", ""); err != nil || selected.Value.Metadata.Name != "release-shell" {
		t.Fatalf("ResolveRecipe() = %#v, %v", selected, err)
	}
	catalog = NewCatalog(".", []Loaded{capability, recipeA, recipeB})
	if _, err := catalog.ResolveRecipe("release-build", ""); !errors.Is(err, ErrRecipeAmbiguous) {
		t.Fatalf("ResolveRecipe() error = %v, want ErrRecipeAmbiguous", err)
	}
	if selected, err := catalog.ResolveRecipe("release-build", "release-bash"); err != nil || selected.Value.Metadata.Name != "release-bash" {
		t.Fatalf("explicit ResolveRecipe() = %#v, %v", selected, err)
	}
}

func TestVersionConstraints(t *testing.T) {
	version, ok := extractSemver("node v22.4.1")
	if !ok {
		t.Fatal("extractSemver() failed")
	}
	for constraint, wanted := range map[string]bool{
		">=22.0.0 <26.0.0": true,
		"^22.1.0":          true,
		"~22.4.0":          true,
		"<22.0.0":          false,
	} {
		got, err := satisfiesConstraint(version, constraint)
		if err != nil || got != wanted {
			t.Errorf("satisfiesConstraint(%q) = %t, %v; want %t", constraint, got, err, wanted)
		}
	}
}
