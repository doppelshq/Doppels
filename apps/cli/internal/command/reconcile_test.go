package command

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPreviewAndApplyDiscoverProjectAndWriteLockAfterSuccess(t *testing.T) {
	var reconciles atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/v1/session" {
			fmt.Fprint(writer, `{"apiVersion":"doppels.so/v1alpha1","identity":{"kind":"identity","id":"owner"},"personal":{"organization":"owner","space":"private"}}`)
			return
		}
		if request.URL.Path != "/api/v1/organizations/acme/spaces/platform/preview" && request.URL.Path != "/api/v1/organizations/acme/spaces/platform/apply" {
			http.NotFound(writer, request)
			return
		}
		reconciles.Add(1)
		var body struct {
			Space struct {
				SourceAuthority string `json:"sourceAuthority"`
				Metadata        struct {
					DisplayName string `json:"displayName"`
				} `json:"metadata"`
			} `json:"space"`
			Resources []struct {
				Kind            string         `json:"kind"`
				SourceAuthority string         `json:"sourceAuthority"`
				ManifestSource  string         `json:"manifestSource"`
				Manifest        map[string]any `json:"manifest"`
				Revision        struct {
					ManifestSHA256 string `json:"manifestSha256"`
				} `json:"revision"`
			} `json:"resources"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Space.SourceAuthority != "manifest" || body.Space.Metadata.DisplayName != "Platform" {
			t.Fatalf("Space body = %#v", body.Space)
		}
		if len(body.Resources) != 2 {
			t.Fatalf("resources = %#v", body.Resources)
		}
		for _, resource := range body.Resources {
			if resource.SourceAuthority != "manifest" {
				t.Fatalf("resource integrity mismatch: %#v", resource)
			}
			if resource.Kind == "Capability" {
				digest := sha256.Sum256([]byte(resource.ManifestSource))
				if resource.ManifestSource == "" || hex.EncodeToString(digest[:]) != resource.Revision.ManifestSHA256 {
					t.Fatalf("Capability integrity mismatch: %#v", resource)
				}
				continue
			}
			if resource.Kind != "Recipe" || resource.ManifestSource != "" {
				t.Fatalf("Recipe leaked manifest source: %#v", resource)
			}
			for _, forbidden := range []string{"env", "defaults", "steps", "procedure", "evidence", "returns"} {
				if _, leaked := resource.Manifest[forbidden]; leaked {
					t.Fatalf("Recipe descriptor leaked %s: %#v", forbidden, resource.Manifest)
				}
			}
		}
		changes := `[{"action":"create","kind":"Space","name":"platform","reason":"missing"}]`
		if strings.HasSuffix(request.URL.Path, "/preview") {
			fmt.Fprintf(writer, `{"changes":%s,"applicable":true}`, changes)
		} else {
			fmt.Fprintf(writer, `{"changes":%s,"applied":true}`, changes)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	if err := os.MkdirAll(filepath.Join(root, ".doppels"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".doppels", "platform.space.yaml"), []byte(`apiVersion: doppels.so/v1alpha1
kind: Space
metadata:
  name: platform
  displayName: Platform
`), 0o600); err != nil {
		t.Fatal(err)
	}
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"DOPPELS_CONFIG_HOME=" + filepath.Join(root, "config")}
	app.HTTPClient = server.Client()
	if code := app.Run([]string{"login", "--server", server.URL, "--token", "token"}); code != ExitSuccess {
		t.Fatalf("login exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"org", "use", "acme"}); code != ExitSuccess {
		t.Fatalf("org use exit = %d", code)
	}
	if code := app.Run([]string{"space", "use", "platform"}); code != ExitSuccess {
		t.Fatalf("context exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"preview", "--json"}); code != ExitSuccess {
		t.Fatalf("preview exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"kind": "Preview"`) {
		t.Fatalf("preview json = %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(root, "doppels.lock")); !os.IsNotExist(err) {
		t.Fatalf("preview unexpectedly wrote lock: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"apply", "--json"}); code != ExitSuccess {
		t.Fatalf("apply exit = %d, stderr = %s", code, stderr.String())
	}
	lock, err := os.ReadFile(filepath.Join(root, "doppels.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lock), `"kind": "Capability"`) || !strings.Contains(string(lock), `"kind": "Recipe"`) {
		t.Fatalf("lock = %s", lock)
	}
	if reconciles.Load() != 2 {
		t.Fatalf("reconcile calls = %d", reconciles.Load())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"preview"}); code != ExitSuccess {
		t.Fatalf("human preview exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Cloud") || !strings.Contains(stdout.String(), server.URL) || !strings.Contains(stdout.String(), "Scope") || !strings.Contains(stdout.String(), "acme/platform") {
		t.Fatalf("human preview missing Cloud/Scope: %s", stdout.String())
	}
	if reconciles.Load() != 3 {
		t.Fatalf("reconcile calls after human preview = %d", reconciles.Load())
	}

	for filename, version := range map[string]string{"other-v1.yaml": "1.0.0", "other-v2.yaml": "2.0.0"} {
		writeManifest(t, root, "capabilities", filename, fmt.Sprintf(`apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: other, version: %s}
inputs: {}
outputs: {answer: {type: string}}
`, version))
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"preview"}); code != ExitContract {
		t.Fatalf("multi-revision preview exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `multiple local revisions of Capability "other"`) || reconciles.Load() != 3 {
		t.Fatalf("stderr/calls = %s / %d", stderr.String(), reconciles.Load())
	}
	for _, filename := range []string{"other-v1.yaml", "other-v2.yaml"} {
		if err := os.Remove(filepath.Join(root, ".doppels", "capabilities", filename)); err != nil {
			t.Fatal(err)
		}
	}

	// Reusing an immutable version with changed bytes is rejected locally before
	// another network request.
	capabilityPath := filepath.Join(root, ".doppels", "capabilities", "greet.yaml")
	data, err := os.ReadFile(capabilityPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capabilityPath, append(data, []byte("# changed\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"preview"}); code != ExitContract {
		t.Fatalf("changed preview exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "version bump") || reconciles.Load() != 3 {
		t.Fatalf("stderr/calls = %s / %d", stderr.String(), reconciles.Load())
	}
}
