package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// The listing can find the runs that asked for a given concurrency key.
//
// cleat#1172 measured the parameter being ACCEPTED and IGNORED:
//
//	/api/workflows?concurrency_key=K            -> 100 rows
//	/api/workflows?concurrency_key=NONSENSE-XYZ -> 100 rows
//
// Identical output for the real key, a nonsense key, and no filter at all. A
// parameter read and discarded is worse than one that 400s, because the caller
// reads the result as an answer.
//
// # Why the count is asserted alongside the rows
//
// applyWorkflowFilters is shared by the listing and by CountWorkflows, which is
// what X-Total-Count reports. If a filter were added to one and not the other,
// the page would narrow while the total stayed wide -- and a caller paging on
// that total would walk off the end of a result set that no longer exists. The
// two agreeing is the property; sharing the function is only the mechanism.
func TestListingFiltersByConcurrencyKey(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			// StartNewRunWithConcurrencyKey, not StartNewRunWithOptions: the
			// options struct is on an unmerged branch (cleat#1187) and this
			// change must stand on develop alone.
			starter, ok := store.(interface {
				StartNewRunWithConcurrencyKey(context.Context, string, string, int, json.RawMessage, string, string, int, string) (string, bool, error)
			})
			if !ok {
				t.Fatalf("%T cannot record a concurrency key", store)
			}

			stamp := time.Now().UnixNano()
			const wanted = "nightly-sync"
			const other = "hourly-sync"

			// Two runs want the key, one wants a different key, one wants none.
			// The third and fourth are what make a filter that returns
			// everything distinguishable from one that works.
			mk := func(i int, key string) string {
				t.Helper()
				id, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
					fmt.Sprintf("ck-list-%d-%d", i, stamp), "test-workflow", 1,
					json.RawMessage(`{}`), "", DefaultTenantUUID, 0, key)
				if err != nil {
					t.Fatalf("start[%d]: %v", i, err)
				}
				return id
			}
			a, b := mk(0, wanted), mk(1, wanted)
			c := mk(2, other)
			d := mk(3, "")

			counter, hasCount := store.(interface {
				CountWorkflows(context.Context, WorkflowFilter) (int, error)
			})

			check := func(name, key string, wantIDs ...string) {
				t.Helper()
				f := WorkflowFilter{ConcurrencyKey: key, Limit: 100}
				got, err := store.ListWorkflows(ctx, f)
				if err != nil {
					t.Fatalf("%s: ListWorkflows: %v", name, err)
				}
				seen := map[string]bool{}
				for _, wf := range got {
					seen[wf.ID] = true
				}
				if len(got) != len(wantIDs) {
					t.Errorf("%s: returned %d rows, want %d.\n\nThis is cleat#1172 if the count "+
						"equals the unfiltered total -- the parameter is being read and discarded.",
						name, len(got), len(wantIDs))
				}
				for _, id := range wantIDs {
					if !seen[id] {
						t.Errorf("%s: %s missing from the results", name, id)
					}
				}
				if hasCount {
					n, err := counter.CountWorkflows(ctx, f)
					if err != nil {
						t.Fatalf("%s: CountWorkflows: %v", name, err)
					}
					if n != len(got) {
						t.Errorf("%s: X-Total-Count would report %d for a page of %d.\n\n"+
							"The listing and the count must apply the same filter, or a caller "+
							"paging on the total walks off the end of the result set.", name, n, len(got))
					}
				}
			}

			check("the key two runs asked for", wanted, a, b)
			check("a key nobody asked for", "NONSENSE-XYZ")
			// No filter: all four, which is what the broken behaviour returned
			// for every one of the cases above.
			check("no filter at all", "", a, b, c, d)
		})
	}
}
