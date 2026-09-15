package main

import (
	"fmt"
	"os/exec"
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
