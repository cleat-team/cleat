package plugin_test

import (
	"go/ast"
	"go/types"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// Enumerate every Scan target whose STATIC TYPE is uuid.UUID.
//
// cleat#1137: SQL Server returns UNIQUEIDENTIFIER in mixed-endian byte order.
// uuid.UUID's Scan accepts those 16 bytes without error and yields a DIFFERENT
// uuid. plugin.GUID exists to swap them; scanning into uuid.UUID directly is
// the fault.
//
// WHY THIS USES go/types AND NOT A REGEX, WHICH IS THE WHOLE POINT.
// `&x` is a NAME. The defect is a TYPE. The issue records two lexical readings
// that bracket the answer and neither is it:
//
//   - narrow (locals declared `var x uuid.UUID`) MISSES the confirmed bug --
//     the scheduler's fault was `rows.Scan(&s.id, &s.tenantID, ...)`, on struct
//     fields.
//   - wide (any `&ident` whose name matches some uuid.UUID field) FLAGS THE FIX,
//     because dueSchedule still has fields named id/tenantID typed uuid.UUID
//     beside the new plugin.GUID locals.
//
// A scan that reports the repair as the defect is the shape #1134's LIMIT rule
// had: it flagged `UPDATE ... WHERE id IN (SELECT ... LIMIT n)` -- the correct
// PostgreSQL fix -- as the fault, on its first run, against the fix in the same
// commit.
//
// Two more lexical readings were tried while claiming this issue and produced
// 13, and 54-across-14. Four scans, four answers. They are not answering the
// same question badly; they are answering different questions, and none of them
// is "what type does this identifier resolve to". Only the type checker is.
func TestNoPluginScansIntoUUIDDirectly(t *testing.T) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports,
		Dir: "..",
	}
	pkgs, err := packages.Load(cfg, "./plugins/...")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// A partial load reports fewer sites, which reads exactly like a clean tree.
	var loadErrs int
	packages.Visit(pkgs, nil, func(p *packages.Package) { loadErrs += len(p.Errors) })
	if loadErrs > 0 {
		t.Fatalf("%d package load errors; the type information is incomplete and a "+
			"pass here would mean nothing", loadErrs)
	}
	if len(pkgs) < 15 {
		t.Fatalf("only %d plugin packages loaded; the pattern is wrong and this guard "+
			"asserts nothing", len(pkgs))
	}

	var scans int
	found := map[string][]string{}
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			fname := p.Fset.Position(f.Pos()).Filename
			if strings.HasSuffix(fname, "_test.go") {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Scan" {
					return true
				}
				scans++
				for _, a := range call.Args {
					if !isUUIDPointer(p.TypesInfo.TypeOf(a)) {
						continue
					}
					pos := p.Fset.Position(a.Pos())
					found[p.Name] = append(found[p.Name],
						shortPath(pos.Filename)+":"+itoa(pos.Line))
				}
				return true
			})
		}
	}
	// Vacuity: a walk that examined no Scan calls cannot have found anything.
	if scans < 30 {
		t.Fatalf("only %d Scan() calls examined across the plugin tree; the walk is "+
			"broken", scans)
	}

	if len(found) == 0 {
		t.Logf("examined %d Scan() calls across %d packages; no argument resolves to "+
			"*uuid.UUID", scans, len(pkgs))
		return
	}
	names := make([]string, 0, len(found))
	total := 0
	for k, v := range found {
		names = append(names, k)
		total += len(v)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString("\n  " + n + ":")
		for _, loc := range found[n] {
			b.WriteString("\n      " + loc)
		}
	}
	t.Errorf("%d Scan arguments in %d plugins resolve to *uuid.UUID.%s\n\n"+
		"SQL Server returns UNIQUEIDENTIFIER in mixed-endian byte order. "+
		"uuid.UUID's Scan takes those 16 bytes without error and produces a "+
		"DIFFERENT uuid -- nothing fails, and the value is wrong. Scan into "+
		"plugin.GUID and assign its .UUID afterwards, as plugins/scheduler does.",
		total, len(found), b.String())
}

func isUUIDPointer(t types.Type) bool {
	ptr, ok := t.(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types.Named)
	if !ok {
		return false
	}
	o := named.Obj()
	return o.Pkg() != nil && o.Pkg().Path() == "github.com/google/uuid" && o.Name() == "UUID"
}

func shortPath(p string) string {
	if i := strings.Index(p, "/plugins/"); i >= 0 {
		return "plugins" + p[i+len("/plugins"):]
	}
	return p
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
