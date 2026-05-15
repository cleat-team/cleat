package main

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
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
		`import "github.com/cleat-team/cleat/cleat"`,
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

func TestExprToString_DefaultCase(t *testing.T) {
	// The default case in exprToString uses fmt.Sprintf("%T", expr).
	// Test with an expression that doesn't match any known case.
	// The expression "C" (a channel type) hits the default branch.
	src := `package test
	type _ chan int
	`
	fset, decls := mustParseType(t, src)
	typeSpec := decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec)
	got := exprToString(typeSpec.Type)
	_ = fset
	// Should contain the expression type name.
	if got == "" {
		t.Errorf("expected non-empty result for channel type")
	}
}

func TestExprToString_Ellipsis(t *testing.T) {
	src := `package test
	type _ func(...int)
	`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Find the FuncType with an Ellipsis field.
	for _, decl := range f.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			funcType, ok := typeSpec.Type.(*ast.FuncType)
			if !ok || funcType.Params == nil {
				continue
			}
			for _, param := range funcType.Params.List {
				got := exprToString(param.Type)
				if got != "...int" {
					t.Errorf("exprToString(...int) = %q, want %q", got, "...int")
				}
			}
		}
	}
}

func TestRunClient_Basic(t *testing.T) {
	dir := t.TempDir()
	specContent := `package payments_spec

type ChargeRequest struct {
	UserID      string ` + "`json:\"user_id\"`" + `
	AmountCents int    ` + "`json:\"amount_cents\"`" + `
}

type ChargeResponse struct {
	ChargeID string ` + "`json:\"charge_id\"`" + `
	Status   string ` + "`json:\"status\"`" + `
}

type Client interface {
	Charge(req ChargeRequest) (*ChargeResponse, error)
}
`
	if err := os.WriteFile(filepath.Join(dir, "spec.go"), []byte(specContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Run runClient with --o to write output to a temp file.
	outputFile := filepath.Join(t.TempDir(), "gen_client.go")
	runClient([]string{"-o", outputFile, "-service", "payments", "-p", "payments", dir})

	// Verify the generated file exists and contains expected content.
	code, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("failed to read generated file: %v", err)
	}
	s := string(code)
	checks := []string{
		"package payments",
		`DurableCallTyped("payments", "Charge"`,
		"type Client struct",
	}
	for _, check := range checks {
		if !strings.Contains(s, check) {
			t.Errorf("expected output to contain %q", check)
		}
	}
}

func TestRunClient_Stdout(t *testing.T) {
	dir := t.TempDir()
	specContent := `package inventory_spec

type CheckRequest struct {
	SKU string ` + "`json:\"sku\"`" + `
}

type CheckResponse struct {
	Available bool ` + "`json:\"available\"`" + `
}

type Client interface {
	CheckStock(req CheckRequest) (*CheckResponse, error)
}
`
	if err := os.WriteFile(filepath.Join(dir, "spec.go"), []byte(specContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Capture stdout by running in a sub-process-like capture.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w

	outCh := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		outCh <- buf.String()
	}()

	runClient([]string{"-service", "inventory", dir})

	w.Close()
	os.Stdout = old
	output := <-outCh

	if !strings.Contains(output, "package inventory") {
		t.Errorf("expected 'package inventory' in stdout, got: %s", output)
	}
	if !strings.Contains(output, "DurableCallTyped") {
		t.Errorf("expected DurableCallTyped in stdout, got: %s", output)
	}
}

func TestRunClient_ServiceNameDerivation(t *testing.T) {
	dir := t.TempDir()
	specContent := `package orders_spec

type Client interface {
	GetState(req struct{}) error
}
`
	if err := os.WriteFile(filepath.Join(dir, "spec.go"), []byte(specContent), 0644); err != nil {
		t.Fatal(err)
	}

	outputFile := filepath.Join(t.TempDir(), "gen.go")
	runClient([]string{"-o", outputFile, dir})

	code, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("failed to read generated file: %v", err)
	}
	s := string(code)
	if !strings.Contains(s, `DurableCallTyped("orders"`) {
		t.Errorf("expected service name 'orders' derived from package, got: %s", s)
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

func TestParseSpecDir_EmbeddedField(t *testing.T) {
	dir := t.TempDir()
	content := `package spec

type Base struct {
	X string ` + "`json:\"x\"`" + `
}

type Derived struct {
	Base
	Y int ` + "`json:\"y\"`" + `
}

type Client interface {
	Do(req struct{ X string }) error
}
`
	if err := os.WriteFile(filepath.Join(dir, "spec.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := parseSpecDir(dir)
	if err != nil {
		t.Fatalf("parseSpecDir: %v", err)
	}

	// Derived struct should have 1 field (Y) — embedded Base is skipped.
	if len(spec.Types) != 2 {
		t.Fatalf("expected 2 types, got %d", len(spec.Types))
	}
	derivedType := spec.Types[1]
	if derivedType.Name != "Derived" {
		t.Errorf("expected Derived, got %q", derivedType.Name)
	}
	if len(derivedType.Fields) != 1 {
		t.Errorf("expected 1 visible field (Base is embedded), got %d", len(derivedType.Fields))
	}
	if len(derivedType.Fields) > 0 && derivedType.Fields[0].Name != "Y" {
		t.Errorf("expected field Y, got %q", derivedType.Fields[0].Name)
	}
}

func TestParseSpecDir_NonGenDecl(t *testing.T) {
	// Verify that non-GenDecl declarations (e.g., func decls) don't cause issues.
	dir := t.TempDir()
	content := `package spec

func helper() {}

type Client interface {
	Call(req struct{}) error
}
`
	if err := os.WriteFile(filepath.Join(dir, "spec.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := parseSpecDir(dir)
	if err != nil {
		t.Fatalf("parseSpecDir: %v", err)
	}
	if len(spec.Methods) != 1 {
		t.Errorf("expected 1 method, got %d", len(spec.Methods))
	}
}

func TestGenerateCode_FormatFallback(t *testing.T) {
	// generateCode should fall back to unformatted output when format.Source fails.
	// Use a spec that generates syntactically invalid Go to trigger the fallback.
	spec := &SpecInfo{
		ServiceName: "test",
		Types: []TypeInfo{
			{
				Name: "BadType",
				Fields: []FieldInfo{
					{Name: "X", Type: "invalid@type"},
				},
			},
		},
		Methods: []MethodInfo{
			{Name: "Do", RequestType: "BadType"},
		},
	}

	code, err := generateCode(spec, "test")
	if err != nil {
		t.Fatalf("generateCode should not return error on format failure, got: %v", err)
	}
	if len(code) == 0 {
		t.Fatal("expected non-empty output even with formatting failure")
	}
}

func TestGenerateCode_EmptyTypesMethods(t *testing.T) {
	spec := &SpecInfo{
		ServiceName: "empty",
	}
	code, err := generateCode(spec, "empty")
	if err != nil {
		t.Fatalf("generateCode: %v", err)
	}
	if len(code) == 0 {
		t.Fatal("expected non-empty output")
	}
	if !strings.Contains(string(code), "package empty") {
		t.Errorf("expected 'package empty' in output, got %q", string(code))
	}
}

func TestExprToString_ArrayWithConstLen(t *testing.T) {
	src := `package test
	const N = 5
	type _ [N]int
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		if genDecl, ok := decl.(*ast.GenDecl); ok && genDecl.Tok == token.TYPE {
			for _, spec := range genDecl.Specs {
				if typeSpec, ok := spec.(*ast.TypeSpec); ok {
					got := exprToString(typeSpec.Type)
					if got != "[N]int" {
						t.Errorf("exprToString([N]int) = %q, want %q", got, "[N]int")
					}
					return
				}
			}
		}
	}
	t.Fatal("no type spec found")
}

func TestParseSpecDir_WithImport(t *testing.T) {
	// Import spec should be skipped (not a type spec).
	dir := t.TempDir()
	content := `package spec

import "fmt"

type Client interface {
	Call(req struct{ X string }) error
}
`
	if err := os.WriteFile(filepath.Join(dir, "spec.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := parseSpecDir(dir)
	if err != nil {
		t.Fatalf("parseSpecDir: %v", err)
	}
	if spec.PackageName != "spec" {
		t.Errorf("PackageName = %q, want %q", spec.PackageName, "spec")
	}
	if len(spec.Methods) != 1 {
		t.Errorf("expected 1 method, got %d", len(spec.Methods))
	}
}

func TestParseSpecDir_EmbeddedInterface(t *testing.T) {
	// Embedded interface in Client should be skipped (no names).
	dir := t.TempDir()
	content := `package spec

type Logger interface {
	Log(msg string)
}

type Client interface {
	Logger
	Call(req struct{ X string }) error
}
`
	if err := os.WriteFile(filepath.Join(dir, "spec.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := parseSpecDir(dir)
	if err != nil {
		t.Fatalf("parseSpecDir: %v", err)
	}
	if len(spec.Methods) != 1 {
		t.Errorf("expected 1 method (Logger is embedded, should be skipped), got %d", len(spec.Methods))
	}
	if spec.Methods[0].Name != "Call" {
		t.Errorf("expected method name 'Call', got %q", spec.Methods[0].Name)
	}
}
