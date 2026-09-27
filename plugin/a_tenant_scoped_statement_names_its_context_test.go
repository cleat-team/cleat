package plugin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A statement against a tenant-scoped plugin table must be issued on a context
// that can carry a tenant. cleat#1552.
//
// WHAT GOES WRONG WITHOUT THIS, and it is not symmetric between the dialects.
// plugin.PluginDB's Exec/Query/QueryRow take the context as their first
// argument, and that context is the ONLY carrier of the tenant --
// engine.beginTenantTx reads it and nothing else. A statement handed
// context.Background() therefore runs unscoped, however carefully it spells
// `WHERE tenant_id = $1` in its own text: a policy does not read the literal.
//
//	PostgreSQL   cleat.assert_tenant_set() RAISES. Loud, immediate, fixed.
//	SQL Server   a filter predicate CANNOT raise -- it must be an inline
//	             table-valued function, which has no procedural body -- so the
//	             statement matches nothing and reports success.
//
// The SQL Server half is why this guard exists. cleat#1629 shipped the policies
// and two fixtures went wrong exactly this way: a DELETE removed no rows,
// reported success, and the next scenario counted one row too many and blamed
// its own SELECT. Writing this guard found two MORE of the same shape that had
// not surfaced yet -- featureflags' cleanup, and a pagerdutyalert seed whose
// error was discarded entirely, so a test asserting "missing API key" would
// have passed against a config that was never created.
//
// WHY IT KEYS ON THE METHOD NAME. plugin.PluginDB spells them Exec/Query/
// QueryRow; database/sql spells them ExecContext/QueryContext/QueryRowContext.
// That difference is exactly the distinction the guard needs, and it is
// decidable without type information: on the adapter the context is the only
// carrier, while a raw *sql.DB or a *sql.Conn may already be pinned to a tenant
// by other means. An earlier draft flagged both and reported six false
// positives in plugins/scheduledbackup/commands.go, where cliBackupRun holds a
// connection from connScopedToTenant and the context genuinely does not matter.
//
// WHAT IT CANNOT SEE, stated rather than left to be discovered: a context
// variable that was built from context.Background() a few lines earlier, a
// statement assembled at runtime rather than written as a literal, and any call
// through a raw database handle. It catches one shape exactly, and that shape
// is the one that has gone wrong four times.

// bareContextSite is one Exec/Query/QueryRow on context.Background() whose
// statement names a tenant-scoped table.
type bareContextSite struct {
	File  string
	Func  string
	Line  int
	Table string
	Stmt  string
}

func (s bareContextSite) key() string { return s.File + ":" + s.Func }

// bareContextAllowed lists the sites where a bare context is the POINT.
//
// Every entry today is a test whose subject is the refusal itself -- "a
// statement with no tenant set is refused", "the table's owner wrote a row with
// no tenant in context". Removing the bare context would delete the test.
//
// THE COUNT IS PART OF THE ENTRY, like scripts/skip-ledger.tsv's, because two
// of these functions hold two sites each and a key alone would let a third
// arrive unnoticed under a reason written for the other two.
//
// NO PRODUCTION CODE IS LISTED, AND NONE MAY BE. That is asserted separately in
// TestNoProductionStatementUsesABareContext rather than left to whoever edits
// this map: a bare context in production has no test to be the point of.
var bareContextAllowed = map[string]struct {
	count int
	why   string
}{
	"plugins/blobstore/blob_index_rows_are_scoped_by_a_policy_test.go:TestBlobIndexRowsAreScopedByAPolicyNotOnlyByTheQuery": {1,
		"asserts blob_index's policy refuses an unscoped write; the bare context is the subject"},
	"plugins/eventstore/event_stream_rows_are_scoped_by_a_policy_test.go:TestEventStreamRowsAreScopedByAPolicyAndTheSweepSaysSo": {1,
		"asserts event_stream's policy refuses an unscoped write, and that the sweep says so"},
	"plugins/featureflags/feature_flags_rows_are_scoped_by_a_policy_test.go:TestFeatureFlagsRowsAreScopedByAPolicyNotOnlyByTheQuery": {1,
		"asserts feature_flags is scoped by the policy and not only by the query's own WHERE"},
	"plugins/kvstore/kv_store_has_a_policy_behind_its_predicate_test.go:TestKVStoreRowsAreScopedByAPolicyNotOnlyByTheQuery": {2,
		"two subtests, two refusals: one for an ordinary role, one proving FORCE binds the table's OWNER too"},
	"plugins/kvstore/read_only_plugins_are_tenant_scoped_too_test.go:TestAReadOnlyPluginCanReadATenantScopedTable": {1,
		"cleat#1285's fix must not scope a statement that has no tenant; this is the fail-closed half"},
	"plugins/notifications/webhook_config_rows_are_scoped_by_a_policy_test.go:TestWebhookConfigRowsAreScopedByAPolicyAndTheLoopSaysSo": {2,
		"the seed, and deliver()'s config lookup verbatim -- the one statement in the loop that needs the bypass"},
}

// pluginDBMethods are plugin.PluginDB's, which take the context first and have
// no other way to learn the tenant. database/sql's *Context spellings are
// deliberately absent; see the note at the top of this file.
var pluginDBMethods = map[string]bool{"Exec": true, "Query": true, "QueryRow": true}

func isBareContext(e ast.Expr) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	s, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := s.X.(*ast.Ident)
	return ok && pkg.Name == "context" && (s.Sel.Name == "Background" || s.Sel.Name == "TODO")
}

// tenantScopedTableNames reads the declarations themselves rather than a list
// kept here, so a plugin that scopes a new table is covered the day it lands.
func tenantScopedTableNames(t *testing.T, root string, files []string) map[string]bool {
	t.Helper()
	decl := regexp.MustCompile(`TenantScoped:\s*\[\]string\{([^}]*)\}`)
	name := regexp.MustCompile(`"([^"]+)"`)
	out := map[string]bool{}
	for _, rel := range files {
		if !strings.HasPrefix(rel, "plugins/") || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		for _, m := range decl.FindAllStringSubmatch(string(b), -1) {
			for _, n := range name.FindAllStringSubmatch(m[1], -1) {
				out[n[1]] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no TenantScoped declarations found at all, so this guard is asking " +
			"about an empty set of tables and would report any tree as clean")
	}
	return out
}

func scanBareContextSites(t *testing.T, root string, files []string, scoped map[string]bool) []bareContextSite {
	t.Helper()
	fset := token.NewFileSet()
	var sites []bareContextSite

	names := make([]*regexp.Regexp, 0, len(scoped))
	order := make([]string, 0, len(scoped))
	for tbl := range scoped {
		order = append(order, tbl)
	}
	sort.Strings(order)
	for _, tbl := range order {
		names = append(names, regexp.MustCompile(`\b`+regexp.QuoteMeta(tbl)+`\b`))
	}

	for _, rel := range files {
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !pluginDBMethods[sel.Sel.Name] || !isBareContext(call.Args[0]) {
					return true
				}
				var lits []string
				for _, a := range call.Args[1:] {
					ast.Inspect(a, func(m ast.Node) bool {
						if lit, ok := m.(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if s, err := strconv.Unquote(lit.Value); err == nil {
								lits = append(lits, s)
							}
						}
						return true
					})
				}
				for _, s := range lits {
					for i, re := range names {
						if re.MatchString(s) {
							sites = append(sites, bareContextSite{
								File: rel, Func: fn.Name.Name, Line: fset.Position(call.Pos()).Line,
								Table: order[i], Stmt: strings.Join(strings.Fields(s), " "),
							})
							return true
						}
					}
				}
				return true
			})
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})
	return sites
}

// treeGoFiles lists the repository's Go sources, tests included.
//
// git ls-files rather than a walk: a walk descends into .claude/worktrees/, an
// entire second copy of the repository, and would attribute a call site to a
// scratch checkout. --others --exclude-standard so a site added in an untracked
// file is still seen, since that is the change being reviewed.
func treeGoFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files",
		"--cached", "--others", "--exclude-standard", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, rel := range strings.Fields(string(out)) {
		if strings.Contains(rel, "/testdata/") || strings.HasPrefix(rel, "testdata/") {
			continue
		}
		files = append(files, rel)
	}
	return files
}

// TestEveryTenantScopedStatementNamesItsContext is the ratchet.
func TestEveryTenantScopedStatementNamesItsContext(t *testing.T) {
	root := repoRoot(t)
	files := treeGoFiles(t, root)
	sites := scanBareContextSites(t, root, files, tenantScopedTableNames(t, root, files))

	found := map[string][]bareContextSite{}
	for _, s := range sites {
		found[s.key()] = append(found[s.key()], s)
	}

	for key, group := range found {
		allowed, ok := bareContextAllowed[key]
		if !ok {
			s := group[0]
			t.Errorf("%s:%d (%s) issues a statement against %s on a bare context:\n"+
				"  %s\n"+
				"  The context is the only carrier of the tenant -- a policy does not read the\n"+
				"  tenant_id in the statement's own text. PostgreSQL RAISES here; SQL Server\n"+
				"  matches nothing and reports success, which is the failure that is hard to see.\n"+
				"  Use plugin.ForTenant(ctx, tenant), or plugin.AcrossAllTenants(ctx, reason) if\n"+
				"  it genuinely spans tenants. If the refusal IS the point, add it to\n"+
				"  bareContextAllowed with a reason.",
				s.File, s.Line, s.Func, s.Table, s.Stmt)
			continue
		}
		if len(group) != allowed.count {
			t.Errorf("%s has %d bare-context statement(s), the allowlist declares %d.\n"+
				"  reason on file: %s\n"+
				"  More than declared means something new is relying on a reason written for\n"+
				"  something else; fewer means the entry is stale and now covers nothing.",
				key, len(group), allowed.count, allowed.why)
		}
	}
	for key, allowed := range bareContextAllowed {
		if _, ok := found[key]; !ok {
			t.Errorf("stale allowlist entry %q: no bare-context statement there.\n"+
				"  reason on file: %s\n"+
				"  An entry that matches nothing is a grant covering something that is not\n"+
				"  there -- it will silently cover the next thing to take that name.",
				key, allowed.why)
		}
	}
}

// TestNoProductionStatementUsesABareContext is the half that may not be
// negotiated away in the allowlist.
//
// Every allowlisted site is a test whose subject is the refusal. Production
// code has no such excuse: a bare context there is a statement that raises on
// PostgreSQL and quietly does nothing on SQL Server. This assertion exists
// separately so that adding a production entry to bareContextAllowed does not
// silence it.
func TestNoProductionStatementUsesABareContext(t *testing.T) {
	root := repoRoot(t)
	files := treeGoFiles(t, root)
	var prod []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			prod = append(prod, f)
		}
	}
	for _, s := range scanBareContextSites(t, root, prod, tenantScopedTableNames(t, root, files)) {
		t.Errorf("PRODUCTION code at %s:%d (%s) reaches %s on a bare context:\n  %s\n"+
			"  This raises on PostgreSQL and silently matches nothing on SQL Server.",
			s.File, s.Line, s.Func, s.Table, s.Stmt)
	}
}

// TestTheBareContextScannerReportsAnUndeclaredSite is the known-positive.
//
// The ratchet answers "does it pass when the tree is fine?", which every broken
// version of it also answers yes to. This answers the harder question in both
// directions: the fixture holds one statement that IS the defect and one that
// is the correct form, and a scanner that reported neither -- or both -- would
// be useless in a way a clean tree cannot reveal.
func TestTheBareContextScannerReportsAnUndeclaredSite(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join("plugin", "testdata", "barecontext", "unscoped.go")

	sites := scanBareContextSites(t, root, []string{fixture},
		tenantScopedTableNames(t, root, treeGoFiles(t, root)))

	if len(sites) != 1 {
		t.Fatalf("the scanner found %d site(s) in the fixture, want exactly 1 -- it either "+
			"cannot see the defect any more, or it is now reporting the correctly scoped "+
			"statement beside it", len(sites))
	}
	if sites[0].Func != "theDefect" {
		t.Fatalf("the scanner reported %q; the fixture's defect is in theDefect and its "+
			"correct form is in theCorrectForm", sites[0].Func)
	}
	if _, declared := bareContextAllowed[sites[0].key()]; declared {
		t.Fatal("the fixture site is in the allowlist; it must stay out of it, or this test " +
			"proves nothing")
	}
}
