package execution

import (
	"bytes"
	"sort"
	"sync"
)

// redactedMarker is what replaces every secret occurrence, on disk and live.
var redactedMarker = []byte("[REDACTED]")

// normalizeSecrets returns the secret list both redaction paths agree on:
// blanks and duplicates removed, longest first, ties broken lexicographically.
// Determinism matters twice over — an unstable order would make two runs of
// the same output differ, and any divergence between the live stream and the
// file on disk is a leak.
func normalizeSecrets(secrets []string) []string {
	unique := make([]string, 0, len(secrets))
	seen := make(map[string]struct{}, len(secrets))
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if _, ok := seen[secret]; ok {
			continue
		}
		seen[secret] = struct{}{}
		unique = append(unique, secret)
	}
	sort.Slice(unique, func(i, j int) bool {
		if len(unique[i]) != len(unique[j]) {
			return len(unique[i]) > len(unique[j])
		}
		return unique[i] < unique[j]
	})
	return unique
}

// streamedRedactor is a stateful byte filter that replaces secret substrings
// with [REDACTED] while the subprocess is still writing.
//
// Contract (see Options.LogStream):
//   - Inputs are unredacted subprocess chunks (raw stdout / stderr bytes).
//   - Outputs are redacted chunks emitted incrementally: post-redact, pre-cap.
//   - The concatenation of everything emitted through feed+close equals
//     redact(fullRawOutput, secrets) byte-for-byte, so the live stream and
//     the redacted-on-disk file cannot diverge.
//   - Buffered bytes never exceed the sum of the secret lengths, so a Run
//     that prints megabytes streams instead of accumulating in memory.
//
// It is a pipeline of single-secret stages, one per secret in the same order
// the disk path applies them. Each stage is exactly bytes.ReplaceAll for its
// own secret, expressed incrementally, so chaining them reproduces the
// sequential passes of redact() by construction instead of by heuristic.
type streamedRedactor struct {
	mu     sync.Mutex
	stages []*secretStage
}

func newStreamedRedactor(secrets []string) *streamedRedactor {
	normalized := normalizeSecrets(secrets)
	stages := make([]*secretStage, 0, len(normalized))
	for _, secret := range normalized {
		stages = append(stages, &secretStage{secret: []byte(secret)})
	}
	return &streamedRedactor{stages: stages}
}

// log returns a LogFunc bound to stream that redacts each chunk with this
// redactor's secrets and forwards the redacted bytes to sink. Chunks for
// other streams pass through untouched. Called from the subprocess goroutine
// in executeStep.
func (r *streamedRedactor) log(stream LogStream, sink LogFunc) LogFunc {
	if sink == nil {
		return func(LogStream, []byte) {}
	}
	if len(r.stages) == 0 {
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

	data := chunk
	for _, stage := range r.stages {
		data = stage.feed(data)
	}
	return data
}

// close flushes every stage at end of step: each stage's residue cannot
// contain its own secret (it is shorter than it), but it still has to travel
// through the stages downstream, which may yet match on it.
func (r *streamedRedactor) close() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	var data []byte
	for _, stage := range r.stages {
		data = append(stage.feed(data), stage.flush()...)
	}
	return data
}

// buffered reports how many bytes are held back waiting for more input.
func (r *streamedRedactor) buffered() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	total := 0
	for _, stage := range r.stages {
		total += len(stage.tail)
	}
	return total
}

// secretStage is the incremental form of bytes.ReplaceAll for one secret:
// occurrences are matched left to right and never rescanned, and the only
// bytes held back are the last len(secret)-1, which are the ones a future
// chunk could complete into an occurrence.
type secretStage struct {
	secret []byte
	tail   []byte
}

func (s *secretStage) feed(chunk []byte) []byte {
	if len(chunk) == 0 && len(s.tail) == 0 {
		return nil
	}
	buf := append(append([]byte(nil), s.tail...), chunk...)
	var out []byte
	cursor := 0
	for {
		index := bytes.Index(buf[cursor:], s.secret)
		if index < 0 {
			break
		}
		out = append(out, buf[cursor:cursor+index]...)
		out = append(out, redactedMarker...)
		cursor += index + len(s.secret)
	}
	hold := len(s.secret) - 1
	if remaining := len(buf) - cursor; hold > remaining {
		hold = remaining
	}
	out = append(out, buf[cursor:len(buf)-hold]...)
	s.tail = append([]byte(nil), buf[len(buf)-hold:]...)
	return out
}

// flush returns the residue left after the last feed. It is shorter than the
// secret, so it cannot contain an occurrence of it.
func (s *secretStage) flush() []byte {
	tail := s.tail
	s.tail = nil
	return tail
}
