package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"doppels.so/cli/internal/manifest"
)

var (
	hexDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	versionPattern   = regexp.MustCompile(`(?:^|[^0-9A-Za-z])v?([0-9]+\.[0-9]+\.[0-9]+)`)
	uuidPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

func validateInvocation(invocation Invocation) (map[string]any, error) {
	if invocation.Capability == nil {
		return nil, fmt.Errorf("%w: Capability is required", ErrInvalidInvocation)
	}
	if invocation.ProjectRoot == "" {
		return nil, fmt.Errorf("%w: project root is required", ErrInvalidInvocation)
	}
	if err := validateReference(invocation.CapabilityRef, invocation.Capability.Metadata.Name, invocation.Capability.Metadata.Version, manifest.CapabilitySchemaID, manifest.CapabilitySchemaSHA256); err != nil {
		return nil, fmt.Errorf("%w: Capability reference: %v", ErrInvalidInvocation, err)
	}
	if invocation.Recipe != nil {
		if invocation.RecipeRef == nil {
			return nil, fmt.Errorf("%w: Recipe reference is required", ErrInvalidInvocation)
		}
		if err := validateReference(*invocation.RecipeRef, invocation.Recipe.Metadata.Name, invocation.Recipe.Metadata.Version, manifest.RecipeSchemaID, manifest.RecipeSchemaSHA256); err != nil {
			return nil, fmt.Errorf("%w: Recipe reference: %v", ErrInvalidInvocation, err)
		}
		provided := false
		for _, name := range invocation.Recipe.Provides {
			provided = provided || name == invocation.Capability.Metadata.Name
		}
		if !provided {
			return nil, fmt.Errorf("%w: Recipe %s does not provide Capability %s", ErrInvalidInvocation, invocation.Recipe.Metadata.Name, invocation.Capability.Metadata.Name)
		}
		if invocation.Recipe.Runtime != "shell" && invocation.Recipe.Runtime != "manual" {
			return nil, fmt.Errorf("%w: unsupported Recipe runtime %q", ErrInvalidInvocation, invocation.Recipe.Runtime)
		}
		if invocation.Recipe.Runtime == "shell" && invocation.NodeID == "" {
			return nil, fmt.Errorf("%w: automatic execution requires nodeId", ErrInvalidInvocation)
		}
	} else if invocation.RecipeRef != nil {
		return nil, fmt.Errorf("%w: Recipe reference without a Recipe", ErrInvalidInvocation)
	}
	if !validActor(invocation.RequestedBy, true) {
		return nil, fmt.Errorf("%w: requestedBy is required", ErrInvalidInvocation)
	}
	if !validActor(invocation.Executor, false) {
		return nil, fmt.Errorf("%w: executor is required", ErrInvalidInvocation)
	}
	if invocation.RequestID != "" && !uuidPattern.MatchString(invocation.RequestID) {
		return nil, fmt.Errorf("%w: requestId must be a UUID", ErrInvalidInvocation)
	}
	if invocation.RunID != "" && !uuidPattern.MatchString(invocation.RunID) {
		return nil, fmt.Errorf("%w: runId must be a UUID", ErrInvalidInvocation)
	}
	return resolveInputs(invocation.Capability, invocation.Inputs)
}

func validateExistingRequest(invocation Invocation) error {
	request := invocation.ExistingRequest
	if request.APIVersion != APIVersion || request.Kind != "Request" || !uuidPattern.MatchString(request.ID) || request.CreatedAt.IsZero() || request.IdempotencyKey == "" || !validActor(request.RequestedBy, true) {
		return fmt.Errorf("%w: existing Request is incomplete or invalid", ErrInvalidInvocation)
	}
	if err := validateRequestOrigin(request); err != nil {
		return err
	}
	if request.ShareID != "" && !uuidPattern.MatchString(request.ShareID) {
		return fmt.Errorf("%w: existing Request shareId must be a UUID", ErrInvalidInvocation)
	}
	if err := validateReference(request.Capability, invocation.Capability.Metadata.Name, invocation.Capability.Metadata.Version, manifest.CapabilitySchemaID, manifest.CapabilitySchemaSHA256); err != nil {
		return fmt.Errorf("%w: existing Request Capability reference: %v", ErrInvalidInvocation, err)
	}
	if invocation.CapabilityRef.Name != "" && invocation.CapabilityRef != request.Capability {
		return fmt.Errorf("%w: existing Request Capability reference differs from Invocation", ErrInvalidInvocation)
	}
	if invocation.RequestID != "" && invocation.RequestID != request.ID {
		return fmt.Errorf("%w: existing Request id differs from Invocation", ErrInvalidInvocation)
	}
	if invocation.ShareID != "" && invocation.ShareID != request.ShareID {
		return fmt.Errorf("%w: existing Request shareId differs from Invocation", ErrInvalidInvocation)
	}
	if invocation.IdempotencyKey != "" && invocation.IdempotencyKey != request.IdempotencyKey {
		return fmt.Errorf("%w: existing Request idempotencyKey differs from Invocation", ErrInvalidInvocation)
	}
	if invocation.RequestedBy.ID != "" && invocation.RequestedBy != request.RequestedBy {
		return fmt.Errorf("%w: existing Request actor differs from Invocation", ErrInvalidInvocation)
	}
	if invocation.Inputs != nil {
		left, leftErr := resolveInputs(invocation.Capability, invocation.Inputs)
		right, rightErr := resolveInputs(invocation.Capability, request.Inputs)
		if leftErr != nil || rightErr != nil || !reflect.DeepEqual(left, right) {
			return fmt.Errorf("%w: existing Request inputs differ from Invocation", ErrInvalidInvocation)
		}
	}
	return nil
}

func validateRequestOrigin(request *RequestRecord) error {
	switch request.Origin {
	case "share":
		if request.ShareID == "" {
			return fmt.Errorf("%w: existing Request origin share requires shareId", ErrInvalidInvocation)
		}
	case "cli", "api":
		if request.ShareID != "" {
			return fmt.Errorf("%w: existing Request origin %s forbids shareId", ErrInvalidInvocation, request.Origin)
		}
	default:
		return fmt.Errorf("%w: existing Request origin must be share, cli or api", ErrInvalidInvocation)
	}
	return nil
}

func validActor(actor ActorReference, guest bool) bool {
	if actor.ID == "" || len(actor.ID) > 200 {
		return false
	}
	switch actor.Kind {
	case "identity", "agent", "service", "anonymous":
		return true
	case "guest":
		return guest
	default:
		return false
	}
}

func validateReference(reference DefinitionReference, name, version, schemaID, schemaSHA256 string) error {
	if reference.Name != name || reference.Version != version {
		return fmt.Errorf("name/version do not match definition")
	}
	if !hexDigestPattern.MatchString(reference.ManifestSHA256) {
		return fmt.Errorf("manifestSha256 must be a lowercase SHA-256")
	}
	if reference.Schema.ID != schemaID || reference.Schema.SHA256 != schemaSHA256 {
		return fmt.Errorf("schema reference does not match published %s contract", APIVersion)
	}
	return nil
}

func resolveInputs(capability *manifest.Capability, supplied map[string]any) (map[string]any, error) {
	resolved := make(map[string]any, len(capability.Inputs))
	for name := range supplied {
		if _, exists := capability.Inputs[name]; !exists {
			return nil, fmt.Errorf("%w: input %q is not declared", ErrInvalidInvocation, name)
		}
	}
	for name, contract := range capability.Inputs {
		value, exists := supplied[name]
		if !exists && contract.Default != nil {
			value, exists = contract.Default, true
		}
		if !exists {
			if contract.Required {
				return nil, fmt.Errorf("%w: required input %q is missing", ErrInvalidInvocation, name)
			}
			continue
		}
		normalized, err := normalizeScalar(value, contract.Type)
		if err != nil {
			return nil, fmt.Errorf("%w: input %q: %v", ErrInvalidInvocation, name, err)
		}
		if len(contract.Enum) > 0 && !enumContains(contract.Enum, normalized, contract.Type) {
			return nil, fmt.Errorf("%w: input %q is not one of the allowed values", ErrInvalidInvocation, name)
		}
		resolved[name] = normalized
	}
	return resolved, nil
}

// ParseInputs converts CLI --input name=value strings into Capability-owned
// scalar types, then applies defaults and validates required and enum fields.
func ParseInputs(capability *manifest.Capability, supplied map[string]string) (map[string]any, error) {
	typed := make(map[string]any, len(supplied))
	for name, text := range supplied {
		contract, exists := capability.Inputs[name]
		if !exists {
			return nil, fmt.Errorf("%w: input %q is not declared", ErrInvalidInvocation, name)
		}
		var value any = text
		var err error
		switch contract.Type {
		case "integer", "number", "boolean":
			value, err = parseOutputString(text, contract.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: input %q: %v", ErrInvalidInvocation, name, err)
		}
		typed[name] = value
	}
	return resolveInputs(capability, typed)
}

func enumContains(options []any, value any, kind string) bool {
	for _, option := range options {
		normalized, err := normalizeScalar(option, kind)
		if err == nil && reflect.DeepEqual(normalized, value) {
			return true
		}
	}
	return false
}

func normalizeScalar(value any, kind string) (any, error) {
	switch kind {
	case "string":
		result, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		return result, nil
	case "boolean":
		result, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("must be a boolean")
		}
		return result, nil
	case "integer":
		return normalizeInteger(value)
	case "number":
		return normalizeNumber(value)
	default:
		return nil, fmt.Errorf("unsupported scalar type %q", kind)
	}
}

func normalizeInteger(value any) (any, error) {
	var normalized int64
	switch number := value.(type) {
	case int:
		normalized = int64(number)
	case int8:
		normalized = int64(number)
	case int16:
		normalized = int64(number)
	case int32:
		normalized = int64(number)
	case int64:
		normalized = number
	case uint:
		if uint64(number) > uint64(manifest.MaxSafeInteger) {
			return nil, fmt.Errorf("is outside the portable integer range")
		}
		normalized = int64(number)
	case uint8:
		normalized = int64(number)
	case uint16:
		normalized = int64(number)
	case uint32:
		normalized = int64(number)
	case uint64:
		if number > uint64(manifest.MaxSafeInteger) {
			return nil, fmt.Errorf("is outside the portable integer range")
		}
		normalized = int64(number)
	case json.Number:
		parsed, err := strconv.ParseInt(string(number), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("must be a base-10 portable integer")
		}
		normalized = parsed
	default:
		return nil, fmt.Errorf("must be an integer")
	}
	if normalized < manifest.MinSafeInteger || normalized > manifest.MaxSafeInteger {
		return nil, fmt.Errorf("is outside the portable integer range")
	}
	return normalized, nil
}

func normalizeNumber(value any) (any, error) {
	var result float64
	switch number := value.(type) {
	case float64:
		result = number
	case float32:
		result = float64(number)
	case int:
		result = float64(number)
	case int8:
		result = float64(number)
	case int16:
		result = float64(number)
	case int32:
		result = float64(number)
	case int64:
		result = float64(number)
	case uint:
		result = float64(number)
	case uint8:
		result = float64(number)
	case uint16:
		result = float64(number)
	case uint32:
		result = float64(number)
	case uint64:
		result = float64(number)
	case json.Number:
		parsed, err := strconv.ParseFloat(string(number), 64)
		if err != nil {
			return nil, fmt.Errorf("must be a number")
		}
		result = parsed
	default:
		return nil, fmt.Errorf("must be a number")
	}
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return nil, fmt.Errorf("must be finite")
	}
	if result == math.Trunc(result) && (result < float64(manifest.MinSafeInteger) || result > float64(manifest.MaxSafeInteger)) {
		return nil, fmt.Errorf("an integral number is outside the portable integer range")
	}
	return result, nil
}

func environmentMap(entries []string) map[string]string {
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			result[name] = value
		}
	}
	return result
}

func environmentList(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}

func preflight(ctx context.Context, invocation Invocation, options Options, inputs map[string]any) error {
	recipe := invocation.Recipe
	if recipe == nil || recipe.Runtime == "manual" {
		return nil
	}
	baseEnvironment := options.Environment
	if baseEnvironment == nil {
		baseEnvironment = os.Environ()
	}
	host := environmentMap(baseEnvironment)
	lookup := options.LookupCommand
	if lookup == nil {
		lookup = func(name string) (string, error) { return lookupPath(name, host["PATH"]) }
	}
	if recipe.Requires != nil {
		for _, requirement := range recipe.Requires.Commands {
			path, err := lookup(requirement.Name)
			if err != nil {
				return fmt.Errorf("%w: command %q is not available in PATH", ErrRequirements, requirement.Name)
			}
			if requirement.Version != "" {
				if err := checkCommandVersion(ctx, path, requirement.Version, environmentList(minimalEnvironment(host))); err != nil {
					return fmt.Errorf("%w: command %q: %v", ErrRequirements, requirement.Name, err)
				}
			}
		}
		for _, name := range recipe.Requires.HostEnv {
			if _, exists := host[name]; !exists {
				return fmt.Errorf("%w: host environment variable %q is not set", ErrRequirements, name)
			}
		}
		available := values{inputs: inputs, steps: map[string]map[string]any{}}
		for _, template := range recipe.Requires.Files {
			relative, err := renderTemplate(template, available)
			if err != nil {
				return fmt.Errorf("%w: required file %q: %v", ErrRequirements, template, err)
			}
			path, err := securePath(invocation.ProjectRoot, filepath.Join(invocation.ProjectRoot, filepath.FromSlash(relative)), false)
			if err != nil {
				return fmt.Errorf("%w: required file %q: %v", ErrRequirements, relative, err)
			}
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("%w: required file %q: %v", ErrRequirements, relative, err)
			}
		}
	}
	for _, step := range recipe.Steps {
		if step.Run == nil {
			return fmt.Errorf("%w: Step %q has no run block", ErrInvalidInvocation, step.ID)
		}
		if strings.Contains(step.Run.Script, "{{") || strings.Contains(step.Run.Script, "}}") || strings.Contains(step.Run.Script, "${{") {
			return fmt.Errorf("%w: Step %q script contains a Doppels expression", ErrInvalidInvocation, step.ID)
		}
		if _, err := lookup(step.Run.Shell); err != nil {
			return fmt.Errorf("%w: shell %q for Step %q is not available", ErrRequirements, step.Run.Shell, step.ID)
		}
	}
	return nil
}

func lookupPath(name, pathValue string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		return "", exec.ErrNotFound
	}
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			directory = "."
		}
		candidate := filepath.Join(directory, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func checkCommandVersion(ctx context.Context, path, constraint string, environment []string) error {
	probeContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	command := exec.CommandContext(probeContext, path, "--version")
	configureProcess(command)
	command.Env = environment
	output, err := command.CombinedOutput()
	if errors.Is(probeContext.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("version probe timed out")
	}
	if err != nil && len(output) == 0 {
		return fmt.Errorf("cannot determine version: %w", err)
	}
	match := versionPattern.FindStringSubmatch(string(output))
	if match == nil {
		return fmt.Errorf("cannot parse a semantic version from --version")
	}
	version, err := parseVersion(match[1])
	if err != nil {
		return err
	}
	for _, condition := range strings.Fields(constraint) {
		operator := "="
		for _, prefix := range []string{">=", "<=", ">", "<", "=", "^", "~"} {
			if strings.HasPrefix(condition, prefix) {
				operator, condition = prefix, strings.TrimPrefix(condition, prefix)
				break
			}
		}
		expected, err := parseVersion(condition)
		if err != nil {
			return err
		}
		comparison := compareVersion(version, expected)
		matches := map[string]bool{"=": comparison == 0, ">": comparison > 0, ">=": comparison >= 0, "<": comparison < 0, "<=": comparison <= 0}[operator]
		if operator == "^" {
			upper := [3]int64{}
			switch {
			case expected[0] > 0:
				upper[0] = expected[0] + 1
			case expected[1] > 0:
				upper[1] = expected[1] + 1
			default:
				upper[2] = expected[2] + 1
			}
			matches = comparison >= 0 && compareVersion(version, upper) < 0
		}
		if operator == "~" {
			upper := [3]int64{expected[0], expected[1] + 1, 0}
			matches = comparison >= 0 && compareVersion(version, upper) < 0
		}
		if !matches {
			return fmt.Errorf("version %s does not satisfy %s", match[1], constraint)
		}
	}
	return nil
}

func parseVersion(value string) ([3]int64, error) {
	var version [3]int64
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return version, fmt.Errorf("invalid semantic version %q", value)
	}
	for index, part := range parts {
		number, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return version, fmt.Errorf("invalid semantic version %q", value)
		}
		version[index] = number
	}
	return version, nil
}

func compareVersion(left, right [3]int64) int {
	for index := range left {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}

// securePath rejects lexical escapes and symlinks whose resolved target is
// outside the project root. The candidate must already exist.
func securePath(projectRoot, candidate string, directory bool) (string, error) {
	rootAbsolute, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", err
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(rootAbsolute)
	if err != nil {
		return "", err
	}
	if !inside(rootAbsolute, candidate) && !inside(root, candidate) {
		return "", fmt.Errorf("path escapes project root")
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	if !inside(root, resolved) {
		return "", fmt.Errorf("symlink target escapes project root")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if directory && !info.IsDir() {
		return "", fmt.Errorf("path is not a directory")
	}
	if !directory && info.IsDir() {
		return "", fmt.Errorf("path is a directory")
	}
	return resolved, nil
}

func inside(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
