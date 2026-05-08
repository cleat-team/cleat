package host

import (
	"encoding/json"
	"strings"
)

// maxRedactDepth limits the recursion depth when redacting nested JSON values
// to prevent stack overflow from malicious deeply-nested input.
const maxRedactDepth = 50

// sensitivePatterns lists field name substrings whose values should be redacted.
// Matching is done case-insensitively.
var sensitivePatterns = []string{
	"token",
	"secret",
	"password",
	"credential",
	"api_key",
	"authorization",
	"api-key",
}

// isSensitiveField returns true if the field name matches any sensitive pattern.
func isSensitiveField(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range sensitivePatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// looksLikeJWT returns true if the string has three base64-ish segments separated
// by dots, which is the characteristic structure of a JSON Web Token.
func looksLikeJWT(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	// Each segment should be non-empty base64-url (no whitespace, valid chars).
	for _, p := range parts {
		if len(p) == 0 {
			return false
		}
		for _, c := range p {
			if !isBase64URLChar(c) {
				return false
			}
		}
	}
	return true
}

// isBase64URLChar returns true if c is a valid base64url character or '=' padding.
func isBase64URLChar(c rune) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '-' || c == '_' || c == '='
}

// Redact processes a JSON string and returns a redacted version where
// sensitive fields have their values replaced with "[REDACTED]". It handles:
//   - Nested objects recursively
//   - String values that look like JWTs
//   - Field names matching token, secret, password, credential, api_key,
//     authorization, and similar patterns (case-insensitive)
//
// If the input is not valid JSON, it is returned as-is.
func Redact(raw string) string {
	var data interface{}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		// If it's not valid JSON, still check if the whole string looks like a JWT.
		if looksLikeJWT(raw) {
			return `"[REDACTED]"`
		}
		return raw
	}

	result := redactValue(data)
	out, err := json.Marshal(result)
	if err != nil {
		return raw
	}
	return string(out)
}

// redactValue recursively redacts sensitive values in an arbitrary JSON value.
func redactValue(v interface{}) interface{} {
	return redactValueDepth(v, 0)
}

// redactValueDepth recursively redacts sensitive values with a recursion depth
// limit to prevent stack overflow from deeply-nested input.
func redactValueDepth(v interface{}, depth int) interface{} {
	if depth > maxRedactDepth {
		return v
	}
	switch val := v.(type) {
	case map[string]interface{}:
		return redactMapDepth(val, depth)
	case []interface{}:
		return redactSliceDepth(val, depth)
	case string:
		if looksLikeJWT(val) {
			return "[REDACTED]"
		}
		return val
	default:
		return val
	}
}

// redactMap redacts sensitive fields in a JSON object.
func redactMap(m map[string]interface{}) map[string]interface{} {
	return redactMapDepth(m, 0)
}

// redactMapDepth redacts sensitive fields in a JSON object with a recursion
// depth limit.
func redactMapDepth(m map[string]interface{}, depth int) map[string]interface{} {
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		if isSensitiveField(k) {
			result[k] = "[REDACTED]"
		} else {
			result[k] = redactValueDepth(v, depth+1)
		}
	}
	return result
}

// redactSlice recursively redacts each element of a JSON array.
func redactSlice(arr []interface{}) []interface{} {
	return redactSliceDepth(arr, 0)
}

// redactSliceDepth recursively redacts each element of a JSON array with a
// recursion depth limit.
func redactSliceDepth(arr []interface{}, depth int) []interface{} {
	result := make([]interface{}, len(arr))
	for i, v := range arr {
		result[i] = redactValueDepth(v, depth+1)
	}
	return result
}

// RedactMap is like Redact but operates on a map[string]interface{} in-place,
// returning the same map. This is useful when the input is already parsed.
func RedactMap(m map[string]interface{}) {
	for k, v := range m {
		if isSensitiveField(k) {
			m[k] = "[REDACTED]"
		} else {
			switch nested := v.(type) {
			case map[string]interface{}:
				RedactMap(nested)
			}
		}
	}
}
