package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundtripMultipleMessages(t *testing.T) {
	var wire bytes.Buffer
	encoder := NewEncoder(&wire)
	if err := encoder.WriteFrame(map[string]any{"jsonrpc": "2.0", "method": "a"}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.WriteFrame(map[string]any{"jsonrpc": "2.0", "method": "b"}); err != nil {
		t.Fatal(err)
	}
	if got := wire.String(); !strings.HasSuffix(got, "\n") || strings.Count(got, "\n") != 2 {
		t.Fatalf("wire = %q", got)
	}

	decoder := NewDecoder(&wire)
	var methods []string
	for {
		frame, err := decoder.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		message, protoErr := DecodeMessage(frame)
		if protoErr != nil {
			t.Fatal(protoErr)
		}
		methods = append(methods, message.Method)
	}
	if strings.Join(methods, ",") != "a,b" {
		t.Fatalf("methods = %v", methods)
	}
}

func TestFrameRejectsOversizedLine(t *testing.T) {
	huge := "{" + strings.Repeat("x", MaxFrameBytes) + "}"
	decoder := NewDecoder(strings.NewReader(huge + "\n"))
	if _, err := decoder.ReadFrame(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestFrameRejectsOversizedLineWithoutNewline(t *testing.T) {
	// A line can be long and unterminated; the decoder must still flag it
	// instead of blocking forever or silently truncating.
	huge := strings.Repeat("x", MaxFrameBytes+1)
	decoder := NewDecoder(strings.NewReader(huge))
	if _, err := decoder.ReadFrame(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestFrameAcceptsLineAtExactLimit(t *testing.T) {
	line := strings.Repeat("x", MaxFrameBytes)
	decoder := NewDecoder(strings.NewReader(line + "\n"))
	frame, err := decoder.ReadFrame()
	if err != nil {
		t.Fatalf("frame at exactly 4 MiB must be accepted: %v", err)
	}
	if len(frame) != MaxFrameBytes {
		t.Fatalf("len = %d", len(frame))
	}
}

func TestEncoderRejectsFrameThatExceedsWireLimit(t *testing.T) {
	var wire bytes.Buffer
	if err := NewEncoder(&wire).WriteFrame(strings.Repeat("x", MaxFrameBytes)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

// TestFrameLimitIsSymmetric pins that MaxFrameBytes means the same thing in
// both directions: a payload the decoder accepts must be writable, otherwise
// two Runner processes could disagree about what is a legal frame.
func TestFrameLimitIsSymmetric(t *testing.T) {
	payload := json.RawMessage(`"` + strings.Repeat("x", MaxFrameBytes-2) + `"`)
	if length := len(payload); length != MaxFrameBytes {
		t.Fatalf("payload = %d bytes, want %d", length, MaxFrameBytes)
	}
	if _, err := NewDecoder(bytes.NewReader(append(payload, '\n'))).ReadFrame(); err != nil {
		t.Fatalf("decoder rejected a frame of exactly MaxFrameBytes: %v", err)
	}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).WriteFrame(payload); err != nil {
		t.Fatalf("encoder rejected a frame the decoder accepts: %v", err)
	}
	oversized := json.RawMessage(`"` + strings.Repeat("x", MaxFrameBytes-1) + `"`)
	if err := NewEncoder(&bytes.Buffer{}).WriteFrame(oversized); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	if _, err := NewDecoder(bytes.NewReader(append(oversized, '\n'))).ReadFrame(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}
