package proto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// MaxFrameBytes is the hard cap per NDJSON line (RFC §4). The Runner never
// emits larger frames; log payloads paginate through getRunLogs instead.
const MaxFrameBytes = 4 << 20 // 4 MiB

// ErrFrameTooLarge marks a line exceeding MaxFrameBytes. Connections hit by
// it must close: there is no framing resynchronization.
var ErrFrameTooLarge = errors.New("frame exceeds 4 MiB")

// Encoder writes complete NDJSON frames; every Write is one message.
type Encoder struct {
	mu     sync.Mutex
	writer io.Writer
}

func NewEncoder(writer io.Writer) *Encoder { return &Encoder{writer: writer} }

// WriteFrame marshals value and writes it as a single \n-terminated line.
func (e *Encoder) WriteFrame(value any) error {
	encoded, err := marshalCompact(value)
	if err != nil {
		return err
	}
	if len(encoded)+1 > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.writer.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return nil
}

// Decoder reads complete NDJSON frames.
type Decoder struct {
	scanner *bufio.Scanner
}

func NewDecoder(reader io.Reader) *Decoder {
	decoder := &Decoder{scanner: bufio.NewScanner(reader)}
	decoder.scanner.Buffer(make([]byte, 0, 64<<10), MaxFrameBytes+1)
	return decoder
}

// ReadFrame returns the raw bytes of the next line (without \n). It returns
// io.EOF on clean close and ErrFrameTooLarge for oversized lines.
func (d *Decoder) ReadFrame() ([]byte, error) {
	if !d.scanner.Scan() {
		if err := d.scanner.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) {
				return nil, fmt.Errorf("%w", ErrFrameTooLarge)
			}
			return nil, err
		}
		return nil, io.EOF
	}
	line := d.scanner.Bytes()
	if len(line) > MaxFrameBytes {
		return nil, fmt.Errorf("%w", ErrFrameTooLarge)
	}
	// Detach from the scanner's reused buffer.
	return append([]byte(nil), line...), nil
}

func marshalCompact(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	// json.Encoder.Encode already appends '\n'.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
