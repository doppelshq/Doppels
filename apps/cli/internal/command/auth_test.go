package command

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginContextWhoAmIAndCloudLists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/session":
			fmt.Fprint(writer, `{"apiVersion":"doppels.so/v1alpha1","identity":{"kind":"identity","id":"yuri"},"personal":{"organization":"yuri","space":"private"}}`)
		case "/api/v1/organizations":
			fmt.Fprint(writer, `{"organizations":[{"id":"org-id","name":"acme","displayName":"Acme","createdAt":"2026-08-01T10:00:00Z","updatedAt":"2026-08-01T10:00:00Z"}]}`)
		case "/api/v1/organizations/acme/spaces":
			fmt.Fprint(writer, `{"spaces":[{"id":"space-id","name":"platform","displayName":"Platform","summary":null,"description":null,"labels":{},"annotations":{},"createdAt":"2026-08-01T10:00:00Z","updatedAt":"2026-08-01T10:00:00Z"}]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"DOPPELS_CONFIG_HOME=" + filepath.Join(root, "config")}
	app.HTTPClient = server.Client()
	app.Stdin = strings.NewReader("test-token\n")
	if code := app.Run([]string{"login", "--server", server.URL, "--token-stdin"}); code != ExitSuccess {
		t.Fatalf("login exit = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "test-token") || strings.Contains(stderr.String(), "test-token") {
		t.Fatal("login token leaked to command output")
	}
	loginOut := stdout.String()
	if strings.Contains(loginOut, "identity/yuri") {
		t.Fatalf("Identity should not prefix kind, stdout = %s", loginOut)
	}
	if !strings.Contains(loginOut, "yuri") || !strings.Contains(loginOut, "Cloud") || !strings.Contains(loginOut, server.URL) {
		t.Fatalf("stdout = %s", loginOut)
	}
	if !strings.Contains(loginOut, "Org") || !strings.Contains(loginOut, "Space") || !strings.Contains(loginOut, "private") {
		t.Fatalf("expected Org and Space, stdout = %s", loginOut)
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"context"}); code != ExitSuccess {
		t.Fatalf("context exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "yuri/private") {
		t.Fatalf("stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"org", "use", "acme"}); code != ExitSuccess {
		t.Fatalf("org use exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"spaces", "list"}); code != ExitSuccess {
		t.Fatalf("spaces list exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "platform") {
		t.Fatalf("stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"space", "use", "platform"}); code != ExitSuccess {
		t.Fatalf("space use exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"whoami", "--json"}); code != ExitSuccess {
		t.Fatalf("whoami exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"id": "yuri"`) || !strings.Contains(stdout.String(), `"space": "platform"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"whoami"}); code != ExitSuccess {
		t.Fatalf("whoami exit = %d, stderr = %s", code, stderr.String())
	}
	who := stdout.String()
	if strings.Contains(who, "identity/yuri") {
		t.Fatalf("Identity should not prefix kind, stdout = %s", who)
	}
	if !strings.Contains(who, "yuri") || !strings.Contains(who, "Cloud") || !strings.Contains(who, server.URL) {
		t.Fatalf("stdout = %s", who)
	}
	if !strings.Contains(who, "Org") || !strings.Contains(who, "acme") {
		t.Fatalf("expected current Org, stdout = %s", who)
	}
	if !strings.Contains(who, "Space") || !strings.Contains(who, "platform") {
		t.Fatalf("expected current Space, stdout = %s", who)
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"organizations", "list"}); code != ExitSuccess {
		t.Fatalf("organizations list exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "acme") {
		t.Fatalf("stdout = %s", out)
	}
	if !strings.Contains(out, "local") {
		t.Fatalf("expected offline local org in list, stdout = %s", out)
	}
	if !strings.Contains(out, "*") {
		t.Fatalf("expected current-org marker, stdout = %s", out)
	}
	if !strings.Contains(out, "Cloud") || !strings.Contains(out, server.URL) {
		t.Fatalf("expected Cloud when server is not doppels.so, stdout = %s", out)
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"organizations", "list", "--json"}); code != ExitSuccess {
		t.Fatalf("organizations --json exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"server"`) || !strings.Contains(stdout.String(), server.URL) {
		t.Fatalf("expected server in OrganizationList, stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"org", "use", "local"}); code != ExitSuccess {
		t.Fatalf("org use local exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"organizations", "list"}); code != ExitSuccess {
		t.Fatalf("organizations list after local exit = %d, stderr = %s", code, stderr.String())
	}
	out = stdout.String()
	if !strings.Contains(out, "*") || !strings.Contains(out, "local") {
		t.Fatalf("expected * on local, stdout = %s", out)
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"logout"}); code != ExitSuccess {
		t.Fatalf("logout exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"whoami"}); code != ExitContract {
		t.Fatalf("whoami after logout exit = %d, stderr = %s", code, stderr.String())
	}
	hint := stderr.String()
	if !strings.Contains(hint, "Not logged in to Doppels Cloud") {
		t.Fatalf("stderr = %s", hint)
	}
	if !strings.Contains(hint, "doppels login") || !strings.Contains(hint, "--server") {
		t.Fatalf("expected login hint, stderr = %s", hint)
	}
}

func TestLoginDoesNotPersistRejectedToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	root := t.TempDir()
	app, _, stderr := testApp(root)
	app.Environment = []string{"DOPPELS_CONFIG_HOME=" + filepath.Join(root, "config")}
	app.HTTPClient = server.Client()
	if code := app.Run([]string{"login", "--server", server.URL, "--token", "bad"}); code != ExitOperational {
		t.Fatalf("login exit = %d, stderr = %s", code, stderr.String())
	}
	matches, err := filepath.Glob(filepath.Join(root, "config", "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("rejected token was persisted: %v", matches)
	}
}

func TestLoginResetsContextToPersonalScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/session" || request.Header.Get("Authorization") != "Bearer test-token" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"apiVersion":"doppels.so/v1alpha1","identity":{"kind":"identity","id":"yuri"},"personal":{"organization":"yuri","space":"private"}}`)
	}))
	defer server.Close()

	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"DOPPELS_CONFIG_HOME=" + filepath.Join(root, "config")}
	app.HTTPClient = server.Client()
	if code := app.Run([]string{"login", "--server", server.URL + "/", "--token", "test-token"}); code != ExitSuccess {
		t.Fatalf("initial login exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"org", "use", "acme"}); code != ExitSuccess {
		t.Fatalf("org use exit = %d, stderr = %s", code, stderr.String())
	}
	if code := app.Run([]string{"space", "use", "platform"}); code != ExitSuccess {
		t.Fatalf("space use exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"logout"}); code != ExitSuccess {
		t.Fatalf("logout exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"login", "--token", "test-token"}); code != ExitSuccess {
		t.Fatalf("second login exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"context"}); code != ExitSuccess {
		t.Fatalf("context exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "yuri/private") {
		t.Fatalf("context = %q", stdout.String())
	}
}
