package transport

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempSocketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "runner.sock")
}

func TestUnixListenDialRoundtrip(t *testing.T) {
	path := tempSocketPath(t)
	transport := Unix{}

	listener, err := transport.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverDone := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- ""
			return
		}
		defer conn.Close()
		buf := make([]byte, 5)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("pong!"))
		serverDone <- string(buf)
	}()

	client, err := transport.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := client.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "pong!" {
		t.Fatalf("echo = %q", buf)
	}
	if got := <-serverDone; got != "ping\n" {
		t.Fatalf("server read %q", got)
	}
}

func TestUnixSocketHasUserOnlyPermissions(t *testing.T) {
	path := tempSocketPath(t)
	transport := Unix{}
	listener, err := transport.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket perms = %o, want 600", perm)
	}
}

func TestUnixListenReplacesStaleSocket(t *testing.T) {
	path := tempSocketPath(t)
	// A leftover socket inode from a crashed runner blocks the path: bind a
	// real socket, then close it keeping the file (no accept loop remains).
	dead, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := dead.Close(); err != nil {
		t.Fatal(err)
	}

	transport := Unix{}
	listener, err := transport.Listen(path)
	if err != nil {
		t.Fatalf("listen over stale socket: %v", err)
	}
	defer listener.Close()

	// And the fresh listener actually works.
	client, err := transport.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
}

func TestUnixListenFailsWhenLiveListenerExists(t *testing.T) {
	path := tempSocketPath(t)
	transport := Unix{}
	first, err := transport.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	if _, err := transport.Listen(path); err == nil {
		t.Fatal("second listen on a live socket must fail")
	}
}

func TestDialTimesOutQuicklyOnDeadSocket(t *testing.T) {
	path := tempSocketPath(t)
	// A socket file that exists but has no listener behind it must fail
	// fast (the CLI/desktop spawn path depends on quick probes).
	if err := os.WriteFile(path, []byte("dead"), 0o600); err != nil {
		t.Fatal(err)
	}
	transport := Unix{}
	start := time.Now()
	if _, err := transport.Dial(path); err == nil {
		t.Fatal("dial to dead socket must fail")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("dial took %s; want fast failure", elapsed)
	}
}

// Interface satisfaction pins the abstraction the daemon and future Windows
// named-pipe transport share.
var _ Listener = (*unixListener)(nil)
var _ net.Listener = (*unixListener)(nil)
