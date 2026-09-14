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
	"strconv"
	"strings"
)

// maxCanonicalNumberDigits bounds how far an exponent may expand a number
// into its decimal form. A frame is capped at 4 MiB, but `1e100000` is only
// eight bytes on the wire and would otherwise allocate 100 KB — and
// `1e9223372036854775807` would overflow the decimal point outright. Inputs
// longer than the budget keep their own length as the budget, so a literal
// long integer still canonicalizes to itself.
const maxCanonicalNumberDigits = 1024

// ErrCanonicalNumberRange marks a number whose canonical decimal form is not
// representable within the digit budget. Callers map it to -32602.
var ErrCanonicalNumberRange = errors.New("number exponent out of canonical range")

// ErrCanonicalDuplicateKey marks an object with a repeated key. The canonical
// form must not depend on whether a parser keeps the first or the last
// occurrence, so the payload is rejected instead of silently collapsed.
var ErrCanonicalDuplicateKey = errors.New("duplicate object key")

// CanonicalJSON re-encodes a JSON value with keys sorted recursively, no
// insignificant whitespace, and stable number spelling. It is the basis of
// the persisted idempotency request_fingerprint (RFC §12): two payloads that
// differ only in key order must produce the same canonical bytes.
func CanonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeCanonical(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err == nil {
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

// decodeCanonical walks the token stream instead of unmarshalling into a map,
// which is what makes duplicate keys observable: encoding/json would keep the
// last occurrence and hide the ambiguity inside the fingerprint.
func decodeCanonical(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	return decodeCanonicalValue(decoder, token)
}

func decodeCanonicalValue(decoder *json.Decoder, token json.Token) (any, error) {
	delim, ok := token.(json.Delim)
	if !ok {
		// string, json.Number, bool or nil.
		return token, nil
	}
	switch delim {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object key must be a string, got %v", keyToken)
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("%w: %q", ErrCanonicalDuplicateKey, key)
			}
			value, err := decodeCanonical(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if _, err := decoder.Token(); err != nil { // closing '}'
			return nil, err
		}
		return object, nil
	case '[':
		array := []any{}
		for decoder.More() {
			item, err := decodeCanonical(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, item)
		}
		if _, err := decoder.Token(); err != nil { // closing ']'
			return nil, err
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected delimiter %v", delim)
	}
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
		number, err := canonicalNumber(typed.String())
		if err != nil {
			return err
		}
		buf.WriteString(number)
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

// canonicalNumber renders the exact decimal expansion of a JSON number: no
// exponent, no leading zeros, no trailing fraction zeros, and never through
// float64 (9007199254740993 and 9007199254740992 must stay distinct).
func canonicalNumber(text string) (string, error) {
	original := text
	negative := strings.HasPrefix(text, "-")
	if negative {
		text = text[1:]
	}
	lowered := strings.ToLower(text)
	mantissa := lowered
	exponent := 0
	if marker := strings.IndexByte(lowered, 'e'); marker >= 0 {
		mantissa = lowered[:marker]
		parsed, err := strconv.Atoi(lowered[marker+1:])
		if err != nil {
			return "", fmt.Errorf("%w: %s", ErrCanonicalNumberRange, original)
		}
		exponent = parsed
	}
	// point is the position of the decimal separator inside the digit string.
	point := strings.IndexByte(mantissa, '.')
	if point < 0 {
		point = len(mantissa)
	} else {
		mantissa = strings.ReplaceAll(mantissa, ".", "")
	}
	// Dropping leading zeros moves the separator left by as many digits; the
	// previous implementation trimmed without adjusting, which turned 0.1
	// into 1 and collapsed distinct fingerprints.
	trimmed := strings.TrimLeft(mantissa, "0")
	point -= len(mantissa) - len(trimmed)
	digits := strings.TrimRight(trimmed, "0")
	if digits == "" {
		return "0", nil
	}
	budget := maxCanonicalNumberDigits
	if len(original) > budget {
		budget = len(original)
	}
	if exponent > budget || exponent < -budget {
		return "", fmt.Errorf("%w: %s", ErrCanonicalNumberRange, original)
	}
	point += exponent
	var result string
	switch {
	case point <= 0:
		if len(digits)-point+2 > budget {
			return "", fmt.Errorf("%w: %s", ErrCanonicalNumberRange, original)
		}
		result = "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		if point > budget {
			return "", fmt.Errorf("%w: %s", ErrCanonicalNumberRange, original)
		}
		result = digits + strings.Repeat("0", point-len(digits))
	default:
		result = digits[:point] + "." + digits[point:]
	}
	if negative {
		return "-" + result, nil
	}
	return result, nil
}
