package engine

// The MySQL half of the gate cleat#1031 asks for: a tenant-scoped table may not
// be read or written by MySQL SQL that does not say which tenant is asking.
//
// WHY MYSQL NEEDS THIS MORE THAN SQL SERVER DID. The SQL Server guard next door
// exists because dbo.fn_tenant_filter is off for any dbo.cleat_admin
// connection, so the per-query predicate is the whole of the isolation there.
// On MySQL it is not "off for one role" -- there is NO row-level security at
// all. `grep -ril "row.level security\|CREATE POLICY" migrations/mysql` returns
// nothing across every migration. So on this dialect a missing predicate has
// nothing whatever behind it, on any connection, for every tenant-scoped table.
//
// The dialect with the least backstop had the least tooling, which is the
// inversion cleat#1031 records.
//
// THE DERIVATION DIFFERS, AND SAYING SO MATTERS. MSSQL derives its
// tenant-scoped tables from fn_tenant_filter bindings -- the security mechanism
// names them. MySQL has no such mechanism, so the set has to come from the
// schema itself: every table declaring a tenant_id column. That is a weaker
// definition and it is the best available one; a table that ought to carry
// tenant_id and does not is invisible to this guard, exactly as it is to the
// database.
//
// THE SCAN IS SHARED, NOT COPIED. tenantStatementsFor is the SQL Server guard's
// own scan, and everything it learned applies here unchanged: an INSERT is
// exempt once it WRITES tenant_id but a WHERE must COMPARE it to a bind
// parameter, because a column list scopes the row created and a join condition
// correlates two tables while restricting neither. Both of those were real
// leaks that a substring check passed. A second copy of that logic would drift
// the first time either dialect learned something new, which is the defect
// cleat#1032 was an instance of.
//
// WHAT IT DOES NOT CHECK, said out loud so a pass is not read as more than it
// is: the same blind spots as the SQL Server guard. An INSERT that writes
// tenant_id may still READ another tenant's row in a subquery. SQL built by
// concatenation is invisible. It reasons about text, not about what the server
// does with it.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// mysqlTenantPredicateAllowlist is keyed "<file base>:<enclosing Go function>",
// and its reasons carry the same three distinct claims the SQL Server guard
// documents -- they are not interchangeable.
//
// A fourth rule, learned from reading all six MSSQL entries (cleat#1031): a
// reason must describe why the statement is SAFE, not merely state something
// true about it. mssql_operations.go:updateStickyWorkerOnce carries
// "scopedByCaller" and is in fact safe because nothing in production calls the
// method at all -- a reason that is true of the code and irrelevant to its
// safety expires silently the day someone wires it up.
var mysqlTenantPredicateAllowlist = map[string]string{
	// The claim queries select their candidates under `AND tenant_id = ?` in
	// the SAME function, then act on those ids. The follow-up UPDATE and SELECT
	// carry `WHERE id IN (...)` with ids that cannot name another tenant's row,
	// because the only query that produced them was scoped. Safe by
	// construction, and the construction is visible in one function rather than
	// two hops away.
	"mysql_lifecycle.go:ClaimWorkflows":       mysqlScopedByCandidateQuery,
	"mysql_lifecycle.go:ClaimStickyWorkflows": mysqlScopedByCandidateQuery,

	// Verified rather than assumed, which is this allowlist's own rule: both
	// return ErrCrossTenantClaimUnsupported unless the store is the admin one,
	// so the Go-level gate named in the reason actually exists.
	"mysql_lifecycle.go:ClaimWorkflowsAcrossTenants": mysqlDeliberatelyCrossTenant,
	"mysql_ops.go:GetDueSchedulesAcrossTenants":      mysqlDeliberatelyCrossTenant,

	// `SELECT tenant_id FROM tenant_api_keys WHERE key_hash = ?` is how a
	// request LEARNS its tenant. Scoping it to a tenant would be circular:
	// there is no tenant to scope to until this returns.
	"mysql_store.go:ResolveTenantFromAPIKey": mysqlMustNotScope,
}

const (
	mysqlScopedByCandidateQuery = "scoped by construction: the ids come from a candidate query " +
		"in the same function carrying AND tenant_id = ?"
	mysqlDeliberatelyCrossTenant = "deliberately cross-tenant, and gated in Go -- returns " +
		"ErrCrossTenantClaimUnsupported unless the store is the admin one (checked, not assumed)"
	mysqlMustNotScope = "MUST NOT be scoped: it is how a request learns which tenant it is"
)

func TestMySQLTenantScopedTablesAreQueriedWithATenantPredicate(t *testing.T) {
	tables := mysqlTenantScopedTables(t)

	// Vacuity guard, ported deliberately and before any predicate checking.
	// An empty table set makes every question below answer "nothing found",
	// which is indistinguishable from a clean tree. A throwaway version of this
	// audit reported a confident zero from exactly this, and the only reason it
	// was caught was that its author printed the intermediate count.
	if len(tables) == 0 {
		t.Fatal("no tables with a tenant_id column found in migrations/mysql -- the " +
			"parse is broken and this guard would pass no matter what the store did")
	}
	for _, want := range []string{"workflow_instances", "workflow_defs", "workflow_schedules"} {
		if !tables[want] {
			t.Fatalf("%s is not among the parsed tenant-scoped tables %v -- the migration "+
				"parse is broken, and a negative result from a broken parse is not a "+
				"negative result", want, sortedSet(tables))
		}
	}

	used := map[string]bool{}
	var scanned int
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, st := range tenantStatementsFor(
			joinConcatenatedSQL(string(blankGoComments(t, src, path))), path, tables, looksLikeMySQL) {
			scanned++
			key := filepath.Base(path) + ":" + st.fn
			if _, ok := mysqlTenantPredicateAllowlist[key]; ok {
				used[key] = true
				continue
			}
			t.Errorf("%s:%d (%s) reads or writes a tenant-scoped table with no WHERE, ON "+
				"or HAVING clause comparing tenant_id TO A PARAMETER:\n\t%s\n\n"+
				"MySQL has no row-level security -- not disabled for one role, absent "+
				"entirely -- so this predicate is the whole of the isolation for this "+
				"statement on this dialect.\n\n"+
				"Note what this does NOT say. The statement may well mention tenant_id: "+
				"a join condition like `d.tenant_id = w.tenant_id` does, and correlates "+
				"two tables while restricting neither to a caller. Only a comparison "+
				"against ? carries \"the tenant asking\".\n\n"+
				"If it genuinely does not need one, add %q to "+
				"mysqlTenantPredicateAllowlist WITH A REASON THAT DESCRIBES WHY IT IS "+
				"SAFE -- not merely something true about it. See the note at the top.",
				filepath.Base(path), st.line, st.fn, st.excerpt, key)
		}
	}

	// A scan that examined nothing is not a pass. Distinct from the table
	// check above: the tables can parse correctly while the Go walk finds no
	// MySQL statements at all, which would also report a clean tree.
	if scanned == 0 && len(mysqlTenantPredicateAllowlist) == 0 {
		t.Error("the scan found no MySQL statements touching a tenant-scoped table at " +
			"all, which cannot be right for a dialect with a full store implementation " +
			"-- looksLikeMySQL or the literal scan is broken")
	}

	for key := range mysqlTenantPredicateAllowlist {
		if !used[key] {
			t.Errorf("mysqlTenantPredicateAllowlist has an entry for %q but that function "+
				"has no unscoped statement any more -- delete the entry rather than "+
				"leaving it to cover something else later", key)
		}
	}
}

// joinConcatenatedSQL splices `...` + expr + `...` into one literal so the scan
// sees the whole statement.
//
// Without it this guard reports a FALSE POSITIVE, which is worse than a miss:
// MySQLStore.QueueDepth builds
//
//	query := `SELECT ... WHERE status = 'ready' AND task_queue IN (` +
//		clause + `) AND tenant_id = ?`
//
// and the literal scan saw only the first fragment, which ends before the
// tenant clause. The statement is scoped; the guard called it unscoped. Six
// real findings alongside one confident wrong one is not a usable report --
// the wrong one is what a reader checks first, and it is what they remember.
//
// The SQL Server guard's header names this blind spot and lives with it,
// because SQL Server's equivalents happen to be single literals. MySQL's are
// not, so the port has to close it rather than inherit it.
//
// Deliberately crude: it joins the TEXT of adjacent literals and drops the
// expression between them, so `IN (` + clause + `)` reads as `IN ()`. That is
// enough for a predicate question -- which columns are compared to what -- and
// it is not enough to parse the statement, which this guard does not do
// anyway.
func joinConcatenatedSQL(src string) string {
	// `lit` + <anything with no backtick> + `lit`, repeatedly.
	re := regexp.MustCompile("`([^`]*)`\\s*\\+\\s*[^`+]*\\+\\s*`([^`]*)`")
	for i := 0; i < 8; i++ { // bounded: a few splices is all any statement here needs
		next := re.ReplaceAllString(src, "`$1 $2`")
		if next == src {
			break
		}
		src = next
	}
	return src
}

// looksLikeMySQL decides whether a statement is this dialect's.
//
// The filename carries it for the store files. The markers are for SQL that
// lives elsewhere: `?` placeholders are the giveaway, but they are also what
// SQLite and others use, so a bare `?` outside a mysql_ file is not enough on
// its own and the marker set stays narrow rather than greedy. A false positive
// here reports a Postgres statement as an unscoped MySQL one, which is a
// confident wrong finding rather than a missed one.
func looksLikeMySQL(file, sql string) bool {
	if strings.HasPrefix(filepath.Base(file), "mysql_") {
		return true
	}
	for _, re := range mysqlDialectMarkers {
		if re.MatchString(sql) {
			return true
		}
	}
	return false
}

var mysqlDialectMarkers = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bON DUPLICATE KEY UPDATE\b`),
	regexp.MustCompile(`(?i)\bNOW\(6\)`),
	regexp.MustCompile(`(?i)\bSTRAIGHT_JOIN\b`),
	regexp.MustCompile("(?i)`[a-z_]+`"), // backtick-quoted identifiers
}

// mysqlTenantScopedTables reads the shipped MySQL migrations and returns every
// table declaring a tenant_id column.
//
// Derived from the schema rather than from a security mechanism, because MySQL
// has none -- see the file header. CREATE TABLE bodies only: an ALTER that adds
// tenant_id later would be missed, so the anchor check in the test is what
// stops a silent under-read becoming a clean pass.
func mysqlTenantScopedTables(t *testing.T) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "migrations", "mysql", "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	create := regexp.MustCompile(`(?is)CREATE TABLE (?:IF NOT EXISTS )?` + "`?" + `(\w+)` + "`?" + `\s*\((.*?)\n\s*\)`)
	addCol := regexp.MustCompile("(?i)ALTER TABLE `?(\\w+)`?\\s+ADD COLUMN `?tenant_id`?")
	out := map[string]bool{}
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		body := string(src)
		for _, m := range create.FindAllStringSubmatch(body, -1) {
			if regexp.MustCompile("(?i)`?tenant_id`?\\s").MatchString(m[2]) {
				out[strings.ToLower(m[1])] = true
			}
		}
		for _, m := range addCol.FindAllStringSubmatch(body, -1) {
			out[strings.ToLower(m[1])] = true
		}
	}
	return out
}

// TestJoinConcatenatedSQLSeesTheWholeStatement is joinConcatenatedSQL's own
// known-positive and negative control, because a splice that is too eager hides
// real findings and one that is too timid reports false ones.
//
// The first case is QueueDepth's actual shape, which the guard called unscoped
// before this existed. The second is the mirror: a genuinely unscoped statement
// built the same way must still be reported, or the fix for the false positive
// would have bought silence.
func TestJoinConcatenatedSQLSeesTheWholeStatement(t *testing.T) {
	scoped := "query := `SELECT COUNT(*) FROM workflow_instances WHERE status = 'ready' " +
		"AND task_queue IN (` + clause + `) AND tenant_id = ?`"
	got := joinConcatenatedSQL(scoped)
	if !strings.Contains(got, "tenant_id = ?") || !strings.Contains(got, "SELECT COUNT(*)") {
		t.Errorf("the splice lost part of a scoped statement:\n  %s\n\n"+
			"QueueDepth is built exactly this way, and before the splice the scan saw "+
			"only the fragment before the first +, which ends before the tenant clause. "+
			"It reported a scoped statement as unscoped.", got)
	}

	// NEGATIVE CONTROL: no tenant clause anywhere, same construction. The
	// splice must not invent one, and the statement must still be visible as a
	// single scannable literal.
	unscoped := "query := `UPDATE workflow_instances SET status = 'ready' WHERE id IN (` + clause + `)`"
	got = joinConcatenatedSQL(unscoped)
	if strings.Contains(got, "tenant_id") {
		t.Errorf("the splice invented a tenant predicate: %s", got)
	}
	if !strings.Contains(got, "UPDATE workflow_instances") {
		t.Errorf("the splice lost the unscoped statement entirely, which would hide it "+
			"from the guard: %s", got)
	}
}
