package command

import (
	"fmt"
	"io"
	"strings"

	"doppels.so/cli/internal/runstate"
)

const defaultRunListLimit = 20

func displayRefName(ref string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(ref), "@")
	return name
}

func colorRunStatus(style termStyle, status string) string {
	switch strings.TrimSpace(status) {
	case "succeeded":
		return style.green(status)
	case "failed":
		return style.red(status)
	case "running":
		return style.cyan(status)
	case "interrupted", "cancelled", "pending_manual":
		return style.yellow(status)
	default:
		return status
	}
}

func allSourcesLocal(items []runstate.Summary) bool {
	for _, item := range items {
		if item.Source != "" && item.Source != "local" {
			return false
		}
	}
	return true
}

func writeRunList(writer io.Writer, items []runstate.Summary, total, limit int, all bool, nowFmt func(created string) string) {
	style := newTermStyle(writer)
	showSource := !allSourcesLocal(items)
	header := []string{style.dim("ID"), style.dim("STATUS")}
	if showSource {
		header = append(header, style.dim("SOURCE"))
	}
	header = append(header, style.dim("CAPABILITY"), style.dim("RECIPE"), style.dim("CREATED"))
	rows := [][]string{header}
	for _, item := range items {
		row := []string{shortRunID(item.ID), colorRunStatus(style, item.Status)}
		if showSource {
			row = append(row, item.Source)
		}
		row = append(row, displayRefName(item.Capability), displayRefName(item.Recipe), nowFmt(item.CreatedAt))
		rows = append(rows, row)
	}
	writeAlignedColumns(writer, rows)
	if total == 0 {
		return
	}
	shown := len(items)
	if shown >= total && (all || limit <= 0 || total <= limit) {
		if total > defaultRunListLimit {
			fmt.Fprintf(writer, "\n%s\n", style.dim(fmt.Sprintf("showing %d of %d", shown, total)))
		}
		return
	}
	hint := fmt.Sprintf("showing %d of %d  ·  doppels runs list --limit %d  ·  --all", shown, total, nextRunListLimit(limit, total))
	fmt.Fprintf(writer, "\n%s\n", style.dim(hint))
}

func nextRunListLimit(limit, total int) int {
	if limit <= 0 {
		limit = defaultRunListLimit
	}
	next := limit * 2
	if next < total && next < 100 {
		return next
	}
	if total > limit {
		return total
	}
	return limit
}

func resolveRunID(root, raw string) (string, error) {
	id := trimRunResource(strings.TrimSpace(raw))
	if id == "" {
		return "", fmt.Errorf("empty Run id")
	}
	items, err := runstate.List(root)
	if err != nil {
		return "", err
	}
	var exact, prefixes []string
	for _, item := range items {
		if item.ID == id {
			exact = append(exact, item.ID)
			continue
		}
		if strings.HasPrefix(item.ID, id) {
			prefixes = append(prefixes, item.ID)
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0], nil
	case len(exact) > 1:
		return "", fmt.Errorf("Run id %q matched %d indexed Runs", id, len(exact))
	case len(prefixes) == 1:
		return prefixes[0], nil
	case len(prefixes) > 1:
		return "", fmt.Errorf("Run id prefix %q is ambiguous (%d matches); use a longer prefix", id, len(prefixes))
	default:
		return id, nil
	}
}
