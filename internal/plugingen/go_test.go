package plugingen

import (
	"strings"
	"testing"
)

func TestGenerateGo_Simple(t *testing.T) {
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

	code, err := GenerateGo(ir)
	if err != nil {
		t.Fatalf("GenerateGo returned error: %v", err)
	}

	if !strings.Contains(code, "// Auto-generated from plugin manifest: echo v0.1.0") {
		t.Error("missing header comment")
	}
	if !strings.Contains(code, "package plugin") {
		t.Error("missing package declaration")
	}
	if !strings.Contains(code, `import "encoding/json"`) {
		t.Error("missing json import")
	}
	if !strings.Contains(code, "type EchoPlugin struct") {
		t.Error("expected EchoPlugin struct type")
	}
	if !strings.Contains(code, "func NewEchoPlugin") {
		t.Error("expected NewEchoPlugin constructor")
	}
	if !strings.Contains(code, "func (p *EchoPlugin) Echo") {
		t.Error("expected Echo method on EchoPlugin")
	}
}

func TestGenerateGo_WithTypes(t *testing.T) {
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

	code, err := GenerateGo(ir)
	if err != nil {
		t.Fatalf("GenerateGo returned error: %v", err)
	}

	if !strings.Contains(code, "type PutInput struct") {
		t.Error("expected PutInput struct")
	}
	if !strings.Contains(code, "type PutOutput struct") {
		t.Error("expected PutOutput struct")
	}
	if !strings.Contains(code, "Key string") || !strings.Contains(code, `json:"key"`) {
		t.Error("expected Key field with JSON tag")
	}
	if !strings.Contains(code, "Data []byte") {
		t.Error("expected Data field as []byte")
	}
	if !strings.Contains(code, "func (p *BlobstorePlugin) Put") {
		t.Error("expected Put method on BlobstorePlugin")
	}
}

func TestGenerateGo_Streaming(t *testing.T) {
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

	code, err := GenerateGo(ir)
	if err != nil {
		t.Fatalf("GenerateGo returned error: %v", err)
	}

	if !strings.Contains(code, "PluginCallStreaming") {
		t.Error("expected streaming method to use PluginCallStreaming")
	}
	if !strings.Contains(code, "<-chan StreamEvent") {
		t.Error("expected streaming return type")
	}
}

func TestGenerateGo_Empty(t *testing.T) {
	ir := &IR{
		PluginName:    "empty",
		PluginVersion: "0.0.0",
		Description:   "Empty plugin",
	}

	code, err := GenerateGo(ir)
	if err != nil {
		t.Fatalf("GenerateGo returned error: %v", err)
	}

	if !strings.Contains(code, "type EmptyPlugin struct") {
		t.Error("expected class declaration")
	}
	if len(code) < 50 {
		t.Error("generated code is too short")
	}
}
