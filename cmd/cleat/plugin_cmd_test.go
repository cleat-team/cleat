package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// ---- runPlugin ----

func TestRunPlugin_NoArgs(t *testing.T) {
	if os.Getenv("TEST_PLUGIN_NO_ARGS") == "1" {
		runPlugin([]string{})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunPlugin_NoArgs$")
	cmd.Env = append(os.Environ(), "TEST_PLUGIN_NO_ARGS=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit error for no args, got none")
	}
	if !strings.Contains(string(out), "Usage: cleat plugin") {
		t.Errorf("expected 'Usage: cleat plugin' in output, got: %s", string(out))
	}
}

func TestRunPlugin_UnknownSubcommand(t *testing.T) {
	if os.Getenv("TEST_PLUGIN_UNKNOWN") == "1" {
		runPlugin([]string{"bogus", "arg"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunPlugin_UnknownSubcommand$")
	cmd.Env = append(os.Environ(), "TEST_PLUGIN_UNKNOWN=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit error for unknown subcommand, got none")
	}
	if !strings.Contains(string(out), "Unknown plugin subcommand") {
		t.Errorf("expected 'Unknown plugin subcommand' in output, got: %s", string(out))
	}
}
