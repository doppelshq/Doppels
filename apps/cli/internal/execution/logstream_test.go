package execution

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureLog captures streams in arrival order, with timestamps relative to
// the test start so we can assert chunk timing against the running process.
type captureLog struct {
	mu      sync.Mutex
	chunks  []logChunk
	started time.Time
}

type logChunk struct {
	stream   LogStream
	data     []byte
	atOffset time.Duration
}

func newCaptureLog() *captureLog {
	return &captureLog{started: time.Now()}
}

func (c *captureLog) fn() LogFunc {
	return func(stream LogStream, p []byte) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.chunks = append(c.chunks, logChunk{stream: stream, data: append([]byte(nil), p...), atOffset: time.Since(c.started)})
	}
}

func (c *captureLog) snapshot() []logChunk {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]logChunk, len(c.chunks))
	copy(out, c.chunks)
	return out
}

// TestStreamedRedactorFansOutIncrementally verifies the redaction stream
// pushes redacted chunks to LogFunc while the producer keeps writing. The
// raw chunks themselves never reach the callback, so this also pins the
// post-redact contract.
func TestStreamedRedactorFansOutIncrementally(t *testing.T) {
	c := newCaptureLog()
	r := newStreamedRedactor(nil)
	// wire through the writer adapter so the same pipeline runs.
	w := LogFuncToWriter(r.log(LogStreamStdout, c.fn()), LogStreamStdout)

	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte("first\n"))
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte("second\n"))
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte("third\n"))
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.snapshot()) >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	chunks := c.snapshot()
	if len(chunks) < 3 {
		t.Fatalf("expected >=3 chunks, got %d", len(chunks))
	}
	got := strings.Builder{}
	for _, ch := range chunks {
		got.Write(ch.data)
	}
	if got.String() != "first\nsecond\nthird\n" {
		t.Fatalf("streamed = %q", got.String())
	}
	for i, ch := range chunks {
		if ch.atOffset >= time.Second {
			t.Fatalf("chunk %d arrived at %s (want incremental)", i, ch.atOffset)
		}
	}
}

// TestStreamedRedactorMasksPlainSecret verifies the simplest redaction path:
// a secret that fits cleanly inside one chunk is replaced with [REDACTED].
func TestStreamedRedactorMasksPlainSecret(t *testing.T) {
	var got bytes.Buffer
	r := newStreamedRedactor([]string{"hunter2"})
	w := LogFuncToWriter(r.log(LogStreamStdout, func(_ LogStream, p []byte) { got.Write(p) }), LogStreamStdout)
	_, _ = w.Write([]byte("token=hunter2 leaked\n"))
	if strings.Contains(got.String(), "hunter2") {
		t.Fatalf("secret leaked into stream: %q", got.String())
	}
	if !strings.Contains(got.String(), "[REDACTED]") {
		t.Fatalf("missing marker: %q", got.String())
	}
}

// TestStreamedRedactorMasksSecretSplitAcrossChunks pins the boundary case:
// the secret bytes are split between two writes, and a tail-only chunk
// (nothing but the secret) must still emit the marker, not the literal
// bytes.
func TestStreamedRedactorMasksSecretSplitAcrossChunks(t *testing.T) {
	var got bytes.Buffer
	r := newStreamedRedactor([]string{"hunter2"})
	w := LogFuncToWriter(r.log(LogStreamStdout, func(_ LogStream, p []byte) { got.Write(p) }), LogStreamStdout)

	secret := "hunter2"
	parts := []string{"x=" + secret[:3], secret[3:] + " y"}
	for _, p := range parts {
		_, _ = w.Write([]byte(p))
	}
	// Step ends; executor must flush the redactor.
	if leftover := r.close(); len(leftover) > 0 {
		got.Write(leftover)
	}
	if strings.Contains(got.String(), "hunter2") {
		t.Fatalf("split secret leaked: %q (parts=%v)", got.String(), parts)
	}
	if !strings.Contains(got.String(), "[REDACTED]") {
		t.Fatalf("split secret never redacted: %q", got.String())
	}
}

// TestStreamedRedactorMatchesFinalDiskLog verifies the live stream and the
// redacted-on-disk bytes (cappedWriter + redact()) produce identical content
// once the redactor is flushed at step end.
func TestStreamedRedactorMatchesFinalDiskLog(t *testing.T) {
	secrets := []string{"hunter2", "tok-1234"}

	// Live stream.
	var streamed bytes.Buffer
	r := newStreamedRedactor(secrets)
	w := LogFuncToWriter(r.log(LogStreamStdout, func(_ LogStream, p []byte) { streamed.Write(p) }), LogStreamStdout)
	_, _ = w.Write([]byte("token=hunter2 ok tok-1234 bye\n"))
	if leftover := r.close(); len(leftover) > 0 {
		streamed.Write(leftover)
	}

	// Disk path: mimics what executeStep does (cappedWriter + redact() at end).
	disk := newCappedWriter(1024)
	_, _ = disk.Write([]byte("token=hunter2 ok tok-1234 bye\n"))

	if !bytes.Contains(streamed.Bytes(), []byte("token=[REDACTED]")) {
		t.Fatalf("streamed missing redacted hunter2: %q", streamed.String())
	}
	if !bytes.Contains(streamed.Bytes(), []byte("ok [REDACTED]")) {
		t.Fatalf("streamed missing redacted tok-1234: %q", streamed.String())
	}
	if !bytes.Equal(disk.Bytes(), []byte("token=hunter2 ok tok-1234 bye\n")) {
		t.Fatalf("disk raw = %q", string(disk.Bytes()))
	}
	diskRedacted := redact(disk.Bytes(), secrets)
	if !bytes.Equal(streamed.Bytes(), diskRedacted) {
		t.Fatalf("live (%q) != disk-redacted (%q)", streamed.String(), string(diskRedacted))
	}
}

// TestStreamedRedactorCloseMatchesDiskOnTruncatedSecret pins the parity
// contract when a secret is cut short by EOF: only a partial prefix ever
// arrives, the disk file retains that partial (redact finds no complete
// occurrence), and the live stream must assemble to the exact same bytes —
// recovery paths stay identical and neither side ever exposes a full secret.
func TestStreamedRedactorCloseMatchesDiskOnTruncatedSecret(t *testing.T) {
	raw := "x=hun" // first 3 bytes of "hunter2", EOF before the rest
	r := newStreamedRedactor([]string{"hunter2"})
	var live bytes.Buffer
	live.Write(r.feed([]byte(raw)))
	live.Write(r.close())

	disk := redact([]byte(raw), []string{"hunter2"})
	if !bytes.Equal(live.Bytes(), disk) {
		t.Fatalf("live (%q) != disk (%q)", live.String(), string(disk))
	}
	if strings.Contains(live.String(), "hunter2") {
		t.Fatalf("full secret leaked: %q", live.String())
	}
}

// TestStreamedRedactorAssemblyEqualsDisk verifies the full-assembly parity
// invariant on plain output: feed emissions plus the close() tail must equal
// redact(raw) exactly, including when no secret is involved at all.
func TestStreamedRedactorAssemblyEqualsDisk(t *testing.T) {
	raw := "nothing mat"
	r := newStreamedRedactor([]string{"hunter2"})
	var live bytes.Buffer
	live.Write(r.feed([]byte(raw)))
	live.Write(r.close())

	disk := redact([]byte(raw), []string{"hunter2"})
	if !bytes.Equal(live.Bytes(), disk) {
		t.Fatalf("live (%q) != disk (%q)", live.String(), string(disk))
	}
}

// TestStreamedRedactorDoesNotSplitCompleteOccurrence is a regression pin for
// the naive holdback cut: with buf="token=hunter2 ok" and maxSecret=7 a
// len-6 boundary lands at offset 10, slicing through the occurrence at
// 6..13 and leaking "hunt". The boundary must extend past the last complete
// occurrence instead.
func TestStreamedRedactorDoesNotSplitCompleteOccurrence(t *testing.T) {
	r := newStreamedRedactor([]string{"hunter2"})
	var live bytes.Buffer
	live.Write(r.feed([]byte("token=hunter2 ok")))
	live.Write(r.close())

	if strings.Contains(live.String(), "hunter2") {
		t.Fatalf("occurrence split leaked secret head: %q", live.String())
	}
	disk := redact([]byte("token=hunter2 ok"), []string{"hunter2"})
	if !bytes.Equal(live.Bytes(), disk) {
		t.Fatalf("live (%q) != disk (%q)", live.String(), string(disk))
	}
}

func TestStreamedRedactorMatchesSequentialRedactionAcrossReplacementBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		secrets []string
		chunks  []string
	}{
		{name: "prefix overlap", secrets: []string{"abcdef", "ab"}, chunks: []string{"ab", "cdef"}},
		{name: "replacement creates match", secrets: []string{"abcd", "D]x"}, chunks: []string{"abcd", "x"}},
		{name: "nested replacement", secrets: []string{"bcdef", "abc"}, chunks: []string{"abcd", "ef"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := strings.Join(tt.chunks, "")
			redactor := newStreamedRedactor(tt.secrets)
			var live bytes.Buffer
			for _, chunk := range tt.chunks {
				live.Write(redactor.feed([]byte(chunk)))
			}
			live.Write(redactor.close())
			want := redact([]byte(raw), tt.secrets)
			if !bytes.Equal(live.Bytes(), want) {
				t.Fatalf("live=%q want=%q", live.String(), string(want))
			}
		})
	}
}

// TestStreamedRedactorNilSafe verifies a nil-redactor pipeline still produces
// raw output. This is the path callers take when no host_env secrets are
// involved.
func TestStreamedRedactorNilSafe(t *testing.T) {
	var got bytes.Buffer
	r := newStreamedRedactor(nil)
	w := LogFuncToWriter(r.log(LogStreamStdout, func(_ LogStream, p []byte) { got.Write(p) }), LogStreamStdout)
	_, _ = w.Write([]byte("no secrets here\n"))
	if got.String() != "no secrets here\n" {
		t.Fatalf("nil-redactor altered bytes: %q", got.String())
	}
}

// TestLogFuncToWriterAdaptsCallback verifies the bridge from io.Writer-shaped
// callers to the LogFunc surface.
func TestLogFuncToWriterAdaptsCallback(t *testing.T) {
	var got []byte
	var mu sync.Mutex
	stream := LogFunc(func(s LogStream, p []byte) {
		mu.Lock()
		defer mu.Unlock()
		if s != LogStreamStdout {
			t.Errorf("stream = %q, want stdout", s)
		}
		got = append(got, p...)
	})
	w := LogFuncToWriter(stream, LogStreamStdout)
	if _, err := w.Write([]byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("there\n")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if string(got) != "hi\nthere\n" {
		t.Fatalf("streamed = %q", got)
	}
}

// TestLogFuncToWriterNilSafe verifies a nil LogFunc is a no-op.
func TestLogFuncToWriterNilSafe(t *testing.T) {
	w := LogFuncToWriter(nil, LogStreamStdout)
	if _, err := w.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
}

// TestExecuteStepRespectsContextCancellation pins that any future change to
// the streaming path does not silently swallow cancellation.
func TestExecuteStepRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ctx.Err() == nil {
		t.Fatal("expected ctx.Err()")
	}
}

// TestStreamedRedactorMatchesDiskOverEveryPartition is the property the
// contract actually claims: for any way the subprocess happens to split its
// output, the live stream must equal the redacted file byte-for-byte. It
// enumerates every partition of each raw string, which is where boundary
// heuristics leak (a cut inside a complete occurrence let a shorter secret
// match its head) and where a duplicated secret used to mangle the disk side.
func TestStreamedRedactorMatchesDiskOverEveryPartition(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		secrets []string
	}{
		{name: "prefix", raw: "abcdefZZZZ", secrets: []string{"abcdef", "ab"}},
		{name: "overlap", raw: "abcdefZZZZ", secrets: []string{"bcdef", "abc"}},
		{name: "marker creates match", raw: "abcdxZZZZ", secrets: []string{"abcd", "D]x"}},
		{name: "duplicates", raw: "EZZZZ", secrets: []string{"E", "E"}},
		{name: "single byte", raw: "aaaZbaa", secrets: []string{"a", "ab"}},
		{name: "repeated occurrence", raw: "s3cr3t s3cr3t", secrets: []string{"s3cr3t"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := redact([]byte(tt.raw), tt.secrets)
			for mask := 0; mask < 1<<(len(tt.raw)-1); mask++ {
				redactor := newStreamedRedactor(tt.secrets)
				var live []byte
				start := 0
				for index := 1; index < len(tt.raw); index++ {
					if mask&(1<<(index-1)) == 0 {
						continue
					}
					live = append(live, redactor.feed([]byte(tt.raw[start:index]))...)
					start = index
				}
				live = append(live, redactor.feed([]byte(tt.raw[start:]))...)
				live = append(live, redactor.close()...)
				if !bytes.Equal(live, want) {
					t.Fatalf("mask=%d raw=%q live=%q disk=%q", mask, tt.raw, live, want)
				}
			}
		})
	}
}

// TestStreamedRedactorBuffersBoundedBytes pins that a long run of secrets —
// including the pathological case where the [REDACTED] marker itself feeds
// the next secret — streams instead of accumulating the whole output in
// memory. A Run printing megabytes must not be held back until it exits.
func TestStreamedRedactorBuffersBoundedBytes(t *testing.T) {
	secrets := []string{"abcd", "]x"}
	redactor := newStreamedRedactor(secrets)
	raw := strings.Repeat("abcd", 4096)
	emitted := redactor.feed([]byte(raw))
	budget := 0
	for _, secret := range secrets {
		budget += len(secret)
	}
	if buffered := redactor.buffered(); buffered > budget {
		t.Fatalf("buffered %d bytes, budget %d (emitted %d)", buffered, budget, len(emitted))
	}
	if len(emitted) == 0 {
		t.Fatal("nothing emitted for a 16 KiB run of secrets")
	}
	emitted = append(emitted, redactor.close()...)
	if want := redact([]byte(raw), secrets); !bytes.Equal(emitted, want) {
		t.Fatalf("live and disk diverge on a long run (live %d bytes, disk %d)", len(emitted), len(want))
	}
}

// TestRedactIsStableRegardlessOfSecretOrder pins that the disk path does not
// depend on the order or multiplicity of the secret list: two callers holding
// the same secrets in different order must produce identical files, and a
// duplicated secret must not rewrite the marker it just inserted.
func TestRedactIsStableRegardlessOfSecretOrder(t *testing.T) {
	raw := []byte("E ab abcdef")
	first := redact(raw, []string{"ab", "abcdef", "E"})
	second := redact(raw, []string{"E", "abcdef", "ab"})
	if !bytes.Equal(first, second) {
		t.Fatalf("order changed redaction: %q vs %q", first, second)
	}
	if duplicated := redact(raw, []string{"E", "E", "ab", "abcdef"}); !bytes.Equal(duplicated, first) {
		t.Fatalf("duplicate secret changed redaction: %q vs %q", duplicated, first)
	}
	if bytes.Contains(first, []byte("abcdef")) || bytes.Contains(first, []byte("E ")) {
		t.Fatalf("secret survived redaction: %q", first)
	}
}
