package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactSensitiveFields(t *testing.T) {
	input := `{"token": "secret-value", "name": "public"}`
	got := Redact(input)
	if !strings.Contains(got, `"[REDACTED]"`) {
		t.Errorf("expected token value to be redacted, got: %s", got)
	}
	if strings.Contains(got, "secret-value") {
		t.Errorf("token value should not appear in output: %s", got)
	}
	if !strings.Contains(got, "public") {
		t.Errorf("non-sensitive field should be preserved: %s", got)
	}
}

func TestRedactCaseInsensitive(t *testing.T) {
	input := `{"TOKEN": "abc123", "Api-Key": "xyz789", "name": "hello"}`
	got := Redact(input)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	if parsed["TOKEN"] != "[REDACTED]" {
		t.Errorf("TOKEN should be [REDACTED], got %v", parsed["TOKEN"])
	}
	if parsed["Api-Key"] != "[REDACTED]" {
		t.Errorf("Api-Key should be [REDACTED], got %v", parsed["Api-Key"])
	}
	if parsed["name"] != "hello" {
		t.Errorf("name should be preserved, got %v", parsed["name"])
	}
}

func TestRedactJWT(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyMTIzIn0.a1b2c3d4e5f6g7h8i9j0"
	input := `{"jwt": "` + jwt + `"}`
	got := Redact(input)
	if !strings.Contains(got, `"[REDACTED]"`) {
		t.Errorf("expected JWT to be redacted, got: %s", got)
	}
}

func TestRedactPreservesNonSensitive(t *testing.T) {
	input := `{"message": "hello world", "count": 42, "active": true}`
	got := Redact(input)
	if !strings.Contains(got, "hello world") {
		t.Errorf("non-sensitive message should be preserved: %s", got)
	}
	if !strings.Contains(got, "42") {
		t.Errorf("non-sensitive count should be preserved: %s", got)
	}
	if !strings.Contains(got, "true") {
		t.Errorf("non-sensitive active should be preserved: %s", got)
	}
}

func TestRedactNestedObject(t *testing.T) {
	input := `{"user": {"token": "abc", "profile": {"name": "Alice"}}}`
	got := Redact(input)
	if strings.Contains(got, "abc") {
		t.Errorf("nested token value should be redacted: %s", got)
	}
	if !strings.Contains(got, "Alice") {
		t.Errorf("nested non-sensitive value should be preserved: %s", got)
	}
}

func TestRedactNonJSON(t *testing.T) {
	input := "plain text"
	got := Redact(input)
	if got != input {
		t.Errorf("non-JSON input should be returned as-is, got: %s", got)
	}
}

func TestRedactOnRead(t *testing.T) {
	// RedactOnRead should behave identically to Redact.
	input := `{"token": "secret", "name": "hello"}`
	got := RedactOnRead(input)
	if !strings.Contains(got, `"[REDACTED]"`) {
		t.Errorf("RedactOnRead should redact sensitive fields: %s", got)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("RedactOnRead should preserve non-sensitive fields: %s", got)
	}
}

func TestLoadRedactPatterns(t *testing.T) {
	defer ResetPatterns()

	// Create a temp patterns file.
	dir := t.TempDir()
	path := filepath.Join(dir, "patterns.txt")
	content := "# custom patterns\nmy-secret\nprivate-key\n\napi-token\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write patterns file: %v", err)
	}

	if err := LoadRedactPatterns(path); err != nil {
		t.Fatalf("LoadRedactPatterns failed: %v", err)
	}

	if !patternsFileLoaded {
		t.Error("patternsFileLoaded should be true after loading")
	}

	// Check that custom patterns are used in redaction.
	input := `{"my-secret": "sensitive-data", "private-key": "abc123", "name": "hello"}`
	got := Redact(input)
	if !strings.Contains(got, `"[REDACTED]"`) {
		t.Errorf("custom patterns should redact fields: %s", got)
	}
	if strings.Contains(got, "sensitive-data") {
		t.Errorf("custom pattern value should be redacted: %s", got)
	}
}

func TestLoadRedactPatternsMissingFile(t *testing.T) {
	err := LoadRedactPatterns("/nonexistent/file.txt")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestRedactMap_RecursesIntoArrays(t *testing.T) {
	input := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{"token": "secret-in-array"},
		},
	}
	RedactMap(input)
	items := input["items"].([]interface{})
	first := items[0].(map[string]interface{})
	if first["token"] != "[REDACTED]" {
		t.Errorf("array element not redacted: got %v", first["token"])
	}
}

func TestRedactMap(t *testing.T) {
	m := map[string]interface{}{
		"token": "secret",
		"name":  "Alice",
		"nested": map[string]interface{}{
			"password": "p@ss",
			"color":    "blue",
		},
	}
	RedactMap(m)
	if m["token"] != "[REDACTED]" {
		t.Errorf("token should be redacted, got %v", m["token"])
	}
	if m["name"] != "Alice" {
		t.Errorf("name should be preserved, got %v", m["name"])
	}
	nested := m["nested"].(map[string]interface{})
	if nested["password"] != "[REDACTED]" {
		t.Errorf("nested password should be redacted, got %v", nested["password"])
	}
	if nested["color"] != "blue" {
		t.Errorf("nested color should be preserved, got %v", nested["color"])
	}
}

// ---------------------------------------------------------------------------
// Direct unit tests for unexported helpers and edge cases
// ---------------------------------------------------------------------------

func TestIsSensitiveField(t *testing.T) {
	tests := []struct {
		name      string
		sensitive bool
	}{
		{"token", true},
		{"TOKEN", true},
		{"my_token", true},
		{"authorization", true},
		{"Authorization", true},
		{"api_key", true},
		{"api-key", true},
		{"API-KEY", true},
		{"credential", true},
		{"password", true},
		{"secret", true},
		{"name", false},
		{"message", false},
		{"count", false},
		{"id", false},
	}
	for _, tc := range tests {
		got := isSensitiveField(tc.name)
		if got != tc.sensitive {
			t.Errorf("isSensitiveField(%q) = %v, want %v", tc.name, got, tc.sensitive)
		}
	}
}

func TestLooksLikeJWT_EdgeCases(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		// Standard three-segment JWT.
		{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signature", true},
		// Wrong number of parts.
		{"part1.part2", false},
		{"part1.part2.part3.part4", false},
		// Empty segment.
		{"part1..part3", false},
		// Invalid base64url characters.
		{"part1!part2.part3!part4.part5!part6", false},
		// Empty string.
		{"", false},
		// Single part (no dots).
		{"just-a-string", false},
		// Whitespace in segment.
		{"part1 part2.part3.part4", false},
	}
	for _, tc := range tests {
		got := looksLikeJWT(tc.input)
		if got != tc.want {
			t.Errorf("looksLikeJWT(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestIsBase64URLChar(t *testing.T) {
	tests := []struct {
		c    rune
		want bool
	}{
		// Valid characters.
		{'a', true}, {'z', true}, {'A', true}, {'Z', true},
		{'0', true}, {'9', true},
		{'-', true}, {'_', true}, {'=', true},
		// Invalid characters.
		{'!', false}, {'@', false}, {'#', false}, {' ', false},
		{'+', false}, {'/', false}, {'.', false},
	}
	for _, tc := range tests {
		got := isBase64URLChar(tc.c)
		if got != tc.want {
			t.Errorf("isBase64URLChar(%q) = %v, want %v", tc.c, got, tc.want)
		}
	}
}

func TestRedact_NonJSON_JWT(t *testing.T) {
	// A non-JSON string that looks like a JWT should be fully redacted.
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.sig"
	got := Redact(jwt)
	if got != "[REDACTED]" {
		t.Errorf("Redact(non-JSON JWT) = %q, want %q", got, "[REDACTED]")
	}
}

func TestRedactSlice(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{"token": "secret", "name": "hello"},
		"plain-string",
		42,
		[]interface{}{"nested-list"},
	}
	result := redactSlice(input)
	if len(result) != 4 {
		t.Fatalf("len = %d, want 4", len(result))
	}
	// First element: map with redacted field.
	m, ok := result[0].(map[string]interface{})
	if !ok {
		t.Fatal("first element should be a map")
	}
	if m["token"] != "[REDACTED]" {
		t.Errorf("token in slice should be [REDACTED], got %v", m["token"])
	}
	if m["name"] != "hello" {
		t.Errorf("name should be preserved, got %v", m["name"])
	}
	// Non-map elements should pass through.
	if result[1] != "plain-string" {
		t.Errorf("string element = %v, want %q", result[1], "plain-string")
	}
	if result[2] != 42 {
		t.Errorf("int element = %v, want 42", result[2])
	}
}

func TestRedactMap_NonRecursedTypes(t *testing.T) {
	m := map[string]interface{}{
		"name":   "Alice",
		"count":  42,
		"active": true,
		"data":   nil,
	}
	RedactMap(m)
	if m["name"] != "Alice" {
		t.Errorf("name should be preserved, got %v", m["name"])
	}
	if m["count"] != 42 {
		t.Errorf("count should be preserved, got %v", m["count"])
	}
	if m["active"] != true {
		t.Errorf("active should be preserved, got %v", m["active"])
	}
	if m["data"] != nil {
		t.Errorf("data should be preserved as nil, got %v", m["data"])
	}
}

func TestRedactMap_DeeplyNested(t *testing.T) {
	m := map[string]interface{}{
		"nested": map[string]interface{}{
			"deep": map[string]interface{}{
				"token": "deep-secret",
				"color": "red",
			},
		},
	}
	RedactMap(m)
	nested := m["nested"].(map[string]interface{})
	deep := nested["deep"].(map[string]interface{})
	if deep["token"] != "[REDACTED]" {
		t.Errorf("deeply nested token should be redacted, got %v", deep["token"])
	}
	if deep["color"] != "red" {
		t.Errorf("deeply nested color should be preserved, got %v", deep["color"])
	}
}

func TestResetPatterns(t *testing.T) {
	// Load a pattern, then reset.
	defer ResetPatterns()

	customPatterns = append(customPatterns, "test-pattern")
	patternsFileLoaded = true

	ResetPatterns()
	if len(customPatterns) != 0 {
		t.Errorf("customPatterns should be empty after ResetPatterns, got %d", len(customPatterns))
	}
	if patternsFileLoaded {
		t.Error("patternsFileLoaded should be false after ResetPatterns")
	}
}

func TestLoadRedactPatterns_Duplicates(t *testing.T) {
	defer ResetPatterns()

	// Load patterns that duplicate built-in sensitive patterns.
	dir := t.TempDir()
	path := dir + "/dups.txt"
	content := "# duplicate test\ntoken\nsecret\nmy-custom\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := LoadRedactPatterns(path); err != nil {
		t.Fatalf("LoadRedactPatterns: %v", err)
	}

	// "token" and "secret" are duplicates (in sensitivePatterns) and should not be added.
	// Only "my-custom" is genuinely new.
	if len(customPatterns) != 1 || customPatterns[0] != "my-custom" {
		t.Errorf("customPatterns = %v, want [my-custom]", customPatterns)
	}
}
