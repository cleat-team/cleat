package engine_test

import (
	"sort"
	"strings"
	"testing"
)

// compareRequiredColumns, exercised against synthetic facts -- no database.
//
// cleat#1087. The comparison used to render each finding as
// "<table>.<col>: X requires a value, Y supplies one" and match a baseline of
// those strings, with Y chosen by `sort.Strings(names); base := names[0]`. So
// the baseline was keyed on whichever configured dialect sorted first: mssql
// with all three up, mysql without SQL Server. A two-dialect run reported the
// same two asymmetries in BOTH directions -- as new disagreements, and as
// baseline entries that "no longer exist".
//
// # Why this test is not database-backed
//
// The bug is in which dialects are configured, and the database-backed test can
// only be run in the configuration the machine happens to have. Synthetic facts
// let the two-dialect and three-dialect cases be asserted in the same run, on
// any machine, in CI, which is the only way the property "the answer does not
// depend on the configured set" can be checked at all.
//
// # Both directions have a known-positive
//
// The repair here is easy to overshoot. A stale check that reports nothing ever
// also fixes the false positive, and it passes a green tree and a negative
// control identically -- so "it stopped complaining" is not evidence. Every
// case below states which of the two errors it would catch:
//
//	newAsymmetryIsReported      over-reporting still works (a real find)
//	closedAsymmetryIsReported   under-reporting still works (the list shrinks)
//	twoDialect...NotStale       THE BUG: a correct entry must survive
//	unconfiguredDialect...      the narrowing, done without a blind spot

func facts(tables []string, required map[string][]string) schemaFacts {
	f := schemaFacts{tables: map[string]bool{}, required: requiredColumns{}}
	for _, t := range tables {
		f.tables[strings.ToLower(t)] = true
	}
	for table, cols := range required {
		lt := strings.ToLower(table)
		f.required[lt] = map[string]bool{}
		for _, c := range cols {
			f.required[lt][c] = true
		}
	}
	return f
}

// auditOnly is the real shape of the cleat#958 asymmetry: MySQL requires
// audit_events.id, the others generate or default it.
func auditOnly(dialects ...string) map[string]schemaFacts {
	got := map[string]schemaFacts{}
	for _, d := range dialects {
		if d == "mysql" {
			got[d] = facts([]string{"audit_events"}, map[string][]string{"audit_events": {"id"}})
			continue
		}
		got[d] = facts([]string{"audit_events"}, nil)
	}
	return got
}

var auditTables = map[string][]string{"audit-log": {"audit_events"}}

var auditBaseline = []columnAsymmetry{
	{Table: "audit_events", Column: "id", Dialect: "mysql", Plugin: "audit-log"},
}

func summarise(t *testing.T, dis, stale []columnAsymmetry, missing []string) string {
	t.Helper()
	var b []string
	for _, a := range dis {
		b = append(b, "NEW"+a.String())
	}
	for _, a := range stale {
		b = append(b, "STALE"+a.String())
	}
	b = append(b, missing...)
	sort.Strings(b)
	return strings.Join(b, "\n")
}

// THE BUG. A correct, still-true baseline entry must not be reported as
// anything when SQL Server is not configured.
//
// Before the fix this produced two errors at once: the disagreement rendered
// against postgres instead of mssql so it missed the baseline, and the mssql-
// rendered baseline entry matched nothing so it was reported stale -- under a
// message reading "Good news, and the list must shrink to match", whose invited
// repair is to delete an entry that is entirely correct.
func TestAsymmetryBaselineSurvivesATwoDialectRun(t *testing.T) {
	dis, stale, missing := compareRequiredColumns(
		auditOnly("mysql", "postgres"), auditTables, auditBaseline)

	if got := summarise(t, dis, stale, missing); got != "" {
		t.Errorf("a two-dialect run must report nothing for a baselined, still-true "+
			"asymmetry; got:\n%s", got)
	}
}

// The same facts under every configured subset must give the same answer.
// That is the property the pair-keying broke, and a single-configuration
// assertion cannot express it.
func TestAsymmetryAnswerDoesNotDependOnWhichDialectsAreConfigured(t *testing.T) {
	sets := [][]string{
		{"mysql", "postgres"},
		{"mysql", "mssql"},
		{"mysql", "mssql", "postgres"},
	}
	for _, set := range sets {
		t.Run(strings.Join(set, "+"), func(t *testing.T) {
			dis, stale, missing := compareRequiredColumns(
				auditOnly(set...), auditTables, auditBaseline)
			if got := summarise(t, dis, stale, missing); got != "" {
				t.Errorf("configured %v changed the answer; got:\n%s", set, got)
			}
		})
	}
}

// Known-positive for over-reporting: an asymmetry the baseline does not excuse
// is still reported. Without this, a fix that excused everything would pass.
func TestAsymmetryNewDisagreementIsStillReported(t *testing.T) {
	got := auditOnly("mysql", "postgres")
	// postgres now also requires a column nobody has excused.
	got["postgres"] = facts([]string{"audit_events"},
		map[string][]string{"audit_events": {"tenant_id"}})

	dis, stale, _ := compareRequiredColumns(got, auditTables, auditBaseline)
	if len(stale) != 0 {
		t.Errorf("expected no stale entries, got %v", renderAsymmetries(stale))
	}
	if len(dis) != 1 {
		t.Fatalf("expected exactly 1 disagreement, got %v", renderAsymmetries(dis))
	}
	if dis[0].Dialect != "postgres" || dis[0].Column != "tenant_id" {
		t.Errorf("wrong disagreement: %s", dis[0])
	}
}

// Known-positive for under-reporting: an entry whose dialect WAS compared and
// whose asymmetry is genuinely gone must still be reported.
//
// This is the case the narrowing must not swallow. A stale check scoped so
// tightly that it never fires would satisfy every other test in this file.
func TestAsymmetryClosedEntryIsStillReportedStale(t *testing.T) {
	// MySQL no longer requires it: all dialects supply the value.
	got := map[string]schemaFacts{
		"mysql":    facts([]string{"audit_events"}, nil),
		"postgres": facts([]string{"audit_events"}, nil),
	}

	dis, stale, _ := compareRequiredColumns(got, auditTables, auditBaseline)
	if len(dis) != 0 {
		t.Errorf("expected no disagreements, got %v", renderAsymmetries(dis))
	}
	if len(stale) != 1 {
		t.Fatalf("a compared-and-closed entry must be reported stale, got %v",
			renderAsymmetries(stale))
	}
	if stale[0].Dialect != "mysql" || stale[0].Column != "id" {
		t.Errorf("wrong stale entry: %s", stale[0])
	}
}

// The narrowing itself: an entry naming a dialect this run never configured is
// left alone, because the run has no evidence either way about it.
//
// Paired deliberately with the test above. Together they say the stale check
// fires on absence of the FACT and not on absence of the DIALECT, which is the
// distinction the pair-keyed version could not make.
func TestAsymmetryEntryForAnUnconfiguredDialectIsNotReportedStale(t *testing.T) {
	baseline := append([]columnAsymmetry{}, auditBaseline...)
	baseline = append(baseline, columnAsymmetry{
		Table: "audit_events", Column: "created_at", Dialect: "mssql", Plugin: "audit-log",
	})

	dis, stale, _ := compareRequiredColumns(
		auditOnly("mysql", "postgres"), auditTables, baseline)

	if len(dis) != 0 {
		t.Errorf("expected no disagreements, got %v", renderAsymmetries(dis))
	}
	if len(stale) != 0 {
		t.Errorf("an entry for an unconfigured dialect must not be called stale "+
			"-- this run measured nothing about it; got %v", renderAsymmetries(stale))
	}

	// ... and it IS reported once mssql is configured and disagrees with it.
	got := auditOnly("mysql", "postgres", "mssql")
	dis, stale, _ = compareRequiredColumns(got, auditTables, baseline)
	if len(stale) != 1 || stale[0].Dialect != "mssql" {
		t.Errorf("with mssql configured and the column not required, the entry must be "+
			"stale; got %v", renderAsymmetries(stale))
	}
	if len(dis) != 0 {
		t.Errorf("expected no disagreements, got %v", renderAsymmetries(dis))
	}
}

// Two dialects requiring and one supplying is two findings, one per requiring
// dialect -- not one finding about a pair. This is what makes an entry
// independently retireable as each dialect is fixed.
func TestAsymmetryReportsOneEntryPerRequiringDialect(t *testing.T) {
	got := map[string]schemaFacts{
		"mysql":    facts([]string{"audit_events"}, map[string][]string{"audit_events": {"id"}}),
		"mssql":    facts([]string{"audit_events"}, map[string][]string{"audit_events": {"id"}}),
		"postgres": facts([]string{"audit_events"}, nil),
	}

	dis, _, _ := compareRequiredColumns(got, auditTables, nil)
	if len(dis) != 2 {
		t.Fatalf("expected one entry per requiring dialect, got %v", renderAsymmetries(dis))
	}
	if dis[0].Dialect != "mssql" || dis[1].Dialect != "mysql" {
		t.Errorf("expected mssql and mysql entries, got %v", renderAsymmetries(dis))
	}
}

// A table absent from one dialect is the other test's business and must not
// surface as N column asymmetries. Unchanged behaviour, asserted because the
// restructure moved the code that does it.
func TestAsymmetryMissingTableIsNotAColumnDisagreement(t *testing.T) {
	got := map[string]schemaFacts{
		"mysql":    facts(nil, nil),
		"postgres": facts([]string{"audit_events"}, map[string][]string{"audit_events": {"id"}}),
	}

	dis, stale, missing := compareRequiredColumns(got, auditTables, nil)
	if len(dis) != 0 || len(stale) != 0 {
		t.Errorf("a missing table must produce no column findings, got dis=%v stale=%v",
			renderAsymmetries(dis), renderAsymmetries(stale))
	}
	if len(missing) != 1 || !strings.Contains(missing[0], "audit_events") {
		t.Errorf("expected the table reported as missing, got %v", missing)
	}
}

// The same narrowing along the other axis: a table that was SKIPPED because it
// is absent from some configured dialect produces no finding, and that is not
// evidence the asymmetry closed.
//
// Found by re-reading the fix rather than from a failure. Without it the stale
// check answers "did we find this?" when the question is "did we look?" -- the
// original bug, one axis over.
func TestAsymmetrySkippedTableIsNotEvidenceTheEntryClosed(t *testing.T) {
	got := map[string]schemaFacts{
		// audit_events exists only on postgres, so the table is skipped whole.
		"mysql":    facts(nil, nil),
		"postgres": facts([]string{"audit_events"}, nil),
	}

	dis, stale, missing := compareRequiredColumns(got, auditTables, auditBaseline)
	if len(dis) != 0 {
		t.Errorf("expected no disagreements, got %v", renderAsymmetries(dis))
	}
	if len(missing) != 1 {
		t.Errorf("expected the table reported as missing, got %v", missing)
	}
	if len(stale) != 0 {
		t.Errorf("a table that was never compared cannot show an entry is closed; got %v",
			renderAsymmetries(stale))
	}
}
