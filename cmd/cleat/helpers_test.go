package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// plugin_cmd.go — parsePluginSpec
// ---------------------------------------------------------------------------

func TestParsePluginSpec(t *testing.T) {
	tests := []struct {
		spec             string
		wantName         string
		wantConstraint   string
	}{
		{"my-plugin@^1.0.0", "my-plugin", "^1.0.0"},
		{"my-plugin", "my-plugin", ""},
		{"@v1.0.0", "", "v1.0.0"},
		{"a/b@1.0", "a/b", "1.0"},
		{"", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			name, constraint := parsePluginSpec(tt.spec)
			if name != tt.wantName {
				t.Errorf("parsePluginSpec(%q) name = %q, want %q", tt.spec, name, tt.wantName)
			}
			if constraint != tt.wantConstraint {
				t.Errorf("parsePluginSpec(%q) constraint = %q, want %q", tt.spec, constraint, tt.wantConstraint)
			}
		})
	}
}

func TestParsePluginSpec_AtBoundaries(t *testing.T) {
	// Multiple @ signs
	name, constraint := parsePluginSpec("a@b@c")
	if name != "a" {
		t.Errorf("name = %q, want %q", name, "a")
	}
	if constraint != "b@c" {
		t.Errorf("constraint = %q, want %q", constraint, "b@c")
	}
}

// ---------------------------------------------------------------------------
// plugin_cmd.go — ensureV
// ---------------------------------------------------------------------------

func TestEnsureV(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"1.0.0", "v1.0.0"},
		{"v1.0.0", "v1.0.0"},
		{"", "v"},
		{"v", "v"},
		{"0.0.1", "v0.0.1"},
		{"V1.0.0", "vV1.0.0"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ensureV(tt.input)
			if got != tt.want {
				t.Errorf("ensureV(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// run_embedded.go — truncate
// ---------------------------------------------------------------------------

func TestTruncate(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
		{"abc", 0, "..."},
		{"short", 100, "short"},
		{"exact", 5, "exact"},
		{"exact.", 6, "exact."},
	}
	for _, tt := range tests {
		name := fmt.Sprintf("%s_%d", tt.input, tt.maxLen)
		t.Run(name, func(t *testing.T) {
			got := truncate(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// main.go — logBuildProgress
// ---------------------------------------------------------------------------

func TestLogBuildProgress(t *testing.T) {
	// Should not panic with either jsonOut value.
	// Coverage: jsonOut=true writes to stderr, jsonOut=false writes to stdout.
	t.Run("jsonOut_true", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		oldStderr := os.Stderr
		os.Stderr = w

		logBuildProgress("json-msg-%s", true, "test")

		w.Close()
		os.Stderr = oldStderr
		var buf bytes.Buffer
		io.Copy(&buf, r)
		if !strings.Contains(buf.String(), "json-msg-test") {
			t.Errorf("expected 'json-msg-test' on stderr, got %q", buf.String())
		}
	})

	t.Run("jsonOut_false", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		oldStdout := os.Stdout
		os.Stdout = w

		logBuildProgress("stdout-msg-%s", false, "test")

		w.Close()
		os.Stdout = oldStdout
		var buf bytes.Buffer
		io.Copy(&buf, r)
		if !strings.Contains(buf.String(), "stdout-msg-test") {
			t.Errorf("expected 'stdout-msg-test' on stdout, got %q", buf.String())
		}
	})
}
