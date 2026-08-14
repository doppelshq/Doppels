package command

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishUploadsCapabilityAndRecipeSource(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/v1/session" {
			io.WriteString(writer, `{"apiVersion":"doppels.so/v1alpha1","identity":{"kind":"identity","id":"owner"},"personal":{"organization":"owner","space":"private"}}`)
			return
		}
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/organizations/acme/hub/publications" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(request.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		writer.WriteHeader(http.StatusCreated)
		io.WriteString(writer, `{"organization":"acme","capabilityName":"greet","capabilityVersion":"1.0.0","capabilitySummary":"Say hello","recipeName":"greet-shell","recipeVersion":"1.0.0","status":"listed","publicPath":"/@acme/greet","capabilitySource":"cap","recipeSource":"rec"}`)
	}))
	defer server.Close()

	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", strings.Replace(runCapabilityFixture, "metadata: {name: greet, version: 1.0.0}", "metadata: {name: greet, version: 1.0.0, summary: Say hello}", 1))
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"DOPPELS_CONFIG_HOME=" + filepath.Join(root, "config")}
	app.HTTPClient = server.Client()
	if code := app.Run([]string{"login", "--server", server.URL, "--token", "token"}); code != ExitSuccess {
		t.Fatalf("login exit = %d, stderr = %s", code, stderr.String())
	}
	if code := app.Run([]string{"org", "use", "acme"}); code != ExitSuccess {
		t.Fatalf("org use exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"publish", "capability/greet", "--yes", "--json"}); code != ExitSuccess {
		t.Fatalf("publish exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"kind": "Publish"`) || !strings.Contains(stdout.String(), `"/@acme/greet"`) {
		t.Fatalf("publish json = %s", stdout.String())
	}
	if gotBody["capabilityName"] != "greet" || gotBody["recipeName"] != "greet-shell" {
		t.Fatalf("body = %#v", gotBody)
	}
	source, _ := gotBody["recipeSource"].(string)
	if !strings.Contains(source, "export MESSAGE=") {
		t.Fatalf("recipe source missing script: %#v", gotBody["recipeSource"])
	}
}

func TestPublishWithoutYesIsContractError(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, _, stderr := loggedInHubApp(t, root, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/session" {
			io.WriteString(w, `{"apiVersion":"doppels.so/v1alpha1","identity":{"kind":"identity","id":"owner"},"personal":{"organization":"owner","space":"private"}}`)
			return
		}
		http.Error(w, "should not publish", http.StatusInternalServerError)
	})))
	if code := app.Run([]string{"publish", "capability/greet", "--json"}); code != ExitContract {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--yes") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestUnpublishMarksListing(t *testing.T) {
	var unpublished bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/v1/session" {
			io.WriteString(writer, `{"apiVersion":"doppels.so/v1alpha1","identity":{"kind":"identity","id":"owner"},"personal":{"organization":"owner","space":"private"}}`)
			return
		}
		if request.Method == http.MethodPost && request.URL.Path == "/api/v1/organizations/acme/hub/publications/greet/unpublish" {
			unpublished = true
			io.WriteString(writer, `{"organization":"acme","capabilityName":"greet","capabilityVersion":"1.0.0","recipeName":"greet-shell","recipeVersion":"1.0.0","status":"unpublished","publicPath":"/@acme/greet","capabilitySource":"cap","recipeSource":"rec"}`)
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"DOPPELS_CONFIG_HOME=" + filepath.Join(root, "config")}
	app.HTTPClient = server.Client()
	if code := app.Run([]string{"login", "--server", server.URL, "--token", "token"}); code != ExitSuccess {
		t.Fatalf("login exit = %d, stderr = %s", code, stderr.String())
	}
	if code := app.Run([]string{"org", "use", "acme"}); code != ExitSuccess {
		t.Fatalf("org use = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"unpublish", "capability/greet", "--yes", "--json"}); code != ExitSuccess {
		t.Fatalf("unpublish exit = %d, stderr = %s", code, stderr.String())
	}
	if !unpublished {
		t.Fatal("unpublish was not called")
	}
	if !strings.Contains(stdout.String(), `"kind": "Unpublish"`) || !strings.Contains(stdout.String(), `"unpublished"`) {
		t.Fatalf("unpublish json = %s", stdout.String())
	}
}

func TestInstallWritesModuleAndForkCopiesAuthorYAML(t *testing.T) {
	listing := `{
		"organization":"acme",
		"capabilityName":"greet",
		"capabilityVersion":"1.0.0",
		"capabilitySummary":"Say hello",
		"capabilitySource":"apiVersion: doppels.so/v1alpha1\nkind: Capability\nmetadata: {name: greet, version: 1.0.0}\n",
		"recipeName":"greet-shell",
		"recipeVersion":"1.0.0",
		"recipeSource":"apiVersion: doppels.so/v1alpha1\nkind: Recipe\nmetadata: {name: greet-shell, version: 1.0.0}\nprovides: [greet]\n",
		"status":"listed",
		"publicPath":"/@acme/greet"
	}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/hub/acme/greet" {
			http.NotFound(writer, request)
			return
		}
		if auth := request.Header.Get("Authorization"); auth != "" {
			t.Fatalf("install must not send a token, got %q", auth)
		}
		writer.Header().Set("Content-Type", "application/json")
		io.WriteString(writer, listing)
	}))
	defer server.Close()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".doppels"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "capabilities"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "recipes"), 0o755); err != nil {
		t.Fatal(err)
	}
	app, stdout, stderr := testApp(root)
	app.HTTPClient = server.Client()
	app.Environment = []string{"DOPPELS_SERVER=" + server.URL}
	if code := app.Run([]string{"install", "@acme/greet@1.0.0", "--json"}); code != ExitSuccess {
		t.Fatalf("install exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"kind": "Install"`) {
		t.Fatalf("install json = %s", stdout.String())
	}
	moduleDir := filepath.Join(root, ".doppels", "modules", "@acme", "greet")
	if _, err := os.Stat(filepath.Join(moduleDir, "capability.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moduleDir, "recipe.yaml")); err != nil {
		t.Fatal(err)
	}
	pin, err := os.ReadFile(filepath.Join(moduleDir, "module.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pin), `"organization": "acme"`) || !strings.Contains(string(pin), `"capabilityVersion": "1.0.0"`) {
		t.Fatalf("module.json = %s", pin)
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"fork", "@acme/greet", "--json"}); code != ExitSuccess {
		t.Fatalf("fork exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"kind": "Fork"`) {
		t.Fatalf("fork json = %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(root, "capabilities", "greet.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "recipes", "greet-shell.yaml")); err != nil {
		t.Fatal(err)
	}
}

func loggedInHubApp(t *testing.T, root string, server *httptest.Server) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Cleanup(server.Close)
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"DOPPELS_CONFIG_HOME=" + filepath.Join(root, "config")}
	app.HTTPClient = server.Client()
	if code := app.Run([]string{"login", "--server", server.URL, "--token", "token"}); code != ExitSuccess {
		t.Fatalf("login exit = %d, stderr = %s", code, stderr.String())
	}
	if code := app.Run([]string{"org", "use", "acme"}); code != ExitSuccess {
		t.Fatalf("org use exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	return app, stdout, stderr
}
