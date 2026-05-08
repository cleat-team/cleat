package plugin

import (
	"testing"
)

// TestCapabilityEnforcementWASMDeclaresStartWorkflow validates that a WASM
// plugin declaring start_workflow is rejected when limits deny it.
func TestCapabilityEnforcementWASMDeclaresStartWorkflow(t *testing.T) {
	// Simulate a WASM plugin manifest declaring start_workflow.
	declared := CapabilityLimits{
		StartWorkflow: true,
	}

	// Use DefaultLimits which denies start_workflow.
	limits := DefaultLimits()

	err := ValidateCapabilities(declared, limits)
	if err == nil {
		t.Fatal("expected error: WASM plugin declaring start_workflow should be denied")
	}
}

// TestCapabilityEnforcementWASMDeclaresDatabase verifies that a WASM plugin
// declaring database access is accepted when limits grant it.
func TestCapabilityEnforcementWASMDeclaresDatabase(t *testing.T) {
	declared := CapabilityLimits{
		Database: true,
	}
	limits := DefaultLimits()

	if err := ValidateCapabilities(declared, limits); err != nil {
		t.Errorf("expected database to be allowed in default limits, got: %v", err)
	}
}

// TestCapabilityEnforcementWASMDeclaresCallPlugin verifies call_plugin restrictions.
func TestCapabilityEnforcementWASMDeclaresCallPlugin(t *testing.T) {
	// DefaultLimits has CallPlugin: nil (deny all).
	declared := CapabilityLimits{
		CallPlugin: []string{"some-plugin"},
	}
	limits := DefaultLimits()

	err := ValidateCapabilities(declared, limits)
	if err == nil {
		t.Fatal("expected error: call_plugin should be denied by default")
	}
}

// TestCapabilityEnforcementWASMAllAllowed verifies that if limits grant all
// declared capabilities, validation passes.
func TestCapabilityEnforcementWASMAllAllowed(t *testing.T) {
	declared := CapabilityLimits{
		Database:       true,
		SignalWorkflow: true,
		CallPlugin:     []string{"plugin-a"},
	}
	limits := CapabilityLimits{
		Database:       true,
		SignalWorkflow: true,
		CallPlugin:     []string{"plugin-a", "plugin-b"},
	}

	if err := ValidateCapabilities(declared, limits); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}
