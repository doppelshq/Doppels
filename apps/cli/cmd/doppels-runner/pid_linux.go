//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
)

func processMatchesRunner(pid int) bool {
	targetPath := filepath.Join("/proc", strconv.Itoa(pid), "exe")
	target, err := os.Stat(targetPath)
	if err != nil {
		return false
	}
	currentPath, err := os.Executable()
	if err != nil {
		return false
	}
	current, err := os.Stat(currentPath)
	return err == nil && os.SameFile(target, current)
}
