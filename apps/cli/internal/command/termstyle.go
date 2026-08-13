package command

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/mattn/go-isatty"
)

// fieldLabelWidth is the shared pad for human label columns (Run, Cap, Status…).
const fieldLabelWidth = 7

// termStyle wraps ANSI SGR sequences. Disabled for non-TTY writers, dumb
// terminals, and when NO_COLOR is set (https://no-color.org).
type termStyle struct {
	enabled bool
}

func newTermStyle(writer io.Writer) termStyle {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return termStyle{}
	}
	file, ok := writer.(*os.File)
	if !ok {
		return termStyle{}
	}
	fd := file.Fd()
	if !isatty.IsTerminal(fd) && !isatty.IsCygwinTerminal(fd) {
		return termStyle{}
	}
	return termStyle{enabled: true}
}

func (s termStyle) wrap(code, text string) string {
	if !s.enabled || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s termStyle) bold(text string) string   { return s.wrap("1", text) }
func (s termStyle) dim(text string) string    { return s.wrap("2", text) }
func (s termStyle) cyan(text string) string   { return s.wrap("36", text) }
func (s termStyle) green(text string) string  { return s.wrap("32", text) }
func (s termStyle) yellow(text string) string { return s.wrap("33", text) }
func (s termStyle) red(text string) string    { return s.wrap("31", text) }
func (s termStyle) boldCyan(text string) string {
	return s.wrap("1;36", text)
}
func (s termStyle) boldGreen(text string) string {
	return s.wrap("1;32", text)
}
func (s termStyle) boldRed(text string) string {
	return s.wrap("1;31", text)
}
func (s termStyle) boldYellow(text string) string {
	return s.wrap("1;33", text)
}

func (s termStyle) label(text string) string {
	return s.dim(text)
}

func (s termStyle) value(text string) string {
	return s.bold(text)
}

// field returns a dim label padded to fieldLabelWidth for aligned columns.
func (s termStyle) field(name string) string {
	return s.label(fmt.Sprintf("%-*s", fieldLabelWidth, name))
}

// shortIDPrimary renders a display-only short id, with the full id dim when longer.
func (s termStyle) shortIDPrimary(id string) string {
	short := shortRunID(id)
	if short == id || id == "" {
		return s.value(short)
	}
	return s.value(short) + "  " + s.dim(id)
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visibleLen(text string) int {
	return len([]rune(ansiEscape.ReplaceAllString(text, "")))
}

func padStatusLine(msg string, width int) string {
	pad := max(0, width-visibleLen(msg))
	if pad == 0 {
		return msg
	}
	return msg + strings.Repeat(" ", pad)
}

// writeAlignedColumns prints rows with spacing based on visible (ANSI-stripped)
// width so styled cells still line up under headers.
func writeAlignedColumns(writer io.Writer, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	colCount := 0
	for _, row := range rows {
		if len(row) > colCount {
			colCount = len(row)
		}
	}
	widths := make([]int, colCount)
	for _, row := range rows {
		for i, cell := range row {
			if n := visibleLen(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}
	const gap = 2
	for _, row := range rows {
		for i := 0; i < colCount; i++ {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			fmt.Fprint(writer, cell)
			if i == colCount-1 {
				continue
			}
			fmt.Fprint(writer, strings.Repeat(" ", widths[i]-visibleLen(cell)+gap))
		}
		fmt.Fprintln(writer)
	}
}
