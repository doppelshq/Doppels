package command

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
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

func absolutePath(path string) string {
	cleaned := filepath.Clean(path)
	if abs, err := filepath.Abs(cleaned); err == nil {
		return abs
	}
	return cleaned
}

func runStateFile(stateDir string) string {
	if strings.TrimSpace(stateDir) == "" {
		return ""
	}
	return filepath.Join(absolutePath(stateDir), "run.json")
}

func fileLinkLabel(abs string) string {
	return filepath.Base(abs)
}

// filePath renders a filesystem file. TTY: short label + file:// hyperlink
// (BEL-terminated OSC 8). Directories are not openable in the Cursor terminal.
func (s termStyle) filePath(path string) string {
	abs := absolutePath(path)
	if !s.enabled {
		return abs
	}
	return s.fileLink(abs, fileLinkLabel(abs))
}

func (s termStyle) fileLink(path, label string) string {
	abs := absolutePath(path)
	if strings.TrimSpace(label) == "" {
		label = fileLinkLabel(abs)
	}
	if !s.enabled {
		return label
	}
	link := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return "\x1b]8;;" + link.String() + "\x07" + label + "\x1b]8;;\x07"
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
var osc8Link = regexp.MustCompile(`\x1b\]8;;[^\x07]*\x07`)

func visibleLen(text string) int {
	stripped := ansiEscape.ReplaceAllString(text, "")
	stripped = osc8Link.ReplaceAllString(stripped, "")
	stripped = strings.ReplaceAll(stripped, "\x1b]8;;\x07", "")
	return len([]rune(stripped))
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

type prefixWriter struct {
	w      io.Writer
	prefix string
	bol    bool
}

func prefixLines(w io.Writer, prefix string) io.Writer {
	return &prefixWriter{w: w, prefix: prefix, bol: true}
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	out := make([]byte, 0, len(b)+len(p.prefix))
	for _, c := range b {
		if p.bol {
			out = append(out, p.prefix...)
			p.bol = false
		}
		out = append(out, c)
		if c == '\n' {
			p.bol = true
		}
	}
	if _, err := p.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}
