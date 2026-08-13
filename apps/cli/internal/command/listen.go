package command

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/registryclient"
	"doppels.so/cli/internal/runsync"
	"doppels.so/cli/internal/shareclient"
)

type listenJob struct {
	origin       string // "share" or "space"
	created      *shareclient.ShareCreated
	channel      *shareclient.Channel
	request      *execution.RequestRecord
	organization string
	space        string
}

// runListen attaches to Shares already created by this Identity (console or
// `doppels share`). It never creates Shares — Share stays one-shot.
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
			fmt.Fprintln(app.Stderr, "login or DOPPELS_API_TOKEN is required for listen")
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
			fmt.Fprintln(app.Stderr, "login or DOPPELS_API_TOKEN is required for listen")
			return ExitOperational
		default:
			fmt.Fprintf(app.Stderr, "load login: %v\n", sessionErr)
			return ExitOperational
		}
	}
	if defaultServer == "" {
		defaultServer = "https://doppels.so"
	}

	flags := app.flagSet("listen")
	server := flags.String("server", defaultServer, "Doppels control-plane URL")
	pollEvery := flags.Duration("poll", 2*time.Second, "inbox poll interval")
	approveAll := flags.Bool("yes", false, "auto-fulfill Requests and approve required Steps")
	jsonOutput := flags.Bool("json", false, "emit machine-readable listen events on stdout")
	org := flags.String("org", "", "Organization to listen on (default: current context)")
	space := flags.String("space", "", "limit listening to one Space in the Organization")
	capability := flags.String("capability", "", "limit listening to one Capability name")
	var rawOutputs, rawEvidence namedValues
	flags.Var(&rawOutputs, "output", "manual output as name=value (repeatable)")
	flags.Var(&rawEvidence, "evidence", "manual evidence as name=value (repeatable)")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "listen takes no positional args; use --org, --space, and --capability to filter")
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
		fmt.Fprintf(app.Stderr, "resolve listen scope: %v\n", err)
		return ExitOperational
	}
	scopeView.LocalProjects = index.projectLabels()
	if !*jsonOutput {
		writeListenBanner(app.Stderr, scopeView)
	}

	ctx := app.context()
	interaction := newInteraction(app.Stdin, app.Stderr)
	jobs := make(chan listenJob, 16)
	producerErr := make(chan error, 1)

	pauseStatus := &atomic.Bool{}
	go app.listenInboxProducer(ctx, listenInboxConfig{
		shareClient: client,
		registry:    registry,
		apiToken:    apiToken,
		filters:     filters,
		scopeView:   scopeView,
		pollEvery:   *pollEvery,
		jsonOutput:  *jsonOutput,
		pauseStatus: pauseStatus,
		jobs:        jobs,
		errCh:       producerErr,
	})

	for {
		select {
		case <-ctx.Done():
			if !*jsonOutput {
				clearListenStatus(app.Stderr)
				fmt.Fprintln(app.Stderr, "Listening stopped.")
			}
			drainListenJobs(jobs)
			return shareCommandExitCode(ctx, ctx.Err())
		case err := <-producerErr:
			drainListenJobs(jobs)
			if err == nil || errors.Is(err, context.Canceled) {
				if !*jsonOutput {
					clearListenStatus(app.Stderr)
					fmt.Fprintln(app.Stderr, "Listening stopped.")
				}
				return shareCommandExitCode(ctx, ctx.Err())
			}
			clearListenStatus(app.Stderr)
			fmt.Fprintf(app.Stderr, "Unable to keep listening: %v\n", err)
			return shareCommandExitCode(ctx, err)
		case job, ok := <-jobs:
			if !ok {
				return ExitSuccess
			}
			queued := len(jobs)
			if *jsonOutput {
				payload := map[string]any{
					"kind":    "ListenRequest",
					"origin":  job.origin,
					"request": job.request,
					"queued":  queued,
				}
				if job.created != nil {
					payload["shareId"] = job.created.Share.ID
				}
				if job.organization != "" {
					payload["organization"] = job.organization
					payload["space"] = job.space
				}
				app.writeJSON(payload)
			} else {
				pauseStatus.Store(true)
				clearListenStatus(app.Stderr)
				writeListenJobPrompt(app.Stderr, *job.request, job, queued, app.now())
			}

			if job.origin == "share" && job.created != nil {
				if pending, pendingErr := client.Pending(ctx, job.created.Share.ID, job.created.RunnerToken); pendingErr == nil && pending.Request != nil {
					job.request = pending.Request
				}
			}

			fulfill := *approveAll
			if !fulfill {
				switch strings.ToLower(strings.TrimSpace(job.request.OperatorDecision)) {
				case "approve":
					fulfill = true
					if !*jsonOutput {
						style := newTermStyle(app.Stderr)
						fmt.Fprintln(app.Stderr, "  "+style.dim("Console already approved this Request."))
					}
				case "reject":
					fulfill = false
					if !*jsonOutput {
						style := newTermStyle(app.Stderr)
						fmt.Fprintln(app.Stderr, "  "+style.dim("Console already rejected this Request."))
					}
				default:
					decision, decideErr := interaction.decideFulfillment()
					if decideErr != nil {
						pauseStatus.Store(false)
						if job.channel != nil {
							job.channel.Close()
						}
						drainListenJobs(jobs)
						return sharePromptExitCode(decideErr)
					}
					fulfill = decision
				}
			}

			if !fulfill {
				pauseStatus.Store(false)
				if job.origin == "share" {
					if rejectErr := submitOperatorRejected(ctx, job.channel, job.created, job.request, app.localNodeID(), app.now); rejectErr != nil {
						fmt.Fprintf(app.Stderr, "Unable to reject: %v\n", rejectErr)
						if job.channel != nil {
							job.channel.Close()
						}
						return shareCommandExitCode(ctx, rejectErr)
					}
				} else if registry != nil {
					if _, rejectErr := registry.DecideRequest(ctx, apiToken, job.organization, job.space, job.request.ID, "reject"); rejectErr != nil {
						fmt.Fprintf(app.Stderr, "Unable to reject: %v\n", rejectErr)
						return shareCommandExitCode(ctx, rejectErr)
					}
				}
				if !*jsonOutput {
					style := newTermStyle(app.Stderr)
					fmt.Fprintln(app.Stderr)
					fmt.Fprintln(app.Stderr, "  "+style.boldRed("→ Rejected.")+" "+style.dim("The recipient will see the Request cancelled."))
					fmt.Fprintln(app.Stderr)
				} else {
					payload := map[string]any{"kind": "ListenRejected", "requestId": job.request.ID, "origin": job.origin}
					if job.created != nil {
						payload["shareId"] = job.created.Share.ID
					}
					app.writeJSON(payload)
				}
				if job.channel != nil {
					job.channel.Close()
				}
				continue
			}

			if !*jsonOutput {
				style := newTermStyle(app.Stderr)
				fmt.Fprintln(app.Stderr)
				fmt.Fprintln(app.Stderr, "  "+style.boldGreen("→ Approved.")+" "+style.dim("Running on this machine…"))
				fmt.Fprintln(app.Stderr)
			}

			var code int
			capabilityName := ""
			if job.request != nil {
				capabilityName = job.request.Capability.Name
			} else if job.created != nil {
				capabilityName = job.created.Share.CapabilityRevision.Name
			}
			root, catalog, resolveErr := index.resolve(capabilityName, job.space)
			if resolveErr != nil {
				fmt.Fprintln(app.Stderr, resolveErr)
				pauseStatus.Store(false)
				if job.channel != nil {
					job.channel.Close()
				}
				drainListenJobs(jobs)
				return ExitContract
			}
			switch job.origin {
			case "space":
				if registry != nil && strings.ToLower(strings.TrimSpace(job.request.OperatorDecision)) != "approve" {
					if _, err := registry.DecideRequest(ctx, apiToken, job.organization, job.space, job.request.ID, "approve"); err != nil {
						fmt.Fprintf(app.Stderr, "Unable to approve: %v\n", err)
						pauseStatus.Store(false)
						return shareCommandExitCode(ctx, err)
					}
				}
				code = app.fulfillSpaceListenJob(ctx, registry, apiToken, scopeView.Header.Identity, root, catalog, job, *approveAll, interaction, outputValues, evidenceValues, *jsonOutput)
			default:
				code = app.fulfillListenJob(ctx, client, root, catalog, job, *approveAll, interaction, outputValues, evidenceValues, *jsonOutput)
			}
			pauseStatus.Store(false)
			if job.channel != nil {
				job.channel.Close()
			}
			if code != ExitSuccess {
				drainListenJobs(jobs)
				return code
			}
			if !*jsonOutput {
				style := newTermStyle(app.Stderr)
				fmt.Fprintln(app.Stderr)
				fmt.Fprintln(app.Stderr, "  "+style.boldGreen("→ Done.")+" "+style.dim("Back to listening…"))
				fmt.Fprintln(app.Stderr)
			}
		}
	}
}

type listenInboxConfig struct {
	shareClient *shareclient.Client
	registry    *registryclient.Client
	apiToken    string
	filters     listenFilters
	scopeView   listenScopeView
	pollEvery   time.Duration
	jsonOutput  bool
	pauseStatus *atomic.Bool
	jobs        chan<- listenJob
	errCh       chan<- error
}

func (app *App) listenInboxProducer(ctx context.Context, cfg listenInboxConfig) {
	defer close(cfg.jobs)

	var mu sync.Mutex
	claimedShares := map[string]bool{}
	claimedRequests := map[string]bool{}

	claimShare := func(shareID string) bool {
		mu.Lock()
		defer mu.Unlock()
		if claimedShares[shareID] {
			return false
		}
		claimedShares[shareID] = true
		return true
	}
	releaseShare := func(shareID string) {
		mu.Lock()
		delete(claimedShares, shareID)
		mu.Unlock()
	}
	claimRequest := func(requestID string) bool {
		mu.Lock()
		defer mu.Unlock()
		if claimedRequests[requestID] {
			return false
		}
		claimedRequests[requestID] = true
		return true
	}
	releaseRequest := func(requestID string) {
		mu.Lock()
		delete(claimedRequests, requestID)
		mu.Unlock()
	}

	ticker := time.NewTicker(cfg.pollEvery)
	defer ticker.Stop()

	poll := func() error {
		if cfg.registry == nil {
			return errors.New("registry client is required for listen")
		}
		inbox, err := cfg.registry.ListenInbox(ctx, cfg.apiToken, registryclient.ListenFilters{
			Organization: cfg.filters.Organization,
			Space:        cfg.filters.Space,
			Capability:   cfg.filters.Capability,
		})
		if err != nil {
			return err
		}
		if !cfg.jsonOutput && (cfg.pauseStatus == nil || !cfg.pauseStatus.Load()) {
			open := len(inbox.Shares)
			idle := 0
			shareReady := 0
			for _, item := range inbox.Shares {
				switch {
				case item.AwaitingFulfillment:
					shareReady++
				case !item.HasRequest:
					idle++
				}
			}
			writeListenScopeStatus(app.Stderr, len(inbox.Scopes), len(cfg.scopeView.Capabilities), open, shareReady, len(inbox.Requests))
		}
		for _, item := range inbox.Shares {
			shareItem := shareclient.InboxItem(item)
			if !item.AwaitingFulfillment && item.HasRequest {
				continue
			}
			if !claimShare(item.Share.ID) {
				continue
			}
			shareID := item.Share.ID
			go func(item shareclient.InboxItem) {
				handedOff, err := app.watchInboxShare(ctx, cfg, item)
				if err != nil {
					releaseShare(shareID)
					if ctx.Err() == nil {
						select {
						case cfg.errCh <- err:
						default:
						}
					}
					return
				}
				if !handedOff {
					releaseShare(shareID)
				}
			}(shareItem)
		}
		for _, pending := range inbox.Requests {
			if pending.Status != "requested" && pending.Status != "approved" {
				continue
			}
			if !claimRequest(pending.Summary.ID) {
				continue
			}
			requestID := pending.Summary.ID
			org := pending.Summary.Organization
			space := pending.Summary.Space
			if org == "" || space == "" {
				releaseRequest(requestID)
				continue
			}
			request := pending.Summary
			select {
			case <-ctx.Done():
				releaseRequest(requestID)
				return ctx.Err()
			case cfg.jobs <- listenJob{
				origin:       "space",
				request:      &request,
				organization: org,
				space:        space,
			}:
			default:
				releaseRequest(requestID)
			}
		}
		return nil
	}

	if err := poll(); err != nil {
		cfg.errCh <- err
		return
	}

	for {
		select {
		case <-ctx.Done():
			cfg.errCh <- ctx.Err()
			return
		case <-ticker.C:
			if err := poll(); err != nil {
				cfg.errCh <- err
				return
			}
		}
	}
}

func (app *App) watchInboxShare(ctx context.Context, cfg listenInboxConfig, item shareclient.InboxItem) (handedOff bool, err error) {
	created := &shareclient.ShareCreated{
		Share:       item.Share,
		RunnerToken: cfg.apiToken,
	}
	channel, initial, err := cfg.shareClient.OpenChannel(ctx, item.Share.ID, cfg.apiToken)
	if err != nil {
		return false, fmt.Errorf("connect Share %s: %w", item.Share.ID, err)
	}
	keepOpen := false
	defer func() {
		if !keepOpen {
			channel.Close()
		}
	}()

	if !cfg.jsonOutput && (cfg.pauseStatus == nil || !cfg.pauseStatus.Load()) {
		clearListenStatus(app.Stderr)
		writeListenAttached(app.Stderr, item, app.now())
	} else if cfg.jsonOutput {
		app.writeJSON(map[string]any{
			"kind":      "ListenShareAttached",
			"shareId":   item.Share.ID,
			"expiresAt": item.Share.ExpiresAt,
		})
	}

	waitContext, cancelWait := context.WithDeadline(ctx, item.Share.ExpiresAt)
	defer cancelWait()
	request, alreadyComplete, err := awaitSharedRequest(waitContext, cfg.shareClient, channel, created, initial)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return false, nil
		}
		return false, err
	}
	if alreadyComplete || request == nil {
		return false, nil
	}

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case cfg.jobs <- listenJob{origin: "share", created: created, channel: channel, request: request}:
		keepOpen = true
		return true, nil
	}
}

func (app *App) fulfillSpaceListenJob(
	ctx context.Context,
	registry *registryclient.Client,
	token, identity, root string,
	catalog *manifest.Catalog,
	job listenJob,
	approveAll bool,
	interaction *interaction,
	outputValues, evidenceValues map[string]string,
	jsonOutput bool,
) int {
	capabilityDefinition, recipeDefinition, err := resolveSpaceListenFulfillment(catalog, *job.request, interaction, approveAll)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}
	if code := app.validateShareHost(root, catalog, recipeDefinition); code != ExitSuccess {
		return code
	}
	executor := execution.ActorReference{Kind: "identity", ID: identity}
	if identity == "" {
		executor = app.localIdentity()
	}
	capabilityReference := execution.ReferenceCapability(capabilityDefinition)
	invocation := execution.Invocation{
		ProjectRoot:     root,
		Capability:      capabilityDefinition.Value,
		CapabilityRef:   capabilityReference,
		Executor:        executor,
		NodeID:          app.localNodeID(),
		Space:           job.space,
		ExistingRequest: job.request,
	}
	if recipeDefinition != nil {
		reference := execution.ReferenceRecipe(*recipeDefinition)
		invocation.Recipe = recipeDefinition.Value
		invocation.RecipeRef = &reference
		invocation.RecipeDirectory = recipeDefinition.Source.Directory
	}
	runtimeStdout := app.Stdout
	if jsonOutput {
		runtimeStdout = app.Stderr
	}
	options := execution.Options{
		ApproveAll:  approveAll,
		Approve:     interaction.approve,
		Manual:      interaction.manual(outputValues, evidenceValues),
		Stdout:      runtimeStdout,
		Stderr:      app.Stderr,
		Environment: app.environment(),
		Now:         app.now,
	}
	var timeline *runTimeline
	if !jsonOutput {
		timeline = newRunTimeline(app.Stderr, invocation)
		options.OnEvent = timeline.onEvent
	}
	result, runErr := execution.Execute(ctx, invocation, options)
	if registry != nil && job.organization != "" && job.space != "" {
		if _, flushErr := runsync.Flush(ctx, root, registry, token, job.organization, job.space); flushErr != nil {
			fmt.Fprintf(app.Stderr, "warning: run outbox flush: %v\n", flushErr)
		}
	}
	if jsonOutput && result.Run.ID != "" {
		app.writeJSON(map[string]any{
			"kind":         "ListenRunFinished",
			"origin":       "space",
			"organization": job.organization,
			"space":        job.space,
			"runId":        result.Run.ID,
			"status":       result.Status,
			"requestId":    job.request.ID,
		})
	} else if !jsonOutput {
		elapsed := time.Duration(0)
		if timeline != nil {
			elapsed = timeline.elapsed()
		}
		writeLocalRunSummary(app.Stdout, result, elapsed)
		if runErr != nil {
			fmt.Fprintf(app.Stderr, "run failed: %v\n", runErr)
		}
	}
	return executionExitCode(runErr)
}

func resolveSpaceListenFulfillment(catalog *manifest.Catalog, request execution.RequestRecord, interaction *interaction, approveAll bool) (manifest.CapabilityDefinition, *manifest.RecipeDefinition, error) {
	capabilityDefinition, err := findCapability(catalog, request.Capability.Name, request.Capability.Version)
	if err != nil {
		return manifest.CapabilityDefinition{}, nil, fmt.Errorf("local Capability for Request: %w", err)
	}
	selected, selectionErr := catalog.ResolveRecipe(request.Capability.Name, "")
	switch {
	case selectionErr == nil:
		return capabilityDefinition, &selected, nil
	case errors.Is(selectionErr, manifest.ErrRecipeNotFound):
		if approveAll {
			return capabilityDefinition, nil, nil
		}
		picked, pickErr := pickListenRecipe(catalog, request.Capability.Name, interaction, approveAll)
		if pickErr != nil {
			return manifest.CapabilityDefinition{}, nil, pickErr
		}
		return capabilityDefinition, &picked, nil
	default:
		return manifest.CapabilityDefinition{}, nil, selectionErr
	}
}

func writeListenJobPrompt(writer io.Writer, request execution.RequestRecord, job listenJob, queued int, now time.Time) {
	if job.origin == "share" && job.created != nil {
		writeListenDecisionPrompt(writer, request, job.created, queued, now)
		return
	}
	writeListenSpaceDecisionPrompt(writer, request, job.organization, job.space, queued, now)
}

func writeListenSpaceDecisionPrompt(writer io.Writer, request execution.RequestRecord, organization, space string, queued int, now time.Time) {
	style := newTermStyle(writer)
	rule := style.cyan("════════════════════════════════════════")
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, rule)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Request"), style.boldCyan(fmt.Sprintf("%s@%s", request.Capability.Name, request.Capability.Version)))
	fmt.Fprintln(writer, rule)
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("From"), style.value(listenRequester(request.RequestedBy)))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Id"), style.value(shortListenID(request.ID)))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Scope"), style.value(organization+"/"+space))
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
	if queued > 0 {
		fmt.Fprintln(writer)
		fmt.Fprintf(writer, "  %s\n", style.dim(fmt.Sprintf("(%s queued after this)", listenCount(queued, "Request", "Requests"))))
	}
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, rule)
	fmt.Fprintln(writer)
}

func (app *App) fulfillListenJob(
	ctx context.Context,
	client *shareclient.Client,
	root string,
	catalog *manifest.Catalog,
	job listenJob,
	approveAll bool,
	interaction *interaction,
	outputValues, evidenceValues map[string]string,
	jsonOutput bool,
) int {
	capabilityDefinition, recipeDefinition, err := resolveListenFulfillment(catalog, job.created.Share, interaction, approveAll)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}
	if code := app.validateShareHost(root, catalog, recipeDefinition); code != ExitSuccess {
		return code
	}

	capabilityReference := execution.ReferenceCapability(capabilityDefinition)
	invocation := execution.Invocation{
		ProjectRoot:     root,
		Capability:      capabilityDefinition.Value,
		CapabilityRef:   capabilityReference,
		Executor:        job.created.Share.SharedBy,
		NodeID:          app.localNodeID(),
		ShareID:         job.created.Share.ID,
		ExistingRequest: job.request,
	}
	if recipeDefinition != nil {
		reference := execution.ReferenceRecipe(*recipeDefinition)
		invocation.Recipe = recipeDefinition.Value
		invocation.RecipeRef = &reference
		invocation.RecipeDirectory = recipeDefinition.Source.Directory
	}

	runtimeStdout := app.Stdout
	if jsonOutput {
		runtimeStdout = app.Stderr
	}
	options := execution.Options{
		ApproveAll: approveAll,
		Approve:    interaction.approve,
		Manual:     interaction.manual(outputValues, evidenceValues),
		OnRun: func(callbackContext context.Context, run execution.RunRecord) error {
			if job.channel == nil {
				return nil
			}
			_, err := job.channel.SubmitRun(callbackContext, run)
			return err
		},
		BeforeSuccess: func(callbackContext context.Context, run execution.RunRecord, returns, evidence map[string]any) error {
			if job.channel == nil {
				return nil
			}
			return uploadSharedArtifacts(callbackContext, app.Stderr, client, job.created.Share.ID, job.created.RunnerToken, run.ID, returns, evidence)
		},
		Stdout:      runtimeStdout,
		Stderr:      app.Stderr,
		Environment: app.environment(),
		Now:         app.now,
	}
	var timeline *runTimeline
	if !jsonOutput {
		timeline = newRunTimeline(app.Stderr, invocation)
		options.OnEvent = func(callbackContext context.Context, event execution.RunEvent) error {
			if err := timeline.onEvent(callbackContext, event); err != nil {
				return err
			}
			if job.channel == nil {
				return nil
			}
			_, err := job.channel.SubmitRunEvent(callbackContext, event)
			return err
		}
	} else {
		options.OnEvent = func(callbackContext context.Context, event execution.RunEvent) error {
			if job.channel == nil {
				return nil
			}
			_, err := job.channel.SubmitRunEvent(callbackContext, event)
			return err
		}
	}
	result, runErr := execution.Execute(ctx, invocation, options)

	if jsonOutput {
		if result.Run.ID != "" {
			app.writeJSON(map[string]any{
				"kind":      "ListenRunFinished",
				"shareId":   job.created.Share.ID,
				"runId":     result.Run.ID,
				"status":    result.Status,
				"requestId": job.request.ID,
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
	return executionExitCode(runErr)
}

func resolveListenFulfillment(catalog *manifest.Catalog, share shareclient.Share, interaction *interaction, approveAll bool) (manifest.CapabilityDefinition, *manifest.RecipeDefinition, error) {
	var empty manifest.CapabilityDefinition
	capabilityDefinition, err := resolveCapabilityArgument(catalog, "capability/"+share.CapabilityRevision.Name+"@"+share.CapabilityRevision.Version)
	if err != nil {
		return empty, nil, fmt.Errorf("local Capability for Share: %w", err)
	}
	localRef := execution.ReferenceCapability(capabilityDefinition)
	if localRef != share.CapabilityRevision {
		return empty, nil, fmt.Errorf("local Capability %s@%s digest does not match the Share", share.CapabilityRevision.Name, share.CapabilityRevision.Version)
	}

	var recipeDefinition *manifest.RecipeDefinition
	if share.Recipe != nil {
		selected, selectionErr := catalog.ResolveRecipe(share.CapabilityRevision.Name, share.Recipe.Name+"@"+share.Recipe.Version)
		if selectionErr != nil {
			selected, selectionErr = catalog.ResolveRecipe(share.CapabilityRevision.Name, share.Recipe.Name)
		}
		if selectionErr != nil {
			return empty, nil, fmt.Errorf("local Recipe for Share: %w", selectionErr)
		}
		recipeDefinition = &selected
		localRecipe := execution.ReferenceRecipe(selected)
		if localRecipe != *share.Recipe {
			return empty, nil, fmt.Errorf("local Recipe %s digest does not match the Share", share.Recipe.Name)
		}
	} else {
		selected, selectionErr := catalog.ResolveRecipe(share.CapabilityRevision.Name, "")
		switch {
		case selectionErr == nil:
			recipeDefinition = &selected
		case errors.Is(selectionErr, manifest.ErrRecipeNotFound):
		case errors.Is(selectionErr, manifest.ErrRecipeAmbiguous):
			picked, pickErr := pickListenRecipe(catalog, share.CapabilityRevision.Name, interaction, approveAll)
			if pickErr != nil {
				return empty, nil, pickErr
			}
			recipeDefinition = &picked
		default:
			return empty, nil, selectionErr
		}
	}
	return capabilityDefinition, recipeDefinition, nil
}

func pickListenRecipe(catalog *manifest.Catalog, capability string, interaction *interaction, approveAll bool) (manifest.RecipeDefinition, error) {
	matches := catalog.RecipesForCapability(capability)
	if len(matches) == 0 {
		return manifest.RecipeDefinition{}, manifest.ErrRecipeNotFound
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if approveAll {
		return matches[0], nil
	}
	if interaction == nil {
		return manifest.RecipeDefinition{}, fmt.Errorf("%w for %s; select one explicitly", manifest.ErrRecipeAmbiguous, capability)
	}
	style := newTermStyle(interaction.output)
	fmt.Fprintln(interaction.output)
	fmt.Fprintln(interaction.output, "  "+style.bold("Multiple Recipes provide "+capability+". Pick one:"))
	for index, match := range matches {
		fmt.Fprintf(interaction.output, "  %s  %s@%s\n", style.label(fmt.Sprintf("[%d]", index+1)), match.Value.Metadata.Name, match.Value.Metadata.Version)
	}
	for {
		answer, err := interaction.read("  Recipe › ")
		if err != nil {
			return manifest.RecipeDefinition{}, err
		}
		answer = strings.TrimSpace(answer)
		if n, convErr := strconv.Atoi(answer); convErr == nil && n >= 1 && n <= len(matches) {
			return matches[n-1], nil
		}
		for _, match := range matches {
			name := match.Value.Metadata.Name
			full := name + "@" + match.Value.Metadata.Version
			if answer == name || answer == full {
				return match, nil
			}
		}
		fmt.Fprintln(interaction.output, "  "+style.yellow("Not recognized. Enter a number or Recipe name."))
	}
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

func writeListenDecisionPrompt(writer io.Writer, request execution.RequestRecord, created *shareclient.ShareCreated, queued int, now time.Time) {
	style := newTermStyle(writer)
	rule := style.cyan("════════════════════════════════════════")
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, rule)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Request"), style.boldCyan(fmt.Sprintf("%s@%s", request.Capability.Name, request.Capability.Version)))
	fmt.Fprintln(writer, rule)
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("From"), style.value(listenRequester(request.RequestedBy)))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Id"), style.value(shortListenID(request.ID)))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Share"), style.value(shortListenID(created.Share.ID)))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Expires"), style.value(listenExpiry(created.Share.ExpiresAt, now)))
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
	if queued > 0 {
		fmt.Fprintln(writer)
		fmt.Fprintf(writer, "  %s\n", style.dim(fmt.Sprintf("(%s queued after this)", listenCount(queued, "Request", "Requests"))))
	}
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, rule)
	fmt.Fprintln(writer)
}

func (interaction *interaction) decideFulfillment() (bool, error) {
	style := newTermStyle(interaction.output)
	prompt := fmt.Sprintf("  %s   %s  › ", style.boldGreen("[a] Approve"), style.boldRed("[r] Reject"))
	for {
		answer, err := interaction.read(prompt)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "a", "accept", "approve", "y", "yes", "s", "si", "sí", "aceptar":
			return true, nil
		case "r", "reject", "n", "no", "rechazar":
			return false, nil
		case "":
			fmt.Fprintln(interaction.output, "  "+style.dim("Empty. Type a or r."))
		default:
			fmt.Fprintln(interaction.output, "  "+style.yellow("Not recognized. Type a (approve) or r (reject)."))
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

func submitOperatorRejected(
	ctx context.Context,
	channel *shareclient.Channel,
	created *shareclient.ShareCreated,
	request *execution.RequestRecord,
	nodeID string,
	now func() time.Time,
) error {
	runID, err := listenUUID()
	if err != nil {
		return err
	}
	when := now().UTC().Truncate(time.Millisecond)
	run := execution.RunRecord{
		APIVersion: execution.APIVersion,
		Kind:       "Run",
		ID:         runID,
		RequestID:  request.ID,
		CreatedAt:  when,
		Capability: created.Share.CapabilityRevision,
		Recipe:     created.Share.Recipe,
		Inputs:     request.Inputs,
		Executor:   created.Share.SharedBy,
		NodeID:     nodeID,
	}
	if _, err := channel.SubmitRun(ctx, run); err != nil {
		return err
	}
	events := []execution.RunEvent{
		{
			APIVersion: execution.APIVersion, Kind: "RunEvent", RunID: runID,
			Sequence: 0, OccurredAt: when, Type: "run_created",
		},
		{
			APIVersion: execution.APIVersion, Kind: "RunEvent", RunID: runID,
			Sequence: 1, OccurredAt: when, Type: "run_cancelled",
			Data: map[string]any{"reason": "operator_rejected"},
		},
	}
	for _, event := range events {
		if _, err := channel.SubmitRunEvent(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func drainListenJobs(jobs <-chan listenJob) {
	for {
		select {
		case job, ok := <-jobs:
			if !ok {
				return
			}
			if job.channel != nil {
				job.channel.Close()
			}
		default:
			return
		}
	}
}

func listenUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	hex := fmt.Sprintf("%x", value)
	return strings.Join([]string{hex[0:8], hex[8:12], hex[12:16], hex[16:20], hex[20:32]}, "-"), nil
}
