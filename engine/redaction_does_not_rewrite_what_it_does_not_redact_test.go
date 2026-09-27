package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactIsTheIdentityWhenNothingIsSensitive is the property the replay path
// depends on and the old implementation could not provide.
//
// Redact runs on the READ path over ten fields of every event record
// (store_events.go and its MySQL and SQL Server twins), and replay loads its
// history through that read -- so whatever Redact does to a payload is what a
// resumed workflow sees instead of what its first run saw. Anything short of
// byte equality here is the engine introducing the non-determinism it exists to
// prevent.
func TestRedactIsTheIdentityWhenNothingIsSensitive(t *testing.T) {
	cases := []string{
		// Key order: a Go map has none, so the round trip sorted these.
		`{"zebra":1,"apple":2,"mango":3}`,
		`{"nested":{"z":1,"a":2},"outer":"v"}`,

		// Numbers: every one of these came back a different literal, and the
		// first two came back a different VALUE -- json.Unmarshal into `any`
		// routes through float64, which cannot hold an integer above 2^53.
		`{"id":12345678901234567890}`,
		`{"id":9007199254740993}`,
		`{"amount":1.0}`,
		`{"big":1e300}`,
		`{"neg":-0.0}`,

		// Formatting the caller chose.
		"{\n  \"a\": 1,\n  \"b\": [1, 2, 3]\n}",
		`{"unicode":"café","escaped":"a\"b"}`,

		// Shapes that are valid JSON but not objects.
		`[3,1,2]`,
		`"plain string"`,
		`42`,
		`null`,
		`true`,

		// Not JSON at all: returned untouched, as before.
		`not json at all`,
		``,
	}

	for _, in := range cases {
		if got := Redact(in); got != in {
			t.Errorf("Redact rewrote a payload with nothing sensitive in it:\n  in:  %s\n  out: %s", in, got)
		}
	}
}

// TestRedactChangesOnlyTheRedactedSpan pins the other half: when something IS
// hidden, everything around it survives unchanged. A fix that preserved bytes
// by declining to redact would pass the test above and be useless.
func TestRedactChangesOnlyTheRedactedSpan(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "sensitive key, neighbours keep order and precision",
			in:   `{"zebra":12345678901234567890,"token":"pm_tok_555","apple":1.0}`,
			want: `{"zebra":12345678901234567890,"token":"[REDACTED]","apple":1.0}`,
		},
		{
			name: "whole subtree under a sensitive key becomes one placeholder",
			in:   `{"credentials":{"user":"u","pass":"p"},"keep":{"z":1,"a":2}}`,
			want: `{"credentials":"[REDACTED]","keep":{"z":1,"a":2}}`,
		},
		{
			name: "jwt by shape, not by key name",
			in:   `{"opaque":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig","n":9007199254740993}`,
			want: `{"opaque":"[REDACTED]","n":9007199254740993}`,
		},
		{
			name: "inside an array",
			in:   `{"list":[{"api_key":"k","id":1},{"id":2}]}`,
			want: `{"list":[{"api_key":"[REDACTED]","id":1},{"id":2}]}`,
		},
		{
			name: "whitespace the caller chose is preserved around the change",
			in:   "{\n  \"password\": \"p\",\n  \"id\": 7\n}",
			want: "{\n  \"password\": \"[REDACTED]\",\n  \"id\": 7\n}",
		},
		{
			name: "a colon inside a string value is not mistaken for structure",
			in:   `{"url":"https://h/a:b,c","token":"t"}`,
			want: `{"url":"https://h/a:b,c","token":"[REDACTED]"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Redact(tt.in)
			if got != tt.want {
				t.Errorf("\n  in:   %s\n  got:  %s\n  want: %s", tt.in, got, tt.want)
			}
			if !json.Valid([]byte(got)) {
				t.Errorf("output is not valid JSON: %s", got)
			}
		})
	}
}

// TestRedactStillHidesEverythingItUsedTo runs the built-in pattern list through
// the new implementation, so "preserves bytes" cannot be achieved by redacting
// less than before.
func TestRedactStillHidesEverythingItUsedTo(t *testing.T) {
	for _, p := range sensitivePatterns {
		in := `{"x":1,"my_` + p + `_field":"s3cret","y":2}`
		got := Redact(in)
		if strings.Contains(got, "s3cret") {
			t.Errorf("pattern %q no longer redacts: %s", p, got)
		}
		if !strings.Contains(got, `"x":1`) || !strings.Contains(got, `"y":2`) {
			t.Errorf("pattern %q damaged its neighbours: %s", p, got)
		}
	}
}

// TestRedactSurvivesDeepNesting keeps the stack-overflow guard honest: the
// old implementation recursed per level and capped at maxRedactDepth, and the
// new one recurses too, so the bound still has to hold.
func TestRedactSurvivesDeepNesting(t *testing.T) {
	const levels = 20000
	in := strings.Repeat(`{"a":`, levels) + `1` + strings.Repeat(`}`, levels)

	done := make(chan string, 1)
	go func() { done <- Redact(in) }()
	got := <-done

	// Depth beyond the cap is left alone rather than rewritten, so the only
	// requirement is that it returns something and does not panic.
	if got == "" {
		t.Error("Redact returned empty for deeply nested input")
	}
}
