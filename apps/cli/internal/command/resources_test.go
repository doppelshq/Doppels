package command

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPluralDefinitionCommandsAreLocal(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, stdout, stderr := testApp(root)

	if code := app.Run([]string{"capabilities", "list", "--json"}); code != ExitSuccess {
		t.Fatalf("capabilities list exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"kind": "CapabilityList"`) || !strings.Contains(stdout.String(), `"name": "greet"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"capabilities", "--json"}); code != ExitSuccess {
		t.Fatalf("capabilities alias exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"kind": "CapabilityList"`) {
		t.Fatalf("alias stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"caps", "--json"}); code != ExitSuccess {
		t.Fatalf("caps alias exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"kind": "CapabilityList"`) {
		t.Fatalf("caps stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"recipes"}); code != ExitSuccess {
		t.Fatalf("recipes alias exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "greet-shell") && !strings.Contains(stdout.String(), "NAME") {
		t.Fatalf("recipes alias stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"recipes", "show", "greet-shell", "--json"}); code != ExitSuccess {
		t.Fatalf("recipes show exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"kind": "RecipeDescription"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestRunsListShowAndLogsReadLocalState(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester"}
	app.Hostname = func() (string, error) { return "test-node", nil }
	if code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada", "--json"}); code != ExitSuccess {
		t.Fatalf("run exit = %d, stderr = %s", code, stderr.String())
	}
	var executed struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &executed); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"runs", "list", "--json"}); code != ExitSuccess {
		t.Fatalf("runs list exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), executed.Run.ID) || !strings.Contains(stdout.String(), `"status": "succeeded"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"source": "local"`) {
		t.Fatalf("expected local source, stdout = %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"runs", "show", executed.Run.ID}); code != ExitSuccess {
		t.Fatalf("runs show exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Cap") || !strings.Contains(stdout.String(), "greet@1.0.0") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Timeline") || !strings.Contains(stdout.String(), "Succeeded") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"runs", "logs", executed.Run.ID}); code != ExitSuccess {
		t.Fatalf("runs logs exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "step-noise") || !strings.Contains(stdout.String(), "greet · stdout") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".doppels", "runs", executed.Run.ID)); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	app.Sleep = func(time.Duration) {}
	if code := app.Run([]string{"runs", "logs", executed.Run.ID, "-f"}); code != ExitSuccess {
		t.Fatalf("runs logs -f exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "step-noise") || !strings.Contains(stdout.String(), "Succeeded") {
		t.Fatalf("follow stdout = %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"runs", "logs", executed.Run.ID, "-f", "--json"}); code != ExitContract {
		t.Fatalf("follow+json exit = %d, want %d, stderr = %s", code, ExitContract, stderr.String())
	}
}

func TestRunsListMergesCloudSourceWhenLoggedIn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/session":
			fmt.Fprint(writer, `{"apiVersion":"doppels.so/v1alpha1","identity":{"kind":"identity","id":"yuri"},"personal":{"organization":"yuri","space":"private"}}`)
		case "/api/v1/organizations/acme/spaces/platform/runs":
			fmt.Fprint(writer, `{"runs":[{"id":"cloud-run","requestId":"cloud-req","createdAt":"2026-08-02T12:00:00Z","capability":{"name":"greet","version":"1.0.0"},"status":"succeeded","source":"cloud","inputs":{},"executor":{"kind":"identity","id":"yuri"}}]}`)
		case "/api/v1/organizations/personal/spaces/default/runs":
			fmt.Fprint(writer, `{"runs":[]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester", "DOPPELS_CONFIG_HOME=" + filepath.Join(root, "config")}
	app.HTTPClient = server.Client()
	app.Hostname = func() (string, error) { return "test-node", nil }
	if code := app.Run([]string{"login", "--server", server.URL, "--token", "test-token"}); code != ExitSuccess {
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
	if code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada", "--json"}); code != ExitSuccess {
		t.Fatalf("run exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"runs", "list", "--json"}); code != ExitSuccess {
		t.Fatalf("runs list exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"id": "cloud-run"`) || !strings.Contains(stdout.String(), `"source": "cloud"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"source": "local"`) {
		t.Fatalf("expected local run retained, stdout = %s", stdout.String())
	}
}
