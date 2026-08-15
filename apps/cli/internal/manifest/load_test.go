package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeStrictTypedManifest(t *testing.T) {
	document, err := Decode([]byte(validCapabilityYAML))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	capability, ok := document.(*Capability)
	if !ok {
		t.Fatalf("Decode() type = %T, want *Capability", document)
	}
	if capability.Metadata.Name != "release-build" || capability.Inputs["version"].Type != "string" {
		t.Fatalf("unexpected decoded Capability: %#v", capability)
	}
}

func TestDecodeRejectsUnknownFieldAndMultipleDocuments(t *testing.T) {
	tests := map[string]string{
		"unknown field":      strings.Replace(validCapabilityYAML, "outputs:", "surprise: true\noutputs:", 1),
		"multiple documents": validCapabilityYAML + "\n---\nkind: Capability\n",
		"unsupported kind":   strings.Replace(validCapabilityYAML, "kind: Capability", "kind: Playbook", 1),
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(source)); err == nil {
				t.Fatal("Decode() unexpectedly succeeded")
			}
		})
	}
}

func TestLoadRetainsSourceDirectoryAndDigest(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "capability.yaml")
	if err := os.WriteFile(path, []byte(validCapabilityYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Directory != directory {
		t.Fatalf("Directory = %q, want %q", loaded.Directory, directory)
	}
	if len(loaded.SHA256) != 64 {
		t.Fatalf("SHA256 = %q, want 64 hex characters", loaded.SHA256)
	}
}

func TestDecodeAndValidateMutableSpace(t *testing.T) {
	document, err := Decode([]byte(`apiVersion: doppels.so/v1alpha1
kind: Space
metadata:
  name: platform
  displayName: Platform
  labels: {environment: production}
`))
	if err != nil {
		t.Fatal(err)
	}
	space, ok := document.(*Space)
	if !ok || space.Metadata.Name != "platform" {
		t.Fatalf("Space = %#v", document)
	}
	loaded := Loaded{Path: "doppels.platform.yaml", Document: document}
	if diagnostics := Validate([]Loaded{loaded}, ValidationOptions{}).Diagnostics; len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if _, err := Decode([]byte(`apiVersion: doppels.so/v1alpha1
kind: Space
metadata: {name: platform, version: 1.0.0}
`)); err == nil {
		t.Fatal("Space metadata.version must be rejected")
	}
}

const validCapabilityYAML = `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata:
  name: release-build
  version: 1.0.0
  summary: Build a release.
inputs:
  version:
    type: string
    required: true
outputs:
  archive:
    type: artifact
    mediaType: application/gzip
  checksum:
    type: string
`

const validRecipeYAML = `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata:
  name: release-shell
  version: 1.0.0
provides: [release-build]
runtime: shell
requires:
  commands:
    - name: tar
      version: ">=1.2.0 <2.0.0"
  hostEnv: [SIGNING_KEY]
  files: [package.json]
defaults:
  approval: never
steps:
  - id: build
    name: Build archive
    env:
      VERSION: "{{ inputs.version }}"
      KEY:
        from: host_env
        name: SIGNING_KEY
    run:
      shell: sh
      script: |
        tar -czf "release-$VERSION.tgz" dist/
        export CHECKSUM=abc123
    produces:
      archive:
        file: "release-{{ inputs.version }}.tgz"
      checksum:
        env: CHECKSUM
returns:
  archive: "{{ steps.build.archive }}"
  checksum: "{{ steps.build.checksum }}"
`

func TestRecipeArtifactRetentionDaysOrDefault(t *testing.T) {
	t.Parallel()

	plain, err := Decode([]byte(validRecipeYAML))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	recipe := plain.(*Recipe)
	if got := recipe.ArtifactRetentionDaysOrDefault(); got != 7 {
		t.Fatalf("ArtifactRetentionDaysOrDefault() = %d, want 7", got)
	}

	withRetention := strings.Replace(
		validRecipeYAML,
		"defaults:\n  approval: never",
		"defaults:\n  approval: never\n  artifactRetentionDays: 14",
		1,
	)
	document, err := Decode([]byte(withRetention))
	if err != nil {
		t.Fatalf("Decode(withRetention) error = %v", err)
	}
	recipe = document.(*Recipe)
	if got := recipe.ArtifactRetentionDaysOrDefault(); got != 14 {
		t.Fatalf("ArtifactRetentionDaysOrDefault() = %d, want 14", got)
	}
}
