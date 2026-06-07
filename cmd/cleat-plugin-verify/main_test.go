package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
// Section 1: TestHelperProcess infrastructure
// ============================================================================

// TestHelperProcess is not a real test. It is invoked as a subprocess to
// test os.Exit behavior in main().
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_HELPER_PROCESS") != "1" {
		t.Skip("not a helper process")
	}
	extra := os.Getenv("RUNHELPERARGS")
	os.Args = []string{"cleat-plugin-verify.test"}
	if extra != "" {
		os.Args = append(os.Args, strings.Fields(extra)...)
	}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	main()
}

// runHelper executes the test binary as a subprocess with the given extra
// args and returns combined stdout+stderr.
func runHelper(t *testing.T, extraArgs ...string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=TestHelperProcess$")
	cmd.Env = append(os.Environ(),
		"GO_HELPER_PROCESS=1",
		"RUNHELPERARGS="+strings.Join(extraArgs, " "),
	)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// ============================================================================
// Fixture helpers
// ============================================================================

// writePluginGo creates a plugin.go file using the Info() method pattern.
func writePluginGo(t *testing.T, dir, name string) {
	t.Helper()
	content := fmt.Sprintf(`package main

import "github.com/cleat-team/cleat/internal/plugin"

type MyPlugin struct{}

func (p *MyPlugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{Name: %q}
}
`, name)
	if err := os.WriteFile(filepath.Join(dir, "plugin.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// writePluginGoRegister creates a plugin.go file using plugin.Register() in init().
func writePluginGoRegister(t *testing.T, dir, name string) {
	t.Helper()
	content := fmt.Sprintf(`package main

import "github.com/cleat-team/cleat/internal/plugin"

func init() {
	plugin.Register(plugin.PluginInfo{Name: %q})
}
`, name)
	if err := os.WriteFile(filepath.Join(dir, "plugin.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// writePluginGoBoth creates a plugin.go file with both Info() and Register().
func writePluginGoBoth(t *testing.T, dir, infoName, registerName string) {
	t.Helper()
	content := fmt.Sprintf(`package main

import "github.com/cleat-team/cleat/internal/plugin"

type MyPlugin struct{}

func (p *MyPlugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{Name: %q}
}

func init() {
	plugin.Register(plugin.PluginInfo{Name: %q})
}
`, infoName, registerName)
	if err := os.WriteFile(filepath.Join(dir, "plugin.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// writePluginGoUnparseable creates an unparseable plugin.go file.
func writePluginGoUnparseable(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "plugin.go"), []byte("}{{{ invalid"), 0644); err != nil {
		t.Fatal(err)
	}
}

// writeWorkflowGo creates a workflow .go file with a single function body.
// The body string is inserted into a valid Go package/file skeleton.
func writeWorkflowGo(t *testing.T, dir, fileName, body string) {
	t.Helper()
	content := fmt.Sprintf(`package wf

func MyWorkflow(h interface{}) error {
%s
	return nil
}
`, body)
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// writeWorkflowGoFull creates a workflow .go file from a complete source template.
func writeWorkflowGoFull(t *testing.T, dir, fileName, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// extractJSON returns the first JSON object found in s (from first { to last }).
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return s
	}
	end := strings.LastIndexByte(s, '}')
	if end < 0 || end <= start {
		return s
	}
	return s[start : end+1]
}

// ============================================================================
// Section 2: Pure function tests
// ============================================================================

// --- minInt ---

func TestMinInt(t *testing.T) {
	tests := []struct {
		a, b, want int
	}{
		{0, 0, 0},
		{1, 2, 1},
		{2, 1, 1},
		{-1, 1, -1},
		{1, -1, -1},
		{-5, -3, -5},
		{-3, -5, -5},
	}
	for _, tt := range tests {
		got := minInt(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("minInt(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// --- sortedMapKeys ---

func TestSortedMapKeys_Empty(t *testing.T) {
	keys := sortedMapKeys(map[string]string{})
	if len(keys) != 0 {
		t.Errorf("expected empty slice, got %v", keys)
	}
}

func TestSortedMapKeys_Single(t *testing.T) {
	keys := sortedMapKeys(map[string]string{"a": "1"})
	if len(keys) != 1 || keys[0] != "a" {
		t.Errorf("expected [a], got %v", keys)
	}
}

func TestSortedMapKeys_Multiple(t *testing.T) {
	keys := sortedMapKeys(map[string]string{"c": "3", "a": "1", "b": "2"})
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %v", keys)
	}
	if keys[0] != "a" || keys[1] != "b" || keys[2] != "c" {
		t.Errorf("expected sorted [a b c], got %v", keys)
	}
}

func TestSortedMapKeys_DuplicateValues(t *testing.T) {
	keys := sortedMapKeys(map[string]string{"y": "same", "x": "same"})
	if len(keys) != 2 || keys[0] != "x" || keys[1] != "y" {
		t.Errorf("expected [x y], got %v", keys)
	}
}

// --- levenshtein ---

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"abc", "ab", 1},
		{"ab", "abc", 1},
		{"abc", "def", 3},
		{"kitten", "sitting", 3},
		{"hello", "world", 4},
		{"abc", "abcd", 1},
	}
	for _, tt := range tests {
		got := levenshtein(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// --- bestMatch ---

func TestBestMatch_EmptyKnownNames(t *testing.T) {
	got := bestMatch("foo", map[string]string{})
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestBestMatch_ExactMatch(t *testing.T) {
	known := map[string]string{"foo": "/plugins/foo"}
	got := bestMatch("foo", known)
	if got != "" {
		t.Errorf("exact match should return empty, got %q", got)
	}
}

func TestBestMatch_CloseMatch(t *testing.T) {
	known := map[string]string{"foobar": "/plugins/foobar"}
	got := bestMatch("foobaz", known)
	if !strings.Contains(got, "Did you mean") || !strings.Contains(got, "foobar") {
		t.Errorf("expected suggestion for close match, got %q", got)
	}
}

func TestBestMatch_DistantMatch(t *testing.T) {
	known := map[string]string{"alice": "/plugins/alice"}
	got := bestMatch("bob", known)
	if got != "" {
		t.Errorf("distant match should return empty, got %q", got)
	}
}

func TestBestMatch_ClosestOfSeveral(t *testing.T) {
	// "slak-notify" is distance 1 from "slack-notify" (insert 'c')
	// "slak-notify" is distance 5 from "pagerduty-alert" (much further)
	known := map[string]string{
		"slack-notify":   "/plugins/slack-notify",
		"pagerduty-alert": "/plugins/pagerduty-alert",
	}
	got := bestMatch("slak-notify", known)
	if !strings.Contains(got, "slack-notify") {
		t.Errorf("expected closest match 'slack-notify', got %q", got)
	}
}

func TestBestMatch_BeyondThree(t *testing.T) {
	known := map[string]string{"abcdefgh": "/plugins/long"}
	got := bestMatch("xyz", known)
	if got != "" {
		t.Errorf("distance > 3 should return empty, got %q", got)
	}
}

// --- extractStringArg ---

func TestExtractStringArg_OutOfBounds(t *testing.T) {
	ce := parseCallExpr(t, `foo("hello")`)
	got := extractStringArg(ce, 1)
	if got != "" {
		t.Errorf("out of bounds should return empty, got %q", got)
	}
}

func TestExtractStringArg_NonStringIdent(t *testing.T) {
	ce := parseCallExpr(t, `foo(x)`)
	got := extractStringArg(ce, 0)
	if got != "" {
		t.Errorf("non-string ident should return empty, got %q", got)
	}
}

func TestExtractStringArg_CallExprAsArg(t *testing.T) {
	ce := parseCallExpr(t, `foo(bar())`)
	got := extractStringArg(ce, 0)
	if got != "" {
		t.Errorf("call expr arg should return empty, got %q", got)
	}
}

func TestExtractStringArg_Valid(t *testing.T) {
	ce := parseCallExpr(t, `foo("hello")`)
	got := extractStringArg(ce, 0)
	if got != "hello" {
		t.Errorf("expected 'hello', got %q", got)
	}
}

func TestExtractStringArg_EmptyString(t *testing.T) {
	ce := parseCallExpr(t, `foo("")`)
	got := extractStringArg(ce, 0)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestExtractStringArg_BadQuoting(t *testing.T) {
	// strconv.Unquote will fail on unquoted strings.
	// We need to construct a BasicLit with an un-unquotable value.
	ce := &ast.CallExpr{
		Fun: ast.NewIdent("foo"),
		Args: []ast.Expr{
			&ast.BasicLit{Kind: token.STRING, Value: "not-a-valid-quoted-string"},
		},
	}
	got := extractStringArg(ce, 0)
	if got != "" {
		t.Errorf("bad quoting should return empty, got %q", got)
	}
}

func TestExtractStringArg_EmptyArgs(t *testing.T) {
	ce := &ast.CallExpr{Fun: ast.NewIdent("foo")}
	got := extractStringArg(ce, 0)
	if got != "" {
		t.Errorf("empty args should return empty, got %q", got)
	}
}

// parseCallExpr is a test helper that parses a Go expression statement and
// returns the *ast.CallExpr contained within.
func parseCallExpr(t *testing.T, src string) *ast.CallExpr {
	t.Helper()
	expr, err := parser.ParseExpr(src)
	if err != nil {
		t.Fatalf("parseCallExpr: %v", err)
	}
	ce, ok := expr.(*ast.CallExpr)
	if !ok {
		t.Fatalf("expected *ast.CallExpr, got %T", expr)
	}
	return ce
}

// --- extractNameFromCompositeLit ---

// parseReturnCompositeLit parses "return X" from a function body and returns
// the CompositeLit inside the return statement.
func parseReturnCompositeLit(t *testing.T, bodySrc string) *ast.CompositeLit {
	t.Helper()
	fset := token.NewFileSet()
	src := "package main\nfunc f() plugin.PluginInfo {\n" + bodySrc + "\n}"
	f, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fn := f.Decls[0].(*ast.FuncDecl)
	ret := fn.Body.List[0].(*ast.ReturnStmt)
	return ret.Results[0].(*ast.CompositeLit)
}

func TestExtractNameFromCompositeLit_Valid(t *testing.T) {
	cl := parseReturnCompositeLit(t, `return plugin.PluginInfo{Name: "myplugin"}`)
	got := extractNameFromCompositeLit(cl)
	if got != "myplugin" {
		t.Errorf("expected 'myplugin', got %q", got)
	}
}

func TestExtractNameFromCompositeLit_NilCompositeLit(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil CompositeLit")
		}
	}()
	extractNameFromCompositeLit(nil)
}

func TestExtractNameFromCompositeLit_NoNameField(t *testing.T) {
	cl := parseReturnCompositeLit(t, `return plugin.PluginInfo{Version: "1.0"}`)
	got := extractNameFromCompositeLit(cl)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractNameFromCompositeLit_NonKVExprElements(t *testing.T) {
	// Positional elements (not KeyValueExpr) should be skipped.
	cl := parseReturnCompositeLit(t, `return plugin.PluginInfo{"myplugin"}`)
	got := extractNameFromCompositeLit(cl)
	if got != "" {
		t.Errorf("expected empty for positional elements, got %q", got)
	}
}

func TestExtractNameFromCompositeLit_NonStringValue(t *testing.T) {
	cl := parseReturnCompositeLit(t, `return plugin.PluginInfo{Name: someVar}`)
	got := extractNameFromCompositeLit(cl)
	if got != "" {
		t.Errorf("expected empty for non-string value, got %q", got)
	}
}

func TestExtractNameFromCompositeLit_UnquotedValue(t *testing.T) {
	cl := &ast.CompositeLit{
		Elts: []ast.Expr{
			&ast.KeyValueExpr{
				Key:   ast.NewIdent("Name"),
				Value: &ast.BasicLit{Kind: token.STRING, Value: "bad"},
			},
		},
	}
	got := extractNameFromCompositeLit(cl)
	if got != "" {
		t.Errorf("expected empty for unquotable string, got %q", got)
	}
}

func TestExtractNameFromCompositeLit_EmptyElements(t *testing.T) {
	cl := &ast.CompositeLit{Elts: nil}
	got := extractNameFromCompositeLit(cl)
	if got != "" {
		t.Errorf("expected empty for nil elts, got %q", got)
	}
}

// --- extractNameFromReturn ---

// parseReturnBody parses a function body and returns its BlockStmt.
func parseReturnBody(t *testing.T, bodySrc string) *ast.BlockStmt {
	t.Helper()
	fset := token.NewFileSet()
	src := "package main\nfunc f() plugin.PluginInfo " + bodySrc
	f, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f.Decls[0].(*ast.FuncDecl).Body
}

func TestExtractNameFromReturn_Valid(t *testing.T) {
	body := parseReturnBody(t, `{ return plugin.PluginInfo{Name: "myplugin"} }`)
	got := extractNameFromReturn(body)
	if got != "myplugin" {
		t.Errorf("expected 'myplugin', got %q", got)
	}
}

func TestExtractNameFromReturn_NilBody(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil body")
		}
	}()
	extractNameFromReturn(nil)
}

func TestExtractNameFromReturn_NoReturnStmt(t *testing.T) {
	body := parseReturnBody(t, `{ x := 1; _ = x }`)
	got := extractNameFromReturn(body)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractNameFromReturn_EmptyStmts(t *testing.T) {
	body := parseReturnBody(t, `{}`)
	got := extractNameFromReturn(body)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractNameFromReturn_MultipleStmts(t *testing.T) {
	body := parseReturnBody(t, `{ x := 1; return plugin.PluginInfo{Name: "myplugin"} }`)
	got := extractNameFromReturn(body)
	if got != "myplugin" {
		t.Errorf("expected 'myplugin', got %q", got)
	}
}

func TestExtractNameFromReturn_EmptyResults(t *testing.T) {
	body := parseReturnBody(t, `{ return }`)
	got := extractNameFromReturn(body)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractNameFromReturn_NonCompositeLitResult(t *testing.T) {
	body := parseReturnBody(t, `{ return someVar }`)
	got := extractNameFromReturn(body)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// --- extractNameFromRegister ---

func parseInitBody(t *testing.T, bodySrc string) *ast.BlockStmt {
	t.Helper()
	fset := token.NewFileSet()
	src := "package main\nfunc init() " + bodySrc
	f, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f.Decls[0].(*ast.FuncDecl).Body
}

func TestExtractNameFromRegister_Valid(t *testing.T) {
	body := parseInitBody(t, `{ plugin.Register(plugin.PluginInfo{Name: "myplugin"}) }`)
	got := extractNameFromRegister(body)
	if got != "myplugin" {
		t.Errorf("expected 'myplugin', got %q", got)
	}
}

func TestExtractNameFromRegister_NilBody(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil body")
		}
	}()
	extractNameFromRegister(nil)
}

func TestExtractNameFromRegister_EmptyStmts(t *testing.T) {
	body := parseInitBody(t, `{}`)
	got := extractNameFromRegister(body)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractNameFromRegister_NoCallExpr(t *testing.T) {
	body := parseInitBody(t, `{ x := 1; _ = x }`)
	got := extractNameFromRegister(body)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractNameFromRegister_NonSelectorExprCall(t *testing.T) {
	// Fun is an Ident (direct function call, not a selector expression).
	body := parseInitBody(t, `{ registerFunc(plugin.PluginInfo{Name: "myplugin"}) }`)
	got := extractNameFromRegister(body)
	if got != "" {
		t.Errorf("expected empty for non-selector call, got %q", got)
	}
}

func TestExtractNameFromRegister_NotPluginCall(t *testing.T) {
	body := parseInitBody(t, `{ other.Register(plugin.PluginInfo{Name: "myplugin"}) }`)
	got := extractNameFromRegister(body)
	if got != "" {
		t.Errorf("expected empty for non-plugin pkg, got %q", got)
	}
}

func TestExtractNameFromRegister_NotRegister(t *testing.T) {
	body := parseInitBody(t, `{ plugin.Other(plugin.PluginInfo{Name: "myplugin"}) }`)
	got := extractNameFromRegister(body)
	if got != "" {
		t.Errorf("expected empty for non-Register method, got %q", got)
	}
}

func TestExtractNameFromRegister_NonCompositeLitArg(t *testing.T) {
	body := parseInitBody(t, `{ plugin.Register(someVar) }`)
	got := extractNameFromRegister(body)
	if got != "" {
		t.Errorf("expected empty for non-composite-lit arg, got %q", got)
	}
}

func TestExtractNameFromRegister_NonExprStmt(t *testing.T) {
	body := parseInitBody(t, `{ return }`)
	got := extractNameFromRegister(body)
	if got != "" {
		t.Errorf("expected empty for non-expr stmt, got %q", got)
	}
}

// --- extractPluginName ---

func TestExtractPluginName_InfoMethod(t *testing.T) {
	dir := t.TempDir()
	writePluginGo(t, dir, "myplugin")
	got := extractPluginName(filepath.Join(dir, "plugin.go"))
	if got != "myplugin" {
		t.Errorf("expected 'myplugin', got %q", got)
	}
}

func TestExtractPluginName_RegisterFallback(t *testing.T) {
	dir := t.TempDir()
	writePluginGoRegister(t, dir, "myplugin")
	got := extractPluginName(filepath.Join(dir, "plugin.go"))
	if got != "myplugin" {
		t.Errorf("expected 'myplugin' from Register fallback, got %q", got)
	}
}

func TestExtractPluginName_BothPresent(t *testing.T) {
	dir := t.TempDir()
	writePluginGoBoth(t, dir, "info_name", "register_name")
	got := extractPluginName(filepath.Join(dir, "plugin.go"))
	if got != "info_name" {
		t.Errorf("expected 'info_name' (Info takes priority), got %q", got)
	}
}

func TestExtractPluginName_NeitherPresent(t *testing.T) {
	dir := t.TempDir()
	content := `package main
func someFunc() {}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got := extractPluginName(filepath.Join(dir, "plugin.go"))
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractPluginName_UnparseableFile(t *testing.T) {
	dir := t.TempDir()
	writePluginGoUnparseable(t, dir)
	got := extractPluginName(filepath.Join(dir, "plugin.go"))
	if got != "" {
		t.Errorf("expected empty for unparseable, got %q", got)
	}
}

func TestExtractPluginName_NonexistentFile(t *testing.T) {
	got := extractPluginName("/nonexistent/plugin.go")
	if got != "" {
		t.Errorf("expected empty for nonexistent file, got %q", got)
	}
}

// TestExtractPluginName_InfoMethodWithoutReceiver ensures a standalone
// function named "Info" (no receiver) does NOT match the Info() path.
func TestExtractPluginName_InfoMethodWithoutReceiver(t *testing.T) {
	dir := t.TempDir()
	content := `package main
import "github.com/cleat-team/cleat/internal/plugin"
func Info() plugin.PluginInfo {
	return plugin.PluginInfo{Name: "no_receiver"}
}
func init() {
	plugin.Register(plugin.PluginInfo{Name: "from_register"})
}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got := extractPluginName(filepath.Join(dir, "plugin.go"))
	if got != "from_register" {
		t.Errorf("expected 'from_register' (no-receiver Info skipped, Register fallback), got %q", got)
	}
}

// ============================================================================
// Section 3: File-scanning function tests
// ============================================================================

// --- scanPlugins ---

func TestScanPlugins_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	plugins := scanPlugins(dir)
	if len(plugins) != 0 {
		t.Errorf("expected 0 plugins in empty dir, got %v", plugins)
	}
}

func TestScanPlugins_NoPluginGo(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "somedir")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	plugins := scanPlugins(dir)
	if len(plugins) != 0 {
		t.Errorf("expected 0 plugins, got %v", plugins)
	}
}

func TestScanPlugins_ValidInfoMethod(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "myplugin")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGo(t, sub, "myplugin")
	plugins := scanPlugins(dir)
	if len(plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(plugins))
	}
	if plugins[0].Name != "myplugin" {
		t.Errorf("expected 'myplugin', got %q", plugins[0].Name)
	}
	if !strings.Contains(plugins[0].Dir, "myplugin") {
		t.Errorf("expected dir to contain 'myplugin', got %q", plugins[0].Dir)
	}
}

func TestScanPlugins_ValidRegisterFallback(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "myplugin")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGoRegister(t, sub, "myplugin")
	plugins := scanPlugins(dir)
	if len(plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(plugins))
	}
	if plugins[0].Name != "myplugin" {
		t.Errorf("expected 'myplugin', got %q", plugins[0].Name)
	}
}

func TestScanPlugins_MultiplePlugins(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		sub := filepath.Join(dir, name)
		if err := os.Mkdir(sub, 0755); err != nil {
			t.Fatal(err)
		}
		writePluginGo(t, sub, name)
	}
	plugins := scanPlugins(dir)
	if len(plugins) != 3 {
		t.Fatalf("expected 3 plugins, got %d", len(plugins))
	}
}

func TestScanPlugins_SkipsFiles(t *testing.T) {
	dir := t.TempDir()
	// A regular file (not a directory) should be skipped.
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	plugins := scanPlugins(dir)
	if len(plugins) != 0 {
		t.Errorf("expected 0 plugins, got %d", len(plugins))
	}
}

func TestScanPlugins_ScansHiddenDirs(t *testing.T) {
	// scanPlugins does NOT skip hidden directories — only scanWorkflowDir does.
	// Hidden dirs with valid plugin.go files are included.
	dir := t.TempDir()
	sub := filepath.Join(dir, ".hidden_plugin")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGo(t, sub, "hidden")
	plugins := scanPlugins(dir)
	if len(plugins) != 1 {
		t.Errorf("expected 1 plugin (hidden dirs NOT skipped by scanPlugins), got %d", len(plugins))
	}
	if plugins[0].Name != "hidden" {
		t.Errorf("expected 'hidden', got %q", plugins[0].Name)
	}
}

func TestScanPlugins_NonexistentDir(t *testing.T) {
	out := runHelper(t, "--plugins", "/nonexistent/plugins", "--workflows", "/tmp")
	if !strings.Contains(out, "Error reading plugins directory") {
		t.Errorf("expected error message, got: %s", out)
	}
}

func TestScanPlugins_UnreadableDir(t *testing.T) {
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "noperms")
	if err := os.Mkdir(unreadable, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(unreadable, 0755) })
	out := runHelper(t, "--plugins", unreadable, "--workflows", dir)
	if !strings.Contains(out, "Error reading plugins directory") {
		t.Errorf("expected error message for unreadable dir, got: %s", out)
	}
}

// --- scanWorkflowFile: direct methods ---

func TestScanWorkflowFile_DurableCall(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGo(t, dir, "wf.go", `	h.DurableCall("myplugin", "op", nil)`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	errors, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
	if len(errors) != 0 {
		t.Errorf("expected 0 errors, got %v", errors)
	}
}

func TestScanWorkflowFile_Call(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGo(t, dir, "wf.go", `	h.Call("myplugin", "op", nil)`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	_, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

func TestScanWorkflowFile_PluginCallStreaming(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGo(t, dir, "wf.go", `	h.PluginCallStreaming("myplugin", "func", input)`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	_, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

func TestScanWorkflowFile_DurableCallWithHeartbeat(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGo(t, dir, "wf.go", `	h.DurableCallWithHeartbeat("myplugin", "op", req, interval, cb)`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	_, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

func TestScanWorkflowFile_OnCall(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGo(t, dir, "wf.go", `	env.OnCall("myplugin", "op", matcher)`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	_, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

// --- scanWorkflowFile: offset methods ---

func TestScanWorkflowFile_DurableCallWithOptions(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGo(t, dir, "wf.go", `	h.DurableCallWithOptions(opts, "myplugin", "op", req)`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	_, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

func TestScanWorkflowFile_CallWithOptions(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGo(t, dir, "wf.go", `	h.CallWithOptions(opts, "myplugin", "op", req)`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	_, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

func TestScanWorkflowFile_DurableCallJSONWithOptions(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGo(t, dir, "wf.go", `	h.DurableCallJSONWithOptions(opts, "myplugin", "op", req, &resp)`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	_, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

// --- scanWorkflowFile: generic methods (ast.IndexExpr) ---

func TestScanWorkflowFile_GenericDurableCallTyped(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGoFull(t, dir, "wf.go", `package wf
func MyWorkflow(h interface{}) error {
	h.DurableCallTyped[MyType]("myplugin", "op", req)
	return nil
}`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	_, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

func TestScanWorkflowFile_GenericDurableCallTypedWithOptions(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGoFull(t, dir, "wf.go", `package wf
func MyWorkflow(h interface{}) error {
	h.DurableCallTypedWithOptions[MyType](opts, "myplugin", "op", req)
	return nil
}`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	_, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

func TestScanWorkflowFile_GenericDurableCallJSON(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGoFull(t, dir, "wf.go", `package wf
func MyWorkflow(h interface{}) error {
	h.DurableCallJSON[MyType]("myplugin", "op", req, &resp)
	return nil
}`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	_, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

// --- scanWorkflowFile: package functions (cleat.*) ---

func TestScanWorkflowFile_CleatCallTyped(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGoFull(t, dir, "wf.go", `package wf
func MyWorkflow(h interface{}) error {
	cleat.CallTyped[MyType](h, "myplugin", "op", req)
	return nil
}`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	_, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

func TestScanWorkflowFile_CleatPluginCallTyped(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGoFull(t, dir, "wf.go", `package wf
func MyWorkflow(h interface{}) error {
	cleat.PluginCallTyped[MyType](h, "myplugin", "func", req)
	return nil
}`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	_, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

func TestScanWorkflowFile_CleatCallTypedWithOptions(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGoFull(t, dir, "wf.go", `package wf
func MyWorkflow(h interface{}) error {
	cleat.CallTypedWithOptions[MyType](h, opts, "myplugin", "op", req)
	return nil
}`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	_, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

// --- scanWorkflowFile: error and edge cases ---

func TestScanWorkflowFile_UnknownPlugin(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGo(t, dir, "wf.go", `	h.DurableCall("unknown_plugin", "op", nil)`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	errors, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errors))
	}
	if errors[0].Code != "P001" {
		t.Errorf("expected P001 code, got %q", errors[0].Code)
	}
	if !strings.Contains(errors[0].Message, "unknown_plugin") {
		t.Errorf("expected message to contain 'unknown_plugin', got %q", errors[0].Message)
	}
}

func TestScanWorkflowFile_MixedKnownAndUnknown(t *testing.T) {
	dir := t.TempDir()
	content := `package wf
func MyWorkflow(h interface{}) error {
	h.DurableCall("known_a", "op", nil)
	h.DurableCall("unknown", "op", nil)
	h.DurableCall("known_b", "op", nil)
	return nil
}`
	writeWorkflowGoFull(t, dir, "wf.go", content)
	known := map[string]string{"known_a": "/p/a", "known_b": "/p/b"}
	errors, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 3 {
		t.Errorf("expected 3 calls, got %d", count)
	}
	if len(errors) != 1 {
		t.Errorf("expected 1 error, got %d", len(errors))
	}
}

func TestScanWorkflowFile_NoCalls(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGo(t, dir, "wf.go", `	x := 1; _ = x`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	errors, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 0 {
		t.Errorf("expected 0 calls, got %d", count)
	}
	if len(errors) != 0 {
		t.Errorf("expected 0 errors, got %v", errors)
	}
}

func TestScanWorkflowFile_VariablePluginName(t *testing.T) {
	// Non-literal string arguments should be skipped (not counted).
	dir := t.TempDir()
	writeWorkflowGo(t, dir, "wf.go", `	h.DurableCall(pluginVar, "op", nil)`)
	known := map[string]string{"myplugin": "/p/myplugin"}
	errors, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 0 {
		t.Errorf("expected 0 calls (variable arg skipped), got %d", count)
	}
	if len(errors) != 0 {
		t.Errorf("expected 0 errors, got %v", errors)
	}
}

func TestScanWorkflowFile_UnparseableFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wf.go"), []byte("}{{ bad syntax"), 0644); err != nil {
		t.Fatal(err)
	}
	known := map[string]string{"myplugin": "/p/myplugin"}
	errors, count := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if count != 0 {
		t.Errorf("expected 0 calls, got %d", count)
	}
	if len(errors) != 0 {
		t.Errorf("expected 0 errors for unparseable file, got %v", errors)
	}
}

func TestScanWorkflowFile_NonexistentFile(t *testing.T) {
	known := map[string]string{"myplugin": "/p/myplugin"}
	errors, count := scanWorkflowFile("/nonexistent/wf.go", known)
	if count != 0 {
		t.Errorf("expected 0 calls, got %d", count)
	}
	if len(errors) != 0 {
		t.Errorf("expected 0 errors, got %v", errors)
	}
}

// TestScanWorkflowFile_SuggestionCloseMatch verifies that an unknown plugin
// with a close match (<3 Levenshtein) gets a suggestion.
func TestScanWorkflowFile_SuggestionCloseMatch(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGo(t, dir, "wf.go", `	h.DurableCall("slak-notify", "op", nil)`)
	known := map[string]string{"slack-notify": "/p/slack-notify"}
	errors, _ := scanWorkflowFile(filepath.Join(dir, "wf.go"), known)
	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errors))
	}
	if errors[0].Suggestion == "" {
		t.Error("expected suggestion for close match")
	}
}

// --- scanWorkflowDir ---

func TestScanWorkflowDir_Directory(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowGo(t, dir, "a.go", `	h.DurableCall("p1", "op", nil)`)
	writeWorkflowGo(t, dir, "b.go", `	h.DurableCall("p2", "op", nil)`)
	known := map[string]string{"p1": "/p/p1", "p2": "/p/p2"}
	files, calls, errors := scanWorkflowDir(dir, known)
	if files != 2 {
		t.Errorf("expected 2 files, got %d", files)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
	if len(errors) != 0 {
		t.Errorf("expected 0 errors, got %v", errors)
	}
}

func TestScanWorkflowDir_NestedDirectories(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	writeWorkflowGo(t, dir, "top.go", `	h.DurableCall("p1", "op", nil)`)
	writeWorkflowGo(t, sub, "nested.go", `	h.DurableCall("p2", "op", nil)`)
	known := map[string]string{"p1": "/p/p1", "p2": "/p/p2"}
	files, calls, _ := scanWorkflowDir(dir, known)
	if files != 2 {
		t.Errorf("expected 2 files, got %d", files)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
}

func TestScanWorkflowDir_SkipsHiddenDirs(t *testing.T) {
	dir := t.TempDir()
	hidden := filepath.Join(dir, ".hidden")
	if err := os.Mkdir(hidden, 0755); err != nil {
		t.Fatal(err)
	}
	writeWorkflowGo(t, hidden, "hidden.go", `	h.DurableCall("p", "op", nil)`)
	known := map[string]string{"p": "/p/p"}
	files, calls, _ := scanWorkflowDir(dir, known)
	if files != 0 {
		t.Errorf("expected 0 files (hidden dir skipped), got %d", files)
	}
	if calls != 0 {
		t.Errorf("expected 0 calls, got %d", calls)
	}
}

func TestScanWorkflowDir_SkipsTestdata(t *testing.T) {
	dir := t.TempDir()
	td := filepath.Join(dir, "testdata")
	if err := os.Mkdir(td, 0755); err != nil {
		t.Fatal(err)
	}
	writeWorkflowGo(t, td, "wf.go", `	h.DurableCall("p", "op", nil)`)
	known := map[string]string{"p": "/p/p"}
	files, _, _ := scanWorkflowDir(dir, known)
	if files != 0 {
		t.Errorf("expected 0 files (testdata skipped), got %d", files)
	}
}

func TestScanWorkflowDir_SkipsVendor(t *testing.T) {
	dir := t.TempDir()
	ven := filepath.Join(dir, "vendor")
	if err := os.Mkdir(ven, 0755); err != nil {
		t.Fatal(err)
	}
	writeWorkflowGo(t, ven, "wf.go", `	h.DurableCall("p", "op", nil)`)
	known := map[string]string{"p": "/p/p"}
	files, _, _ := scanWorkflowDir(dir, known)
	if files != 0 {
		t.Errorf("expected 0 files (vendor skipped), got %d", files)
	}
}

func TestScanWorkflowDir_SkipsNodeModules(t *testing.T) {
	dir := t.TempDir()
	nm := filepath.Join(dir, "node_modules")
	if err := os.Mkdir(nm, 0755); err != nil {
		t.Fatal(err)
	}
	writeWorkflowGo(t, nm, "wf.go", `	h.DurableCall("p", "op", nil)`)
	known := map[string]string{"p": "/p/p"}
	files, _, _ := scanWorkflowDir(dir, known)
	if files != 0 {
		t.Errorf("expected 0 files (node_modules skipped), got %d", files)
	}
}

func TestScanWorkflowDir_SingleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "single.go")
	writeWorkflowGo(t, dir, "single.go", `	h.DurableCall("p", "op", nil)`)
	known := map[string]string{"p": "/p/p"}
	files, calls, _ := scanWorkflowDir(path, known)
	if files != 1 {
		t.Errorf("expected 1 file, got %d", files)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestScanWorkflowDir_SingleFileNonGo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "readme.txt")
	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	known := map[string]string{"p": "/p/p"}
	files, _, _ := scanWorkflowDir(path, known)
	if files != 0 {
		t.Errorf("expected 0 files (non-.go), got %d", files)
	}
}

func TestScanWorkflowDir_NonexistentPath(t *testing.T) {
	known := map[string]string{"p": "/p/p"}
	files, calls, errors := scanWorkflowDir("/nonexistent/path", known)
	if files != 0 {
		t.Errorf("expected 0 files, got %d", files)
	}
	if calls != 0 {
		t.Errorf("expected 0 calls, got %d", calls)
	}
	if len(errors) != 0 {
		t.Errorf("expected 0 errors, got %v", errors)
	}
}

func TestScanWorkflowDir_SkipsNonGoFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readme.md"), []byte("# docs"), 0644); err != nil {
		t.Fatal(err)
	}
	writeWorkflowGo(t, dir, "wf.go", `	h.DurableCall("p", "op", nil)`)
	known := map[string]string{"p": "/p/p"}
	files, _, _ := scanWorkflowDir(dir, known)
	if files != 1 {
		t.Errorf("expected 1 file (only .go), got %d", files)
	}
}

func TestScanWorkflowDir_SkipsDotDir(t *testing.T) {
	dir := t.TempDir()
	hidden := filepath.Join(dir, ".git")
	if err := os.Mkdir(hidden, 0755); err != nil {
		t.Fatal(err)
	}
	writeWorkflowGo(t, hidden, "wf.go", `	h.DurableCall("p", "op", nil)`)
	known := map[string]string{"p": "/p/p"}
	files, _, _ := scanWorkflowDir(dir, known)
	if files != 0 {
		t.Errorf("expected 0 files (.git skipped), got %d", files)
	}
}

// ============================================================================
// Section 4: Main integration tests
// ============================================================================

func TestMain_MissingPlugins(t *testing.T) {
	out := runHelper(t)
	if !strings.Contains(out, "--plugins is required") {
		t.Errorf("expected '--plugins is required', got: %s", out)
	}
}

func TestMain_MissingWorkflows(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "p")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGo(t, sub, "p")
	out := runHelper(t, "--plugins", dir)
	if !strings.Contains(out, "--workflows is required") {
		t.Errorf("expected '--workflows is required', got: %s", out)
	}
}

func TestMain_NoPluginsFound(t *testing.T) {
	dir := t.TempDir()
	out := runHelper(t, "--plugins", dir, "--workflows", dir)
	if !strings.Contains(out, "no plugins found") {
		t.Errorf("expected 'no plugins found', got: %s", out)
	}
}

func TestMain_AllMatch(t *testing.T) {
	pluginsDir := t.TempDir()
	psub := filepath.Join(pluginsDir, "myplugin")
	if err := os.Mkdir(psub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGo(t, psub, "myplugin")

	wfDir := t.TempDir()
	writeWorkflowGo(t, wfDir, "wf.go", `	h.DurableCall("myplugin", "op", nil)`)

	out := runHelper(t, "--plugins", pluginsDir, "--workflows", wfDir)
	if !strings.Contains(out, "All plugin names match") {
		t.Errorf("expected success message, got: %s", out)
	}
}

func TestMain_MismatchFound(t *testing.T) {
	pluginsDir := t.TempDir()
	psub := filepath.Join(pluginsDir, "myplugin")
	if err := os.Mkdir(psub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGo(t, psub, "myplugin")

	wfDir := t.TempDir()
	writeWorkflowGo(t, wfDir, "wf.go", `	h.DurableCall("badplugin", "op", nil)`)

	out := runHelper(t, "--plugins", pluginsDir, "--workflows", wfDir)
	if !strings.Contains(out, "P001") {
		t.Errorf("expected P001 error, got: %s", out)
	}
	if !strings.Contains(out, "1 error") {
		t.Errorf("expected '1 error', got: %s", out)
	}
}

func TestMain_JSONOutput(t *testing.T) {
	pluginsDir := t.TempDir()
	psub := filepath.Join(pluginsDir, "myplugin")
	if err := os.Mkdir(psub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGo(t, psub, "myplugin")

	wfDir := t.TempDir()
	writeWorkflowGo(t, wfDir, "wf.go", `	h.DurableCall("badplugin", "op", nil)`)

	out := runHelper(t, "--json", "--plugins", pluginsDir, "--workflows", wfDir)

	var output VetOutput
	jsonStr := extractJSON(out)
	if err := json.Unmarshal([]byte(jsonStr), &output); err != nil {
		t.Fatalf("invalid JSON output: %v\nOutput: %s", err, out)
	}
	if output.Summary.PluginsScanned != 1 {
		t.Errorf("expected 1 plugin scanned, got %d", output.Summary.PluginsScanned)
	}
	if output.Summary.WorkflowFiles != 1 {
		t.Errorf("expected 1 workflow file, got %d", output.Summary.WorkflowFiles)
	}
	if output.Summary.ErrorsFound != 1 {
		t.Errorf("expected 1 error, got %d", output.Summary.ErrorsFound)
	}
	if len(output.Errors) != 1 {
		t.Errorf("expected 1 error in array, got %d", len(output.Errors))
	}
}

func TestMain_JSONOutput_NoErrors(t *testing.T) {
	pluginsDir := t.TempDir()
	psub := filepath.Join(pluginsDir, "myplugin")
	if err := os.Mkdir(psub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGo(t, psub, "myplugin")

	wfDir := t.TempDir()
	writeWorkflowGo(t, wfDir, "wf.go", `	h.DurableCall("myplugin", "op", nil)`)

	out := runHelper(t, "--json", "--plugins", pluginsDir, "--workflows", wfDir)
	var output VetOutput
	jsonStr := extractJSON(out)
	if err := json.Unmarshal([]byte(jsonStr), &output); err != nil {
		t.Fatalf("invalid JSON: %v\nOutput: %s", err, out)
	}
	if output.Summary.ErrorsFound != 0 {
		t.Errorf("expected 0 errors, got %d", output.Summary.ErrorsFound)
	}
}

func TestMain_CIOutput(t *testing.T) {
	pluginsDir := t.TempDir()
	psub := filepath.Join(pluginsDir, "myplugin")
	if err := os.Mkdir(psub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGo(t, psub, "myplugin")

	wfDir := t.TempDir()
	writeWorkflowGo(t, wfDir, "wf.go", `	h.DurableCall("badplugin", "op", nil)`)

	out := runHelper(t, "--ci", "--plugins", pluginsDir, "--workflows", wfDir)
	if !strings.Contains(out, "::error file=") {
		t.Errorf("expected CI annotation format, got: %s", out)
	}
	if !strings.Contains(out, "title=P001") {
		t.Errorf("expected title=P001, got: %s", out)
	}
}

func TestMain_CIOutput_NoErrors(t *testing.T) {
	pluginsDir := t.TempDir()
	psub := filepath.Join(pluginsDir, "myplugin")
	if err := os.Mkdir(psub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGo(t, psub, "myplugin")

	wfDir := t.TempDir()
	writeWorkflowGo(t, wfDir, "wf.go", `	h.DurableCall("myplugin", "op", nil)`)

	out := runHelper(t, "--ci", "--plugins", pluginsDir, "--workflows", wfDir)
	if !strings.Contains(out, "All plugin names verified successfully") {
		t.Errorf("expected success message in CI mode, got: %s", out)
	}
}

func TestMain_MultipleWorkflows(t *testing.T) {
	pluginsDir := t.TempDir()
	psub := filepath.Join(pluginsDir, "myplugin")
	if err := os.Mkdir(psub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGo(t, psub, "myplugin")

	wfDir1 := t.TempDir()
	writeWorkflowGo(t, wfDir1, "a.go", `	h.DurableCall("myplugin", "op", nil)`)
	wfDir2 := t.TempDir()
	writeWorkflowGo(t, wfDir2, "b.go", `	h.DurableCall("myplugin", "op", nil)`)

	out := runHelper(t, "--plugins", pluginsDir, "--workflows", wfDir1, "--workflows", wfDir2)
	if !strings.Contains(out, "2 workflow file") {
		t.Errorf("expected '2 workflow files', got: %s", out)
	}
	if !strings.Contains(out, "All plugin names match") {
		t.Errorf("expected success, got: %s", out)
	}
}

func TestMain_SingleFileWorkflow(t *testing.T) {
	pluginsDir := t.TempDir()
	psub := filepath.Join(pluginsDir, "myplugin")
	if err := os.Mkdir(psub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGo(t, psub, "myplugin")

	wfDir := t.TempDir()
	wfPath := filepath.Join(wfDir, "single.go")
	writeWorkflowGo(t, wfDir, "single.go", `	h.DurableCall("myplugin", "op", nil)`)

	out := runHelper(t, "--plugins", pluginsDir, "--workflows", wfPath)
	if !strings.Contains(out, "All plugin names match") {
		t.Errorf("expected success for single file, got: %s", out)
	}
}

func TestMain_CITrumpsJSON(t *testing.T) {
	// When both --ci and --json are specified, --ci takes precedence.
	pluginsDir := t.TempDir()
	psub := filepath.Join(pluginsDir, "myplugin")
	if err := os.Mkdir(psub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGo(t, psub, "myplugin")

	wfDir := t.TempDir()
	writeWorkflowGo(t, wfDir, "wf.go", `	h.DurableCall("myplugin", "op", nil)`)

	out := runHelper(t, "--ci", "--json", "--plugins", pluginsDir, "--workflows", wfDir)
	// CI mode outputs plain text, not JSON.
	if strings.HasPrefix(out, "{") {
		t.Errorf("expected CI format (not JSON) when both flags present, got: %s", out)
	}
	if !strings.Contains(out, "All plugin names verified successfully") {
		t.Errorf("expected CI success message, got: %s", out)
	}
}

func TestMain_DefaultOutputFormat(t *testing.T) {
	pluginsDir := t.TempDir()
	psub := filepath.Join(pluginsDir, "myplugin")
	if err := os.Mkdir(psub, 0755); err != nil {
		t.Fatal(err)
	}
	writePluginGo(t, psub, "myplugin")

	wfDir := t.TempDir()
	writeWorkflowGo(t, wfDir, "wf.go", `	h.DurableCall("myplugin", "op", nil)`)

	out := runHelper(t, "--plugins", pluginsDir, "--workflows", wfDir)
	if !strings.Contains(out, "Scanned") {
		t.Errorf("expected 'Scanned' summary in default format, got: %s", out)
	}
}

// TestMain_WorkflowsFlagRejectedError verifies flag parsing rejects an unknown flag.
func TestMain_UnknownFlag(t *testing.T) {
	out := runHelper(t, "--bogus")
	if !strings.Contains(out, "flag provided but not defined") && !strings.Contains(out, "Usage") {
		t.Errorf("expected unknown flag error, got: %s", out)
	}
}
