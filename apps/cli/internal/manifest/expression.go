package manifest

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	inputExpressionPattern   = regexp.MustCompile(`^inputs\.(` + `[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*` + `)$`)
	stepExpressionPattern    = regexp.MustCompile(`^steps\.(` + `[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*` + `)\.(` + `[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*` + `)$`)
	exactStepTemplatePattern = regexp.MustCompile(`^\{\{\s*steps\.(` + `[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*` + `)\.(` + `[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*` + `)\s*\}\}$`)
)

type expressionContext struct {
	inputs  map[string]struct{}
	results map[string]map[string]Product
}

func templateDiagnostics(source, field, value string, context expressionContext) []Diagnostic {
	var diagnostics []Diagnostic
	if strings.Contains(value, "${{") {
		diagnostics = append(diagnostics, diag(source, field, "expression.obsolete", "uses obsolete ${{ ... }} syntax; use {{ ... }}"))
	}

	remainder := value
	for {
		start := strings.Index(remainder, "{{")
		if start < 0 {
			if strings.Contains(remainder, "}}") {
				diagnostics = append(diagnostics, diag(source, field, "expression.incomplete", "contains an unmatched }}"))
			}
			break
		}
		if strings.Contains(remainder[:start], "}}") {
			diagnostics = append(diagnostics, diag(source, field, "expression.incomplete", "contains an unmatched }}"))
		}
		after := remainder[start+2:]
		end := strings.Index(after, "}}")
		if end < 0 {
			diagnostics = append(diagnostics, diag(source, field, "expression.incomplete", "contains an unmatched {{"))
			break
		}
		expression := strings.TrimSpace(after[:end])
		diagnostics = append(diagnostics, validateExpression(source, field, expression, context)...)
		remainder = after[end+2:]
	}
	return diagnostics
}

func validateExpression(source, field, expression string, context expressionContext) []Diagnostic {
	if match := inputExpressionPattern.FindStringSubmatch(expression); match != nil {
		if _, exists := context.inputs[match[1]]; !exists {
			return []Diagnostic{diag(source, field, "expression.unknown-input", "references input %q not available with the same contract in every provided Capability; changing Capability inputs without updating Recipes that provide them is a conscious breaking change", match[1])}
		}
		return nil
	}
	if match := stepExpressionPattern.FindStringSubmatch(expression); match != nil {
		products, stepExists := context.results[match[1]]
		if !stepExists {
			return []Diagnostic{diag(source, field, "expression.unavailable-step", "references Step %q before it is available", match[1])}
		}
		if _, resultExists := products[match[2]]; !resultExists {
			return []Diagnostic{diag(source, field, "expression.unknown-result", "references result %q not produced by Step %q", match[2], match[1])}
		}
		return nil
	}
	return []Diagnostic{diag(source, field, "expression.unsupported", "contains unsupported expression {{ %s }}", expression)}
}

func environmentSemanticDiagnostics(source, base string, environment map[string]EnvironmentValue, declaredHostEnv map[string]struct{}, context expressionContext) []Diagnostic {
	var diagnostics []Diagnostic
	for name, value := range environment {
		field := base + "." + name
		if value.Literal != nil {
			diagnostics = append(diagnostics, templateDiagnostics(source, field, *value.Literal, context)...)
		}
		if value.HostEnv != nil {
			if _, declared := declaredHostEnv[value.HostEnv.Name]; !declared {
				diagnostics = append(diagnostics, diag(source, field, "environment.undeclared-host-env", "reads host variable %q without declaring it in requires.hostEnv", value.HostEnv.Name))
			}
		}
	}
	return diagnostics
}

func scriptDiagnostics(source, field, script string) []Diagnostic {
	if strings.Contains(script, "{{") || strings.Contains(script, "}}") || strings.Contains(script, "${{") {
		return []Diagnostic{diag(source, field, "script.interpolation", "must receive Doppels values through Step env; scripts cannot contain {{ ... }}")}
	}
	return nil
}

func exactStepResult(expression string) (string, string, bool) {
	match := exactStepTemplatePattern.FindStringSubmatch(expression)
	if match == nil {
		return "", "", false
	}
	return match[1], match[2], true
}

func expressionLocation(stepIndex int, suffix string) string {
	return fmt.Sprintf("steps[%d].%s", stepIndex, suffix)
}
