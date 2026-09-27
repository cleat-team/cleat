package wasm

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteManualJSONHelpers(t *testing.T) {
	var buf bytes.Buffer
	writeManualJSONHelpers(&buf)
	code := buf.String()

	checks := []string{
		"func buildJSONStringArray",
		"func parseSimpleResult",
		"func parseChildResultArray",
	}
	for _, c := range checks {
		if !strings.Contains(code, c) {
			t.Errorf("expected output to contain: %s", c)
		}
	}
}

func TestGenerateField_DurableCall(t *testing.T) {
	var buf bytes.Buffer
	hf := HostFunction{ImportName: "cleat_call", FieldName: "DurableCall"}
	adef := adapterDefs["DurableCall"]

	generateField(&buf, hf, adef)
	code := buf.String()

	checks := []string{
		"DurableCall: func(",
		"service string",
		"operation string",
		"requestJSON string",
		"(string, error)",
		"cleatCallImport(",
	}
	for _, c := range checks {
		if !strings.Contains(code, c) {
			t.Errorf("expected output to contain: %s", c)
		}
	}
}

func TestNeedsJSON(t *testing.T) {
	// PluginCallStreaming uses json.Unmarshal.
	usage := &UsageInfo{
		Used: map[string]bool{"plugin_call_streaming": true},
		Funcs: []HostFunction{
			{ImportName: "plugin_call_streaming", FieldName: "PluginCallStreaming"},
		},
	}
	if !needsJSON(usage) {
		t.Error("expected needsJSON=true for PluginCallStreaming")
	}

	// DurableCall does not use json.
	usage2 := &UsageInfo{
		Used: map[string]bool{"cleat_call": true},
		Funcs: []HostFunction{
			{ImportName: "cleat_call", FieldName: "DurableCall"},
		},
	}
	if needsJSON(usage2) {
		t.Error("expected needsJSON=false for DurableCall")
	}
}
