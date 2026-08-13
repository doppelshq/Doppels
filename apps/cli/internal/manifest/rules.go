package manifest

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	MinSafeInteger int64 = -9007199254740991
	MaxSafeInteger int64 = 9007199254740991
)

var (
	identifierPattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*$`)
	semverPattern          = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	environmentPattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	productEnvPattern      = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	commandPattern         = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	durationPattern        = regexp.MustCompile(`^[1-9][0-9]*(ms|s|m|h)$`)
	labelPattern           = regexp.MustCompile(`^[a-z][a-z0-9._/-]{0,62}$`)
	annotationPattern      = regexp.MustCompile(`^[a-z][a-z0-9._/-]{0,253}$`)
	constraintTokenPattern = regexp.MustCompile(`^(>=|>|<=|<|=|~|\^)?[0-9]+\.[0-9]+\.[0-9]+$`)
)

func validIdentifier(value string) bool {
	return utf8.RuneCountInString(value) <= 63 && identifierPattern.MatchString(value)
}

func validRelativePath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, `\`) || hasWindowsDrivePrefix(value) {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func hasWindowsDrivePrefix(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	first := value[0]
	return first >= 'A' && first <= 'Z' || first >= 'a' && first <= 'z'
}

func validVersionConstraint(value string) bool {
	parts := strings.Fields(value)
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if !constraintTokenPattern.MatchString(part) {
			return false
		}
	}
	return true
}

func scalarMatches(value any, kind string) bool {
	switch kind {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		switch number := value.(type) {
		case int:
			return int64(number) >= MinSafeInteger && int64(number) <= MaxSafeInteger
		case int8:
			return true
		case int16:
			return true
		case int32:
			return true
		case int64:
			return number >= MinSafeInteger && number <= MaxSafeInteger
		case uint:
			return uint64(number) <= uint64(MaxSafeInteger)
		case uint8:
			return true
		case uint16:
			return true
		case uint32:
			return uint64(number) <= uint64(MaxSafeInteger)
		case uint64:
			return number <= uint64(MaxSafeInteger)
		case float32:
			return !float32IsInvalid(number) && number == float32(int64(number)) && float64(number) >= float64(MinSafeInteger) && float64(number) <= float64(MaxSafeInteger)
		case float64:
			return !math.IsNaN(number) && !math.IsInf(number, 0) && number == math.Trunc(number) && number >= float64(MinSafeInteger) && number <= float64(MaxSafeInteger)
		default:
			return false
		}
	case "number":
		switch number := value.(type) {
		case int:
			return int64(number) >= MinSafeInteger && int64(number) <= MaxSafeInteger
		case int8:
			return true
		case int16:
			return true
		case int32:
			return true
		case int64:
			return number >= MinSafeInteger && number <= MaxSafeInteger
		case uint:
			return uint64(number) <= uint64(MaxSafeInteger)
		case uint8:
			return true
		case uint16:
			return true
		case uint32:
			return uint64(number) <= uint64(MaxSafeInteger)
		case uint64:
			return number <= uint64(MaxSafeInteger)
		case float32:
			return portableFloat(float64(number))
		case float64:
			return portableFloat(number)
		default:
			return false
		}
	default:
		return false
	}
}

func portableFloat(value float64) bool {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return false
	}
	return value != math.Trunc(value) || value >= float64(MinSafeInteger) && value <= float64(MaxSafeInteger)
}

func float32IsInvalid(value float32) bool {
	return float32(math.Inf(1)) == value || float32(math.Inf(-1)) == value || value != value
}

func scalarEqual(left, right any) bool {
	if isNumber(left) && isNumber(right) {
		leftNumber, leftOK := toFloat64(left)
		rightNumber, rightOK := toFloat64(right)
		return leftOK && rightOK && leftNumber == rightNumber
	}
	return reflect.DeepEqual(left, right)
}

func isNumber(value any) bool {
	_, ok := toFloat64(value)
	return ok
}

func toFloat64(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int8:
		return float64(number), true
	case int16:
		return float64(number), true
	case int32:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint:
		return float64(number), true
	case uint8:
		return float64(number), true
	case uint16:
		return float64(number), true
	case uint32:
		return float64(number), true
	case uint64:
		return float64(number), true
	case float32:
		return float64(number), !float32IsInvalid(number)
	case float64:
		return number, !math.IsNaN(number) && !math.IsInf(number, 0)
	default:
		return 0, false
	}
}

func containsScalar(values []any, wanted any) bool {
	for _, value := range values {
		if scalarEqual(value, wanted) {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func inputSignature(input InputContract) string {
	return fmt.Sprintf("%s|%t|%t|%s|%s", input.Type, input.Required, input.DefaultSet, scalarKey(input.Default), scalarListKey(input.Enum))
}

func outputSignature(output OutputContract) string {
	return output.Type + "|" + output.MediaType
}

func scalarKey(value any) string {
	if value == nil {
		return "<nil>"
	}
	if number, ok := toFloat64(value); ok {
		return "number:" + strconv.FormatFloat(number, 'g', -1, 64)
	}
	return fmt.Sprintf("%T:%v", value, value)
}

func scalarListKey(values []any) string {
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = scalarKey(value)
	}
	return strings.Join(parts, ",")
}
