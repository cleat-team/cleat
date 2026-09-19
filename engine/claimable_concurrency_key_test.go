package engine

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The claim path's concurrency-key filter is written out many times over --
// two arms, at different site counts: the mutex arm (a bare key is held by at
// most one run) appears at NINE sites, the registered-queue arm (a declared
// queue admits at most concurrency_limit holders) at SIX. This test is the
// reason that duplication is safe.
//
// # Why it is not one shared constant
//
// It was, first: one placeholder-free string per dialect, spliced in with `+.
// That broke TestMySQLTenantScopedTablesAreQueriedWithATenantPredicate and its
// SQL Server twin, and the header of mssql_tenant_predicate_test.go says why in
// so many words -- "It also cannot see SQL built by concatenation." Those guards
// read each raw string literal on its own. Splicing turned whole statements
// into fragments, so the half holding `workflow_instances` no longer held the
// `tenant_id = @pN` that justified it, and the guard that is the WHOLE of
// tenant isolation on the two dialects without row-level security started
// reporting violations it could not evaluate.
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
// malformed. That is what makes every copy worth a guard.
//
// # Two arms, two site counts
//
// cleat#1116 split the predicate. The mutex arm keeps the original shape --
// `concurrency_key_hash IS NULL OR NOT EXISTS (…)` -- and still appears at the
// count/claim sites *and* at the sticky claim path, which was
// deliberately left mutex-only (it is an orthogonal routing path, not queue
// admission). The registered arm -- `count(queue_holders) < concurrency_limit`
// -- appears only where a declared queue is actually being admitted against:
// CountRunnableWorkflows and ClaimWorkflows, one each per dialect.
func TestTheClaimableConcurrencyKeyPredicateIsIdenticalAtEverySite(t *testing.T) {
	// Ordered, not a map: Go randomises map iteration, which would make WHICH
	// file supplies the canonical copy vary per run -- and with it the wording
	// of any failure. A guard whose message changes between identical runs is
	// hard to trust and harder to bisect.
	// THE COUNTS CAME DOWN when the widened cross-tenant claim was retired:
	// each dialect had one such claim and each carried a copy of this
	// predicate, so MySQL and SQL Server lost one apiece, and PostgreSQL's
	// copy lived in the admin.claim_workflows function rather than in this
	// file. Lowering a census because statements were deleted is the intended
	// edit; the guard's question -- do the remaining sites all spell it the
	// same way -- is unchanged.
	files := []struct {
		name           string
		mutexWant      int
		registeredWant int
	}{
		{"store_lifecycle.go", 3, 2},
		{"mysql_lifecycle.go", 3, 2},
		{"mssql_lifecycle.go", 3, 2},
	}

	// The clock is the one licensed difference between dialects.
	clock := regexp.MustCompile(`now\(\)|NOW\(6\)|SYSUTCDATETIME\(\)`)
	space := regexp.MustCompile(`\s+`)

	// The mutex arm names the workflow row `w.` at the count/claim sites (which
	// LEFT JOIN queues and alias it) and `workflow_instances.` at the
	// sticky sites (which do not). Normalise both to `w.` so the
	// copies compare equal.
	//
	// Grab from "concurrency_key_hash IS NULL OR NOT EXISTS" to the ")" that
	// closes the NOT EXISTS arm. Requires the expires_at test, which is what
	// distinguishes the DEFERRAL predicate from other clauses that also mention
	// concurrency_key_hash.
	//
	// cleat#1186 added one: SQL Server's claim carries
	// `AND (concurrency_key_hash IS NULL OR EXISTS (... k.workflow_id = ...id))`
	// to require that a run holds its OWN key. That is a different question --
	// "do I hold it" rather than "is it held by someone else, right now" -- and
	// without the expires_at discriminator the guard counted it as an extra copy
	// and failed. It failing was correct: two clauses that look alike and mean
	// different things is exactly what it exists to notice.
	mutex := regexp.MustCompile(`(?s)concurrency_key_hash IS NULL\s*OR NOT EXISTS.*?workflow_id <>\s*w\.id\)`)

	// The registered arm: the subquery counting live queue_holders, compared
	// against the declared limit. Grabbed from the opening paren of the count
	// subquery through the "< q.concurrency_limit)" comparison.
	registered := regexp.MustCompile(`(?s)\(SELECT count\(\*\) FROM queue_holders qh\s+WHERE.*?< q\.concurrency_limit\s*\)`)

	var mutexCanon, regCanon, mutexFrom, regFrom string
	mutexTotal, regTotal := 0, 0

	for _, f := range files {
		raw, err := os.ReadFile(f.name)
		if err != nil {
			t.Fatalf("read %s: %v", f.name, err)
		}
		src := strings.ReplaceAll(string(raw), "workflow_instances.", "w.")

		mh := mutex.FindAllString(src, -1)
		if len(mh) != f.mutexWant {
			t.Errorf("%s: found %d copies of the mutex arm, expected %d.\n\n"+
				"A claim or count statement either lost the filter or gained one that does not "+
				"match the shape this test looks for. Both are the divergence this test exists "+
				"to catch -- see the comment above.", f.name, len(mh), f.mutexWant)
			continue
		}
		mutexTotal += len(mh)
		for i, h := range mh {
			norm := space.ReplaceAllString(clock.ReplaceAllString(h, "<CLOCK>"), " ")
			if mutexCanon == "" {
				mutexCanon, mutexFrom = norm, f.name
				continue
			}
			if norm != mutexCanon {
				t.Errorf("%s mutex copy %d differs from the one in %s.\n\n  this: %s\n  that: %s\n\n"+
					"Every site must carry the same predicate modulo the dialect clock "+
					"function. Divergence here is silent at runtime.", f.name, i+1, mutexFrom, norm, mutexCanon)
			}
		}

		rh := registered.FindAllString(src, -1)
		if len(rh) != f.registeredWant {
			t.Errorf("%s: found %d copies of the registered-queue arm, expected %d.\n\n"+
				"A count or claim statement either lost the queue-limit filter or gained one "+
				"that does not match the shape this test looks for.", f.name, len(rh), f.registeredWant)
			continue
		}
		regTotal += len(rh)
		for i, h := range rh {
			norm := space.ReplaceAllString(clock.ReplaceAllString(h, "<CLOCK>"), " ")
			if regCanon == "" {
				regCanon, regFrom = norm, f.name
				continue
			}
			if norm != regCanon {
				t.Errorf("%s registered copy %d differs from the one in %s.\n\n  this: %s\n  that: %s\n\n"+
					"Every site must carry the same predicate modulo the dialect clock "+
					"function. Divergence here is silent at runtime.", f.name, i+1, regFrom, norm, regCanon)
			}
		}
	}

	if mutexTotal != 9 {
		t.Errorf("found %d mutex-arm sites in total, expected 9", mutexTotal)
	}
	if regTotal != 6 {
		t.Errorf("found %d registered-arm sites in total, expected 6", regTotal)
	}

	// The original design note, kept as an assertion because it is the property
	// that lets this text sit inside statements numbered $N, ? and @pN alike --
	// and two of them are built with fmt.Sprintf. A placeholder introduced here
	// would shift every index after its insertion point, at four sites per
	// dialect, and the failure would be a query that RUNS and matches the wrong
	// rows.
	if regexp.MustCompile(`\$\d|@p\d|\?`).MatchString(mutexCanon + regCanon) {
		t.Errorf("a predicate arm has acquired a placeholder:\n  %s\n  %s\n\n"+
			"It must reference only columns and literals. See the comment above.", mutexCanon, regCanon)
	}
	if !strings.Contains(mutexCanon, "ck.tenant_id = w.tenant_id") {
		t.Errorf("the mutex arm no longer correlates the key to the candidate row's tenant:\n  %s\n\n"+
			"Without it a key held by one tenant can defer another tenant's run.", mutexCanon)
	}
}
