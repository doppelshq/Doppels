package execution

import (
	"bytes"
	"strings"
	"testing"
)

func TestCappedWriterRetainsPrefixAndAcceptsAll(t *testing.T) {
	w := newCappedWriter(8)
	n, err := w.Write([]byte("abcdefghijklmnop"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 16 {
		t.Fatalf("Write n = %d, want 16", n)
	}
	if !w.Truncated() {
		t.Fatal("expected truncated")
	}
	if got := string(w.Bytes()); got != "abcdefgh" {
		t.Fatalf("Bytes = %q", got)
	}
	final := finalizeLogBytes(w.Bytes(), w.Truncated(), 8)
	if !strings.Contains(string(final), "[doppels: truncated after 8B]") {
		t.Fatalf("marker missing: %q", final)
	}
	if !bytes.HasPrefix(final, []byte("abcdefgh")) {
		t.Fatalf("prefix lost: %q", final)
	}
}

func TestCappedWriterUnderLimit(t *testing.T) {
	w := newCappedWriter(32)
	if _, err := w.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if w.Truncated() {
		t.Fatal("unexpected truncate")
	}
	final := finalizeLogBytes(w.Bytes(), w.Truncated(), 32)
	if string(final) != "ok" {
		t.Fatalf("final = %q", final)
	}
}

func TestFormatByteCount(t *testing.T) {
	if got := formatByteCount(16 << 20); got != "16MiB" {
		t.Fatalf("16MiB = %q", got)
	}
	if got := formatByteCount(1024); got != "1KiB" {
		t.Fatalf("1KiB = %q", got)
	}
}
