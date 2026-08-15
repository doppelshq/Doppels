package manifest

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

func structuralDiagnostics(loaded Loaded) []Diagnostic {
	var diagnostics []Diagnostic
	typeMeta := loaded.Document.Type()
	if typeMeta.APIVersion != APIVersion {
		diagnostics = append(diagnostics, diag(loaded.Path, "apiVersion", "manifest.api-version", "must be %q", APIVersion))
	}
	switch document := loaded.Document.(type) {
	case *Capability:
		diagnostics = append(diagnostics, metadataDiagnostics(loaded.Path, document.Metadata)...)
		diagnostics = append(diagnostics, capabilityDiagnostics(loaded.Path, document)...)
	case *Recipe:
		diagnostics = append(diagnostics, metadataDiagnostics(loaded.Path, document.Metadata)...)
		diagnostics = append(diagnostics, recipeStructuralDiagnostics(loaded.Path, document)...)
	case *Space:
		diagnostics = append(diagnostics, spaceMetadataDiagnostics(loaded.Path, document.Metadata)...)
	}
	return diagnostics
}

func spaceMetadataDiagnostics(source string, metadata SpaceMetadata) []Diagnostic {
	base := Metadata{
		Name: metadata.Name, Version: "0.0.0", DisplayName: metadata.DisplayName,
		Summary: metadata.Summary, Description: metadata.Description,
		Labels: metadata.Labels, Annotations: metadata.Annotations,
	}
	return metadataDiagnostics(source, base)
}

func metadataDiagnostics(source string, metadata Metadata) []Diagnostic {
	var diagnostics []Diagnostic
	if !validIdentifier(metadata.Name) {
		diagnostics = append(diagnostics, diag(source, "metadata.name", "metadata.name", "must be a lowercase identifier of at most 63 characters"))
	}
	if len(metadata.Version) > 100 {
		diagnostics = append(diagnostics, diag(source, "metadata.version", "metadata.version", "must contain at most 100 characters"))
	} else if !semverPattern.MatchString(metadata.Version) {
		diagnostics = append(diagnostics, diag(source, "metadata.version", "metadata.version", "must be a semantic version such as 1.0.0"))
	}
	for field, value := range map[string]struct {
		value string
		min   int
		max   int
	}{
		"metadata.displayName": {metadata.DisplayName, 0, 120},
		"metadata.summary":     {metadata.Summary, 0, 160},
		"metadata.description": {metadata.Description, 0, 500},
	} {
		length := utf8.RuneCountInString(value.value)
		if length > value.max || (value.value != "" && length < value.min) {
			diagnostics = append(diagnostics, diag(source, field, "metadata.length", "must contain at most %d characters", value.max))
		}
	}
	if metadata.Impact != "" && metadata.Impact != "low" && metadata.Impact != "medium" && metadata.Impact != "high" && metadata.Impact != "critical" {
		diagnostics = append(diagnostics, diag(source, "metadata.impact", "metadata.impact", "must be low, medium, high, or critical"))
	}
	if !uniqueStrings(metadata.Tags) {
		diagnostics = append(diagnostics, diag(source, "metadata.tags", "metadata.tags", "must not contain duplicates"))
	}
	for index, tag := range metadata.Tags {
		if tag == "" || utf8.RuneCountInString(tag) > 80 {
			diagnostics = append(diagnostics, diag(source, fmt.Sprintf("metadata.tags[%d]", index), "metadata.tag", "must contain between 1 and 80 characters"))
		}
	}
	for name, value := range metadata.Labels {
		if !labelPattern.MatchString(name) {
			diagnostics = append(diagnostics, diag(source, "metadata.labels."+name, "metadata.label-name", "has an invalid label name"))
		}
		if utf8.RuneCountInString(value) > 120 {
			diagnostics = append(diagnostics, diag(source, "metadata.labels."+name, "metadata.label-value", "must contain at most 120 characters"))
		}
	}
	for name := range metadata.Annotations {
		if !annotationPattern.MatchString(name) {
			diagnostics = append(diagnostics, diag(source, "metadata.annotations."+name, "metadata.annotation-name", "has an invalid annotation name"))
		}
	}
	return diagnostics
}

func capabilityDiagnostics(source string, capability *Capability) []Diagnostic {
	var diagnostics []Diagnostic
	if capability.Documentation != nil && !validRelativePath(capability.Documentation.Readme) {
		diagnostics = append(diagnostics, diag(source, "documentation.readme", "path.relative", "must be a non-empty relative path without parent traversal"))
	}
	if capability.Inputs == nil {
		diagnostics = append(diagnostics, diag(source, "inputs", "capability.inputs", "is required (use {} when the Capability has no inputs)"))
	}
	for name, input := range capability.Inputs {
		field := "inputs." + name
		if !validIdentifier(name) {
			diagnostics = append(diagnostics, diag(source, field, "contract.input-name", "must be a valid identifier"))
		}
		if input.Type != "string" && input.Type != "integer" && input.Type != "number" && input.Type != "boolean" {
			diagnostics = append(diagnostics, diag(source, field+".type", "contract.input-type", "must be string, integer, number, or boolean"))
			continue
		}
		if utf8.RuneCountInString(input.Description) > 500 {
			diagnostics = append(diagnostics, diag(source, field+".description", "contract.description", "must contain at most 500 characters"))
		}
		seen := make([]any, 0, len(input.Enum))
		if input.Enum != nil && len(input.Enum) == 0 {
			diagnostics = append(diagnostics, diag(source, field+".enum", "contract.enum-empty", "must contain at least one value when present"))
		}
		for index, value := range input.Enum {
			if !scalarMatches(value, input.Type) {
				diagnostics = append(diagnostics, diag(source, fmt.Sprintf("%s.enum[%d]", field, index), "contract.enum-type", "does not match input type %s", input.Type))
			}
			if containsScalar(seen, value) {
				diagnostics = append(diagnostics, diag(source, field+".enum", "contract.enum-unique", "must not contain duplicate values"))
			}
			seen = append(seen, value)
		}
		if input.DefaultSet {
			if !scalarMatches(input.Default, input.Type) {
				diagnostics = append(diagnostics, diag(source, field+".default", "contract.default-type", "does not match input type %s", input.Type))
			} else if len(input.Enum) > 0 && !containsScalar(input.Enum, input.Default) {
				diagnostics = append(diagnostics, diag(source, field+".default", "contract.default-enum", "is not one of the declared enum values"))
			}
		}
	}
	if len(capability.Outputs) == 0 {
		diagnostics = append(diagnostics, diag(source, "outputs", "capability.outputs", "must declare at least one output"))
	}
	for name, output := range capability.Outputs {
		field := "outputs." + name
		if !validIdentifier(name) {
			diagnostics = append(diagnostics, diag(source, field, "contract.output-name", "must be a valid identifier"))
		}
		if output.Type != "string" && output.Type != "integer" && output.Type != "number" && output.Type != "boolean" && output.Type != "artifact" {
			diagnostics = append(diagnostics, diag(source, field+".type", "contract.output-type", "must be string, integer, number, boolean, or artifact"))
		}
		if output.MediaType != "" && output.Type != "artifact" {
			diagnostics = append(diagnostics, diag(source, field+".mediaType", "contract.media-type", "is only valid for artifact outputs"))
		}
		if utf8.RuneCountInString(output.Description) > 500 {
			diagnostics = append(diagnostics, diag(source, field+".description", "contract.description", "must contain at most 500 characters"))
		}
	}
	return diagnostics
}

func recipeStructuralDiagnostics(source string, recipe *Recipe) []Diagnostic {
	var diagnostics []Diagnostic
	if len(recipe.Provides) == 0 {
		diagnostics = append(diagnostics, diag(source, "provides", "recipe.provides", "must contain at least one Capability name"))
	}
	if !uniqueStrings(recipe.Provides) {
		diagnostics = append(diagnostics, diag(source, "provides", "recipe.provides", "must not contain duplicates"))
	}
	for index, provided := range recipe.Provides {
		if !validIdentifier(provided) {
			diagnostics = append(diagnostics, diag(source, fmt.Sprintf("provides[%d]", index), "recipe.provides-name", "must be a valid Capability identifier"))
		}
	}
	if recipe.Runtime != "shell" && recipe.Runtime != "manual" {
		diagnostics = append(diagnostics, diag(source, "runtime", "recipe.runtime", "must be shell or manual"))
		return diagnostics
	}
	if recipe.Runtime == "manual" {
		if recipe.Procedure == nil || !validRelativePath(recipe.Procedure.Readme) {
			diagnostics = append(diagnostics, diag(source, "procedure.readme", "recipe.manual-procedure", "manual Recipes require a relative procedure.readme path"))
		}
		if len(recipe.Evidence) == 0 {
			diagnostics = append(diagnostics, diag(source, "evidence", "recipe.manual-evidence", "manual Recipes require at least one evidence field"))
		}
		for name, evidence := range recipe.Evidence {
			field := "evidence." + name
			if !validIdentifier(name) {
				diagnostics = append(diagnostics, diag(source, field, "recipe.evidence-name", "must be a valid identifier"))
			}
			if evidence.Type != "string" && evidence.Type != "artifact" {
				diagnostics = append(diagnostics, diag(source, field+".type", "recipe.evidence-type", "must be string or artifact"))
			}
			if utf8.RuneCountInString(evidence.Description) > 500 {
				diagnostics = append(diagnostics, diag(source, field+".description", "contract.description", "must contain at most 500 characters"))
			}
		}
		if recipe.Requires != nil || recipe.Env != nil || recipe.Steps != nil || recipe.Returns != nil {
			diagnostics = append(diagnostics, diag(source, "runtime", "recipe.manual-fields", "manual Recipes cannot declare requires, env, steps, or returns"))
		}
		diagnostics = append(diagnostics, defaultsStructuralDiagnostics(source, recipe.Defaults)...)
		return diagnostics
	}

	if recipe.Procedure != nil || recipe.Evidence != nil {
		diagnostics = append(diagnostics, diag(source, "runtime", "recipe.shell-fields", "shell Recipes cannot declare procedure or evidence"))
	}
	diagnostics = append(diagnostics, requirementsStructuralDiagnostics(source, recipe.Requires)...)
	diagnostics = append(diagnostics, environmentStructuralDiagnostics(source, "env", recipe.Env)...)
	diagnostics = append(diagnostics, defaultsStructuralDiagnostics(source, recipe.Defaults)...)
	if len(recipe.Steps) == 0 {
		diagnostics = append(diagnostics, diag(source, "steps", "recipe.steps", "shell Recipes require at least one Step"))
	}
	for index, step := range recipe.Steps {
		base := fmt.Sprintf("steps[%d]", index)
		if !validIdentifier(step.ID) {
			diagnostics = append(diagnostics, diag(source, base+".id", "step.id", "must be a valid identifier"))
		}
		if strings.TrimSpace(step.Name) == "" || utf8.RuneCountInString(step.Name) > 120 {
			diagnostics = append(diagnostics, diag(source, base+".name", "step.name", "must contain between 1 and 120 characters"))
		}
		if step.Approval != "" && step.Approval != "never" && step.Approval != "required" {
			diagnostics = append(diagnostics, diag(source, base+".approval", "approval.invalid", "must be never or required"))
		}
		if step.WorkingDirectory != "" && !validRelativePath(step.WorkingDirectory) {
			diagnostics = append(diagnostics, diag(source, base+".workingDirectory", "path.relative", "must be a relative path without parent traversal"))
		}
		diagnostics = append(diagnostics, environmentStructuralDiagnostics(source, base+".env", step.Env)...)
		if step.Run == nil {
			diagnostics = append(diagnostics, diag(source, base+".run", "step.run", "is required"))
		} else {
			if step.Run.Shell != "sh" && step.Run.Shell != "bash" {
				diagnostics = append(diagnostics, diag(source, base+".run.shell", "step.shell", "must be sh or bash"))
			}
			if strings.TrimSpace(step.Run.Script) == "" {
				diagnostics = append(diagnostics, diag(source, base+".run.script", "step.script", "must not be empty"))
			}
			if step.Run.Timeout != "" && !durationPattern.MatchString(step.Run.Timeout) {
				diagnostics = append(diagnostics, diag(source, base+".run.timeout", "duration.invalid", "must be a positive duration using ms, s, m, or h"))
			}
		}
		if step.Produces != nil && len(step.Produces) == 0 {
			diagnostics = append(diagnostics, diag(source, base+".produces", "step.produces", "must contain at least one result when present"))
		}
		for name, product := range step.Produces {
			field := base + ".produces." + name
			if !validIdentifier(name) {
				diagnostics = append(diagnostics, diag(source, field, "step.product-name", "must be a valid identifier"))
			}
			if (product.File == "") == (product.Env == "") {
				diagnostics = append(diagnostics, diag(source, field, "step.product", "must declare exactly one of file or env"))
			} else if product.File != "" && !validRelativePath(product.File) {
				diagnostics = append(diagnostics, diag(source, field+".file", "path.relative", "must be a relative file path without parent traversal"))
			} else if product.Env != "" && !productEnvPattern.MatchString(product.Env) {
				diagnostics = append(diagnostics, diag(source, field+".env", "step.product-env", "must be an uppercase environment variable name"))
			}
		}
	}
	if len(recipe.Returns) == 0 {
		diagnostics = append(diagnostics, diag(source, "returns", "recipe.returns", "shell Recipes require at least one return"))
	}
	for name := range recipe.Returns {
		if !validIdentifier(name) {
			diagnostics = append(diagnostics, diag(source, "returns."+name, "recipe.return-name", "must be a valid identifier"))
		}
	}
	return diagnostics
}

func requirementsStructuralDiagnostics(source string, requirements *Requirements) []Diagnostic {
	if requirements == nil {
		return nil
	}
	var diagnostics []Diagnostic
	seenCommands := map[string]struct{}{}
	for index, command := range requirements.Commands {
		field := fmt.Sprintf("requires.commands[%d]", index)
		if !commandPattern.MatchString(command.Name) {
			diagnostics = append(diagnostics, diag(source, field+".name", "requirement.command", "must be a non-empty command name"))
		}
		if command.Version != "" && !validVersionConstraint(command.Version) {
			diagnostics = append(diagnostics, diag(source, field+".version", "requirement.version", "must contain semantic-version comparators"))
		}
		key := command.Name + "\x00" + command.Version
		if _, exists := seenCommands[key]; exists {
			diagnostics = append(diagnostics, diag(source, field, "requirement.duplicate", "duplicates a command requirement"))
		}
		seenCommands[key] = struct{}{}
	}
	if !uniqueStrings(requirements.HostEnv) {
		diagnostics = append(diagnostics, diag(source, "requires.hostEnv", "requirement.duplicate", "must not contain duplicates"))
	}
	for index, name := range requirements.HostEnv {
		if !environmentPattern.MatchString(name) {
			diagnostics = append(diagnostics, diag(source, fmt.Sprintf("requires.hostEnv[%d]", index), "requirement.host-env", "must be an environment variable name"))
		}
	}
	if !uniqueStrings(requirements.Files) {
		diagnostics = append(diagnostics, diag(source, "requires.files", "requirement.duplicate", "must not contain duplicates"))
	}
	for index, file := range requirements.Files {
		if !validRelativePath(file) {
			diagnostics = append(diagnostics, diag(source, fmt.Sprintf("requires.files[%d]", index), "path.relative", "must be a relative path without parent traversal"))
		}
	}
	return diagnostics
}

func environmentStructuralDiagnostics(source, base string, environment map[string]EnvironmentValue) []Diagnostic {
	var diagnostics []Diagnostic
	for name, value := range environment {
		field := base + "." + name
		if !environmentPattern.MatchString(name) {
			diagnostics = append(diagnostics, diag(source, field, "environment.name", "must be an environment variable name"))
		}
		if (value.Literal == nil) == (value.HostEnv == nil) {
			diagnostics = append(diagnostics, diag(source, field, "environment.value", "must be a string or host_env reference"))
			continue
		}
		if value.HostEnv != nil {
			if value.HostEnv.From != "host_env" {
				diagnostics = append(diagnostics, diag(source, field+".from", "environment.source", "must be host_env"))
			}
			if !environmentPattern.MatchString(value.HostEnv.Name) {
				diagnostics = append(diagnostics, diag(source, field+".name", "environment.host-env", "must be an environment variable name"))
			}
		}
	}
	return diagnostics
}

func defaultsStructuralDiagnostics(source string, defaults *Defaults) []Diagnostic {
	if defaults == nil {
		return nil
	}
	var diagnostics []Diagnostic
	if defaults.Timeout != "" && !durationPattern.MatchString(defaults.Timeout) {
		diagnostics = append(diagnostics, diag(source, "defaults.timeout", "duration.invalid", "must be a positive duration using ms, s, m, or h"))
	}
	if defaults.Approval != "" && defaults.Approval != "never" && defaults.Approval != "required" {
		diagnostics = append(diagnostics, diag(source, "defaults.approval", "approval.invalid", "must be never or required"))
	}
	if defaults.WorkingDirectory != "" && !validRelativePath(defaults.WorkingDirectory) {
		diagnostics = append(diagnostics, diag(source, "defaults.workingDirectory", "path.relative", "must be a relative path without parent traversal"))
	}
	if defaults.ArtifactRetentionDays != nil {
		switch *defaults.ArtifactRetentionDays {
		case 1, 7, 14, 30:
		default:
			diagnostics = append(diagnostics, diag(source, "defaults.artifactRetentionDays", "retention.invalid", "must be 1, 7, 14, or 30"))
		}
	}
	return diagnostics
}
