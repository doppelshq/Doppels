package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/listener"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/registryclient"
	"doppels.so/cli/internal/shareclient"
)

type listenJob = listener.Job

// cliProducerReporter adapts listener callbacks onto the `node up` TTY and
// JSON surfaces, preserving the exact byte-level output of the pre-listener
// implementation (status line pausing included).
type cliProducerReporter struct {
	app             *App
	jsonOutput      bool
	capabilityCount int
	pause           *atomic.Bool
}

func (r *cliProducerReporter) ScopeStatus(counts listener.InboxCounts) {
	if r.jsonOutput || r.pause.Load() {
		return
	}
	writeListenScopeStatus(r.app.Stderr, counts.Scopes, r.capabilityCount, counts.SharesOpen, counts.SharesReady, counts.Requests)
}

func (r *cliProducerReporter) ShareAttached(item shareclient.InboxItem) {
	if r.jsonOutput {
		r.app.writeJSON(map[string]any{
			"kind":      "ListenShareAttached",
			"shareId":   item.Share.ID,
			"expiresAt": item.Share.ExpiresAt,
		})
		return
	}
	if r.pause.Load() {
		return
	}
	clearListenStatus(r.app.Stderr)
	writeListenAttached(r.app.Stderr, item, r.app.now())
}

func (app *App) runNode(arguments []string) int {
	if isHelp(arguments) {
		writeNodeUsage(app.Stdout)
		return ExitSuccess
	}
	if len(arguments) == 0 {
		writeNodeUsage(app.Stderr)
		return ExitContract
	}
	switch arguments[0] {
	case "up":
		return app.runListen(arguments[1:])
	default:
		fmt.Fprintf(app.Stderr, "unknown node subcommand %q\n", arguments[0])
		writeNodeUsage(app.Stderr)
		return ExitContract
	}
}

func writeNodeUsage(writer io.Writer) {
	fmt.Fprintln(writer, "Usage: doppels node up [--org …] [--space …] [--capability …] [--yes] [--json]")
	fmt.Fprintln(writer, "Bring this Node online in the foreground. Ctrl-C stops it.")
	fmt.Fprintln(writer, "On each Request: [a]pprove  [r]eject  [s]kip  [b]ackground")
}

// runListen powers `doppels node up`: it attaches to Shares already created by
// this Identity (console or `doppels share`). It never creates Shares — Share
// stays one-shot.
func (app *App) runListen(arguments []string) int {
	defaultServer := environmentValue(app.environment(), "DOPPELS_SERVER")
	apiToken := environmentValue(app.environment(), "DOPPELS_API_TOKEN")
	var store *configstore.Store
	resolvedStore, configErr := app.configStore()
	if configErr == nil {
		store = resolvedStore
		if profile, profileErr := store.Profile(); profileErr == nil && defaultServer == "" {
			defaultServer = profile.Server
		} else if profileErr != nil && !errors.Is(profileErr, configstore.ErrNotLoggedIn) && !errors.Is(profileErr, configstore.ErrNotConfigured) && apiToken == "" {
			fmt.Fprintf(app.Stderr, "load CLI profile: %v\n", profileErr)
			return ExitOperational
		}
	} else if apiToken == "" {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", configErr)
		return ExitOperational
	}
	sessionServer := ""
	if apiToken == "" {
		if store == nil {
			fmt.Fprintln(app.Stderr, "login or DOPPELS_API_TOKEN is required for node up")
			return ExitOperational
		}
		session, sessionErr := store.Session()
		switch {
		case sessionErr == nil:
			apiToken = session.Token
			sessionServer = session.Profile.Server
			if defaultServer == "" {
				defaultServer = session.Profile.Server
			}
		case errors.Is(sessionErr, configstore.ErrNotLoggedIn), errors.Is(sessionErr, configstore.ErrNotConfigured):
			fmt.Fprintln(app.Stderr, "login or DOPPELS_API_TOKEN is required for node up")
			return ExitOperational
		default:
			fmt.Fprintf(app.Stderr, "load login: %v\n", sessionErr)
			return ExitOperational
		}
	}
	if defaultServer == "" {
		defaultServer = "https://doppels.so"
	}

	flags := app.flagSet("node up")
	server := flags.String("server", defaultServer, "Doppels control-plane URL")
	pollEvery := flags.Duration("poll", 2*time.Second, "inbox poll interval")
	approveAll := flags.Bool("yes", false, "auto-fulfill Requests and approve required Steps")
	jsonOutput := flags.Bool("json", false, "emit machine-readable Node events on stdout")
	org := flags.String("org", "", "Organization to serve (default: current context)")
	space := flags.String("space", "", "limit the Node to one Space in the Organization")
	capability := flags.String("capability", "", "limit the Node to one Capability name")
	var rawOutputs, rawEvidence namedValues
	flags.Var(&rawOutputs, "output", "manual output as name=value (repeatable)")
	flags.Var(&rawEvidence, "evidence", "manual evidence as name=value (repeatable)")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "node up takes no positional args; use --org, --space, and --capability to filter")
		return ExitContract
	}
	filters := listenFilters{Organization: *org, Space: *space, Capability: *capability}
	if sessionServer != "" {
		target, targetErr := registryclient.ParseServer(*server)
		profileTarget, profileErr := registryclient.ParseServer(sessionServer)
		if targetErr != nil || profileErr != nil || target.String() != profileTarget.String() {
			writeServerTokenMismatch(app.Stderr, sessionServer, *server)
			return ExitContract
		}
	}
	if *pollEvery <= 0 {
		fmt.Fprintln(app.Stderr, "--poll must be a positive duration")
		return ExitContract
	}

	index, code := app.listenLocalIndex()
	if code != ExitSuccess {
		return code
	}
	catalog := index.Merged
	outputValues, err := parseNamedValues(rawOutputs)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}
	evidenceValues, err := parseNamedValues(rawEvidence)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}

	client, err := shareclient.New(shareclient.Options{Server: *server, HTTPClient: app.HTTPClient, Now: app.now})
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}

	scopeView, filters, registry, err := app.resolveListenScope(*server, apiToken, store, catalog, filters)
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve Node scope: %v\n", err)
		return ExitOperational
	}
	scopeView.LocalTrees = index.localTrees()
	scopeView.LocalProjects = index.projectLabels()
	if !*jsonOutput {
		writeListenBanner(app.Stderr, scopeView)
	}

	ctx := app.context()
	interaction := newInteraction(app.Stdin, app.Stderr)
	port := &cliApprovalPort{interaction: interaction, manualOutputs: outputValues, manualEvidence: evidenceValues}
	jobs := make(chan listenJob, 16)
	producerErr := make(chan error, 1)

	pauseStatus := &atomic.Bool{}
	reporter := &cliProducerReporter{app: app, jsonOutput: *jsonOutput, capabilityCount: len(scopeView.Capabilities), pause: pauseStatus}
	go func() {
		producerErr <- listener.RunProducer(ctx, listener.ProducerConfig{
			Registry:  registry,
			Shares:    client,
			Token:     apiToken,
			Filters:   filters,
			PollEvery: *pollEvery,
			Jobs:      jobs,
			Reporter:  reporter,
		})
	}()

	var background sync.WaitGroup
	var held []listenJob
	decisionN := 0
	stopNode := func(message string, err error) int {
		background.Wait()
		if !*jsonOutput {
			clearListenStatus(app.Stderr)
			if message != "" {
				fmt.Fprintln(app.Stderr, message)
			}
		}
		drainListenJobs(jobs)
		for _, extra := range held {
			if extra.Channel != nil {
				extra.Channel.Close()
			}
		}
		if err != nil {
			return shareCommandExitCode(ctx, err)
		}
		return ExitSuccess
	}

	for {
		var job listenJob
		if len(held) > 0 {
			job, held = held[0], held[1:]
		} else {
			select {
			case <-ctx.Done():
				return stopNode("Node offline.", ctx.Err())
			case err := <-producerErr:
				if err == nil || errors.Is(err, context.Canceled) {
					return stopNode("Node offline.", ctx.Err())
				}
				clearListenStatus(app.Stderr)
				fmt.Fprintf(app.Stderr, "Unable to keep Node online: %v\n", err)
				background.Wait()
				drainListenJobs(jobs)
				return shareCommandExitCode(ctx, err)
			case next, ok := <-jobs:
				if !ok {
					return stopNode("", nil)
				}
				job = next
			}
		}
		for {
			select {
			case extra, ok := <-jobs:
				if !ok {
					jobs = nil
				} else {
					held = append(held, extra)
					continue
				}
			default:
			}
			break
		}

		decisionN++
		queue := listenPromptQueue{Index: decisionN, Total: decisionN + len(held)}
		for _, extra := range held {
			name := ""
			if extra.Request != nil {
				name = extra.Request.Capability.Name
			}
			if name != "" {
				queue.Queued = append(queue.Queued, name)
			}
		}

		if *jsonOutput {
			payload := map[string]any{
				"kind":    "ListenRequest",
				"origin":  job.Origin,
				"request": job.Request,
				"queued":  len(held),
			}
			if job.Created != nil {
				payload["shareId"] = job.Created.Share.ID
			}
			if job.Organization != "" {
				payload["organization"] = job.Organization
				payload["space"] = job.Space
			}
			app.writeJSON(payload)
		} else {
			pauseStatus.Store(true)
			clearListenStatus(app.Stderr)
			writeListenJobPrompt(app.Stderr, *job.Request, job, queue, app.now())
		}

		if job.Origin == "share" && job.Created != nil {
			if pending, pendingErr := client.Pending(ctx, job.Created.Share.ID, job.Created.RunnerToken); pendingErr == nil && pending.Request != nil {
				job.Request = pending.Request
			}
		}

		decision := listener.DecisionApprove
		if !*approveAll {
			switch strings.ToLower(strings.TrimSpace(job.Request.OperatorDecision)) {
			case "approve":
				decision = listener.DecisionApprove
				if !*jsonOutput {
					style := newTermStyle(app.Stderr)
					fmt.Fprintln(app.Stderr, "  "+style.dim("Console already approved this Request."))
				}
			case "reject":
				decision = listener.DecisionReject
				if !*jsonOutput {
					style := newTermStyle(app.Stderr)
					fmt.Fprintln(app.Stderr, "  "+style.dim("Console already rejected this Request."))
				}
			default:
				got, decideErr := port.DecideFulfillment(ctx, job, listener.QueueInfo{Index: queue.Index, Total: queue.Total, Queued: queue.Queued})
				if decideErr != nil {
					pauseStatus.Store(false)
					if job.Channel != nil {
						job.Channel.Close()
					}
					return stopNode("", decideErr)
				}
				decision = got
			}
		}

		if decision == listener.DecisionSkip {
			pauseStatus.Store(false)
			if !*jsonOutput {
				style := newTermStyle(app.Stderr)
				fmt.Fprintln(app.Stderr)
				fmt.Fprintln(app.Stderr, "  "+style.dim("→ Skipped.")+" "+style.dim("Left pending; next Request…"))
				fmt.Fprintln(app.Stderr)
			} else {
				payload := map[string]any{"kind": "ListenSkipped", "requestId": job.Request.ID, "origin": job.Origin}
				if job.Created != nil {
					payload["shareId"] = job.Created.Share.ID
				}
				app.writeJSON(payload)
			}
			if job.Channel != nil {
				job.Channel.Close()
			}
			continue
		}

		if decision == listener.DecisionReject {
			pauseStatus.Store(false)
			if job.Origin == "share" {
				if rejectErr := listener.RejectShare(ctx, job.Channel, job.Created, job.Request, app.localNodeID(), app.now); rejectErr != nil {
					fmt.Fprintf(app.Stderr, "Unable to reject: %v\n", rejectErr)
					if job.Channel != nil {
						job.Channel.Close()
					}
					background.Wait()
					return shareCommandExitCode(ctx, rejectErr)
				}
			} else if registry != nil {
				if _, rejectErr := registry.DecideRequest(ctx, apiToken, job.Organization, job.Space, job.Request.ID, "reject"); rejectErr != nil {
					fmt.Fprintf(app.Stderr, "Unable to reject: %v\n", rejectErr)
					background.Wait()
					return shareCommandExitCode(ctx, rejectErr)
				}
			}
			if !*jsonOutput {
				style := newTermStyle(app.Stderr)
				fmt.Fprintln(app.Stderr)
				fmt.Fprintln(app.Stderr, "  "+style.boldRed("→ Rejected.")+" "+style.dim("The recipient will see the Request cancelled."))
				fmt.Fprintln(app.Stderr)
			} else {
				payload := map[string]any{"kind": "ListenRejected", "requestId": job.Request.ID, "origin": job.Origin}
				if job.Created != nil {
					payload["shareId"] = job.Created.Share.ID
				}
				app.writeJSON(payload)
			}
			if job.Channel != nil {
				job.Channel.Close()
			}
			continue
		}

		backgroundRun := decision == listener.DecisionBackground
		stepApprove := *approveAll || backgroundRun
		if !*jsonOutput {
			style := newTermStyle(app.Stderr)
			fmt.Fprintln(app.Stderr)
			if backgroundRun {
				fmt.Fprintln(app.Stderr, "  "+style.boldGreen("→ Approved (background).")+" "+style.dim("Required Steps auto-approved. Next Request…"))
			} else {
				fmt.Fprintln(app.Stderr, "  "+style.boldGreen("→ Approved.")+" "+style.dim("Running on this machine…"))
			}
			fmt.Fprintln(app.Stderr)
		}

		runJob := func(current listenJob, autoSteps bool) int {
			capabilityName := ""
			if current.Request != nil {
				capabilityName = current.Request.Capability.Name
			} else if current.Created != nil {
				capabilityName = current.Created.Share.CapabilityRevision.Name
			}
			root, catalog, resolveErr := index.resolve(capabilityName, current.Space)
			if resolveErr != nil {
				fmt.Fprintln(app.Stderr, resolveErr)
				if current.Channel != nil {
					current.Channel.Close()
				}
				return ExitContract
			}
			closeChannel := func() {
				if current.Channel != nil {
					current.Channel.Close()
				}
			}

			runtimeStdout := app.Stdout
			if *jsonOutput {
				runtimeStdout = app.Stderr
			} else {
				runtimeStdout = prefixLines(runtimeStdout, "    ")
			}

			var capability manifest.CapabilityDefinition
			var recipe *manifest.RecipeDefinition
			var fulfillResolveErr error
			if current.Origin == "space" {
				capability, recipe, fulfillResolveErr = listener.ResolveSpaceFulfillment(catalog, *current.Request, port, autoSteps)
			} else {
				capability, recipe, fulfillResolveErr = listener.ResolveShareFulfillment(catalog, current.Created.Share, port, autoSteps)
			}
			if fulfillResolveErr != nil {
				fmt.Fprintln(app.Stderr, fulfillResolveErr)
				closeChannel()
				return ExitContract
			}
			if code := app.validateShareHost(root, catalog, recipe); code != ExitSuccess {
				closeChannel()
				return code
			}

			timelineInvocation := execution.Invocation{Capability: capability.Value}
			if recipe != nil {
				timelineInvocation.Recipe = recipe.Value
			}
			var timeline *runTimeline
			if !*jsonOutput {
				timeline = newRunTimeline(app.Stderr, timelineInvocation)
			}

			if current.Origin == "space" {
				var spaceRegistry listener.SpaceRegistry
				if registry != nil {
					spaceRegistry = registry
				}
				result, runErr := listener.FulfillSpace(ctx, listener.SpaceFulfillConfig{
					Registry:      spaceRegistry,
					Token:         apiToken,
					Identity:      scopeView.Header.Identity,
					LocalIdentity: app.localIdentity(),
					NodeID:        app.localNodeID(),
					Root:          root,
					Catalog:       catalog,
					Approvals:     port,
					ApproveAll:    autoSteps,
					Output:        runtimeStdout,
					Err:           app.Stderr,
					Environment:   app.environment(),
					Now:           app.now,
					OnEvent: func(callbackContext context.Context, event execution.RunEvent) error {
						if timeline != nil {
							return timeline.onEvent(callbackContext, event)
						}
						return nil
					},
				}, current, capability, recipe)
				var approveErr *listener.ApproveError
				if errors.As(runErr, &approveErr) {
					fmt.Fprintf(app.Stderr, "Unable to approve: %v\n", approveErr.Err)
					closeChannel()
					return shareCommandExitCode(ctx, approveErr.Err)
				}
				if *jsonOutput && result.Run.ID != "" {
					app.writeJSON(map[string]any{
						"kind":         "ListenRunFinished",
						"origin":       "space",
						"organization": current.Organization,
						"space":        current.Space,
						"runId":        result.Run.ID,
						"status":       result.Status,
						"requestId":    current.Request.ID,
					})
				} else if !*jsonOutput {
					elapsed := time.Duration(0)
					if timeline != nil {
						elapsed = timeline.elapsed()
					}
					writeLocalRunSummary(app.Stdout, result, elapsed)
					if runErr != nil {
						fmt.Fprintf(app.Stderr, "run failed: %v\n", runErr)
					}
				}
				closeChannel()
				return executionExitCode(runErr)
			}

			result, runErr := listener.FulfillShare(ctx, listener.ShareFulfillConfig{
				Root:   root,
				NodeID: app.localNodeID(),
				BeforeSuccess: func(callbackContext context.Context, run execution.RunRecord, returns, evidence map[string]any) error {
					if current.Channel == nil {
						return nil
					}
					return uploadSharedArtifacts(callbackContext, app.Stderr, client, current.Created.Share.ID, current.Created.RunnerToken, run.ID, returns, evidence)
				},
				Output:      runtimeStdout,
				Err:         app.Stderr,
				Environment: app.environment(),
				Now:         app.now,
				OnEvent: func(callbackContext context.Context, event execution.RunEvent) error {
					if timeline != nil {
						return timeline.onEvent(callbackContext, event)
					}
					return nil
				},
			}, current, capability, recipe)
			if *jsonOutput {
				if result.Run.ID != "" {
					app.writeJSON(map[string]any{
						"kind":      "ListenRunFinished",
						"shareId":   current.Created.Share.ID,
						"runId":     result.Run.ID,
						"status":    result.Status,
						"requestId": current.Request.ID,
					})
				}
			} else {
				elapsed := time.Duration(0)
				if timeline != nil {
					elapsed = timeline.elapsed()
				}
				writeLocalRunSummary(app.Stdout, result, elapsed)
			}
			if runErr != nil {
				fmt.Fprintf(app.Stderr, "shared run failed: %v\n", runErr)
			}
			closeChannel()
			return executionExitCode(runErr)
		}

		if backgroundRun {
			current := job
			background.Add(1)
			go func() {
				defer background.Done()
				_ = runJob(current, true)
			}()
			pauseStatus.Store(false)
			continue
		}

		code := runJob(job, stepApprove)
		pauseStatus.Store(false)
		if code != ExitSuccess {
			background.Wait()
			drainListenJobs(jobs)
			return code
		}
		if !*jsonOutput {
			style := newTermStyle(app.Stderr)
			fmt.Fprintln(app.Stderr)
			fmt.Fprintln(app.Stderr, "  "+style.boldGreen("→ Done.")+" "+style.dim("Waiting for Requests…"))
			fmt.Fprintln(app.Stderr)
		}
	}
}

func writeListenJobPrompt(writer io.Writer, request execution.RequestRecord, job listenJob, queue listenPromptQueue, now time.Time) {
	if job.Origin == "share" && job.Created != nil {
		writeListenDecisionPrompt(writer, request, job.Created, queue, now)
		return
	}
	writeListenSpaceDecisionPrompt(writer, request, job.Organization, job.Space, queue, now)
}

func writeListenSpaceDecisionPrompt(writer io.Writer, request execution.RequestRecord, organization, space string, queue listenPromptQueue, now time.Time) {
	style := newTermStyle(writer)
	rule := style.cyan("════════════════════════════════════════")
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, rule)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Request"), style.boldCyan(formatListenRequestTitle(request, queue)))
	fmt.Fprintln(writer, rule)
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("From"), style.value(listenRequester(request.RequestedBy)))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Id"), style.value(shortListenID(request.ID)))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Scope"), style.value(organization+"/"+space))
	writeListenInputBlock(writer, style, request)
	writeListenQueueFooter(writer, style, queue)
	_ = now
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, rule)
	fmt.Fprintln(writer)
}

type listenHeader struct {
	Node         string
	Server       string
	Identity     string
	Organization string
	Space        string
}

func (app *App) listenHeader(server, apiToken string, store *configstore.Store) listenHeader {
	header := listenHeader{
		Node:   app.localNodeID(),
		Server: server,
	}
	if store != nil {
		if profile, err := store.Profile(); err == nil {
			header.Organization = profile.Context.Organization
			header.Space = profile.Context.Space
		}
	}
	if registry, err := app.registryClient(server); err == nil {
		if remote, err := registry.Session(app.context(), apiToken); err == nil {
			header.Identity = remote.Identity.ID
			if header.Organization == "" && remote.Personal != nil {
				header.Organization = remote.Personal.Organization
				header.Space = remote.Personal.Space
			}
		}
	}
	return header
}

func clearListenStatus(writer io.Writer) {
	fmt.Fprint(writer, "\r"+strings.Repeat(" ", 72)+"\r")
}

func writeListenAttached(writer io.Writer, item shareclient.InboxItem, now time.Time) {
	style := newTermStyle(writer)
	capName := item.Share.CapabilityRevision.Name
	capVer := item.Share.CapabilityRevision.Version
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s %s\n", style.boldCyan("+"), style.bold(fmt.Sprintf("%s@%s", capName, capVer)))
	fmt.Fprintf(writer, "    %s\n", style.dim(fmt.Sprintf("share %s · expires %s", shortListenID(item.Share.ID), listenExpiry(item.Share.ExpiresAt, now))))
	if item.AwaitingFulfillment {
		fmt.Fprintln(writer, "    "+style.boldYellow("Request ready, opening decision…"))
	} else {
		fmt.Fprintln(writer, "    "+style.dim("waiting for recipient inputs…"))
	}
}

func writeListenDecisionPrompt(writer io.Writer, request execution.RequestRecord, created *shareclient.ShareCreated, queue listenPromptQueue, now time.Time) {
	style := newTermStyle(writer)
	rule := style.cyan("════════════════════════════════════════")
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, rule)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Request"), style.boldCyan(formatListenRequestTitle(request, queue)))
	fmt.Fprintln(writer, rule)
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("From"), style.value(listenRequester(request.RequestedBy)))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Id"), style.value(shortListenID(request.ID)))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Share"), style.value(shortListenID(created.Share.ID)))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Expires"), style.value(listenExpiry(created.Share.ExpiresAt, now)))
	writeListenInputBlock(writer, style, request)
	writeListenQueueFooter(writer, style, queue)
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, rule)
	fmt.Fprintln(writer)
}

func formatListenRequestTitle(request execution.RequestRecord, queue listenPromptQueue) string {
	title := fmt.Sprintf("%s@%s", request.Capability.Name, request.Capability.Version)
	if queue.Index > 0 && queue.Total > 0 {
		return fmt.Sprintf("[%d/%d]  %s", queue.Index, queue.Total, title)
	}
	return title
}

func writeListenInputBlock(writer io.Writer, style termStyle, request execution.RequestRecord) {
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "  "+style.bold("Inputs"))
	fmt.Fprintln(writer, "  "+style.dim("──────"))
	names := make([]string, 0, len(request.Inputs))
	for name := range request.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintln(writer, "    "+style.dim("(none)"))
	}
	width := 8
	for _, name := range names {
		if len(name) > width {
			width = len(name)
		}
	}
	for _, name := range names {
		fmt.Fprintf(writer, "    %s  %s\n", style.label(fmt.Sprintf("%-*s", width, name)), style.value(formatListenInput(request.Inputs[name])))
	}
}

func writeListenQueueFooter(writer io.Writer, style termStyle, queue listenPromptQueue) {
	if len(queue.Queued) == 0 {
		return
	}
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.dim("queue"), style.value(strings.Join(queue.Queued, " · ")))
}

func (interaction *interaction) decideFulfillment() (fulfillDecision, error) {
	style := newTermStyle(interaction.output)
	prompt := fmt.Sprintf("  %s  %s  %s  %s  › ",
		style.boldGreen("[a] Approve"),
		style.boldRed("[r] Reject"),
		style.bold("[s] Skip"),
		style.boldCyan("[b] Background"),
	)
	for {
		answer, err := interaction.read(prompt)
		if err != nil {
			return fulfillReject, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "a", "accept", "approve", "y", "yes", "si", "sí", "aceptar":
			return fulfillApprove, nil
		case "r", "reject", "n", "no", "rechazar":
			return fulfillReject, nil
		case "s", "skip":
			return fulfillSkip, nil
		case "b", "background", "bg":
			return fulfillBackground, nil
		case "":
			fmt.Fprintln(interaction.output, "  "+style.dim("Empty. Type a, r, s, or b."))
		default:
			fmt.Fprintln(interaction.output, "  "+style.yellow("Not recognized. Type a (approve), r (reject), s (skip), or b (background)."))
		}
	}
}

func listenRequester(actor execution.ActorReference) string {
	id := strings.TrimSpace(actor.ID)
	switch actor.Kind {
	case "guest":
		if id == "" || id == "guest" {
			return "guest"
		}
		return id + " (guest)"
	case "identity":
		if id == "" {
			return "identity"
		}
		return id
	case "":
		if id == "" {
			return "unknown"
		}
		return id
	default:
		if id == "" {
			return actor.Kind
		}
		return fmt.Sprintf("%s · %s", id, actor.Kind)
	}
}

func formatListenInput(value any) string {
	switch typed := value.(type) {
	case nil:
		return "—"
	case string:
		if typed == "" {
			return "\"\""
		}
		return typed
	case bool:
		if typed {
			return "yes"
		}
		return "no"
	case float64:
		if typed == float64(int64(typed)) {
			return fmt.Sprintf("%d", int64(typed))
		}
		return fmt.Sprintf("%g", typed)
	case json.Number:
		return typed.String()
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return "<invalid>"
		}
		return string(encoded)
	}
}

func shortListenID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func listenExpiry(expiresAt, now time.Time) string {
	if expiresAt.IsZero() {
		return "—"
	}
	return formatRelativeTime(now, expiresAt)
}

func listenCount(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

func drainListenJobs(jobs <-chan listenJob) {
	for {
		select {
		case job, ok := <-jobs:
			if !ok {
				return
			}
			if job.Channel != nil {
				job.Channel.Close()
			}
		default:
			return
		}
	}
}
