package main

import (
	"testing"

	"github.com/cleat-team/cleat/cleat/cleattest"
)

func TestProcess(t *testing.T) {
	env := cleattest.NewTestEnv()

	// Example: stub a plugin call
	env.OnPluginCall("llm", "analyze").Return(`{"choices":[{"message":{"content":"analysis complete"}}]}`, nil)

	result, err := Process(env.H(), "test-input")
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	if result != "processed: test-input" {
		t.Errorf("expected 'processed: test-input', got %q", result)
	}
}
