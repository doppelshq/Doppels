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

func hostDiagnostics(catalog *Catalog, host Host) []Diagnostic {
	if host == nil {
		return nil
	}
	var diagnostics []Diagnostic
	for _, revisions := range catalog.Recipes {
		for _, definition := range revisions {
			recipe := definition.Value
			if recipe.Runtime != "shell" || recipe.Requires == nil {
				continue
			}
			source := definition.Source.Path
			for index, requirement := range recipe.Requires.Commands {
				field := fmt.Sprintf("requires.commands[%d]", index)
				path, err := host.LookupCommand(requirement.Name)
				if err != nil {
					diagnostics = append(diagnostics, diag(source, field, "host.command-missing", "required command %q is not available in PATH", requirement.Name))
					continue
				}
				if requirement.Version == "" {
					continue
				}
				output, err := host.CommandVersion(path)
				if err != nil {
					diagnostics = append(diagnostics, diag(source, field, "host.version-probe", "cannot determine %s version: %v", requirement.Name, err))
					continue
				}
				version, ok := extractSemver(output)
				if !ok {
					diagnostics = append(diagnostics, diag(source, field, "host.version-probe", "cannot parse a semantic version from %s --version", requirement.Name))
					continue
				}
				matches, err := satisfiesConstraint(version, requirement.Version)
				if err != nil {
					// The structural validator already reports malformed constraints.
					continue
				}
				if !matches {
					diagnostics = append(diagnostics, diag(source, field, "host.version-mismatch", "%s %s does not satisfy %s", requirement.Name, version.String(), requirement.Version))
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
					diagnostics = append(diagnostics, diag(source, fmt.Sprintf("steps[%d].run.shell", index), "host.shell-missing", "required shell %q is not available in PATH", step.Run.Shell))
				}
			}
			for index, name := range recipe.Requires.HostEnv {
				if _, exists := host.LookupEnv(name); !exists {
					diagnostics = append(diagnostics, diag(source, fmt.Sprintf("requires.hostEnv[%d]", index), "host.env-missing", "required host environment variable %q is not set", name))
				}
			}
			for index, file := range recipe.Requires.Files {
				if strings.Contains(file, "{{") || strings.Contains(file, "}}") {
					// Dynamic paths can only be checked once invocation inputs exist.
					continue
				}
				path := filepath.Join(catalog.Root, filepath.FromSlash(file))
				info, err := host.Stat(path)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						diagnostics = append(diagnostics, diag(source, fmt.Sprintf("requires.files[%d]", index), "host.file-missing", "required project file %q does not exist", file))
					} else {
						diagnostics = append(diagnostics, diag(source, fmt.Sprintf("requires.files[%d]", index), "host.file-unavailable", "cannot inspect required project file %q: %v", file, err))
					}
				} else if info.IsDir() {
					diagnostics = append(diagnostics, diag(source, fmt.Sprintf("requires.files[%d]", index), "host.file-invalid", "required project path %q is a directory, not a file", file))
				}
			}
		}
	}
	return diagnostics
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
