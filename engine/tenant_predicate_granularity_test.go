package engine

import (
	"strings"
	"testing"
)

// The exemption mechanism's own tests, over synthetic source rather than the
// store files, so they say something about the MECHANISM and keep saying it
// while the store changes.
//
// WHY THESE EXIST AS TESTS AND NOT AS A PARAGRAPH. cleat#1032 was a
// function-granularity entry covering two statements with a reason true of one
// of them, and it survived because nothing could express the difference.
// Measured on this tree before the fix: an unscoped
// `DELETE FROM event_history WHERE workflow_id = @p1` dropped into
// updateStickyWorkerOnce -- whose exemption is about an unrelated sticky-worker
// UPDATE -- left the guard reporting `ok`. The identical statement in a
// function with no entry failed it. Only the enclosing function differed.
//
// A note in the header would have rotted the moment someone re-keyed the map.
// These go red instead.

const granularityTables = "workflow_instances,event_history"

func granularityTableSet() map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.Split(granularityTables, ",") {
		out[t] = true
	}
	return out
}

// twoStatementSource is one function issuing two unscoped statements -- the
// exact shape deleteExpiredEventsOnce had.
const twoStatementSource = "package engine\n" +
	"func oneFunctionTwoStatements(ctx context.Context, tx *sql.Tx, id string) {\n" +
	"\t_, _ = tx.ExecContext(ctx, `UPDATE workflow_instances SET sticky_worker_id = @p2 WHERE id = @p1`, id)\n" +
	"\t_, _ = tx.ExecContext(ctx, `DELETE FROM event_history WHERE workflow_id = @p1`, id)\n" +
	"}\n"

func granularityStatements(t *testing.T) []tenantStatement {
	t.Helper()
	sts := tenantStatementsFor(twoStatementSource, "mssql_scratch.go", granularityTableSet(), looksLikeMSSQL)
	if len(sts) != 2 {
		t.Fatalf("the fixture must present exactly two unscoped statements, got %d -- if "+
			"the scan stopped seeing them these tests assert nothing", len(sts))
	}
	if sts[0].fn != "oneFunctionTwoStatements" || sts[1].fn != sts[0].fn {
		t.Fatalf("both statements must be attributed to one function, got %q and %q",
			sts[0].fn, sts[1].fn)
	}
	return sts
}

// The known-positive. Before cleat#1032 this passed, which is the whole reason
// it is written down.
func TestAnExemptionDoesNotExtendToTheNextStatementInItsFunction(t *testing.T) {
	sts := granularityStatements(t)
	allow := map[string]stmtExemption{
		stmtKey("mssql_scratch.go", sts[0].fn, sts[0].norm): {
			SQL:    sts[0].norm,
			Reason: scopedByCaller,
		},
	}

	if _, _, ok := exemptionFor(allow, "mssql_scratch.go", sts[0]); !ok {
		t.Error("the exempted statement is not exempt; the key does not round-trip")
	}
	if key, _, ok := exemptionFor(allow, "mssql_scratch.go", sts[1]); ok {
		t.Errorf("the SECOND statement in the same function is exempt under %q, and nobody "+
			"wrote a reason for it. A function-granularity key is back: an unscoped "+
			"statement now inherits an exemption written about a different one, which is "+
			"cleat#1032 exactly.", key)
	}
}

// The mirror: the mechanism must still exempt what it was told to, or the fix
// is just a guard that fails on everything.
func TestAnExemptionStillCoversTheStatementItNames(t *testing.T) {
	sts := granularityStatements(t)
	allow := map[string]stmtExemption{}
	for _, st := range sts {
		allow[stmtKey("mssql_scratch.go", st.fn, st.norm)] = stmtExemption{SQL: st.norm, Reason: scopedByCaller}
	}
	for i, st := range sts {
		if _, _, ok := exemptionFor(allow, "mssql_scratch.go", st); !ok {
			t.Errorf("statement %d is not exempt despite having its own entry", i)
		}
	}
}

// A reason is a claim about particular SQL. Change the SQL and the claim has to
// be made again -- which is what stops an entry outliving the statement that
// earned it.
func TestEditingAnExemptedStatementInvalidatesItsExemption(t *testing.T) {
	sts := granularityStatements(t)
	allow := map[string]stmtExemption{
		stmtKey("mssql_scratch.go", sts[0].fn, sts[0].norm): {SQL: sts[0].norm, Reason: scopedByCaller},
	}
	edited := sts[0]
	edited.norm = strings.Replace(edited.norm, "sticky_worker_id = @p2", "sticky_worker_id = @p2, generation = @p3", 1)
	if edited.norm == sts[0].norm {
		t.Fatal("the fixture edit changed nothing, so this test asserts nothing")
	}
	if _, _, ok := exemptionFor(allow, "mssql_scratch.go", edited); ok {
		t.Error("an edited statement kept the exemption written for its earlier text")
	}
}

// The same statement text in two different functions is two different
// exemptions. Sharing one would be function granularity turned inside out.
func TestTheSameSQLInTwoFunctionsNeedsTwoExemptions(t *testing.T) {
	sts := granularityStatements(t)
	a := stmtKey("mssql_scratch.go", "functionA", sts[0].norm)
	b := stmtKey("mssql_scratch.go", "functionB", sts[0].norm)
	if a == b {
		t.Errorf("identical SQL in two functions produced one key %q", a)
	}
	other := sts[0]
	other.fn = "functionB"
	if _, _, ok := exemptionFor(map[string]stmtExemption{a: {SQL: sts[0].norm, Reason: scopedByCaller}},
		"mssql_scratch.go", other); ok {
		t.Error("functionB's statement is exempt under functionA's entry")
	}
}

// Both shipped allowlists must actually use the statement key. A future
// refactor that reverts either to a function key would leave every test above
// green -- they exercise stmtKey directly -- while the guards went blind again.
func TestBothAllowlistsAreKeyedByStatement(t *testing.T) {
	for name, allow := range map[string]map[string]stmtExemption{
		"tenantPredicateAllowlist":      tenantPredicateAllowlist,
		"mysqlTenantPredicateAllowlist": mysqlTenantPredicateAllowlist,
	} {
		if len(allow) == 0 {
			t.Errorf("%s is empty; this test asserts nothing about it", name)
			continue
		}
		for key, ex := range allow {
			if !strings.Contains(key, "#") {
				t.Errorf("%s key %q carries no statement digest -- it exempts a whole "+
					"function, and whatever that function grows next", name, key)
			}
			if ex.SQL == "" {
				t.Errorf("%s entry %q has an empty SQL: field, so it does not say what it "+
					"covers", name, key)
			}
			if ex.Reason == "" {
				t.Errorf("%s entry %q has no reason", name, key)
			}
		}
	}
}
