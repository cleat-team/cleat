package plugingen

import (
	"strings"
	"testing"
)

func TestGenerateRust_Simple(t *testing.T) {
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

	code, err := GenerateRust(ir)
	if err != nil {
		t.Fatalf("GenerateRust returned error: %v", err)
	}

	if !strings.Contains(code, "// Auto-generated from plugin manifest: echo v0.1.0") {
		t.Error("missing header comment")
	}
	if !strings.Contains(code, "use serde::{Deserialize, Serialize};") {
		t.Error("missing serde import")
	}
	if !strings.Contains(code, "pub struct EchoPlugin") {
		t.Error("expected EchoPlugin struct")
	}
	if !strings.Contains(code, "impl EchoPlugin") {
		t.Error("expected impl block")
	}
	if !strings.Contains(code, "pub async fn echo") {
		t.Error("expected async echo method")
	}
	if !strings.Contains(code, "host_calls.plugin_call") {
		t.Error("expected plugin_call invocation")
	}
}

func TestGenerateRust_WithTypes(t *testing.T) {
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
			{
				Name: "PutOutput",
				Fields: []FieldIR{
					{Name: "sha256", Type: "string"},
					{Name: "size", Type: "int64"},
				},
			},
		},
		HostFunctions: []HostFuncIR{
			{
				Name:        "put",
				Description: "Store a blob",
				InputType:   "PutInput",
				OutputType:  "PutOutput",
				Idempotent:  true,
			},
		},
	}

	code, err := GenerateRust(ir)
	if err != nil {
		t.Fatalf("GenerateRust returned error: %v", err)
	}

	if !strings.Contains(code, "#[derive(Debug, Clone, Serialize, Deserialize)]") {
		t.Error("expected derive macros")
	}
	if !strings.Contains(code, "pub struct PutInput") {
		t.Error("expected PutInput struct")
	}
	if !strings.Contains(code, "pub struct PutOutput") {
		t.Error("expected PutOutput struct")
	}
	if !strings.Contains(code, "pub key: String") {
		t.Error("expected key: String field")
	}
	if !strings.Contains(code, "pub data: Vec<u8>") {
		t.Error("expected data: Vec<u8> field")
	}
	if !strings.Contains(code, "pub size: i64") {
		t.Error("expected size: i64 field")
	}
}

func TestGenerateRust_Streaming(t *testing.T) {
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

	code, err := GenerateRust(ir)
	if err != nil {
		t.Fatalf("GenerateRust returned error: %v", err)
	}

	if !strings.Contains(code, "futures::Stream") {
		t.Error("expected futures::Stream in streaming return type")
	}
	if !strings.Contains(code, "unimplemented") {
		t.Error("expected unimplemented! macro for streaming")
	}
}

func TestGenerateRust_Empty(t *testing.T) {
	ir := &IR{
		PluginName:    "empty",
		PluginVersion: "0.0.0",
		Description:   "Empty plugin",
	}

	code, err := GenerateRust(ir)
	if err != nil {
		t.Fatalf("GenerateRust returned error: %v", err)
	}

	if !strings.Contains(code, "pub struct EmptyPlugin") {
		t.Error("expected EmptyPlugin struct")
	}
	if !strings.Contains(code, "pub fn new") {
		t.Error("expected new constructor")
	}
}

func TestGenerateRust_OptionalField(t *testing.T) {
	ir := &IR{
		PluginName:    "test",
		PluginVersion: "0.1.0",
		Description:   "Test optional fields",
		Types: []TypeIR{
			{
				Name: "TestInput",
				Fields: []FieldIR{
					{Name: "required", Type: "string"},
					{Name: "optional", Type: "string", Optional: true},
				},
			},
		},
		HostFunctions: []HostFuncIR{
			{
				Name:       "test",
				InputType:  "TestInput",
				OutputType: "string",
			},
		},
	}

	code, err := GenerateRust(ir)
	if err != nil {
		t.Fatalf("GenerateRust returned error: %v", err)
	}

	if !strings.Contains(code, "pub required: String") {
		t.Error("expected required field without Option")
	}
	if !strings.Contains(code, "pub optional: Option<String>") {
		t.Error("expected optional field with Option")
	}
	if !strings.Contains(code, "#[serde(default)]") {
		t.Error("expected #[serde(default)] on optional field")
	}
}
