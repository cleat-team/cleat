package plugingen

import (
	"strings"
	"testing"
)

// ---- Type mapping functions ----

func TestTsType(t *testing.T) {
	ir := &IR{}
	tests := []struct {
		input string
		want  string
	}{
		{"string", "string"},
		{"int64", "number"},
		{"float64", "number"},
		{"bool", "boolean"},
		{"bytes", "Uint8Array"},
		{"timestamp", "string"},
		{"uuid", "string"},
		{"object", "Record<string, any>"},
		{"array", "any[]"},
		{"Unknown", "any"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := tsType(tc.input, ir)
			if got != tc.want {
				t.Errorf("tsType(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestTsTypeNamedType(t *testing.T) {
	ir := &IR{
		Types: []TypeIR{
			{Name: "PutInput"},
			{Name: "PutOutput"},
		},
	}
	got := tsType("PutInput", ir)
	if got != "PutInput" {
		t.Errorf("tsType(PutInput) = %q, want %q", got, "PutInput")
	}
}

func TestPyType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"string", "str"},
		{"int64", "int"},
		{"float64", "float"},
		{"bool", "bool"},
		{"bytes", "bytes"},
		{"timestamp", "str"},
		{"uuid", "str"},
		{"object", "dict"},
		{"CustomType", "CustomType"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := pyType(tc.input)
			if got != tc.want {
				t.Errorf("pyType(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestRustType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"string", "String"},
		{"int64", "i64"},
		{"float64", "f64"},
		{"bool", "bool"},
		{"bytes", "Vec<u8>"},
		{"timestamp", "String"},
		{"uuid", "String"},
		{"object", "serde_json::Value"},
		{"CustomType", "CustomType"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := rustType(tc.input)
			if got != tc.want {
				t.Errorf("rustType(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestGoType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"string", "string"},
		{"int64", "int64"},
		{"float64", "float64"},
		{"bool", "bool"},
		{"bytes", "[]byte"},
		{"timestamp", "string"},
		{"uuid", "string"},
		{"object", "map[string]interface{}"},
		{"CustomType", "CustomType"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := goType(tc.input)
			if got != tc.want {
				t.Errorf("goType(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---- Field type checkers ----

func TestIsMapType(t *testing.T) {
	tests := []struct {
		name string
		fir  FieldIR
		want bool
	}{
		{"map type", FieldIR{Type: "map"}, true},
		{"with key", FieldIR{KeyType: "string"}, true},
		{"string field", FieldIR{Type: "string"}, false},
		{"int64 field", FieldIR{Type: "int64"}, false},
		{"array field", FieldIR{Type: "array"}, false},
		{"empty field", FieldIR{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isMapType(tc.fir)
			if got != tc.want {
				t.Errorf("isMapType(%+v) = %v, want %v", tc.fir, got, tc.want)
			}
		})
	}
}

func TestIsArrayType(t *testing.T) {
	tests := []struct {
		name string
		fir  FieldIR
		want bool
	}{
		{"array type", FieldIR{Type: "array"}, true},
		{"with items", FieldIR{ItemsType: "string"}, true},
		{"string field", FieldIR{Type: "string"}, false},
		{"int64 field", FieldIR{Type: "int64"}, false},
		{"object field", FieldIR{Type: "object"}, false},
		{"empty field", FieldIR{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isArrayType(tc.fir)
			if got != tc.want {
				t.Errorf("isArrayType(%+v) = %v, want %v", tc.fir, got, tc.want)
			}
		})
	}
}

// ---- Field type mappers ----

func TestPyFieldType(t *testing.T) {
	tests := []struct {
		name string
		fir  FieldIR
		want string
	}{
		{"string field", FieldIR{Type: "string"}, "str"},
		{"int64 field", FieldIR{Type: "int64"}, "int"},
		{"array with items", FieldIR{Type: "array", ItemsType: "string"}, "list[str]"},
		{"array without items", FieldIR{Type: "array"}, "list"},
		{"map with value", FieldIR{Type: "map", ValueType: "string"}, "dict[str, str]"},
		{"map without value", FieldIR{Type: "map"}, "dict"},
		{"nested object", FieldIR{Type: "object", Nested: &TypeIR{}}, "dict"},
		{"map keyed", FieldIR{KeyType: "string", ValueType: "int64"}, "dict[str, int]"},
		{"named type", FieldIR{Type: "CustomType"}, "CustomType"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pyFieldType(tc.fir)
			if got != tc.want {
				t.Errorf("pyFieldType(%+v) = %q, want %q", tc.fir, got, tc.want)
			}
		})
	}
}

func TestRustFieldType(t *testing.T) {
	tests := []struct {
		name string
		fir  FieldIR
		want string
	}{
		{"string field", FieldIR{Type: "string"}, "String"},
		{"int64 field", FieldIR{Type: "int64"}, "i64"},
		{"array with items type", FieldIR{Type: "array", ItemsType: "string"}, "Vec<String>"},
		{"array without items", FieldIR{Type: "array"}, "Vec<serde_json::Value>"},
		{"map with value type", FieldIR{Type: "map", ValueType: "string"}, "std::collections::HashMap<String, String>"},
		{"map without value", FieldIR{Type: "map"}, "std::collections::HashMap<String, serde_json::Value>"},
		{"nested object", FieldIR{Type: "object", Nested: &TypeIR{}}, "serde_json::Value"},
		{"named type", FieldIR{Type: "CustomType"}, "CustomType"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rustFieldType(tc.fir)
			if got != tc.want {
				t.Errorf("rustFieldType(%+v) = %q, want %q", tc.fir, got, tc.want)
			}
		})
	}
}

func TestGoFieldType(t *testing.T) {
	tests := []struct {
		name string
		fir  FieldIR
		want string
	}{
		{"string field", FieldIR{Type: "string"}, "string"},
		{"int64 field", FieldIR{Type: "int64"}, "int64"},
		{"bytes field", FieldIR{Type: "bytes"}, "[]byte"},
		{"array with items type", FieldIR{Type: "array", ItemsType: "string"}, "[]string"},
		{"array without items", FieldIR{Type: "array"}, "[]interface{}"},
		{"map with value type", FieldIR{Type: "map", ValueType: "string"}, "map[string]string"},
		{"map without value", FieldIR{Type: "map"}, "map[string]interface{}"},
		{"nested object", FieldIR{Type: "object", Nested: &TypeIR{}}, "interface{}"},
		{"named type", FieldIR{Type: "CustomType"}, "CustomType"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := goFieldType(tc.fir)
			if got != tc.want {
				t.Errorf("goFieldType(%+v) = %q, want %q", tc.fir, got, tc.want)
			}
		})
	}
}

func TestTsFieldType(t *testing.T) {
	ir := &IR{}
	tests := []struct {
		name string
		fir  FieldIR
		want string
	}{
		{"string field", FieldIR{Type: "string"}, "string"},
		{"int64 field", FieldIR{Type: "int64"}, "number"},
		{"bytes field", FieldIR{Type: "bytes"}, "Uint8Array"},
		{"array with simple items", FieldIR{Type: "array", ItemsType: "string"}, "string[]"},
		{"array with named type items", FieldIR{Type: "array", ItemsType: "CustomType"}, "CustomType[]"},
		{"array without items", FieldIR{Type: "array"}, "any[]"},
		{"map with value type", FieldIR{Type: "map", ValueType: "string"}, "Record<string, string>"},
		{"map without value", FieldIR{Type: "map"}, "Record<string, any>"},
		{"nested object", FieldIR{Type: "object", Nested: &TypeIR{}}, "Record<string, any>"},
		{"named type", FieldIR{Type: "CustomType"}, "CustomType"},
		{"empty type", FieldIR{}, "any"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tsFieldType(tc.fir, ir)
			if got != tc.want {
				t.Errorf("tsFieldType(%+v) = %q, want %q", tc.fir, got, tc.want)
			}
		})
	}
}

// ---- Helpers ----

func TestUnique(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{"empty", nil, nil},
		{"no dupes", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"with dupes", []string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}},
		{"all same", []string{"x", "x", "x"}, []string{"x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := unique(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("unique() = %v (len %d), want %v (len %d)", got, len(got), tc.want, len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("unique()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestCollectReferencedTypes(t *testing.T) {
	ir := &IR{
		HostFunctions: []HostFuncIR{
			{InputType: "PutInput", OutputType: "PutOutput"},
			{InputType: "string", OutputType: "GetOutput"},
		},
	}
	refs := collectReferencedTypes(ir)
	if !refs["PutInput"] {
		t.Error("expected PutInput to be referenced")
	}
	if !refs["PutOutput"] {
		t.Error("expected PutOutput to be referenced")
	}
	if !refs["GetOutput"] {
		t.Error("expected GetOutput to be referenced")
	}
	if refs["string"] {
		t.Error("expected string not to be in referenced types (it's simple)")
	}
}

func TestCollectReferencedTypesEmpty(t *testing.T) {
	ir := &IR{}
	refs := collectReferencedTypes(ir)
	if len(refs) != 0 {
		t.Errorf("expected empty refs, got %v", refs)
	}
}

func TestSanitizeIdent(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"hello_world", "hello_world"},
		{"hello-world", "hello_world"},
		{"hello.world", "hello_world"},
		{"123abc", "_23abc"}, // runs consume digits, but first digit is replaced with _
		{"", "empty"},
		{"   ", "___"}, // non-alphanumeric chars become _
		{"_leading", "_leading"},
		{"abc-def_ghi", "abc_def_ghi"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := sanitizeIdent(tc.input)
			if got != tc.want {
				t.Errorf("sanitizeIdent(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---- FieldIR utility methods ----

func TestFieldIRIsArrayTypeMethod(t *testing.T) {
	tests := []struct {
		name string
		f    FieldIR
		want bool
	}{
		{"array type", FieldIR{Type: "array"}, true},
		{"items type", FieldIR{ItemsType: "string"}, true},
		{"string field", FieldIR{Type: "string"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.f.IsArrayType()
			if got != tc.want {
				t.Errorf("IsArrayType() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFieldIRArrayLike(t *testing.T) {
	tests := []struct {
		name string
		f    FieldIR
		want bool
	}{
		{"array type", FieldIR{Type: "array"}, true},
		{"items type", FieldIR{ItemsType: "string"}, true},
		{"string field", FieldIR{Type: "string"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.f.ArrayLike()
			if got != tc.want {
				t.Errorf("ArrayLike() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- Go generation with edge cases ----

func TestGenerateGo_WithOptionalField(t *testing.T) {
	ir := &IR{
		PluginName:    "test",
		PluginVersion: "0.1.0",
		Description:   "Test optional",
		Types: []TypeIR{
			{
				Name: "TestInput",
				Fields: []FieldIR{
					{Name: "name", Type: "string"},
					{Name: "nickname", Type: "string", Optional: true},
				},
			},
		},
		HostFunctions: []HostFuncIR{
			{
				Name:       "hello",
				InputType:  "TestInput",
				OutputType: "string",
			},
		},
	}

	code, err := GenerateGo(ir)
	if err != nil {
		t.Fatalf("GenerateGo returned error: %v", err)
	}

	if !strings.Contains(code, "Name string") {
		t.Error("expected Name string field")
	}
	if !strings.Contains(code, "Nickname string") {
		t.Error("expected Nickname string field")
	}
	if !strings.Contains(code, `json:"nickname,omitempty"`) {
		t.Error("expected omitempty on optional field")
	}
}

// ---- getPyDefault ----

func TestGetPyDefault(t *testing.T) {
	tests := []struct {
		name string
		f    FieldIR
		want string
	}{
		{"optional", FieldIR{Optional: true}, "None"},
		{"string", FieldIR{Type: "string"}, `""`},
		{"timestamp", FieldIR{Type: "timestamp"}, `""`},
		{"uuid", FieldIR{Type: "uuid"}, `""`},
		{"bool", FieldIR{Type: "bool"}, "False"},
		{"int64", FieldIR{Type: "int64"}, "0"},
		{"float64", FieldIR{Type: "float64"}, "0"},
		{"array type", FieldIR{Type: "array"}, "field(default_factory=list)"},
		{"map type", FieldIR{Type: "map", KeyType: "string"}, "field(default_factory=dict)"},
		{"custom", FieldIR{Type: "CustomType"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getPyDefault(tc.f)
			if got != tc.want {
				t.Errorf("getPyDefault(%+v) = %q, want %q", tc.f, got, tc.want)
			}
		})
	}
}

// ---- getRustSerdeAttr ----

func TestGetRustSerdeAttr(t *testing.T) {
	tests := []struct {
		name string
		f    FieldIR
		want string
	}{
		{"optional", FieldIR{Optional: true}, "    #[serde(default)]"},
		{"required", FieldIR{Optional: false}, ""},
		{"default", FieldIR{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getRustSerdeAttr(tc.f)
			if got != tc.want {
				t.Errorf("getRustSerdeAttr(%+v) = %q, want %q", tc.f, got, tc.want)
			}
		})
	}
}
