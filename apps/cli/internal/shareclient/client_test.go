package shareclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
)

func TestCreateUsesAPITokenAndOnlySendsPublicDefinitionData(t *testing.T) {
	var called atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		called.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/shares" {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer api-token" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(request.Body)
		bodyText := string(body)
		for _, forbidden := range []string{"sharedBy", "script", "DEPLOY_TOKEN", "hostEnv", "runnerToken"} {
			if strings.Contains(bodyText, forbidden) {
				t.Errorf("bootstrap leaked %q: %s", forbidden, bodyText)
			}
		}
		var create CreateShareRequest
		if err := json.Unmarshal(body, &create); err != nil {
			t.Error(err)
		}
		if create.Recipe == nil || create.Recipe.ManifestSHA256 != strings.Repeat("b", 64) {
			t.Errorf("missing exact Recipe reference: %#v", create.Recipe)
		}
		response := ShareCreated{APIVersion: APIVersion, Kind: "ShareCreated", Share: testShare(), PublicURL: server.URL + "/s/test", RunnerToken: testRunnerToken}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(response)
	}))
	defer server.Close()
	client, err := New(Options{Server: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	recipe := testRecipeReference()
	created, err := client.Create(context.Background(), "api-token", CreateShareRequest{
		CapabilityRevision: testCapabilityReference(), Capability: testCapability(), Recipe: &recipe, ExpiresAt: testNow.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.PublicURL != server.URL+"/s/test" || created.RunnerToken != testRunnerToken {
		t.Fatalf("unexpected response: %#v", created)
	}
	if called.Load() != 1 {
		t.Fatalf("requests = %d", called.Load())
	}
}

func TestCreateAllowsAnonymousWithoutAPIToken(t *testing.T) {
	var sawAuth string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sawAuth = request.Header.Get("Authorization")
		share := testShare()
		share.SharedBy = execution.ActorReference{Kind: "anonymous", ID: "anonymous"}
		response := ShareCreated{APIVersion: APIVersion, Kind: "ShareCreated", Share: share, PublicURL: server.URL + "/s/anon", RunnerToken: testRunnerToken}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(response)
	}))
	defer server.Close()
	client, err := New(Options{Server: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	recipe := testRecipeReference()
	created, err := client.Create(context.Background(), "", CreateShareRequest{
		CapabilityRevision: testCapabilityReference(), Capability: testCapability(), Recipe: &recipe, ExpiresAt: testNow.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sawAuth != "" {
		t.Fatalf("Authorization = %q, want empty", sawAuth)
	}
	if created.Share.SharedBy.Kind != "anonymous" {
		t.Fatalf("sharedBy = %#v", created.Share.SharedBy)
	}
}

func TestNewRequiresTLSForRemoteServers(t *testing.T) {
	for _, test := range []struct {
		server string
		valid  bool
	}{
		{server: "https://doppels.so", valid: true},
		{server: "http://localhost:4000", valid: true},
		{server: "http://127.77.1.2:4000", valid: true},
		{server: "http://[::1]:4000", valid: true},
		{server: "http://doppels.so", valid: false},
		{server: "http://localhost.example.test", valid: false},
		{server: "http://0.0.0.0:4000", valid: false},
	} {
		t.Run(test.server, func(t *testing.T) {
			_, err := New(Options{Server: test.server})
			if (err == nil) != test.valid {
				t.Fatalf("New(%q) error = %v, valid = %t", test.server, err, test.valid)
			}
		})
	}
}

func TestCreateRejectsInsecureRemotePublicURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		response := ShareCreated{
			APIVersion:  APIVersion,
			Kind:        "ShareCreated",
			Share:       testShare(),
			PublicURL:   "http://example.test/s/public-token",
			RunnerToken: testRunnerToken,
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(response)
	}))
	defer server.Close()

	client, err := New(Options{
		Server:     server.URL,
		HTTPClient: server.Client(),
		Now:        func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	recipe := testRecipeReference()
	_, err = client.Create(context.Background(), "api-token", CreateShareRequest{
		CapabilityRevision: testCapabilityReference(),
		Capability:         testCapability(),
		Recipe:             &recipe,
		ExpiresAt:          testNow.Add(time.Hour),
	})
	if err == nil || !strings.Contains(err.Error(), "insecure public Share URL") {
		t.Fatalf("error = %v", err)
	}
}

func TestPendingUsesRunnerTokenAndPreservesIntegerInputs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/shares/"+testShareID+"/pending" {
			t.Errorf("path = %s", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+testRunnerToken {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(writer).Encode(testPending())
	}))
	defer server.Close()
	client, _ := New(Options{Server: server.URL, HTTPClient: server.Client()})
	pending, err := client.Pending(context.Background(), testShareID, testRunnerToken)
	if err != nil {
		t.Fatal(err)
	}
	value, ok := pending.Request.Inputs["count"].(json.Number)
	if !ok || value.String() != "9007199254740993" {
		t.Fatalf("input = %#v (%T)", pending.Request.Inputs["count"], pending.Request.Inputs["count"])
	}
}

func TestPendingReturnsTypedPermanentHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusGone) }))
	defer server.Close()
	client, _ := New(Options{Server: server.URL, HTTPClient: server.Client()})
	_, err := client.Pending(context.Background(), testShareID, testRunnerToken)
	var problem HTTPError
	if !errors.As(err, &problem) || problem.StatusCode != http.StatusGone || !problem.Permanent() {
		t.Fatalf("error = %#v", err)
	}
}

func TestUploadArtifactAuthenticatesAndVerifiesAllMetadata(t *testing.T) {
	directory := t.TempDir()
	artifactPath := filepath.Join(directory, "release.tgz")
	contents := []byte("release bytes")
	if err := os.WriteFile(artifactPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256(contents)
	digest := hex.EncodeToString(digestBytes[:])
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		wantPath := "/api/v1/shares/" + testShareID + "/runs/" + testRunID + "/artifacts/artifact-1"
		if request.Method != http.MethodPut || request.URL.Path != wantPath {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+testRunnerToken {
			t.Errorf("missing runner auth")
		}
		if request.Header.Get("X-Doppel-Filename") != "release.tgz" || request.Header.Get("X-Doppel-Sha256") != digest {
			t.Errorf("incorrect artifact headers: %#v", request.Header)
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != string(contents) {
			t.Errorf("body = %q", body)
		}
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(ArtifactUploadResponse{Artifact: executionArtifact("artifact-1", "release.tgz", "application/gzip", int64(len(contents)), digest)})
	}))
	defer server.Close()
	client, _ := New(Options{Server: server.URL, HTTPClient: server.Client()})
	artifact, err := client.UploadArtifact(context.Background(), testShareID, testRunnerToken, ArtifactUpload{RunID: testRunID, ArtifactID: "artifact-1", Path: artifactPath, Filename: "release.tgz", MediaType: "application/gzip"})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Filename != "release.tgz" || artifact.MediaType != "application/gzip" {
		t.Fatalf("artifact = %#v", artifact)
	}
}

func TestUploadArtifactRejectsUnsafeFilenameAndOversizeBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	client, _ := New(Options{Server: server.URL, HTTPClient: server.Client()})
	directory := t.TempDir()
	small := filepath.Join(directory, "file")
	if err := os.WriteFile(small, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{"../secret", "bad\\name", "bad\r\nheader"} {
		if _, err := client.UploadArtifact(context.Background(), testShareID, testRunnerToken, ArtifactUpload{RunID: testRunID, ArtifactID: "id", Path: small, Filename: filename}); err == nil {
			t.Errorf("filename %q accepted", filename)
		}
	}
	large := filepath.Join(directory, "large")
	file, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxArtifactBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := client.UploadArtifact(context.Background(), testShareID, testRunnerToken, ArtifactUpload{RunID: testRunID, ArtifactID: "id", Path: large}); err == nil {
		t.Fatal("oversized artifact accepted")
	}
	if calls.Load() != 0 {
		t.Fatalf("HTTP calls = %d", calls.Load())
	}
}

func TestUploadArtifactRejectsChangedCloudMetadata(t *testing.T) {
	directory := t.TempDir()
	artifactPath := filepath.Join(directory, "release.tgz")
	contents := []byte("release bytes")
	if err := os.WriteFile(artifactPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256(contents)
	digest := hex.EncodeToString(digestBytes[:])
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(ArtifactUploadResponse{Artifact: executionArtifact("artifact-1", "changed.tgz", "application/gzip", int64(len(contents)), digest)})
	}))
	defer server.Close()
	client, _ := New(Options{Server: server.URL, HTTPClient: server.Client()})
	_, err := client.UploadArtifact(context.Background(), testShareID, testRunnerToken, ArtifactUpload{RunID: testRunID, ArtifactID: "artifact-1", Path: artifactPath, Filename: "release.tgz", MediaType: "application/gzip"})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v", err)
	}
}

func executionArtifact(id, filename, mediaType string, size int64, digest string) execution.ArtifactReference {
	return execution.ArtifactReference{ID: id, Filename: filename, MediaType: mediaType, SizeBytes: size, SHA256: digest}
}
