// Command doppels-runner is the persistent Doppels Runner daemon (RFC 001):
// it owns local execution authority and exposes the v1 IPC over a Unix Domain
// Socket (or, post-alpha, a Windows Named Pipe). Desktop and CLI clients
// connect to it; the Runner never executes Steps requested through any other
// channel.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/runs"
	"doppels.so/cli/internal/runner/server"
	"doppels.so/cli/internal/runner/transport"
	"doppels.so/cli/internal/runner/workspace"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "install" || os.Args[1] == "uninstall") {
		deps, err := defaultLifecycleDependencies()
		if err == nil {
			_, err = executeLifecycleSubcommand(os.Args[1:], deps)
		}
		if err != nil {
			log.Fatalf("doppels-runner: %v", err)
		}
		return
	}

	socket := flag.String("socket", "", "path to the IPC socket (default: <configdir>/runner.sock)")
	tokenFlag := flag.String("token", "", "runner token clients must present at initialize (default: read from <socket-dir>/runner.token or generated)")
	runnerVersion := flag.String("runner-version", "0.1.0-dev", "runner version reported in initialize")
	configDir := flag.String("config", "", "runner config dir (default: XDG user config + /doppels)")
	flag.Parse()

	if err := prepareProcessGroup(); err != nil {
		log.Fatalf("doppels-runner: %v", err)
	}
	if err := run(*socket, *tokenFlag, *runnerVersion, *configDir); err != nil {
		log.Fatalf("doppels-runner: %v", err)
	}
}

func run(socketPath, tokenFlag, runnerVersion, configDir string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return runWithContext(ctx, socketPath, tokenFlag, runnerVersion, configDir)
}

func runWithContext(ctx context.Context, socketPath, tokenFlag, runnerVersion, configDir string) error {
	if configDir == "" {
		defaultDir, err := runnerConfigDir()
		if err != nil {
			return err
		}
		configDir = defaultDir
	}
	if socketPath == "" {
		socketPath = filepath.Join(configDir, "runner.sock")
	}
	token, err := resolveToken(tokenFlag, filepath.Join(configDir, "runner.token"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	registry := workspace.NewRegistry(filepath.Join(configDir, "workspaces.json"))
	if err := registry.Load(); err != nil {
		return err
	}
	workspaces := workspace.NewService(registry, workspace.Deps{Host: manifest.OSHost{}})
	pidPath := filepath.Join(configDir, runnerPIDFile)
	if existingRunner(token, socketPath) {
		if pid, readErr := readPIDFile(pidPath); readErr == nil {
			return anotherInstanceError(pid)
		}
		return fmt.Errorf("another instance already running at %s", socketPath)
	}
	pid, err := acquirePIDLock(pidPath, os.Getpid(), processMatchesRunner)
	if err != nil {
		return err
	}
	defer func() {
		if err := pid.Close(); err != nil {
			log.Printf("doppels-runner: clean up PID file: %v", err)
		}
	}()

	log.Printf("doppels-runner: listening on %s", socketPath)
	listener, err := transport.Unix{}.Listen(socketPath)
	if err != nil {
		if existingRunner(token, socketPath) {
			if owner, readErr := readPIDFile(pidPath); readErr == nil && owner != os.Getpid() {
				return anotherInstanceError(owner)
			}
			return fmt.Errorf("another instance already running at %s", socketPath)
		}
		return fmt.Errorf("listen: %w", err)
	}

	// srv is assigned below, after config is built; OnStarted/OnFinished
	// only fire once Runs actually start, well after that assignment
	// completes, so closing over it here is safe.
	var srv *server.Server
	manager := runs.NewManager(ctx, workspaces, runs.Config{
		Log: log.Printf,
		OnStarted: func(summary proto.RunSummary) {
			srv.EmitNodeEvent(proto.NodeEvent{Kind: proto.NodeEventRunStarted, Payload: summary})
		},
		OnFinished: func(summary proto.RunSummary) {
			srv.EmitNodeEvent(proto.NodeEvent{Kind: proto.NodeEventRunFinished, Payload: summary})
		},
	})
	defer manager.Close()

	startedAt := time.Now().UTC().Format(time.RFC3339)
	config := server.Config{
		Token:         token,
		RunnerVersion: runnerVersion,
		Capabilities:  []string{},
		NodeStatus: func() proto.NodeStatus {
			return workspaces.NodeStatus(proto.NodeStatus{
				State:           "online",
				RunnerVersion:   runnerVersion,
				ProtocolVersion: proto.ProtocolVersion,
				StartedAt:       startedAt,
			})
		},
		OnShutdown: func() {
			if err := manager.Close(); err != nil {
				log.Printf("doppels-runner: close run manager: %v", err)
			}
		},
		Log: log.Printf,
	}
	srv = server.New(config)
	workspace.RegisterRPC(srv, workspaces)
	runs.RegisterRPC(srv, manager)
	serve := func(ctx context.Context) error { return srv.Serve(ctx, listener) }
	return serveWithGracefulShutdown(ctx, gracefulShutdownTimeout, serve, killOwnProcessGroup)
}

// probeTimeout bounds the handshake probe against a socket that accepts but
// never answers: startup must fail loudly instead of hanging forever.
const probeTimeout = 5 * time.Second

// existingRunner reports whether a live runner already owns socketPath. Only
// a well-formed, authenticated v1/initialize result counts: a peer that
// answers garbage, rejects our token or answers a different id is not a
// runner we may defer to, and exiting 0 for it would leave the host without
// any runner at all.
func existingRunner(token, socketPath string) bool {
	conn, err := transport.Unix{}.Dial(socketPath)
	if err != nil {
		return false
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(probeTimeout)); err != nil {
		return false
	}
	if err := proto.NewEncoder(conn).WriteFrame(map[string]any{
		"jsonrpc": "2.0", "id": probeID, "method": "v1/initialize",
		"params": map[string]any{
			"protocolVersion": proto.ProtocolVersion,
			"token":           token,
			"client":          map[string]any{"name": "doppels-runner", "version": "probe"},
		},
	}); err != nil {
		return false
	}
	frame, err := proto.NewDecoder(conn).ReadFrame()
	if err != nil {
		return false
	}
	// Decoded locally instead of through proto.Response so the probe can
	// check the envelope itself: version, id and the initialize result.
	var response struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Error   *struct {
			Code int `json:"code"`
		} `json:"error"`
		Result struct {
			ProtocolVersion int    `json:"protocolVersion"`
			RunnerVersion   string `json:"runnerVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frame, &response); err != nil {
		return false
	}
	if response.JSONRPC != "2.0" || response.Error != nil || response.ID != probeID {
		return false
	}
	return response.Result.ProtocolVersion == proto.ProtocolVersion && response.Result.RunnerVersion != ""
}

const probeID = "probe"

// runnerConfigDir resolves the runner's config directory under the user's
// config root; DOPPELS_RUNNER_CONFIG overrides.
func runnerConfigDir() (string, error) {
	if override := os.Getenv("DOPPELS_RUNNER_CONFIG"); override != "" {
		return override, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "doppels"), nil
}

// resolveToken honours the explicit flag, otherwise reads a persisted
// token, otherwise generates + persists a fresh 32-byte hex one.
func resolveToken(flagValue, path string) (string, error) {
	if flagValue != "" {
		return validateToken(flagValue)
	}
	switch token, err := readToken(path); {
	case err == nil:
		return token, nil
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	// Create the token in a temporary file and link it into place: link
	// fails if the path already exists, so concurrent starts elect exactly
	// one writer and every loser reads a file that is already complete. A
	// bare O_EXCL create would expose an empty file to the losers between
	// create and write.
	staging, err := os.CreateTemp(filepath.Dir(path), ".runner.token-*")
	if err != nil {
		return "", fmt.Errorf("persist token: %w", err)
	}
	defer os.Remove(staging.Name())
	if err := staging.Chmod(0o600); err != nil {
		staging.Close()
		return "", fmt.Errorf("persist token: %w", err)
	}
	if _, err := staging.WriteString(token); err != nil {
		staging.Close()
		return "", fmt.Errorf("persist token: %w", err)
	}
	if err := staging.Close(); err != nil {
		return "", fmt.Errorf("persist token: %w", err)
	}
	if err := os.Link(staging.Name(), path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("persist token: %w", err)
		}
		return readToken(path)
	}
	return token, nil
}

// readToken loads a persisted token and refuses one that other users can
// read: the token is the entire authority to execute Steps on this host.
func readToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if mode := info.Mode(); mode.Perm()&0o077 != 0 {
		return "", fmt.Errorf("runner token %s is accessible to other users (mode %#o); restore it with chmod 600", path, mode.Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	return validateToken(string(data))
}

func validateToken(raw string) (string, error) {
	if strings.HasSuffix(raw, "\n") {
		raw = strings.TrimSuffix(raw, "\n")
		if strings.HasSuffix(raw, "\r") {
			raw = strings.TrimSuffix(raw, "\r")
		}
	}
	if len(raw) != 64 {
		return "", fmt.Errorf("runner token must be exactly 32 bytes encoded as 64 hex characters")
	}
	if _, err := hex.DecodeString(raw); err != nil {
		return "", fmt.Errorf("runner token is not valid hex: %w", err)
	}
	return raw, nil
}

func generateToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
