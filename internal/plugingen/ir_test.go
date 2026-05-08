package plugingen

import (
	"testing"
)

func TestIR_Simple(t *testing.T) {
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

	if ir.PluginName != "echo" {
		t.Errorf("PluginName = %q, want %q", ir.PluginName, "echo")
	}
	if ir.PluginVersion != "0.1.0" {
		t.Errorf("PluginVersion = %q, want %q", ir.PluginVersion, "0.1.0")
	}
	if ir.Description != "Echo plugin" {
		t.Errorf("Description = %q, want %q", ir.Description, "Echo plugin")
	}
	if len(ir.HostFunctions) != 1 {
		t.Fatalf("expected 1 host function, got %d", len(ir.HostFunctions))
	}
	if ir.HostFunctions[0].Name != "echo" {
		t.Errorf("HostFunc name = %q, want %q", ir.HostFunctions[0].Name, "echo")
	}
	if ir.HostFunctions[0].InputType != "string" {
		t.Errorf("InputType = %q, want %q", ir.HostFunctions[0].InputType, "string")
	}
	if ir.HostFunctions[0].OutputType != "string" {
		t.Errorf("OutputType = %q, want %q", ir.HostFunctions[0].OutputType, "string")
	}
}

func TestIR_WithTypes(t *testing.T) {
	ir := &IR{
		PluginName:    "blobstore",
		PluginVersion: "0.2.0",
		Description:   "Blob storage plugin",
		Types: []TypeIR{
			{
				Name: "GetInput",
				Fields: []FieldIR{
					{Name: "key", Type: "string", Description: "Blob key"},
				},
			},
			{
				Name: "GetOutput",
				Fields: []FieldIR{
					{Name: "data", Type: "bytes"},
					{Name: "content_type", Type: "string"},
				},
			},
		},
		HostFunctions: []HostFuncIR{
			{
				Name:        "get",
				Description: "Get a blob by key",
				InputType:   "GetInput",
				OutputType:  "GetOutput",
				Idempotent:  true,
			},
		},
	}

	if len(ir.Types) != 2 {
		t.Fatalf("expected 2 types, got %d", len(ir.Types))
	}
	if ir.Types[0].Name != "GetInput" {
		t.Errorf("Type[0].Name = %q, want %q", ir.Types[0].Name, "GetInput")
	}
	if len(ir.Types[0].Fields) != 1 {
		t.Errorf("expected 1 field in GetInput, got %d", len(ir.Types[0].Fields))
	}
	if ir.Types[1].Name != "GetOutput" {
		t.Errorf("Type[1].Name = %q, want %q", ir.Types[1].Name, "GetOutput")
	}
	if len(ir.Types[1].Fields) != 2 {
		t.Errorf("expected 2 fields in GetOutput, got %d", len(ir.Types[1].Fields))
	}
	if ir.Types[1].Fields[0].Name != "data" {
		t.Errorf("Field[0].Name = %q, want %q", ir.Types[1].Fields[0].Name, "data")
	}
	if ir.Types[1].Fields[0].Type != "bytes" {
		t.Errorf("Field[0].Type = %q, want %q", ir.Types[1].Fields[0].Type, "bytes")
	}
	if ir.HostFunctions[0].Idempotent != true {
		t.Error("expected Idempotent = true")
	}
}

func TestIR_StreamingHostFunc(t *testing.T) {
	ir := &IR{
		PluginName:    "llm",
		PluginVersion: "1.0.0",
		Description:   "LLM plugin",
		HostFunctions: []HostFuncIR{
			{
				Name:       "chat",
				InputType:  "string",
				OutputType: "string",
				Streaming:  true,
			},
		},
	}

	if len(ir.HostFunctions) != 1 {
		t.Fatalf("expected 1 host function, got %d", len(ir.HostFunctions))
	}
	if !ir.HostFunctions[0].Streaming {
		t.Error("expected Streaming = true")
	}
}

func TestIR_Empty(t *testing.T) {
	ir := &IR{
		PluginName:    "empty",
		PluginVersion: "0.0.0",
		Description:   "Empty plugin",
	}

	if ir.PluginName != "empty" {
		t.Errorf("PluginName = %q, want %q", ir.PluginName, "empty")
	}
	if len(ir.HostFunctions) != 0 {
		t.Errorf("expected 0 host functions, got %d", len(ir.HostFunctions))
	}
	if len(ir.Types) != 0 {
		t.Errorf("expected 0 types, got %d", len(ir.Types))
	}
}

func TestIsSimpleType(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"string", true},
		{"int64", true},
		{"float64", true},
		{"bool", true},
		{"bytes", true},
		{"timestamp", true},
		{"uuid", true},
		{"object", false},
		{"array", false},
		{"map", false},
		{"CustomType", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isSimpleType(tt.input)
			if got != tt.want {
				t.Errorf("isSimpleType(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsBuiltinType(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"string", true},
		{"int64", true},
		{"", true},
		{"CustomType", false},
		{"PutInput", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isBuiltinType(tt.input)
			if got != tt.want {
				t.Errorf("isBuiltinType(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
