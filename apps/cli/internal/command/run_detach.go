package command

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
)

const (
	envDetachWorker = "DOPPELS_DETACH_WORKER"
	envDetachRunID  = "DOPPELS_DETACH_RUN_ID"
)

func isDetachWorker(environment []string) bool {
	for _, item := range environment {
		if item == envDetachWorker+"=1" {
			return true
		}
	}
	return false
}

func detachRunID(environment []string) string {
	prefix := envDetachRunID + "="
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(item, prefix))
		}
	}
	return ""
}

func filterDetachArgs(arguments []string) []string {
	filtered := make([]string, 0, len(arguments))
	for i := 0; i < len(arguments); i++ {
		arg := arguments[i]
		switch {
		case arg == "-d", arg == "--detach":
			continue
		case strings.HasPrefix(arg, "--detach="):
			continue
		default:
			filtered = append(filtered, arg)
		}
	}
	return filtered
}

func newDetachRunID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func (app *App) startDetachedRun(root string, runArgs []string, jsonOutput bool) int {
	if isDetachWorker(app.environment()) {
		fmt.Fprintln(app.Stderr, "--detach cannot nest inside a detached worker")
		return ExitContract
	}
	runID, err := newDetachRunID()
	if err != nil {
		fmt.Fprintf(app.Stderr, "allocate Run id: %v\n", err)
		return ExitOperational
	}
	detachDir := filepath.Join(root, project.Directory, "detach")
	if err := os.MkdirAll(detachDir, 0o700); err != nil {
		fmt.Fprintf(app.Stderr, "create detach dir: %v\n", err)
		return ExitOperational
	}
	logPath := filepath.Join(detachDir, runID+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		fmt.Fprintf(app.Stderr, "create detach log: %v\n", err)
		return ExitOperational
	}
	defer logFile.Close()

	executable := app.Executable
	if executable == nil {
		executable = os.Executable
	}
	exe, err := executable()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve executable: %v\n", err)
		return ExitOperational
	}
	childArgs := append([]string{"run"}, filterDetachArgs(runArgs)...)
	cmd := exec.Command(exe, childArgs...)
	cwd, err := app.Getwd()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve working directory: %v\n", err)
		return ExitOperational
	}
	cmd.Dir = cwd
	cmd.Env = append(detachChildEnv(app.environment()), envDetachWorker+"=1", envDetachRunID+"="+runID)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if attr := detachSysProcAttr(); attr != nil {
		cmd.SysProcAttr = attr
	}

	start := app.StartCommand
	if start == nil {
		start = func(command *exec.Cmd) error { return command.Start() }
	}
	if err := start(cmd); err != nil {
		fmt.Fprintf(app.Stderr, "start detached run: %v\n", err)
		return ExitOperational
	}
	if cmd.Process != nil {
		_ = os.WriteFile(filepath.Join(detachDir, runID+".pid"), []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o600)
		_ = cmd.Process.Release()
	}

	stateDir := filepath.Join(root, project.Directory, "runs", runID)
	if jsonOutput {
		app.writeJSON(map[string]any{
			"apiVersion": manifest.APIVersion,
			"kind":       "DetachedRun",
			"runId":      runID,
			"stateDir":   stateDir,
			"logFile":    logPath,
			"status":     "detached",
		})
		return ExitSuccess
	}
	style := newTermStyle(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Run"), style.shortIDPrimary(runID))
	fmt.Fprintf(app.Stdout, "  %s  %s %s\n", style.field("Status"), style.boldCyan(arrowMark(style)), style.bold("Detached"))
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Log"), style.dim(shortStateDir(logPath)))
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Follow"), "doppels runs logs "+shortRunID(runID)+" -f")
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Inspect"), "doppels runs show "+shortRunID(runID))
	return ExitSuccess
}

func detachChildEnv(environment []string) []string {
	out := make([]string, 0, len(environment))
	for _, item := range environment {
		if strings.HasPrefix(item, envDetachWorker+"=") || strings.HasPrefix(item, envDetachRunID+"=") {
			continue
		}
		out = append(out, item)
	}
	return out
}
