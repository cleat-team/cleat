// plugin-client generates a typed caller-side client for a plugin's
// plugin.RegisterTyped[Req,Resp] host functions.
//
// Usage:
//
//	cleat-gen plugin-client -plugin <name> [-o <file>] [-p <package>] <plugin-package>
//
// The source of truth is the plugin's own, compiler-checked registration
// call sites -- never a hand-maintained plugin.json manifest (cleat#2656
// records one that silently drifted from its plugin's real types). This
// tool loads <plugin-package> with full type information, finds every
// plugin.RegisterTyped[Req,Resp](scope, opts, fn) call, and for each one
// reads:
//
//   - Req and Resp off go/types' recorded instantiation for that call --
//     these are almost always INFERRED, not spelled out at the call site
//     (RegisterTyped(scope, opts, p.sendMessage) needs no
//     RegisterTyped[SendMessageInput, SendMessageOutput] annotation), so a
//     bare AST parse cannot see them; only a type-checked load can.
//   - the operation name off opts's literal FuncOptions{Name: "..."} field.
//
// It emits one Go source file: a struct redeclaration per distinct Req/Resp
// type, plus a package-level cleat.NewPluginFunc[Req,Resp](plugin, op) var
// per registered function, named from the operation name
// ("send_message" -> SendMessage).
//
// The structs are redeclared rather than imported from the plugin's own
// package on purpose: a workflow calling the generated client compiles to
// WASM, and a plugin's implementation package typically imports things (a
// SQL driver, net/http, ...) that do not build for that target. Redeclaring
// keeps the generated client's only cleat-side dependency on
// "github.com/cleat-team/cleat/cleat", exactly like cleat-gen's existing
// "client" command.
//
// WHERE THE OUTPUT FILE HAS TO LIVE, IF A WORKFLOW `cleat build` COMPILES
// WILL CALL IT. `cleat build` stages a workflow by globbing *.go
// NON-RECURSIVELY in the workflow's own source directory (wasm/build.go,
// PrepareBuildDir) and does not pull in ANY sibling package by copying it in
// -- so an arbitrary subpackage sitting next to the workflow's own source
// cannot be imported by such a workflow at all; `go mod tidy` in the
// isolated build directory falls through to the module proxy and fails with
// "module ... found, but does not contain package .../yourclient" (measured
// live in cleat#2626's own PR, the first time this was tried, against
// examples/, which is cleat's own separate and unpublished Go module).
//
// Two placements avoid that, for different reasons, and the choice affects
// what the client's line count actually represents:
//
//   - SAME PACKAGE AS THE WORKFLOW: generate with `-p <the workflow's
//     package name>` and `-o <workflow dir>/<plugin>_client_gen.go`, call
//     its vars unqualified. No cross-package import exists at all, so
//     staging is never in question. This makes the client that workflow's
//     OWN code -- its line count is a real, non-amortised cost of that one
//     workflow, and a second workflow calling the same plugin generates and
//     pays for its own separate copy.
//   - A PACKAGE cleat build ALREADY LOCALLY REPLACES FOR EVERY WORKFLOW:
//     wasm/build.go's generated go.mod carries a `replace
//     github.com/cleat-team/cleat/cleat => <local checkout>/cleat`
//     unconditionally, for every workflow build in this repository -- so
//     any package under cleat/ (e.g. cleat/pluginclients/<plugin>/) resolves
//     the same way the cleat package itself already does, with no staging
//     involved and no proxy fallback possible. For a BUNDLED plugin (one
//     shipped in this repository), this is the right default: the client is
//     genuinely shared, platform-provided code, reused by every workflow
//     that calls that plugin rather than paid for again per caller, and its
//     line count should be accounted for that way rather than folded into
//     any one workflow's total. For a THIRD-PARTY, out-of-tree plugin,
//     there is no cleat/ to place it under -- the analogous move is
//     generating into the PLUGIN AUTHOR'S OWN published module, which an
//     importing workflow then depends on as an ordinary external module
//     (resolved via go.sum/the proxy normally, since it is properly
//     published and versioned there -- the failure mode above is specific
//     to an UNPUBLISHED same-repo sibling module, not to external
//     dependencies as such).
//
// This repository's own bundled-plugin clients live under
// cleat/pluginclients/ for exactly this reason: see
// examples/order-lifecycle, and cleat#2626/cleat#2658 for the fuller
// accounting of why the two placements are not cosmetic alternatives.
//
// Neither constraint touches a native Go caller (cleat/embedded, cleatctl,
// a plugin's own tests): `cleat build`'s staging model is specific to a
// WASM-compiled workflow, and a native caller can use any package normally.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/cleat-team/cleat/internal/analyzer"
	"golang.org/x/tools/go/packages"
)

// PluginFuncInfo describes one generated cleat.NewPluginFunc var.
type PluginFuncInfo struct {
	VarName      string
	OpName       string
	RequestType  string
	ResponseType string
}

// PluginClientSpec holds everything needed to render a plugin-client file.
type PluginClientSpec struct {
	SourcePackageName string
	PluginName        string
	Types             []TypeInfo
	Funcs             []PluginFuncInfo
	Imports           []string
}

func runPluginClient(args []string) {
	fs := flag.NewFlagSet("plugin-client", flag.ExitOnError)
	outputFile := fs.String("o", "", "output file path")
	pluginName := fs.String("plugin", "", "the plugin's registered name, e.g. \"slack-notify\" (required)")
	outPkg := fs.String("p", "", "output package name (defaults to the source package's name)")
	_ = fs.Parse(args)

	remainder := fs.Args()
	if len(remainder) < 1 || *pluginName == "" {
		fmt.Fprintf(os.Stderr, "Usage: cleat-gen plugin-client -plugin <name> [-o <file>] [-p <package>] <plugin-package>\n")
		os.Exit(1)
	}
	pkgPattern := remainder[0]

	spec, err := loadPluginClientSpec(pkgPattern, *pluginName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	outPkgName := *outPkg
	if outPkgName == "" {
		outPkgName = spec.SourcePackageName
	}

	code, err := generatePluginClientCode(spec, outPkgName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating code: %v\n", err)
		os.Exit(1)
	}

	if *outputFile != "" {
		dir := filepath.Dir(*outputFile)
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating directory %s: %v\n", dir, err)
			os.Exit(1)
		}
		if err := os.WriteFile(*outputFile, code, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing file %s: %v\n", *outputFile, err)
			os.Exit(1)
		}
	} else {
		os.Stdout.Write(code)
	}
}

const registerTypedPkgPath = "github.com/cleat-team/cleat/plugin"

// loadPluginClientSpec type-checks pkgPattern and reads every
// plugin.RegisterTyped call site it contains.
func loadPluginClientSpec(pkgPattern, pluginName string) (*PluginClientSpec, error) {
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Mode:  analyzer.LoadMode,
		Fset:  fset,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, pkgPattern)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", pkgPattern, err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no package found at %s", pkgPattern)
	}
	var loadErrs []error
	packages.Visit(pkgs, func(p *packages.Package) bool {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, fmt.Errorf("%s: %v", p.PkgPath, e))
		}
		return true
	}, nil)
	if len(loadErrs) > 0 {
		return nil, fmt.Errorf("package load errors: %v", loadErrs)
	}

	pkg := pkgs[0]
	spec := &PluginClientSpec{SourcePackageName: pkg.Name, PluginName: pluginName}

	seenTypes := map[string]string{} // short name -> fully qualified name
	importSet := map[string]bool{}
	// A type declared in pkg itself (the plugin's own implementation
	// package) is redeclared locally in the generated client rather than
	// imported -- see collectNestedSamePackageTypes -- so the qualifier
	// gives it no prefix. Anything else is a normal foreign-package
	// reference, imported and prefixed as usual.
	qualifier := func(p *types.Package) string {
		if p == nil || p == pkg.Types {
			return ""
		}
		importSet[p.Path()] = true
		return p.Name()
	}

	for _, file := range pkg.Syntax {
		var walkErr error
		ast.Inspect(file, func(n ast.Node) bool {
			if walkErr != nil {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fnIdent := registerTypedIdent(call.Fun)
			if fnIdent == nil {
				return true
			}
			fn, ok := pkg.TypesInfo.Uses[fnIdent].(*types.Func)
			if !ok || fn.Pkg() == nil || fn.Pkg().Path() != registerTypedPkgPath || fn.Name() != "RegisterTyped" {
				return true
			}

			pos := fset.Position(call.Pos())
			inst, ok := pkg.TypesInfo.Instances[fnIdent]
			if !ok || inst.TypeArgs == nil || inst.TypeArgs.Len() != 2 {
				walkErr = fmt.Errorf("%s: could not resolve RegisterTyped's Req/Resp type arguments", pos)
				return false
			}
			if len(call.Args) < 2 {
				walkErr = fmt.Errorf("%s: RegisterTyped call is missing arguments", pos)
				return false
			}

			opName, err := extractFuncOptionsName(call.Args[1])
			if err != nil {
				walkErr = fmt.Errorf("%s: %w", pos, err)
				return false
			}

			reqName, err := collectStructType(inst.TypeArgs.At(0), pkg.Types, spec, seenTypes, qualifier)
			if err != nil {
				walkErr = fmt.Errorf("%s: request type: %w", pos, err)
				return false
			}
			respName, err := collectStructType(inst.TypeArgs.At(1), pkg.Types, spec, seenTypes, qualifier)
			if err != nil {
				walkErr = fmt.Errorf("%s: response type: %w", pos, err)
				return false
			}

			spec.Funcs = append(spec.Funcs, PluginFuncInfo{
				VarName:      opNameToGoIdent(opName),
				OpName:       opName,
				RequestType:  reqName,
				ResponseType: respName,
			})
			return true
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}

	if len(spec.Funcs) == 0 {
		return nil, fmt.Errorf("no plugin.RegisterTyped call sites found in %s", pkgPattern)
	}

	for path := range importSet {
		spec.Imports = append(spec.Imports, path)
	}
	sort.Strings(spec.Imports)
	sort.Slice(spec.Funcs, func(i, j int) bool { return spec.Funcs[i].OpName < spec.Funcs[j].OpName })

	return spec, nil
}

// registerTypedIdent returns the identifier naming the called function,
// whichever of the shapes go/ast uses for a (possibly generic, possibly
// explicitly instantiated) call it is written as.
func registerTypedIdent(fun ast.Expr) *ast.Ident {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel
	case *ast.Ident:
		return f
	case *ast.IndexExpr:
		return registerTypedIdent(f.X)
	case *ast.IndexListExpr:
		return registerTypedIdent(f.X)
	}
	return nil
}

// extractFuncOptionsName reads the literal string passed as
// FuncOptions{Name: "..."}. Anything else -- a variable, a non-literal
// expression -- is refused with a message saying so, rather than silently
// producing a client with the wrong operation name.
func extractFuncOptionsName(arg ast.Expr) (string, error) {
	cl, ok := arg.(*ast.CompositeLit)
	if !ok {
		return "", fmt.Errorf("the opts argument must be a FuncOptions{...} composite literal, not %T", arg)
	}
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Name" {
			continue
		}
		lit, ok := kv.Value.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", fmt.Errorf("FuncOptions.Name must be a string literal")
		}
		name, err := strconv.Unquote(lit.Value)
		if err != nil {
			return "", fmt.Errorf("FuncOptions.Name: %w", err)
		}
		return name, nil
	}
	return "", fmt.Errorf("FuncOptions literal has no Name field")
}

// collectStructType resolves t to a named struct, records its fields into
// spec.Types the first time it is seen, and returns its short name. A field
// whose type is itself a named struct declared in ownerPkg (the plugin's
// own implementation package) is redeclared too, recursively, via
// collectNestedSamePackageTypes -- the generated client must never import
// ownerPkg (see the file-level doc comment), so any type it needs from
// there has to be reproduced locally instead, the same way the top-level
// Req/Resp type already is.
func collectStructType(t types.Type, ownerPkg *types.Package, spec *PluginClientSpec, seen map[string]string, qualifier types.Qualifier) (string, error) {
	named, ok := t.(*types.Named)
	if !ok {
		return "", fmt.Errorf("%s is not a named struct type", t.String())
	}
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return "", fmt.Errorf("%s is not backed by a struct", named.String())
	}

	name := named.Obj().Name()
	if !named.Obj().Exported() {
		return "", fmt.Errorf("%s is unexported -- a RegisterTyped request/response type (or a same-package type it references) must be exported so the generated client can be used outside its own package", named.String())
	}
	fqName := named.Obj().Pkg().Path() + "." + name
	if existing, ok := seen[name]; ok {
		if existing != fqName {
			return "", fmt.Errorf("two different types are both named %q (%s and %s) -- the generator emits one Go type per short name and cannot tell them apart", name, existing, fqName)
		}
		return name, nil
	}
	seen[name] = fqName

	ti := TypeInfo{Name: name}
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if f.Embedded() {
			return "", fmt.Errorf("%s embeds %s -- embedded fields are not supported by the generator", named.String(), f.Name())
		}
		if !f.Exported() {
			return "", fmt.Errorf("%s.%s is unexported -- every field of a RegisterTyped request/response type must be exported", named.String(), f.Name())
		}
		if err := collectNestedSamePackageTypes(f.Type(), ownerPkg, spec, seen, qualifier); err != nil {
			return "", fmt.Errorf("%s.%s: %w", named.String(), f.Name(), err)
		}
		fi := FieldInfo{Name: f.Name(), Type: types.TypeString(f.Type(), qualifier)}
		if tag := st.Tag(i); tag != "" {
			fi.Tag = "`" + tag + "`"
		}
		ti.Fields = append(ti.Fields, fi)
	}
	spec.Types = append(spec.Types, ti)
	return name, nil
}

// collectNestedSamePackageTypes walks t and, for every named struct type it
// finds that is declared in ownerPkg, redeclares it via collectStructType
// (which recurses further into that type's own fields). A named type from
// any OTHER package is left alone -- types.TypeString renders it as an
// import-qualified reference instead, via qualifier.
func collectNestedSamePackageTypes(t types.Type, ownerPkg *types.Package, spec *PluginClientSpec, seen map[string]string, qualifier types.Qualifier) error {
	switch tt := t.(type) {
	case *types.Named:
		if tt.Obj().Pkg() == ownerPkg {
			_, err := collectStructType(tt, ownerPkg, spec, seen, qualifier)
			return err
		}
		return nil
	case *types.Pointer:
		return collectNestedSamePackageTypes(tt.Elem(), ownerPkg, spec, seen, qualifier)
	case *types.Slice:
		return collectNestedSamePackageTypes(tt.Elem(), ownerPkg, spec, seen, qualifier)
	case *types.Array:
		return collectNestedSamePackageTypes(tt.Elem(), ownerPkg, spec, seen, qualifier)
	case *types.Map:
		if err := collectNestedSamePackageTypes(tt.Key(), ownerPkg, spec, seen, qualifier); err != nil {
			return err
		}
		return collectNestedSamePackageTypes(tt.Elem(), ownerPkg, spec, seen, qualifier)
	}
	return nil
}

// opNameToGoIdent converts a snake_case or kebab-case operation name into
// an exported Go identifier: "send_message" -> "SendMessage".
func opNameToGoIdent(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' })
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	if b.Len() == 0 {
		return "Op"
	}
	return b.String()
}

func generatePluginClientCode(spec *PluginClientSpec, pkgName string) ([]byte, error) {
	tmpl, err := template.New("plugin-client").Parse(pluginClientTemplate)
	if err != nil {
		return nil, fmt.Errorf("parsing template: %w", err)
	}

	data := struct {
		PackageName string
		PluginName  string
		Types       []TypeInfo
		Funcs       []PluginFuncInfo
		Imports     []string
	}{
		PackageName: pkgName,
		PluginName:  spec.PluginName,
		Types:       spec.Types,
		Funcs:       spec.Funcs,
		Imports:     spec.Imports,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("executing template: %w", err)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("formatting generated source: %w\n%s", err, buf.String())
	}
	return formatted, nil
}

const pluginClientTemplate = `// Code generated by cleat-gen plugin-client. DO NOT EDIT.

package {{.PackageName}}

import (
	"github.com/cleat-team/cleat/cleat"
{{- range .Imports}}
	"{{.}}"
{{- end}}
)

{{range .Types}}
type {{.Name}} struct {
{{- range .Fields}}
	{{.Name}} {{.Type}} {{.Tag}}
{{- end}}
}
{{end}}
{{range .Funcs}}
var {{.VarName}} = cleat.NewPluginFunc[{{.RequestType}}, {{.ResponseType}}]("{{$.PluginName}}", "{{.OpName}}")
{{end}}`
