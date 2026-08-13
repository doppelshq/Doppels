package manifest

import (
	"fmt"
	"sort"
)

func semanticDiagnostics(catalog *Catalog) []Diagnostic {
	var diagnostics []Diagnostic
	capabilityRevisions := map[string]CapabilityDefinition{}
	for name, revisions := range catalog.Capabilities {
		seen := map[string]CapabilityDefinition{}
		for _, revision := range revisions {
			version := revision.Value.Metadata.Version
			if previous, exists := seen[version]; exists {
				diagnostics = append(diagnostics, diag(revision.Source.Path, "metadata.version", "catalog.duplicate-revision", "duplicates Capability %s@%s from %s", name, version, previous.Source.Path))
			}
			seen[version] = revision
		}
		if len(revisions) == 1 {
			capabilityRevisions[name] = revisions[0]
		}
	}
	for name, revisions := range catalog.Recipes {
		seen := map[string]RecipeDefinition{}
		for _, revision := range revisions {
			version := revision.Value.Metadata.Version
			if previous, exists := seen[version]; exists {
				diagnostics = append(diagnostics, diag(revision.Source.Path, "metadata.version", "catalog.duplicate-revision", "duplicates Recipe %s@%s from %s", name, version, previous.Source.Path))
			}
			seen[version] = revision
		}
	}

	for _, revisions := range catalog.Recipes {
		for _, definition := range revisions {
			diagnostics = append(diagnostics, recipeSemanticDiagnostics(definition, catalog, capabilityRevisions)...)
		}
	}
	return diagnostics
}

func recipeSemanticDiagnostics(definition RecipeDefinition, catalog *Catalog, selected map[string]CapabilityDefinition) []Diagnostic {
	recipe := definition.Value
	source := definition.Source.Path
	var diagnostics []Diagnostic
	var capabilities []*Capability
	for index, name := range recipe.Provides {
		revisions := catalog.Capabilities[name]
		switch len(revisions) {
		case 0:
			diagnostics = append(diagnostics, diag(source, fmt.Sprintf("provides[%d]", index), "recipe.unknown-capability", "references unknown Capability %q", name))
		case 1:
			capabilities = append(capabilities, selected[name].Value)
		default:
			diagnostics = append(diagnostics, diag(source, fmt.Sprintf("provides[%d]", index), "recipe.ambiguous-capability", "Capability %q has multiple local revisions; keep exactly one active revision in the local Space for the MVP", name))
		}
	}
	if recipe.Runtime != "shell" {
		return diagnostics
	}

	commonInputs := commonCapabilityInputs(source, capabilities, &diagnostics)
	outputs := capabilityOutputs(source, capabilities, &diagnostics)
	context := expressionContext{inputs: commonInputs, results: make(map[string]map[string]Product)}
	declaredHostEnv := map[string]struct{}{}
	if recipe.Requires != nil {
		for _, name := range recipe.Requires.HostEnv {
			declaredHostEnv[name] = struct{}{}
		}
	}

	diagnostics = append(diagnostics, environmentSemanticDiagnostics(source, "env", recipe.Env, declaredHostEnv, context)...)
	if recipe.Defaults != nil && recipe.Defaults.WorkingDirectory != "" {
		diagnostics = append(diagnostics, templateDiagnostics(source, "defaults.workingDirectory", recipe.Defaults.WorkingDirectory, context)...)
	}
	if recipe.Requires != nil {
		for index, file := range recipe.Requires.Files {
			diagnostics = append(diagnostics, templateDiagnostics(source, fmt.Sprintf("requires.files[%d]", index), file, context)...)
		}
	}

	seenSteps := map[string]struct{}{}
	defaultApproval := ""
	if recipe.Defaults != nil {
		defaultApproval = recipe.Defaults.Approval
	}
	for index, step := range recipe.Steps {
		base := fmt.Sprintf("steps[%d]", index)
		if _, exists := seenSteps[step.ID]; exists {
			diagnostics = append(diagnostics, diag(source, base+".id", "step.duplicate", "duplicates Step id %q", step.ID))
			continue
		}
		seenSteps[step.ID] = struct{}{}
		if step.Approval == "" && defaultApproval == "" {
			diagnostics = append(diagnostics, diag(source, base+".approval", "approval.unresolved", "must be declared on the Step or inherited from defaults.approval"))
		}
		diagnostics = append(diagnostics, environmentSemanticDiagnostics(source, base+".env", step.Env, declaredHostEnv, context)...)
		if step.WorkingDirectory != "" {
			diagnostics = append(diagnostics, templateDiagnostics(source, base+".workingDirectory", step.WorkingDirectory, context)...)
		}
		if step.Run != nil {
			diagnostics = append(diagnostics, scriptDiagnostics(source, base+".run.script", step.Run.Script)...)
		}
		for name, product := range step.Produces {
			if product.File != "" {
				diagnostics = append(diagnostics, templateDiagnostics(source, base+".produces."+name+".file", product.File, context)...)
			}
		}
		context.results[step.ID] = step.Produces
	}

	for name, expression := range recipe.Returns {
		field := "returns." + name
		diagnostics = append(diagnostics, templateDiagnostics(source, field, expression, context)...)
		output, exists := outputs[name]
		if !exists {
			diagnostics = append(diagnostics, diag(source, field, "return.undeclared-output", "is not declared by any provided Capability"))
			continue
		}
		stepID, productName, exact := exactStepResult(expression)
		if !exact {
			diagnostics = append(diagnostics, diag(source, field, "return.reference", "must be exactly {{ steps.<step>.<result> }}"))
			continue
		}
		product, exists := context.results[stepID][productName]
		if !exists {
			continue
		}
		if output.Type == "artifact" && product.File == "" {
			diagnostics = append(diagnostics, diag(source, field, "return.artifact", "artifact output must reference a file product"))
		}
		if output.Type != "artifact" && product.Env == "" {
			diagnostics = append(diagnostics, diag(source, field, "return.scalar", "%s output must reference an env product", output.Type))
		}
	}
	for name := range outputs {
		if _, exists := recipe.Returns[name]; !exists {
			diagnostics = append(diagnostics, diag(source, "returns", "return.missing-output", "does not satisfy Capability output %q; fix the Recipe or accept that applying a changed Capability contract breaks this Recipe until it is updated", name))
		}
	}
	return diagnostics
}

func commonCapabilityInputs(source string, capabilities []*Capability, diagnostics *[]Diagnostic) map[string]struct{} {
	common := map[string]struct{}{}
	if len(capabilities) == 0 {
		return common
	}
	names := map[string]struct{}{}
	for _, capability := range capabilities {
		for name := range capability.Inputs {
			names[name] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		contracts := make([]InputContract, 0, len(capabilities))
		presentEverywhere := true
		for _, capability := range capabilities {
			contract, exists := capability.Inputs[name]
			if !exists {
				presentEverywhere = false
				break
			}
			contracts = append(contracts, contract)
		}
		if !presentEverywhere {
			continue
		}
		compatible := true
		for _, contract := range contracts[1:] {
			if inputSignature(contract) != inputSignature(contracts[0]) {
				compatible = false
				break
			}
		}
		if !compatible {
			*diagnostics = append(*diagnostics, diag(source, "provides", "contract.incompatible-input", "provided Capabilities define incompatible input %q; applying a breaking Capability revision is a conscious decision and will break Recipes that still provide the previous contract", name))
			continue
		}
		common[name] = struct{}{}
	}
	return common
}

func capabilityOutputs(source string, capabilities []*Capability, diagnostics *[]Diagnostic) map[string]OutputContract {
	outputs := map[string]OutputContract{}
	for _, capability := range capabilities {
		for name, contract := range capability.Outputs {
			if existing, exists := outputs[name]; exists && outputSignature(existing) != outputSignature(contract) {
				*diagnostics = append(*diagnostics, diag(source, "provides", "contract.incompatible-output", "provided Capabilities define incompatible output %q; applying a breaking Capability revision is a conscious decision and will break Recipes that still provide the previous contract", name))
				continue
			}
			outputs[name] = contract
		}
	}
	return outputs
}
