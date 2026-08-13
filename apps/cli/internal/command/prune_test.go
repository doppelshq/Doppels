package command

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPruneDryRunAndApplySendKeepSetAndDoNotWriteLock(t *testing.T) {
	var pruneCalls atomic.Int32
	var lastApply atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/v1/session" {
			fmt.Fprint(writer, `{"apiVersion":"doppels.so/v1alpha1","identity":{"kind":"identity","id":"owner"},"personal":{"organization":"owner","space":"private"}}`)
			return
		}
		if request.URL.Path != "/api/v1/organizations/acme/spaces/platform/prune" {
			http.NotFound(writer, request)
			return
		}
		pruneCalls.Add(1)
		var body struct {
			Keep []struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"keep"`
			Apply bool `json:"apply"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		lastApply.Store(body.Apply)
		kinds := map[string]string{}
		for _, entry := range body.Keep {
			kinds[entry.Kind] = entry.Name
		}
		if len(body.Keep) != 2 || kinds["Capability"] != "greet" || kinds["Recipe"] != "greet-shell" {
			t.Fatalf("keep = %#v", body.Keep)
		}
		if body.Apply {
			fmt.Fprint(writer, `{"changes":[{"action":"unregister","kind":"Recipe","name":"other-recipe","reason":"absent_locally"}],"pruned":true}`)
		} else {
			fmt.Fprint(writer, `{"changes":[{"action":"unregister","kind":"Recipe","name":"other-recipe","reason":"absent_locally"}],"applicable":true}`)
		}
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

	if code := app.Run([]string{"prune"}); code != ExitSuccess {
		t.Fatalf("prune dry-run exit = %d, stderr = %s", code, stderr.String())
	}
	if lastApply.Load() {
		t.Fatal("dry run must not set apply")
	}
	if !strings.Contains(stdout.String(), "unregister") ||
		!strings.Contains(stdout.String(), "Dry run only; re-run with --yes to unregister") {
		t.Fatalf("dry run stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"prune", "--yes", "--json"}); code != ExitSuccess {
		t.Fatalf("prune apply exit = %d, stderr = %s", code, stderr.String())
	}
	if !lastApply.Load() {
		t.Fatal("apply run must set apply")
	}
	var response struct {
		Kind    string `json:"kind"`
		Pruned  bool   `json:"pruned"`
		Changes []struct {
			Action string `json:"action"`
			Kind   string `json:"kind"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode json: %v, stdout = %s", err, stdout.String())
	}
	if response.Kind != "Prune" || !response.Pruned || len(response.Changes) != 1 ||
		response.Changes[0].Action != "unregister" {
		t.Fatalf("response = %#v", response)
	}
	if pruneCalls.Load() != 2 {
		t.Fatalf("prune calls = %d", pruneCalls.Load())
	}
}

func TestPruneReportsNothingToPruneWhenKeepSetCoversEverything(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/v1/session" {
			fmt.Fprint(writer, `{"apiVersion":"doppels.so/v1alpha1","identity":{"kind":"identity","id":"owner"},"personal":{"organization":"owner","space":"private"}}`)
			return
		}
		fmt.Fprint(writer, `{"changes":[],"applicable":true}`)
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
		t.Fatalf("org use exit = %d", code)
	}
	if code := app.Run([]string{"space", "use", "platform"}); code != ExitSuccess {
		t.Fatalf("context exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()

	if code := app.Run([]string{"prune"}); code != ExitSuccess {
		t.Fatalf("prune exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Nothing to prune") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestPruneRejectsPositionalArguments(t *testing.T) {
	root := t.TempDir()
	app, _, stderr := testApp(root)
	if code := app.Run([]string{"prune", "capability/greet"}); code != ExitContract {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
}
