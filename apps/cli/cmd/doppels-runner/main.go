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

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/server"
	"doppels.so/cli/internal/runner/transport"
)

func main() {
	socket := flag.String("socket", "", "path to the IPC socket (default: <configdir>/runner.sock)")
	tokenFlag := flag.String("token", "", "runner token clients must present at initialize (default: read from <socket-dir>/runner.token or generated)")
	runnerVersion := flag.String("runner-version", "0.1.0-dev", "runner version reported in initialize")
	configDir := flag.String("config", "", "runner config dir (default: XDG user config + /doppels)")
	flag.Parse()

	if err := run(*socket, *tokenFlag, *runnerVersion, *configDir); err != nil {
		log.Fatalf("doppels-runner: %v", err)
	}
}

func run(socketPath, tokenFlag, runnerVersion, configDir string) error {
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

	log.Printf("doppels-runner: listening on %s", socketPath)
	listener, err := transport.Unix{}.Listen(socketPath)
	if err != nil {
		if existingRunner(token, socketPath) {
			return nil
		}
		return fmt.Errorf("listen: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	startedAt := time.Now().UTC().Format(time.RFC3339)
	hostname, _ := os.Hostname()
	_ = hostname
	config := server.Config{
		Token:         token,
		RunnerVersion: runnerVersion,
		Capabilities:  []string{},
		NodeStatus: func() proto.NodeStatus {
			return proto.NodeStatus{
				State:           "online",
				RunnerVersion:   runnerVersion,
				ProtocolVersion: proto.ProtocolVersion,
				StartedAt:       startedAt,
				Workspaces:      []proto.WorkspaceSummary{},
			}
		},
		Log: log.Printf,
	}
	srv := server.New(config)
	return srv.Serve(ctx, listener)
}

func existingRunner(token, socketPath string) bool {
	conn, err := transport.Unix{}.Dial(socketPath)
	if err != nil {
		return false
	}
	defer conn.Close()
	if err := proto.NewEncoder(conn).WriteFrame(map[string]any{
		"jsonrpc": "2.0", "id": "probe", "method": "v1/initialize",
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
	var response proto.Response
	if err := json.Unmarshal(frame, &response); err != nil {
		return false
	}
	return response.Err == nil
}

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
	if data, err := os.ReadFile(path); err == nil {
		return validateToken(string(data))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read token: %w", err)
	}
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return "", fmt.Errorf("read token after race: %w", readErr)
			}
			return validateToken(string(data))
		}
		return "", fmt.Errorf("persist token: %w", err)
	}
	if _, err := file.WriteString(token); err != nil {
		file.Close()
		return "", fmt.Errorf("persist token: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("persist token: %w", err)
	}
	return token, nil
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
