package manifest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Host interface {
	LookupCommand(name string) (string, error)
	LookupEnv(name string) (string, bool)
	Stat(path string) (fs.FileInfo, error)
	CommandVersion(path string) (string, error)
}

type OSHost struct{}

func (OSHost) LookupCommand(name string) (string, error) { return exec.LookPath(name) }
func (OSHost) LookupEnv(name string) (string, bool)      { return os.LookupEnv(name) }
func (OSHost) Stat(path string) (fs.FileInfo, error)     { return os.Stat(path) }

func (OSHost) CommandVersion(path string) (string, error) {
	var lastErr error
	for _, arguments := range [][]string{{"--version"}, {"version"}, {"-version"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		output, err := exec.CommandContext(ctx, path, arguments...).CombinedOutput()
		contextErr := ctx.Err()
		cancel()
		if contextErr != nil {
			lastErr = fmt.Errorf("version probe timed out")
			continue
		}
		if _, ok := extractSemver(string(output)); ok {
			return string(output), nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("version output did not contain a semantic version")
		}
	}
	return "", lastErr
}

type hostIssue struct {
	Field  string
	Code   string
	Format string
	Args   []any
	Label  string
}

func (issue hostIssue) diagnostic(source string) Diagnostic {
	return diag(source, issue.Field, issue.Code, issue.Format, issue.Args...)
}

// CheckRequires lists unmet Recipe host requirements on this Node.
// Empty means commands, hostEnv, files, and Step shells are available.
func CheckRequires(recipe *Recipe, catalogRoot string, host Host) []string {
	issues := recipeHostIssues(recipe, catalogRoot, host)
	labels := make([]string, 0, len(issues))
	seen := map[string]struct{}{}
	for _, issue := range issues {
		if issue.Label == "" {
			continue
		}
		if _, ok := seen[issue.Label]; ok {
			continue
		}
		seen[issue.Label] = struct{}{}
		labels = append(labels, issue.Label)
	}
	return labels
}

func hostDiagnostics(catalog *Catalog, host Host) []Diagnostic {
	if host == nil || catalog == nil {
		return nil
	}
	var diagnostics []Diagnostic
	for _, revisions := range catalog.Recipes {
		for _, definition := range revisions {
			source := definition.Source.Path
			for _, issue := range recipeHostIssues(definition.Value, catalog.Root, host) {
				diagnostics = append(diagnostics, issue.diagnostic(source))
			}
		}
	}
	return diagnostics
}

func recipeHostIssues(recipe *Recipe, catalogRoot string, host Host) []hostIssue {
	if host == nil || recipe == nil {
		return nil
	}
	if recipe.Runtime != "shell" || recipe.Requires == nil {
		return nil
	}
	var issues []hostIssue
	for index, requirement := range recipe.Requires.Commands {
		field := fmt.Sprintf("requires.commands[%d]", index)
		path, err := host.LookupCommand(requirement.Name)
		if err != nil {
			issues = append(issues, hostIssue{
				Field:  field,
				Code:   "host.command-missing",
				Format: "required command %q is not available in PATH",
				Args:   []any{requirement.Name},
				Label:  "command " + requirement.Name,
			})
			continue
		}
		if requirement.Version == "" {
			continue
		}
		output, err := host.CommandVersion(path)
		if err != nil {
			issues = append(issues, hostIssue{
				Field:  field,
				Code:   "host.version-probe",
				Format: "cannot determine %s version: %v",
				Args:   []any{requirement.Name, err},
				Label:  "command " + requirement.Name,
			})
			continue
		}
		version, ok := extractSemver(output)
		if !ok {
			issues = append(issues, hostIssue{
				Field:  field,
				Code:   "host.version-probe",
				Format: "cannot parse a semantic version from %s --version",
				Args:   []any{requirement.Name},
				Label:  "command " + requirement.Name,
			})
			continue
		}
		matches, err := satisfiesConstraint(version, requirement.Version)
		if err != nil {
			continue
		}
		if !matches {
			issues = append(issues, hostIssue{
				Field:  field,
				Code:   "host.version-mismatch",
				Format: "%s %s does not satisfy %s",
				Args:   []any{requirement.Name, version.String(), requirement.Version},
				Label:  "command " + requirement.Name,
			})
		}
	}
	seenShells := map[string]struct{}{}
	for index, step := range recipe.Steps {
		if step.Run == nil {
			continue
		}
		if _, checked := seenShells[step.Run.Shell]; checked {
			continue
		}
		seenShells[step.Run.Shell] = struct{}{}
		if _, err := host.LookupCommand(step.Run.Shell); err != nil {
			issues = append(issues, hostIssue{
				Field:  fmt.Sprintf("steps[%d].run.shell", index),
				Code:   "host.shell-missing",
				Format: "required shell %q is not available in PATH",
				Args:   []any{step.Run.Shell},
				Label:  "shell " + step.Run.Shell,
			})
		}
	}
	for index, name := range recipe.Requires.HostEnv {
		if _, exists := host.LookupEnv(name); !exists {
			issues = append(issues, hostIssue{
				Field:  fmt.Sprintf("requires.hostEnv[%d]", index),
				Code:   "host.env-missing",
				Format: "required host environment variable %q is not set",
				Args:   []any{name},
				Label:  "env " + name,
			})
		}
	}
	for index, file := range recipe.Requires.Files {
		if strings.Contains(file, "{{") || strings.Contains(file, "}}") {
			continue
		}
		path := filepath.Join(catalogRoot, filepath.FromSlash(file))
		info, err := host.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				issues = append(issues, hostIssue{
					Field:  fmt.Sprintf("requires.files[%d]", index),
					Code:   "host.file-missing",
					Format: "required project file %q does not exist",
					Args:   []any{file},
					Label:  "file " + file,
				})
			} else {
				issues = append(issues, hostIssue{
					Field:  fmt.Sprintf("requires.files[%d]", index),
					Code:   "host.file-unavailable",
					Format: "cannot inspect required project file %q: %v",
					Args:   []any{file, err},
					Label:  "file " + file,
				})
			}
		} else if info.IsDir() {
			issues = append(issues, hostIssue{
				Field:  fmt.Sprintf("requires.files[%d]", index),
				Code:   "host.file-invalid",
				Format: "required project path %q is a directory, not a file",
				Args:   []any{file},
				Label:  "file " + file,
			})
		}
	}
	return issues
}

var versionInOutputPattern = regexp.MustCompile(`v?([0-9]+\.[0-9]+\.[0-9]+)(?:[-+][0-9A-Za-z.-]+)?`)

type semVersion struct {
	major int64
	minor int64
	patch int64
}

func (v semVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

func extractSemver(value string) (semVersion, bool) {
	match := versionInOutputPattern.FindStringSubmatch(value)
	if match == nil {
		return semVersion{}, false
	}
	version, err := parseSemver(match[1])
	return version, err == nil
}

func parseSemver(value string) (semVersion, error) {
	core := strings.SplitN(value, "-", 2)[0]
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semVersion{}, fmt.Errorf("invalid semantic version %q", value)
	}
	parsed := make([]int64, 3)
	for index, part := range parts {
		number, err := strconv.ParseInt(part, 10, 64)
		if err != nil || number < 0 {
			return semVersion{}, fmt.Errorf("invalid semantic version %q", value)
		}
		parsed[index] = number
	}
	return semVersion{major: parsed[0], minor: parsed[1], patch: parsed[2]}, nil
}

func compareVersion(left, right semVersion) int {
	if left.major != right.major {
		if left.major < right.major {
			return -1
		}
		return 1
	}
	if left.minor != right.minor {
		if left.minor < right.minor {
			return -1
		}
		return 1
	}
	if left.patch != right.patch {
		if left.patch < right.patch {
			return -1
		}
		return 1
	}
	return 0
}

func satisfiesConstraint(version semVersion, constraint string) (bool, error) {
	for _, token := range strings.Fields(constraint) {
		operator := "="
		value := token
		for _, candidate := range []string{">=", "<=", ">", "<", "=", "~", "^"} {
			if strings.HasPrefix(token, candidate) {
				operator = candidate
				value = strings.TrimPrefix(token, candidate)
				break
			}
		}
		required, err := parseSemver(value)
		if err != nil {
			return false, err
		}
		comparison := compareVersion(version, required)
		matches := false
		switch operator {
		case "=":
			matches = comparison == 0
		case ">=":
			matches = comparison >= 0
		case ">":
			matches = comparison > 0
		case "<=":
			matches = comparison <= 0
		case "<":
			matches = comparison < 0
		case "~":
			upper := semVersion{major: required.major, minor: required.minor + 1}
			matches = comparison >= 0 && compareVersion(version, upper) < 0
		case "^":
			upper := semVersion{}
			switch {
			case required.major > 0:
				upper.major = required.major + 1
			case required.minor > 0:
				upper.minor = required.minor + 1
			default:
				upper.patch = required.patch + 1
			}
			matches = comparison >= 0 && compareVersion(version, upper) < 0
		}
		if !matches {
			return false, nil
		}
	}
	return true, nil
}
