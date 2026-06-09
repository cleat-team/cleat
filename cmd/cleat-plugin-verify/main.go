// Command cleat-plugin-verify validates that all DurableCall/PluginCall plugin
// names in workflow Go files match registered plugin names.
//
// This prevents the #1 runtime bug class: a mismatched plugin name compiles
// fine but fails silently at runtime when the workflow tries to call an
// unregistered plugin.
//
// Usage:
//
//	cleat-plugin-verify --plugins ./plugins/ --workflows ../my-app/workflows/
//	cleat-plugin-verify --json --plugins ./plugins/ --workflows ../app1/workflows/ --workflows ../app2/workflows/
//	cleat-plugin-verify --ci --plugins ./plugins/ --workflows ../app/workflows/  (GitHub Actions)
//
// Exit code: 0 if all plugin names match, 1 if mismatches are found.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// PluginEntry describes a discovered plugin.
type PluginEntry struct {
	Name string
	Dir  string
}

// VetResult mirrors the cleat vet output format for consistency.
type VetResult struct {
	Code       string   `json:"code"`
	File       string   `json:"file"`
	Line       int      `json:"line"`
	Column     int      `json:"column"`
	Message    string   `json:"message"`
	Suggestion string   `json:"suggestion"`
	Chain      []string `json:"chain,omitempty"`
}

// VetOutput is the top-level JSON output structure (matches cleat vet).
type VetOutput struct {
	Errors   []VetResult `json:"errors"`
	Warnings []VetResult `json:"warnings"`
	Summary  VetSummary  `json:"summary"`
}

// VetSummary aggregates scan counts.
type VetSummary struct {
	PluginsScanned    int `json:"plugins_scanned"`
	WorkflowFiles     int `json:"workflow_files"`
	DurableCallsFound int `json:"durable_calls_found"`
	ErrorsFound       int `json:"errors_found"`
}

func main() {
	pluginsDir := flag.String("plugins", "", "Directory containing plugin subdirectories")
	jsonOut := flag.Bool("json", false, "Output results as JSON")
	ciOut := flag.Bool("ci", false, "GitHub Actions annotation format (takes precedence over --json)")

	var workflowDirs []string
	flag.Func("workflows", "Workflow directory or .go file (repeatable)", func(s string) error {
		workflowDirs = append(workflowDirs, s)
		return nil
	})

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: cleat-plugin-verify [--json|--ci] --plugins <dir> --workflows <dir> [--workflows <dir> ...]\n")
		fmt.Fprintf(os.Stderr, "\nValidates that all DurableCall/PluginCall plugin names in workflow files\n")
		fmt.Fprintf(os.Stderr, "match registered plugin names. Exits with code 1 on mismatches.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	if *pluginsDir == "" {
		fmt.Fprintf(os.Stderr, "Error: --plugins is required\n")
		flag.Usage()
		os.Exit(1)
	}
	if len(workflowDirs) == 0 {
		fmt.Fprintf(os.Stderr, "Error: at least one --workflows is required\n")
		flag.Usage()
		os.Exit(1)
	}

	useCI := *ciOut
	useJSON := *jsonOut && !useCI

	// --- Step 1: Scan all plugin directories to build the known name set ---
	plugins := scanPlugins(*pluginsDir)
	if len(plugins) == 0 {
		fmt.Fprintf(os.Stderr, "Error: no plugins found in %s\n", *pluginsDir)
		os.Exit(1)
	}

	knownNames := make(map[string]string) // name -> plugin directory (for suggestions)
	knownByDir := make(map[string]string) // dir -> name (for reverse lookup)
	for _, p := range plugins {
		knownNames[p.Name] = p.Dir
		knownByDir[p.Dir] = p.Name
	}

	if !useJSON && !useCI {
		fmt.Fprintf(os.Stderr, "Found %d registered plugin(s): %s\n",
			len(plugins), strings.Join(sortedMapKeys(knownNames), ", "))
		fmt.Fprintf(os.Stderr, "Scanning %d workflow director(ies)...\n", len(workflowDirs))
	}

	// --- Step 2: Scan workflow directories for DurableCall/PluginCall usage ---
	var allErrors []VetResult
	totalFiles := 0
	totalCalls := 0

	for _, wfDir := range workflowDirs {
		files, calls, errs := scanWorkflowDir(wfDir, knownNames)
		totalFiles += files
		totalCalls += calls
		allErrors = append(allErrors, errs...)
	}

	// Sort errors deterministically by file, then line.
	sort.Slice(allErrors, func(i, j int) bool {
		if allErrors[i].File != allErrors[j].File {
			return allErrors[i].File < allErrors[j].File
		}
		return allErrors[i].Line < allErrors[j].Line
	})

	output := VetOutput{
		Errors:   allErrors,
		Warnings: nil,
		Summary: VetSummary{
			PluginsScanned:    len(plugins),
			WorkflowFiles:     totalFiles,
			DurableCallsFound: totalCalls,
			ErrorsFound:       len(allErrors),
		},
	}

	// --- Step 3: Report results ---
	switch {
	case useCI:
		for _, e := range allErrors {
			fmt.Printf("::error file=%s,line=%d,title=P001::%s", e.File, e.Line, e.Message)
			if e.Suggestion != "" {
				fmt.Printf(" (%s)", e.Suggestion)
			}
			fmt.Println()
		}
		if len(allErrors) == 0 {
			fmt.Println("All plugin names verified successfully.")
		}

	case useJSON:
		jsonBytes, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error marshaling JSON: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(jsonBytes))

	default:
		if len(allErrors) > 0 {
			fmt.Fprintf(os.Stderr, "\nFound %d plugin name mismatch(es):\n\n", len(allErrors))
			for _, e := range allErrors {
				fmt.Fprintf(os.Stderr, "  P001 %s:%d: %s\n", e.File, e.Line, e.Message)
				if e.Suggestion != "" {
					fmt.Fprintf(os.Stderr, "       %s\n", e.Suggestion)
				}
			}
			fmt.Fprintln(os.Stderr)
		}
		fmt.Fprintf(os.Stderr, "Scanned %d plugins, %d workflow file(s), %d DurableCall(s). ",
			len(plugins), totalFiles, totalCalls)
		if len(allErrors) == 0 {
			fmt.Fprintln(os.Stderr, "All plugin names match!")
		} else {
			fmt.Fprintf(os.Stderr, "%d error(s).\n", len(allErrors))
		}
	}

	if len(allErrors) > 0 {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Plugin scanning — extract registered names from plugin.go files
// ---------------------------------------------------------------------------

// scanPlugins reads all plugin subdirectories and extracts their registered names.
func scanPlugins(pluginsDir string) []PluginEntry {
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading plugins directory %s: %v\n", pluginsDir, err)
		os.Exit(1)
	}

	var plugins []PluginEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pluginDir := filepath.Join(pluginsDir, e.Name())
		pluginGo := filepath.Join(pluginDir, "plugin.go")
		if _, err := os.Stat(pluginGo); os.IsNotExist(err) {
			continue
		}

		name := extractPluginName(pluginGo)
		if name != "" {
			plugins = append(plugins, PluginEntry{Name: name, Dir: pluginDir})
		}
	}
	return plugins
}

// extractPluginName parses a plugin.go file and extracts the registered plugin name
// from the Info() method (primary) or from plugin.Register() in init() (fallback).
func extractPluginName(pluginGo string) string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, pluginGo, nil, parser.ParseComments)
	if err != nil {
		return ""
	}

	// Primary: find Info() method returning plugin.PluginInfo{Name: "..."}
	for _, decl := range f.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if funcDecl.Name.Name != "Info" {
			continue
		}
		if funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
			continue
		}
		if funcDecl.Body == nil {
			continue
		}
		if name := extractNameFromReturn(funcDecl.Body); name != "" {
			return name
		}
	}

	// Fallback: find plugin.Register(plugin.PluginInfo{Name: "..."}) in init()
	for _, decl := range f.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Name.Name != "init" {
			continue
		}
		if funcDecl.Body == nil {
			continue
		}
		if name := extractNameFromRegister(funcDecl.Body); name != "" {
			return name
		}
	}

	return ""
}

// extractNameFromReturn looks for `return plugin.PluginInfo{Name: "...", ...}`
// in a function body.
func extractNameFromReturn(body *ast.BlockStmt) string {
	for _, stmt := range body.List {
		ret, ok := stmt.(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			continue
		}
		cl, ok := ret.Results[0].(*ast.CompositeLit)
		if !ok {
			continue
		}
		if name := extractNameFromCompositeLit(cl); name != "" {
			return name
		}
	}
	return ""
}

// extractNameFromRegister looks for `plugin.Register(plugin.PluginInfo{Name: "..."}, ...)`
// in an init() function body.
func extractNameFromRegister(body *ast.BlockStmt) string {
	for _, stmt := range body.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		ce, ok := expr.X.(*ast.CallExpr)
		if !ok || len(ce.Args) == 0 {
			continue
		}
		// Verify it's a call to plugin.Register
		sel, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Register" {
			continue
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "plugin" {
			continue
		}
		// First arg should be plugin.PluginInfo{...}
		cl, ok := ce.Args[0].(*ast.CompositeLit)
		if !ok {
			continue
		}
		if name := extractNameFromCompositeLit(cl); name != "" {
			return name
		}
	}
	return ""
}

// extractNameFromCompositeLit extracts the Name field from a composite literal
// like plugin.PluginInfo{Name: "llm", ...}.
func extractNameFromCompositeLit(cl *ast.CompositeLit) string {
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Name" {
			continue
		}
		val, ok := kv.Value.(*ast.BasicLit)
		if !ok || val.Kind != token.STRING {
			continue
		}
		s, err := strconv.Unquote(val.Value)
		if err != nil {
			return ""
		}
		return s
	}
	return ""
}

// ---------------------------------------------------------------------------
// Workflow scanning — find DurableCall/PluginCall invocations and verify names
// ---------------------------------------------------------------------------

// directMethods are method names where arg[0] is the plugin/service name.
// These call patterns accept their first string argument as the plugin name:
//
//	h.DurableCall("plugin", "op", req)
//	h.Call("plugin", "op", req)
//	h.PluginCall("plugin", "func", input)
//	h.DurableCallJSON("plugin", "op", req, &resp)
//	h.DurableCallTyped("plugin", "op", req, &resp)
//	h.DurableCallWithHeartbeat("plugin", "op", req, interval, cb)
//	h.PluginCallStreaming("plugin", "func", input)
//	env.OnCall("plugin", "op", matcher)           // test mock setup
var directMethods = map[string]bool{
	"DurableCall":              true,
	"Call":                     true,
	"PluginCall":               true,
	"DurableCallJSON":          true,
	"DurableCallTyped":         true,
	"DurableCallWithHeartbeat": true,
	"PluginCallStreaming":      true,
	"OnCall":                   true,
}

// offsetMethods are method names where arg[1] is the plugin/service name
// because arg[0] is a CallOptions/opts struct:
//
//	h.DurableCallWithOptions(opts, "plugin", "op", req)
//	h.CallWithOptions(opts, "plugin", "op", req)
var offsetMethods = map[string]bool{
	"DurableCallWithOptions":      true,
	"CallWithOptions":             true,
	"DurableCallJSONWithOptions":  true,
	"DurableCallTypedWithOptions": true,
}

// pkgFuncs are package-level generic functions from the cleat SDK where the
// value is the argument index of the plugin name:
//
//	cleat.CallTyped[T](h, "plugin", "op", req)         -> arg 1
//	cleat.PluginCallTyped[T](h, "plugin", "func", req) -> arg 1
//	cleat.CallTypedWithOptions[T](h, opts, "plugin", "op", req) -> arg 2
var pkgFuncs = map[string]int{
	"CallTyped":            1,
	"PluginCallTyped":      1,
	"CallTypedWithOptions": 2,
}

// scanWorkflowDir walks a directory and scans all .go files.
// Returns file count, total DurableCall count, and any errors.
func scanWorkflowDir(dir string, knownNames map[string]string) (files int, calls int, errors []VetResult) {
	info, err := os.Stat(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot access %s: %v\n", dir, err)
		return 0, 0, nil
	}

	if !info.IsDir() {
		// Single file
		if strings.HasSuffix(dir, ".go") {
			fileErrors, fileCalls := scanWorkflowFile(dir, knownNames)
			return 1, fileCalls, fileErrors
		}
		return 0, 0, nil
	}

	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// Skip hidden dirs, testdata, vendor, node_modules
			if d.Name() != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			if d.Name() == "testdata" || d.Name() == "vendor" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fileErrors, fileCalls := scanWorkflowFile(path, knownNames)
		files++
		calls += fileCalls
		errors = append(errors, fileErrors...)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: error walking %s: %v\n", dir, err)
	}
	return
}

// scanWorkflowFile parses a single .go file and finds all DurableCall/PluginCall
// invocations, checking each plugin name against known names.
func scanWorkflowFile(path string, knownNames map[string]string) (errors []VetResult, callCount int) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, 0
	}

	ast.Inspect(f, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		var pluginName string
		pos := fset.Position(ce.Pos())

		switch fun := ce.Fun.(type) {
		case *ast.SelectorExpr:
			// Case 1: Regular method call: receiver.MethodName(...)
			if _, ok := fun.X.(*ast.Ident); ok {
				selName := fun.Sel.Name
				if directMethods[selName] {
					pluginName = extractStringArg(ce, 0)
				} else if offsetMethods[selName] {
					pluginName = extractStringArg(ce, 1)
				}
			}
			// Case 2: Package function call: cleat.FuncName(...) (non-generic)
			if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "cleat" {
				selName := fun.Sel.Name
				if argIdx, ok := pkgFuncs[selName]; ok {
					pluginName = extractStringArg(ce, argIdx)
				}
			}

		case *ast.IndexExpr:
			// Case 3: Generic call: receiver.MethodName[T](...) or cleat.FuncName[T](...)
			if sel, ok := fun.X.(*ast.SelectorExpr); ok {
				selName := sel.Sel.Name

				// Method on a receiver: h.DurableCallTyped[T](...)
				if _, ok := sel.X.(*ast.Ident); ok {
					if directMethods[selName] {
						pluginName = extractStringArg(ce, 0)
					} else if offsetMethods[selName] {
						pluginName = extractStringArg(ce, 1)
					}
				}

				// Package function: cleat.FuncName[T](h, ...)
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "cleat" {
					if argIdx, ok := pkgFuncs[selName]; ok {
						pluginName = extractStringArg(ce, argIdx)
					}
				}
			}
		}

		if pluginName == "" {
			return true
		}

		callCount++

		// Check if this plugin name is known.
		if _, known := knownNames[pluginName]; !known {
			suggestion := bestMatch(pluginName, knownNames)
			errors = append(errors, VetResult{
				Code:       "P001",
				File:       path,
				Line:       pos.Line,
				Column:     pos.Column,
				Message:    fmt.Sprintf("Unknown plugin %q in DurableCall", pluginName),
				Suggestion: suggestion,
			})
		}

		return true
	})

	return errors, callCount
}

// extractStringArg extracts a string literal argument from a call expression.
// Returns "" if the argument is not a string literal (e.g., variable, expression).
func extractStringArg(ce *ast.CallExpr, idx int) string {
	if idx >= len(ce.Args) {
		return ""
	}
	lit, ok := ce.Args[idx].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return s
}

// ---------------------------------------------------------------------------
// "Did you mean?" suggestion via Levenshtein distance
// ---------------------------------------------------------------------------

// bestMatch finds the closest matching plugin name using Levenshtein distance.
// Returns an empty string if no close match is found (distance > 3).
func bestMatch(name string, knownNames map[string]string) string {
	if len(knownNames) == 0 {
		return ""
	}

	bestName := ""
	bestDist := 999

	for known := range knownNames {
		dist := levenshtein(name, known)
		if dist < bestDist {
			bestDist = dist
			bestName = known
		}
	}

	if bestDist > 0 && bestDist <= 3 {
		dir := knownNames[bestName]
		return fmt.Sprintf("Did you mean %q? (registered in %s)", bestName, dir)
	}
	return ""
}

// levenshtein computes the Levenshtein distance between two strings
// using the single-row optimization for efficiency.
func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)

	for j := range prev {
		prev[j] = j
	}

	for i, ca := range a {
		curr[0] = i + 1
		for j, cb := range b {
			cost := 1
			if ca == cb {
				cost = 0
			}
			curr[j+1] = minInt(curr[j]+1, minInt(prev[j+1]+1, prev[j]+cost))
		}
		prev, curr = curr, prev
	}

	return prev[len(b)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// sortedMapKeys returns the sorted keys of a string->string map.
func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
