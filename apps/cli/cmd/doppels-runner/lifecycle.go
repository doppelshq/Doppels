package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// lifecycleOptions is the fully resolved service definition shared by the
// platform launchers. Paths are resolved before rendering because user
// services do not inherit the install command's working directory.
type lifecycleOptions struct {
	Executable string
	HomeDir    string
	ConfigHome string
	ConfigDir  string
	SocketPath string
	Token      string
	UID        int
	Enable     bool
	StartNow   bool
}

func (o lifecycleOptions) programArguments() []string {
	args := []string{o.Executable, "--config=" + o.ConfigDir}
	if o.SocketPath != "" {
		args = append(args, "--socket="+o.SocketPath)
	}
	if o.Token != "" {
		args = append(args, "--token="+o.Token)
	}
	return args
}

func writeServiceFile(path string, contents []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "doppels-runner.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(contents); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

type commandRunner interface {
	Run(name string, args ...string) error
}

type execCommandRunner struct{}

func (execCommandRunner) Run(name string, args ...string) error {
	command := exec.Command(name, args...)
	if output, err := command.CombinedOutput(); err != nil {
		if len(output) != 0 {
			return fmt.Errorf("%w: %s", err, output)
		}
		return err
	}
	return nil
}
