package plugin

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// envField is one field of an `Environment` struct declaration, as written
// in source: its name and the exact source text of its type expression.
type envField struct {
	name    string
	typeStr string
}

// parseEnvironmentStructFields parses src (a standalone Go file's contents)
// for `type Environment struct { ... }` and returns its fields, via
// go/parser rather than a line-oriented scan -- a field's type can itself
// contain spaces (e.g. `func(ctx context.Context, req StartRequest) (runID
// string, err error)`), which no regex reads the way the compiler does.
func parseEnvironmentStructFields(t *testing.T, filename, src string) []envField {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("%s does not parse as Go: %v\n---\n%s", filename, err, src)
	}
	var fields []envField
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Environment" {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, f := range st.Fields.List {
				var buf bytes.Buffer
				if err := format.Node(&buf, fset, f.Type); err != nil {
					t.Fatalf("%s: rendering a field's type expression: %v", filename, err)
				}
				typeStr := buf.String()
				for _, n := range f.Names {
					fields = append(fields, envField{name: n.Name, typeStr: typeStr})
				}
			}
		}
	}
	return fields
}

var fencedGoBlockRe = regexp.MustCompile("(?s)```go\\n(.*?)\\n```")

// docEnvironmentFields extracts the fields of the first ```go fenced block
// in path that declares `type Environment struct`.
func docEnvironmentFields(t *testing.T, path string) []envField {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, m := range fencedGoBlockRe.FindAllStringSubmatch(string(data), -1) {
		if strings.Contains(m[1], "type Environment struct") {
			return parseEnvironmentStructFields(t, "doc-environment-block.go", "package doc\n\n"+m[1]+"\n")
		}
	}
	t.Fatalf("%s has no ```go fenced block declaring `type Environment struct` -- the guide or this check drifted", path)
	return nil
}

// realEnvironmentFields reads plugin.go off disk and parses its actual
// Environment struct the same way docEnvironmentFields parses the doc's --
// so this compares two parses of SOURCE TEXT against each other, rather
// than a parse against a reflect.Type. That matters for a field like Mux:
// ServeMux is a type alias for *http.ServeMux, so reflect.TypeOf(Environment{})
// would report Mux's type as "*http.ServeMux" regardless of which name the
// source used, and a doc correctly written as `Mux ServeMux` would be
// flagged as wrong. Comparing source text to source text means the doc is
// checked against the same unqualified names (PluginDB, ServeMux, Dialect,
// EgressTransport, ...) a reader of plugin.go itself sees.
func realEnvironmentFields(t *testing.T) []envField {
	t.Helper()
	data, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatalf("read plugin.go: %v", err)
	}
	fields := parseEnvironmentStructFields(t, "plugin.go", string(data))
	if len(fields) == 0 {
		t.Fatal("parsed zero fields out of plugin.go's own Environment struct -- this check is broken, not the doc")
	}
	return fields
}

// TestPluginDeveloperGuideEnvironmentBlockHasNoWrongField guards against
// cleat#2711: the guide's `Environment` example showed `DB *sql.DB` (real:
// PluginDB) and `Mux *http.ServeMux` (real: ServeMux), and the surrounding
// prose ("Use env.DB for database/sql queries") followed the wrong type
// into an actively misleading sentence.
//
// This is a SUBSET check, not a completeness check: it fails if the guide
// shows a field plugin.Environment does not have, or shows a real field
// under the wrong type -- but it does NOT require the guide to list every
// field. Environment carries 19 fields today, several advanced and rarely
// needed on a first plugin (Audit, Secrets, Payloads, DeploymentSecrets,
// HostResolver/RequireHostMatch, MintOAuthAPIKey, RevokeExpiredOAuthAPIKeys,
// SetTenantSuspended). The guide's block is deliberately a curated
// "getting started" example, not a reference. A check requiring 1:1
// completeness would force that example to either grow to 19 fields (wrong
// shape for an introduction) or maintain its own "intentionally omitted"
// allowlist -- a second copy of the truth that can itself drift, which is
// the exact failure class this issue is about. Catching a field that is
// PRESENT AND WRONG is cheap, robust, and does not need one; catching a
// field that is MISSING is a different, harder problem this test does not
// attempt -- see cleat#2711's own framing.
func TestPluginDeveloperGuideEnvironmentBlockHasNoWrongField(t *testing.T) {
	real := map[string]string{}
	for _, f := range realEnvironmentFields(t) {
		real[f.name] = f.typeStr
	}

	docFields := docEnvironmentFields(t, "../docs/contributor/plugins/plugin-developer-guide.md")
	if len(docFields) == 0 {
		t.Fatal("parsed zero fields out of the guide's Environment block -- this check is broken, not the doc")
	}
	for _, df := range docFields {
		rt, ok := real[df.name]
		if !ok {
			t.Errorf("guide's Environment example shows a field %q that plugin.Environment does not have", df.name)
			continue
		}
		if rt != df.typeStr {
			t.Errorf("guide's Environment example shows `%s %s`, but plugin.Environment's %s is `%s`", df.name, df.typeStr, df.name, rt)
		}
	}
}
