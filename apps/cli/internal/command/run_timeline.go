package command

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"doppels.so/cli/internal/execution"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// runTimeline prints a live execution progress stream as RunEvents arrive.
type runTimeline struct {
	writer      io.Writer
	style       termStyle
	capName     string
	capVer      string
	recipe      string
	pinWarnings []string
	stepNames   map[string]string
	header      bool
	hideCatalog bool

	mu          sync.Mutex
	spinStop    chan struct{}
	spinDone    chan struct{}
	spinning    bool
	runStarted  time.Time
	stepStarted time.Time
}

func newRunTimeline(writer io.Writer, invocation execution.Invocation) *runTimeline {
	timeline := &runTimeline{
		writer:    writer,
		style:     newTermStyle(writer),
		capName:   invocation.Capability.Metadata.Name,
		capVer:    invocation.Capability.Metadata.Version,
		stepNames: map[string]string{},
	}
	if invocation.Recipe != nil {
		timeline.recipe = revisionLabel(invocation.Recipe.Metadata.Name, invocation.Recipe.Metadata.Version)
		for _, step := range invocation.Recipe.Steps {
			label := strings.TrimSpace(step.Name)
			if label == "" {
				label = step.ID
			}
			timeline.stepNames[step.ID] = label
		}
	} else {
		timeline.recipe = "manual"
	}
	return timeline
}

func (t *runTimeline) elapsed() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.runStarted.IsZero() {
		return 0
	}
	return time.Since(t.runStarted)
}

func (t *runTimeline) onEvent(_ context.Context, event execution.RunEvent) error {
	switch event.Type {
	case "run_created":
		t.writeHeader(event.RunID)
	case "validation_succeeded":
		t.stopSpinner()
		t.line(t.okMark(), "validated")
	case "validation_failed":
		t.stopSpinner()
		t.line(t.failMark(), "validation failed")
	case "approval_requested":
		t.stopSpinner()
		t.line(t.waitMark(), "waiting approval · "+t.stepLabel(event.StepID))
	case "approval_approved":
		t.line(t.okMark(), "approved · "+t.stepLabel(event.StepID))
	case "approval_rejected":
		t.line(t.failMark(), "rejected · "+t.stepLabel(event.StepID))
	case "step_started":
		t.startSpinner(t.stepLabel(event.StepID))
	case "step_succeeded":
		t.finishStep(t.okMark(), t.stepLabel(event.StepID))
	case "step_failed":
		detail := t.stepLabel(event.StepID)
		if event.Data != nil {
			if errText, ok := event.Data["error"].(string); ok && errText != "" {
				detail += " · " + errText
			}
		}
		t.finishStep(t.failMark(), detail)
	case "run_succeeded":
		t.stopSpinner()
		fmt.Fprintln(t.writer)
	case "run_failed":
		t.stopSpinner()
		t.line(t.style.boldRed(t.failRaw()), "failed")
		fmt.Fprintln(t.writer)
	case "run_interrupted":
		t.stopSpinner()
		t.line(t.style.boldYellow(t.waitRaw()), "interrupted")
		fmt.Fprintln(t.writer)
	case "run_cancelled":
		t.stopSpinner()
		t.line(t.style.boldYellow(t.waitRaw()), "cancelled")
		fmt.Fprintln(t.writer)
	}
	return nil
}

func (t *runTimeline) writeHeader(runID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.header {
		return
	}
	t.header = true
	t.runStarted = time.Now()
	fmt.Fprintln(t.writer)
	fmt.Fprintf(t.writer, "  %s  %s\n", t.style.field("Run"), t.style.value(shortRunID(runID)))
	if !t.hideCatalog {
		fmt.Fprintf(t.writer, "  %s  %s\n", t.style.field("Cap"), t.style.bold(t.capName))
		fmt.Fprintf(t.writer, "  %s  %s\n", t.style.field("Recipe"), t.recipe)
	}
	for range t.pinWarnings {
		fmt.Fprintf(t.writer, "  %s  %s\n", t.style.field("PIN"), t.style.boldYellow("stale"))
	}
	fmt.Fprintln(t.writer)
}

func (t *runTimeline) startSpinner(label string) {
	t.stopSpinner()
	started := time.Now()
	t.mu.Lock()
	t.stepStarted = started
	t.mu.Unlock()

	if !t.style.enabled {
		t.line(t.runMark(), label+" …")
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	t.mu.Lock()
	t.spinStop = stop
	t.spinDone = done
	t.spinning = true
	t.mu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		frame := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				elapsed := formatDurationLive(time.Since(started))
				t.mu.Lock()
				mark := t.style.cyan(spinnerFrames[frame%len(spinnerFrames)])
				msg := fmt.Sprintf("  %s %s  %s", mark, label, t.style.dim("· "+elapsed))
				fmt.Fprintf(t.writer, "\r\x1b[2K%s", msg)
				t.mu.Unlock()
				frame++
			}
		}
	}()
}

func (t *runTimeline) stopSpinner() {
	t.mu.Lock()
	stop := t.spinStop
	done := t.spinDone
	spinning := t.spinning
	t.spinStop = nil
	t.spinDone = nil
	t.spinning = false
	t.mu.Unlock()
	if !spinning || stop == nil {
		return
	}
	close(stop)
	<-done
	t.mu.Lock()
	fmt.Fprint(t.writer, "\r\x1b[2K")
	t.mu.Unlock()
}

func (t *runTimeline) finishStep(mark, text string) {
	t.mu.Lock()
	started := t.stepStarted
	t.mu.Unlock()
	t.stopSpinner()
	if !started.IsZero() {
		text = text + "  " + t.style.dim("· "+formatDuration(time.Since(started)))
	}
	t.line(mark, text)
}

func (t *runTimeline) line(mark, text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.writer, "  %s %s\n", mark, text)
}

func (t *runTimeline) stepLabel(stepID string) string {
	if name, ok := t.stepNames[stepID]; ok && name != "" {
		return name
	}
	if stepID == "" {
		return "step"
	}
	return stepID
}

func (t *runTimeline) okRaw() string {
	if t.style.enabled {
		return "✓"
	}
	return "ok"
}

func (t *runTimeline) failRaw() string {
	if t.style.enabled {
		return "✗"
	}
	return "x"
}

func (t *runTimeline) waitRaw() string {
	if t.style.enabled {
		return "…"
	}
	return ".."
}

func (t *runTimeline) runRaw() string {
	if t.style.enabled {
		return "→"
	}
	return ">"
}

func (t *runTimeline) okMark() string   { return t.style.green(t.okRaw()) }
func (t *runTimeline) failMark() string { return t.style.red(t.failRaw()) }
func (t *runTimeline) waitMark() string { return t.style.yellow(t.waitRaw()) }
func (t *runTimeline) runMark() string  { return t.style.cyan(t.runRaw()) }

func shortRunID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		ms := d / time.Millisecond
		if ms < 1 {
			ms = 0
		}
		return fmt.Sprintf("%dms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		minutes := int(d.Minutes())
		seconds := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
}

// formatDurationLive is for the in-progress spinner: whole seconds only so the
// counter ticks cleanly (0s → 1s → 2s) without millisecond flicker.
func formatDurationLive(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", minutes, seconds)
}

func writeLocalRunSummary(writer io.Writer, result execution.Result, elapsed time.Duration) {
	if result.Run.ID == "" {
		return
	}
	style := newTermStyle(writer)
	status := strings.TrimSpace(result.Status)
	timing := ""
	if elapsed > 0 {
		timing = "  " + style.dim("· "+formatDuration(elapsed))
	}
	switch status {
	case "succeeded":
		fmt.Fprintf(writer, "  %s  %s%s\n", style.boldGreen(checkMark(style)), style.bold("Succeeded"), timing)
	case "failed":
		fmt.Fprintf(writer, "  %s  %s%s\n", style.boldRed(crossMark(style)), style.bold("Failed"), timing)
	case "interrupted":
		fmt.Fprintf(writer, "  %s  %s%s\n", style.boldYellow(ellipsisMark(style)), style.bold("Interrupted"), timing)
	case "cancelled":
		fmt.Fprintf(writer, "  %s  %s%s\n", style.boldYellow(ellipsisMark(style)), style.bold("Cancelled"), timing)
	default:
		fmt.Fprintf(writer, "  %s  %s%s\n", style.field("Status"), style.value(status), timing)
	}
	if result.StateDir != "" {
		fmt.Fprintf(writer, "  %s  %s\n", style.field("State"), style.filePath(runStateFile(result.StateDir)))
	}
	if len(result.Returns) == 0 && len(result.Evidence) == 0 {
		return
	}
	fmt.Fprintln(writer)
	if len(result.Returns) > 0 {
		fmt.Fprintf(writer, "  %s\n", style.bold("Returns"))
		writeNamedResultMap(writer, style, result.Returns)
	}
	if len(result.Evidence) > 0 {
		fmt.Fprintf(writer, "  %s\n", style.bold("Evidence"))
		writeNamedResultMap(writer, style, result.Evidence)
	}
}

func checkMark(style termStyle) string {
	if style.enabled {
		return "✓"
	}
	return "ok"
}

func crossMark(style termStyle) string {
	if style.enabled {
		return "✗"
	}
	return "x"
}

func ellipsisMark(style termStyle) string {
	if style.enabled {
		return "…"
	}
	return ".."
}

func shortSHA(value string) string {
	if len(value) <= 16 {
		return value
	}
	return value[:8] + "…" + value[len(value)-6:]
}

func formatByteSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

func writeNamedResultMap(writer io.Writer, style termStyle, values map[string]any) {
	names := make([]string, 0, len(values))
	width := 0
	for name := range values {
		names = append(names, name)
		if len(name) > width {
			width = len(name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		pad := strings.Repeat(" ", max(0, width-len(name)))
		switch value := values[name].(type) {
		case execution.ArtifactReference:
			path := value.Filename
			if value.LocalPath != "" {
				path = style.filePath(value.LocalPath)
			}
			fmt.Fprintf(writer, "    %s%s  %s\n", style.cyan(name), pad, path)
			fmt.Fprintf(writer, "    %s%s  %s · sha256 %s\n", strings.Repeat(" ", len(name)), pad,
				style.dim(formatByteSize(value.SizeBytes)), style.dim(shortSHA(value.SHA256)))
		default:
			fmt.Fprintf(writer, "    %s%s  %v\n", style.cyan(name), pad, value)
		}
	}
}
