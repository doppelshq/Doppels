package runnerclient

import (
	"os"
	"path/filepath"
	"time"

	"doppels.so/cli/internal/runner/proto"
)

// ClientVersion identifies this client build to the Runner's handshake
// (RFC §6: client.version). It is deliberately a fixed constant, not a full
// build-info story — PR8 scope is the client itself, not a CLI versioning
// overhaul.
const ClientVersion = "0.0.0-dev"

// DefaultHandshakeTimeout bounds how long Dial waits for v1/initialize to
// answer. It must stay comfortably under the server's 10s handshake window
// (RFC §6) so a client-side timeout never races a legitimate slow-but-alive
// server response.
const DefaultHandshakeTimeout = 5 * time.Second

// Options configures Dial. Zero values resolve to the standard runner
// config directory (mirroring cmd/doppels-runner's runnerConfigDir): socket
// at "<configdir>/runner.sock", token read from "<configdir>/runner.token".
type Options struct {
	// SocketPath overrides the default runner.sock location.
	SocketPath string
	// Token overrides reading the on-disk runner.token file.
	Token string
	// ClientName identifies this client in the handshake (RFC §6); the
	// Runner derives a started Run's "source" from it. Defaults to "cli".
	ClientName string
	// ProtocolVersion is the protocol major version requested at handshake.
	// Defaults to proto.ProtocolVersion.
	ProtocolVersion int
	// HandshakeTimeout bounds Dial's wait for v1/initialize. Defaults to
	// DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration
	// OnNotification receives every incoming v1 notification (runEvent,
	// nodeEvent, runLog, ...) dispatched by the background read loop. It may
	// be nil (notifications are simply dropped) and may be changed at any
	// time via Client.SetNotificationHandler.
	OnNotification func(Notification)
}

// runnerConfigDir mirrors cmd/doppels-runner's runnerConfigDir: the same
// ~6 lines are duplicated here (rather than imported) so this package stays
// usable in tests and by Desktop's future FFI binding without pulling in the
// cmd package.
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

// withDefaults returns a copy of opts with every zero-valued field resolved
// to its default. Resolving SocketPath/Token requires touching the
// filesystem, which can fail (e.g. no config dir); errors from that are
// surfaced by Dial rather than swallowed here.
func (o Options) withDefaults() (Options, error) {
	resolved := o
	if resolved.ClientName == "" {
		resolved.ClientName = "cli"
	}
	if resolved.ProtocolVersion == 0 {
		resolved.ProtocolVersion = proto.ProtocolVersion
	}
	if resolved.HandshakeTimeout <= 0 {
		resolved.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if resolved.SocketPath == "" || resolved.Token == "" {
		configDir, err := runnerConfigDir()
		if err != nil {
			return Options{}, err
		}
		if resolved.SocketPath == "" {
			resolved.SocketPath = filepath.Join(configDir, "runner.sock")
		}
		if resolved.Token == "" {
			tokenBytes, err := os.ReadFile(filepath.Join(configDir, "runner.token"))
			if err != nil {
				return Options{}, err
			}
			resolved.Token = trimToken(tokenBytes)
		}
	}
	return resolved, nil
}

func trimToken(raw []byte) string {
	// Token files are written as a bare hex string; trim any trailing
	// newline defensively without pulling in strings.TrimSpace's broader
	// whitespace set (a token must never silently absorb interior spaces).
	end := len(raw)
	for end > 0 && (raw[end-1] == '\n' || raw[end-1] == '\r') {
		end--
	}
	return string(raw[:end])
}

// FromEnvironment applies the CLI's hidden daemon-routing knobs on top of
// opts: DOPPELS_RUNNER_SOCK overrides SocketPath when set. It does not
// decide whether to dial at all (see ShouldDialRunner) — only where.
func (o Options) FromEnvironment(getenv func(string) string) Options {
	if getenv == nil {
		getenv = os.Getenv
	}
	resolved := o
	if sock := getenv("DOPPELS_RUNNER_SOCK"); sock != "" {
		resolved.SocketPath = sock
	}
	return resolved
}

// ShouldDialRunner reports whether callers should even attempt Dial, honoring
// the hidden DOPPELS_RUNNER=0 override that forces the standalone path.
func ShouldDialRunner(getenv func(string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	return getenv("DOPPELS_RUNNER") != "0"
}
