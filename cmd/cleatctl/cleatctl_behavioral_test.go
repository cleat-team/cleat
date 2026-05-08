package main

import (
	"strings"
	"testing"
)

func TestPrintUsage_AllCommands(t *testing.T) {
	output := captureStderr(t, printUsage)
	for _, exp := range []string{
		"cleatctl", "versions list", "versions gc",
		"deploy workflow", "deploy plugin", "CLEAT_DB_URL",
	} {
		if !strings.Contains(output, exp) {
			t.Errorf("expected %q in output", exp)
		}
	}
}

func TestPrintDeployUsage_All(t *testing.T) {
	output := captureStderr(t, printDeployUsage)
	for _, exp := range []string{"workflow", "plugin", "WASM binary"} {
		if !strings.Contains(output, exp) {
			t.Errorf("expected %q in output", exp)
		}
	}
}

func TestPrintVersionsUsage_All(t *testing.T) {
	output := captureStderr(t, printVersionsUsage)
	for _, exp := range []string{"list", "deprecate", "restore", "purge", "active"} {
		if !strings.Contains(output, exp) {
			t.Errorf("expected %q in output", exp)
		}
	}
}
