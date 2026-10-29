package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#3583: --redact-patterns-file loaded its patterns into customPatterns and
// nothing read them, so an operator who named a field there believed it was
// redacted and it was not. TestLoadRedactPatterns could not see it: it asserted
// on "my-secret", which the built-in "secret" already matches.
//
// "private-key" is covered by no built-in, which is what makes this a test of
// the custom list and not of the built-ins. The first case checks that, so a
// future built-in that covers it turns this test into a failure that says so
// instead of a pass that proves nothing.
func TestACustomRedactionPatternRedactsItsField(t *testing.T) {
	defer ResetPatterns()

	const in = `{"private-key":"abc123","name":"hello","monkey":"x"}`
	if got := Redact(in); got != in {
		t.Fatalf("CONTROL: with no custom pattern loaded Redact already rewrites %s to %s, so this test would say nothing about the custom list", in, got)
	}

	path := filepath.Join(t.TempDir(), "patterns.txt")
	if err := os.WriteFile(path, []byte("# fields only this deployment has\nPrivate-Key\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := LoadRedactPatterns(path); err != nil {
		t.Fatalf("LoadRedactPatterns: %v", err)
	}

	got := Redact(in)
	if strings.Contains(got, "abc123") {
		t.Errorf("the custom pattern did not redact its field: %s", got)
	}
	if !strings.Contains(got, `"name":"hello"`) || !strings.Contains(got, `"monkey":"x"`) {
		t.Errorf("a field the pattern does not name was rewritten: %s", got)
	}

	// Case-insensitive, and a substring of the key, as the flag documents.
	for key, want := range map[string]bool{
		"private-key":      true,
		"PRIVATE-KEY":      true,
		"x_private-key_v2": true,
		"private_key":      false, // a different spelling: the pattern is a substring, not a word match
		"private":          false,
		"token_usage":      false, // the built-in exception for descriptors is unchanged
		"password":         true,  // the built-ins still apply beside the custom list
	} {
		if got := isSensitiveField(key); got != want {
			t.Errorf("isSensitiveField(%q) = %v, want %v", key, got, want)
		}
	}

	// The map form and the nested form go through the same test.
	m := map[string]any{"private-key": "abc123", "keep": "me", "nested": map[string]any{"private-key": "def456"}}
	RedactMap(m)
	if m["private-key"] != "[REDACTED]" {
		t.Errorf("RedactMap left the custom field: %v", m["private-key"])
	}
	if n, _ := m["nested"].(map[string]any); n["private-key"] != "[REDACTED]" {
		t.Errorf("RedactMap left the nested custom field: %v", n["private-key"])
	}
	if m["keep"] != "me" {
		t.Errorf("RedactMap rewrote an unnamed field: %v", m["keep"])
	}

	// ResetPatterns takes them back out, so a later test is not affected.
	ResetPatterns()
	if got := Redact(in); got != in {
		t.Errorf("a custom pattern outlived ResetPatterns: %s", got)
	}
}

// A custom pattern that spells a built-in name is a different rule, not a
// duplicate of it. The built-in "token" reads a key as words, so it leaves
// max_tokens, token_usage and tokenizer_name alone; a custom "token" is a
// substring of the whole key and redacts all three. LoadRedactPatterns used to
// drop the line as a duplicate of the built-in (it dated from when the built-ins
// were substrings), so an operator who wrote "token" to catch max_tokens loaded
// nothing and was told nothing.
func TestACustomPatternSpelledLikeABuiltinIsALoadedRule(t *testing.T) {
	defer ResetPatterns()

	const body = `{"max_tokens":"v1","token_usage":"v2","tokenizer_name":"v3","name":"keep"}`

	// The control: with no custom list, the built-in leaves these three alone. If a
	// future change makes the built-in redact them, the case below proves nothing
	// and this says so.
	got := Redact(body)
	for _, v := range []string{"v1", "v2", "v3"} {
		if !strings.Contains(got, v) {
			t.Fatalf("the built-ins already redact the value %q, so this test no longer tests the custom list: %s", v, got)
		}
	}

	path := filepath.Join(t.TempDir(), "patterns.txt")
	if err := os.WriteFile(path, []byte("token\nsecret\npassword\nprivate-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := LoadRedactPatterns(path); err != nil {
		t.Fatalf("LoadRedactPatterns: %v", err)
	}
	if len(customPatterns) != 4 {
		t.Errorf("customPatterns = %v, want all four lines loaded", customPatterns)
	}

	got = Redact(body)
	for _, v := range []string{"v1", "v2", "v3"} {
		if strings.Contains(got, v) {
			t.Errorf("custom \"token\" did not redact the value %q: %s", v, got)
		}
	}
	if !strings.Contains(got, `"name":"keep"`) {
		t.Errorf("a key the pattern does not name was changed: %s", got)
	}
}
