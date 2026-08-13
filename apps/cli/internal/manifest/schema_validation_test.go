package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEmbeddedSchemasMatchRepositoryByteForByte(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	repository := filepath.Clean(filepath.Join(filepath.Dir(filename), "../../../.."))
	for schemaFilename := range embeddedSchemaFiles {
		canonical, err := os.ReadFile(filepath.Join(repository, "schemas", schemaFilename))
		if err != nil {
			t.Fatal(err)
		}
		embedded, err := embeddedSchemaFS.ReadFile("schemas/" + schemaFilename)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(embedded, canonical) {
			t.Fatalf("embedded %s differs from the canonical published schema", schemaFilename)
		}
	}
}

func TestPublishedSchemaBundlesResolveExactClosure(t *testing.T) {
	resources, err := embeddedSchemaResources()
	if err != nil {
		t.Fatal(err)
	}
	for rootID, wanted := range map[string]string{
		CapabilitySchemaID: CapabilitySchemaID + "," + CommonSchemaID,
		RecipeSchemaID:     CommonSchemaID + "," + RecipeSchemaID,
		SpaceSchemaID:      CommonSchemaID + "," + SpaceSchemaID,
	} {
		bundle, err := schemaBundle(rootID, resources)
		if err != nil {
			t.Fatalf("schemaBundle(%s): %v", rootID, err)
		}
		if got := schemaResourceIDs(bundle); got != wanted {
			t.Fatalf("schemaBundle(%s) = %s, want %s", rootID, got, wanted)
		}
	}
}

func TestCommonSchemaBytesAffectEveryDependentBundleDigest(t *testing.T) {
	resources, err := embeddedSchemaResources()
	if err != nil {
		t.Fatal(err)
	}
	changed := make(map[string]*schemaResource, len(resources))
	for id, resource := range resources {
		copy := *resource
		copy.Source = append([]byte(nil), resource.Source...)
		changed[id] = &copy
	}
	changed[CommonSchemaID].Source = append(changed[CommonSchemaID].Source, '\n')
	for _, rootID := range []string{CapabilitySchemaID, RecipeSchemaID, SpaceSchemaID} {
		before, err := schemaBundleSHA256(rootID, resources)
		if err != nil {
			t.Fatal(err)
		}
		after, err := schemaBundleSHA256(rootID, changed)
		if err != nil {
			t.Fatal(err)
		}
		if before == after {
			t.Fatalf("%s digest did not fix transitive common schema bytes", rootID)
		}
	}
}

func TestSchemaBundleRejectsUnfixedExternalReferencesAndCycles(t *testing.T) {
	const (
		rootID       = "https://example.test/root.json"
		dependencyID = "https://example.test/dependency.json"
	)
	resource := func(source string) *schemaResource {
		t.Helper()
		parsed, err := parseSchemaResource([]byte(source))
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	root := resource(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"` + rootID + `","$ref":"` + dependencyID + `"}`)
	if _, err := schemaBundle(rootID, map[string]*schemaResource{rootID: root}); err == nil || !strings.Contains(err.Error(), "unfixed external resource") {
		t.Fatalf("missing external reference error = %v", err)
	}
	dependency := resource(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"` + dependencyID + `","$ref":"` + rootID + `"}`)
	if _, err := schemaBundle(rootID, map[string]*schemaResource{rootID: root, dependencyID: dependency}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v", err)
	}
}

func TestDecodeRejectsSchemaPresenceAndLengthViolations(t *testing.T) {
	tests := map[string]string{
		"capability optional object null":  strings.Replace(validCapabilityYAML, "inputs:", "documentation: null\ninputs:", 1),
		"capability optional string empty": strings.Replace(validCapabilityYAML, "  summary: Build a release.", `  displayName: ""`, 1),
		"recipe optional object null": strings.Replace(validRecipeYAML, `requires:
  commands:
    - name: tar
      version: ">=1.2.0 <2.0.0"
  hostEnv: [SIGNING_KEY]
  files: [package.json]
`, "requires: null\n", 1),
		"space optional map null": `apiVersion: doppels.so/v1alpha1
kind: Space
metadata:
  name: platform
  labels: null
`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(source)); err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("Decode() error = %v", err)
			}
		})
	}
}

func TestManifestPathsArePortablePOSIXRelative(t *testing.T) {
	valid := strings.Replace(validCapabilityYAML, "inputs:", "documentation:\n  readme: ./docs/README.md\ninputs:", 1)
	if _, err := Decode([]byte(valid)); err != nil {
		t.Fatalf("valid POSIX path: %v", err)
	}
	if !validRelativePath("./docs/README.md") {
		t.Fatal("typed validator rejected a valid POSIX relative path")
	}
	for name, path := range map[string]string{
		"absolute":         "/docs/README.md",
		"parent traversal": "docs/../README.md",
		"windows drive":    "C:/docs/README.md",
		"backslash":        `docs\README.md`,
	} {
		t.Run(name, func(t *testing.T) {
			if validRelativePath(path) {
				t.Fatalf("typed validator accepted path %q", path)
			}
			source := strings.Replace(validCapabilityYAML, "inputs:", "documentation:\n  readme: '"+path+"'\ninputs:", 1)
			if _, err := Decode([]byte(source)); err == nil {
				t.Fatalf("Decode() accepted path %q", path)
			}
		})
	}
}

func TestCapabilityIntegerDefaultsAndEnumsUsePortableRange(t *testing.T) {
	input := func(defaultValue, enumValue string) string {
		return strings.Replace(validCapabilityYAML, `    type: string
    required: true`, `    type: integer
    required: true
    default: `+defaultValue+`
    enum: [`+enumValue+`]`, 1)
	}
	for _, value := range []string{"-9007199254740991", "9007199254740991"} {
		if _, err := Decode([]byte(input(value, value))); err != nil {
			t.Fatalf("portable boundary %s rejected: %v", value, err)
		}
	}
	for _, value := range []string{"-9007199254740992", "9007199254740992"} {
		if _, err := Decode([]byte(input(value, value))); err == nil {
			t.Fatalf("out-of-range integer %s accepted", value)
		}
	}
}

func TestCapabilityIntegralNumbersUsePortableRange(t *testing.T) {
	input := func(defaultValue string) string {
		return strings.Replace(validCapabilityYAML, `    type: string
    required: true`, `    type: number
    required: true
    default: `+defaultValue, 1)
	}
	for _, value := range []string{"-9007199254740991", "1.5", "9007199254740991"} {
		if _, err := Decode([]byte(input(value))); err != nil {
			t.Fatalf("portable number %s rejected: %v", value, err)
		}
	}
	for _, value := range []string{"-9007199254740992", "9007199254740992", "1e20"} {
		if _, err := Decode([]byte(input(value))); err == nil {
			t.Fatalf("non-portable integral number %s accepted", value)
		}
	}
}

func TestYAMLSubsetRejectsAmbiguousOrNonJSONScalars(t *testing.T) {
	withDefault := func(value string) string {
		return strings.Replace(validCapabilityYAML, "    required: true", "    required: true\n    default: "+value, 1)
	}
	withDisplayName := func(value string) string {
		return strings.Replace(validCapabilityYAML, "  summary: Build a release.", "  displayName: "+value+"\n  summary: Build a release.", 1)
	}
	tests := map[string]struct {
		source  string
		message string
	}{
		"leading-zero integer": {withDefault("0123"), "JSON number grammar"},
		"hex integer":          {withDefault("0x10"), "JSON number grammar"},
		"underscored integer":  {withDefault("1_000"), "JSON number grammar"},
		"positive sign":        {withDefault("+1"), "JSON number grammar"},
		"infinity":             {withDefault(".inf"), "JSON number grammar"},
		"ambiguous boolean":    {withDisplayName("yes"), "must be quoted"},
		"ambiguous on":         {withDisplayName("ON"), "must be quoted"},
		"sexagesimal":          {withDisplayName("12:34"), "must be quoted"},
		"timestamp":            {withDisplayName("2026-08-02"), "outside the JSON-compatible subset"},
		"alternative null":     {withDisplayName("~"), "lowercase null"},
		"explicit tag":         {withDisplayName("!!str release"), "explicit YAML tags"},
		"anchor":               {withDisplayName("&label release"), "anchors and aliases"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(test.source)); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Decode() error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestYAMLSubsetRejectsAliasesMergeKeysAndDuplicateKeys(t *testing.T) {
	tests := map[string]string{
		"alias": `apiVersion: doppels.so/v1alpha1
kind: Space
metadata:
  name: &name platform
  displayName: *name
`,
		"merge": `apiVersion: doppels.so/v1alpha1
kind: Space
metadata:
  <<: &base
    name: platform
`,
		"duplicate": `apiVersion: doppels.so/v1alpha1
kind: Space
metadata:
  name: platform
  name: duplicate
`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(source)); err == nil {
				t.Fatal("Decode() unexpectedly accepted non-deterministic YAML")
			}
		})
	}
}

func TestYAMLSubsetPreservesQuotedAmbiguousStringsCommentsAndBlockScalars(t *testing.T) {
	capability := strings.Replace(validCapabilityYAML, "  summary: Build a release.", "  displayName: \"yes\"\n  summary: \"2026-08-02\" # quoted strings stay strings", 1)
	if _, err := Decode([]byte(capability)); err != nil {
		t.Fatalf("quoted strings: %v", err)
	}
	if _, err := Decode([]byte(validRecipeYAML + "# trailing comment\n")); err != nil {
		t.Fatalf("comments and block script: %v", err)
	}
}

func TestMetadataVersionIsLimitedToOneHundredCharacters(t *testing.T) {
	validVersion := "1.0.0-" + strings.Repeat("a", 94)
	invalidVersion := validVersion + "a"
	if len(validVersion) != 100 || len(invalidVersion) != 101 {
		t.Fatal("test versions have unexpected lengths")
	}
	if _, err := Decode([]byte(strings.Replace(validCapabilityYAML, "version: 1.0.0", "version: "+validVersion, 1))); err != nil {
		t.Fatalf("100-character version rejected: %v", err)
	}
	if _, err := Decode([]byte(strings.Replace(validCapabilityYAML, "version: 1.0.0", "version: "+invalidVersion, 1))); err == nil {
		t.Fatal("101-character version accepted")
	}
	if diagnostics := metadataDiagnostics("capability.yaml", Metadata{Name: "release-build", Version: invalidVersion}); !codes(diagnostics)["metadata.version"] {
		t.Fatalf("typed validator accepted 101-character version: %#v", diagnostics)
	}
}
