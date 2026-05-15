package host

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
