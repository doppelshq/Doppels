package execution

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"doppels.so/cli/internal/manifest"
)

func (r *runner) runManual(ctx context.Context) (Result, error) {
	procedurePath := ""
	if recipe := r.invocation.Recipe; recipe != nil {
		if recipe.Runtime != "manual" {
			return r.fail(fmt.Errorf("%w: unsupported runtime %q", ErrInvalidInvocation, recipe.Runtime))
		}
		if recipe.Procedure == nil || recipe.Procedure.Readme == "" {
			return r.fail(fmt.Errorf("%w: manual Recipe has no procedure", ErrInvalidInvocation))
		}
		base := r.invocation.RecipeDirectory
		if base == "" {
			base = r.invocation.ProjectRoot
		}
		path, err := securePath(r.invocation.ProjectRoot, filepath.Join(base, filepath.FromSlash(recipe.Procedure.Readme)), false)
		if err != nil {
			return r.fail(fmt.Errorf("manual procedure: %w", err))
		}
		procedurePath = path
	}
	if r.options.Manual == nil {
		r.result.Status = "pending_manual"
		if err := r.indexRun("pending_manual", false); err != nil {
			return r.result, err
		}
		return r.result, ErrManualRequired
	}
	manualResult, err := r.options.Manual(ctx, ManualRequest{
		RunID: r.result.Run.ID, ProjectRoot: r.invocation.ProjectRoot,
		Capability: r.invocation.Capability, Recipe: r.invocation.Recipe,
		Inputs: cloneMap(r.inputs), ProcedurePath: procedurePath,
	})
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return r.interrupt(err)
		}
		return r.fail(err)
	}
	returns, err := r.materializeManualReturns(manualResult.Returns)
	if err != nil {
		return r.fail(err)
	}
	evidence, err := r.materializeManualEvidence(manualResult.Evidence)
	if err != nil {
		return r.fail(err)
	}
	r.result.Returns, r.result.Evidence = returns, evidence
	if r.options.BeforeSuccess != nil {
		if err := r.options.BeforeSuccess(ctx, r.result.Run, returns, evidence); err != nil {
			return r.fail(fmt.Errorf("publish successful returns: %w", err))
		}
	}
	r.result.Status = "succeeded"
	data := map[string]any{"returns": publicReturns(returns)}
	if len(evidence) > 0 {
		data["evidence"] = publicReturns(evidence)
	}
	if err := r.emit("run_succeeded", "", data); err != nil {
		return r.sealAfterError(err)
	}
	return r.result, nil
}

func (r *runner) materializeManualReturns(supplied map[string]any) (map[string]any, error) {
	for name := range supplied {
		if _, exists := r.invocation.Capability.Outputs[name]; !exists {
			return nil, fmt.Errorf("manual fulfillment returned undeclared output %q", name)
		}
	}
	result := make(map[string]any, len(r.invocation.Capability.Outputs))
	for name, contract := range r.invocation.Capability.Outputs {
		value, exists := supplied[name]
		if !exists {
			return nil, fmt.Errorf("manual fulfillment is missing output %q", name)
		}
		converted, err := r.materializeManualValue(value, contract)
		if err != nil {
			return nil, fmt.Errorf("manual output %q: %w", name, err)
		}
		result[name] = converted
	}
	return result, nil
}

func (r *runner) materializeManualEvidence(supplied map[string]any) (map[string]any, error) {
	if r.invocation.Recipe == nil {
		result := make(map[string]any, len(supplied))
		for name, value := range supplied {
			switch typed := value.(type) {
			case FileValue:
				artifact, err := r.snapshotManualFile(typed, typed.MediaType)
				if err != nil {
					return nil, fmt.Errorf("manual evidence %q: %w", name, err)
				}
				result[name] = artifact
			case string, bool, int, int64, float64:
				result[name] = typed
			default:
				return nil, fmt.Errorf("manual evidence %q has unsupported type", name)
			}
		}
		return result, nil
	}
	for name := range supplied {
		if _, exists := r.invocation.Recipe.Evidence[name]; !exists {
			return nil, fmt.Errorf("manual fulfillment returned undeclared evidence %q", name)
		}
	}
	result := make(map[string]any, len(r.invocation.Recipe.Evidence))
	for name, contract := range r.invocation.Recipe.Evidence {
		value, exists := supplied[name]
		if !exists {
			return nil, fmt.Errorf("manual fulfillment is missing evidence %q", name)
		}
		output := manifest.OutputContract{Type: contract.Type}
		converted, err := r.materializeManualValue(value, output)
		if err != nil {
			return nil, fmt.Errorf("manual evidence %q: %w", name, err)
		}
		result[name] = converted
	}
	return result, nil
}

func (r *runner) materializeManualValue(value any, contract manifest.OutputContract) (any, error) {
	if contract.Type != "artifact" {
		return convertOutput(value, contract)
	}
	file, ok := value.(FileValue)
	if !ok {
		if path, stringValue := value.(string); stringValue {
			file = FileValue{Path: path}
		} else {
			return nil, fmt.Errorf("must be a file path")
		}
	}
	return r.snapshotManualFile(file, contract.MediaType)
}

func (r *runner) snapshotManualFile(file FileValue, declaredMediaType string) (ArtifactReference, error) {
	path, err := securePath(r.invocation.ProjectRoot, filepath.Join(r.invocation.ProjectRoot, filepath.FromSlash(file.Path)), false)
	if err != nil {
		return ArtifactReference{}, err
	}
	mediaType := declaredMediaType
	if mediaType == "" {
		mediaType = file.MediaType
	}
	artifact, err := snapshotArtifact(r.store, path, mediaType)
	if err != nil {
		return ArtifactReference{}, err
	}
	r.result.Artifacts[artifact.ID] = artifact
	return artifact, nil
}
