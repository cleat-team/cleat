package plugin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// A plugin table carrying a tenant_id column must be declared TenantScoped,
// because that declaration is the ONLY thing that gives it a row-level security
// policy: plugin/migration.go's applyTenantScoping reads Migration.TenantScoped
// and nothing else. A table with the column and without the declaration is
// tenant-owned data with no policy on any dialect, and nothing says so.
//
// # Why this is a guard and not a fix
//
// Measured on develop the day this was written: 19 plugin directories, 26
// tables carrying a tenant_id column, 26 declarations, and a per-plugin match
// on every one. Nothing is wrong today. What is missing is anything that KEEPS
// it that way -- a new plugin adding a tenant table and forgetting the
// declaration gets no policy, no error, and no failing test, because the
// declaration is an opt-in and an omitted opt-in looks exactly like a table
// that does not need one.
//
// migration 056 states the shape, about a different guard:
//
//	so it answers "is every statement against a KNOWN tenant-scoped table
//	scoped?" and cannot answer "is every table that should be tenant-scoped
//	actually one?"
//
// Every existing check here is on the first side of that: cleat#1552's tests
// prove a DECLARED table gets its policy, and cleat#1640's guard proves a
// statement names a context. This is the second side.
//
// # Why the file set is not pluginMigrationFiles
//
// TestPluginDialectArmsDeclareTheSameColumns scans `plugins/*/migrations.go`.
// plugins/pgvector keeps its Migration literal in plugin.go, so it is outside
// that set entirely -- and it is the one plugin whose table would be easiest to
// forget, being the only one that is deliberately PostgreSQL-only.
//
// That costs the older guard nothing today, checked rather than assumed:
// pgvector declares only an `Up` arm, and that guard compares a table only when
// two or more arms declare it, so there is nothing there for it to miss. It
// would cost THIS guard its most likely finding, so the set here is every
// non-test .go file under plugins/, and TestTheTenantScopedScanReachesEveryPlugin
// below asserts the set actually reaches each declaring directory rather than
// leaving that to the glob.
func TestEveryPluginTableWithATenantColumnIsDeclaredTenantScoped(t *testing.T) {
	byPlugin := tenantScopedSurvey(t, pluginSourceFiles(t))

	var undeclared []string
	tables := 0
	for _, p := range sortedKeys(byPlugin) {
		s := byPlugin[p]
		for _, table := range sortedStrings(s.tenantTables) {
			tables++
			if !s.declared[table] {
				undeclared = append(undeclared, p+"."+table)
			}
		}
	}

	// A scan that matched nothing would report no undeclared tables, which is
	// the same output as a clean tree. There were 26 on 2026-09-16; the floor is
	// a sanity bound on the EXTRACTION, deliberately well below that, not a
	// census to keep in step.
	if tables < 10 {
		t.Fatalf("the scan found only %d plugin tables carrying a tenant_id column, and "+
			"there were 26 when this was written. A parse that matches almost nothing "+
			"passes vacuously, so this is a failure: fix the extraction rather than "+
			"lowering this bound", tables)
	}

	if len(undeclared) > 0 {
		t.Errorf("%v carry a tenant_id column and are not declared TenantScoped.\n\n"+
			"TenantScoped is what applyTenantScoping reads to install a row-level "+
			"security policy, so an undeclared table holding one tenant's rows has no "+
			"policy on any dialect -- on PostgreSQL nothing constrains a query that "+
			"forgets its predicate, and on SQL Server nothing constrains it either "+
			"(cleat#1552 installs plugin policies from this declaration alone).\n\n"+
			"If the column is genuinely not a tenant reference -- a foreign key to "+
			"another tenant's row, say -- that is worth a comment on the table rather "+
			"than silence, and worth saying here.", undeclared)
	}
	t.Logf("%d plugin tables carry a tenant_id column across %d plugins, and every one is "+
		"declared TenantScoped", tables, len(byPlugin))
}

// The scan's own scope, asserted rather than assumed. A glob that stops
// reaching a directory removes findings silently, and the finding it would
// remove first is the one from the directory that does not follow the
// convention.
func TestTheTenantScopedScanReachesEveryPlugin(t *testing.T) {
	files := pluginSourceFiles(t)
	seen := map[string]bool{}
	for _, f := range files {
		seen[strings.Split(f, "/")[1]] = true
	}

	// Every directory that declares TenantScoped anywhere must be in the set.
	// Derived from a separate command so it is a different reading of the same
	// question, not a restatement of the glob above.
	grep := exec.Command("git", "grep", "-l", "TenantScoped:", "--", "plugins/")
	grep.Dir = ".."
	out, err := grep.Output()
	if err != nil {
		// Exit 1 means "no match", which here means the declaration has been
		// renamed or removed wholesale -- not an infrastructure failure, and
		// not something to pass over.
		t.Fatalf("git grep for TenantScoped: found nothing under plugins/ (%v). Either the "+
			"field was renamed, in which case this guard and applyTenantScoping both need "+
			"repointing, or this command is running from the wrong directory", err)
	}
	var missing []string
	declaring := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l == "" || strings.HasSuffix(l, "_test.go") {
			continue
		}
		dir := strings.Split(l, "/")[1]
		declaring[dir] = true
		if !seen[dir] {
			missing = append(missing, l)
		}
	}
	if len(declaring) == 0 {
		t.Fatal("no file under plugins/ declares TenantScoped, so this test compared nothing")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%v declare TenantScoped and are not reachable by pluginSourceFiles, so "+
			"TestEveryPluginTableWithATenantColumnIsDeclaredTenantScoped cannot see them.\n\n"+
			"This is the failure mode the file set was widened for: "+
			"plugins/*/migrations.go misses plugins/pgvector/plugin.go.", missing)
	}
	t.Logf("%d plugin directories declare TenantScoped and all are in the scan's %d files",
		len(declaring), len(files))
}

// pluginScope is one plugin's answer to both halves of the question.
type pluginScope struct {
	tenantTables map[string]bool // tables whose CREATE TABLE carries a tenant_id column
	declared     map[string]bool // names appearing in any TenantScoped on that plugin
}

// tenantScopedSurvey parses each file with go/ast and evaluates concatenation.
//
// A REGEX CANNOT DO THIS, and the neighbouring guard paid to learn it: a Go raw
// string cannot contain a backtick, so MySQL DDL for a column named `key` is
// written as `CREATE TABLE ...` + "`key`" + ` ...`, and a textual scan captures
// the first fragment and stops. See TestPluginDialectArmsDeclareTheSameColumns,
// whose evalStringExpr this reuses rather than reimplements.
//
// Every arm is scanned, not just Up. A table's tenant_id column can be present
// in one dialect's DDL and absent from another -- that is a different defect,
// and cleat#1291's guard is the one that reports it -- so treating a table as
// tenant-owned if ANY arm gives it the column is the right side to err on here:
// it can only ever ask for MORE tables to be declared.
func tenantScopedSurvey(t *testing.T, files []string) map[string]*pluginScope {
	t.Helper()
	out := map[string]*pluginScope{}
	get := func(p string) *pluginScope {
		if out[p] == nil {
			out[p] = &pluginScope{tenantTables: map[string]bool{}, declared: map[string]bool{}}
		}
		return out[p]
	}

	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, "../"+path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		p := strings.Split(path, "/")[1]
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, el := range cl.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				switch key.Name {
				case "Up", "UpMySQL", "UpMSSQL":
					sql, ok := evalStringExpr(kv.Value)
					if !ok {
						continue
					}
					for table, cols := range createTableColumns(sql, key.Name) {
						for _, c := range cols {
							if c == "tenant_id" {
								get(p).tenantTables[table] = true
							}
						}
					}
				case "TenantScoped":
					for _, name := range stringSliceLiteral(kv.Value) {
						get(p).declared[normaliseIdent(name)] = true
					}
				}
			}
			return true
		})
	}
	return out
}

// stringSliceLiteral reads the elements of a []string{...} composite literal.
func stringSliceLiteral(e ast.Expr) []string {
	cl, ok := e.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	var out []string
	for _, el := range cl.Elts {
		if s, ok := evalStringExpr(el); ok {
			out = append(out, s)
		}
	}
	return out
}

// pluginSourceFiles is every non-test .go file under plugins/.
//
// git ls-files rather than a filesystem walk, so a scratch checkout under
// .claude/worktrees -- a whole second copy of the repo -- cannot contribute a
// file and make this report about a tree nobody is editing.
func pluginSourceFiles(t *testing.T) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "plugins/")
	cmd.Dir = ".."
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasSuffix(l, ".go") && !strings.HasSuffix(l, "_test.go") {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		t.Fatal("git ls-files returned no plugin source, so every scan below would be empty")
	}
	return out
}

func sortedKeys(m map[string]*pluginScope) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStrings(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The known-positive. Every plugin in the tree is correct today, so a guard
// run only against the tree is green whether or not it works -- this points the
// same scanner at a case that is already known to be broken and requires it to
// say so.
//
// Three cases, and the third is the one a regex-based scan fails: a tenant
// table whose DDL is split by a backtick-quoted identifier, which is how a
// MySQL arm is written when a column name is reserved.
func TestTheTenantScopedScannerReportsAnUndeclaredTable(t *testing.T) {
	survey := tenantScopedSurvey(t, []string{"plugin/testdata/tenantscoped/undeclared.go"})
	if len(survey) != 1 {
		t.Fatalf("the fixture produced %d plugin scopes, want 1 -- the scanner did not read "+
			"it, so nothing below means anything", len(survey))
	}
	var s *pluginScope
	for _, v := range survey {
		s = v
	}

	for _, tc := range []struct {
		table      string
		wantScoped bool
		why        string
	}{
		{"fixture_undeclared_rows", false,
			"a tenant_id column with no TenantScoped -- the defect this guard exists for"},
		{"fixture_declared_rows", true,
			"the same table declared; reporting it would make the guard fire on correct code"},
		{"fixture_concat_rows", false,
			"tenant_id in DDL split by a backtick-quoted identifier -- a textual scan " +
				"stops at the first fragment and never sees the column"},
	} {
		if !s.tenantTables[tc.table] {
			t.Errorf("the scanner did not see a tenant_id column on %s (%s). It cannot "+
				"report a table it cannot see", tc.table, tc.why)
			continue
		}
		if got := s.declared[tc.table]; got != tc.wantScoped {
			t.Errorf("%s: declared = %v, want %v (%s)", tc.table, got, tc.wantScoped, tc.why)
		}
	}

	// And the verdict the guard actually acts on, rather than its inputs.
	var undeclared []string
	for _, table := range sortedStrings(s.tenantTables) {
		if !s.declared[table] {
			undeclared = append(undeclared, table)
		}
	}
	want := []string{"fixture_concat_rows", "fixture_undeclared_rows"}
	if strings.Join(undeclared, ",") != strings.Join(want, ",") {
		t.Errorf("the scanner reports %v as undeclared, want %v", undeclared, want)
	}
}
