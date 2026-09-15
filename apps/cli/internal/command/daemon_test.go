package command

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runnerclient"
)

// fakeDaemon is an in-memory daemonClient: tests record the calls the CLI
// routing makes and return canned results. Only the methods a test actually
// exercises need non-zero behavior; the rest embed the interface so the
// compiler still catches signature drift.
type fakeDaemon struct {
	started    []runnerclient.StartRunParams
	startErr   error
	startRunID string

	subscribedRun    string
	subscribedFrom   int
	subscribeEvents  []proto.RunEventPayload
	subscribeErr     error
	subscribeStatus  string
	subscribeCalls   []int
	subscribeResults []runnerclient.SubscribeRunResult
	subscribeHooks   []func()
	notificationFunc func(runnerclient.Notification)

	decided   []string
	decideErr error
	onDecide  func()

	getRunIncludes []bool
	recoveryEvents []proto.RunEventPayload
	getRunResults  []runnerclient.GetRunResult
	getRunHooks    []func()
	getRunCalls    int

	gotRuns    runnerclient.ListRunsParams
	runsResult runnerclient.ListRunsResult
	runsErr    error

	statusResult proto.NodeStatus
	statusErr    error

	closed bool
}

func (f *fakeDaemon) StartRun(ctx context.Context, params runnerclient.StartRunParams) (runnerclient.StartRunResult, error) {
	f.started = append(f.started, params)
	if f.startErr != nil {
		return runnerclient.StartRunResult{}, f.startErr
	}
	return runnerclient.StartRunResult{RequestID: "req-1", RunID: f.startRunID}, nil
}

func (f *fakeDaemon) SubscribeRun(ctx context.Context, runID string, fromSequence int) (runnerclient.SubscribeRunResult, error) {
	f.subscribedRun, f.subscribedFrom = runID, fromSequence
	call := len(f.subscribeCalls)
	f.subscribeCalls = append(f.subscribeCalls, fromSequence)
	if call < len(f.subscribeHooks) && f.subscribeHooks[call] != nil {
		f.subscribeHooks[call]()
	}
	if f.subscribeErr != nil {
		return runnerclient.SubscribeRunResult{}, f.subscribeErr
	}
	if call < len(f.subscribeResults) {
		return f.subscribeResults[call], nil
	}
	return runnerclient.SubscribeRunResult{Events: f.subscribeEvents, Status: f.subscribeStatus}, nil
}

func (f *fakeDaemon) GetRun(ctx context.Context, runID string, includeEvents bool) (runnerclient.GetRunResult, error) {
	f.getRunIncludes = append(f.getRunIncludes, includeEvents)
	if includeEvents {
		call := f.getRunCalls
		f.getRunCalls++
		if call < len(f.getRunHooks) && f.getRunHooks[call] != nil {
			f.getRunHooks[call]()
		}
		if call < len(f.getRunResults) {
			return f.getRunResults[call], nil
		}
		return runnerclient.GetRunResult{
			Summary: proto.RunSummary{RunID: runID, Status: "running"},
			Events:  f.recoveryEvents,
		}, nil
	}
	return runnerclient.GetRunResult{Summary: proto.RunSummary{
		RunID: runID, Status: "succeeded", Capability: "greet", RequestID: "req-1",
		CreatedAt: "2026-09-15T12:00:00Z",
	}}, nil
}

func (f *fakeDaemon) DecideApproval(ctx context.Context, runID, stepID, decision string) error {
	f.decided = append(f.decided, runID+":"+stepID+":"+decision)
	if f.onDecide != nil {
		f.onDecide()
	}
	return f.decideErr
}

func (f *fakeDaemon) GetNodeStatus(ctx context.Context) (proto.NodeStatus, error) {
	return f.statusResult, f.statusErr
}

func (f *fakeDaemon) ListRuns(ctx context.Context, params runnerclient.ListRunsParams) (runnerclient.ListRunsResult, error) {
	f.gotRuns = params
	return f.runsResult, f.runsErr
}

func (f *fakeDaemon) SetNotificationHandler(handler func(runnerclient.Notification)) {
	f.notificationFunc = handler
}

func (f *fakeDaemon) Close() error {
	f.closed = true
	return nil
}

// daemonFixtureApp builds a testApp whose catalog holds the standard shell
// greet fixture and whose daemon routing is faked.
func daemonFixtureApp(t *testing.T) (*App, *strings.Builder, *strings.Builder) {
	t.Helper()
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, _, _ := testApp(root)
	app.Stdout = &strings.Builder{}
	app.Stderr = &strings.Builder{}
	return app, app.Stdout.(*strings.Builder), app.Stderr.(*strings.Builder)
}

// TestRunRoutesShellRecipeToDaemon pins the PR8 routing contract: a shell
// Recipe (the only runtime representable over the IPC v1) goes to the daemon
// when its socket is reachable, and the CLI streams the replayed terminal
// event to a success exit — never executing the step locally.
func TestRunRoutesShellRecipeToDaemon(t *testing.T) {
	app, _, stderr := daemonFixtureApp(t)
	daemon := &fakeDaemon{
		startRunID: "run-daemon-1",
		subscribeEvents: []proto.RunEventPayload{
			{RunID: "run-daemon-1", Sequence: 0, Type: "run_created", OccurredAt: "2026-09-15T12:00:00Z"},
			{RunID: "run-daemon-1", Sequence: 1, Type: "run_succeeded", OccurredAt: "2026-09-15T12:00:01Z"},
		},
		subscribeStatus: "succeeded",
	}
	app.DialRunner = func(ctx context.Context, opts runnerclient.Options) (daemonClient, error) {
		return daemon, nil
	}

	code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if len(daemon.started) != 1 {
		t.Fatalf("startRun calls = %d, want 1", len(daemon.started))
	}
	started := daemon.started[0]
	if started.Capability != "greet@1.0.0" {
		t.Fatalf("capability = %q, want greet@1.0.0", started.Capability)
	}
	if started.ApprovalMode != "interactive" {
		t.Fatalf("approvalMode = %q, want interactive (no --yes)", started.ApprovalMode)
	}
	if started.IdempotencyKey == "" {
		t.Fatal("idempotencyKey must be generated for daemon runs")
	}
	if daemon.subscribedRun != "run-daemon-1" || daemon.subscribedFrom != 0 {
		t.Fatalf("subscribeRun = (%q, %d), want (run-daemon-1, 0)", daemon.subscribedRun, daemon.subscribedFrom)
	}
	if !daemon.closed {
		t.Fatal("daemon client must be closed after the command finishes")
	}
}

// TestRunAutoYesMapsToAutoApprovalMode pins that --yes rides through to the
// daemon as approvalMode=auto instead of prompting locally.
func TestRunAutoYesMapsToAutoApprovalMode(t *testing.T) {
	app, _, stderr := daemonFixtureApp(t)
	daemon := &fakeDaemon{
		startRunID: "run-auto",
		subscribeEvents: []proto.RunEventPayload{
			{RunID: "run-auto", Sequence: 0, Type: "run_succeeded", OccurredAt: "2026-09-15T12:00:00Z"},
		},
	}
	app.DialRunner = func(ctx context.Context, opts runnerclient.Options) (daemonClient, error) {
		return daemon, nil
	}

	code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada", "--yes"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if daemon.started[0].ApprovalMode != "auto" {
		t.Fatalf("approvalMode = %q, want auto (--yes)", daemon.started[0].ApprovalMode)
	}
}

func TestRunDecidesApprovalRequestFromSubscribeReplay(t *testing.T) {
	app, _, stderr := daemonFixtureApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	app.Context = ctx
	app.Stdin = strings.NewReader("yes\n")

	daemon := &fakeDaemon{
		startRunID: "run-replayed-approval",
		subscribeEvents: []proto.RunEventPayload{{
			RunID: "run-replayed-approval", Sequence: 1, Type: "approval_requested",
			StepID: "step1", OccurredAt: "2026-09-15T12:00:00Z",
		}},
	}
	daemon.onDecide = func() {
		daemon.notificationFunc(runnerclient.Notification{
			Method: "v1/runEvent",
			Params: json.RawMessage(`{"runId":"run-replayed-approval","sequence":2,"type":"run_succeeded","occurredAt":"2026-09-15T12:00:01Z"}`),
		})
	}
	app.DialRunner = func(ctx context.Context, opts runnerclient.Options) (daemonClient, error) {
		return daemon, nil
	}

	code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if len(daemon.decided) != 1 || daemon.decided[0] != "run-replayed-approval:step1:approve" {
		t.Fatalf("decideApproval calls = %v, want exactly [run-replayed-approval:step1:approve]", daemon.decided)
	}
}

func TestRunRecoversFromRunEventGap(t *testing.T) {
	app, _, stderr := daemonFixtureApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	app.Context = ctx

	daemon := &fakeDaemon{
		startRunID: "run-gap",
		recoveryEvents: []proto.RunEventPayload{
			{RunID: "run-gap", Sequence: 0, Type: "run_created", OccurredAt: "2026-09-15T12:00:00Z"},
			{RunID: "run-gap", Sequence: 1, Type: "step_started", StepID: "run", OccurredAt: "2026-09-15T12:00:01Z"},
		},
		subscribeResults: []runnerclient.SubscribeRunResult{
			{Status: "running"},
			{Status: "succeeded", Events: []proto.RunEventPayload{{
				RunID: "run-gap", Sequence: 2, Type: "run_succeeded", OccurredAt: "2026-09-15T12:00:02Z",
			}}},
		},
	}
	daemon.subscribeHooks = []func(){func() {
		daemon.notificationFunc(runnerclient.Notification{
			Method: "v1/nodeEvent",
			Params: json.RawMessage(`{"kind":"runEventGap","payload":{"runId":"run-gap","fromSequence":0}}`),
		})
	}}
	app.DialRunner = func(ctx context.Context, opts runnerclient.Options) (daemonClient, error) {
		return daemon, nil
	}

	code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if len(daemon.getRunIncludes) < 2 || !daemon.getRunIncludes[0] {
		t.Fatalf("getRun includeEvents calls = %v, want recovery call with true before final lookup", daemon.getRunIncludes)
	}
	if len(daemon.subscribeCalls) != 2 || daemon.subscribeCalls[1] != 2 {
		t.Fatalf("subscribeRun fromSequence calls = %v, want [0 2]", daemon.subscribeCalls)
	}
	if !strings.Contains(stderr.String(), "Run") {
		t.Fatalf("timeline did not render recovered step_started event: %s", stderr.String())
	}
}

func TestRunRecoversFromLocalEventBufferOverflow(t *testing.T) {
	app, _, stderr := daemonFixtureApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	app.Context = ctx

	recovered := make([]proto.RunEventPayload, 257)
	for sequence := range recovered {
		recovered[sequence] = proto.RunEventPayload{
			RunID: "run-overflow", Sequence: sequence, Type: "step_started", StepID: "run",
			OccurredAt: "2026-09-15T12:00:00Z",
		}
	}
	daemon := &fakeDaemon{
		startRunID: "run-overflow",
		subscribeResults: []runnerclient.SubscribeRunResult{
			{Status: "running"},
			{Status: "running"},
		},
		getRunResults: []runnerclient.GetRunResult{
			{
				Summary: proto.RunSummary{RunID: "run-overflow", Status: "running"},
				Events:  recovered,
			},
			{
				Summary: proto.RunSummary{RunID: "run-overflow", Status: "succeeded"},
				Events: []proto.RunEventPayload{{
					RunID: "run-overflow", Sequence: 257, Type: "run_succeeded", OccurredAt: "2026-09-15T12:00:01Z",
				}},
			},
		},
	}
	daemon.subscribeHooks = []func(){func() {
		for sequence := 0; sequence < 257; sequence++ {
			payload, err := json.Marshal(proto.RunEventPayload{
				RunID: "run-overflow", Sequence: sequence, Type: "step_started", StepID: "run",
				OccurredAt: "2026-09-15T12:00:00Z",
			})
			if err != nil {
				t.Fatal(err)
			}
			daemon.notificationFunc(runnerclient.Notification{Method: "v1/runEvent", Params: payload})
		}
	}}
	daemon.getRunHooks = []func(){func() {
		// A notification arriving after local overflow must not reach the
		// command's event processor; canonical GetRun polling replaces the
		// live stream from this point onward.
		if daemon.notificationFunc == nil {
			return
		}
		payload, err := json.Marshal(proto.RunEventPayload{
			RunID: "run-overflow", Sequence: 257, Type: "approval_requested", StepID: "too-late",
			OccurredAt: "2026-09-15T12:00:01Z",
		})
		if err != nil {
			t.Fatal(err)
		}
		daemon.notificationFunc(runnerclient.Notification{Method: "v1/runEvent", Params: payload})
	}}
	daemon.onDecide = func() {
		payload := json.RawMessage(`{"runId":"run-overflow","sequence":258,"type":"run_succeeded","occurredAt":"2026-09-15T12:00:02Z"}`)
		daemon.notificationFunc(runnerclient.Notification{Method: "v1/runEvent", Params: payload})
	}
	app.DialRunner = func(ctx context.Context, opts runnerclient.Options) (daemonClient, error) {
		return daemon, nil
	}

	code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if len(daemon.getRunIncludes) < 3 || !daemon.getRunIncludes[0] || !daemon.getRunIncludes[1] {
		t.Fatalf("getRun includeEvents calls = %v, want canonical polling through terminal state", daemon.getRunIncludes)
	}
	if len(daemon.subscribeCalls) != 1 {
		t.Fatalf("subscribeRun fromSequence calls = %v, want only the initial [0]", daemon.subscribeCalls)
	}
	if daemon.notificationFunc != nil {
		t.Fatal("notification handler remains installed after local overflow")
	}
	if len(daemon.decided) != 0 {
		t.Fatalf("notifications processed after local overflow: decideApproval calls = %v", daemon.decided)
	}
}

// TestRunDaemonInvalidInputsMapsToContractExit pins the RFC §11 exit-code
// mapping: -32008 invalidInputs is a contract failure, not an operational
// one.
func TestRunDaemonInvalidInputsMapsToContractExit(t *testing.T) {
	app, _, stderr := daemonFixtureApp(t)
	daemon := &fakeDaemon{startErr: &proto.Error{Code: proto.CodeInvalidInputs, Message: "input count is required"}}
	app.DialRunner = func(ctx context.Context, opts runnerclient.Options) (daemonClient, error) {
		return daemon, nil
	}

	code := app.Run([]string{"run", "capability/greet"})
	if code != ExitContract {
		t.Fatalf("exit = %d, want ExitContract for -32008, stderr = %s", code, stderr.String())
	}
}

// TestRunDaemonUnavailableFallsBackToStandalone pins the fallback
// invariant: ErrNotRunning silently takes the existing standalone path and
// the run still succeeds locally.
func TestRunDaemonUnavailableFallsBackToStandalone(t *testing.T) {
	app, _, stderr := daemonFixtureApp(t)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester"}
	app.Hostname = func() (string, error) { return "test-node", nil }
	app.DialRunner = func(ctx context.Context, opts runnerclient.Options) (daemonClient, error) {
		return nil, runnerclient.ErrNotRunning
	}

	code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada", "--json"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
}

// TestRunDaemonHardErrorIsNotSilent pins that a dial failure OTHER than
// ErrNotRunning (bad token, version mismatch) is reported, never swallowed
// into a silent standalone fallback.
func TestRunDaemonHardErrorIsNotSilent(t *testing.T) {
	app, _, stderr := daemonFixtureApp(t)
	app.DialRunner = func(ctx context.Context, opts runnerclient.Options) (daemonClient, error) {
		return nil, &proto.Error{Code: proto.CodeAuthFailed, Message: "token mismatch"}
	}

	code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada"})
	if code != ExitOperational {
		t.Fatalf("exit = %d, want ExitOperational for auth failure", code)
	}
	if !strings.Contains(stderr.String(), "token mismatch") {
		t.Fatalf("stderr must surface the dial error: %s", stderr.String())
	}
}

// TestRunsListRoutesToDaemon pins `doppels runs list` against a live
// daemon: the table renders from v1/listRuns and the workspace filter is
// the detected root.
func TestRunsListRoutesToDaemon(t *testing.T) {
	app, stdout, stderr := daemonFixtureApp(t)
	daemon := &fakeDaemon{
		runsResult: runnerclient.ListRunsResult{Runs: []proto.RunSummary{
			{RunID: "run-1", Status: "succeeded", Capability: "greet", CreatedAt: "2026-09-15T12:00:00Z"},
		}},
	}
	app.DialRunner = func(ctx context.Context, opts runnerclient.Options) (daemonClient, error) {
		return daemon, nil
	}

	code := app.Run([]string{"runs", "list"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "run-1") {
		t.Fatalf("stdout must include the daemon-sourced run: %s", stdout.String())
	}
	if daemon.gotRuns.Workspace == "" {
		t.Fatal("listRuns must be scoped to the detected workspace root")
	}
	if !daemon.closed {
		t.Fatal("daemon client must be closed after the command finishes")
	}
}

// TestRunsListFallsBackToStandalone pins the fallback for runs list.
func TestRunsListFallsBackToStandalone(t *testing.T) {
	app, _, stderr := daemonFixtureApp(t)
	app.DialRunner = func(ctx context.Context, opts runnerclient.Options) (daemonClient, error) {
		return nil, runnerclient.ErrNotRunning
	}

	code := app.Run([]string{"runs", "list"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
}

// TestNodeUpReportsRunningDaemon pins that `node up` against a live daemon
// reports its status and exits — it must never start a second listener.
func TestNodeUpReportsRunningDaemon(t *testing.T) {
	app, stdout, stderr := daemonFixtureApp(t)
	daemon := &fakeDaemon{statusResult: proto.NodeStatus{
		State: "online", RunnerVersion: "0.1.0-test", ProtocolVersion: 1,
	}}
	app.DialRunner = func(ctx context.Context, opts runnerclient.Options) (daemonClient, error) {
		return daemon, nil
	}

	code := app.Run([]string{"node", "up"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "online") || !strings.Contains(out, "0.1.0-test") {
		t.Fatalf("stdout must report node state and version: %s", out)
	}
	if !daemon.closed {
		t.Fatal("daemon client must be closed after the command finishes")
	}
}
