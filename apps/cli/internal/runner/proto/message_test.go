package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeRequestAcceptsStringAndIntID(t *testing.T) {
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"v1/ping","params":{}}`,
		`{"jsonrpc":"2.0","id":"abc","method":"v1/ping","params":{}}`,
	} {
		message, err := DecodeMessage([]byte(raw))
		if err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if message.Method != "v1/ping" {
			t.Fatalf("method = %q", message.Method)
		}
		if message.ID.Empty() {
			t.Fatalf("id missing in %s", raw)
		}
		if message.IsNotification() {
			t.Fatalf("request with id decoded as notification: %s", raw)
		}
	}
}

func TestDecodeNotificationHasNoID(t *testing.T) {
	message, err := DecodeMessage([]byte(`{"jsonrpc":"2.0","method":"v1/runEvent","params":{"runId":"r-1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !message.IsNotification() {
		t.Fatal("expected notification")
	}
	if message.Method != "v1/runEvent" {
		t.Fatalf("method = %q", message.Method)
	}
	var payload struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(message.Params, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RunID != "r-1" {
		t.Fatalf("runId = %q", payload.RunID)
	}
}

func TestDecodeRejectsBatch(t *testing.T) {
	_, err := DecodeMessage([]byte(`[{"jsonrpc":"2.0","id":1,"method":"v1/ping"}]`))
	if err == nil || err.Code != CodeInvalidRequest {
		t.Fatalf("batch must fail with -32600, got %v", err)
	}
}

func TestDecodeRejectsWrongVersion(t *testing.T) {
	_, err := DecodeMessage([]byte(`{"jsonrpc":"1.0","id":1,"method":"v1/ping"}`))
	if err == nil || err.Code != CodeInvalidRequest {
		t.Fatalf("wrong jsonrpc version must fail with -32600, got %v", err)
	}
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	_, err := DecodeMessage([]byte(`{"jsonrpc":"2.0"`))
	if err == nil || err.Code != CodeParse {
		t.Fatalf("malformed JSON must fail with -32700, got %v", err)
	}
}

func TestDecodeRejectsRequestWithoutMethod(t *testing.T) {
	_, err := DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1}`))
	if err == nil || err.Code != CodeInvalidRequest {
		t.Fatalf("missing method must fail with -32600, got %v", err)
	}
}

func TestDecodeIgnoresUnknownFields(t *testing.T) {
	message, err := DecodeMessage([]byte(`{"jsonrpc":"2.0","id":7,"method":"v1/ping","futureField":{"x":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if message.Method != "v1/ping" {
		t.Fatalf("method = %q", message.Method)
	}
}

func TestResponseMarshalIncludesResultOrNull(t *testing.T) {
	// With a result: "result" present, "error" absent.
	encoded, err := json.Marshal(NewResponse(int64(3), map[string]string{"pong": "ok"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"result"`) || strings.Contains(string(encoded), `"error"`) {
		t.Fatalf("result response shape: %s", encoded)
	}
	// With nil result: explicit "result":null, still no "error".
	encoded, err = json.Marshal(NewResponse(int64(3), nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"result":null`) {
		t.Fatalf("null result must be explicit: %s", encoded)
	}
}

func TestResponseMarshalError(t *testing.T) {
	encoded, err := json.Marshal(NewErrorResponse(int64(5), &Error{Code: CodeMethodNotFound, Message: "method not found"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"error"`) || strings.Contains(string(encoded), `"result"`) {
		t.Fatalf("error response shape: %s", encoded)
	}
}

func TestNotificationMarshalOmitsID(t *testing.T) {
	encoded, err := json.Marshal(NewNotification("v1/runEvent", map[string]any{"runId": "r-1", "sequence": 2}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"id"`) {
		t.Fatalf("notification must not carry id: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"jsonrpc":"2.0"`) {
		t.Fatalf("notification must carry jsonrpc: %s", encoded)
	}
}

func TestSharedTypesUseCamelCaseJSONFields(t *testing.T) {
	encoded, err := json.Marshal(&NodeStatus{
		State:           "online",
		RunnerVersion:   "0.1.0",
		ProtocolVersion: ProtocolVersion,
		StartedAt:       "2026-09-13T10:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{"runnerVersion", "protocolVersion", "startedAt", "workspaces"} {
		if !strings.Contains(text, `"`+want+`"`) {
			t.Fatalf("NodeStatus missing camelCase field %q: %s", want, text)
		}
	}

	encoded, err = json.Marshal(&RunEventPayload{RunID: "r-1", Sequence: 3, Type: "run_created", OccurredAt: "2026-09-13T10:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"runId", "sequence", "occurredAt"} {
		if !strings.Contains(string(encoded), `"`+want+`"`) {
			t.Fatalf("RunEventPayload missing %q: %s", want, encoded)
		}
	}
}

func TestCanonicalJSONSortsKeysRecursivelyWithoutSpaces(t *testing.T) {
	raw := json.RawMessage(`{"inputs":{"b":2,"a":1},"capability":"db/backup","recipe":null}`)
	canonical, err := CanonicalJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"capability":"db/backup","inputs":{"a":1,"b":2},"recipe":null}`
	if string(canonical) != want {
		t.Fatalf("canonical = %s, want %s", canonical, want)
	}
}

func TestCanonicalJSONPreservesNumberSpelling(t *testing.T) {
	// Numbers must survive canonicalization without float mangling.
	canonical, err := CanonicalJSON(json.RawMessage(`{"limit":1e3,"n":12345678901234567890}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"limit":1000`) {
		t.Fatalf("1e3 must canonicalize to 1000: %s", canonical)
	}
	if !strings.Contains(string(canonical), `"n":12345678901234567890`) {
		t.Fatalf("big int must not float-mangle: %s", canonical)
	}
}

func TestFingerprintStableAcrossKeyOrder(t *testing.T) {
	a, err := Fingerprint(json.RawMessage(`{"a":1,"b":{"z":true,"y":[1,2]}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fingerprint(json.RawMessage(`{"b":{"y":[1,2],"z":true},"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("fingerprint differs across key order: %s vs %s", a, b)
	}
}

func TestFingerprintPreservesExactLargeNumbers(t *testing.T) {
	left, err := Fingerprint(json.RawMessage(`{"n":9007199254740993.0}`))
	if err != nil {
		t.Fatal(err)
	}
	right, err := Fingerprint(json.RawMessage(`{"n":9007199254740992}`))
	if err != nil {
		t.Fatal(err)
	}
	if left == right {
		t.Fatal("distinct exact numbers must not share a fingerprint")
	}
}

func TestDecodeMessageRejectsTrailingJSON(t *testing.T) {
	_, protoErr := DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"v1/ping"} garbage`))
	if protoErr == nil || protoErr.Code != CodeParse {
		t.Fatalf("err = %+v, want parse error", protoErr)
	}
}

func TestDecodeMessageRejectsFractionalID(t *testing.T) {
	_, protoErr := DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1.5,"method":"v1/ping"}`))
	if protoErr == nil || protoErr.Code != CodeInvalidRequest {
		t.Fatalf("err = %+v, want invalidRequest", protoErr)
	}
}

func TestDecodeMessagePreservesValidIDAcrossTypedFieldFailures(t *testing.T) {
	tests := []struct {
		name   string
		frame  string
		wantID string
	}{
		{
			name:   "jsonrpc has wrong type",
			frame:  `{"jsonrpc":2,"id":"keep-me","method":"v1/ping"}`,
			wantID: `"keep-me"`,
		},
		{
			name:   "method has wrong type with string id",
			frame:  `{"jsonrpc":"2.0","id":"keep-me","method":42}`,
			wantID: `"keep-me"`,
		},
		{
			name:   "method has wrong type with integer id",
			frame:  `{"jsonrpc":"2.0","id":12345,"method":42}`,
			wantID: `12345`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message, protoErr := DecodeMessage([]byte(tt.frame))
			if protoErr == nil || protoErr.Code != CodeInvalidRequest {
				t.Fatalf("err = %+v, want invalidRequest", protoErr)
			}
			if message == nil {
				t.Fatal("message = nil, want valid id preserved")
			}
			if message.ID.String() != tt.wantID {
				t.Fatalf("id = %s, want %s", message.ID.String(), tt.wantID)
			}
		})
	}

	t.Run("fractional id is not preserved", func(t *testing.T) {
		message, protoErr := DecodeMessage([]byte(`{"jsonrpc":2,"id":1.5,"method":"v1/ping"}`))
		if protoErr == nil || protoErr.Code != CodeInvalidRequest {
			t.Fatalf("err = %+v, want invalidRequest", protoErr)
		}
		if message != nil {
			t.Fatalf("message = %#v, want nil because id itself is invalid", message)
		}
	})
}

// TestDecodeMessagePreservesValidIDAcrossSizeBoundaries reproduces a review
// finding: an id that already decoded successfully and already passed its
// own size validation must be correlatable in the error response for any
// later, unrelated defect (an oversized method) — id null is reserved for
// when the id itself couldn't be determined or trusted, not for "something
// else in the request was also wrong." Covers the exact byte boundaries: id
// at MaxIDBytes succeeds and id at MaxIDBytes+1 is rejected (with id
// necessarily absent — the id itself is what's invalid); method at
// MaxMethodBytes succeeds structurally (a real not-found method name is
// dispatch's job, not the parser's) and method at MaxMethodBytes+1 is
// rejected while still returning the caller's — independently valid — id.
func TestDecodeMessagePreservesValidIDAcrossSizeBoundaries(t *testing.T) {
	frame := func(id, method string) []byte {
		data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	t.Run("id at MaxIDBytes succeeds", func(t *testing.T) {
		id := strings.Repeat("i", MaxIDBytes-2) // -2 for the id's own quotes
		message, protoErr := DecodeMessage(frame(id, "v1/ping"))
		if protoErr != nil {
			t.Fatalf("err = %+v, want success", protoErr)
		}
		if message.ID.String() != `"`+id+`"` {
			t.Fatalf("id = %s, want preserved", message.ID.String())
		}
	})

	t.Run("id at MaxIDBytes+1 is rejected without an id to preserve", func(t *testing.T) {
		id := strings.Repeat("i", MaxIDBytes-1)
		message, protoErr := DecodeMessage(frame(id, "v1/ping"))
		if protoErr == nil || protoErr.Code != CodeInvalidRequest {
			t.Fatalf("err = %+v, want invalidRequest", protoErr)
		}
		if message != nil {
			t.Fatalf("message = %#v, want nil (the id itself is what's invalid, never eligible to preserve)", message)
		}
	})

	t.Run("method at MaxMethodBytes succeeds structurally", func(t *testing.T) {
		method := "v1/" + strings.Repeat("m", MaxMethodBytes-3)
		message, protoErr := DecodeMessage(frame("keep-me", method))
		if protoErr != nil {
			t.Fatalf("err = %+v, want success (an unregistered method name is dispatch's job)", protoErr)
		}
		if message.ID.String() != `"keep-me"` {
			t.Fatalf("id = %s, want preserved", message.ID.String())
		}
	})

	t.Run("method at MaxMethodBytes+1 is rejected but preserves a valid id", func(t *testing.T) {
		method := "v1/" + strings.Repeat("m", MaxMethodBytes-2)
		message, protoErr := DecodeMessage(frame("keep-me", method))
		if protoErr == nil || protoErr.Code != CodeInvalidRequest {
			t.Fatalf("err = %+v, want invalidRequest", protoErr)
		}
		if message == nil {
			t.Fatal("message = nil, want the already-valid id preserved for correlation")
		}
		if message.ID.String() != `"keep-me"` {
			t.Fatalf("id = %s, want preserved %q", message.ID.String(), "keep-me")
		}
	})

	t.Run("method over cap and id over cap: id stays null, not the method's fault", func(t *testing.T) {
		method := "v1/" + strings.Repeat("m", MaxMethodBytes-2)
		id := strings.Repeat("i", MaxIDBytes-1)
		message, protoErr := DecodeMessage(frame(id, method))
		if protoErr == nil || protoErr.Code != CodeInvalidRequest {
			t.Fatalf("err = %+v, want invalidRequest", protoErr)
		}
		if message != nil {
			t.Fatalf("message = %#v, want nil (id fails its own check regardless of the method also being invalid)", message)
		}
	})
}

// TestFingerprintCollidesForSemanticallyEqualNumbers pins the deliberate
// idempotency: 1 and 1.0 are the same number, two exponents of the same
// value collapse to the same canonical bytes, and exponent-free integers
// render as plain decimals. Any caller relying on the fingerprint to
// reject "same value spelled differently" must look elsewhere.
func TestFingerprintCollidesForSemanticallyEqualNumbers(t *testing.T) {
	cases := [][]string{
		{"1", "1.0", "1e0", "1E0"},
		{"1000", "1e3", "1.0e3"},
		{"0", "0.0", "0e0", "-0"},
	}
	for _, group := range cases {
		digests := make(map[string]string)
		for _, spelling := range group {
			digest, err := Fingerprint(json.RawMessage(spelling))
			if err != nil {
				t.Fatalf("%s: %v", spelling, err)
			}
			digests[spelling] = digest
		}
		for spelling, digest := range digests {
			for other, otherDigest := range digests {
				if digest != otherDigest {
					t.Fatalf("expected %s and %s to collide, got %s vs %s",
						spelling, other, digest, otherDigest)
				}
			}
		}
	}
}

// TestFingerprintDifferentiatesByContent pins the discriminative side:
// payloads that share structure but differ in values produce different
// fingerprints. The canonical form is byte-stable but not collision-free.
func TestFingerprintDifferentiatesByContent(t *testing.T) {
	a, err := Fingerprint(json.RawMessage(`{"capability":"a","inputs":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fingerprint(json.RawMessage(`{"capability":"b","inputs":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("different capability strings produced same fingerprint")
	}
	c, err := Fingerprint(json.RawMessage(`{"capability":"a","inputs":{"x":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Fatal("presence of an input key did not affect fingerprint")
	}
}

// TestFingerprintPreservesArrayOrder pins that array order matters for
// idempotency: requests with shuffled inputs are distinct operations even
// when the inputs land in the same set.
func TestFingerprintPreservesArrayOrder(t *testing.T) {
	a, err := Fingerprint(json.RawMessage(`{"a":[1,2,3]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fingerprint(json.RawMessage(`{"a":[3,2,1]}`))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("array reorder must produce a different fingerprint")
	}
}

// TestFingerprintHandlesUnicodeKeys pins the UTF-8 byte-sort stability of
// Go's sort.Strings: distinct keys serialize in their canonical Code Point
// order regardless of the input order.
func TestFingerprintHandlesUnicodeKeys(t *testing.T) {
	a, err := Fingerprint(json.RawMessage(`{"α":1,"β":2}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fingerprint(json.RawMessage(`{"β":2,"α":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("unicode key order diverges: %s vs %s", a, b)
	}
}
