package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCanonicalNumberSpelling pins the decimal normalization: a canonical
// number is the exact decimal expansion of the input, without exponent,
// leading zeros or trailing fraction zeros, and with the sign preserved.
// Fractional values are the regression: normalizing 0.1 to 1 would make two
// different inputs share a request_fingerprint (RFC §12).
func TestCanonicalNumberSpelling(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"0.1", "0.1"},
		{"0.01", "0.01"},
		{"0.10", "0.1"},
		{"-0.5", "-0.5"},
		{"1e08", "100000000"},
		{"1.5e-3", "0.0015"},
		{"-1.5e-3", "-0.0015"},
		{"-2", "-2"},
		{"-0", "0"},
		{"0.0", "0"},
		{"12345678901234567890", "12345678901234567890"},
		{"9007199254740993.0", "9007199254740993"},
	}
	for _, tc := range cases {
		got, err := CanonicalJSON(json.RawMessage(tc.raw))
		if err != nil {
			t.Errorf("%s: %v", tc.raw, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s canonicalized to %s, want %s", tc.raw, got, tc.want)
		}
	}
}

// TestCanonicalNumberDistinguishesFractions guards the fingerprint against
// collapsing distinct fractional inputs onto the same digest.
func TestCanonicalNumberDistinguishesFractions(t *testing.T) {
	spellings := []string{"0.1", "0.01", "1", "10"}
	digests := make(map[string]string)
	for _, spelling := range spellings {
		digest, err := Fingerprint(json.RawMessage(spelling))
		if err != nil {
			t.Fatalf("%s: %v", spelling, err)
		}
		if previous, ok := digests[digest]; ok {
			t.Fatalf("%s and %s share a fingerprint", previous, spelling)
		}
		digests[digest] = spelling
	}
}

// TestCanonicalNumberRejectsUnrepresentableExponents pins that an absurd
// exponent is a protocol error, not a panic or a multi-gigabyte allocation:
// a remote client must not be able to kill the Runner with one frame.
func TestCanonicalNumberRejectsUnrepresentableExponents(t *testing.T) {
	for _, raw := range []string{
		`1e9223372036854775807`,
		`1e-9223372036854775808`,
		`-1e999999999999999999999`,
		`1e100000`,
		`1e-100000`,
	} {
		func() {
			defer func() {
				if value := recover(); value != nil {
					t.Errorf("%s panicked: %v", raw, value)
				}
			}()
			canonical, err := CanonicalJSON(json.RawMessage(raw))
			if err == nil {
				t.Errorf("%s accepted, canonical = %s", raw, canonical)
			}
		}()
	}
}

// TestCanonicalNumberAcceptsLargeButBoundedExponents keeps legitimate
// scientific notation working up to the documented digit budget.
func TestCanonicalNumberAcceptsLargeButBoundedExponents(t *testing.T) {
	canonical, err := CanonicalJSON(json.RawMessage(`1e64`))
	if err != nil {
		t.Fatal(err)
	}
	want := "1" + strings.Repeat("0", 64)
	if string(canonical) != want {
		t.Fatalf("canonical = %s, want %s", canonical, want)
	}
}

// TestCanonicalJSONRejectsDuplicateKeys fixes the policy for a payload whose
// meaning depends on the parser: a fingerprint over "last one wins" would
// make two clients disagree about the same bytes.
func TestCanonicalJSONRejectsDuplicateKeys(t *testing.T) {
	for _, raw := range []string{
		`{"x":1,"x":2}`,
		`{"x":1,"x":2}`,
		`{"a":{"b":1,"b":2}}`,
		`[{"k":1,"k":1}]`,
	} {
		canonical, err := CanonicalJSON(json.RawMessage(raw))
		if err == nil {
			t.Errorf("%s accepted, canonical = %s", raw, canonical)
		}
	}
}

// TestCanonicalJSONUnescapesKeysBeforeSorting pins that the sort operates on
// decoded key bytes, so an escaped key and its literal form agree.
func TestCanonicalJSONUnescapesKeysBeforeSorting(t *testing.T) {
	escaped, err := CanonicalJSON(json.RawMessage(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	literal, err := CanonicalJSON(json.RawMessage(`{"a":1,"b":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(escaped) != string(literal) {
		t.Fatalf("escaped = %s, literal = %s", escaped, literal)
	}
}
