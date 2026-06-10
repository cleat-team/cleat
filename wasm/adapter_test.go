package wasm

import (
	"bytes"
	"strings"
	"testing"
)

func TestGenerateHostFunc_DurableCall(t *testing.T) {
	var buf bytes.Buffer
	hf := HostFunction{ImportName: "cleat_call", FieldName: "DurableCall"}
	adef := adapterDefs["DurableCall"]

	generateHostFunc(&buf, hf, adef)
	code := buf.String()

	checks := []string{
		"func host_DurableCall(",
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

func TestGenerateHostFunc_DurableSleep(t *testing.T) {
	var buf bytes.Buffer
	hf := HostFunction{ImportName: "cleat_sleep", FieldName: "DurableSleep"}
	adef := adapterDefs["DurableSleep"]

	generateHostFunc(&buf, hf, adef)
	code := buf.String()

	checks := []string{
		"func host_DurableSleep(",
		"durationMs int64",
		"cleatSleepImport(",
		"sleepStatus",
	}
	for _, c := range checks {
		if !strings.Contains(code, c) {
			t.Errorf("expected output to contain: %s", c)
		}
	}
}

func TestGenerateHostFunc_DurableLog(t *testing.T) {
	var buf bytes.Buffer
	hf := HostFunction{ImportName: "cleat_log", FieldName: "DurableLog"}
	adef := adapterDefs["DurableLog"]

	generateHostFunc(&buf, hf, adef)
	code := buf.String()

	checks := []string{
		"func host_DurableLog(",
		"message string",
		"cleatLogImport(",
	}
	for _, c := range checks {
		if !strings.Contains(code, c) {
			t.Errorf("expected output to contain: %s", c)
		}
	}
}

func TestGenerateHostWrapperFunc_DurableCallJSON(t *testing.T) {
	var buf bytes.Buffer
	wdef := hostWrapperDefs["DurableCallJSON"]

	generateHostWrapperFunc(&buf, "DurableCallJSON", wdef)
	code := buf.String()

	checks := []string{
		"func host_DurableCallJSON(",
		"service string",
		"operation string",
		"requestJSON string",
		"result interface{}",
		"host_DurableCall(service, operation, requestJSON)",
	}
	for _, c := range checks {
		if !strings.Contains(code, c) {
			t.Errorf("expected output to contain: %s", c)
		}
	}
}

func TestGenerateHostWrapperFunc_AwaitSignals(t *testing.T) {
	var buf bytes.Buffer
	wdef := hostWrapperDefs["AwaitSignals"]

	generateHostWrapperFunc(&buf, "AwaitSignals", wdef)
	code := buf.String()

	checks := []string{
		"func host_AwaitSignals(",
		"signalNames []string",
		"timeout time.Duration",
		"cleat.SignalResult",
		"host_DurableAwaitSignals(",
	}
	for _, c := range checks {
		if !strings.Contains(code, c) {
			t.Errorf("expected output to contain: %s", c)
		}
	}
}

func TestGenerateHostWrapperFunc_Now(t *testing.T) {
	var buf bytes.Buffer
	wdef := hostWrapperDefs["Now"]

	generateHostWrapperFunc(&buf, "Now", wdef)
	code := buf.String()

	checks := []string{
		"func host_Now(",
		"time.Time",
		"host_NowMs()",
		"time.Unix(",
	}
	for _, c := range checks {
		if !strings.Contains(code, c) {
			t.Errorf("expected output to contain: %s", c)
		}
	}
}

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
