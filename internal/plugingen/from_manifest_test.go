package plugingen

import (
	"testing"

	"github.com/rcownie/cleat/internal/plugin"
)

func TestFromManifest_Empty(t *testing.T) {
	m := &plugin.Manifest{
		Name:        "test",
		Version:     "0.1.0",
		Description: "A test plugin",
		Author:      "test",
	}
	ir, err := FromManifest(m)
	if err != nil {
		t.Fatalf("FromManifest returned error: %v", err)
	}
	if ir.PluginName != "test" {
		t.Errorf("expected PluginName 'test', got %q", ir.PluginName)
	}
	if len(ir.HostFunctions) != 0 {
		t.Errorf("expected 0 host functions, got %d", len(ir.HostFunctions))
	}
	if len(ir.Types) != 0 {
		t.Errorf("expected 0 types, got %d", len(ir.Types))
	}
}

func TestFromManifest_HostFunctions(t *testing.T) {
	m := &plugin.Manifest{
		Name:        "blobstore",
		Version:     "0.1.0",
		Description: "Blob storage plugin",
		Author:      "cleat",
		Types: map[string]plugin.TypeDef{
			"put_input": {
				Type: "object",
				Fields: map[string]plugin.FieldDef{
					"key":  {Type: "string", Description: "Blob key"},
					"data": {Type: "bytes", Description: "Blob data"},
				},
			},
			"put_output": {
				Type: "object",
				Fields: map[string]plugin.FieldDef{
					"key":    {Type: "string"},
					"sha256": {Type: "string"},
					"size":   {Type: "int64"},
				},
			},
		},
		HostFunctions: map[string]plugin.HostFuncDef{
			"put": {
				Description: "Store a blob",
				Input:       plugin.TypeDef{Type: "put_input"},
				Output:      plugin.TypeDef{Type: "put_output"},
				Idempotent:  true,
			},
		},
	}
	ir, err := FromManifest(m)
	if err != nil {
		t.Fatalf("FromManifest returned error: %v", err)
	}
	if len(ir.HostFunctions) != 1 {
		t.Fatalf("expected 1 host function, got %d", len(ir.HostFunctions))
	}
	fn := ir.HostFunctions[0]
	if fn.Name != "put" {
		t.Errorf("expected function name 'put', got %q", fn.Name)
	}
	if fn.InputType != "put_input" {
		t.Errorf("expected InputType 'put_input', got %q", fn.InputType)
	}
	if fn.OutputType != "put_output" {
		t.Errorf("expected OutputType 'put_output', got %q", fn.OutputType)
	}
	if !fn.Idempotent {
		t.Errorf("expected Idempotent = true")
	}
	if len(ir.Types) != 2 {
		t.Errorf("expected 2 types, got %d", len(ir.Types))
	}
}

func TestFromManifest_SimpleIO(t *testing.T) {
	m := &plugin.Manifest{
		Name:        "echo",
		Version:     "0.1.0",
		Description: "Echo plugin",
		Author:      "cleat",
		HostFunctions: map[string]plugin.HostFuncDef{
			"echo": {
				Description: "Echo input back",
				Input:       plugin.TypeDef{Type: "string"},
				Output:      plugin.TypeDef{Type: "string"},
			},
		},
	}
	ir, err := FromManifest(m)
	if err != nil {
		t.Fatalf("FromManifest returned error: %v", err)
	}
	if len(ir.HostFunctions) != 1 {
		t.Fatalf("expected 1 host function, got %d", len(ir.HostFunctions))
	}
	fn := ir.HostFunctions[0]
	if fn.InputType != "string" {
		t.Errorf("expected InputType 'string', got %q", fn.InputType)
	}
	if fn.OutputType != "string" {
		t.Errorf("expected OutputType 'string', got %q", fn.OutputType)
	}
}

func TestFromManifest_InlineObjectIO(t *testing.T) {
	m := &plugin.Manifest{
		Name:        "test",
		Version:     "0.1.0",
		Description: "Test plugin",
		Author:      "cleat",
		HostFunctions: map[string]plugin.HostFuncDef{
			"do_stuff": {
				Description: "Do stuff with inline objects",
				Input: plugin.TypeDef{
					Type: "object",
					Fields: map[string]plugin.FieldDef{
						"name":  {Type: "string"},
						"count": {Type: "int64"},
					},
				},
				Output: plugin.TypeDef{
					Type: "object",
					Fields: map[string]plugin.FieldDef{
						"result": {Type: "string"},
					},
				},
			},
		},
	}
	ir, err := FromManifest(m)
	if err != nil {
		t.Fatalf("FromManifest returned error: %v", err)
	}
	if len(ir.Types) != 2 {
		t.Fatalf("expected 2 synthetic types, got %d", len(ir.Types))
	}
	// The synthetic types should be named after the function and direction.
	hasInput := false
	hasOutput := false
	for _, typ := range ir.Types {
		if typ.Name == "do_stuffInput" {
			hasInput = true
			if len(typ.Fields) != 2 {
				t.Errorf("expected 2 fields in do_stuffInput, got %d", len(typ.Fields))
			}
		}
		if typ.Name == "do_stuffOutput" {
			hasOutput = true
			if len(typ.Fields) != 1 {
				t.Errorf("expected 1 field in do_stuffOutput, got %d", len(typ.Fields))
			}
		}
	}
	if !hasInput {
		t.Error("expected synthetic type do_stuffInput")
	}
	if !hasOutput {
		t.Error("expected synthetic type do_stuffOutput")
	}
}

func TestPascalCase(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello_world", "HelloWorld"},
		{"kebab-case", "KebabCase"},
		{"alreadyPascal", "AlreadyPascal"},
		{"simple", "Simple"},
		{"", ""},
	}
	for _, tc := range tests {
		result := toPascalCase(tc.input)
		if result != tc.expected {
			t.Errorf("toPascalCase(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestSnakeCase(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"HelloWorld", "hello_world"},
		{"LLMPlugin", "l_l_m_plugin"}, // note: consecutive capitals handled char-by-char
		{"simple", "simple"},
	}
	for _, tc := range tests {
		result := toSnakeCase(tc.input)
		if result != tc.expected {
			t.Errorf("toSnakeCase(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestFromManifest_ArrayFields(t *testing.T) {
	m := &plugin.Manifest{
		Name:        "test",
		Version:     "0.1.0",
		Description: "Test with arrays",
		Author:      "cleat",
		Types: map[string]plugin.TypeDef{
			"item": {
				Type: "object",
				Fields: map[string]plugin.FieldDef{
					"name":  {Type: "string"},
					"value": {Type: "int64"},
				},
			},
			"container": {
				Type: "object",
				Fields: map[string]plugin.FieldDef{
					"items":       {Type: "array", Items: &plugin.FieldDef{Type: "string"}},
					"complex_ref": {Type: "array", Items: &plugin.FieldDef{Type: "item"}},
				},
			},
		},
	}
	ir, err := FromManifest(m)
	if err != nil {
		t.Fatalf("FromManifest returned error: %v", err)
	}
	if len(ir.Types) != 2 {
		t.Fatalf("expected 2 types, got %d", len(ir.Types))
	}
}

// ---- fieldDefToFieldIR coverage: inline object, array+inline, map, optional ----

func TestFromManifest_InlineObjectField(t *testing.T) {
	m := &plugin.Manifest{
		Name:    "test",
		Version: "0.1.0",
		Types: map[string]plugin.TypeDef{
			"container": {
				Type: "object",
				Fields: map[string]plugin.FieldDef{
					"inner": {
						Type: "object",
						Fields: map[string]plugin.FieldDef{
							"a": {Type: "string", Description: "field A"},
							"b": {Type: "int64"},
						},
					},
				},
			},
		},
	}
	ir, err := FromManifest(m)
	if err != nil {
		t.Fatalf("FromManifest: %v", err)
	}
	if len(ir.Types) != 1 {
		t.Fatalf("expected 1 type, got %d", len(ir.Types))
	}
	typ := ir.Types[0]
	if len(typ.Fields) != 1 {
		t.Fatalf("expected 1 field, got %d", len(typ.Fields))
	}
	f := typ.Fields[0]
	if f.Name != "inner" || f.Type != "object" {
		t.Errorf("expected inner/object, got %s/%s", f.Name, f.Type)
	}
	if f.Nested == nil {
		t.Fatal("expected nested TypeIR for inline object")
	}
	if len(f.Nested.Fields) != 2 {
		t.Fatalf("expected 2 nested fields, got %d", len(f.Nested.Fields))
	}
	if f.Nested.Fields[0].Name != "a" || f.Nested.Fields[0].Type != "string" {
		t.Errorf("nested field 0: expected a/string, got %s/%s", f.Nested.Fields[0].Name, f.Nested.Fields[0].Type)
	}
}

func TestFromManifest_ArrayWithInlineObjectItems(t *testing.T) {
	m := &plugin.Manifest{
		Name:    "test",
		Version: "0.1.0",
		Types: map[string]plugin.TypeDef{
			"container": {
				Type: "object",
				Fields: map[string]plugin.FieldDef{
					"items": {
						Type: "array",
						Items: &plugin.FieldDef{
							Type: "object",
							Fields: map[string]plugin.FieldDef{
								"name":  {Type: "string"},
								"count": {Type: "int64"},
							},
						},
					},
				},
			},
		},
	}
	ir, err := FromManifest(m)
	if err != nil {
		t.Fatalf("FromManifest: %v", err)
	}
	if len(ir.Types) != 1 {
		t.Fatalf("expected 1 type, got %d", len(ir.Types))
	}
	f := ir.Types[0].Fields[0]
	if f.Type != "array" {
		t.Errorf("expected array type, got %s", f.Type)
	}
	if f.Nested == nil {
		t.Fatal("expected nested TypeIR for inline object items")
	}
	if len(f.Nested.Fields) != 2 {
		t.Fatalf("expected 2 nested item fields, got %d", len(f.Nested.Fields))
	}
}

func TestFromManifest_MapWithSimpleTypes(t *testing.T) {
	m := &plugin.Manifest{
		Name:    "test",
		Version: "0.1.0",
		Types: map[string]plugin.TypeDef{
			"config": {
				Type: "object",
				Fields: map[string]plugin.FieldDef{
					"settings": {
						Type:       "map",
						Description: "key-value settings",
						KeyType:    &plugin.FieldDef{Type: "string"},
						ValueType:  &plugin.FieldDef{Type: "string"},
					},
				},
			},
		},
	}
	ir, err := FromManifest(m)
	if err != nil {
		t.Fatalf("FromManifest: %v", err)
	}
	f := ir.Types[0].Fields[0]
	if f.Type != "map" {
		t.Errorf("expected map type, got %s", f.Type)
	}
	if f.KeyType != "string" {
		t.Errorf("expected KeyType string, got %s", f.KeyType)
	}
	if f.ValueType != "string" {
		t.Errorf("expected ValueType string, got %s", f.ValueType)
	}
}

func TestFromManifest_MapWithObjectValue(t *testing.T) {
	m := &plugin.Manifest{
		Name:    "test",
		Version: "0.1.0",
		Types: map[string]plugin.TypeDef{
			"catalog": {
				Type: "object",
				Fields: map[string]plugin.FieldDef{
					"products": {
						Type:    "map",
						KeyType: &plugin.FieldDef{Type: "string"},
						ValueType: &plugin.FieldDef{
							Type: "object",
							Fields: map[string]plugin.FieldDef{
								"sku":   {Type: "string"},
								"price": {Type: "float64"},
							},
						},
					},
				},
			},
		},
	}
	ir, err := FromManifest(m)
	if err != nil {
		t.Fatalf("FromManifest: %v", err)
	}
	f := ir.Types[0].Fields[0]
	if f.Type != "map" {
		t.Errorf("expected map type, got %s", f.Type)
	}
	if f.KeyType != "string" {
		t.Errorf("expected KeyType string, got %s", f.KeyType)
	}
	if f.Nested == nil {
		t.Fatal("expected nested TypeIR for map object value")
	}
}

func TestFromManifest_MapWithNamedRefValue(t *testing.T) {
	m := &plugin.Manifest{
		Name:    "test",
		Version: "0.1.0",
		Types: map[string]plugin.TypeDef{
			"metadata": {
				Type: "object",
				Fields: map[string]plugin.FieldDef{
					"labels": {
						Type:      "map",
						KeyType:   &plugin.FieldDef{Type: "string"},
						ValueType: &plugin.FieldDef{Type: "label_value"},
					},
				},
			},
			"label_value": {
				Type: "object",
				Fields: map[string]plugin.FieldDef{
					"value": {Type: "string"},
				},
			},
		},
	}
	ir, err := FromManifest(m)
	if err != nil {
		t.Fatalf("FromManifest: %v", err)
	}
	if len(ir.Types) != 2 {
		t.Fatalf("expected 2 types, got %d", len(ir.Types))
	}
}

func TestFromManifest_OptionalField(t *testing.T) {
	m := &plugin.Manifest{
		Name:    "test",
		Version: "0.1.0",
		Types: map[string]plugin.TypeDef{
			"with_opt": {
				Type: "object",
				Fields: map[string]plugin.FieldDef{
					"maybe": {
						Type: "optional",
						Fields: map[string]plugin.FieldDef{
							"name":  {Type: "string"},
							"value": {Type: "int64"},
						},
					},
				},
			},
		},
	}
	ir, err := FromManifest(m)
	if err != nil {
		t.Fatalf("FromManifest: %v", err)
	}
	f := ir.Types[0].Fields[0]
	if f.Type != "optional" {
		t.Errorf("expected optional type, got %s", f.Type)
	}
	if !f.Optional {
		t.Error("expected Optional=true")
	}
	if f.Nested == nil {
		t.Fatal("expected nested TypeIR for optional")
	}
	if len(f.Nested.Fields) != 2 {
		t.Fatalf("expected 2 nested fields, got %d", len(f.Nested.Fields))
	}
}

func TestFromManifest_ArrayTopLevelIO(t *testing.T) {
	// Tests resolveTypeRef fallback: an array type without fields as function I/O.
	m := &plugin.Manifest{
		Name:    "test",
		Version: "0.1.0",
		HostFunctions: map[string]plugin.HostFuncDef{
			"process": {
				Description: "Process with raw array type",
				Input:       plugin.TypeDef{Type: "array"},
				Output:      plugin.TypeDef{Type: "map"},
			},
		},
	}
	ir, err := FromManifest(m)
	if err != nil {
		t.Fatalf("FromManifest: %v", err)
	}
	if len(ir.HostFunctions) != 1 {
		t.Fatalf("expected 1 host function, got %d", len(ir.HostFunctions))
	}
	fn := ir.HostFunctions[0]
	if fn.InputType != "array" {
		t.Errorf("expected InputType 'array', got %q", fn.InputType)
	}
	if fn.OutputType != "map" {
		t.Errorf("expected OutputType 'map', got %q", fn.OutputType)
	}
}

// ---- sanitizeIdent coverage ----

func TestSanitizeIdent_WithSpecialChars(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello world", "hello_world"}, // space becomes _
		{"123abc", "_23abc"},           // leading digit becomes _
		{"valid_ident", "valid_ident"},
		{"", "empty"},
		{"a-b+c", "a_b_c"}, // special chars become _
	}
	for _, tc := range tests {
		result := sanitizeIdent(tc.input)
		if result != tc.expected {
			t.Errorf("sanitizeIdent(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}
