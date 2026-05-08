package cleat

import "testing"

func TestVersionWorkflowName(t *testing.T) {
	if WorkflowName != "unknown" {
		t.Errorf("WorkflowName = %q, want %q", WorkflowName, "unknown")
	}
}

func TestVersionWorkflowVersion(t *testing.T) {
	if WorkflowVersion != 0 {
		t.Errorf("WorkflowVersion = %d, want %d", WorkflowVersion, 0)
	}
}

func TestVersionMinVersion(t *testing.T) {
	if MinVersion != 1 {
		t.Errorf("MinVersion = %d, want %d", MinVersion, 1)
	}
}

func TestVersionABIVersion(t *testing.T) {
	if ABIVersion != 1 {
		t.Errorf("ABIVersion = %d, want %d", ABIVersion, 1)
	}
}

func TestVersionPluginDeps(t *testing.T) {
	if PluginDeps != "{}" {
		t.Errorf("PluginDeps = %q, want %q", PluginDeps, "{}")
	}
}
