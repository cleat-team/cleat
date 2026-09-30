package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAndValidateValidManifest(t *testing.T) {
	// Create a temporary valid manifest file.
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "plugin.json")
	content := `{
		"name": "blobstore",
		"version": "0.1.0",
		"description": "Content-addressed blob storage",
		"author": "cleat",
		"capabilities": {
			"database": true,
			"http_routes": true
		},
		"host_functions": {
			"put": {
				"description": "Store a blob",
				"input": { "type": "object", "fields": { "key": { "type": "string" } } },
				"output": { "type": "object", "fields": { "key": { "type": "string" } } }
			}
		}
	}`
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest failed: %v", err)
	}
	if m.Name != "blobstore" {
		t.Errorf("expected name blobstore, got %q", m.Name)
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("ValidateManifest failed: %v", err)
	}
}

func TestValidateEmptyName(t *testing.T) {
	m := &Manifest{
		Name:        "",
		Version:     "1.0.0",
		Description: "test",
		Author:      "test",
	}
	err := ValidateManifest(m)
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestValidateInvalidVersion(t *testing.T) {
	m := &Manifest{
		Name:        "test",
		Version:     "not-a-version",
		Description: "test",
		Author:      "test",
	}
	err := ValidateManifest(m)
	if err == nil {
		t.Fatal("expected error for invalid version")
	}
}

func TestValidateBadHostFunctionName(t *testing.T) {
	m := &Manifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Author:      "test",
		HostFunctions: map[string]HostFuncDef{
			"bad/name": {
				Description: "test",
				Input:       TypeDef{Type: "string"},
				Output:      TypeDef{Type: "string"},
			},
		},
	}
	err := ValidateManifest(m)
	if err == nil {
		t.Fatal("expected error for host function name with '/'")
	}
}

func TestValidateHostFunctionNameWithNullByte(t *testing.T) {
	m := &Manifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Author:      "test",
		HostFunctions: map[string]HostFuncDef{
			"bad\x00name": {
				Description: "test",
				Input:       TypeDef{Type: "string"},
				Output:      TypeDef{Type: "string"},
			},
		},
	}
	err := ValidateManifest(m)
	if err == nil {
		t.Fatal("expected error for host function name with null byte")
	}
}

func TestValidateMissingRequiredFields(t *testing.T) {
	m := &Manifest{}
	err := ValidateManifest(m)
	if err == nil {
		t.Fatal("expected error for manifest with all fields empty")
	}
}

func TestValidateInvalidNamePattern(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{"valid-name", true},
		{"valid123", true},
		{"org/name", true},
		{"my-org/my-plugin", true},
		{"-starts-with-dash", false},
		{"UPPERCASE", false},
		{"has space", false},
		{"/leading-slash", false},
		{"double//slash", false},
		{"trailing/", false},
	}

	for _, tc := range tests {
		m := &Manifest{
			Name:        tc.name,
			Version:     "1.0.0",
			Description: "test",
			Author:      "test",
		}
		err := ValidateManifest(m)
		if tc.valid && err != nil {
			t.Errorf("expected name %q to be valid, got error: %v", tc.name, err)
		}
		if !tc.valid && err == nil {
			t.Errorf("expected name %q to be invalid, got no error", tc.name)
		}
	}
}

func TestLoadManifestUnsupportedExt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.toml")
	if err := os.WriteFile(path, []byte("name = 'test'"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadManifest(path)
	if err == nil {
		t.Fatal("expected error for unsupported extension")
	}
}

func TestLoadManifestNonexistent(t *testing.T) {
	_, err := LoadManifest("/nonexistent/plugin.json")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestDefaultCapabilities(t *testing.T) {
	c := DefaultCapabilities()
	if c.Database {
		t.Error("expected Database to be false")
	}
	if c.StartWorkflow {
		t.Error("expected StartWorkflow to be false")
	}
	if c.SignalWorkflow {
		t.Error("expected SignalWorkflow to be false")
	}
	if c.HTTPRoutes {
		t.Error("expected HTTPRoutes to be false")
	}
	if c.HTTPMiddleware {
		t.Error("expected HTTPMiddleware to be false")
	}
	if c.BackgroundWorker {
		t.Error("expected BackgroundWorker to be false")
	}
	if len(c.CallPlugin) != 0 {
		t.Error("expected CallPlugin to be empty")
	}
}

func TestValidateTypeReference(t *testing.T) {
	types := map[string]TypeDef{
		"BlobInfo": {
			Type: "object",
			Fields: map[string]FieldDef{
				"key":  {Type: "string"},
				"size": {Type: "int64"},
			},
		},
	}

	// Valid: inline object type.
	err := validateTypeRef(TypeDef{Type: "object", Fields: map[string]FieldDef{"x": {Type: "string"}}}, types, "test")
	if err != nil {
		t.Errorf("expected no error for inline object, got: %v", err)
	}

	// Valid: simple type.
	err = validateTypeRef(TypeDef{Type: "string"}, types, "test")
	if err != nil {
		t.Errorf("expected no error for simple type, got: %v", err)
	}

	// Valid: defined type reference.
	err = validateTypeRef(TypeDef{Type: "BlobInfo"}, types, "test")
	if err != nil {
		t.Errorf("expected no error for defined type, got: %v", err)
	}

	// Invalid: undefined named type.
	err = validateTypeRef(TypeDef{Type: "UndefinedType"}, types, "test")
	if err == nil {
		t.Error("expected error for undefined type")
	}
}

func TestJSONBytes(t *testing.T) {
	m := &Manifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test plugin",
		Author:      "me",
	}
	b, err := m.JSONBytes()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("expected non-empty JSON output")
	}
}

func TestValidateTypeDefMissingFieldType(t *testing.T) {
	err := validateTypeDef(TypeDef{
		Fields: map[string]FieldDef{
			"bad_field": {Type: ""},
		},
	}, nil, "testType")
	if err == nil {
		t.Fatal("expected error for field with empty type")
	}
	if !strings.Contains(err.Error(), "type is required") {
		t.Errorf("expected 'type is required', got: %v", err)
	}
}

func TestValidateTypeDefUnsupportedFieldType(t *testing.T) {
	err := validateTypeDef(TypeDef{
		Fields: map[string]FieldDef{
			"bad_field": {Type: "unsupported_type"},
		},
	}, nil, "testType")
	if err == nil {
		t.Fatal("expected error for unsupported field type")
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Errorf("expected 'unsupported type', got: %v", err)
	}
}

// TestValidateTypeDefFieldReferencesDefinedType is cleat#2776: a field's
// type can now name another entry in types, the same allowance
// validateTypeRef already gave a host function's top-level input/output.
func TestValidateTypeDefFieldReferencesDefinedType(t *testing.T) {
	types := map[string]TypeDef{
		"BlobInfo": {
			Type: "object",
			Fields: map[string]FieldDef{
				"key":  {Type: "string"},
				"size": {Type: "int64"},
			},
		},
	}
	err := validateTypeDef(TypeDef{
		Type: "object",
		Fields: map[string]FieldDef{
			"blob": {Type: "BlobInfo"},
		},
	}, types, "testType")
	if err != nil {
		t.Errorf("expected no error for a field referencing a defined type, got: %v", err)
	}
}

// TestValidateTypeDefFieldReferencesUndefinedType confirms the fix is a
// widening, not a removal: a name that resolves to nothing is still
// rejected, whether or not it happens to collide with a builtin's spelling.
func TestValidateTypeDefFieldReferencesUndefinedType(t *testing.T) {
	types := map[string]TypeDef{
		"BlobInfo": {Type: "object"},
	}
	err := validateTypeDef(TypeDef{
		Fields: map[string]FieldDef{
			"blob": {Type: "NotDefined"},
		},
	}, types, "testType")
	if err == nil {
		t.Fatal("expected error for a field referencing an undefined type name")
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Errorf("expected 'unsupported type', got: %v", err)
	}
}

// TestValidateManifestRejectsSelfReferencingFieldType is cleat#2806's R1:
// a self-referencing field (Node.next: Node) validates at the
// validateTypeDef level -- "Node" is a real name in types -- but
// internal/plugingen generates it as a Go struct field BY VALUE, and
// go/types rejects that with "invalid recursive type: Node refers to
// itself" (measured against the real generator while fixing this). This
// used to be documented here as a deliberate allowance; it is a rejection
// instead, at the ValidateManifest level, since validateTypeDef alone has
// no way to see the cycle -- it only checks whether one name resolves.
func TestValidateManifestRejectsSelfReferencingFieldType(t *testing.T) {
	m := &Manifest{
		Name: "linked", Version: "0.1.0", Description: "test", Author: "test",
		Types: map[string]TypeDef{
			"Node": {
				Type: "object",
				Fields: map[string]FieldDef{
					"value": {Type: "string"},
					"next":  {Type: "Node", Optional: true},
				},
			},
		},
		HostFunctions: map[string]HostFuncDef{
			"push": {Description: "push", Input: TypeDef{Type: "Node"}, Output: TypeDef{Type: "Node"}},
		},
	}
	err := ValidateManifest(m)
	if err == nil {
		t.Fatal("expected an error for a self-referencing field type")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected the error to name a cycle, got: %v", err)
	}
}

// TestValidateManifestRejectsIndirectTypeCycle is the A -> B -> A shape
// cleat-review's #2806 review named but did not measure: no single type
// references itself, but the chain does, and it has the identical
// downstream consequence -- a Go struct that contains a struct that
// contains itself, still "invalid recursive type", just one hop removed
// from the field that names the cycle.
func TestValidateManifestRejectsIndirectTypeCycle(t *testing.T) {
	m := &Manifest{
		Name: "mutual", Version: "0.1.0", Description: "test", Author: "test",
		Types: map[string]TypeDef{
			"A": {Type: "object", Fields: map[string]FieldDef{"b": {Type: "B"}}},
			"B": {Type: "object", Fields: map[string]FieldDef{"a": {Type: "A"}}},
		},
		HostFunctions: map[string]HostFuncDef{
			"f": {Description: "f", Input: TypeDef{Type: "A"}, Output: TypeDef{Type: "A"}},
		},
	}
	err := ValidateManifest(m)
	if err == nil {
		t.Fatal("expected an error for an indirect A -> B -> A type cycle")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected the error to name a cycle, got: %v", err)
	}
}

// TestValidateManifestAllowsAcyclicArrayAndMapReferences confirms the
// cycle check does not over-reach: a type used as an array's item type or
// a map's value type does not need to be complete "up front" the way a
// direct field reference does -- Go generates a slice/map element, not an
// embedded struct -- so an acyclic reference through those positions must
// still validate.
func TestValidateManifestAllowsAcyclicArrayAndMapReferences(t *testing.T) {
	m := &Manifest{
		Name: "collections", Version: "0.1.0", Description: "test", Author: "test",
		Types: map[string]TypeDef{
			"Item": {Type: "object", Fields: map[string]FieldDef{"name": {Type: "string"}}},
			"Bag": {Type: "object", Fields: map[string]FieldDef{
				"items": {Type: "array", Items: &FieldDef{Type: "Item"}},
				"byKey": {Type: "map", KeyType: &FieldDef{Type: "string"}, ValueType: &FieldDef{Type: "Item"}},
			}},
		},
		HostFunctions: map[string]HostFuncDef{
			"f": {Description: "f", Input: TypeDef{Type: "Bag"}, Output: TypeDef{Type: "Bag"}},
		},
	}
	if err := ValidateManifest(m); err != nil {
		t.Errorf("expected no error for acyclic array/map references, got: %v", err)
	}
}

// TestLoadAndValidateManifestWithFieldReferencingDefinedType is the
// end-to-end case: a real manifest, through LoadManifest and
// ValidateManifest, where one type's field is another type's name rather
// than an inlined copy of its shape.
func TestLoadAndValidateManifestWithFieldReferencingDefinedType(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "plugin.json")
	content := `{
		"name": "messaging",
		"version": "0.1.0",
		"description": "A plugin whose fields reference a shared shape",
		"author": "cleat",
		"types": {
			"Attachment": {
				"type": "object",
				"fields": {
					"filename": { "type": "string" },
					"size": { "type": "int64" }
				}
			},
			"Message": {
				"type": "object",
				"fields": {
					"body": { "type": "string" },
					"attachment": { "type": "Attachment" }
				}
			}
		},
		"host_functions": {
			"send": {
				"description": "Send a message",
				"input": { "type": "Message" },
				"output": { "type": "object", "fields": { "ok": { "type": "bool" } } }
			}
		}
	}`
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest failed: %v", err)
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("ValidateManifest failed: %v", err)
	}
}

func TestValidateTypeDefEmptyFields(t *testing.T) {
	err := validateTypeDef(TypeDef{}, nil, "emptyType")
	if err != nil {
		t.Errorf("expected no error for empty fields, got: %v", err)
	}
}

func TestValidateTypeRefEmptyType(t *testing.T) {
	err := validateTypeRef(TypeDef{Type: ""}, nil, "test")
	if err == nil {
		t.Fatal("expected error for empty type")
	}
	if !strings.Contains(err.Error(), "type is empty") {
		t.Errorf("expected 'type is empty', got: %v", err)
	}
}
