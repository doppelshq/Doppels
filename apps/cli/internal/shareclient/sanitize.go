package shareclient

import (
	"encoding/json"

	"doppels.so/cli/internal/execution"
)

// SanitizeRunEvent creates the only representation that may cross the local
// execution boundary. Logs, local paths, errors and intermediate products are
// intentionally not part of the shared protocol. Declared manual evidence is
// durable private audit data, so it is retained on the terminal event; the
// Cloud's public projection is responsible for never exposing it to a guest.
func SanitizeRunEvent(source execution.RunEvent) execution.RunEvent {
	result := execution.RunEvent{
		APIVersion: source.APIVersion,
		Kind:       source.Kind,
		RunID:      source.RunID,
		Sequence:   source.Sequence,
		OccurredAt: source.OccurredAt,
		Type:       source.Type,
		StepID:     source.StepID,
	}
	data := map[string]any{}
	if status, ok := source.Data["status"].(string); ok && publicStatus(status) {
		data["status"] = status
	}
	if timedOut, ok := source.Data["timedOut"].(bool); ok {
		data["timedOut"] = timedOut
	}
	if exitCode, ok := publicNumber(source.Data["exitCode"]); ok {
		data["exitCode"] = exitCode
	}
	if source.Type == "run_succeeded" {
		if values, ok := source.Data["returns"].(map[string]any); ok {
			data["returns"] = sanitizedValues(values)
		}
		if values, ok := source.Data["evidence"].(map[string]any); ok {
			data["evidence"] = sanitizedValues(values)
		}
	}
	if len(data) > 0 {
		result.Data = data
	}
	return result
}

func sanitizedValues(values map[string]any) map[string]any {
	result := map[string]any{}
	for name, value := range values {
		if sanitized, ok := publicReturn(value); ok {
			result[name] = sanitized
		}
	}
	return result
}

func publicStatus(value string) bool {
	switch value {
	case "pending", "running", "waiting_approval", "approved", "rejected", "succeeded", "failed", "cancelled", "interrupted":
		return true
	default:
		return false
	}
}

func publicNumber(value any) (any, bool) {
	switch value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return value, true
	default:
		return nil, false
	}
}

func publicReturn(value any) (any, bool) {
	switch typed := value.(type) {
	case string, bool, json.Number:
		return typed, true
	case execution.ArtifactReference:
		return typed.PublicValue(), true
	case *execution.ArtifactReference:
		if typed == nil {
			return nil, false
		}
		return typed.PublicValue(), true
	case map[string]any:
		rawArtifact, exists := typed["artifact"]
		if !exists || len(typed) != 1 {
			return nil, false
		}
		artifact, ok := rawArtifact.(map[string]any)
		if !ok {
			return nil, false
		}
		return sanitizeArtifactMap(artifact)
	default:
		if number, ok := publicNumber(typed); ok {
			return number, true
		}
		return nil, false
	}
}

func sanitizeArtifactMap(source map[string]any) (any, bool) {
	artifact := map[string]any{}
	for _, name := range []string{"id", "filename", "mediaType", "sizeBytes", "sha256"} {
		value, exists := source[name]
		if !exists {
			return nil, false
		}
		artifact[name] = value
	}
	return map[string]any{"artifact": artifact}, true
}
