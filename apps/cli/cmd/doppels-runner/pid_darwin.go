//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func processMatchesRunner(pid int) bool {
	output, err := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return false
	}
	current, err := os.Executable()
	if err != nil {
		return false
	}
	command := strings.TrimSpace(string(output))
	return command == current || filepath.Base(command) == filepath.Base(current)
}
