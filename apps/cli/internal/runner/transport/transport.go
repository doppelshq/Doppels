// Package transport abstracts the OS-specific local IPC channel for the
// Doppels Runner (RFC 001 §3): Unix Domain Sockets on macOS/Linux/WS2, with
// a Windows Named Pipe transport to come post-alpha.
package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// DialTimeout bounds how long a client waits for a runner connection.
const DialTimeout = 2 * time.Second

// Listener accepts incoming runner connections.
type Listener interface {
	Accept() (net.Conn, error)
	Close() error
	Addr() net.Addr
}

// Transport listens and dials the platform's local IPC channel. path is the
// socket file (UDS) or pipe name.
type Transport interface {
	Listen(path string) (Listener, error)
	Dial(path string) (net.Conn, error)
}

// Unix is the Unix Domain Socket transport.
type Unix struct{}

func (Unix) Listen(path string) (Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("socket directory: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err == nil {
		// The socket grants access purely to the same user (RFC §3); the
		// umask could have made it looser.
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
			listener.Close()
			return nil, fmt.Errorf("socket permissions: %w", chmodErr)
		}
		return &unixListener{Listener: listener}, nil
	}
	// A leftover socket from a crashed runner is not a live listener: probe
	// it, reclaim the path, and listen again. A live runner keeps ownership.
	if isStaleSocket(path) {
		if removeErr := os.Remove(path); removeErr != nil {
			return nil, fmt.Errorf("remove stale socket: %w", removeErr)
		}
		listener, retryErr := net.Listen("unix", path)
		if retryErr == nil {
			if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
				listener.Close()
				return nil, fmt.Errorf("socket permissions: %w", chmodErr)
			}
			return &unixListener{Listener: listener}, nil
		}
		return nil, fmt.Errorf("listen after stale cleanup: %w", retryErr)
	}
	return nil, err
}

func (Unix) Dial(path string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: DialTimeout}
	return dialer.DialContext(context.Background(), "unix", path)
}

type unixListener struct {
	net.Listener
}

func (l *unixListener) Close() error {
	// net.UnixListener unlinks by default unless SetUnlinkOnClose(false);
	// keep explicit best-effort removal for the documented shutdown path.
	if err := l.Listener.Close(); err != nil {
		return err
	}
	if addr := l.Addr(); addr != nil {
		if path := addr.String(); path != "" {
			_ = os.Remove(path)
		}
	}
	return nil
}

// isStaleSocket reports whether a blocking path is a socket nobody is
// serving (crashed runner) rather than a live listener.
func isStaleSocket(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSocket == 0 {
		// Not even a socket (regular file, directory): refuse to delete
		// arbitrary user files; only reclaim true socket remnants.
		return false
	}
	conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err == nil {
		conn.Close()
		return false // live listener owns the path
	}
	var (
		errno   syscall.Errno
		dialErr *net.OpError
	)
	if errors.As(err, &dialErr) || errors.As(err, &errno) {
		// Connection refused / ECONNREFUSED means no accept loop remains.
		return true
	}
	return false
}
