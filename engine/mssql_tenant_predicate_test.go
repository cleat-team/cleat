package engine

// The gate IMPROVEMENT-PLAN 3.86 asks for: a tenant-scoped table may not be
// read or written by SQL Server SQL that does not say which tenant is asking.
//
// Why a gate and not a fifth audit. Five passes over this surface found five,
// five, twelve, six and two leaking statements -- each pass reading one file
// while scoping something else, and the last two (3.91) found by an early draft
// of this guard rather than by a person. That is not a method, and the sixth
// file would not be one either. This fails at authoring time instead, in every
// job, with no SQL Server required.
//
// Both allowlisted reasons that name a Go-level gate were checked against the
// code rather than assumed: claimWorkflowsAcrossTenantsOnce and
// GetDueSchedulesAcrossTenants both call requireCleatAdminMembership. An
// allowlist whose reasons are not true is worse than no allowlist, because it
// reads like it was checked.
//
// WHY IT IS NOT A SUBSTRING CHECK, which is the lesson that produced it.
// scripts/mssql-tenant-predicate-audit.py -- this guard's ancestor, deleted in
// the same change that added this file -- asked whether `tenant_id` appeared
// anywhere in the statement. DeliverSignal's MERGE satisfied that test while
// leaking: it named tenant_id in its INSERT column
// list, which scopes the row the call CREATES and says nothing about the row it
// MATCHES, and a MERGE is an UPDATE when matched. So a caller holding another
// tenant's workflow id overwrote that workflow's pending signal payload and the
// script counted the statement as already predicated. The check below asks
// WHERE the column appears: in a WHERE, an ON or a HAVING, or -- for an INSERT,
// which has no rows to leak -- in the column list it writes.
//
// THE ALLOWLIST SAYS WHY, AND THE REASONS ARE NOT INTERCHANGEABLE. Three
// distinct claims are in play and collapsing them is how this surface got into
// the state it was in:
//
//   - scopedByCaller: the id came from a row the engine had already read under
//     a predicate, and cmd/cleat-worker/setup.go:storeFor re-scopes the store
//     to each instance's own tenant before these run. Safe BY CONSTRUCTION.
//     This is NOT the same as "a UUID cannot be guessed", which is a claim
//     about what an attacker knows and was false for every statement whose id
//     arrives from an HTTP request -- those are fixed, not allowlisted.
//   - mustNotScope: adding the predicate would BREAK the statement. One entry,
//     and it needs to keep being one.
//   - deliberatelyCrossTenant: the statement's whole purpose is to see every
//     tenant, and it is gated on cleat_admin membership at the Go level.
//
// WHAT THIS DOES NOT CHECK, said out loud so nobody reads a pass as more than
// it is. An INSERT is exempted once it writes tenant_id, but an INSERT can
// still READ another tenant's row in a subquery -- StartChildWorkflow's
// `ISNULL((SELECT task_queue FROM workflow_instances WHERE id = @p4), ...)`
// does exactly that, and this guard is blind to it. It also cannot see SQL
// built by concatenation, and it reasons about text rather than about what the
// server does with it.
//
// A stale entry fails the test too. An allowlisted function that no longer has
// an unscoped statement means somebody fixed it, and the entry must go rather
// than sit there granting permission nobody is using.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	scopedByCaller = "scoped by construction: the id comes from a row already read under a " +
		"predicate, and the store is re-scoped per instance by cmd/cleat-worker/setup.go:storeFor"
	mustNotScope            = "MUST NOT be scoped: see the comment at the site"
	deliberatelyCrossTenant = "deliberately cross-tenant, gated on cleat_admin membership in Go"
	// Not a grant. An entry carrying this is a statement known to leak, kept
	// here only so the ratchet holds while it is fixed, and it must name where
	// it is tracked.
	openFinding = "OPEN FINDING, not a grant: IMPROVEMENT-PLAN 3.92"
)

// tenantPredicateAllowlist is keyed by "<file base>:<enclosing Go function>".
var tenantPredicateAllowlist = map[string]string{
	"mssql_deployment.go:TraceWorkflow":                  scopedByCaller,
	"mssql_events.go:VerifyWorkflowEvents":               scopedByCaller,
	"mssql_events.go:appendEventsInTxOpts":               scopedByCaller,
	"mssql_lifecycle.go:BatchHeartbeat":                  mustNotScope,
	"mssql_lifecycle.go:CheckCancellation":               scopedByCaller,
	"mssql_lifecycle.go:completeWorkflowOnce":            scopedByCaller,
	"mssql_lifecycle.go:continueAsNewOnce":               scopedByCaller,
	"mssql_lifecycle.go:failWorkflowOnce":                scopedByCaller,
	"mssql_lifecycle.go:heartbeatOnce":                   scopedByCaller,
	"mssql_lifecycle.go:moveToDeadLetterQueueOnce":       scopedByCaller,
	"mssql_lifecycle.go:releaseWorkflowOnce":             scopedByCaller,
	"mssql_lifecycle.go:claimWorkflowsAcrossTenantsOnce": deliberatelyCrossTenant,
	"mssql_schedules.go:GetDueSchedulesAcrossTenants":    deliberatelyCrossTenant,
	"mssql_operations.go:clearStickyWorkerOnce":          scopedByCaller,
	"mssql_operations.go:getEventCountOnce":              scopedByCaller,
	"mssql_operations.go:updateStickyWorkerOnce":         scopedByCaller,
	"mssql_schedules.go:GetCompactionCandidates":         scopedByCaller,
	"mssql_schedules.go:LoadCompactionState":             scopedByCaller,
	"mssql_schedules.go:compactHistoryOnce":              scopedByCaller,
	"mssql_schedules.go:deleteExpiredEventsOnce":         scopedByCaller,
	"mssql_signals_promises.go:GetChildCount":            scopedByCaller,
	"mssql_signals_promises.go:GetChildResult":           scopedByCaller,
	"mssql_signals_promises.go:PollAndClaimSignal":       scopedByCaller,
	"mssql_signals_promises.go:RejectPromise":            scopedByCaller,
	"mssql_signals_promises.go:ResolvePromise":           scopedByCaller,

	// THE THREE FINDS. Two came from writing down WHY an exemption was safe
	// rather than from the scan itself, and the third from widening where the
	// scan looks:
	//
	//  - claimWorkflowsOnce and claimStickyWorkflowsOnce appeared here when this
	//    guard was first run and are NOT in this list, because 3.91 fixed them.
	//    Four hand audits and a substring script had passed over them.
	//  - the two below were about to be written down as scopedByCaller, and that
	//    reason is FALSE. terminateWorkflowOnce calls enforceParentClosePolicy
	//    unconditionally after its commit -- so since 3.86 scoped the terminate
	//    itself, a cross-tenant terminate now matches no parent and then
	//    cascades into another tenant's CHILDREN anyway. The parent is
	//    protected and the cascade is not. 3.92.
	"mssql_lifecycle.go:enforceParentClosePolicy":  openFinding,
	"mssql_lifecycle.go:childrenClosedByTerminate": openFinding,

	//  - adminAppendAudit reads event_history by workflow_id and step with no
	//    tenant. Found only after the scan stopped being a glob over
	//    mssql_*.go: it lives in store_admin.go, which the first version of
	//    this guard never opened. 3.92.
	"store_admin.go:adminAppendAudit": openFinding,
}

func TestMSSQLTenantScopedTablesAreQueriedWithATenantPredicate(t *testing.T) {
	tables := mssqlTenantScopedTables(t)
	if len(tables) == 0 {
		t.Fatal("no tables bound to dbo.fn_tenant_filter found in migrations/mssql -- the " +
			"parse is broken and this guard would pass no matter what the store did")
	}
	// Sanity anchors. If the parse silently stops finding these, everything
	// below becomes vacuous.
	for _, want := range []string{"workflow_instances", "workflow_defs", "workflow_schedules"} {
		if !tables[want] {
			t.Fatalf("%s is not among the parsed tenant-scoped tables %v -- the migration "+
				"parse is broken", want, sortedSet(tables))
		}
	}

	used := map[string]bool{}
	var scanned int
	// The same walk TestMSSQLUUIDColumnsAreConvertedInProjections uses, and for
	// the same reason it stopped globbing mssql_*.go: SQL Server statements
	// live outside those files. A filename-shaped scan reported a clean tree
	// for engine/store_admin.go, engine/store_intent.go,
	// engine/store_admin_rereplay.go and plugin/migration.go -- four files
	// carrying @pN parameters -- and one of them was leaking (3.92). A guard
	// defined by where it looks rather than by what it looks for is a
	// confident green over the files it does not open.
	for _, path := range goFilesCarryingSQL(t) {
		scanned++
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, st := range mssqlTenantStatements(string(src), path, tables) {
			key := filepath.Base(path) + ":" + st.fn
			if _, ok := tenantPredicateAllowlist[key]; ok {
				used[key] = true
				continue
			}
			if st.isInsert {
				t.Errorf("%s:%d (%s) INSERTs into a tenant-scoped table without writing "+
					"tenant_id:\n    %s", path, st.line, st.fn, st.excerpt)
				continue
			}
			t.Errorf("%s:%d (%s) reads or writes a tenant-scoped table with no tenant_id in "+
				"any WHERE, ON or HAVING clause:\n    %s\n\n"+
				"dbo.fn_tenant_filter is OFF for any dbo.cleat_admin connection "+
				"(012_admin_role.sql), which is what a multi-tenant deployment must use, so "+
				"this predicate is the whole of the isolation. If it genuinely does not need "+
				"one, add %q to tenantPredicateAllowlist WITH THE REASON THAT IS ACTUALLY "+
				"TRUE -- see the constants at the top.",
				path, st.line, st.fn, st.excerpt, key)
		}
	}
	// A floor rather than an exact count: the set grows as the repo does. It
	// exists so a walk that silently stops matching fails loudly instead of
	// reporting a clean scan of nothing.
	if scanned < 20 {
		t.Fatalf("only %d files scanned; the walk is broken and this guard asserts nothing", scanned)
	}
	// A grant nobody uses is a grant that outlived its statement.
	for key := range tenantPredicateAllowlist {
		if !used[key] {
			t.Errorf("tenantPredicateAllowlist has an entry for %q but that function has no "+
				"unscoped statement any more -- delete the entry rather than leaving it to "+
				"cover something else later", key)
		}
	}
}

// mssqlTenantScopedTables reads the shipped migrations for the tables actually
// bound to the security policy, rather than hardcoding a list that would go
// stale the next time one is added.
func mssqlTenantScopedTables(t *testing.T) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "migrations", "mssql", "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	bind := regexp.MustCompile(`(?i)ADD FILTER PREDICATE dbo\.fn_tenant_filter\(tenant_id\)\s+ON\s+dbo\.(\w+)`)
	out := map[string]bool{}
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for _, m := range bind.FindAllStringSubmatch(string(src), -1) {
			out[strings.ToLower(m[1])] = true
		}
	}
	return out
}

type tenantStatement struct {
	fn       string
	line     int
	isInsert bool
	excerpt  string
}

// filterClauseEnd terminates a WHERE/ON/HAVING window. WHEN is in the list for
// MERGE, whose ON clause ends at WHEN MATCHED.
var filterClauseEnd = regexp.MustCompile(`(?i)\b(order\s+by|group\s+by|option\s*\(|when)\b`)

// mssqlTenantStatements returns every SQL literal in src that touches a
// tenant-scoped table without naming the tenant in a position that scopes it.
func mssqlTenantStatements(src, path string, tables map[string]bool) []tenantStatement {
	var out []tenantStatement
	for _, lit := range sqlLiteralRe.FindAllStringSubmatchIndex(src, -1) {
		// Comments first, and not as tidiness: the claim queries carry a long
		// -- comment about UUID conversion that mentions tenant_id, which would
		// otherwise satisfy this guard for a statement that has no predicate at
		// all. The UUID guard next door records the mirror-image version of
		// this same mistake.
		sql := stripSQLComments(src[lit[2]:lit[3]])
		flat := strings.ToLower(strings.Join(strings.Fields(sql), " "))
		if !regexp.MustCompile(`\b(select|insert|update|delete|merge)\b`).MatchString(flat) {
			continue
		}
		if !looksLikeMSSQL(path, sql) {
			continue
		}
		var touches bool
		for tbl := range tables {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(tbl) + `\b`).MatchString(flat) {
				touches = true
				break
			}
		}
		if !touches {
			continue
		}

		isInsert := strings.HasPrefix(flat, "insert")
		scoped := false
		if isInsert {
			// An INSERT cannot leak a row it does not read. What it can do is
			// create one with no owner, so the requirement is that it WRITES
			// the column.
			scoped = strings.Contains(flat, "tenant_id")
		} else {
			for _, w := range filterWindows(flat) {
				if strings.Contains(w, "tenant_id") {
					scoped = true
					break
				}
			}
		}
		if scoped {
			continue
		}
		out = append(out, tenantStatement{
			fn:       enclosingFunc(src, lit[2]),
			line:     strings.Count(src[:lit[2]], "\n") + 1,
			isInsert: isInsert,
			excerpt:  excerpt(flat),
		})
	}
	return out
}

// filterWindows returns the text of each WHERE, ON and HAVING clause.
func filterWindows(flat string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`\b(where|having|on)\b`).FindAllStringIndex(flat, -1) {
		tail := flat[m[1]:]
		if e := filterClauseEnd.FindStringIndex(tail); e != nil {
			tail = tail[:e[0]]
		}
		out = append(out, tail)
	}
	return out
}

var funcDeclRe = regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w+)`)

func enclosingFunc(src string, pos int) string {
	ms := funcDeclRe.FindAllStringSubmatch(src[:pos], -1)
	if len(ms) == 0 {
		return "?"
	}
	return ms[len(ms)-1][1]
}

func excerpt(flat string) string {
	if len(flat) > 120 {
		return flat[:120] + " ..."
	}
	return flat
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
