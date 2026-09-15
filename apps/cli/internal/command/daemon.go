package command

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runnerclient"
	"doppels.so/cli/internal/runstate"
)

// daemonClient is the narrow subset of *runnerclient.Client the CLI's
// daemon-routing paths (run, runs, node) actually call. Keeping it narrow
// makes it fakeable in command-level tests without depending on a real
// socket. *runnerclient.Client satisfies this interface structurally.
type daemonClient interface {
	StartRun(ctx context.Context, params runnerclient.StartRunParams) (runnerclient.StartRunResult, error)
	SubscribeRun(ctx context.Context, runID string, fromSequence int) (runnerclient.SubscribeRunResult, error)
	GetRun(ctx context.Context, runID string, includeEvents bool) (runnerclient.GetRunResult, error)
	DecideApproval(ctx context.Context, runID, stepID, decision string) error
	GetNodeStatus(ctx context.Context) (proto.NodeStatus, error)
	ListRuns(ctx context.Context, params runnerclient.ListRunsParams) (runnerclient.ListRunsResult, error)
	SetNotificationHandler(handler func(runnerclient.Notification))
	NotificationsDropped() uint64
	Close() error
}

// getenv looks up name in app.environment(), mirroring the pattern
// experimentalEnabled already uses for DOPPELS_EXPERIMENTAL.
func (app *App) getenv(name string) string {
	return environmentValue(app.environment(), name)
}

// dialRunner attempts to reach the Runner daemon, honoring the hidden
// DOPPELS_RUNNER=0 (force standalone) and DOPPELS_RUNNER_SOCK (override
// socket path) routing knobs. Callers must treat a returned
// runnerclient.ErrNotRunning as "fall back to the standalone path"; any
// other error is a real failure (misconfiguration, auth, protocol) that
// must not be silently swallowed into a standalone fallback.
func (app *App) dialRunner(ctx context.Context) (daemonClient, error) {
	if !runnerclient.ShouldDialRunner(app.getenv) {
		return nil, runnerclient.ErrNotRunning
	}
	opts := runnerclient.Options{ClientName: "cli"}.FromEnvironment(app.getenv)
	dial := app.DialRunner
	if dial == nil {
		dial = func(ctx context.Context, opts runnerclient.Options) (daemonClient, error) {
			client, err := runnerclient.Dial(ctx, opts)
			if err != nil {
				return nil, err
			}
			return client, nil
		}
	}
	return dial(ctx, opts)
}

// reportDaemonError prints a daemon RPC error and maps it to a CLI exit
// code (RFC 001 §11: "El cliente CLI mapea estos códigos a sus exit codes
// existentes"). Non-*proto.Error failures (a dropped connection, decode
// error, ...) are treated as operational failures.
func (app *App) reportDaemonError(err error) int {
	var rpcErr *proto.Error
	if errors.As(err, &rpcErr) {
		fmt.Fprintln(app.Stderr, rpcErr.Message)
		switch rpcErr.Code {
		case proto.CodeInvalidInputs, proto.CodeStalePin:
			return ExitContract
		default:
			return ExitOperational
		}
	}
	fmt.Fprintln(app.Stderr, err)
	return ExitOperational
}

func randomIdempotencyKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "cli-" + hex.EncodeToString(buf), nil
}

// runDaemonStart drives `doppels run` against the Runner daemon: startRun,
// then subscribeRun to replay + stream events to the same timeline renderer
// the standalone path uses, answering approvals interactively when
// approvalMode is "interactive". Once StartRun has returned a runID, this
// function must never return control to a standalone fallback: any later
// failure (subscribe, a dropped connection) is reported against that runID
// instead, so the caller never risks starting a second, duplicate Run.
func (app *App) runDaemonStart(
	ctx context.Context,
	client daemonClient,
	root string,
	capabilityDefinition manifest.CapabilityDefinition,
	recipeDefinition *manifest.RecipeDefinition,
	inputs map[string]any,
	autoApprove bool,
	jsonOutput bool,
	interact *interaction,
) int {
	capabilityRef := capabilityDefinition.Value.Metadata.Name + "@" + capabilityDefinition.Value.Metadata.Version
	recipeRef := ""
	if recipeDefinition != nil {
		recipeRef = recipeDefinition.Value.Metadata.Name
	}
	approvalMode := "interactive"
	if autoApprove {
		approvalMode = "auto"
	}
	key, err := randomIdempotencyKey()
	if err != nil {
		fmt.Fprintf(app.Stderr, "generate idempotency key: %v\n", err)
		return ExitOperational
	}

	started, err := client.StartRun(ctx, runnerclient.StartRunParams{
		Workspace: root, Capability: capabilityRef, Recipe: recipeRef,
		Inputs: inputs, ApprovalMode: approvalMode, IdempotencyKey: key,
	})
	if err != nil {
		return app.reportDaemonError(err)
	}
	return app.streamDaemonRun(ctx, client, started.RunID, capabilityDefinition, recipeDefinition, approvalMode, jsonOutput, interact)
}

var daemonTerminalRunEvents = map[string]bool{
	"run_succeeded": true, "run_failed": true, "run_cancelled": true, "run_interrupted": true,
}

var daemonTerminalRunStatuses = map[string]bool{
	"succeeded": true, "failed": true, "cancelled": true, "interrupted": true,
}

const daemonOverflowPollInterval = 100 * time.Millisecond

// lostDaemonMessage is printed once a Run has already been started against
// the daemon and the connection is later lost: the CLI must never fall back
// to standalone in that case (it would risk a duplicate/orphaned Run), only
// report the runId so the caller can reconcile with `doppels runs show`.
func lostDaemonMessage(runID string) string {
	return fmt.Sprintf("run %s started but the daemon connection was lost — check status with: doppels runs show %s\n", runID, runID)
}

func (app *App) handleDaemonRunEvent(
	ctx context.Context,
	client daemonClient,
	runID string,
	approvalMode string,
	interact *interaction,
	event proto.RunEventPayload,
) int {
	if approvalMode != "interactive" || event.Type != "approval_requested" {
		return ExitSuccess
	}
	approved, promptErr := interact.approve(ctx, execution.ApprovalRequest{RunID: runID, StepID: event.StepID})
	decision := "reject"
	if promptErr == nil && approved {
		decision = "approve"
	}
	if err := client.DecideApproval(ctx, runID, event.StepID, decision); err != nil {
		fmt.Fprint(app.Stderr, lostDaemonMessage(runID))
		return ExitOperational
	}
	return ExitSuccess
}

func (app *App) streamDaemonRun(
	ctx context.Context,
	client daemonClient,
	runID string,
	capabilityDefinition manifest.CapabilityDefinition,
	recipeDefinition *manifest.RecipeDefinition,
	approvalMode string,
	jsonOutput bool,
	interact *interaction,
) int {
	var timeline *runTimeline
	if !jsonOutput {
		invocation := execution.Invocation{Capability: capabilityDefinition.Value}
		if recipeDefinition != nil {
			invocation.Recipe = recipeDefinition.Value
		}
		timeline = newRunTimeline(app.Stderr, invocation)
	}
	render := func(payload proto.RunEventPayload) {
		if timeline == nil {
			return
		}
		occurredAt, _ := time.Parse(time.RFC3339Nano, payload.OccurredAt)
		_ = timeline.onEvent(ctx, execution.RunEvent{
			RunID: payload.RunID, Sequence: payload.Sequence, Type: payload.Type,
			StepID: payload.StepID, Data: payload.Data, OccurredAt: occurredAt,
		})
	}

	events := make(chan proto.RunEventPayload, 256)
	resync := make(chan struct{}, 1)
	localOverflow := make(chan struct{}, 1)
	var consumeLiveEvents atomic.Bool
	consumeLiveEvents.Store(true)
	requestResync := func() {
		select {
		case resync <- struct{}{}:
		default:
		}
	}
	stopLiveEvents := func() {
		if !consumeLiveEvents.CompareAndSwap(true, false) {
			return
		}
		client.SetNotificationHandler(nil)
		select {
		case localOverflow <- struct{}{}:
		default:
		}
	}
	client.SetNotificationHandler(func(n runnerclient.Notification) {
		if !consumeLiveEvents.Load() {
			return
		}
		switch n.Method {
		case "v1/runEvent":
			var payload proto.RunEventPayload
			if err := json.Unmarshal(n.Params, &payload); err != nil || payload.RunID != runID {
				return
			}
			select {
			case events <- payload:
			default:
				stopLiveEvents()
			}
		case "v1/nodeEvent":
			var event struct {
				Kind    string `json:"kind"`
				Payload struct {
					RunID string `json:"runId"`
				} `json:"payload"`
			}
			if err := json.Unmarshal(n.Params, &event); err == nil &&
				event.Kind == proto.NodeEventRunEventGap && event.Payload.RunID == runID {
				requestResync()
			}
		}
	})

	// Capture the transport-drop baseline BEFORE SubscribeRun: any drop
	// that lands during the subscribe window must be visible to the
	// poller, otherwise the CLI can wait forever on a run the daemon has
	// already finalized and dropped notifications for.
	initialDropped := client.NotificationsDropped()

	subscribed, err := client.SubscribeRun(ctx, runID, 0)
	if err != nil {
		fmt.Fprint(app.Stderr, lostDaemonMessage(runID))
		return ExitOperational
	}
	lastRenderedSequence := -1
	processEvents := func(batch []proto.RunEventPayload) (bool, int) {
		terminal := false
		for _, event := range batch {
			if event.Sequence <= lastRenderedSequence {
				continue
			}
			render(event)
			lastRenderedSequence = event.Sequence
			if code := app.handleDaemonRunEvent(ctx, client, runID, approvalMode, interact, event); code != ExitSuccess {
				return false, code
			}
			if daemonTerminalRunEvents[event.Type] {
				terminal = true
			}
		}
		return terminal, ExitSuccess
	}

	terminalSeen, code := processEvents(subscribed.Events)
	if code != ExitSuccess {
		return code
	}

	recoverStream := func() (bool, int) {
		// Notifications already queued locally belong to the stream whose
		// continuity was lost. The durable event list is authoritative.
	DrainEvents:
		for {
			select {
			case <-events:
			default:
				break DrainEvents
			}
		}

		recovered, err := client.GetRun(ctx, runID, true)
		if err != nil {
			fmt.Fprint(app.Stderr, lostDaemonMessage(runID))
			return false, ExitOperational
		}
		terminal, code := processEvents(recovered.Events)
		if code != ExitSuccess || terminal {
			return terminal, code
		}
		if !consumeLiveEvents.Load() {
			return false, ExitSuccess
		}

		subscribed, err := client.SubscribeRun(ctx, runID, lastRenderedSequence+1)
		if err != nil {
			fmt.Fprint(app.Stderr, lostDaemonMessage(runID))
			return false, ExitOperational
		}
		return processEvents(subscribed.Events)
	}
	pollCanonical := func() (bool, int) {
		// Local overflow is best-effort loss, not a daemon-reported stream
		// gap. Keep the original server subscription untouched, stop the
		// local handler, and use the durable event list until the Run ends.
	DrainEvents:
		for {
			select {
			case <-events:
			default:
				break DrainEvents
			}
		}
		for {
			recovered, err := client.GetRun(ctx, runID, true)
			if err != nil {
				fmt.Fprint(app.Stderr, lostDaemonMessage(runID))
				return false, ExitOperational
			}
			terminal, code := processEvents(recovered.Events)
			if code != ExitSuccess || terminal || daemonTerminalRunStatuses[recovered.Summary.Status] {
				return terminal || daemonTerminalRunStatuses[recovered.Summary.Status], code
			}

			select {
			case <-ctx.Done():
				fmt.Fprint(app.Stderr, lostDaemonMessage(runID))
				return false, ExitOperational
			case <-time.After(daemonOverflowPollInterval):
			}
		}
	}

	if !terminalSeen {
		watchdogCtx, cancelWatchdog := context.WithCancel(ctx)
		defer cancelWatchdog()
		watchdogErr := make(chan error, 1)
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-watchdogCtx.Done():
					return
				case <-ticker.C:
					if _, err := client.GetNodeStatus(watchdogCtx); err != nil {
						select {
						case watchdogErr <- err:
						default:
						}
						return
					}
				}
			}
		}()

		// transportDrops tracks notifications the runnerclient read loop
		// dropped at the transport boundary (backpressure overflow). Unlike
		// localOverflow (handler-driven) and resync (daemon-driven), there
		// is no signal channel for transport drops — the only way to know
		// is to poll the counter. When it increases, switch to canonical
		// GetRun polling without resubscribing (the server-side subscription
		// is still alive; only local delivery has fallen behind).
		transportDropSignal := make(chan struct{}, 1)
		lastDropped := initialDropped
		go func() {
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-watchdogCtx.Done():
					return
				case <-ticker.C:
					if client.NotificationsDropped() > lastDropped {
						lastDropped = client.NotificationsDropped()
						select {
						case transportDropSignal <- struct{}{}:
						default:
						}
					}
				}
			}
		}()

	loop:
		for {
			select {
			case <-localOverflow:
				terminal, code := pollCanonical()
				if code != ExitSuccess {
					return code
				}
				if terminal {
					break loop
				}
				continue
			case <-resync:
				terminal, code := recoverStream()
				if code != ExitSuccess {
					return code
				}
				if terminal {
					break loop
				}
				continue
			default:
			}
			select {
			case event := <-events:
				if !consumeLiveEvents.Load() {
					continue
				}
				terminal, code := processEvents([]proto.RunEventPayload{event})
				if code != ExitSuccess {
					return code
				}
				if terminal {
					break loop
				}
			case <-localOverflow:
				terminal, code := pollCanonical()
				if code != ExitSuccess {
					return code
				}
				if terminal {
					break loop
				}
			case <-resync:
				terminal, code := recoverStream()
				if code != ExitSuccess {
					return code
				}
				if terminal {
					break loop
				}
			case <-watchdogErr:
				fmt.Fprint(app.Stderr, lostDaemonMessage(runID))
				return ExitOperational
			case <-transportDropSignal:
				terminal, code := pollCanonical()
				if code != ExitSuccess {
					return code
				}
				if terminal {
					break loop
				}
			case <-ctx.Done():
				fmt.Fprint(app.Stderr, lostDaemonMessage(runID))
				return ExitOperational
			}
		}
	}

	final, err := client.GetRun(ctx, runID, false)
	if err != nil {
		fmt.Fprint(app.Stderr, lostDaemonMessage(runID))
		return ExitOperational
	}
	if jsonOutput {
		app.writeJSON(map[string]any{
			"apiVersion": execution.APIVersion, "kind": "LocalRunResult",
			"status": final.Summary.Status, "run": final.Summary,
		})
	}
	switch final.Summary.Status {
	case "succeeded":
		return ExitSuccess
	case "cancelled", "interrupted":
		return ExitInterrupted
	default:
		return ExitOperational
	}
}

// listRunsViaDaemon renders `doppels runs list` from the daemon's v1/listRuns
// instead of the standalone on-disk runstate index, reusing the same
// writeRunList/writeJSON rendering the standalone path uses.
func (app *App) listRunsViaDaemon(client daemonClient, root string, jsonOutput, all bool, limit int) int {
	params := runnerclient.ListRunsParams{Workspace: root}
	if !all {
		params.Limit = limit
	}
	result, err := client.ListRuns(app.context(), params)
	if err != nil {
		return app.reportDaemonError(err)
	}
	items := make([]runstate.Summary, 0, len(result.Runs))
	for _, run := range result.Runs {
		summary := runstate.Summary{
			ID: run.RunID, Status: run.Status, Source: run.Source,
			CreatedAt: run.CreatedAt, RequestID: run.RequestID, Capability: run.Capability,
		}
		if run.Recipe != nil {
			summary.Recipe = *run.Recipe
		}
		items = append(items, summary)
	}
	if jsonOutput {
		app.writeJSON(map[string]any{"kind": "RunList", "items": items})
		return ExitSuccess
	}
	total := len(items)
	shown := items
	if !all && total > limit {
		shown = items[:limit]
	}
	now := app.now()
	writeRunList(app.Stdout, shown, total, limit, all, func(created string) string {
		return formatDisplayTime(now, created)
	})
	return ExitSuccess
}

// reportNodeStatus renders `doppels node up` against an already-running
// daemon: it must never start a second listener/daemon, so it only reports
// the live status and returns.
func (app *App) reportNodeStatus(client daemonClient) int {
	status, err := client.GetNodeStatus(app.context())
	if err != nil {
		return app.reportDaemonError(err)
	}
	style := newTermStyle(app.Stdout)
	fmt.Fprintln(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Node"), style.value(status.State))
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Version"), status.RunnerVersion)
	fmt.Fprintf(app.Stdout, "  %s  %d\n", style.field("Workspaces"), len(status.Workspaces))
	fmt.Fprintln(app.Stdout, "  "+style.dim("a Runner daemon is already running; not starting a second one"))
	return ExitSuccess
}
