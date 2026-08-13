package execution

import (
	"fmt"
	"regexp"
	"strings"
)

var templatePattern = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

type values struct {
	inputs map[string]any
	steps  map[string]map[string]any
}

func renderTemplate(template string, available values) (string, error) {
	if strings.Contains(template, "${{") {
		return "", fmt.Errorf("obsolete ${{ ... }} expression")
	}
	var renderErr error
	rendered := templatePattern.ReplaceAllStringFunc(template, func(token string) string {
		if renderErr != nil {
			return ""
		}
		expression := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(token, "{{"), "}}"))
		value, err := resolveExpression(expression, available)
		if err != nil {
			renderErr = err
			return ""
		}
		switch typed := value.(type) {
		case ArtifactReference:
			return typed.LocalPath
		case string:
			return typed
		case bool:
			if typed {
				return "true"
			}
			return "false"
		default:
			return fmt.Sprint(typed)
		}
	})
	if renderErr != nil {
		return "", renderErr
	}
	if strings.Contains(rendered, "{{") || strings.Contains(rendered, "}}") {
		return "", fmt.Errorf("malformed template %q", template)
	}
	return rendered, nil
}

func resolveExact(template string, available values) (any, error) {
	match := templatePattern.FindStringSubmatch(template)
	if match == nil || strings.TrimSpace(template) != match[0] {
		return nil, fmt.Errorf("return %q must be one exact expression", template)
	}
	return resolveExpression(strings.TrimSpace(match[1]), available)
}

func resolveExpression(expression string, available values) (any, error) {
	parts := strings.Split(expression, ".")
	if len(parts) == 2 && parts[0] == "inputs" {
		value, exists := available.inputs[parts[1]]
		if !exists {
			return nil, fmt.Errorf("unknown input %q", parts[1])
		}
		return value, nil
	}
	if len(parts) == 3 && parts[0] == "steps" {
		products, exists := available.steps[parts[1]]
		if !exists {
			return nil, fmt.Errorf("step %q is not available", parts[1])
		}
		value, exists := products[parts[2]]
		if !exists {
			return nil, fmt.Errorf("step %q did not produce %q", parts[1], parts[2])
		}
		return value, nil
	}
	return nil, fmt.Errorf("unsupported expression %q", expression)
}
