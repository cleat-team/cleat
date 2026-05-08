package plugin

import (
	"os"
	"path/filepath"
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
