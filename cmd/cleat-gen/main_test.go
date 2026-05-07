package main

import (
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExprToString(t *testing.T) {
	tests := []struct {
		name     string
		input    string // Go source of a type expression
		expected string
	}{
		{"ident", "int", "int"},
		{"pointer", "*ChargeResponse", "*ChargeResponse"},
		{"slice", "[]CartItem", "[]CartItem"},
		{"map", "map[string]int", "map[string]int"},
		{"empty_iface", "interface{}", "interface{}"},
		{"selector", "pkg.Type", "pkg.Type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := `package test
			type _ ` + tt.input + `
			`
			fset, decls := mustParseType(t, src)
			typeSpec := decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec)
			got := exprToString(typeSpec.Type)
			_ = fset
			if got != tt.expected {
				t.Errorf("exprToString = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestParseSpecDir(t *testing.T) {
	dir := t.TempDir()
	specContent := `package payments_spec

type ChargeRequest struct {
	UserID      string ` + "`json:\"user_id\"`" + `
	AmountCents int    ` + "`json:\"amount_cents\"`" + `
	Currency    string ` + "`json:\"currency\"`" + `
}

type ChargeResponse struct {
	ChargeID string ` + "`json:\"charge_id\"`" + `
	Status   string ` + "`json:\"status\"`" + `
}

type Client interface {
	Charge(req ChargeRequest) (*ChargeResponse, error)
	Refund(req ChargeRequest) error
}
`
	if err := os.WriteFile(filepath.Join(dir, "spec.go"), []byte(specContent), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := parseSpecDir(dir)
	if err != nil {
		t.Fatalf("parseSpecDir: %v", err)
	}

	if spec.PackageName != "payments_spec" {
		t.Errorf("PackageName = %q, want %q", spec.PackageName, "payments_spec")
	}
	if spec.ServiceName != "" {
		t.Errorf("ServiceName = %q, want empty (set by caller)", spec.ServiceName)
	}

	// Check types.
	if len(spec.Types) != 2 {
		t.Fatalf("expected 2 types, got %d", len(spec.Types))
	}
	if spec.Types[0].Name != "ChargeRequest" {
		t.Errorf("Types[0].Name = %q", spec.Types[0].Name)
	}
	if len(spec.Types[0].Fields) != 3 {
		t.Errorf("Types[0] has %d fields, want 3", len(spec.Types[0].Fields))
	}

	// Check methods.
	if len(spec.Methods) != 2 {
		t.Fatalf("expected 2 methods, got %d", len(spec.Methods))
	}
	if spec.Methods[0].Name != "Charge" {
		t.Errorf("Methods[0].Name = %q, want %q", spec.Methods[0].Name, "Charge")
	}
	if spec.Methods[0].RequestType != "ChargeRequest" {
		t.Errorf("Methods[0].RequestType = %q", spec.Methods[0].RequestType)
	}
	if spec.Methods[0].ResponseType != "ChargeResponse" {
		t.Errorf("Methods[0].ResponseType = %q", spec.Methods[0].ResponseType)
	}
	if !spec.Methods[0].HasResponse() {
		t.Error("Charge should have a response")
	}

	if spec.Methods[1].Name != "Refund" {
		t.Errorf("Methods[1].Name = %q", spec.Methods[1].Name)
	}
	if spec.Methods[1].HasResponse() {
		t.Error("Refund should not have a response")
	}
}

func TestParseSpecDir_MultipleFiles(t *testing.T) {
	dir := t.TempDir()

	// File 1: types
	if err := os.WriteFile(filepath.Join(dir, "types.go"), []byte(`package menu_spec
type LookupRequest struct {
	RestaurantID string `+"`json:\"restaurant_id\"`"+`
	SKU          string `+"`json:\"sku\"`"+`
}
type LookupResponse struct {
	Name       string `+"`json:\"name\"`"+`
	PriceCents int    `+"`json:\"price_cents\"`"+`
}
`), 0644); err != nil {
		t.Fatal(err)
	}

	// File 2: interface
	if err := os.WriteFile(filepath.Join(dir, "client.go"), []byte(`package menu_spec
type Client interface {
	LookupItem(req LookupRequest) (*LookupResponse, error)
}
`), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := parseSpecDir(dir)
	if err != nil {
		t.Fatalf("parseSpecDir: %v", err)
	}

	if len(spec.Types) != 2 {
		t.Errorf("expected 2 types across files, got %d", len(spec.Types))
	}
	if len(spec.Methods) != 1 {
		t.Errorf("expected 1 method, got %d", len(spec.Methods))
	}
}

func TestParseSpecDir_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := parseSpecDir(dir)
	if err == nil {
		t.Fatal("expected error for empty directory")
	}
	if !strings.Contains(err.Error(), "no Go packages found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseSpecDir_IgnoresNonClientInterfaces(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spec.go"), []byte(`package test
type NotClient interface {
	DoSomething() error
}
type Other struct { X int }
`), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := parseSpecDir(dir)
	if err != nil {
		t.Fatalf("parseSpecDir: %v", err)
	}
	if len(spec.Methods) != 0 {
		t.Errorf("expected 0 methods, got %d", len(spec.Methods))
	}
}

func TestGenerateCode(t *testing.T) {
	spec := &SpecInfo{
		ServiceName: "payments",
		Types: []TypeInfo{
			{
				Name: "ChargeRequest",
				Fields: []FieldInfo{
					{Name: "UserID", Type: "string", Tag: "`json:\"user_id\"`"},
					{Name: "AmountCents", Type: "int", Tag: "`json:\"amount_cents\"`"},
				},
			},
			{
				Name: "ChargeResponse",
				Fields: []FieldInfo{
					{Name: "ChargeID", Type: "string", Tag: "`json:\"charge_id\"`"},
				},
			},
		},
		Methods: []MethodInfo{
			{Name: "Charge", RequestType: "ChargeRequest", ResponseType: "ChargeResponse"},
			{Name: "Refund", RequestType: "ChargeRequest"},
		},
	}

	code, err := generateCode(spec, "payments")
	if err != nil {
		t.Fatalf("generateCode: %v", err)
	}

	s := string(code)

	// Check key elements are present.
	checks := []string{
		"package payments",
		`import "github.com/rcownie/cleat/cleat"`,
		"type ChargeRequest struct",
		`json:"user_id"`,
		"type ChargeResponse struct",
		"type Client struct",
		"h cleat.HostCalls",
		"func NewClient(h cleat.HostCalls) *Client",
		`DurableCallTyped("payments", "Charge"`,
		`DurableCallTyped("payments", "Refund"`,
		"var resp ChargeResponse",
		"return &resp, nil",
		"return nil, err",
		"c.h.DurableCallTyped",
	}
	for _, check := range checks {
		if !strings.Contains(s, check) {
			t.Errorf("expected output to contain %q", check)
		}
	}

	// Void-returning method should not have response variable.
	if strings.Contains(s, "Refund") {
		refundIdx := strings.Index(s, "Refund")
		refundSection := s[refundIdx:]
		if strings.Contains(refundSection, "var resp") {
			t.Error("Refund should not have a response variable")
		}
	}
}

func TestGenerateCode_VoidMethods(t *testing.T) {
	spec := &SpecInfo{
		ServiceName: "dispatch",
		Methods: []MethodInfo{
			{Name: "ReleaseDriver", RequestType: "ReleaseRequest"},
		},
	}

	code, err := generateCode(spec, "dispatch")
	if err != nil {
		t.Fatalf("generateCode: %v", err)
	}

	s := string(code)
	if !strings.Contains(s, ") error {") {
		t.Error("void method should return error")
	}
	if !strings.Contains(s, `return c.h.DurableCallTyped("dispatch", "ReleaseDriver", req, nil)`) {
		t.Error("void method should call DurableCallTyped with nil result")
	}
}

func TestGenerateCode_FormattedOutput(t *testing.T) {
	spec := &SpecInfo{
		ServiceName: "test",
		Methods: []MethodInfo{
			{Name: "Ping", RequestType: "PingRequest", ResponseType: "PingResponse"},
		},
	}

	code, err := generateCode(spec, "test")
	if err != nil {
		t.Fatalf("generateCode: %v", err)
	}

	// go/format.Source succeeds → output should be valid Go.
	if _, err := formatSource(code); err != nil {
		t.Errorf("generated code is not valid Go: %v", err)
	}
}

func TestServiceNameFromPackage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spec.go"), []byte(`package orders_spec
type Client interface {
	GetState(req GetStateRequest) (*GetStateResponse, error)
}
type GetStateRequest struct {}
type GetStateResponse struct {}
`), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := parseSpecDir(dir)
	if err != nil {
		t.Fatalf("parseSpecDir: %v", err)
	}

	// Simulate what runClient does to determine service name.
	svc := strings.TrimSuffix(spec.PackageName, "_spec")
	if svc != "orders" {
		t.Errorf("TrimSuffix(%q) = %q, want %q", spec.PackageName, svc, "orders")
	}
}

func TestParseSpecDir_NonExistent(t *testing.T) {
	_, err := parseSpecDir("/nonexistent/path/12345")
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestParseSpecDir_NoClientInterface(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spec.go"), []byte(`package test
type SomeStruct struct { X int }
`), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := parseSpecDir(dir)
	if err != nil {
		t.Fatalf("parseSpecDir: %v", err)
	}
	if len(spec.Methods) != 0 {
		t.Errorf("expected 0 methods when no Client interface, got %d", len(spec.Methods))
	}
}

// Helpers

func mustParseType(t *testing.T, src string) (*token.FileSet, []ast.Decl) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return fset, f.Decls
}

// formatSource is a test-only formatting check.
func formatSource(src []byte) ([]byte, error) {
	return format.Source(src)
}
