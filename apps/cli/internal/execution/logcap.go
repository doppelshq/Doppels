package execution

import (
	"bytes"
	"fmt"
	"io"
)

// DefaultLogStreamLimit is the max retained bytes per Step stream (stdout or
// stderr). Excess output is discarded after the process writes it so pipes
// never block; stored logs get a truncation marker.
const DefaultLogStreamLimit = 16 << 20 // 16 MiB

const truncatedMarkerFmt = "\n[doppels: truncated after %s]\n"

// cappedWriter keeps at most limit bytes while always accepting Write so the
// child process is never blocked on a full pipe buffer.
type cappedWriter struct {
	limit     int
	n         int
	truncated bool
	buf       bytes.Buffer
}

func newCappedWriter(limit int) *cappedWriter {
	if limit <= 0 {
		limit = DefaultLogStreamLimit
	}
	return &cappedWriter{limit: limit}
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	remaining := w.limit - w.n
	if remaining > 0 {
		take := len(p)
		if take > remaining {
			take = remaining
			w.truncated = true
		}
		if take > 0 {
			_, _ = w.buf.Write(p[:take])
			w.n += take
		}
	} else {
		w.truncated = true
	}
	return len(p), nil
}

func (w *cappedWriter) Bytes() []byte { return w.buf.Bytes() }

func (w *cappedWriter) Truncated() bool { return w.truncated }

// teeWriter fans writes out to a disk writer (cappedWriter) and an optional
// live-stream writer (post-redaction). Both writers must accept the full
// input even when the disk stream truncates so neither blocks the subprocess
// pipe; the live stream is bounded only by the subprocess output rate, which
// the cappedWriter already throttles via the read end of the pipe.
//
// Used only when Options.LogStream is set; absent that option, the
// subprocess writes directly to the cappedWriter.
type teeWriter struct {
	disk io.Writer
	live io.Writer
}

func (t teeWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if t.disk != nil {
		if _, err := t.disk.Write(p); err != nil {
			return 0, err
		}
	}
	if t.live != nil {
		if _, err := t.live.Write(p); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// LogFuncToWriter adapts a LogFunc into an io.Writer for callers that still
// speak io.Writer (TTY, transports). Nil fn yields a no-op writer.
func LogFuncToWriter(fn LogFunc, stream LogStream) io.Writer {
	if fn == nil {
		return nopWriter{}
	}
	return logFuncWriter{fn: fn, stream: stream}
}

type logFuncWriter struct {
	fn     LogFunc
	stream LogStream
}

func (w logFuncWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.fn(w.stream, p)
	return len(p), nil
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func finalizeLogBytes(data []byte, truncated bool, limit int) []byte {
	if !truncated {
		return data
	}
	if limit <= 0 {
		limit = DefaultLogStreamLimit
	}
	marker := fmt.Sprintf(truncatedMarkerFmt, formatByteCount(limit))
	out := make([]byte, 0, len(data)+len(marker))
	out = append(out, data...)
	out = append(out, marker...)
	return out
}

func formatByteCount(n int) string {
	const mib = 1 << 20
	if n >= mib && n%mib == 0 {
		return fmt.Sprintf("%dMiB", n/mib)
	}
	if n >= mib {
		return fmt.Sprintf("%.1fMiB", float64(n)/float64(mib))
	}
	const kib = 1 << 10
	if n >= kib && n%kib == 0 {
		return fmt.Sprintf("%dKiB", n/kib)
	}
	return fmt.Sprintf("%dB", n)
}
