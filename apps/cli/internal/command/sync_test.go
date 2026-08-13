package command

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRecipeDriftBlocksMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/session":
			fmt.Fprint(writer, `{"apiVersion":"doppels.so/v1alpha1","identity":{"kind":"identity","id":"yuri"},"personal":{"organization":"yuri","space":"private"}}`)
		case "/api/v1/organizations/yuri/spaces/private/recipes":
			fmt.Fprint(writer, `{"recipes":[{"id":"r1","name":"greet-shell","sourceAuthority":"manifest","revision":{"id":"rev1","version":"1.0.0","manifestSha256":"deadbeef","schema":{"id":"https://doppels.so/schemas/v1alpha1/recipe.schema.json","sha256":"cafe"}}}]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, _, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester", "DOPPELS_CONFIG_HOME=" + filepath.Join(root, "config")}
	app.HTTPClient = server.Client()
	if code := app.Run([]string{"login", "--server", server.URL, "--token", "test-token"}); code != ExitSuccess {
		t.Fatalf("login exit = %d, stderr = %s", code, stderr.String())
	}
	stderr.Reset()

	_, catalog, code := app.localCatalog()
	if code != ExitSuccess {
		t.Fatalf("catalog exit = %d", code)
	}
	recipe, err := catalog.ResolveRecipe("greet", "greet-shell")
	if err != nil {
		t.Fatal(err)
	}
	if code := app.checkRecipeDrift(&recipe); code != ExitContract {
		t.Fatalf("drift exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "digest differs") || !strings.Contains(stderr.String(), "Sync the Space working tree from Git") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestCheckRecipeDriftAllowsUnregisteredRecipe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/session":
			fmt.Fprint(writer, `{"apiVersion":"doppels.so/v1alpha1","identity":{"kind":"identity","id":"yuri"},"personal":{"organization":"yuri","space":"private"}}`)
		case "/api/v1/organizations/yuri/spaces/private/recipes":
			fmt.Fprint(writer, `{"recipes":[]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, _, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester", "DOPPELS_CONFIG_HOME=" + filepath.Join(root, "config")}
	app.HTTPClient = server.Client()
	if code := app.Run([]string{"login", "--server", server.URL, "--token", "test-token"}); code != ExitSuccess {
		t.Fatalf("login exit = %d, stderr = %s", code, stderr.String())
	}
	_, catalog, code := app.localCatalog()
	if code != ExitSuccess {
		t.Fatal(code)
	}
	recipe, err := catalog.ResolveRecipe("greet", "greet-shell")
	if err != nil {
		t.Fatal(err)
	}
	if code := app.checkRecipeDrift(&recipe); code != ExitSuccess {
		t.Fatalf("exit = %d stderr = %s", code, stderr.String())
	}
}

func TestCheckRecipeDriftSkipsLocalContext(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits++
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/session":
			fmt.Fprint(writer, `{"apiVersion":"doppels.so/v1alpha1","identity":{"kind":"identity","id":"yuri"},"personal":{"organization":"yuri","space":"private"}}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, _, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester", "DOPPELS_CONFIG_HOME=" + filepath.Join(root, "config")}
	app.HTTPClient = server.Client()
	if code := app.Run([]string{"login", "--server", server.URL, "--token", "test-token"}); code != ExitSuccess {
		t.Fatalf("login exit = %d, stderr = %s", code, stderr.String())
	}
	stderr.Reset()
	if code := app.Run([]string{"org", "use", "local"}); code != ExitSuccess {
		t.Fatalf("org use exit = %d, stderr = %s", code, stderr.String())
	}
	if code := app.Run([]string{"space", "use", "private"}); code != ExitSuccess {
		t.Fatalf("space use exit = %d, stderr = %s", code, stderr.String())
	}
	stderr.Reset()
	loginHits := hits

	_, catalog, code := app.localCatalog()
	if code != ExitSuccess {
		t.Fatalf("catalog exit = %d", code)
	}
	recipe, err := catalog.ResolveRecipe("greet", "greet-shell")
	if err != nil {
		t.Fatal(err)
	}
	if code := app.checkRecipeDrift(&recipe); code != ExitSuccess {
		t.Fatalf("drift exit = %d, stderr = %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("local context should stay silent, stderr = %s", stderr.String())
	}
	if hits != loginHits {
		t.Fatalf("local drift check hit cloud: hits before=%d after=%d", loginHits, hits)
	}
}
