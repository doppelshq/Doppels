package runnerclient

import (
	"errors"
	"fmt"
)

// ErrNotRunning means the Runner daemon does not appear to be reachable at
// the configured socket path (no such file, connection refused). Callers use
// this to decide whether to fall back to a standalone execution path.
var ErrNotRunning = errors.New("runner: daemon is not running")

// ErrClosed is returned by Call (and delivered to any Call blocked waiting
// on a response) once the Client has been closed.
var ErrClosed = errors.New("runner: client closed")

// VersionMismatchError is returned by Dial when the Runner does not support
// the requested protocol version (RFC §11, -32002 versionMismatch), whether
// signalled via an explicit error or via a mismatched protocolVersion field
// in an otherwise-successful initialize result.
type VersionMismatchError struct {
	Requested int
	Supported int
}

func (e *VersionMismatchError) Error() string {
	return fmt.Sprintf("runner: protocol version mismatch: requested %d, runner supports %d", e.Requested, e.Supported)
}
