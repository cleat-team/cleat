package plugingen

import (
	"strings"
	"testing"
)

func TestGenerateTypeScript_Simple(t *testing.T) {
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
	code, err := GenerateTypeScript(ir)
	if err != nil {
		t.Fatalf("GenerateTypeScript returned error: %v", err)
	}

	// Check header.
	if !strings.Contains(code, "// Auto-generated from plugin manifest: echo v0.1.0") {
		t.Errorf("missing header comment")
	}

	// Check class.
	if !strings.Contains(code, "export class EchoPlugin {") {
		t.Errorf("expected export class EchoPlugin")
	}

	// Check constructor.
	if !strings.Contains(code, "constructor(private hostCalls: HostCalls)") {
		t.Errorf("missing constructor")
	}

	// Check method.
	if !strings.Contains(code, "async echo(input: string): Promise<string>") {
		t.Errorf("expected async echo method with string types")
	}

	// Check plugin call.
	if !strings.Contains(code, `pluginCall("echo", "echo", input)`) {
		t.Errorf("expected pluginCall invocation")
	}

	// Check JSON parse.
	if !strings.Contains(code, "JSON.parse(response)") {
		t.Errorf("expected JSON.parse(response)")
	}
}

func TestGenerateTypeScript_WithTypes(t *testing.T) {
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
					{Name: "key", Type: "string"},
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
	code, err := GenerateTypeScript(ir)
	if err != nil {
		t.Fatalf("GenerateTypeScript returned error: %v", err)
	}

	// Check interfaces.
	if !strings.Contains(code, "export interface PutInput {") {
		t.Errorf("expected PutInput interface")
	}
	if !strings.Contains(code, "export interface PutOutput {") {
		t.Errorf("expected PutOutput interface")
	}

	// Check field types.
	if !strings.Contains(code, "key: string;") {
		t.Errorf("expected key: string")
	}
	if !strings.Contains(code, "data: Uint8Array;") {
		t.Errorf("expected data: Uint8Array")
	}

	// Check method signature uses named types.
	if !strings.Contains(code, "async put(input: PutInput): Promise<PutOutput>") {
		t.Errorf("expected async put with typed input/output")
	}

	// No input/output interfaces (they're already TypeIR entries).
}

func TestGenerateTypeScript_Streaming(t *testing.T) {
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
	code, err := GenerateTypeScript(ir)
	if err != nil {
		t.Fatalf("GenerateTypeScript returned error: %v", err)
	}

	if !strings.Contains(code, "pluginCallStreaming") {
		t.Errorf("expected streaming method to use pluginCallStreaming")
	}
}

func TestGenerateTypeScript_Empty(t *testing.T) {
	ir := &IR{
		PluginName:    "empty",
		PluginVersion: "0.0.0",
		Description:   "Empty plugin",
	}
	code, err := GenerateTypeScript(ir)
	if err != nil {
		t.Fatalf("GenerateTypeScript returned error: %v", err)
	}
	if !strings.Contains(code, "export class EmptyPlugin {") {
		t.Errorf("expected class declaration")
	}
}

func TestGenerateTypeScript_OptionalFields(t *testing.T) {
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
	code, err := GenerateTypeScript(ir)
	if err != nil {
		t.Fatalf("GenerateTypeScript returned error: %v", err)
	}
	if !strings.Contains(code, "required: string;") {
		t.Errorf("expected required field without ?")
	}
	if !strings.Contains(code, "optional?: string;") {
		t.Errorf("expected optional field with ?")
	}
}
