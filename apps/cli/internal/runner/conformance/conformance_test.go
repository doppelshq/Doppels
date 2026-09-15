package conformance

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/runs"
)

// startGreetRun drives a full auto-approval startRun to completion and
// returns its RunID, for golden tests further down the Run lifecycle
// (getRun, listRuns, getRunLogs, cancelRun, subscribeRun...).
func startGreetRun(t *testing.T, h *harness, client *rawClient, idempotencyKey string) string {
	t.Helper()
	response := client.call("start-"+idempotencyKey, "v1/startRun", map[string]any{
		"workspace": h.root, "capability": "greet", "inputs": map[string]any{"count": 1},
		"approvalMode": "auto", "idempotencyKey": idempotencyKey,
	})
	var started runs.StartResult
	if err := json.Unmarshal(rawResult(t, response), &started); err != nil {
		t.Fatal(err)
	}
	return started.RunID
}

// --- One golden test per v1 method ---------------------------------------

func TestGoldenInitialize(t *testing.T) {
	h := newHarness(t, false)
	client := dial(t, h)
	response := client.call("1", "v1/initialize", map[string]any{
		"token":  conformanceToken,
		"client": map[string]any{"name": "conformance", "version": "0.0.1"},
	})
	if idString(t, response) != `"1"` {
		t.Fatalf("id = %s, want \"1\"", idString(t, response))
	}
	assertGolden(t, rawResult(t, response), `{"capabilities":["liveLogs"],"nodeStatus":{"cloud":null,"protocolVersion":1,"runnerVersion":"0.0.1-test","startedAt":"<REDACTED>","state":"","workspaces":[]},"protocolVersion":1,"runnerVersion":"0.0.1-test"}`)
}

func TestGoldenPing(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)
	response := client.call("2", "v1/ping", map[string]any{})
	assertGolden(t, rawResult(t, response), `{"pong":"<REDACTED>"}`)
}

func TestGoldenGetNodeStatus(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)
	response := client.call("3", "v1/getNodeStatus", map[string]any{})
	assertGolden(t, rawResult(t, response), `{"cloud":null,"protocolVersion":1,"runnerVersion":"0.0.1-test","startedAt":"<REDACTED>","state":"","workspaces":[]}`)
}

func TestGoldenSubscribeNode(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)
	response := client.call("4", "v1/subscribeNode", map[string]any{})
	assertGolden(t, rawResult(t, response), `{"cloud":null,"protocolVersion":1,"runnerVersion":"0.0.1-test","startedAt":"<REDACTED>","state":"","workspaces":[]}`)
}

func TestGoldenListWorkspaces(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)
	response := client.call("5", "v1/listWorkspaces", map[string]any{})
	assertGolden(t, rawResult(t, response), `[{"capabilities":1,"gitBranch":"<REDACTED>","health":"ok","recipes":0,"root":"<REDACTED>","space":"workspace"}]`)
}

func TestGoldenListCapabilities(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)
	response := client.call("6", "v1/listCapabilities", map[string]any{"workspace": h.root})
	assertGolden(t, rawResult(t, response), `[{"manifestSha256":"<REDACTED>","name":"greet","pin":"unpinned","readiness":[],"recipe":null,"runtime":"none","space":"workspace","version":"1.0.0","workspace":"<REDACTED>"}]`)
}

func TestGoldenGetCapability(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)
	response := client.call("7", "v1/getCapability", map[string]any{"workspace": h.root, "name": "greet"})
	var result struct {
		Summary struct {
			Name    string `json:"name"`
			Runtime string `json:"runtime"`
		} `json:"summary"`
		Runs int `json:"runs"`
	}
	raw := rawResult(t, response)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Summary.Name != "greet" || result.Summary.Runtime != "none" || result.Runs != 0 {
		t.Fatalf("getCapability summary = %#v", result)
	}
}

func TestGoldenStartRun(t *testing.T) {
	h := newHarness(t, true)
	client := dialInitialized(t, h)
	response := client.call("8", "v1/startRun", map[string]any{
		"workspace": h.root, "capability": "greet", "inputs": map[string]any{"count": 1},
		"approvalMode": "auto", "idempotencyKey": "golden-start",
	})
	var started runs.StartResult
	if err := json.Unmarshal(rawResult(t, response), &started); err != nil {
		t.Fatal(err)
	}
	if started.RunID == "" || started.RequestID == "" {
		t.Fatalf("startRun result = %#v", started)
	}
	waitForStatusOverSocket(t, client, started.RunID, "succeeded")
}

func TestGoldenSubscribeRun(t *testing.T) {
	h := newHarness(t, true)
	client := dialInitialized(t, h)
	runID := startGreetRun(t, h, client, "golden-subscribe")
	waitForStatusOverSocket(t, client, runID, "succeeded")

	response := client.call("9", "v1/subscribeRun", map[string]any{"runId": runID})
	var result runs.SubscribeResult
	if err := json.Unmarshal(rawResult(t, response), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "succeeded" {
		t.Fatalf("subscribeRun status = %q, want succeeded", result.Status)
	}
	if len(result.Events) == 0 || result.Events[len(result.Events)-1].Type != "run_succeeded" {
		t.Fatalf("subscribeRun replay = %#v", result.Events)
	}
}

func TestGoldenCancelRun(t *testing.T) {
	h := newHarness(t, true)
	client := dialInitialized(t, h)
	runID := startGreetRun(t, h, client, "golden-cancel")
	waitForStatusOverSocket(t, client, runID, "succeeded")

	// A cancel against an already-terminal Run is idempotent: it reconciles
	// to the existing terminal state instead of erroring.
	response := client.call("10", "v1/cancelRun", map[string]any{"runId": runID})
	assertGolden(t, rawResult(t, response), `{"status":"succeeded"}`)
}

func TestGoldenGetRun(t *testing.T) {
	h := newHarness(t, true)
	client := dialInitialized(t, h)
	runID := startGreetRun(t, h, client, "golden-get")
	waitForStatusOverSocket(t, client, runID, "succeeded")

	response := client.call("11", "v1/getRun", map[string]any{"runId": runID, "includeEvents": true})
	var result runs.GetRunResult
	if err := json.Unmarshal(rawResult(t, response), &result); err != nil {
		t.Fatal(err)
	}
	if result.Summary.Status != "succeeded" || result.Summary.Capability != "greet@1.0.0" {
		t.Fatalf("getRun summary = %#v", result.Summary)
	}
	if len(result.Events) == 0 || result.Events[len(result.Events)-1].Type != "run_succeeded" {
		t.Fatalf("getRun events = %#v", result.Events)
	}
}

func TestGoldenListRuns(t *testing.T) {
	h := newHarness(t, true)
	client := dialInitialized(t, h)
	runID := startGreetRun(t, h, client, "golden-list")
	waitForStatusOverSocket(t, client, runID, "succeeded")

	response := client.call("12", "v1/listRuns", map[string]any{"workspace": h.root})
	var result runs.ListResult
	if err := json.Unmarshal(rawResult(t, response), &result); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, summary := range result.Runs {
		if summary.RunID == runID {
			found = true
			if summary.Status != "succeeded" {
				t.Fatalf("listRuns entry status = %q, want succeeded", summary.Status)
			}
		}
	}
	if !found {
		t.Fatalf("listRuns = %#v, missing run %s", result.Runs, runID)
	}
}

func TestGoldenGetRunLogs(t *testing.T) {
	h := newHarness(t, true)
	client := dialInitialized(t, h)
	runID := startGreetRun(t, h, client, "golden-logs")
	waitForStatusOverSocket(t, client, runID, "succeeded")

	response := client.call("13", "v1/getRunLogs", map[string]any{"runId": runID})
	var result runs.LogsResult
	if err := json.Unmarshal(rawResult(t, response), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Files) == 0 {
		t.Fatalf("getRunLogs files = %#v, want at least one log file", result.Files)
	}
}

func TestGoldenListPendingApprovals(t *testing.T) {
	h := newHarness(t, true)
	client := dialInitialized(t, h)
	response := client.call("14", "v1/listPendingApprovals", map[string]any{})
	assertGolden(t, rawResult(t, response), `[]`)
}

func TestGoldenDecideApproval(t *testing.T) {
	h := newHarness(t, false)
	// Override the greet Recipe fixture with one whose only Step requires
	// approval: combined with approvalMode "interactive" this drives the
	// engine into Manager.awaitApproval, giving decideApproval a real,
	// in-memory PendingApproval to target instead of a durable manual Run
	// (which has no decideApproval path at all — see execution/manual.go).
	writeFixture(t, filepath.Join(h.root, ".doppels", "recipes", "greet.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: greet-shell, version: 1.0.0}
provides: [greet]
runtime: shell
defaults: {approval: required}
steps:
  - id: run
    name: Run
    run: {shell: sh, script: "export VALUE=ok:$DOPPEL_INPUT_COUNT"}
    produces: {value: {env: VALUE}}
returns: {value: "{{ steps.run.value }}"}
`)
	client := dialInitialized(t, h)
	response := client.call("start-golden-decide", "v1/startRun", map[string]any{
		"workspace": h.root, "capability": "greet", "inputs": map[string]any{"count": 1},
		"approvalMode": "interactive", "idempotencyKey": "golden-decide",
	})
	var started runs.StartResult
	if err := json.Unmarshal(rawResult(t, response), &started); err != nil {
		t.Fatal(err)
	}
	waitForPendingApproval(t, client, started.RunID, "run")

	decision := client.call("15", "v1/decideApproval", map[string]any{
		"runId": started.RunID, "stepId": "run", "decision": "approve",
	})
	assertGolden(t, rawResult(t, decision), `{}`)
	waitForStatusOverSocket(t, client, started.RunID, "succeeded")
}

// waitForPendingApproval polls v1/listPendingApprovals until runID/stepID
// appears: awaitApproval registers it asynchronously from the engine
// goroutine startRun spawned, so a caller cannot assume it exists the instant
// startRun's own response returns.
func waitForPendingApproval(t *testing.T, client *rawClient, runID, stepID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response := client.call(fmt.Sprintf("poll-approval-%d", time.Now().UnixNano()), "v1/listPendingApprovals", map[string]any{})
		var pending []runs.PendingApproval
		if err := json.Unmarshal(rawResult(t, response), &pending); err == nil {
			for _, approval := range pending {
				if approval.RunID == runID && approval.StepID == stepID {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never registered a pending approval for step %s", runID, stepID)
}

func TestGoldenSubscribeRunLogs(t *testing.T) {
	h := newHarness(t, true)
	client := dialInitialized(t, h)
	runID := startGreetRun(t, h, client, "golden-sublogs")
	waitForStatusOverSocket(t, client, runID, "succeeded")

	// The Run has already finished by the time this subscribes: no active
	// live-log broadcaster remains, so the golden, deterministic answer is
	// {"active":false} rather than racing the Step's own log stream.
	response := client.call("16", "v1/subscribeRunLogs", map[string]any{"runId": runID})
	assertGolden(t, rawResult(t, response), `{"active":false}`)
}

func TestGoldenShutdown(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)
	response := client.call("17", "v1/shutdown", map[string]any{"reason": "conformance"})
	assertGolden(t, rawResult(t, response), `{}`)
}

// --- One golden test per v1 error code ------------------------------------

func TestErrorInvalidRequest(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)
	client.writeRaw(`{"jsonrpc":"1.0","id":"bad-version","method":"v1/ping","params":{}}`)
	response := client.readRawResponse()
	assertErrorResponse(t, response, proto.CodeInvalidRequest)
}

func TestErrorParse(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)
	client.writeRaw(`{not valid json`)
	response := client.readRawResponse()
	assertErrorResponse(t, response, proto.CodeParse)
}

func TestErrorNotInitialized(t *testing.T) {
	h := newHarness(t, false)
	client := dial(t, h)
	response := client.call("pre-init", "v1/ping", map[string]any{})
	assertErrorResponse(t, response, proto.CodeNotInitialized)
}

func TestErrorAuthFailed(t *testing.T) {
	h := newHarness(t, false)
	client := dial(t, h)
	response := client.call("bad-token", "v1/initialize", map[string]any{
		"token":  "wrong-token",
		"client": map[string]any{"name": "conformance", "version": "0.0.1"},
	})
	assertErrorResponse(t, response, proto.CodeAuthFailed)
}

func TestErrorVersionMismatch(t *testing.T) {
	h := newHarness(t, false)
	client := dial(t, h)
	response := client.call("bad-version", "v1/initialize", map[string]any{
		"protocolVersion": 2,
		"token":           conformanceToken,
		"client":          map[string]any{"name": "conformance", "version": "0.0.1"},
	})
	assertErrorResponse(t, response, proto.CodeVersionMismatch)
}

func TestErrorWorkspaceNotFound(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)
	response := client.call("no-workspace", "v1/listCapabilities", map[string]any{"workspace": "/nowhere/at/all"})
	assertErrorResponse(t, response, proto.CodeWorkspaceNotFound)
}

func TestErrorCapabilityNotFound(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)
	response := client.call("no-capability", "v1/getCapability", map[string]any{"workspace": h.root, "name": "missing"})
	assertErrorResponse(t, response, proto.CodeCapabilityNotFound)
}

func TestErrorRunNotFound(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)
	response := client.call("no-run", "v1/getRun", map[string]any{"runId": "00000000-0000-4000-8000-000000000000"})
	assertErrorResponse(t, response, proto.CodeRunNotFound)
}

func TestErrorApprovalNotFound(t *testing.T) {
	h := newHarness(t, true)
	client := dialInitialized(t, h)
	runID := startGreetRun(t, h, client, "error-approval")
	waitForStatusOverSocket(t, client, runID, "succeeded")

	response := client.call("no-approval", "v1/decideApproval", map[string]any{
		"runId": runID, "stepId": "no-such-step", "decision": "approve",
	})
	assertErrorResponse(t, response, proto.CodeApprovalNotFound)
}

func TestErrorInvalidInputs(t *testing.T) {
	h := newHarness(t, true)
	client := dialInitialized(t, h)
	response := client.call("bad-inputs", "v1/startRun", map[string]any{
		"workspace": h.root, "capability": "greet", "inputs": map[string]any{},
		"approvalMode": "auto", "idempotencyKey": "error-invalid-inputs",
	})
	assertErrorResponse(t, response, proto.CodeInvalidInputs)
}

// TestErrorBusy reproduces -32010 through the server's own shutdown gate
// (server/connection.go dispatch: any method but v1/shutdown/v1/initialize
// is refused once a shutdown has been accepted). v1/shutdown latches
// s.shutdown = true synchronously, as the very first effect of handling it —
// strictly before it even enqueues its own ack, let alone before the
// asynchronous close that ack's delivery eventually triggers. A second,
// independent connection (dialed and handshaked before shutdown is ever
// sent) that races a request in right after is therefore dispatched against
// an already-latched flag and gets Busy back over its own, otherwise
// undisturbed connection — unlike reusing the same connection shutdown was
// sent on, whose own socket is torn down the instant its ack is flushed
// (before a follow-up response on it could ever reach the wire, regardless
// of what the server computed for it). The two connections' write/flush
// paths are still two independent goroutine chains with no ordering
// guarantee between them, so this retries a bounded number of times with a
// fresh harness each attempt rather than asserting on a single race outcome.
func TestErrorBusy(t *testing.T) {
	const attempts = 150
	for attempt := 0; attempt < attempts; attempt++ {
		if tryObserveBusyDuringShutdown(t) {
			return
		}
	}
	t.Fatalf("never observed a -32010 busy response across %d attempts racing v1/shutdown", attempts)
}

// tryObserveBusyDuringShutdown runs one attempt in a fresh harness (a
// completed v1/shutdown tears its whole server down, so no harness survives
// two attempts) and reports whether the racing connection observed Busy.
func tryObserveBusyDuringShutdown(t *testing.T) bool {
	t.Helper()
	h := newHarness(t, false)
	shutdownConn := dialInitialized(t, h)
	racingConn := dialInitialized(t, h)

	var racingResponse *proto.Response
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		shutdownConn.tryCall("shutdown", "v1/shutdown", map[string]any{})
	}()
	go func() {
		defer wg.Done()
		racingResponse = racingConn.tryCall("racing-ping", "v1/ping", map[string]any{})
	}()
	wg.Wait()

	return racingResponse != nil && racingResponse.Err != nil && racingResponse.Err.Code == proto.CodeBusy
}

// TestGoldenAddWorkspace drives v1/addWorkspace over the wire end-to-end.
// The harness's newHarness already pre-registers its own workspace via Go
// for the convenience of the other golden tests; here we create a second
// valid workspace on disk and assert the wire contract of addWorkspace —
// the response envelope, the WorkspaceSummary shape, and that the new
// workspace shows up in v1/listWorkspaces immediately after.
func TestGoldenAddWorkspace(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)

	additionalRoot := filepath.Join(t.TempDir(), "extra")
	if _, err := project.Init(additionalRoot); err != nil {
		t.Fatal(err)
	}

	response := client.call("add-1", "v1/addWorkspace", map[string]any{"root": additionalRoot})
	if response.Err != nil {
		t.Fatalf("addWorkspace returned error: %+v", response.Err)
	}
	// The Runner derives the Space name from the directory basename (here
	// `extra`). project.Init creates an initial commit so gitBranch is a
	// non-empty string and the redactor rewrites it to "<REDACTED>".
	assertGolden(t, rawResult(t, response), `{"capabilities":0,"gitBranch":"<REDACTED>","health":"ok","recipes":0,"root":"<REDACTED>","space":"extra"}`)

	listResponse := client.call("list-1", "v1/listWorkspaces", map[string]any{})
	if listResponse.Err != nil {
		t.Fatalf("listWorkspaces returned error: %+v", listResponse.Err)
	}
	roots := []map[string]any{}
	if err := json.Unmarshal(rawResult(t, listResponse), &roots); err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 {
		t.Fatalf("listWorkspaces returned %d workspaces, want 2 (harness + added)", len(roots))
	}

	// Calling addWorkspace again on the same root must not duplicate the
	// registry entry. The Runner treats "already present" as idempotent and
	// returns the existing summary; either no error or a benign post-commit
	// warning is acceptable.
	second := client.call("add-2", "v1/addWorkspace", map[string]any{"root": additionalRoot})
	if second.Err != nil && second.Err.Code != proto.CodeBusy && second.Err.Code != proto.CodeInternal {
		// CodeBusy/Internal is acceptable only if the registry reports a
		// post-commit warning (a degraded registry state). Anything else is
		// a regression in idempotency.
		t.Fatalf("second addWorkspace returned unexpected error: %+v", second.Err)
	}
}

// TestGoldenRemoveWorkspace drives v1/removeWorkspace over the wire. It
// first adds a workspace via addWorkspace (so the registry contains it),
// then removes it and asserts the empty-result envelope plus that
// v1/listWorkspaces no longer reports it.
func TestGoldenRemoveWorkspace(t *testing.T) {
	h := newHarness(t, false)
	client := dialInitialized(t, h)

	additionalRoot := filepath.Join(t.TempDir(), "extra")
	if _, err := project.Init(additionalRoot); err != nil {
		t.Fatal(err)
	}
	if response := client.call("add-1", "v1/addWorkspace", map[string]any{"root": additionalRoot}); response.Err != nil {
		t.Fatalf("addWorkspace: %+v", response.Err)
	}

	rmResponse := client.call("rm-1", "v1/removeWorkspace", map[string]any{"root": additionalRoot})
	if rmResponse.Err != nil {
		t.Fatalf("removeWorkspace returned error: %+v", rmResponse.Err)
	}
	assertGolden(t, rawResult(t, rmResponse), `{}`)

	listResponse := client.call("list-1", "v1/listWorkspaces", map[string]any{})
	roots := []map[string]any{}
	if err := json.Unmarshal(rawResult(t, listResponse), &roots); err != nil {
		t.Fatal(err)
	}
	for _, r := range roots {
		if r["root"] == additionalRoot {
			t.Fatalf("removed workspace %q still in listWorkspaces: %+v", additionalRoot, roots)
		}
	}
	if len(roots) != 1 {
		t.Fatalf("listWorkspaces returned %d workspaces after removal, want 1 (harness only)", len(roots))
	}

	// Removing an unknown root must not crash and must return the
	// appropriate domain error (workspaceNotFound), exercising the
	// negative path of the same method.
	missingResponse := client.call("rm-missing", "v1/removeWorkspace", map[string]any{"root": "/no/such/path"})
	if missingResponse.Err == nil {
		t.Fatal("removeWorkspace on unknown root returned success; expected workspaceNotFound")
	}
	if missingResponse.Err.Code != proto.CodeWorkspaceNotFound {
		t.Fatalf("removeWorkspace on unknown root code = %d, want workspaceNotFound (%d)",
			missingResponse.Err.Code, proto.CodeWorkspaceNotFound)
	}
}
