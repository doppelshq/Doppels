package execution

import (
	"sort"
	"sync"
)

// streamedRedactor is a stateful byte filter that replaces secret substrings
// with [REDACTED] while the subprocess is still writing.
//
// Contract (see Options.LogStream):
//   - Inputs are unredacted subprocess chunks (raw stdout / stderr bytes).
//   - Outputs are redacted chunks emitted incrementally: post-redact, pre-cap.
//   - The redactor buffers trailing bytes across calls so a secret split
//     between two chunks is still masked.
//   - The concatenation of everything emitted through feed+close equals
//     redact(fullRawOutput, secrets) byte-for-byte, so the live stream and
//     the redacted-on-disk file cannot diverge.
type streamedRedactor struct {
	mu      sync.Mutex
	secrets []string // unique, sorted longest-first
	tail    []byte   // unemitted bytes that may start a split secret
}

func newStreamedRedactor(secrets []string) *streamedRedactor {
	uniq := make([]string, 0, len(secrets))
	seen := make(map[string]struct{}, len(secrets))
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		uniq = append(uniq, s)
	}
	// Longest first: prevents a shorter secret replacing the inside of a
	// longer one and bounds the split-secret tail buffer.
	sort.Slice(uniq, func(i, j int) bool { return len(uniq[i]) > len(uniq[j]) })
	return &streamedRedactor{secrets: uniq}
}

// log returns a LogFunc bound to stream that redacts each chunk with this
// redactor's secrets and forwards the redacted bytes to sink. Chunks for
// other streams pass through untouched. Called from the subprocess goroutine
// in executeStep.
func (r *streamedRedactor) log(stream LogStream, sink LogFunc) LogFunc {
	if sink == nil {
		return func(LogStream, []byte) {}
	}
	if len(r.secrets) == 0 {
		return sink
	}
	return func(s LogStream, chunk []byte) {
		if s != stream {
			sink(s, chunk)
			return
		}
		if out := r.feed(chunk); len(out) > 0 {
			sink(s, out)
		}
	}
}

// feed runs one chunk through the redactor. Single-goroutine contract (the
// subprocess goroutine); the mutex only future-proofs parallel producers.
func (r *streamedRedactor) feed(chunk []byte) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.secrets) == 0 {
		return chunk
	}

	combined := append(append([]byte(nil), r.tail...), chunk...)
	r.tail = nil

	out, carry := redactWithCarry(combined, r.secrets)
	r.tail = carry
	return out
}

// close flushes the buffered tail at end of step and returns the final bytes
// to emit. The tail is emitted literally: a complete secret can never sit in
// the tail (redactWithCarry always emits past the end of the last complete
// occurrence), and a partial prefix truncated at EOF must reach the live
// stream exactly like it reaches the redacted disk file, keeping both
// recovery paths identical.
func (r *streamedRedactor) close() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	carry := r.tail
	r.tail = nil
	return carry
}

// redactWithCarry redacts buf and splits it into (out, carry). carry holds
// the trailing bytes that could be the start of a secret split across chunk
// boundaries; the caller must prepend them to the next chunk.
//
// The emit boundary is chosen so no complete occurrence is ever split: a
// naive len(buf)-(maxSecret-1) cut can slice through a secret that fits
// entirely within the buffer and leak its head bytes, so the boundary also
// extends past the end of the last complete occurrence when needed.
func redactWithCarry(buf []byte, secrets []string) (out []byte, carry []byte) {
	maxSecret := len(secrets[0])
	boundary := len(buf) - (maxSecret - 1)
	if boundary < 0 {
		boundary = 0
	}
	if lastEnd := lastOccurrenceEnd(buf, secrets); lastEnd > boundary {
		boundary = lastEnd
	}
	// Same replacement semantics as redact(): sequential ReplaceAll,
	// longest-first. redact() re-sorts a copy, which is a no-op here.
	out = redact(buf[:boundary], secrets)
	return out, append([]byte(nil), buf[boundary:]...)
}

// lastOccurrenceEnd scans buf left-to-right, preferring the longest secret
// at each position (secrets must be sorted longest-first), and returns the
// end offset of the last complete occurrence found, or -1 when none match.
func lastOccurrenceEnd(buf []byte, secrets []string) int {
	lastEnd := -1
	for i := 0; i < len(buf); {
		matched := 0
		for _, s := range secrets {
			n := len(s)
			if n == 0 || n > len(buf)-i {
				continue
			}
			if string(buf[i:i+n]) == s {
				matched = n
				break
			}
		}
		if matched > 0 {
			lastEnd = i + matched
			i += matched
			continue
		}
		i++
	}
	return lastEnd
}
