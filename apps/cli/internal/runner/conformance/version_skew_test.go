package conformance

import (
	"encoding/json"
	"runtime"
	"testing"

	"doppels.so/cli/internal/runner/proto"
)

// TestVersionSkewMatrix drives v1/initialize with a spread of
// protocolVersion claims a Runner may encounter across a fleet's rollout
// (an older client stuck on v0's un-versioned handshake, the current v1,
// and clients from a hypothetical future protocol): only the exact version
// this Runner speaks may complete the handshake; everything else is refused
// with -32002 and the RFC §7 negotiation payload (expected/supported), never
// left half-initialized. Each row also asserts the connection stays usable
// (or is cleanly refused) and that handling it does not leak a goroutine —
// a version-rejected connection must still tear down its per-connection
// goroutines exactly like a normal disconnect.
func TestVersionSkewMatrix(t *testing.T) {
	tests := []struct {
		name            string
		protocolVersion int
		wantMismatch    bool
	}{
		{name: "v0 (legacy, unversioned handshake)", protocolVersion: 0, wantMismatch: false},
		{name: "v1 (current)", protocolVersion: 1, wantMismatch: false},
		{name: "v2 (future major)", protocolVersion: 2, wantMismatch: true},
		{name: "v99 (far future)", protocolVersion: 99, wantMismatch: true},
		{name: "v255 (max byte-ish value)", protocolVersion: 255, wantMismatch: true},
	}

	baseline := runtime.NumGoroutine()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, false)
			client := dial(t, h)

			params := map[string]any{
				"client": map[string]any{"name": "conformance", "version": "0.0.1"},
				"token":  conformanceToken,
			}
			// protocolVersion 0 means "field omitted" on the real wire (RFC
			// §7: v0 predates the field existing at all): sending a literal
			// 0 exercises exactly that back-compat path, since
			// handleInitialize only rejects a *nonzero* mismatch.
			if tt.protocolVersion != 0 {
				params["protocolVersion"] = tt.protocolVersion
			}
			response := client.call("handshake", "v1/initialize", params)

			if tt.wantMismatch {
				assertErrorResponse(t, response, proto.CodeVersionMismatch)
				// server/server.go's handleInitialize reports the client's
				// own claimed version back as "expected" (the version *it*
				// expected the Runner to speak) and the Runner's own single
				// supported version as "supported" — both plain ints, since
				// this Runner supports exactly one protocol major.
				var errData struct {
					Expected  int `json:"expected"`
					Supported int `json:"supported"`
				}
				if err := json.Unmarshal(mustMarshal(t, response.Err.Data), &errData); err != nil {
					t.Fatalf("decode versionMismatch data: %v", err)
				}
				if errData.Expected != tt.protocolVersion {
					t.Fatalf("versionMismatch data.expected = %d, want the client's own claimed version %d", errData.Expected, tt.protocolVersion)
				}
				if errData.Supported != proto.ProtocolVersion {
					t.Fatalf("versionMismatch data.supported = %d, want %d", errData.Supported, proto.ProtocolVersion)
				}
				// A version-rejected connection must not be left half
				// initialized: it must still enforce the v1/initialize
				// requirement gate exactly like a fresh, never-initialized
				// connection, not treat the rejected handshake as having
				// initialized it.
				ping := client.call("post-mismatch-ping", "v1/ping", map[string]any{})
				assertErrorResponse(t, ping, proto.CodeNotInitialized)
				return
			}

			if response.Err != nil {
				t.Fatalf("v1/initialize: %+v", response.Err)
			}
			var result proto.InitializeResult
			if err := json.Unmarshal(rawResult(t, response), &result); err != nil {
				t.Fatal(err)
			}
			if result.ProtocolVersion != proto.ProtocolVersion {
				t.Fatalf("initialize result.protocolVersion = %d, want %d", result.ProtocolVersion, proto.ProtocolVersion)
			}
			// A completed handshake must actually unlock every other method:
			// this is the connection's only signal that initialize succeeded
			// from the wire's point of view.
			ping := client.call("post-handshake-ping", "v1/ping", map[string]any{})
			if ping.Err != nil {
				t.Fatalf("v1/ping after a successful handshake: %+v", ping.Err)
			}
		})
	}

	waitForGoroutineBaseline(t, baseline)
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
