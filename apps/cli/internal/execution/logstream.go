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

// close flushes the buffered tail at end of step using the same sequential
// replacement pass as the disk path. The conservative carry policy may retain
// a complete occurrence when a replacement marker can interact with future
// bytes, so emitting the tail literally would diverge and leak it.
func (r *streamedRedactor) close() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	carry := r.tail
	r.tail = nil
	return redact(carry, r.secrets)
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
	// A replacement can itself end with a prefix of another secret. Such a
	// marker must stay in carry until the future bytes are known; otherwise
	// sequential ReplaceAll over the complete output can redact across the
	// chunk boundary and live output diverges from disk. Walk backwards to the
	// largest safe raw boundary. This is intentionally conservative for
	// pathological marker/secret overlaps, but preserves exact semantics.
	for ; boundary > 0; boundary-- {
		candidate := redact(buf[:boundary], secrets)
		if !endsWithSecretPrefix(candidate, secrets) {
			out = candidate
			break
		}
	}
	if boundary == 0 {
		out = nil
	}
	return out, append([]byte(nil), buf[boundary:]...)
}

func endsWithSecretPrefix(buf []byte, secrets []string) bool {
	for _, secret := range secrets {
		for n := 1; n < len(secret) && n <= len(buf); n++ {
			if string(buf[len(buf)-n:]) == secret[:n] {
				return true
			}
		}
	}
	return false
}
