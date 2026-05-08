package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcownie/cleat/internal/analyzer"
)

// ---------------------------------------------------------------------------
// plugin_cmd.go — parsePluginSpec
// ---------------------------------------------------------------------------

func TestParsePluginSpec(t *testing.T) {
	tests := []struct {
		spec             string
		wantName         string
		wantConstraint   string
	}{
		{"my-plugin@^1.0.0", "my-plugin", "^1.0.0"},
		{"my-plugin", "my-plugin", ""},
		{"@v1.0.0", "", "v1.0.0"},
		{"a/b@1.0", "a/b", "1.0"},
		{"", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			name, constraint := parsePluginSpec(tt.spec)
			if name != tt.wantName {
				t.Errorf("parsePluginSpec(%q) name = %q, want %q", tt.spec, name, tt.wantName)
			}
			if constraint != tt.wantConstraint {
				t.Errorf("parsePluginSpec(%q) constraint = %q, want %q", tt.spec, constraint, tt.wantConstraint)
			}
		})
	}
}

func TestParsePluginSpec_AtBoundaries(t *testing.T) {
	// Multiple @ signs
	name, constraint := parsePluginSpec("a@b@c")
	if name != "a" {
		t.Errorf("name = %q, want %q", name, "a")
	}
	if constraint != "b@c" {
		t.Errorf("constraint = %q, want %q", constraint, "b@c")
	}
}

// ---------------------------------------------------------------------------
// plugin_cmd.go — ensureV
// ---------------------------------------------------------------------------

func TestEnsureV(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"1.0.0", "v1.0.0"},
		{"v1.0.0", "v1.0.0"},
		{"", "v"},
		{"v", "v"},
		{"0.0.1", "v0.0.1"},
		{"V1.0.0", "vV1.0.0"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ensureV(tt.input)
			if got != tt.want {
				t.Errorf("ensureV(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// run_embedded.go — truncate
// ---------------------------------------------------------------------------

func TestTruncate(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
		{"abc", 0, "..."},
		{"short", 100, "short"},
		{"exact", 5, "exact"},
		{"exact.", 6, "exact."},
	}
	for _, tt := range tests {
		name := fmt.Sprintf("%s_%d", tt.input, tt.maxLen)
		t.Run(name, func(t *testing.T) {
			got := truncate(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// main.go — logBuildProgress
// ---------------------------------------------------------------------------

func TestLogBuildProgress(t *testing.T) {
	// Should not panic with either jsonOut value.
	// Coverage: jsonOut=true writes to stderr, jsonOut=false writes to stdout.
	t.Run("jsonOut_true", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		oldStderr := os.Stderr
		os.Stderr = w

		logBuildProgress("json-msg-%s", true, "test")

		w.Close()
		os.Stderr = oldStderr
		var buf bytes.Buffer
		io.Copy(&buf, r)
		if !strings.Contains(buf.String(), "json-msg-test") {
			t.Errorf("expected 'json-msg-test' on stderr, got %q", buf.String())
		}
	})

	t.Run("jsonOut_false", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		oldStdout := os.Stdout
		os.Stdout = w

		logBuildProgress("stdout-msg-%s", false, "test")

		w.Close()
		os.Stdout = oldStdout
		var buf bytes.Buffer
		io.Copy(&buf, r)
		if !strings.Contains(buf.String(), "stdout-msg-test") {
			t.Errorf("expected 'stdout-msg-test' on stdout, got %q", buf.String())
		}
	})
}

// ---------------------------------------------------------------------------
// dev.go — classifyReturn edge cases (fallthrough and default paths)
// ---------------------------------------------------------------------------

func TestClassifyReturn_EdgeCases(t *testing.T) {
	stringType := types.Typ[types.String]
	intType := types.Typ[types.Int]
	boolType := types.Typ[types.Bool]
	errorType := types.NewNamed(
		types.NewTypeName(0, nil, "error", nil),
		nil, nil,
	)

	tests := []struct {
		name     string
		sig      *types.Signature
		wantKind returnKind
		wantType string
	}{
		{
			name: "(int, bool) - second not error, fallthrough to default",
			sig: types.NewSignatureType(nil, nil, nil, nil,
				types.NewTuple(
					types.NewParam(0, nil, "", intType),
					types.NewParam(0, nil, "", boolType),
				), false,
			),
			wantKind: returnStringError,
			wantType: "string",
		},
		{
			name: "(string, error, string) - three results, hits default",
			sig: types.NewSignatureType(nil, nil, nil, nil,
				types.NewTuple(
					types.NewParam(0, nil, "", stringType),
					types.NewParam(0, nil, "", errorType),
					types.NewParam(0, nil, "", stringType),
				), false,
			),
			wantKind: returnStringError,
			wantType: "string",
		},
		{
			name: "(bool) - single non-error non-string type",
			sig: types.NewSignatureType(nil, nil, nil, nil,
				types.NewTuple(
					types.NewParam(0, nil, "", boolType),
				), false,
			),
			wantKind: returnString,
			wantType: "bool",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, typeStr := classifyReturn(tt.sig)
			if kind != tt.wantKind {
				t.Errorf("kind = %d, want %d", kind, tt.wantKind)
			}
			if typeStr != tt.wantType {
				t.Errorf("typeStr = %q, want %q", typeStr, tt.wantType)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// dev.go — buildParams edge cases (only-host-calls, unnamed params)
// ---------------------------------------------------------------------------

func TestBuildParams_OnlyHostCalls(t *testing.T) {
	// Function with only the HostCalls parameter (Params.Len() == 1).
	result := &analyzer.AnalysisResult{
		TargetPkg: &analyzer.Package{Path: "test", Name: "test"},
	}
	sig := types.NewSignatureType(nil, nil, nil,
		types.NewTuple(
			types.NewParam(0, nil, "h", types.Typ[types.String]),
		),
		nil, false,
	)
	fd := &analyzer.FuncDecl{Type: sig}
	params := buildParams(result, fd)
	if params != nil {
		t.Errorf("expected nil for single-param signature, got %v", params)
	}
}

func TestBuildParams_ZeroParams(t *testing.T) {
	// Function with no parameters at all.
	result := &analyzer.AnalysisResult{
		TargetPkg: &analyzer.Package{Path: "test", Name: "test"},
	}
	sig := types.NewSignatureType(nil, nil, nil,
		types.NewTuple(),
		nil, false,
	)
	fd := &analyzer.FuncDecl{Type: sig}
	params := buildParams(result, fd)
	if params != nil {
		t.Errorf("expected nil for zero-param signature, got %v", params)
	}
}

func TestBuildParams_UnnamedParams(t *testing.T) {
	// Params following HostCalls that have no name should use argN convention.
	result := &analyzer.AnalysisResult{
		TargetPkg: &analyzer.Package{Path: "test", Name: "test"},
	}
	sig := types.NewSignatureType(nil, nil, nil,
		types.NewTuple(
			types.NewParam(0, nil, "h", types.Typ[types.String]),
			types.NewParam(0, nil, "", types.Typ[types.String]), // unnamed
			types.NewParam(0, nil, "", types.Typ[types.Int]),    // unnamed
		),
		nil, false,
	)
	fd := &analyzer.FuncDecl{Type: sig}
	params := buildParams(result, fd)
	if len(params) != 2 {
		t.Fatalf("expected 2 params, got %d", len(params))
	}
	if params[0].Name != "Arg1" {
		t.Errorf("expected first unnamed param name 'Arg1', got %q", params[0].Name)
	}
	if params[1].Name != "Arg2" {
		t.Errorf("expected second unnamed param name 'Arg2', got %q", params[1].Name)
	}
}

// ---------------------------------------------------------------------------
// main.go — lookupFile with position returning empty filename
// ---------------------------------------------------------------------------

func TestLookupFile_EmptyFilename(t *testing.T) {
	// A fresh FileSet with no files returns an empty filename for any position.
	fset := token.NewFileSet()
	result := &analyzer.AnalysisResult{
		Funcs: map[string]*analyzer.FuncDecl{
			"pkg.F": {
				Pkg: &analyzer.Package{
					Fset: fset,
				},
				Ast: &ast.FuncDecl{
					Name: ast.NewIdent("F"),
					Type: &ast.FuncType{Params: &ast.FieldList{}},
				},
			},
		},
	}
	got := lookupFile(result, "pkg.F")
	if got != "" {
		t.Errorf("expected empty for synthetic position, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// build_python.go — detectEntryFunctionFallback edge cases
// ---------------------------------------------------------------------------

func TestDetectEntryFunctionFallback_CommentedDecorator(t *testing.T) {
	// Commented-out decorator should be skipped.
	content := "# @cleat_entry\n# def should_not_match():\n#     pass\ndef actual():\n    pass\n"
	tmpFile := filepath.Join(t.TempDir(), "test.py")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := detectEntryFunctionFallback(tmpFile)
	if err == nil {
		t.Error("expected error for only commented-out @cleat_entry")
	}
}

func TestDetectEntryFunctionFallback_BlankLineAfterDecorator(t *testing.T) {
	// Blank line between decorator and def should be handled.
	content := "@cleat_entry\n\ndef my_func():\n    pass\n"
	tmpFile := filepath.Join(t.TempDir(), "test.py")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	name, err := detectEntryFunctionFallback(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "my_func" {
		t.Errorf("expected 'my_func', got %q", name)
	}
}

func TestDetectEntryFunctionFallback_AsyncDef(t *testing.T) {
	// async def should be rejected with an error mentioning "async".
	content := "@cleat_entry\nasync def my_async():\n    pass\n"
	tmpFile := filepath.Join(t.TempDir(), "test.py")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := detectEntryFunctionFallback(tmpFile)
	if err == nil {
		t.Fatal("expected error for async function")
	}
	if !strings.Contains(err.Error(), "async") {
		t.Errorf("expected 'async' in error message, got: %v", err)
	}
}

func TestDetectEntryFunctionFallback_MultipleDecorators(t *testing.T) {
	// Multiple decorators before the function def.
	content := "@some_other_decorator\n@cleat_entry\ndef my_func():\n    pass\n"
	tmpFile := filepath.Join(t.TempDir(), "test.py")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	name, err := detectEntryFunctionFallback(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "my_func" {
		t.Errorf("expected 'my_func', got %q", name)
	}
}

func TestDetectEntryFunctionFallback_EmptyFile(t *testing.T) {
	// Empty file should produce "not found" error.
	tmpFile := filepath.Join(t.TempDir(), "empty.py")
	if err := os.WriteFile(tmpFile, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := detectEntryFunctionFallback(tmpFile)
	if err == nil {
		t.Fatal("expected error for empty file")
	}
	if !strings.Contains(err.Error(), "no @cleat_entry") {
		t.Errorf("expected 'no @cleat_entry' in error, got: %v", err)
	}
}

func TestDetectEntryFunctionFallback_MalformedDef(t *testing.T) {
	// Decorator followed by non-function line (not def, not decorator).
	content := "@cleat_entry\nnot_a_function = 42\n"
	tmpFile := filepath.Join(t.TempDir(), "test.py")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := detectEntryFunctionFallback(tmpFile)
	if err == nil {
		t.Fatal("expected error for missing function def")
	}
}

func TestDetectEntryFunctionFallback_AfterOtherDecorator(t *testing.T) {
	// @cleat_entry followed by another decorator (not def).
	content := "@cleat_entry\n@another_decorator\ndef decorated_func():\n    pass\n"
	tmpFile := filepath.Join(t.TempDir(), "test.py")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	name, err := detectEntryFunctionFallback(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "decorated_func" {
		t.Errorf("expected 'decorated_func', got %q", name)
	}
}

func TestDetectEntryFunctionFallback_NonDefAfterDecorator(t *testing.T) {
	// @cleat_entry followed by a non-def, non-decorator, non-blank line.
	content := "@cleat_entry\nsome_statement()\ndef actual_func():\n    pass\n"
	tmpFile := filepath.Join(t.TempDir(), "test.py")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := detectEntryFunctionFallback(tmpFile)
	if err == nil {
		t.Fatal("expected error when non-def line follows @cleat_entry")
	}
}

// ---------------------------------------------------------------------------
// build_python.go -- detectEntryFunctionFallback continuation line
// ---------------------------------------------------------------------------

func TestDetectEntryFunctionFallback_ContinuationParen(t *testing.T) {
	// Decorator with parenthesized args continuing on next line.
	content := "@cleat_entry(\ndef my_func():\n    pass\n"
	tmpFile := filepath.Join(t.TempDir(), "test.py")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	name, err := detectEntryFunctionFallback(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "my_func" {
		t.Errorf("expected 'my_func', got %q", name)
	}
}

func TestDetectEntryFunctionFallback_CommentAfterDecorator(t *testing.T) {
	// Comment line between decorator and def should be skipped.
	content := "@cleat_entry\n# some comment about this decorator\ndef commented_func():\n    pass\n"
	tmpFile := filepath.Join(t.TempDir(), "test.py")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	name, err := detectEntryFunctionFallback(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "commented_func" {
		t.Errorf("expected 'commented_func', got %q", name)
	}
}

// ---------------------------------------------------------------------------
// buildParams qualifier edge cases
// ---------------------------------------------------------------------------

func TestBuildParams_QualifierDifferentPackage(t *testing.T) {
	// A parameter whose type is from a different package should use the
	// qualifier's other.Name() branch.
	otherPkg := types.NewPackage("other/module", "otherpkg")
	otherType := types.NewNamed(
		types.NewTypeName(0, otherPkg, "OtherType", nil),
		types.NewStruct(nil, nil), nil,
	)

	result := &analyzer.AnalysisResult{
		TargetPkg: &analyzer.Package{Path: "my/pkg", Name: "mypkg"},
	}
	sig := types.NewSignatureType(nil, nil, nil,
		types.NewTuple(
			types.NewParam(0, nil, "h", types.Typ[types.String]),
			types.NewParam(0, nil, "input", otherType),
		),
		nil, false,
	)
	fd := &analyzer.FuncDecl{Type: sig}
	params := buildParams(result, fd)
	if len(params) != 1 {
		t.Fatalf("expected 1 param, got %d", len(params))
	}
	if params[0].TypeStr != "otherpkg.OtherType" {
		t.Errorf("expected TypeStr 'otherpkg.OtherType', got %q", params[0].TypeStr)
	}
}

func TestBuildParams_QualifierSamePackage(t *testing.T) {
	// A parameter whose type is from the target package should use the
	// result.TargetPkg.Name branch.
	targetPkg := types.NewPackage("my/pkg", "mypkg")
	targetType := types.NewNamed(
		types.NewTypeName(0, targetPkg, "MyType", nil),
		types.NewStruct(nil, nil), nil,
	)

	result := &analyzer.AnalysisResult{
		TargetPkg: &analyzer.Package{Path: "my/pkg", Name: "mypkg"},
	}
	sig := types.NewSignatureType(nil, nil, nil,
		types.NewTuple(
			types.NewParam(0, nil, "h", types.Typ[types.String]),
			types.NewParam(0, nil, "data", targetType),
		),
		nil, false,
	)
	fd := &analyzer.FuncDecl{Type: sig}
	params := buildParams(result, fd)
	if len(params) != 1 {
		t.Fatalf("expected 1 param, got %d", len(params))
	}
	if params[0].TypeStr != "mypkg.MyType" {
		t.Errorf("expected TypeStr 'mypkg.MyType', got %q", params[0].TypeStr)
	}
}

// ---------------------------------------------------------------------------
// dev.go -- generateDevMain for return kinds not tested by fixtures
// ---------------------------------------------------------------------------

func TestGenerateDevMain_ReturnNothing(t *testing.T) {
	result := &analyzer.AnalysisResult{
		TargetPkg: &analyzer.Package{Path: "test/pkg", Name: "testpkg"},
	}
	params := []paramInfo{}
	kind := returnNothing
	src, err := generateDevMain(result, "MyFunc", params, kind, "")
	if err != nil {
		t.Fatalf("generateDevMain failed: %v", err)
	}
	content := string(src)
	// For returnNothing: just calls "testpkg.MyFunc(h)" without assigning.
	if !strings.Contains(content, "testpkg.MyFunc(h)") {
		t.Error("expected 'testpkg.MyFunc(h)' call (no assignment)")
	}
	if !strings.Contains(content, "emitResult") {
		t.Error("expected emitResult call")
	}
}

func TestGenerateDevMain_ReturnError(t *testing.T) {
	result := &analyzer.AnalysisResult{
		TargetPkg: &analyzer.Package{Path: "test/pkg", Name: "testpkg"},
	}
	params := []paramInfo{}
	kind := returnError
	src, err := generateDevMain(result, "MyFunc", params, kind, "")
	if err != nil {
		t.Fatalf("generateDevMain failed: %v", err)
	}
	content := string(src)
	// For returnError: assigns err, checks it, emits result "ok".
	if !strings.Contains(content, "err := testpkg.MyFunc(h)") {
		t.Error("expected 'err := testpkg.MyFunc(h)' call")
	}
	if !strings.Contains(content, "emitResult(runner, \"ok\")") {
		t.Error("expected emitResult with \"ok\"")
	}
}

// ---------------------------------------------------------------------------
// build_rust.go -- extractCrateName: package section followed by another section
// ---------------------------------------------------------------------------

func TestExtractCrateName_SectionAfterPackage(t *testing.T) {
	// [package] section followed by [dependencies] before name -> no name found.
	content := "[package]\n[dependencies]\nname = \"should-not-match\"\n"
	tmpFile := filepath.Join(t.TempDir(), "Cargo.toml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got := extractCrateName(tmpFile)
	if want := "rust_workflow"; got != want {
		t.Errorf("extractCrateName = %q, want %q", got, want)
	}
}

