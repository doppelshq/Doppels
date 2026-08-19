package execution

import (
	"crypto/sha256"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"doppels.so/cli/internal/localstate"
	"doppels.so/cli/internal/manifest"
)

var jsonNumberPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

func collectProducts(store *localstate.Store, projectRoot, workingDirectory string, step manifest.Step, available values, snapshot map[string]string, artifacts map[string]ArtifactReference) (map[string]any, error) {
	products := make(map[string]any, len(step.Produces))
	for name, product := range step.Produces {
		switch {
		case product.Env != "":
			value, exists := snapshot[product.Env]
			if !exists {
				return nil, fmt.Errorf("Step %q did not export %s", step.ID, product.Env)
			}
			products[name] = value
		case product.File != "":
			relative, err := renderTemplate(product.File, available)
			if err != nil {
				return nil, fmt.Errorf("Step %q product %q: %w", step.ID, name, err)
			}
			path, err := securePath(projectRoot, filepath.Join(workingDirectory, filepath.FromSlash(relative)), false)
			if err != nil {
				return nil, fmt.Errorf("Step %q product %q: %w", step.ID, name, err)
			}
			artifact, err := snapshotArtifact(store, path, "")
			if err != nil {
				return nil, fmt.Errorf("Step %q product %q: %w", step.ID, name, err)
			}
			artifacts[artifact.ID] = artifact
			products[name] = artifact
		default:
			return nil, fmt.Errorf("Step %q product %q has neither file nor env", step.ID, name)
		}
	}
	return products, nil
}

func snapshotArtifact(store *localstate.Store, source, mediaType string) (ArtifactReference, error) {
	filename := filepath.Base(source)
	id, err := newUUID()
	if err != nil {
		return ArtifactReference{}, err
	}
	snapshot, err := store.CopyArtifact(id, filename, source)
	if err != nil {
		return ArtifactReference{}, err
	}
	file, err := os.Open(snapshot)
	if err != nil {
		return ArtifactReference{}, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return ArtifactReference{}, err
	}
	if mediaType == "" {
		mediaType = detectMediaType(snapshot)
	}
	return ArtifactReference{
		ID: id, Filename: filename, MediaType: mediaType, SizeBytes: size,
		SHA256: fmt.Sprintf("%x", hash.Sum(nil)), LocalPath: snapshot,
	}, nil
}

func detectMediaType(path string) string {
	if mediaType := mime.TypeByExtension(filepath.Ext(path)); mediaType != "" {
		return strings.TrimSpace(strings.Split(mediaType, ";")[0])
	}
	file, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer file.Close()
	buffer := make([]byte, 512)
	count, _ := file.Read(buffer)
	return http.DetectContentType(buffer[:count])
}

func materializeShellReturns(capability *manifest.Capability, recipe *manifest.Recipe, available values, artifacts map[string]ArtifactReference) (map[string]any, error) {
	returns := make(map[string]any, len(capability.Outputs))
	declaredMedia := map[string]string{}
	for name, contract := range capability.Outputs {
		returnVal, exists := recipe.Returns[name]
		if !exists {
			return nil, fmt.Errorf("Recipe does not return Capability output %q", name)
		}
		expression := returnVal.Ref()
		value, err := resolveExact(expression, available)
		if err != nil {
			return nil, fmt.Errorf("return %q: %w", name, err)
		}
		converted, err := convertOutput(value, contract)
		if err != nil {
			return nil, fmt.Errorf("return %q: %w", name, err)
		}
		if artifact, ok := converted.(ArtifactReference); ok {
			if previous, exists := declaredMedia[artifact.ID]; exists && previous != artifact.MediaType {
				return nil, fmt.Errorf("artifact %s is returned with incompatible media types %q and %q", artifact.Filename, previous, artifact.MediaType)
			}
			declaredMedia[artifact.ID] = artifact.MediaType
			artifacts[artifact.ID] = artifact
			// Keep later references to this product canonical as well.
			if parts := strings.Split(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(expression, "{{"), "}}")), "."); len(parts) == 3 {
				available.steps[parts[1]][parts[2]] = artifact
			}
		}
		returns[name] = converted
	}
	return returns, nil
}

func convertOutput(value any, contract manifest.OutputContract) (any, error) {
	if contract.Type == "artifact" {
		artifact, ok := value.(ArtifactReference)
		if !ok {
			return nil, fmt.Errorf("must reference a file product")
		}
		if contract.MediaType != "" {
			artifact.MediaType = contract.MediaType
		}
		return artifact, nil
	}
	if text, ok := value.(string); ok {
		return parseOutputString(text, contract.Type)
	}
	return normalizeScalar(value, contract.Type)
}

func parseOutputString(value, kind string) (any, error) {
	switch kind {
	case "string":
		return value, nil
	case "integer":
		if !regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`).MatchString(value) {
			return nil, fmt.Errorf("%q is not a base-10 integer", value)
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is outside the portable integer range", value)
		}
		return normalizeInteger(parsed)
	case "number":
		if !jsonNumberPattern.MatchString(value) {
			return nil, fmt.Errorf("%q is not a finite JSON number", value)
		}
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a finite JSON number", value)
		}
		return normalizeNumber(parsed)
	case "boolean":
		if value == "true" {
			return true, nil
		}
		if value == "false" {
			return false, nil
		}
		return nil, fmt.Errorf("%q is not true or false", value)
	default:
		return nil, fmt.Errorf("unsupported output type %q", kind)
	}
}

func publicReturns(returns map[string]any) map[string]any {
	result := make(map[string]any, len(returns))
	for name, value := range returns {
		if artifact, ok := value.(ArtifactReference); ok {
			result[name] = artifact.PublicValue()
		} else {
			result[name] = value
		}
	}
	return result
}
