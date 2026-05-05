// Command durable-gen generates typed client wrappers for durable services.
//
// Usage:
//
//	durable-gen client [-o <file>] [-service <name>] [-p <package>] <spec-dir>
//
// The spec directory should contain Go files defining request/response struct
// types and a Client interface. The generator produces a concrete implementation
// that uses DurableCallTyped to eliminate magic strings.
//
// Example spec (spec/payments/spec.go):
//
//	package payments_spec
//
//	type ChargeRequest struct {
//	    UserID      string `json:"user_id"`
//	    AmountCents int    `json:"amount_cents"`
//	    Currency    string `json:"currency"`
//	}
//
//	type ChargeResponse struct {
//	    ChargeID string `json:"charge_id"`
//	    Status   string `json:"status"`
//	}
//
//	type Client interface {
//	    Charge(req ChargeRequest) (*ChargeResponse, error)
//	}
//
// The service name is derived from the spec package name by stripping the
// "_spec" suffix (e.g., "payments_spec" -> "payments"). Override with -service.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// FieldInfo holds information about a struct field.
type FieldInfo struct {
	Name string
	Type string
	Tag  string
}

// TypeInfo holds information about a struct type definition.
type TypeInfo struct {
	Name   string
	Fields []FieldInfo
}

// MethodInfo holds information about an interface method.
type MethodInfo struct {
	Name         string
	RequestType  string
	ResponseType string // empty when the method has no response data (only error)
}

// HasResponse returns true when the method returns a response type.
func (m MethodInfo) HasResponse() bool {
	return m.ResponseType != ""
}

// SpecInfo holds the parsed specification.
type SpecInfo struct {
	PackageName string
	ServiceName string
	Types       []TypeInfo
	Methods     []MethodInfo
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: durable-gen client [-o <file>] [-service <name>] [-p <package>] <spec-dir>\n")
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "client":
		runClient(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		fmt.Fprintf(os.Stderr, "Usage: durable-gen client [-o <file>] [-service <name>] [-p <package>] <spec-dir>\n")
		os.Exit(1)
	}
}

func runClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	outputFile := fs.String("o", "", "output file path")
	serviceName := fs.String("service", "", "service name (defaults to spec package name with _spec suffix stripped)")
	outPkg := fs.String("p", "", "output package name (defaults to service name)")
	fs.Parse(args)

	remainder := fs.Args()
	if len(remainder) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: durable-gen client [-o <file>] [-service <name>] [-p <package>] <spec-dir>\n")
		os.Exit(1)
	}
	specDir := remainder[0]

	spec, err := parseSpecDir(specDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Determine service name.
	svc := *serviceName
	if svc == "" {
		svc = strings.TrimSuffix(spec.PackageName, "_spec")
		if svc == spec.PackageName {
			svc = spec.PackageName // no suffix to strip, use as-is
		}
	}
	spec.ServiceName = svc

	// Determine output package name.
	pkg := *outPkg
	if pkg == "" {
		pkg = svc
	}

	code, err := generateCode(spec, pkg)
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

// parseSpecDir parses all .go files in the given directory and extracts
// struct type definitions and the Client interface.
func parseSpecDir(dir string) (*SpecInfo, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsing spec directory %s: %w", dir, err)
	}

	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no Go packages found in %s", dir)
	}

	// Use the first (and usually only) package.
	var pkg *ast.Package
	for _, p := range pkgs {
		pkg = p
		break
	}

	spec := &SpecInfo{
		PackageName: pkg.Name,
	}

	// Parse all files in the package.
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, specNode := range genDecl.Specs {
				typeSpec, ok := specNode.(*ast.TypeSpec)
				if !ok {
					continue
				}

				switch t := typeSpec.Type.(type) {
				case *ast.StructType:
					ti := TypeInfo{Name: typeSpec.Name.Name}
					for _, field := range t.Fields.List {
						if len(field.Names) == 0 {
							// Embedded field; skip or handle by name.
							continue
						}
						fi := FieldInfo{
							Name: field.Names[0].Name,
							Type: exprToString(field.Type),
						}
						if field.Tag != nil {
							fi.Tag = field.Tag.Value
						}
						ti.Fields = append(ti.Fields, fi)
					}
					spec.Types = append(spec.Types, ti)

				case *ast.InterfaceType:
					if typeSpec.Name.Name == "Client" {
						for _, method := range t.Methods.List {
							ft, ok := method.Type.(*ast.FuncType)
							if !ok || len(method.Names) == 0 {
								continue
							}
							mi := MethodInfo{Name: method.Names[0].Name}

							// Request type is the first parameter.
							if ft.Params != nil && len(ft.Params.List) > 0 {
								mi.RequestType = exprToString(ft.Params.List[0].Type)
							}

							// Response type is the first non-error result.
							if ft.Results != nil {
								for _, result := range ft.Results.List {
									typeName := exprToString(result.Type)
									if typeName != "error" {
										mi.ResponseType = strings.TrimPrefix(typeName, "*")
										break
									}
								}
							}

							spec.Methods = append(spec.Methods, mi)
						}
					}
				}
			}
		}
	}

	return spec, nil
}

// exprToString converts an AST expression to its Go source string representation.
func exprToString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprToString(t.X)
	case *ast.SelectorExpr:
		return exprToString(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + exprToString(t.Elt)
		}
		return "[" + exprToString(t.Len) + "]" + exprToString(t.Elt)
	case *ast.MapType:
		return "map[" + exprToString(t.Key) + "]" + exprToString(t.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.Ellipsis:
		return "..." + exprToString(t.Elt)
	default:
		return fmt.Sprintf("%T", expr)
	}
}

// generateCode produces the typed client Go source code.
func generateCode(spec *SpecInfo, pkgName string) ([]byte, error) {
	tmpl, err := template.New("client").Parse(clientTemplate)
	if err != nil {
		return nil, fmt.Errorf("parsing template: %w", err)
	}

	data := struct {
		PackageName string
		ServiceName string
		Types       []TypeInfo
		Methods     []MethodInfo
	}{
		PackageName: pkgName,
		ServiceName: spec.ServiceName,
		Types:       spec.Types,
		Methods:     spec.Methods,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("executing template: %w", err)
	}

	// Attempt to format the output; fall back to raw if formatting fails.
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return buf.Bytes(), nil
	}
	return formatted, nil
}

const clientTemplate = `// Code generated by durable-gen. DO NOT EDIT.

package {{.PackageName}}

import "github.com/rcownie/durable/durable"

{{range .Types}}
type {{.Name}} struct {
{{- range .Fields}}
	{{.Name}} {{.Type}} {{.Tag}}
{{- end}}
}
{{end}}

type Client struct {
	h durable.HostCalls
}

func NewClient(h durable.HostCalls) *Client {
	return &Client{h: h}
}

{{range .Methods}}
{{- $svc := $.ServiceName}}
func (c *Client) {{.Name}}(req {{.RequestType}}) {{if .HasResponse}}(*{{.ResponseType}}, error){{else}}error{{end}} {
	{{- if .HasResponse}}
	var resp {{.ResponseType}}
	if err := c.h.DurableCallTyped("{{$svc}}", "{{.Name}}", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
	{{- else}}
	return c.h.DurableCallTyped("{{$svc}}", "{{.Name}}", req, nil)
	{{- end}}
}
{{end}}`
