package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/registryclient"
	"doppels.so/cli/internal/shareclient"
)

type shareSessionView struct {
	Kind      string    `json:"kind"`
	ShareID   string    `json:"shareId"`
	PublicURL string    `json:"publicUrl"`
	ExpiresAt time.Time `json:"expiresAt"`
	Status    string    `json:"status"`
}

type shareClosedPayload struct {
	Reason string `json:"reason"`
}

type sharedRunError struct {
	status string
	runID  string
}

func (problem sharedRunError) Error() string {
	return fmt.Sprintf("Share Run %s finished with status %s", problem.runID, problem.status)
}

func (app *App) runShare(arguments []string) int {
	defaultServer := environmentValue(app.environment(), "DOPPELS_SERVER")
	apiToken := environmentValue(app.environment(), "DOPPELS_API_TOKEN")
	sessionServer := ""
	loggedIn := apiToken != ""
	if resolvedStore, configErr := app.configStore(); configErr == nil {
		if profile, profileErr := resolvedStore.Profile(); profileErr == nil && defaultServer == "" {
			defaultServer = profile.Server
		} else if profileErr != nil && !errors.Is(profileErr, configstore.ErrNotLoggedIn) && !errors.Is(profileErr, configstore.ErrNotConfigured) {
			fmt.Fprintf(app.Stderr, "load CLI profile: %v\n", profileErr)
			return ExitOperational
		}
		if apiToken == "" {
			session, sessionErr := resolvedStore.Session()
			switch {
			case sessionErr == nil:
				apiToken = session.Token
				sessionServer = session.Profile.Server
				loggedIn = true
				if defaultServer == "" {
					defaultServer = session.Profile.Server
				}
			case errors.Is(sessionErr, configstore.ErrNotLoggedIn), errors.Is(sessionErr, configstore.ErrNotConfigured):
				// Anonymous share is allowed (ngrok-style).
			default:
				fmt.Fprintf(app.Stderr, "load login: %v\n", sessionErr)
				return ExitOperational
			}
		}
	}
	if defaultServer == "" {
		defaultServer = "https://doppels.so"
	}
	flags := app.flagSet("share")
	expiresText := flags.String("expires", "1h", "one-shot Share lifetime")
	server := flags.String("server", defaultServer, "Doppels control-plane URL")
	recipeName := flags.String("recipe", "", "compatible Recipe name[@version]")
	approveAll := flags.Bool("yes", false, "approve every Step that explicitly requires approval")
	strict := flags.Bool("strict", false, "refuse to share when a lock pin is stale")
	inputsLocked := flags.Bool("locked", false, "make --input values immutable on the public form")
	jsonOutput := flags.Bool("json", false, "write the initial Share session as JSON")
	var rawInputs, rawOutputs, rawEvidence namedValues
	flags.Var(&rawInputs, "input", "prefill Capability input as name=value (repeatable)")
	flags.Var(&rawOutputs, "output", "manual output as name=value (repeatable)")
	flags.Var(&rawEvidence, "evidence", "manual evidence as name=value (repeatable)")
	if err := flags.Parse(resourceFirst(arguments)); err != nil {
		return ExitContract
	}
	if *inputsLocked && len(rawInputs) == 0 {
		fmt.Fprintln(app.Stderr, "--locked requires at least one --input name=value")
		return ExitContract
	}
	if sessionServer != "" {
		target, targetErr := registryclient.ParseServer(*server)
		profileTarget, profileErr := registryclient.ParseServer(sessionServer)
		if targetErr != nil || profileErr != nil || target.String() != profileTarget.String() {
			writeServerTokenMismatch(app.Stderr, sessionServer, *server)
			return ExitContract
		}
	}

	root, catalog, localCode := app.localCatalog()
	hasLocal := localCode == ExitSuccess

	capabilityRef := ""
	if flags.NArg() == 1 {
		capabilityRef = flags.Arg(0)
	} else if flags.NArg() > 1 {
		fmt.Fprintln(app.Stderr, "share accepts at most one capability/<name>[@version]")
		return ExitContract
	}

	interaction := newInteraction(app.Stdin, app.Stderr)
	if !hasLocal {
		if !loggedIn || apiToken == "" {
			return localCode
		}
		if capabilityRef == "" {
			fmt.Fprintln(app.Stderr, "share without a local Space requires capability/<name>[@version] (from the current cloud Space registry)")
			return ExitContract
		}
	} else if capabilityRef == "" {
		if *jsonOutput {
			fmt.Fprintln(app.Stderr, "share requires capability/<name>[@version] when using --json")
			return ExitContract
		}
		selected, pickErr := interaction.pickCapability(catalog, "Share a Capability")
		if pickErr != nil {
			fmt.Fprintln(app.Stderr, pickErr)
			return sharePromptExitCode(pickErr)
		}
		capabilityRef = "capability/" + selected.Value.Metadata.Name + "@" + selected.Value.Metadata.Version
		*expiresText, pickErr = interaction.askDuration(*expiresText)
		if pickErr != nil {
			fmt.Fprintln(app.Stderr, pickErr)
			return sharePromptExitCode(pickErr)
		}
	}

	lifetime, err := time.ParseDuration(*expiresText)
	if err != nil || lifetime <= 0 {
		fmt.Fprintln(app.Stderr, "--expires must be a positive duration such as 30m or 1h")
		return ExitContract
	}
	createdAt := app.now()
	expiresAt := createdAt.Add(lifetime).UTC().Truncate(time.Millisecond)
	if !expiresAt.After(createdAt) {
		fmt.Fprintln(app.Stderr, "--expires is too short")
		return ExitContract
	}

	var capabilityDefinition manifest.CapabilityDefinition
	var recipeDefinition *manifest.RecipeDefinition
	if hasLocal {
		capabilityDefinition, err = resolveCapabilityArgument(catalog, capabilityRef)
		if err != nil {
			fmt.Fprintln(app.Stderr, err)
			return ExitContract
		}
		selected, selectionErr := catalog.ResolveRecipe(capabilityDefinition.Value.Metadata.Name, *recipeName)
		switch {
		case selectionErr == nil:
			recipeDefinition = &selected
		case errors.Is(selectionErr, manifest.ErrRecipeNotFound) && *recipeName == "":
			// A Capability without a Recipe remains shareable and is fulfilled by its owner.
		case errors.Is(selectionErr, manifest.ErrRecipeAmbiguous) && *recipeName == "" && !*jsonOutput && interaction.canPrompt():
			picked, pickErr := interaction.pickRecipe(catalog.RecipesForCapability(capabilityDefinition.Value.Metadata.Name))
			if pickErr != nil {
				fmt.Fprintln(app.Stderr, pickErr)
				return sharePromptExitCode(pickErr)
			}
			recipeDefinition = &picked
		default:
			fmt.Fprintln(app.Stderr, selectionErr)
			if errors.Is(selectionErr, manifest.ErrRecipeAmbiguous) && *recipeName == "" {
				fmt.Fprintln(app.Stderr, "hint: pass --recipe <name>[@version], or run on a TTY without --json to choose")
			}
			return ExitContract
		}
	} else {
		capabilityDefinition, err = app.capabilityFromRegistry(apiToken, *server, capabilityRef)
		if err != nil {
			fmt.Fprintf(app.Stderr, "resolve Capability from registry: %v\n", err)
			return ExitContract
		}
		if *recipeName != "" {
			fmt.Fprintln(app.Stderr, "--recipe requires a local Space working tree")
			return ExitContract
		}
		if cwd, cwdErr := app.Getwd(); cwdErr == nil {
			root = cwd
		}
	}

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
	inputValues, err := parseNamedValues(rawInputs)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}
	var typedShareInputs map[string]any
	if len(inputValues) > 0 {
		typedShareInputs, err = parseSharePrefillInputs(capabilityDefinition.Value, inputValues)
		if err != nil {
			fmt.Fprintln(app.Stderr, err)
			return ExitContract
		}
	}
	if recipeDefinition != nil && recipeDefinition.Value.Runtime == "shell" && (len(outputValues) > 0 || len(evidenceValues) > 0) {
		fmt.Fprintln(app.Stderr, "--output and --evidence are only valid for manual fulfillment")
		return ExitContract
	}
	var pinIssues []lockPinIssue
	if hasLocal {
		var pinCode int
		pinIssues, pinCode = app.checkLockPin(root, capabilityDefinition, recipeDefinition, *strict)
		if pinCode != ExitSuccess {
			return pinCode
		}
		if code := app.validateShareHost(root, catalog, recipeDefinition); code != ExitSuccess {
			return code
		}
	}

	client, err := shareclient.New(shareclient.Options{Server: *server, HTTPClient: app.HTTPClient, Now: app.now})
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}

	capabilityReference := execution.ReferenceCapability(capabilityDefinition)
	createRequest := shareclient.CreateShareRequest{
		CapabilityRevision:    capabilityReference,
		Capability:            capabilityDefinition.Value,
		ExpiresAt:             expiresAt,
		Inputs:                typedShareInputs,
		InputsLocked:          *inputsLocked,
		ArtifactRetentionDays: 7,
	}
	if recipeDefinition != nil {
		reference := execution.ReferenceRecipe(*recipeDefinition)
		createRequest.Recipe = &reference
		createRequest.ArtifactRetentionDays = recipeDefinition.Value.ArtifactRetentionDaysOrDefault()
	}
	ctx := app.context()
	created, err := client.Create(ctx, apiToken, createRequest)
	if err != nil {
		fmt.Fprintf(app.Stderr, "create Share: %v\n", err)
		return shareCommandExitCode(ctx, err)
	}
	app.writeShareSession(created, *jsonOutput, loggedIn)

	channel, initial, err := client.OpenChannel(ctx, created.Share.ID, created.RunnerToken)
	if err != nil {
		fmt.Fprintf(app.Stderr, "connect Share: %v\n", err)
		return shareCommandExitCode(ctx, err)
	}
	defer channel.Close()

	waitContext, cancelWait := context.WithDeadline(ctx, created.Share.ExpiresAt)
	var waitSpin *waitSpinner
	if !*jsonOutput {
		waitSpin = startShareWaitSpinner(app.Stderr)
	}
	request, alreadyComplete, err := awaitSharedRequest(waitContext, client, channel, created, initial)
	waitSpin.Stop()
	cancelWait()
	expiredBeforeRequest := errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil
	if expiredBeforeRequest {
		// A Request persisted at the expiry boundary wins over the local timer.
		// Query the durable source of truth once before declaring the Share expired.
		recoveryContext, cancelRecovery := context.WithTimeout(ctx, 5*time.Second)
		pending, pendingErr := client.Pending(recoveryContext, created.Share.ID, created.RunnerToken)
		cancelRecovery()
		if pendingErr == nil {
			recoveredRequest, recoveredComplete, recoveryErr := requestFromPending(created, pending)
			switch {
			case recoveryErr != nil:
				err, expiredBeforeRequest = recoveryErr, false
			case recoveredRequest != nil || recoveredComplete:
				request, alreadyComplete, err, expiredBeforeRequest = recoveredRequest, recoveredComplete, nil, false
			}
		}
		if ctx.Err() != nil {
			err, expiredBeforeRequest = ctx.Err(), false
		}
	}
	if err != nil {
		if expiredBeforeRequest {
			fmt.Fprintln(app.Stderr, "Share expired before a Request was received")
			return ExitOperational
		}
		fmt.Fprintf(app.Stderr, "wait for Share Request: %v\n", err)
		return shareCommandExitCode(ctx, err)
	}
	if alreadyComplete {
		fmt.Fprintf(app.Stderr, "Share %s is already complete\n", created.Share.ID)
		return ExitSuccess
	}
	writeSharedRequest(app.Stderr, *request)

	invocation := execution.Invocation{
		ProjectRoot:     root,
		Capability:      capabilityDefinition.Value,
		CapabilityRef:   capabilityReference,
		Executor:        created.Share.SharedBy,
		NodeID:          app.localNodeID(),
		ShareID:         created.Share.ID,
		ExistingRequest: request,
	}
	if recipeDefinition != nil {
		reference := execution.ReferenceRecipe(*recipeDefinition)
		invocation.Recipe = recipeDefinition.Value
		invocation.RecipeRef = &reference
		invocation.RecipeDirectory = recipeDefinition.Source.Directory
	}

	runtimeStdout := app.Stdout
	if *jsonOutput {
		// stdout has already emitted its one machine-readable ShareSession document.
		runtimeStdout = app.Stderr
	} else {
		runtimeStdout = prefixLines(runtimeStdout, "    ")
	}
	options := execution.Options{
		ApproveAll: *approveAll,
		Approve:    interaction.approve,
		Manual:     interaction.manual(outputValues, evidenceValues),
		OnRun: func(callbackContext context.Context, run execution.RunRecord) error {
			_, err := channel.SubmitRun(callbackContext, run)
			return err
		},
		BeforeSuccess: func(callbackContext context.Context, run execution.RunRecord, returns, evidence map[string]any) error {
			return uploadSharedArtifacts(callbackContext, app.Stderr, client, created.Share.ID, created.RunnerToken, run.ID, returns, evidence)
		},
		Stdout:      runtimeStdout,
		Stderr:      app.Stderr,
		Environment: app.environment(),
		Now:         app.now,
	}
	var timeline *runTimeline
	if !*jsonOutput {
		timeline = newRunTimeline(app.Stderr, invocation)
		timeline.hideCatalog = true
		if len(pinIssues) > 0 {
			timeline.pinWarnings = []string{"stale"}
		}
		options.OnEvent = func(callbackContext context.Context, event execution.RunEvent) error {
			if err := timeline.onEvent(callbackContext, event); err != nil {
				return err
			}
			_, err := channel.SubmitRunEvent(callbackContext, event)
			return err
		}
	} else {
		options.OnEvent = func(callbackContext context.Context, event execution.RunEvent) error {
			_, err := channel.SubmitRunEvent(callbackContext, event)
			return err
		}
	}
	result, runErr := execution.Execute(ctx, invocation, options)

	if *jsonOutput {
		if result.Run.ID != "" {
			fmt.Fprintf(app.Stderr, "Share %s: Run %s finished with status %s\n", created.Share.ID, result.Run.ID, result.Status)
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

func (app *App) validateShareHost(root string, catalog *manifest.Catalog, recipe *manifest.RecipeDefinition) int {
	if recipe == nil || recipe.Value.Runtime != "shell" {
		return ExitSuccess
	}
	documents := []manifest.Loaded{recipe.Source}
	for _, capabilityName := range recipe.Value.Provides {
		for _, definition := range catalog.Capabilities[capabilityName] {
			documents = append(documents, definition.Source)
		}
	}
	validation := manifest.Validate(documents, manifest.ValidationOptions{Root: root, CheckHost: true, Host: app.Host})
	if len(validation.Diagnostics) == 0 {
		return ExitSuccess
	}
	catalogRoot := root
	if catalog != nil && catalog.Root != "" {
		catalogRoot = catalog.Root
	}
	var other []manifest.Diagnostic
	hostFailed := false
	for _, diagnostic := range validation.Diagnostics {
		if strings.HasPrefix(diagnostic.Code, "host.") {
			hostFailed = true
			continue
		}
		other = append(other, diagnostic)
	}
	if hostFailed {
		writeHostNotReady(
			app.Stderr,
			recipe.Value.Metadata.Name,
			listenRelPath(catalogRoot, recipe.Source.Path),
			manifest.CheckRequires(recipe.Value, catalogRoot, app.Host),
		)
	}
	for _, diagnostic := range other {
		fmt.Fprintln(app.Stderr, diagnostic.Error())
	}
	return diagnosticsExitCode(validation.Diagnostics)
}

func (app *App) writeShareSession(created *shareclient.ShareCreated, jsonOutput bool, loggedIn bool) {
	if jsonOutput {
		app.writeJSON(shareSessionView{
			Kind: "ShareSession", ShareID: created.Share.ID, PublicURL: created.PublicURL,
			ExpiresAt: created.Share.ExpiresAt, Status: "listening",
		})
		return
	}
	writeShareSessionHuman(app.Stdout, created, app.now(), loggedIn)
}

func sharePromptExitCode(err error) int {
	if errors.Is(err, io.EOF) || errors.Is(err, errSharePromptCancelled) {
		return ExitInterrupted
	}
	return ExitContract
}

var errSharePromptCancelled = errors.New("cancelled")

func (interaction *interaction) pickCapability(catalog *manifest.Catalog, title string) (manifest.CapabilityDefinition, error) {
	options := listShareCapabilities(catalog)
	if len(options) == 0 {
		return manifest.CapabilityDefinition{}, errors.New("no Capabilities found in this Space")
	}
	style := newTermStyle(interaction.output)
	fmt.Fprintln(interaction.output)
	if title == "" {
		title = "Select a Capability"
	}
	fmt.Fprintln(interaction.output, "  "+style.bold(title))
	labels := make([]string, len(options))
	for index, option := range options {
		meta := option.Value.Metadata
		label := meta.Name + "@" + meta.Version
		if meta.DisplayName != "" {
			label += " — " + meta.DisplayName
		} else if meta.Summary != "" {
			label += " — " + meta.Summary
		}
		labels[index] = label
	}
	index, err := interaction.pickIndex("Capability", labels)
	if err != nil {
		return manifest.CapabilityDefinition{}, err
	}
	return options[index], nil
}

func (interaction *interaction) pickRecipe(options []manifest.RecipeDefinition) (manifest.RecipeDefinition, error) {
	if len(options) == 0 {
		return manifest.RecipeDefinition{}, manifest.ErrRecipeNotFound
	}
	style := newTermStyle(interaction.output)
	fmt.Fprintln(interaction.output)
	fmt.Fprintln(interaction.output, "  "+style.bold("Multiple Recipes provide this Capability"))
	labels := make([]string, len(options))
	for index, option := range options {
		meta := option.Value.Metadata
		label := meta.Name + "@" + meta.Version
		if meta.DisplayName != "" {
			label += " — " + meta.DisplayName
		}
		labels[index] = label
	}
	index, err := interaction.pickIndex("Recipe", labels)
	if err != nil {
		return manifest.RecipeDefinition{}, err
	}
	return options[index], nil
}

func (interaction *interaction) askDuration(defaultValue string) (string, error) {
	if defaultValue == "" {
		defaultValue = "1h"
	}
	style := newTermStyle(interaction.output)
	answer, err := interaction.read(fmt.Sprintf("  %s  [%s]: ", style.field("Expires"), defaultValue))
	if err != nil {
		return "", err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return defaultValue, nil
	}
	if _, err := time.ParseDuration(answer); err != nil {
		return "", fmt.Errorf("invalid duration %q (use 30m, 1h, 2h…)", answer)
	}
	return answer, nil
}

func (interaction *interaction) pickIndex(kind string, labels []string) (int, error) {
	style := newTermStyle(interaction.output)
	for index, label := range labels {
		fmt.Fprintf(interaction.output, "  %s  %s\n", style.label(fmt.Sprintf("[%d]", index+1)), label)
	}
	defaultHint := "1"
	if len(labels) == 1 {
		answer, err := interaction.read(fmt.Sprintf("  %s  [%s]: ", style.field(kind), defaultHint))
		if err != nil {
			return 0, err
		}
		answer = strings.TrimSpace(answer)
		if answer == "" {
			return 0, nil
		}
		return parsePickIndex(answer, len(labels))
	}
	answer, err := interaction.read(fmt.Sprintf("  %s  [1-%d]: ", style.field(kind), len(labels)))
	if err != nil {
		return 0, err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return 0, nil
	}
	return parsePickIndex(answer, len(labels))
}

func parsePickIndex(answer string, count int) (int, error) {
	var selected int
	if _, err := fmt.Sscanf(answer, "%d", &selected); err != nil {
		return 0, fmt.Errorf("choose a number between 1 and %d", count)
	}
	if selected < 1 || selected > count {
		return 0, fmt.Errorf("choose a number between 1 and %d", count)
	}
	return selected - 1, nil
}

func listShareCapabilities(catalog *manifest.Catalog) []manifest.CapabilityDefinition {
	names := make([]string, 0, len(catalog.Capabilities))
	for name := range catalog.Capabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	options := make([]manifest.CapabilityDefinition, 0, len(names))
	for _, name := range names {
		revisions := catalog.Capabilities[name]
		if len(revisions) == 0 {
			continue
		}
		// Prefer the latest local revision for interactive share.
		options = append(options, revisions[len(revisions)-1])
	}
	return options
}

func awaitSharedRequest(ctx context.Context, client *shareclient.Client, channel *shareclient.Channel, created *shareclient.ShareCreated, initial *shareclient.PendingState) (*execution.RequestRecord, bool, error) {
	if request, complete, err := requestFromPending(created, initial); request != nil || complete || err != nil {
		return request, complete, err
	}
	for {
		update, err := channel.Next(ctx)
		if err != nil {
			return nil, false, err
		}
		if update.Recovery != nil {
			if request, complete, err := requestFromPending(created, update.Recovery); request != nil || complete || err != nil {
				return request, complete, err
			}
			continue
		}
		if update.Message == nil {
			continue
		}
		switch update.Message.Event {
		case "request_available":
			request, err := update.Message.Request()
			if err != nil {
				return nil, false, fmt.Errorf("decode Request: %w", err)
			}
			if err := validateSharedRequest(created.Share, request); err != nil {
				return nil, false, err
			}
			return request, false, nil
		case "share_closed":
			var closed shareClosedPayload
			if err := json.Unmarshal(update.Message.Payload, &closed); err != nil {
				return nil, false, fmt.Errorf("decode Share closure: %w", err)
			}
			if closed.Reason != "completed" {
				return nil, false, fmt.Errorf("Share was %s", closed.Reason)
			}
			pending, err := client.Pending(ctx, created.Share.ID, created.RunnerToken)
			if err != nil {
				return nil, false, err
			}
			return requestFromPending(created, pending)
		}
	}
}

func requestFromPending(created *shareclient.ShareCreated, pending *shareclient.PendingState) (*execution.RequestRecord, bool, error) {
	if pending == nil {
		return nil, false, errors.New("Cloud returned no pending Share state")
	}
	if !sharesEqual(created.Share, pending.Share) {
		return nil, false, errors.New("Cloud pending state changed the Share contract")
	}
	if pending.Request == nil {
		if pending.Run != nil || len(pending.Events) != 0 {
			return nil, false, errors.New("Cloud returned a Run without its Request")
		}
		return nil, false, nil
	}
	if err := validateSharedRequest(created.Share, pending.Request); err != nil {
		return nil, false, err
	}
	if pending.Run == nil {
		if len(pending.Events) != 0 {
			return nil, false, errors.New("Cloud returned RunEvents without a Run")
		}
		return pending.Request, false, nil
	}
	if err := validatePendingRun(created.Share, pending.Request, pending.Run, pending.Events); err != nil {
		return nil, false, err
	}
	status, terminal := terminalRunStatus(pending.Events)
	if !terminal {
		return nil, false, fmt.Errorf("Share already contains unfinished Run %s; refusing a duplicate execution", pending.Run.ID)
	}
	if status == "succeeded" {
		return nil, true, nil
	}
	return nil, false, sharedRunError{status: status, runID: pending.Run.ID}
}

func validateSharedRequest(share shareclient.Share, request *execution.RequestRecord) error {
	if request == nil || request.APIVersion != execution.APIVersion || request.Kind != "Request" || request.ID == "" || request.IdempotencyKey == "" {
		return errors.New("Cloud returned an incomplete Request")
	}
	if request.ShareID != share.ID || request.Capability != share.CapabilityRevision {
		return errors.New("Cloud returned a Request for another Share or Capability revision")
	}
	return nil
}

func validatePendingRun(share shareclient.Share, request *execution.RequestRecord, run *execution.RunRecord, events []execution.RunEvent) error {
	if run.APIVersion != execution.APIVersion || run.Kind != "Run" || run.ID == "" || run.RequestID != request.ID || run.Capability != share.CapabilityRevision || run.Executor != share.SharedBy || !definitionReferencePointersEqual(run.Recipe, share.Recipe) {
		return errors.New("Cloud returned a Run that does not match the Share")
	}
	for index, event := range events {
		if event.APIVersion != execution.APIVersion || event.Kind != "RunEvent" || event.RunID != run.ID || event.Sequence != index {
			return errors.New("Cloud returned an invalid RunEvent sequence")
		}
	}
	return nil
}

func terminalRunStatus(events []execution.RunEvent) (string, bool) {
	if len(events) == 0 {
		return "running", false
	}
	switch events[len(events)-1].Type {
	case "run_succeeded":
		return "succeeded", true
	case "run_failed":
		return "failed", true
	case "run_cancelled":
		return "cancelled", true
	case "run_interrupted":
		return "interrupted", true
	default:
		return "running", false
	}
}

func sharesEqual(left, right shareclient.Share) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func definitionReferencePointersEqual(left, right *execution.DefinitionReference) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func uploadSharedArtifacts(ctx context.Context, progressWriter io.Writer, client *shareclient.Client, shareID, runnerToken, runID string, groups ...map[string]any) error {
	artifacts := map[string]execution.ArtifactReference{}
	for _, values := range groups {
		for name, value := range values {
			artifact, ok := value.(execution.ArtifactReference)
			if !ok {
				if pointer, pointerOK := value.(*execution.ArtifactReference); pointerOK && pointer != nil {
					artifact, ok = *pointer, true
				}
			}
			if !ok {
				continue
			}
			if artifact.ID == "" || artifact.LocalPath == "" {
				return fmt.Errorf("artifact %q has no stable id or local snapshot", name)
			}
			if existing, exists := artifacts[artifact.ID]; exists && (!sameArtifactMetadata(existing, artifact) || existing.LocalPath != artifact.LocalPath) {
				return fmt.Errorf("artifact %s is referenced with conflicting local snapshots or metadata", artifact.ID)
			}
			artifacts[artifact.ID] = artifact
		}
	}
	ids := make([]string, 0, len(artifacts))
	for id := range artifacts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		artifact := artifacts[id]
		writeShareUploadProgress(progressWriter, artifact.Filename, artifact.SizeBytes)
		uploaded, err := client.UploadArtifact(ctx, shareID, runnerToken, shareclient.ArtifactUpload{
			RunID: runID, ArtifactID: artifact.ID, Path: artifact.LocalPath,
			Filename: artifact.Filename, MediaType: artifact.MediaType,
		})
		if err != nil {
			return err
		}
		if !sameArtifactMetadata(artifact, *uploaded) {
			return fmt.Errorf("Cloud artifact %s does not match the local Run return", artifact.ID)
		}
	}
	return nil
}

func sameArtifactMetadata(left, right execution.ArtifactReference) bool {
	return left.ID == right.ID && left.Filename == right.Filename && left.MediaType == right.MediaType && left.SizeBytes == right.SizeBytes && left.SHA256 == right.SHA256
}

func environmentValue(environment []string, wanted string) string {
	var result string
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == wanted {
			result = value
		}
	}
	return result
}

func parseSharePrefillInputs(capability *manifest.Capability, supplied map[string]string) (map[string]any, error) {
	if len(supplied) == 0 {
		return nil, nil
	}
	if capability == nil {
		return nil, errors.New("Capability is required to validate --input")
	}
	partial := &manifest.Capability{Inputs: make(map[string]manifest.InputContract, len(supplied))}
	for name := range supplied {
		contract, ok := capability.Inputs[name]
		if !ok {
			return nil, fmt.Errorf("input %q is not declared by the Capability", name)
		}
		entry := contract
		entry.Required = true
		entry.Default = nil
		partial.Inputs[name] = entry
	}
	return execution.ParseInputs(partial, supplied)
}

func shareCommandExitCode(ctx context.Context, err error) int {
	if errors.Is(err, context.Canceled) || (ctx != nil && errors.Is(ctx.Err(), context.Canceled)) {
		return ExitInterrupted
	}
	return ExitOperational
}
