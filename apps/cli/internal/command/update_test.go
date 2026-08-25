package command

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"doppels.so/cli/internal/version"
)

func TestUpdateRejectsArguments(t *testing.T) {
	root := t.TempDir()
	app, _, stderr := testApp(root)
	if code := app.Run([]string{"update", "now"}); code != ExitContract {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "update accepts no arguments") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestUpdateBrewManaged(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	app.Executable = func() (string, error) {
		return "/opt/homebrew/Caskroom/doppels/1.0.0/doppels", nil
	}
	if code := app.Run([]string{"update", "--json"}); code != ExitSuccess {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"status": "brew-managed"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestUpdateAlreadyCurrent(t *testing.T) {
	previous := version.Version
	t.Cleanup(func() { version.Version = previous })
	version.Version = "9.9.9"

	server, client := githubReleaseClient(t, "v9.9.9", nil)
	defer server.Close()

	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	app.HTTPClient = client
	app.Executable = func() (string, error) { return filepath.Join(root, "doppels"), nil }
	if code := app.Run([]string{"update", "--json"}); code != ExitSuccess {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"status": "up-to-date"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestUpdateReplacesBinary(t *testing.T) {
	previous := version.Version
	t.Cleanup(func() { version.Version = previous })
	version.Version = "dev"

	root := t.TempDir()
	target := filepath.Join(root, "doppels")
	if err := os.WriteFile(target, []byte("old-bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("new-bin")
	archive := doppelsTarball(t, payload)

	server, client := githubReleaseClient(t, "v9.9.9", archive)
	defer server.Close()

	app, stdout, stderr := testApp(root)
	app.HTTPClient = client
	app.Executable = func() (string, error) { return target, nil }
	if code := app.Run([]string{"update", "--json"}); code != ExitSuccess {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"status": "updated"`) || !strings.Contains(stdout.String(), `"to": "9.9.9"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("binary = %q", got)
	}
}

func TestUpdateMissingAsset(t *testing.T) {
	previous := version.Version
	t.Cleanup(func() { version.Version = previous })
	version.Version = "dev"

	root := t.TempDir()
	app, _, stderr := testApp(root)
	app.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "api.github.com" {
			return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
		}
		body := []byte(`[{"tag_name":"v9.9.9","assets":[{"name":"wrong.tar.gz","browser_download_url":"http://example/wrong"}]}]`)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    request,
		}, nil
	})}
	app.Executable = func() (string, error) { return filepath.Join(root, "doppels"), nil }
	if code := app.Run([]string{"update"}); code != ExitOperational {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no release asset found") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func githubReleaseClient(t *testing.T, tag string, archive []byte) (*httptest.Server, *http.Client) {
	t.Helper()
	assetName := fmt.Sprintf("doppels_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/archive.tar.gz" {
			writer.Header().Set("Content-Type", "application/gzip")
			_, _ = writer.Write(archive)
			return
		}
		http.NotFound(writer, request)
	})
	server := httptest.NewServer(mux)
	body, err := json.Marshal([]githubRelease{{
		TagName: tag,
		Assets: []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		}{{Name: assetName, BrowserDownloadURL: server.URL + "/archive.tar.gz"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "api.github.com" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(body)),
				Request:    request,
			}, nil
		}
		return http.DefaultTransport.RoundTrip(request)
	})}
	return server, client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func doppelsTarball(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "doppels", Mode: 0o755, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
