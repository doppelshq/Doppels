// Package proto implements the Doppels Runner IPC wire format (RFC 001 —
// doppels.so/ipc/v1): JSON-RPC 2.0 over NDJSON frames, Doppels error codes,
// and the shared payload types every method speaks.
package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ProtocolVersion is the major protocol number; there is no minor (RFC §7).
const ProtocolVersion = 1

// Capability flags advertised by v1/initialize.
const CapabilityLiveLogs = "liveLogs"

// JSON-RPC 2.0 standard error codes.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
)

// Doppels-specific error range -32000..-32099 (RFC §11).
const (
	CodeNotInitialized     = -32000
	CodeAuthFailed         = -32001
	CodeVersionMismatch    = -32002
	CodeWorkspaceNotFound  = -32003
	CodeCapabilityNotFound = -32004
	CodeRecipeAmbiguous    = -32005
	CodeRunNotFound        = -32006
	CodeApprovalNotFound   = -32007
	CodeInvalidInputs      = -32008
	CodeStalePin           = -32009
	CodeBusy               = -32010
)

// ID preserves a JSON-RPC id exactly as spelled on the wire (string or
// number); empty means absent or null.
type ID struct {
	raw json.RawMessage
}

func (id ID) MarshalJSON() ([]byte, error) {
	if len(id.raw) == 0 {
		return []byte("null"), nil
	}
	return id.raw, nil
}

func (id *ID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if string(trimmed) == "null" {
		id.raw = nil
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var probe any
	if err := decoder.Decode(&probe); err != nil {
		return err
	}
	switch probe.(type) {
	case string:
		id.raw = trimmed
		return nil
	case json.Number:
		text := string(trimmed)
		if bytes.ContainsAny([]byte(text), ".eE") {
			return fmt.Errorf("jsonrpc id must be an integer")
		}
		id.raw = trimmed
		return nil
	default:
		return fmt.Errorf("jsonrpc id must be a string or number")
	}
}

func (id ID) Empty() bool { return len(id.raw) == 0 || string(bytes.TrimSpace(id.raw)) == "null" }

// String renders the id for logs and diagnostics (never secret material).
func (id ID) String() string {
	if id.Empty() {
		return ""
	}
	return string(id.raw)
}

// Message is one decoded incoming frame: either a request (HasID) or a
// notification (IsNotification). Unknown fields are ignored per RFC §5.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      ID              `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	HasID   bool            `json:"-"`
}

func (m *Message) IsNotification() bool { return !m.HasID }

func (m *Message) UnmarshalJSON(data []byte) error {
	var wire struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if len(wire.ID) > 0 && bytes.Equal(bytes.TrimSpace(wire.ID), []byte("null")) {
		return fmt.Errorf("jsonrpc id must not be null")
	}
	m.JSONRPC, m.Method, m.Params = wire.JSONRPC, wire.Method, wire.Params
	if len(wire.ID) > 0 {
		if err := json.Unmarshal(wire.ID, &m.ID); err != nil {
			return err
		}
		m.HasID = true
	}
	return nil
}

// Error is the JSON-RPC error object; Data carries diagnostics payloads.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message)
}

// Response is an outgoing answer to a request: exactly one of Result/Error.
type Response struct {
	ID     any
	Result any
	Err    *Error
}

func NewResponse(id any, result any) Response      { return Response{ID: id, Result: result} }
func NewErrorResponse(id any, err *Error) Response { return Response{ID: id, Err: err} }

func (r Response) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"jsonrpc":"2.0","id":`)
	idJSON, err := json.Marshal(r.ID)
	if err != nil {
		return nil, err
	}
	buf.Write(idJSON)
	if r.Err != nil {
		errJSON, marshalErr := json.Marshal(r.Err)
		if marshalErr != nil {
			return nil, marshalErr
		}
		buf.WriteString(`,"error":`)
		buf.Write(errJSON)
	} else {
		resultJSON, marshalErr := json.Marshal(r.Result)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if r.Result == nil {
			resultJSON = []byte("null")
		}
		buf.WriteString(`,"result":`)
		buf.Write(resultJSON)
	}
	buf.WriteString("}")
	return buf.Bytes(), nil
}

// UnmarshalJSON keeps Result as raw JSON so clients can decode into their
// own shapes without a double-float roundtrip.
func (r *Response) UnmarshalJSON(data []byte) error {
	var wire struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *Error          `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	r.ID = ID{raw: wire.ID}
	r.Err = wire.Error
	r.Result = wire.Result
	return nil
}

// Notification is an outgoing server-initiated message without id.
type Notification struct {
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

func NewNotification(method string, params any) Notification {
	return Notification{Method: method, Params: params}
}

func (n Notification) MarshalJSON() ([]byte, error) {
	type plain Notification
	return json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		plain   `json:",inline"`
	}{JSONRPC: "2.0", plain: plain(n)})
}

// DecodeMessage parses one incoming frame. Batch arrays, wrong jsonrpc
// versions, and frames without a method yield the JSON-RPC error the RFC
// prescribes (-32600 / -32700).
func DecodeMessage(data []byte) (*Message, *Error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, &Error{Code: CodeParse, Message: "empty frame"}
	}
	if trimmed[0] == '[' {
		return nil, &Error{Code: CodeInvalidRequest, Message: "batch requests are not supported"}
	}
	var message Message
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if err := decoder.Decode(&message); err != nil {
		return nil, &Error{Code: CodeParse, Message: "malformed JSON frame"}
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, &Error{Code: CodeParse, Message: "trailing JSON value"}
	} else if !errors.Is(err, io.EOF) {
		return nil, &Error{Code: CodeParse, Message: "malformed JSON frame"}
	}
	if message.JSONRPC != "2.0" {
		return nil, &Error{Code: CodeInvalidRequest, Message: `jsonrpc must be "2.0"`}
	}
	if message.Method == "" {
		return nil, &Error{Code: CodeInvalidRequest, Message: "method is required"}
	}
	return &message, nil
}

// ErrShutdown signals the server accepted a v1/shutdown request.
var ErrShutdown = errors.New("runner shutdown requested")
