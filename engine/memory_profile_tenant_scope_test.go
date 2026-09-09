package engine

// The workflow memory profile -- workflow_memory_stats and its underlying
// workflow_memory_samples -- was keyed by def_name alone on all three
// dialects, with no tenant_id column anywhere:
//
//	def_name TEXT PRIMARY KEY                   (postgres)
//	PRIMARY KEY (def_name)                      (mysql)
//	CONSTRAINT pk_workflow_memory_stats ...     (mssql)
//
// while workflow_defs is keyed (tenant_id, name, version). So a name
// identifies DIFFERENT WASM per tenant, and the EWMA blended the memory
// profile of unrelated code that happened to share a name -- `process_order`
// being the obvious case, likely rather than exotic.
//
// This is not only an out-of-band metric. GET /api/definitions opens with a
// tenant-scoped store, lists that tenant's definitions through it, and then
// enriches each one with LoadMemoryStats, which reads a table that has no
// tenant column to scope on. A tenant's own authenticated response therefore
// carried another tenant's percentiles: correct on the outer read, unscoped
// on the enrichment.
//
// No guard could have caught it. mssqlTenantScopedTables derives its universe
// from `ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.<table>`,
// so it answers "is every statement against a KNOWN tenant-scoped table
// scoped?" and cannot answer "is every table that should be tenant-scoped
// actually one?". A table with no tenant_id is not in its universe at all.
// See cleat#1040; the fix binds both tables to the policy, which puts them
// inside that universe from now on.

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
)

// uniqueDefName returns a definition name no other run of this test can also
// be using.
//
// The two tenant UUIDs below are shared with other tests by convention, and
// nothing truncates workflow_memory_samples between tests, so a FIXED name
// makes these assertions a function of what else has touched the table --
// including a second `go test` process against the same database, which
// CLAUDE.md warns produces failures that look like unrelated flakes. It did
// exactly that here: the retention case failed with counts too LOW because a
// concurrent run's now-tenant-scoped sweep was deleting the same tenant's
// rows under the same name.
//
// Deriving the suffix from the pid and a counter rather than a clock keeps it
// unique across processes without making the name a function of when the test
// ran.
var memoryDefSeq atomic.Int64

func uniqueDefName(base string) string {
	return fmt.Sprintf("%s-%d-%d", base, os.Getpid(), memoryDefSeq.Add(1))
}

// TestTheMemoryProfileIsScopedToTenant asserts that one tenant's memory
// samples do not reach another tenant's estimate or distribution.
//
// The two sample sizes are an order of magnitude apart so that a blend is
// unmistakable rather than a rounding question: with the EWMA's default
// alpha of 0.3, a leak reads as 0.3*9MB + 0.7*1MB = 3.4MB against the 1MB
// that tenant A actually recorded.
func TestTheMemoryProfileIsScopedToTenant(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			const (
				tenantA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
				tenantB = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"

				sampleA int64 = 1_000_000
				sampleB int64 = 9_000_000
			)

			// One name, deployed by both tenants. That is the whole point:
			// the defect is invisible unless the name is shared. Unique per
			// run so the assertions cannot be perturbed by anything else in
			// the table -- see uniqueDefName.
			sharedName := uniqueDefName("memory-profile-shared-name")

			tb, ok := backend.(tenantSetupBackend)
			if !ok {
				t.Fatalf("backend %T does not implement SetupForTenant; this test needs two tenants", backend)
			}

			storeA, teardownA := tb.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := tb.SetupForTenant(t, tenantB)
			defer teardownB()

			ctx := context.Background()

			// A records first, then B. Order matters for the falsification:
			// with one shared row, B's sample is folded into the EWMA that A
			// then reads back.
			if err := storeA.RecordWorkflowMemorySample(ctx, sharedName, sampleA); err != nil {
				t.Fatalf("tenant A record sample: %v", err)
			}
			if err := storeB.RecordWorkflowMemorySample(ctx, sharedName, sampleB); err != nil {
				t.Fatalf("tenant B record sample: %v", err)
			}

			// --- the EWMA summary -------------------------------------------------
			estimates, err := storeA.LoadMemoryEstimates(ctx)
			if err != nil {
				t.Fatalf("tenant A load estimates: %v", err)
			}
			got, ok := estimates[sharedName]
			if !ok {
				t.Fatalf("tenant A has no estimate for %q; it recorded one", sharedName)
			}
			if int64(got) != sampleA {
				t.Errorf("tenant A's estimate for %q is %.0f, want %d.\n"+
					"Tenant B recorded %d under the same name, so a value between the "+
					"two means the summary row is shared across tenants.",
					sharedName, got, sampleA, sampleB)
			}

			// --- the sample distribution ------------------------------------------
			stats, err := storeA.LoadMemoryStats(ctx)
			if err != nil {
				t.Fatalf("tenant A load stats: %v", err)
			}
			var found *WorkflowMemoryStats
			for i := range stats {
				if stats[i].DefName == sharedName {
					found = &stats[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("tenant A has no distribution for %q; it recorded a sample", sharedName)
			}
			if found.SampleCount != 1 {
				t.Errorf("tenant A's distribution for %q counts %d samples, want 1.\n"+
					"Tenant B's sample is being aggregated into tenant A's percentiles.",
					sharedName, found.SampleCount)
			}
			if found.MaxBytes != sampleA {
				t.Errorf("tenant A's max for %q is %d, want %d (tenant B recorded %d)",
					sharedName, found.MaxBytes, sampleA, sampleB)
			}

			// --- and the mirror, so the test cannot pass by scoping everything to
			// --- nothing: B must still see its own sample.
			estimatesB, err := storeB.LoadMemoryEstimates(ctx)
			if err != nil {
				t.Fatalf("tenant B load estimates: %v", err)
			}
			if gotB, ok := estimatesB[sharedName]; !ok || int64(gotB) != sampleB {
				t.Errorf("tenant B's estimate for %q is %v (present=%v), want %d",
					sharedName, gotB, ok, sampleB)
			}
		})
	}
}

// TestMemorySampleRetentionIsPerTenant asserts that the retention sweep keeps
// maxSamplesPerDef samples PER TENANT, not per name across all tenants.
//
// Filed as the second half of cleat#1040 and worth its own test because the
// consequence differs in kind: the blend above is a reporting defect, while
// this one silently reduces the retention each tenant is configured for, in
// proportion to how many tenants share the name.
func TestMemorySampleRetentionIsPerTenant(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			const (
				tenantA   = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
				tenantB   = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
				keep      = 3
				perTenant = 5
			)

			sharedName := uniqueDefName("memory-retention-shared-name")

			tb, ok := backend.(tenantSetupBackend)
			if !ok {
				t.Fatalf("backend %T does not implement SetupForTenant", backend)
			}

			storeA, teardownA := tb.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := tb.SetupForTenant(t, tenantB)
			defer teardownB()

			ctx := context.Background()

			for i := 0; i < perTenant; i++ {
				if err := storeA.RecordWorkflowMemorySample(ctx, sharedName, int64(1_000+i)); err != nil {
					t.Fatalf("tenant A sample %d: %v", i, err)
				}
				if err := storeB.RecordWorkflowMemorySample(ctx, sharedName, int64(9_000+i)); err != nil {
					t.Fatalf("tenant B sample %d: %v", i, err)
				}
			}

			if _, err := storeA.CleanupMemorySamples(ctx, keep); err != nil {
				t.Fatalf("tenant A cleanup: %v", err)
			}

			// A asked to keep 3 of its own. Unscoped, the sweep ranks all ten
			// rows together and keeps 3 in total.
			countFor := func(st WorkflowStore, who string) int {
				stats, err := st.LoadMemoryStats(ctx)
				if err != nil {
					t.Fatalf("%s load stats: %v", who, err)
				}
				for i := range stats {
					if stats[i].DefName == sharedName {
						return stats[i].SampleCount
					}
				}
				return 0
			}

			if n := countFor(storeA, "tenant A"); n != keep {
				t.Errorf("tenant A retained %d samples for %q, want %d", n, sharedName, keep)
			}

			// B never ran a sweep, so all five of its samples must survive.
			// This is the assertion that fails loudest on the unscoped code:
			// A's cleanup deletes B's rows.
			if n := countFor(storeB, "tenant B"); n != perTenant {
				t.Errorf("tenant B retained %d samples for %q, want %d -- "+
					"tenant A's retention sweep deleted rows belonging to tenant B",
					n, sharedName, perTenant)
			}
		})
	}
}
