//go:build ignore

// Command finddeadexports lists every exported top-level function and
// exported method declared in the given directories (recursively), skipping
// _test.go files as a source of declarations (a declaration that only exists
// for tests to call isn't a public-API candidate).
//
// Output, one declaration per line, tab-separated:
//
//	<file>	<line>	<receiver-or-dash>	<search-identifier>
//
// search-identifier is the bare function name for a top-level func, or the
// bare method name for a method (not Receiver.Method — callers write
// x.Deploy(...), not AuditLog.Deploy(...), so the receiver type is dropped
// before the caller side is grepped; it is reported separately only so a
// human reading the baseline can tell which type a method belongs to).
//
// ALSO EMITS one line per exported METHOD SIGNATURE inside an interface type,
// with receiver "interface" rather than a concrete type name (cleat#3284).
// These are not candidates to report as dead or test-only on their own — an
// interface signature cannot be deleted or called independently of its
// implementations — but check-dead-exports.sh needs their file:line the same
// way it needs every implementation's: a one-interface/N-implementations
// method spells its bare name once per implementation PLUS once in the
// interface signature, and a grep cannot tell any of those N+1 declaration
// lines apart from a genuine call expression. Excluding only the single
// declaration under test (the pre-cleat#3284 behaviour) left the other N
// declaration lines, including the interface's own signature, free to
// satisfy the grep as if they were callers. See check-dead-exports.sh's
// code_use_filter and the exclusion-set construction around it.
//
// This existed as a build-tagged throwaway rather than a package under
// scripts/ so that `go build ./...` and `go vet ./...` never see it as part
// of the module.
//
// Used by scripts/check-dead-exports.sh; not meant to be run standalone
// except for debugging it.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		fmt.Fprintln(os.Stderr, "usage: go run finddeadexports.go <dir> [<dir> ...]")
		os.Exit(1)
	}

	type decl struct {
		file string
		line int
		recv string
		name string
	}
	var decls []decl

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				base := filepath.Base(path)
				if base != "." && strings.HasPrefix(base, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if perr != nil {
				// A file that fails to parse under the host's default build
				// tags (e.g. a cgo-gated file when CGO_ENABLED=0) is skipped
				// rather than treated as an error -- same blind spot
				// check-test-only-code.sh documents for staticcheck, and for
				// the same reason: this tool cannot see declarations behind
				// a build tag it did not select, so it must not report on
				// them at all rather than report wrongly.
				fmt.Fprintf(os.Stderr, "skip (parse error): %s: %v\n", path, perr)
				return nil
			}
			for _, top := range f.Decls {
				switch d := top.(type) {
				case *ast.FuncDecl:
					if d.Name == nil || !d.Name.IsExported() {
						continue
					}
					recv := "-"
					if d.Recv != nil && len(d.Recv.List) > 0 {
						recv = recvTypeName(d.Recv.List[0].Type)
					}
					pos := fset.Position(d.Pos())
					decls = append(decls, decl{file: path, line: pos.Line, recv: recv, name: d.Name.Name})
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						iface, ok := ts.Type.(*ast.InterfaceType)
						if !ok || iface.Methods == nil {
							continue
						}
						for _, field := range iface.Methods.List {
							// An embedded interface has no Names (it is
							// itself a Type reference, not a method); only
							// named methods are declaration sites for a
							// method's own bare identifier.
							for _, methodName := range field.Names {
								if !methodName.IsExported() {
									continue
								}
								pos := fset.Position(methodName.Pos())
								decls = append(decls, decl{file: path, line: pos.Line, recv: "interface", name: methodName.Name})
							}
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "walk error:", err)
			os.Exit(1)
		}
	}

	sort.Slice(decls, func(i, j int) bool {
		if decls[i].file != decls[j].file {
			return decls[i].file < decls[j].file
		}
		return decls[i].line < decls[j].line
	})

	for _, d := range decls {
		fmt.Printf("%s\t%d\t%s\t%s\n", d.file, d.line, d.recv, d.name)
	}
}

// recvTypeName extracts the bare type name off a method receiver
// expression, stripping pointer and generic-instantiation wrappers
// (e.g. *Foo, Foo[T]) down to the identifier.
func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	default:
		return "?"
	}
}
