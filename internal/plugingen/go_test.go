package plugingen

import (
	"strings"
	"testing"
)

// TestGenerateGo_Simple is the untyped case: no InputType/OutputType
// declared, so the business method's own signature already matches
// plugin.PluginFunc and is registered directly, with no generated wrapper.
func TestGenerateGo_Simple(t *testing.T) {
	ir := &IR{
		PluginName:    "echo",
		PluginVersion: "0.1.0",
		Description:   "Echo plugin",
		HostFunctions: []HostFuncIR{
			{
				Name:        "echo",
				Description: "Echo input back",
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
	if !strings.Contains(code, "package echo") {
		t.Error("missing package declaration matching the plugin's own package")
	}
	if !strings.Contains(code, `"github.com/cleat-team/cleat/plugin"`) {
		t.Error("missing plugin package import")
	}
	if !strings.Contains(code, "func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error") {
		t.Error("expected RegisterHostFunctions on Plugin")
	}
	if !strings.Contains(code, `scope.Register(plugin.FuncOptions{Name: "echo"}, p.echo)`) {
		t.Error("expected a direct registration of p.echo, with no generated wrapper, for an untyped function")
	}

	stub := `package echo

import "context"

type Plugin struct{}

func (p *Plugin) echo(ctx context.Context, inputJSON string) (string, error) {
	return inputJSON, nil
}
`
	assertGoCompiles(t, code, stub)
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
	// Not "Key string" as one substring: gofmt (this generator's own
	// post-processing step) column-aligns struct fields, so a field
	// preceding a longer one in source order gets padded with more than
	// one space -- exactly what broke here the first time this was
	// written, against the field ordering sortFields produces.
	if !strings.Contains(code, "Key") || !strings.Contains(code, `json:"key"`) {
		t.Error("expected a Key field with a JSON tag")
	}
	if !strings.Contains(code, "Data []byte") {
		t.Error("expected Data field as []byte")
	}
	if !strings.Contains(code, "p.put(ctx, input)") {
		t.Error("expected the wrapper to call p.put with the unmarshaled input")
	}
	if !strings.Contains(code, "Idempotent: true") {
		t.Error("expected Idempotent to propagate into FuncOptions")
	}

	stub := `package blobstore

import "context"

type Plugin struct{}

func (p *Plugin) put(ctx context.Context, input PutInput) (PutOutput, error) {
	return PutOutput{Sha256: "x", Size: int64(len(input.Data))}, nil
}
`
	assertGoCompiles(t, code, stub)
}

// TestGenerateGo_InputTypedOutputRaw covers the mixed case: a typed input
// unmarshaled for the author, but an untyped (raw JSON string) output
// passed straight back -- no marshal step generated.
func TestGenerateGo_InputTypedOutputRaw(t *testing.T) {
	ir := &IR{
		PluginName:    "mixed",
		PluginVersion: "0.1.0",
		Types: []TypeIR{
			{Name: "QueryInput", Fields: []FieldIR{{Name: "q", Type: "string"}}},
		},
		HostFunctions: []HostFuncIR{
			{Name: "query", InputType: "QueryInput"},
		},
	}
	code, err := GenerateGo(ir)
	if err != nil {
		t.Fatalf("GenerateGo: %v", err)
	}
	if !strings.Contains(code, "var input QueryInput") {
		t.Error("expected the wrapper to unmarshal into QueryInput")
	}
	if !strings.Contains(code, "return p.query(ctx, input)") {
		t.Error("expected a direct return of p.query's result, with no marshal step")
	}
	if strings.Contains(code, "outputJSON") {
		t.Error("an untyped output must not generate a marshal step")
	}

	stub := `package mixed

import "context"

type Plugin struct{}

func (p *Plugin) query(ctx context.Context, input QueryInput) (string, error) {
	return input.Q, nil
}
`
	assertGoCompiles(t, code, stub)
}

// TestGenerateGo_InputRawOutputTyped is the mirror case: an untyped input
// passed straight through as inputJSON, with a typed output marshaled back.
func TestGenerateGo_InputRawOutputTyped(t *testing.T) {
	ir := &IR{
		PluginName:    "mixed2",
		PluginVersion: "0.1.0",
		Types: []TypeIR{
			{Name: "Status", Fields: []FieldIR{{Name: "ok", Type: "bool"}}},
		},
		HostFunctions: []HostFuncIR{
			{Name: "check", OutputType: "Status"},
		},
	}
	code, err := GenerateGo(ir)
	if err != nil {
		t.Fatalf("GenerateGo: %v", err)
	}
	if strings.Contains(code, "var input") {
		t.Error("an untyped input must not generate an unmarshal step")
	}
	if !strings.Contains(code, "p.check(ctx, inputJSON)") {
		t.Error("expected the raw inputJSON passed straight through to p.check")
	}
	if !strings.Contains(code, "json.Marshal(output)") {
		t.Error("expected the typed output to be marshaled")
	}

	stub := `package mixed2

import "context"

type Plugin struct{}

func (p *Plugin) check(ctx context.Context, inputJSON string) (Status, error) {
	return Status{Ok: true}, nil
}
`
	assertGoCompiles(t, code, stub)
}

func TestGenerateGo_Streaming(t *testing.T) {
	ir := &IR{
		PluginName:    "llm",
		PluginVersion: "0.1.0",
		Description:   "LLM plugin",
		Types: []TypeIR{
			{Name: "ChatInput", Fields: []FieldIR{{Name: "prompt", Type: "string"}}},
		},
		HostFunctions: []HostFuncIR{
			{
				Name:      "chat_stream",
				InputType: "ChatInput",
				Streaming: true,
			},
		},
	}

	code, err := GenerateGo(ir)
	if err != nil {
		t.Fatalf("GenerateGo returned error: %v", err)
	}

	if !strings.Contains(code, "streamScope, ok := scope.(plugin.StreamFuncRegistry)") {
		t.Error("expected the StreamFuncRegistry type assertion, matching every hand-written plugin that streams")
	}
	if !strings.Contains(code, "streamScope.RegisterStream(") {
		t.Error("expected RegisterStream, not Register, for a streaming function")
	}
	if !strings.Contains(code, "<-chan plugin.StreamEvent") {
		t.Error("expected the real plugin.StreamEvent channel type")
	}

	stub := `package llm

import (
	"context"

	"github.com/cleat-team/cleat/plugin"
)

type Plugin struct{}

func (p *Plugin) chatStream(ctx context.Context, input ChatInput) (<-chan plugin.StreamEvent, error) {
	ch := make(chan plugin.StreamEvent)
	close(ch)
	return ch, nil
}
`
	assertGoCompiles(t, code, stub)
}

// TestGenerateGo_StreamingUntyped covers a streaming function with no
// declared input type: the business method's signature already matches
// plugin.PluginStreamFunc, so it is registered directly.
func TestGenerateGo_StreamingUntyped(t *testing.T) {
	ir := &IR{
		PluginName:    "tail",
		PluginVersion: "0.1.0",
		HostFunctions: []HostFuncIR{
			{Name: "tail_stream", Streaming: true},
		},
	}
	code, err := GenerateGo(ir)
	if err != nil {
		t.Fatalf("GenerateGo: %v", err)
	}
	if !strings.Contains(code, `streamScope.RegisterStream(plugin.FuncOptions{Name: "tail_stream"}, p.tailStream)`) {
		t.Error("expected a direct registration of p.tailStream, with no generated wrapper")
	}

	stub := `package tail

import (
	"context"

	"github.com/cleat-team/cleat/plugin"
)

type Plugin struct{}

func (p *Plugin) tailStream(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
	ch := make(chan plugin.StreamEvent)
	close(ch)
	return ch, nil
}
`
	assertGoCompiles(t, code, stub)
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

	if !strings.Contains(code, "func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {\n\treturn nil\n}") {
		t.Error("expected a no-op RegisterHostFunctions for a plugin with no host functions")
	}

	stub := `package empty

type Plugin struct{}
`
	assertGoCompiles(t, code, stub)
}

// TestGenerateGo_HyphenatedPluginName confirms the generated package name
// strips hyphens the same way every real plugin directory does --
// plugins/scheduledbackup/ for the manifest name "scheduled-backup"
// (CLAUDE.md's "Plugin development" section).
func TestGenerateGo_HyphenatedPluginName(t *testing.T) {
	ir := &IR{
		PluginName:    "scheduled-backup",
		PluginVersion: "0.1.0",
		HostFunctions: []HostFuncIR{{Name: "run"}},
	}
	code, err := GenerateGo(ir)
	if err != nil {
		t.Fatalf("GenerateGo: %v", err)
	}
	if !strings.Contains(code, "package scheduledbackup") {
		t.Errorf("expected package scheduledbackup (hyphen stripped), got:\n%s", code)
	}
}

func TestGenerateGo_UnreferencedType(t *testing.T) {
	// A type that is not referenced by any host function should still be emitted.
	ir := &IR{
		PluginName:    "test",
		PluginVersion: "0.1.0",
		Types: []TypeIR{
			{
				Name: "ReferencedType",
				Fields: []FieldIR{
					{Name: "val", Type: "string"},
				},
			},
			{
				Name: "UnreferencedType",
				Fields: []FieldIR{
					{Name: "extra", Type: "int64"},
				},
			},
		},
		HostFunctions: []HostFuncIR{
			{
				Name:       "do_stuff",
				InputType:  "ReferencedType",
				OutputType: "string",
			},
		},
	}
	code, err := GenerateGo(ir)
	if err != nil {
		t.Fatalf("GenerateGo: %v", err)
	}
	if !strings.Contains(code, "type ReferencedType struct") {
		t.Error("expected ReferencedType struct")
	}
	if !strings.Contains(code, "type UnreferencedType struct") {
		t.Error("expected UnreferencedType struct (should be emitted in second pass)")
	}

	stub := `package test

import "context"

type Plugin struct{}

func (p *Plugin) doStuff(ctx context.Context, input ReferencedType) (string, error) {
	return input.Val, nil
}
`
	assertGoCompiles(t, code, stub)
}
