package proto

import (
	"bytes"
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
