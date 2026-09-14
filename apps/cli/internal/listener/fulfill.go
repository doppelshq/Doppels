package listener

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/registryclient"
	"doppels.so/cli/internal/runsync"
	"doppels.so/cli/internal/shareclient"
)

// SpaceRegistry is everything fulfillment needs from the control plane for
// Space-origin jobs; *registryclient.Client satisfies it.
type SpaceRegistry interface {
	DecideRequest(ctx context.Context, token, organization, space, requestID, decision string) (execution.RequestRecord, error)
	IngestRun(ctx context.Context, token, organization, space string, body registryclient.IngestPayload) error
}

// FindCapability locates exactly one Capability revision in a merged catalog.
func FindCapability(catalog *manifest.Catalog, name, version string) (manifest.CapabilityDefinition, error) {
	matches := catalog.Capabilities[name]
	if version != "" {
		matches = filterCapabilities(matches, version)
	}
	if len(matches) == 0 {
		return manifest.CapabilityDefinition{}, fmt.Errorf("Capability %s not found", name)
	}
	if len(matches) > 1 {
		return manifest.CapabilityDefinition{}, fmt.Errorf("Capability %s has multiple revisions; include @version", name)
	}
	return matches[0], nil
}

func filterCapabilities(definitions []manifest.CapabilityDefinition, version string) []manifest.CapabilityDefinition {
	var matches []manifest.CapabilityDefinition
	for _, definition := range definitions {
		if definition.Value.Metadata.Version == version {
			matches = append(matches, definition)
		}
	}
	return matches
}

// ResolveCapabilityArgument resolves a "capability/<name>[@version]" string.
func ResolveCapabilityArgument(catalog *manifest.Catalog, resource string) (manifest.CapabilityDefinition, error) {
	kind, reference, ok := strings.Cut(resource, "/")
	if !ok || reference == "" || (kind != "capability" && kind != "capabilities") {
		return manifest.CapabilityDefinition{}, errors.New("resource must use capability/<name>[@version]")
	}
	name, version, _ := strings.Cut(reference, "@")
	return FindCapability(catalog, name, version)
}

// ResolveSpaceFulfillment resolves the local Capability (and Recipe, when
// one exists) that fulfills a Space Request. Multiple Recipes route through
// Approvals.PickRecipe unless approveAll short-circuits.
func ResolveSpaceFulfillment(catalog *manifest.Catalog, request execution.RequestRecord, approvals ApprovalPort, approveAll bool) (manifest.CapabilityDefinition, *manifest.RecipeDefinition, error) {
	return ResolveSpaceFulfillmentContext(context.Background(), catalog, request, approvals, approveAll)
}

// ResolveSpaceFulfillmentContext preserves cancellation while a headless
// consumer asks an ApprovalPort to choose among Recipes.
func ResolveSpaceFulfillmentContext(ctx context.Context, catalog *manifest.Catalog, request execution.RequestRecord, approvals ApprovalPort, approveAll bool) (manifest.CapabilityDefinition, *manifest.RecipeDefinition, error) {
	capabilityDefinition, err := FindCapability(catalog, request.Capability.Name, request.Capability.Version)
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
		picked, pickErr := pickRecipe(ctx, catalog, request.Capability.Name, approvals, approveAll)
		if pickErr != nil {
			return manifest.CapabilityDefinition{}, nil, pickErr
		}
		return capabilityDefinition, &picked, nil
	default:
		return manifest.CapabilityDefinition{}, nil, selectionErr
	}
}

// ResolveShareFulfillment resolves the local definitions a Share demands,
// refusing digest drift between the Share contract and the local catalog.
func ResolveShareFulfillment(catalog *manifest.Catalog, share shareclient.Share, approvals ApprovalPort, approveAll bool) (manifest.CapabilityDefinition, *manifest.RecipeDefinition, error) {
	return ResolveShareFulfillmentContext(context.Background(), catalog, share, approvals, approveAll)
}

// ResolveShareFulfillmentContext is the cancellable variant used by active
// listeners and future Runner consumers.
func ResolveShareFulfillmentContext(ctx context.Context, catalog *manifest.Catalog, share shareclient.Share, approvals ApprovalPort, approveAll bool) (manifest.CapabilityDefinition, *manifest.RecipeDefinition, error) {
	var empty manifest.CapabilityDefinition
	capabilityDefinition, err := ResolveCapabilityArgument(catalog, "capability/"+share.CapabilityRevision.Name+"@"+share.CapabilityRevision.Version)
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
			picked, pickErr := pickRecipe(ctx, catalog, share.CapabilityRevision.Name, approvals, approveAll)
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

func pickRecipe(ctx context.Context, catalog *manifest.Catalog, capability string, approvals ApprovalPort, approveAll bool) (manifest.RecipeDefinition, error) {
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
	if approvals == nil {
		return manifest.RecipeDefinition{}, fmt.Errorf("%w for %s; select one explicitly", manifest.ErrRecipeAmbiguous, capability)
	}
	return approvals.PickRecipe(ctx, capability, matches)
}

// ApproveError reports a failed operator approve against the control plane
// before any local execution started.
type ApproveError struct {
	Err error
}

func (e *ApproveError) Error() string { return e.Err.Error() }

func (e *ApproveError) Unwrap() error { return e.Err }

// SpaceFulfillConfig parameterizes FulfillSpace. Presentation sinks (Output,
// Err, OnEvent) stay with the consumer; orchestration stays here.
type SpaceFulfillConfig struct {
	Registry      SpaceRegistry
	Token         string
	Identity      string // remote identity id; empty falls back to LocalIdentity
	LocalIdentity execution.ActorReference
	NodeID        string
	Root          string
	Catalog       *manifest.Catalog
	Approvals     ApprovalPort
	ApproveAll    bool
	// Preapproved tells FulfillSpace that the consumer already posted the
	// remote operator approval against the control plane. Setting this keeps
	// the pre-resolve/validate precedence of the original `node up` flow:
	// a network error reaching the registry prints "Unable to approve" before
	// any local resolution runs.
	Preapproved bool
	Output      io.Writer // runtime stdout sink (the CLI applies line prefixes)
	Err         io.Writer // warnings (outbox flush)
	Environment []string
	Now         func() time.Time
	OnEvent     execution.EventFunc // presentation timeline hook; nil ok
}

// FulfillSpace approves (when needed), executes, and syncs a Space-origin
// Request. It returns the execution Result; rendering and exit codes belong
// to the consumer.
func FulfillSpace(ctx context.Context, cfg SpaceFulfillConfig, job Job, capabilityDefinition manifest.CapabilityDefinition, recipeDefinition *manifest.RecipeDefinition) (execution.Result, error) {
	executor := execution.ActorReference{Kind: "identity", ID: cfg.Identity}
	if cfg.Identity == "" {
		executor = cfg.LocalIdentity
	}
	capabilityReference := execution.ReferenceCapability(capabilityDefinition)
	invocation := execution.Invocation{
		ProjectRoot:     cfg.Root,
		Capability:      capabilityDefinition.Value,
		CapabilityRef:   capabilityReference,
		Executor:        executor,
		NodeID:          cfg.NodeID,
		Space:           job.Space,
		ExistingRequest: job.Request,
	}
	if recipeDefinition != nil {
		reference := execution.ReferenceRecipe(*recipeDefinition)
		invocation.Recipe = recipeDefinition.Value
		invocation.RecipeRef = &reference
		invocation.RecipeDirectory = recipeDefinition.Source.Directory
	}
	options := execution.Options{
		ApproveAll:  cfg.ApproveAll,
		Stdout:      cfg.Output,
		Stderr:      cfg.Err,
		Environment: cfg.Environment,
		Now:         cfg.Now,
		OnEvent:     cfg.OnEvent,
	}
	if cfg.Approvals != nil {
		options.Approve = cfg.Approvals.ApproveStep
		options.Manual = cfg.Approvals.FulfillManual
	}
	if !cfg.Preapproved && strings.ToLower(strings.TrimSpace(job.Request.OperatorDecision)) != "approve" && cfg.Registry != nil {
		if _, err := cfg.Registry.DecideRequest(ctx, cfg.Token, job.Organization, job.Space, job.Request.ID, "approve"); err != nil {
			return execution.Result{}, &ApproveError{Err: err}
		}
	}
	result, runErr := execution.Execute(ctx, invocation, options)
	if cfg.Registry != nil && job.Organization != "" && job.Space != "" {
		if _, flushErr := runsync.Flush(ctx, cfg.Root, cfg.Registry, cfg.Token, job.Organization, job.Space); flushErr != nil {
			fmt.Fprintf(cfg.Err, "warning: run outbox flush: %v\n", flushErr)
		}
	}
	return result, runErr
}

// ShareFulfillConfig parameterizes FulfillShare.
type ShareFulfillConfig struct {
	Root          string
	NodeID        string
	Approvals     ApprovalPort
	ApproveAll    bool
	BeforeSuccess execution.BeforeSuccessFunc // artifact upload; nil skips
	Output        io.Writer
	Err           io.Writer
	Environment   []string
	Now           func() time.Time
	OnEvent       execution.EventFunc // presentation timeline hook; nil ok
}

// FulfillShare executes a Share-origin Request on the local Node, streaming
// the Run, its events, and return artifacts into the Share channel the job
// carries.
func FulfillShare(ctx context.Context, cfg ShareFulfillConfig, job Job, capabilityDefinition manifest.CapabilityDefinition, recipeDefinition *manifest.RecipeDefinition) (execution.Result, error) {
	capabilityReference := execution.ReferenceCapability(capabilityDefinition)
	invocation := execution.Invocation{
		ProjectRoot:     cfg.Root,
		Capability:      capabilityDefinition.Value,
		CapabilityRef:   capabilityReference,
		Executor:        job.Created.Share.SharedBy,
		NodeID:          cfg.NodeID,
		ShareID:         job.Created.Share.ID,
		ExistingRequest: job.Request,
	}
	if recipeDefinition != nil {
		reference := execution.ReferenceRecipe(*recipeDefinition)
		invocation.Recipe = recipeDefinition.Value
		invocation.RecipeRef = &reference
		invocation.RecipeDirectory = recipeDefinition.Source.Directory
	}
	options := execution.Options{
		OnRun: func(callbackContext context.Context, run execution.RunRecord) error {
			if job.Channel == nil {
				return nil
			}
			_, err := job.Channel.SubmitRun(callbackContext, run)
			return err
		},
		BeforeSuccess: cfg.BeforeSuccess,
		Stdout:        cfg.Output,
		Stderr:        cfg.Err,
		Environment:   cfg.Environment,
		Now:           cfg.Now,
		OnEvent: func(callbackContext context.Context, event execution.RunEvent) error {
			if cfg.OnEvent != nil {
				if err := cfg.OnEvent(callbackContext, event); err != nil {
					return err
				}
			}
			if job.Channel == nil {
				return nil
			}
			_, err := job.Channel.SubmitRunEvent(callbackContext, event)
			return err
		},
	}
	if cfg.Approvals != nil {
		options.Approve = cfg.Approvals.ApproveStep
		options.Manual = cfg.Approvals.FulfillManual
	}
	options.ApproveAll = cfg.ApproveAll
	return execution.Execute(ctx, invocation, options)
}

// RejectShare records an operator rejection on the Share channel: a Run with
// a terminal run_cancelled event and no execution.
func RejectShare(ctx context.Context, channel *shareclient.Channel, created *shareclient.ShareCreated, request *execution.RequestRecord, nodeID string, now func() time.Time) error {
	runID, err := uuid()
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

func uuid() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	hex := fmt.Sprintf("%x", value)
	return strings.Join([]string{hex[0:8], hex[8:12], hex[12:16], hex[16:20], hex[20:32]}, "-"), nil
}
