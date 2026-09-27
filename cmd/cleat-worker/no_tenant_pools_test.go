package main

// cleat#1307. The worker builds no per-tenant connection pools, on any dialect,
// and the obvious repair to the bug that revealed this would be worse than the
// bug.
//
// What was there: a `tenantPools = plugin.NewTenantPools(...)` guarded by
// `*driver != "postgres"` -- INSIDE the `case "postgres":` arm. Unreachable, so
// tenantPools was nil everywhere, so cmd/cleat-worker/setup.go's
// `if w.tenantPools != nil` has never once been true.
//
// THE TEMPTING FIX IS THE DANGEROUS ONE. Moving the block out of the switch so
// MySQL and SQL Server reach it reads as obviously right -- the comment above
// it said pools "are only created for those drivers". But plugin.TenantPools is
// PostgreSQL-only in its implementation:
//
//	plugin/tenant_db.go   sql.Open("postgres", tenantDSN)
//	                      fmt.Sprintf("%s user=%s password=%s", ...)   libpq keyword DSN
//	                      SELECT ... WHERE tenant_id = $1              libpq placeholder
//
// A MySQL worker would be handed a postgres connection builder. And MySQL is
// single-tenant BY DECISION -- tiers.yaml, "DECIDED 2026-09-03", enforced by
// migrations/mysql/038_single_tenant_guard.sql -- so making it reachable there
// also contradicts the manifest CLAUDE.md names as the source of truth.
//
// This test exists so that repair cannot land quietly. It is a source-level
// assertion on purpose: the failure it guards is a line of wiring, it needs no
// database, and so it runs in every job rather than only where a DSN is set.

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func workerRepoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestTenantPoolsAreBuiltOnlyUnderRoleIsolation(t *testing.T) {
	root := workerRepoRoot(t)

	out, err := exec.Command("git", "-C", root, "ls-files", "cmd/cleat-worker/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	// A scan with nothing to scan reports a clean tree, which is the
	// "checks never started" reading. The floor belongs on the success path.
	if len(files) < 5 {
		t.Fatalf("git ls-files matched %d files under cmd/cleat-worker; the scan did "+
			"not see the package", len(files))
	}

	seen := 0
	for _, rel := range files {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "NewTenantPools" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "plugin" {
				return true
			}
			seen++
			if !gatedOnRoleIsolation(t, rel, call.Pos(), fset, file) {
				t.Errorf("%s:%d constructs plugin.NewTenantPools OUTSIDE a "+
					"`tenantMode == isolationRole` gate.\n\n"+
					"plugin.TenantPools is PostgreSQL-only -- sql.Open(\"postgres\", ...) "+
					"against a libpq keyword DSN -- and resolveTenantIsolation refuses "+
					"--tenant-isolation=role on any other driver. Ungating it, or moving it "+
					"out of the postgres arm, hands a MySQL worker a postgres connection "+
					"builder. MySQL is single-tenant by decision (tiers.yaml).\n\n"+
					"It must also stay off by default: --require-auth defaults TRUE, so "+
					"gating on that instead would switch a new isolation mechanism on for "+
					"every existing deployment at upgrade (cleat#1307).",
					rel, fset.Position(call.Pos()).Line)
			}
			return true
		})
	}

	// The floor. cleat#1307 wired this up, so a scan finding NO construction is
	// reporting that the feature was removed -- or that this test stopped
	// matching it -- rather than that everything is fine.
	if seen == 0 {
		t.Error("no plugin.NewTenantPools construction found in cmd/cleat-worker.\n\n" +
			"cleat#1307 wired role-per-tenant isolation up; if it has been removed, this " +
			"test should go with it rather than pass silently.")
	}
}

// gatedOnRoleIsolation reports whether the construction at the given offset
// sits inside a block opened by `if tenantMode == isolationRole`.
//
// PARSED, NOT WINDOWED. The first version looked back a fixed eight lines and
// failed on the real code, where the gate is eleven lines up with a baseDSN
// check and its error block in between. Widening the window would have made
// the guard pass by luck and fail again the next time anything is inserted;
// asking the syntax tree which `if` encloses the call cannot drift that way.
//
// A source-level check because that is what the invariant is about: the gate,
// not the value. A runtime test cannot see someone moving the construction out
// of the postgres arm, which is the specific mistake #1350 records -- the
// previous version of this line was unreachable for exactly that reason, and
// the tempting repair was to relocate it.
func gatedOnRoleIsolation(t *testing.T, path string, callPos token.Pos, fset *token.FileSet, file *ast.File) bool {
	t.Helper()
	gated := false
	ast.Inspect(file, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Body == nil {
			return true
		}
		var cond strings.Builder
		if err := printer.Fprint(&cond, fset, ifs.Cond); err != nil {
			return true
		}
		if !strings.Contains(cond.String(), "tenantMode == isolationRole") {
			return true
		}
		if callPos > ifs.Body.Lbrace && callPos < ifs.Body.Rbrace {
			gated = true
		}
		return true
	})
	return gated
}
