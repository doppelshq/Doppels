package command

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/runstate"
)

func writeRunShow(writer io.Writer, detail *runstate.Detail, now time.Time) {
	style := newTermStyle(writer)
	summary := detail.Summary
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Run"), style.shortIDPrimary(summary.ID))
	writeRunStatusLine(writer, style, summary.Status)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Cap"), summary.Capability)
	if summary.Recipe != "" {
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Recipe"), summary.Recipe)
	}
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Created"), formatDisplayTime(now, summary.CreatedAt))
	if summary.RequestID != "" {
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Request"), style.dim(shortRunID(summary.RequestID)))
	}
	if summary.StateDir != "" {
		fmt.Fprintf(writer, "  %s  %s\n", style.field("State"), style.dim(shortStateDir(summary.StateDir)))
	}

	if len(detail.Events) > 0 {
		fmt.Fprintln(writer)
		fmt.Fprintf(writer, "  %s\n", style.bold("Timeline"))
		writeRunEventTimeline(writer, style, detail.Events)
	}

	returns := returnsFromEvents(detail.Events)
	attachArtifactPaths(summary.StateDir, returns)
	if len(returns) > 0 {
		fmt.Fprintln(writer)
		fmt.Fprintf(writer, "  %s\n", style.bold("Returns"))
		writeNamedResultMap(writer, style, returns)
	}
}

func writeRunStatusLine(writer io.Writer, style termStyle, status string) {
	status = strings.TrimSpace(status)
	switch status {
	case "succeeded":
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Status"), style.boldGreen(checkMark(style))+" "+style.bold("Succeeded"))
	case "failed":
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Status"), style.boldRed(crossMark(style))+" "+style.bold("Failed"))
	case "running":
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Status"), style.boldCyan(arrowMark(style))+" "+style.bold("Running"))
	case "interrupted":
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Status"), style.boldYellow(ellipsisMark(style))+" "+style.bold("Interrupted"))
	case "cancelled":
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Status"), style.boldYellow(ellipsisMark(style))+" "+style.bold("Cancelled"))
	default:
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Status"), style.value(status))
	}
}

func arrowMark(style termStyle) string {
	if style.enabled {
		return "→"
	}
	return ">"
}

func writeRunEventTimeline(writer io.Writer, style termStyle, events []execution.RunEvent) {
	for _, event := range events {
		mark, text := formatRunEventLine(style, event)
		fmt.Fprintf(writer, "  %s %s\n", mark, text)
	}
}

func formatRunEventLine(style termStyle, event execution.RunEvent) (string, string) {
	switch event.Type {
	case "run_created":
		return style.dim("·"), "created"
	case "validation_succeeded":
		return style.green(checkMark(style)), "validated"
	case "validation_failed":
		return style.red(crossMark(style)), "validation failed"
	case "approval_requested":
		return style.yellow(ellipsisMark(style)), "approval needed · " + event.StepID
	case "approval_approved":
		return style.green(checkMark(style)), "approved · " + event.StepID
	case "approval_rejected":
		return style.red(crossMark(style)), "rejected · " + event.StepID
	case "step_started":
		return style.cyan(arrowMark(style)), event.StepID
	case "step_succeeded":
		return style.green(checkMark(style)), event.StepID
	case "step_failed":
		detail := event.StepID + " failed"
		if event.Data != nil {
			if errText, ok := event.Data["error"].(string); ok && errText != "" {
				detail += " · " + errText
			}
		}
		return style.red(crossMark(style)), detail
	case "run_succeeded":
		return style.boldGreen(checkMark(style)), "succeeded"
	case "run_failed":
		return style.boldRed(crossMark(style)), "failed"
	case "run_interrupted":
		return style.boldYellow(ellipsisMark(style)), "interrupted"
	case "run_cancelled":
		return style.boldYellow(ellipsisMark(style)), "cancelled"
	default:
		return style.dim("·"), event.Type
	}
}

func returnsFromEvents(events []execution.RunEvent) map[string]any {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != "run_succeeded" || events[i].Data == nil {
			continue
		}
		raw, ok := events[i].Data["returns"]
		if !ok {
			continue
		}
		return normalizeReturnMap(raw)
	}
	return nil
}

func normalizeReturnMap(raw any) map[string]any {
	switch typed := raw.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for name, value := range typed {
			out[name] = normalizeReturnValue(value)
		}
		return out
	default:
		return nil
	}
}

func normalizeReturnValue(value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if nested, ok := object["artifact"].(map[string]any); ok {
		object = nested
	}
	id, _ := object["id"].(string)
	filename, _ := object["filename"].(string)
	mediaType, _ := object["mediaType"].(string)
	sha, _ := object["sha256"].(string)
	var size int64
	switch typed := object["sizeBytes"].(type) {
	case float64:
		size = int64(typed)
	case int64:
		size = typed
	case int:
		size = int64(typed)
	}
	if filename == "" && sha == "" {
		return value
	}
	return execution.ArtifactReference{
		ID: id, Filename: filename, MediaType: mediaType, SizeBytes: size, SHA256: sha,
	}
}

func attachArtifactPaths(stateDir string, returns map[string]any) {
	if stateDir == "" || len(returns) == 0 {
		return
	}
	for name, value := range returns {
		artifact, ok := value.(execution.ArtifactReference)
		if !ok || artifact.LocalPath != "" || artifact.ID == "" || artifact.Filename == "" {
			continue
		}
		candidate := filepath.Join(stateDir, "artifacts", artifact.ID+"-"+artifact.Filename)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			artifact.LocalPath = candidate
			returns[name] = artifact
		}
	}
}

func writeRunLogs(writer io.Writer, detail *runstate.Detail, logs []runstate.Log) {
	style := newTermStyle(writer)
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Run"), style.shortIDPrimary(detail.Summary.ID))
	writeRunStatusLine(writer, style, detail.Summary.Status)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Cap"), detail.Summary.Capability)
	if detail.Summary.Recipe != "" {
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Recipe"), detail.Summary.Recipe)
	}
	if len(logs) == 0 {
		fmt.Fprintln(writer)
		fmt.Fprintf(writer, "  %s\n", style.dim("No step logs recorded."))
		return
	}
	fmt.Fprintln(writer)
	for i, log := range logs {
		if i > 0 {
			fmt.Fprintln(writer)
		}
		title := log.StepID
		if log.Stream != "" {
			title = log.StepID + " · " + log.Stream
		}
		fmt.Fprintf(writer, "  %s\n", style.bold(title))
		content := strings.TrimRight(log.Content, "\n")
		if content == "" {
			fmt.Fprintf(writer, "  %s\n", style.dim("(empty)"))
			continue
		}
		for _, line := range strings.Split(content, "\n") {
			fmt.Fprintf(writer, "  %s\n", line)
		}
	}
}
