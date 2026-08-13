package command

import (
	"fmt"
	"strings"
	"time"

	"doppels.so/cli/internal/runstate"
)

func runFollowDone(status string) bool {
	switch strings.TrimSpace(status) {
	case "succeeded", "failed", "cancelled", "interrupted", "pending_manual":
		return true
	default:
		return false
	}
}

func (app *App) followRunLogs(root, runID string) int {
	style := newTermStyle(app.Stdout)
	offsets := map[string]int{}
	headers := map[string]bool{}
	headerPrinted := false
	poll := app.Sleep
	if poll == nil {
		poll = time.Sleep
	}

	for {
		if err := app.context().Err(); err != nil {
			fmt.Fprintln(app.Stderr, "follow interrupted")
			return ExitInterrupted
		}
		detail, err := runstate.Load(root, runID)
		if err != nil {
			fmt.Fprintf(app.Stderr, "read local Run: %v\n", err)
			return ExitOperational
		}
		if !headerPrinted {
			fmt.Fprintln(app.Stdout)
			fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Run"), style.shortIDPrimary(detail.Summary.ID))
			writeRunStatusLine(app.Stdout, style, detail.Summary.Status)
			fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Cap"), detail.Summary.Capability)
			if detail.Summary.Recipe != "" {
				fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Recipe"), detail.Summary.Recipe)
			}
			if !runFollowDone(detail.Summary.Status) {
				fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Follow"), style.dim("waiting for step logs… (Ctrl+C to stop)"))
			}
			fmt.Fprintln(app.Stdout)
			headerPrinted = true
		}

		logs, err := runstate.Logs(root, runID)
		if err != nil {
			fmt.Fprintf(app.Stderr, "read local Run logs: %v\n", err)
			return ExitOperational
		}
		for _, log := range logs {
			key := log.Path
			if key == "" {
				key = log.StepID + "." + log.Stream
			}
			prev := offsets[key]
			content := log.Content
			if prev > len(content) {
				prev = 0
			}
			if prev == len(content) {
				continue
			}
			if !headers[key] {
				title := log.StepID
				if log.Stream != "" {
					title = log.StepID + " · " + log.Stream
				}
				fmt.Fprintf(app.Stdout, "  %s\n", style.bold(title))
				headers[key] = true
			}
			chunk := content[prev:]
			if strings.TrimSpace(chunk) == "" && prev == 0 {
				fmt.Fprintf(app.Stdout, "  %s\n", style.dim("(empty)"))
			} else {
				chunk = strings.TrimRight(chunk, "\n")
				for _, line := range strings.Split(chunk, "\n") {
					fmt.Fprintf(app.Stdout, "  %s\n", line)
				}
			}
			offsets[key] = len(content)
			fmt.Fprintln(app.Stdout)
		}

		if runFollowDone(detail.Summary.Status) {
			if len(logs) == 0 && len(offsets) == 0 {
				fmt.Fprintf(app.Stdout, "  %s\n", style.dim("No step logs recorded."))
			}
			writeRunStatusLine(app.Stdout, style, detail.Summary.Status)
			return ExitSuccess
		}
		poll(250 * time.Millisecond)
	}
}
