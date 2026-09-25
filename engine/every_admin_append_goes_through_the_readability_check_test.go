package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// cleat#2311. An admin operation that appends an event to a history this worker
// cannot decrypt seals that event under the WRONG key and leaves the run
// unreadable to every single-key worker. The guard is one function,
// PostgresStore.assertHistoryReadable, called from PostgresStore.adminAppendAudit.
// This test keeps it structural, so a new admin verb cannot append around it:
//
//   - in the admin store files, appendEventsInTx may be called only from
//     adminAppendAudit (all three dialects' copies of it);
//   - PostgresStore.adminAppendAudit must call assertHistoryReadable.
//
// It reads the files with the Go parser. MySQL and SQL Server encrypt nothing
// (payload encryption is PostgreSQL-only), so their copies need no check.

type appendSite struct {
	inFunc string
	recv   string
	pos    token.Position
}

func scanAdminAppends(t *testing.T, filename string, src any) (appends []appendSite, auditCallsReadable bool) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		recv := ""
		if fn.Recv != nil && len(fn.Recv.List) == 1 {
			if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
				if id, ok := star.X.(*ast.Ident); ok {
					recv = id.Name
				}
			}
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "appendEventsInTx":
				appends = append(appends, appendSite{fn.Name.Name, recv, fset.Position(call.Pos())})
			case "assertHistoryReadable":
				if fn.Name.Name == "adminAppendAudit" && recv == "PostgresStore" {
					auditCallsReadable = true
				}
			}
			return true
		})
	}
	return appends, auditCallsReadable
}

func TestEveryAdminAppendGoesThroughTheReadabilityCheck(t *testing.T) {
	// Known-positive: a verb that appends on its own is flagged, and the
	// compliant shape is not.
	bad, _ := scanAdminAppends(t, "positive.go", `package engine
type PostgresStore struct{}
func (s *PostgresStore) adminForceSomething() { s.appendEventsInTx() }`)
	if len(bad) != 1 || bad[0].inFunc == "adminAppendAudit" {
		t.Fatalf("the scan does not see an admin verb appending on its own (%+v)", bad)
	}
	ok, readable := scanAdminAppends(t, "negative.go", `package engine
type PostgresStore struct{}
func (s *PostgresStore) adminAppendAudit() { s.assertHistoryReadable(); s.appendEventsInTx() }`)
	if len(ok) != 1 || ok[0].inFunc != "adminAppendAudit" || !readable {
		t.Fatalf("the scan misreads the compliant shape: %+v readable=%v", ok, readable)
	}

	files, err := filepath.Glob("store_admin*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no admin store files found (%v)", err)
	}
	sawAudit, readableOnPostgres := 0, false
	for _, file := range files {
		if filepath.Ext(file) != ".go" || len(file) > 8 && file[len(file)-8:] == "_test.go" {
			continue
		}
		sites, readable := scanAdminAppends(t, file, nil)
		readableOnPostgres = readableOnPostgres || readable
		for _, s := range sites {
			if s.inFunc != "adminAppendAudit" {
				t.Errorf("%s: %s.%s appends an event itself; admin verbs must append through adminAppendAudit, "+
					"which checks the history is readable first (cleat#2311)", s.pos, s.recv, s.inFunc)
				continue
			}
			sawAudit++
		}
	}
	if sawAudit == 0 {
		t.Error("no appendEventsInTx call found in any adminAppendAudit -- the scan is not reading what it should")
	}
	if !readableOnPostgres {
		t.Error("PostgresStore.adminAppendAudit does not call assertHistoryReadable")
	}
}
