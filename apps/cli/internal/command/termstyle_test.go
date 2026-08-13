package command

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteAlignedColumnsIgnoresANSIWidth(t *testing.T) {
	style := termStyle{enabled: true}
	var buf bytes.Buffer
	writeAlignedColumns(&buf, [][]string{
		{" ", style.dim("NAME"), style.dim("DISPLAY")},
		{style.boldCyan("*"), style.bold("local"), "Local"},
		{" ", "acme", "Acme Corp"},
		{" ", "personal", "Personal"},
	})
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d, output = %q", len(lines), buf.String())
	}
	// DISPLAY column should start at the same visible offset on every row.
	displayAt := visibleLen(lines[0]) - visibleLen("DISPLAY")
	for i, line := range lines {
		cells := []string{"DISPLAY", "Local", "Acme Corp", "Personal"}
		want := cells[i]
		gotAt := strings.LastIndex(ansiEscape.ReplaceAllString(line, ""), want)
		if gotAt != displayAt {
			t.Fatalf("row %d display col at %d, want %d\nplain=%q\nraw=%q",
				i, gotAt, displayAt, ansiEscape.ReplaceAllString(line, ""), line)
		}
	}
}
