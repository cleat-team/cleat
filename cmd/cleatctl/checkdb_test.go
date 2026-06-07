package main

import (
	"strings"
	"testing"
)

func TestPrintCheckDBUsage(t *testing.T) {
	stderr := captureStderr(t, func() {
		printCheckDBUsage()
	})

	checks := []string{
		"Usage:",
		"cleatctl check-db",
		"Database ping",
		"Schema migration version",
		"Table accessibility",
		"Workflow instance counts",
		"Event history size",
		"--verbose",
		"CLEAT_DB_URL",
	}

	for _, check := range checks {
		if !strings.Contains(stderr, check) {
			t.Errorf("printCheckDBUsage() output missing %q", check)
		}
	}
}
