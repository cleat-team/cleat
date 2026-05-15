package plugin_test
import (
	"testing"
	"github.com/cleat-team/cleat/internal/plugin"
	"github.com/cleat-team/cleat/internal/plugingen"
)
// TestManifestRoundTrip verifies: manifest --> IR --> code generation -->
// structurally valid output for all languages.
func TestManifestRoundTrip(t *testing.T) {
	// Load the LLM plugin manifest.
	m, err := plugin.LoadManifest("../../plugins/llm/plugin.json")
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	// Validate it.
	if err := plugin.ValidateManifest(m); err != nil {
		t.Fatalf("validate manifest: %v", err)
	}
	// Generate code for all languages -- just verify no errors.
	// (Full compilation check would require language-specific toolchains.)
	ir, err := plugingen.FromManifest(m)
	if err != nil {
		t.Fatalf("build IR: %v", err)
	}
	langs := []string{"typescript", "python", "rust", "go"}
	for _, lang := range langs {
		// Just verify generation doesn't panic or error.
		var code string
		switch lang {
		case "typescript":
			code, err = plugingen.GenerateTypeScript(ir)
		case "python":
			code, err = plugingen.GeneratePython(ir)
		case "rust":
			code, err = plugingen.GenerateRust(ir)
		case "go":
			code, err = plugingen.GenerateGo(ir)
		}
		if err != nil {
			t.Errorf("generate %s: %v", lang, err)
		}
		if len(code) == 0 {
			t.Errorf("generate %s: empty output", lang)
		}
		t.Logf("%s output: %d bytes", lang, len(code))
	}
}
// TestManifestRoundTrip_HelloWorld is the same test but for the example
// third-party plugin manifest to ensure community plugins work too.
func TestManifestRoundTrip_HelloWorld(t *testing.T) {
	m, err := plugin.LoadManifest("../../examples/third-party-plugin/plugin.json")
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if err := plugin.ValidateManifest(m); err != nil {
		t.Fatalf("validate manifest: %v", err)
	}
	ir, err := plugingen.FromManifest(m)
	if err != nil {
		t.Fatalf("build IR: %v", err)
	}
	langs := []string{"typescript", "python", "rust", "go"}
	for _, lang := range langs {
		var code string
		switch lang {
		case "typescript":
			code, err = plugingen.GenerateTypeScript(ir)
		case "python":
			code, err = plugingen.GeneratePython(ir)
		case "rust":
			code, err = plugingen.GenerateRust(ir)
		case "go":
			code, err = plugingen.GenerateGo(ir)
		}
		if err != nil {
			t.Errorf("generate %s: %v", lang, err)
		}
		if len(code) == 0 {
			t.Errorf("generate %s: empty output", lang)
		}
		t.Logf("%s output: %d bytes", lang, len(code))
	}
}
// TestCapabilityEnforcementEndToEnd verifies the full capability enforcement
// pipeline: defaults --> validate --> limits.
func TestCapabilityEnforcementEndToEnd(t *testing.T) {
	// Community plugin with start_workflow: true should be rejected by defaults.
	declared := plugin.CapabilityLimits{
		Database:      plugin.DatabaseAccessReadWrite,
		StartWorkflow: true,
	}
	limits := plugin.DefaultLimits()
	err := plugin.ValidateCapabilities(declared, limits)
	if err == nil {
		t.Error("expected start_workflow to be denied by default limits")
	} else {
		t.Logf("correctly rejected: %v", err)
	}
	// Plugin with only signal_workflow (allowed by defaults) should pass defaults.
	declared2 := plugin.CapabilityLimits{
		SignalWorkflow: true,
	}
	if err := plugin.ValidateCapabilities(declared2, limits); err != nil {
		t.Errorf("expected no violation: %v", err)
	}
}
// TestMinimalManifest validates a minimal (no host functions) plugin manifest.
func TestMinimalManifest(t *testing.T) {
	m := &plugin.Manifest{
		Name:        "test/empty",
		Version:     "0.1.0",
		Description: "A plugin with no host functions",
		Author:      "test",
	}
	if err := plugin.ValidateManifest(m); err != nil {
		t.Fatalf("validate minimal manifest: %v", err)
	}
	ir, err := plugingen.FromManifest(m)
	if err != nil {
		t.Fatalf("build IR: %v", err)
	}
	if len(ir.HostFunctions) != 0 {
		t.Errorf("expected 0 host functions, got %d", len(ir.HostFunctions))
	}
	// Generation should produce valid (minimal) output for all languages.
	for _, lang := range []string{"typescript", "python", "rust", "go"} {
		var code string
		switch lang {
		case "typescript":
			code, err = plugingen.GenerateTypeScript(ir)
		case "python":
			code, err = plugingen.GeneratePython(ir)
		case "rust":
			code, err = plugingen.GenerateRust(ir)
		case "go":
			code, err = plugingen.GenerateGo(ir)
		}
		if err != nil {
			t.Errorf("generate %s: %v", lang, err)
		}
		if len(code) == 0 {
			t.Errorf("generate %s: empty output", lang)
		}
	}
}
