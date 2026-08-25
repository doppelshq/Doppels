package command

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/projectlock"
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

func TestRecipesListShowsCapabilityCount(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, stdout, stderr := testApp(root)
	if code := app.Run([]string{"recipes", "--json"}); code != ExitSuccess {
		t.Fatalf("json exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"capabilities": 1`) {
		t.Fatalf("json missing capabilities count: %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"recipes"}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "CAPS") || strings.Contains(out, "PROVIDES") {
		t.Fatalf("human recipes table = %s", out)
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

func TestCapabilitiesListUsesWorkspaceDiscovery(t *testing.T) {
	workspace := t.TempDir()
	engineering := filepath.Join(workspace, "engineering")
	finance := filepath.Join(workspace, "finance")
	if _, err := project.Init(engineering); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Init(finance); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, engineering, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, engineering, "recipes", "greet.yaml", runRecipeFixture)
	writeManifest(t, finance, "capabilities", "greet.yaml", strings.ReplaceAll(runCapabilityFixture, "greet", "close-month"))
	writeManifest(t, finance, "recipes", "close-month.yaml", strings.ReplaceAll(runRecipeFixture, "greet", "close-month"))

	app, stdout, stderr := testApp(workspace)
	if code := app.Run([]string{"capabilities"}); code != ExitSuccess {
		t.Fatalf("capabilities exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"SPACE",
		"NAME",
		"VERSION",
		"PIN",
		"HOST",
		"RECIPES",
		"RUNS",
		"LAST",
		"FILE",
		"engineering",
		"finance",
		"greet",
		"close-month",
		"unpinned",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ORIGIN") || strings.Contains(out, "applied") {
		t.Fatalf("stale origin copy still rendered:\n%s", out)
	}
	if strings.Contains(out, "├──") || strings.Contains(out, "└──") {
		t.Fatalf("tree still rendered:\n%s", out)
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"capabilities", "--json"}); code != ExitSuccess {
		t.Fatalf("capabilities json exit = %d, stderr = %s", code, stderr.String())
	}
	payload := stdout.String()
	if !strings.Contains(payload, `"space": "engineering"`) || !strings.Contains(payload, `"space": "finance"`) {
		t.Fatalf("json missing spaces: %s", payload)
	}
	if !strings.Contains(payload, `"pin": "unpinned"`) {
		t.Fatalf("json missing unpinned pin: %s", payload)
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"recipes"}); code != ExitSuccess {
		t.Fatalf("recipes exit = %d, stderr = %s", code, stderr.String())
	}
	recipesOut := stdout.String()
	for _, want := range []string{
		"SPACE", "NAME", "VERSION", "RUNTIME", "PIN", "HOST", "CAPS", "RUNS", "LAST", "FILE",
		"engineering", "greet-shell", "unpinned",
	} {
		if !strings.Contains(recipesOut, want) {
			t.Fatalf("recipes missing %q in:\n%s", want, recipesOut)
		}
	}
	if !strings.Contains(recipesOut, "CAPS") {
		t.Fatalf("recipes missing CAPS count column:\n%s", recipesOut)
	}
	if strings.Contains(recipesOut, "PROVIDES") {
		t.Fatalf("recipes still uses PROVIDES names:\n%s", recipesOut)
	}
	if strings.Contains(recipesOut, "├──") || strings.Contains(recipesOut, "└──") {
		t.Fatalf("recipes tree still rendered:\n%s", recipesOut)
	}
}

func TestCapabilitiesListStaysOnCwdSpace(t *testing.T) {
	workspace := t.TempDir()
	here := filepath.Join(workspace, "engineering")
	sibling := filepath.Join(workspace, "finance")
	if _, err := project.Init(here); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Init(sibling); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, here, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, sibling, "capabilities", "close-month.yaml", strings.ReplaceAll(runCapabilityFixture, "greet", "close-month"))

	app, stdout, stderr := testApp(here)
	if code := app.Run([]string{"capabilities", "--json"}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	payload := stdout.String()
	if !strings.Contains(payload, `"name": "greet"`) {
		t.Fatalf("stdout = %s", payload)
	}
	if strings.Contains(payload, "close-month") {
		t.Fatalf("cwd Space leaked sibling catalog: %s", payload)
	}
}

func TestCapabilitiesListMarksAppliedFromLock(t *testing.T) {
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	capPath := writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	data, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if err := projectlock.Write(root, projectlock.New([]projectlock.Entry{{
		Kind:            "Capability",
		SourceAuthority: "manifest",
		Revision: execution.DefinitionReference{
			Name: "greet", Version: "1.0.0", ManifestSHA256: digest,
			Schema: execution.SchemaReference{ID: manifest.CapabilitySchemaID, SHA256: manifest.CapabilitySchemaSHA256},
		},
	}})); err != nil {
		t.Fatal(err)
	}

	app, stdout, stderr := testApp(root)
	if code := app.Run([]string{"capabilities"}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "pinned") {
		t.Fatalf("expected pinned pin, stdout = %s", stdout.String())
	}
	if strings.Contains(stdout.String(), "applied") {
		t.Fatalf("applied copy still present: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "greet.yaml") {
		t.Fatalf("missing FILE path, stdout = %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"capabilities", "--json"}); code != ExitSuccess {
		t.Fatalf("json exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"pin": "pinned"`) {
		t.Fatalf("json pin = %s", stdout.String())
	}
}

func TestCapabilitiesListShowsRunCountAndLast(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester"}
	app.Hostname = func() (string, error) { return "test-node", nil }
	app.Now = func() time.Time { return time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC) }
	if code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada", "--json"}); code != ExitSuccess {
		t.Fatalf("run exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"capabilities"}); code != ExitSuccess {
		t.Fatalf("capabilities exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "RUNS") || !strings.Contains(out, "LAST") {
		t.Fatalf("missing run columns:\n%s", out)
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"capabilities", "--json"}); code != ExitSuccess {
		t.Fatalf("capabilities json exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"runs": 1`) {
		t.Fatalf("expected runs=1, stdout = %s", stdout.String())
	}
}
