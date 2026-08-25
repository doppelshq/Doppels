package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/shareclient"

	"github.com/coder/websocket"
)

func TestShareCreatesAnonymouslyWithoutLogin(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "answer.yaml", shareCapabilityFixture)
	now := time.Now().UTC().Truncate(time.Millisecond)
	var sawAuth string
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/api/v1/shares", func(writer http.ResponseWriter, request *http.Request) {
		sawAuth = request.Header.Get("Authorization")
		var create shareclient.CreateShareRequest
		if err := json.NewDecoder(request.Body).Decode(&create); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(shareclient.ShareCreated{
			APIVersion: shareclient.APIVersion,
			Kind:       "ShareCreated",
			Share: shareclient.Share{
				APIVersion:         shareclient.APIVersion,
				Kind:               "Share",
				ID:                 commandShareID,
				CreatedAt:          now,
				ExpiresAt:          create.ExpiresAt,
				CapabilityRevision: create.CapabilityRevision,
				Capability:         create.Capability,
				Recipe:             create.Recipe,
				SharedBy:           execution.ActorReference{Kind: "anonymous", ID: "anonymous"},
				RequestLimit:       1,
			},
			PublicURL:   server.URL + "/s/public_anon",
			RunnerToken: commandRunnerToken,
		})
	})
	mux.HandleFunc("/socket/websocket", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	})

	app, stdout, _ := testApp(root)
	app.HTTPClient = server.Client()
	app.Now = func() time.Time { return now }
	_ = app.Run([]string{"share", "capability/answer-question", "--server", server.URL, "--expires", "1h"})
	out := stdout.String()
	if sawAuth != "" {
		t.Fatalf("Authorization = %q, want empty", sawAuth)
	}
	if !strings.Contains(out, server.URL+"/s/public_anon") {
		t.Fatalf("missing public URL in output: %s", out)
	}
	if !strings.Contains(out, "doppels login") {
		t.Fatalf("missing login tip in output: %s", out)
	}
}

func TestShareUsesProfileServerWithExplicitEnvironmentToken(t *testing.T) {
	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		called.Store(true)
		if request.Header.Get("Authorization") != "Bearer explicit-token" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "answer.yaml", shareCapabilityFixture)
	app, _, stderr := testApp(root)
	app.Environment = []string{"DOPPELS_API_TOKEN=explicit-token"}
	app.HTTPClient = server.Client()
	store := configstore.New(filepath.Join(root, ".test-config"))
	if err := store.Login(server.URL, "saved-token", time.Now()); err != nil {
		t.Fatal(err)
	}
	if code := app.Run([]string{"share", "capability/answer-question"}); code != ExitOperational {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !called.Load() {
		t.Fatal("profile server was not called")
	}
}

const (
	commandShareID     = "4f2afb9c-7c4f-4d65-897b-9e018b2e84bd"
	commandRequestID   = "a76b9a87-5301-47fd-9854-206e84a39636"
	commandRunnerToken = "runner_command_test_token_1234567890abcdef"
)

func TestShareStreamsOneManualFulfillmentAndKeepsJSONStdoutSingular(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "answer.yaml", shareCapabilityFixture)
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := execution.ActorReference{Kind: "identity", ID: "owner@example.test"}
	serverErrors := make(chan error, 8)
	streamDone := make(chan struct{})
	var shared shareclient.Share
	var sharedMu sync.RWMutex
	var eventTypes []string
	var eventMu sync.Mutex
	runSeen := false

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/api/v1/shares", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer command-api-token" {
			serverErrors <- fmt.Errorf("invalid bootstrap request")
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		var create shareclient.CreateShareRequest
		if err := json.NewDecoder(request.Body).Decode(&create); err != nil {
			serverErrors <- err
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if create.Recipe != nil || create.Capability == nil || create.Capability.Metadata.Name != "answer-question" {
			serverErrors <- fmt.Errorf("unexpected shared definitions: %#v", create)
		}
		value := shareclient.Share{
			APIVersion: manifest.APIVersion, Kind: "Share", ID: commandShareID,
			CreatedAt: now, ExpiresAt: create.ExpiresAt,
			CapabilityRevision: create.CapabilityRevision, Capability: create.Capability,
			SharedBy: owner, RequestLimit: 1,
		}
		sharedMu.Lock()
		shared = value
		sharedMu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(shareclient.ShareCreated{
			APIVersion: manifest.APIVersion, Kind: "ShareCreated", Share: value,
			PublicURL: server.URL + "/s/public", RunnerToken: commandRunnerToken,
		})
	})
	mux.HandleFunc("/socket/websocket", func(writer http.ResponseWriter, request *http.Request) {
		defer close(streamDone)
		if request.URL.Query().Get("vsn") != "2.0.0" || request.URL.Query().Has("token") {
			serverErrors <- fmt.Errorf("invalid WebSocket URL %s", request.URL.String())
			return
		}
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer connection.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		join, err := readCommandPhoenix(ctx, connection)
		if err != nil {
			serverErrors <- err
			return
		}
		var joinPayload map[string]string
		if err := json.Unmarshal(join.Payload, &joinPayload); err != nil || join.Event != "phx_join" || joinPayload["token"] != commandRunnerToken {
			serverErrors <- fmt.Errorf("invalid join: %#v (%v)", join, err)
			return
		}
		sharedMu.RLock()
		share := shared
		sharedMu.RUnlock()
		requestRecord := execution.RequestRecord{
			APIVersion: execution.APIVersion, Kind: "Request", ID: commandRequestID,
			CreatedAt: now, IdempotencyKey: "share:command:request:1", Origin: "share",
			Capability: share.CapabilityRevision, Inputs: map[string]any{"question": "meaning"},
			RequestedBy: execution.ActorReference{Kind: "guest", ID: "Yuri"}, ShareID: share.ID,
		}
		if err := writeCommandPhoenixReply(ctx, connection, join, shareclient.PendingState{Share: share, Request: &requestRecord}); err != nil {
			serverErrors <- err
			return
		}

		ackIndex := 0
		for {
			push, err := readCommandPhoenix(ctx, connection)
			if err != nil {
				serverErrors <- err
				return
			}
			if push.Event == "heartbeat" {
				if err := writeCommandPhoenixReply(ctx, connection, push, map[string]any{}); err != nil {
					serverErrors <- err
					return
				}
				continue
			}
			var submitted shareclient.ShareMessage
			if err := json.Unmarshal(push.Payload, &submitted); err != nil {
				serverErrors <- err
				return
			}
			recordedEvent := ""
			terminal := false
			switch push.Event {
			case "run_submitted":
				recordedEvent = "run_recorded"
				var run execution.RunRecord
				if err := json.Unmarshal(submitted.Payload, &run); err != nil {
					serverErrors <- err
					return
				}
				if run.Executor != owner || run.RequestID != commandRequestID || run.Recipe != nil {
					serverErrors <- fmt.Errorf("unexpected shared Run: %#v", run)
					return
				}
				runSeen = true
			case "run_event_submitted":
				recordedEvent = "run_event_recorded"
				var event execution.RunEvent
				if err := json.Unmarshal(submitted.Payload, &event); err != nil {
					serverErrors <- err
					return
				}
				eventMu.Lock()
				eventTypes = append(eventTypes, event.Type)
				eventMu.Unlock()
				terminal = event.Type == "run_succeeded"
			default:
				serverErrors <- fmt.Errorf("unexpected push %q", push.Event)
				return
			}
			ackIndex++
			recorded := submitted
			recorded.MessageID = fmt.Sprintf("00000000-0000-4000-8000-%012d", ackIndex)
			recorded.Event = recordedEvent
			if err := writeCommandPhoenixReply(ctx, connection, push, recorded); err != nil {
				serverErrors <- err
				return
			}
			if terminal {
				return
			}
		}
	})

	app, stdout, stderr := testApp(root)
	app.Context = context.Background()
	app.Stdin = strings.NewReader("")
	app.Environment = []string{"DOPPELS_API_TOKEN=command-api-token", "DOPPELS_IDENTITY=local-different-owner"}
	app.Hostname = func() (string, error) { return "command-node", nil }
	app.Now = func() time.Time { return now }
	code := app.Run([]string{
		"share", "capability/answer-question", "--server", server.URL,
		"--expires", "1h", "--output", "answer=42", "--json",
	})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	select {
	case <-streamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Share stream did not finish")
	}
	select {
	case err := <-serverErrors:
		t.Fatal(err)
	default:
	}
	var session shareSessionView
	decoder := json.NewDecoder(strings.NewReader(stdout.String()))
	if err := decoder.Decode(&session); err != nil {
		t.Fatalf("decode ShareSession: %v\n%s", err, stdout.String())
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout contains more than one JSON document: %s", stdout.String())
	}
	if session.Kind != "ShareSession" || session.ShareID != commandShareID || session.Status != "listening" || session.PublicURL != server.URL+"/s/public" {
		t.Fatalf("session = %#v", session)
	}
	if !runSeen {
		t.Fatal("Run was not streamed")
	}
	eventMu.Lock()
	gotEvents := append([]string(nil), eventTypes...)
	eventMu.Unlock()
	if len(gotEvents) < 3 || gotEvents[0] != "run_created" || gotEvents[len(gotEvents)-1] != "run_succeeded" {
		t.Fatalf("events = %#v", gotEvents)
	}
	if !strings.Contains(stderr.String(), `Request from "Yuri"`) && !strings.Contains(stderr.String(), "from Yuri") {
		t.Fatalf("stderr missing request: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "status succeeded") && !strings.Contains(stdout.String(), "Succeeded") && !strings.Contains(stderr.String(), "Succeeded") {
		t.Fatalf("stderr/stdout missing success: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}

func TestShareExpiresWhileWaitingAndPerformsDurableFinalCheck(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "answer.yaml", shareCapabilityFixture)
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := execution.ActorReference{Kind: "identity", ID: "owner@example.test"}
	var shared shareclient.Share
	var sharedMu sync.RWMutex
	pendingCalls := 0
	var pendingMu sync.Mutex

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/api/v1/shares", func(writer http.ResponseWriter, request *http.Request) {
		var create shareclient.CreateShareRequest
		_ = json.NewDecoder(request.Body).Decode(&create)
		value := shareclient.Share{
			APIVersion: manifest.APIVersion, Kind: "Share", ID: commandShareID,
			CreatedAt: now, ExpiresAt: create.ExpiresAt,
			CapabilityRevision: create.CapabilityRevision, Capability: create.Capability,
			SharedBy: owner, RequestLimit: 1,
		}
		sharedMu.Lock()
		shared = value
		sharedMu.Unlock()
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(shareclient.ShareCreated{
			APIVersion: manifest.APIVersion, Kind: "ShareCreated", Share: value,
			PublicURL: server.URL + "/s/public", RunnerToken: commandRunnerToken,
		})
	})
	mux.HandleFunc("/api/v1/shares/"+commandShareID+"/pending", func(writer http.ResponseWriter, _ *http.Request) {
		pendingMu.Lock()
		pendingCalls++
		pendingMu.Unlock()
		sharedMu.RLock()
		state := shareclient.PendingState{Share: shared}
		sharedMu.RUnlock()
		_ = json.NewEncoder(writer).Encode(state)
	})
	mux.HandleFunc("/socket/websocket", func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		join, err := readCommandPhoenix(ctx, connection)
		if err != nil {
			return
		}
		sharedMu.RLock()
		state := shareclient.PendingState{Share: shared}
		sharedMu.RUnlock()
		if err := writeCommandPhoenixReply(ctx, connection, join, state); err != nil {
			return
		}
		_, _, _ = connection.Read(ctx)
	})

	app, stdout, stderr := testApp(root)
	app.Context = context.Background()
	app.Environment = []string{"DOPPELS_API_TOKEN=command-api-token"}
	app.Now = time.Now
	started := time.Now()
	code := app.Run([]string{"share", "capability/answer-question", "--server", server.URL, "--expires", "300ms", "--json"})
	if code != ExitOperational {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("expiry took %s", elapsed)
	}
	pendingMu.Lock()
	calls := pendingCalls
	pendingMu.Unlock()
	if calls != 1 {
		t.Fatalf("durable final pending checks = %d", calls)
	}
	if !strings.Contains(stderr.String(), "expired before a Request") {
		t.Fatalf("stderr = %s", stderr.String())
	}
	var session shareSessionView
	if err := json.Unmarshal(stdout.Bytes(), &session); err != nil || session.Kind != "ShareSession" {
		t.Fatalf("stdout = %s, err = %v", stdout.String(), err)
	}
}

func TestShareInteractivePicksCapabilityAndDuration(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "answer.yaml", shareCapabilityFixture)
	writeManifest(t, root, "capabilities", "other.yaml", strings.ReplaceAll(shareCapabilityFixture, "answer-question", "other-task"))
	now := time.Now().UTC().Truncate(time.Millisecond)
	var gotExpires time.Time
	var gotName string
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/api/v1/shares", func(writer http.ResponseWriter, request *http.Request) {
		var create shareclient.CreateShareRequest
		if err := json.NewDecoder(request.Body).Decode(&create); err != nil {
			t.Errorf("decode: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		gotExpires = create.ExpiresAt
		gotName = create.Capability.Metadata.Name
		value := shareclient.Share{
			APIVersion: manifest.APIVersion, Kind: "Share", ID: commandShareID,
			CreatedAt: now, ExpiresAt: create.ExpiresAt,
			CapabilityRevision: create.CapabilityRevision, Capability: create.Capability,
			SharedBy:     execution.ActorReference{Kind: "identity", ID: "owner@example.test"},
			RequestLimit: 1,
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(shareclient.ShareCreated{
			APIVersion:  manifest.APIVersion,
			Kind:        "ShareCreated",
			Share:       value,
			PublicURL:   server.URL + "/s/public",
			RunnerToken: commandRunnerToken,
		})
	})

	app, stdout, stderr := testApp(root)
	app.Stdin = strings.NewReader("2\n15m\n")
	app.Environment = []string{"DOPPELS_API_TOKEN=command-api-token"}
	app.HTTPClient = server.Client()
	app.Now = func() time.Time { return now }
	_ = app.Run([]string{"share", "--server", server.URL})
	if gotName != "other-task" {
		t.Fatalf("capability = %q, stderr = %s", gotName, stderr.String())
	}
	wantExpires := now.Add(15 * time.Minute).UTC().Truncate(time.Millisecond)
	if !gotExpires.Equal(wantExpires) {
		t.Fatalf("expires = %s, want %s", gotExpires, wantExpires)
	}
	if !strings.Contains(stdout.String(), "Waiting for request") {
		t.Fatalf("stdout = %s\nstderr = %s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "Share a Capability") || !strings.Contains(stderr.String(), "other-task@") {
		t.Fatalf("stderr prompts = %s", stderr.String())
	}
}

type shareHostWithShell struct{ unavailableHost }

func (shareHostWithShell) LookupCommand(name string) (string, error) {
	if name == "sh" {
		return "/bin/sh", nil
	}
	return unavailableHost{}.LookupCommand(name)
}

func TestShareBlocksWhenHostRequiresUnmet(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "answer.yaml", shareCapabilityFixture)
	writeManifest(t, root, "recipes", "pagerduty.yaml", shareShellRecipeFixture)

	app, _, stderr := testApp(root)
	app.Host = shareHostWithShell{}
	code := app.Run([]string{"share", "capability/answer-question", "--yes", "--server", "http://127.0.0.1:9"})
	if code != ExitOperational {
		t.Fatalf("exit = %d, want %d; stderr = %s", code, ExitOperational, stderr.String())
	}
	out := stderr.String()
	for _, want := range []string{
		"Recipe not ready",
		"answer-pagerduty",
		"recipes/pagerduty.yaml",
		"command pagerduty",
		"env PAGERDUTY_API_TOKEN",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{
		"host.command-missing",
		"host.env-missing",
		"requires.commands[0]",
		"required command",
	} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("stderr still dumps validator noise %q:\n%s", forbidden, out)
		}
	}
}

func TestShareRejectsInputsAndAmbiguousRecipesBeforeBootstrap(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "answer.yaml", shareCapabilityFixture)
	writeManifest(t, root, "recipes", "one.yaml", shareManualRecipeFixture)
	writeManifest(t, root, "recipes", "two.yaml", strings.Replace(shareManualRecipeFixture, "answer-manual", "answer-review", 1))

	t.Run("locked requires inputs", func(t *testing.T) {
		app, _, stderr := testApp(root)
		if code := app.Run([]string{"share", "capability/answer-question", "--locked"}); code != ExitContract {
			t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "--locked requires at least one --input") {
			t.Fatalf("stderr = %s", stderr.String())
		}
	})

	t.Run("Recipe selection is explicit without TTY", func(t *testing.T) {
		app, _, stderr := testApp(root)
		if code := app.Run([]string{"share", "capability/answer-question"}); code != ExitContract {
			t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "multiple compatible Recipes") {
			t.Fatalf("stderr = %s", stderr.String())
		}
		if !strings.Contains(stderr.String(), "--recipe") {
			t.Fatalf("expected --recipe hint, stderr = %s", stderr.String())
		}
	})

	t.Run("Recipe selection prompts when stdin drives picks", func(t *testing.T) {
		// Covered indirectly by interactive share tests; keep --json hard-fail here.
		app, _, stderr := testApp(root)
		if code := app.Run([]string{"share", "capability/answer-question", "--json"}); code != ExitContract {
			t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "multiple compatible Recipes") {
			t.Fatalf("stderr = %s", stderr.String())
		}
	})

	t.Run("expiry must be positive", func(t *testing.T) {
		app, _, stderr := testApp(root)
		if code := app.Run([]string{"share", "capability/answer-question", "--expires", "0"}); code != ExitContract {
			t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "positive duration") {
			t.Fatalf("stderr = %s", stderr.String())
		}
	})
}

func TestShareSendsArtifactRetentionDaysFromRecipe(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "answer.yaml", shareCapabilityFixture)
	recipe := shareManualRecipeFixture + "defaults:\n  artifactRetentionDays: 14\n"
	writeManifest(t, root, "recipes", "one.yaml", recipe)
	now := time.Now().UTC().Truncate(time.Millisecond)
	var got shareclient.CreateShareRequest
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/api/v1/shares", func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(shareclient.ShareCreated{
			APIVersion: shareclient.APIVersion,
			Kind:       "ShareCreated",
			Share: shareclient.Share{
				APIVersion:            shareclient.APIVersion,
				Kind:                  "Share",
				ID:                    commandShareID,
				CreatedAt:             now,
				ExpiresAt:             got.ExpiresAt,
				CapabilityRevision:    got.CapabilityRevision,
				Capability:            got.Capability,
				Recipe:                got.Recipe,
				SharedBy:              execution.ActorReference{Kind: "anonymous", ID: "anonymous"},
				RequestLimit:          1,
				ArtifactRetentionDays: got.ArtifactRetentionDays,
			},
			PublicURL:   server.URL + "/s/public",
			RunnerToken: commandRunnerToken,
		})
	})
	mux.HandleFunc("/socket/websocket", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	})

	app, _, stderr := testApp(root)
	app.HTTPClient = server.Client()
	app.Now = func() time.Time { return now }
	_ = app.Run([]string{
		"share", "capability/answer-question", "--recipe", "answer-manual",
		"--server", server.URL, "--expires", "1h",
	})
	if got.ArtifactRetentionDays != 14 {
		t.Fatalf("artifactRetentionDays = %d, want 14; stderr = %s", got.ArtifactRetentionDays, stderr.String())
	}
}

func TestShareDefaultsArtifactRetentionDaysToSeven(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "answer.yaml", shareCapabilityFixture)
	writeManifest(t, root, "recipes", "one.yaml", shareManualRecipeFixture)
	now := time.Now().UTC().Truncate(time.Millisecond)
	var got shareclient.CreateShareRequest
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/api/v1/shares", func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(shareclient.ShareCreated{
			APIVersion: shareclient.APIVersion,
			Kind:       "ShareCreated",
			Share: shareclient.Share{
				APIVersion:         shareclient.APIVersion,
				Kind:               "Share",
				ID:                 commandShareID,
				CreatedAt:          now,
				ExpiresAt:          got.ExpiresAt,
				CapabilityRevision: got.CapabilityRevision,
				Capability:         got.Capability,
				Recipe:             got.Recipe,
				SharedBy:           execution.ActorReference{Kind: "anonymous", ID: "anonymous"},
				RequestLimit:       1,
			},
			PublicURL:   server.URL + "/s/public",
			RunnerToken: commandRunnerToken,
		})
	})
	mux.HandleFunc("/socket/websocket", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	})

	app, _, stderr := testApp(root)
	app.HTTPClient = server.Client()
	app.Now = func() time.Time { return now }
	_ = app.Run([]string{
		"share", "capability/answer-question", "--recipe", "answer-manual",
		"--server", server.URL, "--expires", "1h",
	})
	if got.ArtifactRetentionDays != 7 {
		t.Fatalf("artifactRetentionDays = %d, want 7; stderr = %s", got.ArtifactRetentionDays, stderr.String())
	}
}

func TestShareSendsPrefillInputsAndLockedFlag(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "answer.yaml", shareCapabilityFixture)
	writeManifest(t, root, "recipes", "one.yaml", shareManualRecipeFixture)
	now := time.Now().UTC().Truncate(time.Millisecond)
	var got shareclient.CreateShareRequest
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/api/v1/shares", func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(shareclient.ShareCreated{
			APIVersion: shareclient.APIVersion,
			Kind:       "ShareCreated",
			Share: shareclient.Share{
				APIVersion:         shareclient.APIVersion,
				Kind:               "Share",
				ID:                 commandShareID,
				CreatedAt:          now,
				ExpiresAt:          got.ExpiresAt,
				CapabilityRevision: got.CapabilityRevision,
				Capability:         got.Capability,
				Recipe:             got.Recipe,
				SharedBy:           execution.ActorReference{Kind: "anonymous", ID: "anonymous"},
				RequestLimit:       1,
				Inputs:             got.Inputs,
				InputsLocked:       got.InputsLocked,
			},
			PublicURL:   server.URL + "/s/public",
			RunnerToken: commandRunnerToken,
		})
	})
	mux.HandleFunc("/socket/websocket", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	})

	app, _, stderr := testApp(root)
	app.HTTPClient = server.Client()
	app.Now = func() time.Time { return now }
	_ = app.Run([]string{
		"share", "capability/answer-question", "--recipe", "answer-manual",
		"--server", server.URL, "--expires", "1h",
		"--input", "question=prefilled", "--locked",
	})
	if got.Inputs["question"] != "prefilled" {
		t.Fatalf("inputs = %#v, stderr = %s", got.Inputs, stderr.String())
	}
	if !got.InputsLocked {
		t.Fatal("expected inputsLocked")
	}
}

type commandPhoenixFrame struct {
	JoinRef *string
	Ref     *string
	Topic   string
	Event   string
	Payload json.RawMessage
}

func readCommandPhoenix(ctx context.Context, connection *websocket.Conn) (commandPhoenixFrame, error) {
	messageType, data, err := connection.Read(ctx)
	if err != nil {
		return commandPhoenixFrame{}, err
	}
	if messageType != websocket.MessageText {
		return commandPhoenixFrame{}, fmt.Errorf("unexpected WebSocket message type %v", messageType)
	}
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil || len(values) != 5 {
		return commandPhoenixFrame{}, fmt.Errorf("invalid Phoenix frame: %s", data)
	}
	frame := commandPhoenixFrame{Payload: values[4]}
	if string(values[0]) != "null" {
		_ = json.Unmarshal(values[0], &frame.JoinRef)
	}
	if string(values[1]) != "null" {
		_ = json.Unmarshal(values[1], &frame.Ref)
	}
	if err := json.Unmarshal(values[2], &frame.Topic); err != nil {
		return commandPhoenixFrame{}, err
	}
	if err := json.Unmarshal(values[3], &frame.Event); err != nil {
		return commandPhoenixFrame{}, err
	}
	return frame, nil
}

func writeCommandPhoenixReply(ctx context.Context, connection *websocket.Conn, request commandPhoenixFrame, response any) error {
	payload := map[string]any{"status": "ok", "response": response}
	frame := []any{request.JoinRef, request.Ref, request.Topic, "phx_reply", payload}
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return connection.Write(ctx, websocket.MessageText, data)
}

const shareCapabilityFixture = `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: answer-question, version: 1.0.0}
inputs:
  question: {type: string, required: true}
outputs:
  answer: {type: string}
`

const shareManualRecipeFixture = `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: answer-manual, version: 1.0.0}
provides: [answer-question]
runtime: manual
procedure: {readme: ./answer.md}
evidence:
  note: {type: string}
`

const shareShellRecipeFixture = `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: answer-pagerduty, version: 1.0.0}
provides: [answer-question]
runtime: shell
requires:
  commands: [pagerduty]
  hostEnv: [PAGERDUTY_API_TOKEN]
defaults: {approval: never}
steps:
  - id: page
    name: Page
    run: {shell: sh, script: export ANSWER=paged}
    produces:
      answer: {env: ANSWER}
returns:
  answer: "{{ steps.page.answer }}"
`

func TestShareStrictBlocksStaleLockPin(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	writeStaleCapabilityLock(t, root)

	app, _, stderr := testApp(root)
	code := app.Run([]string{"share", "capability/greet", "--input", "name=Ada", "--yes", "--strict", "--json", "--server", "http://127.0.0.1:1"})
	if code != ExitContract {
		t.Fatalf("strict stale share exit = %d, want %d, stderr = %s", code, ExitContract, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, "stale") || !strings.Contains(out, "version bump") {
		t.Fatalf("strict stale share stderr = %s", out)
	}
}
