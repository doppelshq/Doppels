package manifest

import (
	"fmt"
	"sort"
)

type Diagnostic struct {
	Source  string `json:"source"`
	Field   string `json:"field,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (d Diagnostic) Error() string {
	location := d.Source
	if d.Field != "" {
		location += ":" + d.Field
	}
	return fmt.Sprintf("%s: %s: %s", location, d.Code, d.Message)
}

func SortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if left.Source != right.Source {
			return left.Source < right.Source
		}
		if left.Field != right.Field {
			return left.Field < right.Field
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		return left.Message < right.Message
	})
}

func diag(source, field, code, format string, args ...any) Diagnostic {
	return Diagnostic{Source: source, Field: field, Code: code, Message: fmt.Sprintf(format, args...)}
}
