//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"syscall"
)

// prepareProcessGroup isolates a manually launched runner from its parent
// shell before the timeout fallback can target the runner and its children.
// Service managers commonly do this already, so an existing pid==pgid group
// is accepted without another setpgid call.
func prepareProcessGroup() error {
	pid := os.Getpid()
	pgid, err := syscall.Getpgid(0)
	if err != nil {
		return fmt.Errorf("read process group: %w", err)
	}
	if pgid == pid {
		return nil
	}
	if err := syscall.Setpgid(0, 0); err != nil {
		return fmt.Errorf("isolate runner process group: %w", err)
	}
	return nil
}

func killOwnProcessGroup() error {
	pid := os.Getpid()
	pgid, err := syscall.Getpgid(0)
	if err != nil {
		return fmt.Errorf("read process group: %w", err)
	}
	if pgid != pid {
		return fmt.Errorf("refusing to kill shared process group %d (runner PID %d)", pgid, pid)
	}
	return syscall.Kill(-pgid, syscall.SIGKILL)
}
