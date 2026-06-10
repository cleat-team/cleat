package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// ---- runEmbedded ----

func TestRunEmbedded_NoArgs(t *testing.T) {
	if os.Getenv("TEST_RUN_EMBEDDED_NO_ARGS") == "1" {
		runEmbedded([]string{})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunEmbedded_NoArgs$")
	cmd.Env = append(os.Environ(), "TEST_RUN_EMBEDDED_NO_ARGS=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit error for no args, got none")
	}
	if !strings.Contains(string(out), "Usage: cleat run") {
		t.Errorf("expected 'Usage: cleat run' in output, got: %s", string(out))
	}
}

func TestRunEmbedded_NoWasmFlag(t *testing.T) {
	if os.Getenv("TEST_RUN_EMBEDDED_NO_WASM") == "1" {
		// Without --wasm and without a package path, it should error.
		runEmbedded([]string{"--target", "go"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunEmbedded_NoWasmFlag$")
	cmd.Env = append(os.Environ(), "TEST_RUN_EMBEDDED_NO_WASM=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit error, got none")
	}
	if !strings.Contains(string(out), "Usage: cleat run") {
		t.Errorf("expected 'Usage: cleat run' in output, got: %s", string(out))
	}
}
