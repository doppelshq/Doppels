package proto

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// CanonicalJSON re-encodes a JSON value with keys sorted recursively, no
// insignificant whitespace, and stable number spelling. It is the basis of
// the persisted idempotency request_fingerprint (RFC §12): two payloads that
// differ only in key order must produce the same canonical bytes.
func CanonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("canonical JSON must contain exactly one value")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, value); err != nil {
		return nil, err
	}
	return json.RawMessage(buf.Bytes()), nil
}

// Fingerprint returns the SHA-256 hex digest of the canonical encoding.
func Fingerprint(raw json.RawMessage) (string, error) {
	canonical, err := CanonicalJSON(raw)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func writeCanonical(buf *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buf.WriteByte(',')
			}
			keyJSON, err := json.Marshal(key)
			if err != nil {
				return err
			}
			buf.Write(keyJSON)
			buf.WriteByte(':')
			if err := writeCanonical(buf, typed[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case json.Number:
		buf.WriteString(canonicalNumber(typed.String()))
	case string, bool:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		buf.Write(encoded)
	case nil:
		buf.WriteString("null")
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		buf.Write(encoded)
	}
	return nil
}

// canonicalNumber normalizes number spelling: integral values render without
// exponent or fraction, while values that would lose precision keep their
// original digits.
func canonicalNumber(text string) string {
	negative := strings.HasPrefix(text, "-")
	if negative {
		text = text[1:]
	}
	parts := strings.SplitN(strings.ToLower(text), "e", 2)
	mantissa := parts[0]
	exponent := 0
	if len(parts) == 2 {
		if _, err := fmt.Sscan(parts[1], &exponent); err != nil {
			return text
		}
	}
	decimal := strings.IndexByte(mantissa, '.')
	if decimal < 0 {
		decimal = len(mantissa)
	} else {
		mantissa = strings.ReplaceAll(mantissa, ".", "")
	}
	decimal += exponent
	mantissa = strings.TrimLeft(mantissa, "0")
	if mantissa == "" {
		return "0"
	}
	var result string
	switch {
	case decimal <= 0:
		result = "0." + strings.Repeat("0", -decimal) + mantissa
	case decimal >= len(mantissa):
		result = mantissa + strings.Repeat("0", decimal-len(mantissa))
	default:
		result = mantissa[:decimal] + "." + mantissa[decimal:]
	}
	if strings.Contains(result, ".") {
		result = strings.TrimRight(strings.TrimRight(result, "0"), ".")
	}
	if result == "" || result == "0" {
		return "0"
	}
	if negative {
		return "-" + result
	}
	return result
}
