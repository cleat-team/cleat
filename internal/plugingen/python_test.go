package plugingen

import (
	"strings"
	"testing"
)

func TestGeneratePython_Simple(t *testing.T) {
	ir := &IR{
		PluginName:    "echo",
		PluginVersion: "0.1.0",
		Description:   "Echo plugin",
		HostFunctions: []HostFuncIR{
			{
				Name:        "echo",
				Description: "Echo input back",
				InputType:   "string",
				OutputType:  "string",
			},
		},
	}

	code, err := GeneratePython(ir)
	if err != nil {
		t.Fatalf("GeneratePython returned error: %v", err)
	}

	if !strings.Contains(code, "# Auto-generated from plugin manifest: echo v0.1.0") {
		t.Error("missing header comment")
	}
	if !strings.Contains(code, "from __future__ import annotations") {
		t.Error("missing __future__ import")
	}
	if !strings.Contains(code, "from dataclasses import dataclass") {
		t.Error("missing dataclass import")
	}
	if !strings.Contains(code, "class EchoPlugin:") {
		t.Error("expected EchoPlugin class")
	}
	if !strings.Contains(code, "def echo(self") {
		t.Error("expected echo method")
	}
	if !strings.Contains(code, "plugin_call") {
		t.Error("expected plugin_call invocation")
	}
}

func TestGeneratePython_WithTypes(t *testing.T) {
	ir := &IR{
		PluginName:    "blobstore",
		PluginVersion: "0.1.0",
		Description:   "Blob storage",
		Types: []TypeIR{
			{
				Name: "PutInput",
				Fields: []FieldIR{
					{Name: "key", Type: "string", Description: "Blob key"},
					{Name: "data", Type: "bytes", Description: "Blob data"},
				},
			},
		},
		HostFunctions: []HostFuncIR{
			{
				Name:        "put",
				Description: "Store a blob",
				InputType:   "PutInput",
				OutputType:  "PutOutput",
			},
			{
				Name:        "get",
				Description: "Get a blob",
				InputType:   "string",
				OutputType:  "bytes",
			},
		},
	}

	code, err := GeneratePython(ir)
	if err != nil {
		t.Fatalf("GeneratePython returned error: %v", err)
	}

	if !strings.Contains(code, "@dataclass") {
		t.Error("expected @dataclass decorator")
	}
	if !strings.Contains(code, "class PutInput:") {
		t.Error("expected PutInput dataclass")
	}
	if !strings.Contains(code, "key: str") {
		t.Error("expected key: str field")
	}
	if !strings.Contains(code, "data: bytes") {
		t.Error("expected data: bytes field")
	}
	if !strings.Contains(code, "class BlobstorePlugin:") {
		t.Error("expected BlobstorePlugin class")
	}
}

func TestGeneratePython_Streaming(t *testing.T) {
	ir := &IR{
		PluginName:    "llm",
		PluginVersion: "0.1.0",
		Description:   "LLM plugin",
		HostFunctions: []HostFuncIR{
			{
				Name:       "chat_stream",
				InputType:  "string",
				OutputType: "string",
				Streaming:  true,
			},
		},
	}

	code, err := GeneratePython(ir)
	if err != nil {
		t.Fatalf("GeneratePython returned error: %v", err)
	}

	if !strings.Contains(code, "plugin_call_streaming") {
		t.Error("expected streaming method to use plugin_call_streaming")
	}
}

func TestGeneratePython_Empty(t *testing.T) {
	ir := &IR{
		PluginName:    "empty",
		PluginVersion: "0.0.0",
		Description:   "Empty plugin",
	}

	code, err := GeneratePython(ir)
	if err != nil {
		t.Fatalf("GeneratePython returned error: %v", err)
	}

	if !strings.Contains(code, "class EmptyPlugin:") {
		t.Error("expected class declaration")
	}
	if len(code) < 50 {
		t.Error("generated code is too short")
	}
}
