package engine

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The claim path's concurrency-key filter is written out ELEVEN TIMES -- three
// statements in store_lifecycle.go, four each in mysql_ and mssql_lifecycle.go.
// This test is the reason that is safe.
//
// # Why it is not one shared constant
//
// It was, first: one placeholder-free string per dialect, spliced in with `+.
// That broke TestMySQLTenantScopedTablesAreQueriedWithATenantPredicate and its
// SQL Server twin, and the header of mssql_tenant_predicate_test.go says why in
// so many words -- "It also cannot see SQL built by concatenation." Those guards
// read each raw string literal on its own. Splicing turned eleven whole
// statements into twenty-two fragments, so the half holding `workflow_instances`
// no longer held the `tenant_id = @pN` that justified it, and the guard that is
// the WHOLE of tenant isolation on the two dialects without row-level security
// started reporting violations it could not evaluate.
//
// A predicate cannot be shared by concatenation and checked by those guards at
// the same time. The guards win: they protect tenant isolation, this protects
// against a typo. So the text is duplicated and divergence is made a test
// failure instead of an impossibility.
//
// # Why divergence would be quiet
//
// CountRunnableWorkflows' own comment says it "mirrors ClaimWorkflows' candidate
// predicate exactly". If one site gains the filter and another does not, nothing
// errors: the count simply stops describing what is claimable, or one claim path
// hands out a run another would have deferred. No statement fails, no row is
// malformed. That is what makes eleven copies worth a guard.
func TestTheClaimableConcurrencyKeyPredicateIsIdenticalAtEverySite(t *testing.T) {
	// Ordered, not a map: Go randomises map iteration, which would make WHICH
	// file supplies the canonical copy vary per run -- and with it the wording
	// of any failure. A guard whose message changes between identical runs is
	// hard to trust and harder to bisect.
	files := []struct {
		name string
		want int
	}{
		{"store_lifecycle.go", 3},
		{"mysql_lifecycle.go", 4},
		{"mssql_lifecycle.go", 4},
	}

	// The clock is the one licensed difference between dialects.
	clock := regexp.MustCompile(`now\(\)|NOW\(6\)|SYSUTCDATETIME\(\)`)
	space := regexp.MustCompile(`\s+`)

	// Grab from "AND (workflow_instances.concurrency_key_hash" to the closing
	// "))" that ends the NOT EXISTS arm.
	// Requires the expires_at test, which is what distinguishes the DEFERRAL
	// predicate from other clauses that also mention concurrency_key_hash.
	//
	// cleat#1186 added one: SQL Server's claim carries
	// `AND (concurrency_key_hash IS NULL OR EXISTS (... k.workflow_id = ...id))`
	// to require that a run holds its OWN key. That is a different question --
	// "do I hold it" rather than "is it held by someone else, right now" -- and
	// without this the guard counted it as a fifth copy in that file and failed.
	// It failing was correct: two clauses that look alike and mean different
	// things is exactly what it exists to notice. The fix is to say which one
	// this test is about, not to loosen it.
	extract := regexp.MustCompile(`(?s)AND \(workflow_instances\.concurrency_key_hash IS NULL\s*OR NOT EXISTS.*?ck\.expires_at.*?workflow_instances\.id\)\)`)

	var canon string
	var canonFrom string
	total := 0

	for _, f := range files {
		name, want := f.name, f.want
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		hits := extract.FindAllString(string(src), -1)
		if len(hits) != want {
			t.Errorf("%s: found %d copies of the claimable-concurrency-key predicate, expected %d.\n\n"+
				"A claim or count statement either lost the filter or gained one that does not "+
				"match the shape this test looks for. Both are the divergence this test exists "+
				"to catch -- see the comment above.", name, len(hits), want)
			continue
		}
		total += len(hits)

		for i, h := range hits {
			norm := space.ReplaceAllString(clock.ReplaceAllString(h, "<CLOCK>"), " ")
			if canon == "" {
				canon, canonFrom = norm, name
				continue
			}
			if norm != canon {
				t.Errorf("%s copy %d differs from the one in %s.\n\n  this: %s\n  that: %s\n\n"+
					"Every site must carry the same predicate modulo the dialect clock "+
					"function. Divergence here is silent at runtime.", name, i+1, canonFrom, norm, canon)
			}
		}
	}

	if total != 11 {
		t.Errorf("found %d sites in total, expected 11", total)
	}

	// The original design note, kept as an assertion because it is the property
	// that lets this text sit inside statements numbered $N, ? and @pN alike --
	// and two of them are built with fmt.Sprintf. A placeholder introduced here
	// would shift every index after its insertion point, at four sites per
	// dialect, and the failure would be a query that RUNS and matches the wrong
	// rows.
	if regexp.MustCompile(`\$\d|@p\d|\?`).MatchString(canon) {
		t.Errorf("the predicate has acquired a placeholder:\n  %s\n\n"+
			"It must reference only columns and literals. See the comment above.", canon)
	}
	if !strings.Contains(canon, "ck.tenant_id = workflow_instances.tenant_id") {
		t.Errorf("the predicate no longer correlates the key to the candidate row's tenant:\n  %s\n\n"+
			"Without it a key held by one tenant can defer another tenant's run.", canon)
	}
}
