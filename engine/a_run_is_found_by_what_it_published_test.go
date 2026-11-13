package engine

// cleat#3775. A run publishes a map[string]string as its query state, and until now the only reads were
// one key of one known run and the whole map of one known run: "find the run for order 123" had no
// server-side answer. WorkflowFilter.QueryState selects runs by what they published.
//
// This runs on every registered backend, because the predicate is three different statements: PostgreSQL's
// `query_state @> '{...}'::jsonb`, MySQL's JSON_CONTAINS, and one EXISTS over OPENJSON per pair on SQL Server.
// The fixture is built so that each way they could differ shows up:
//
//   - a key or value that differs from the filter only by CASE must not match (SQL Server's default
//     collation is case-insensitive, so its form carries an explicit binary one and this is what proves it);
//   - an empty value and an empty key are real, and "published an empty status" is not "published none";
//   - a value with a quote, a backslash, a percent sign and a non-ASCII letter comes back by equality, which
//     is the case a LIKE-built form would get wrong;
//   - the count and the page agree, because both go through applyWorkflowFilters.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

// publishRun starts a run, claims it and suspends it carrying state, which is how a run's published state
// reaches the database while it is still alive (the case a caller searching for an open order is in).
func publishRun(t *testing.T, store WorkflowStore, idemKey string, state map[string]string) string {
	t.Helper()
	return publishRunAs(t, store, DefaultTenantUUID, "test-workflow", idemKey, state)
}

// publishRunAs is publishRun for a store opened for another tenant, and for another definition name.
func publishRunAs(t *testing.T, store WorkflowStore, tenant, def, idemKey string, state map[string]string) string {
	t.Helper()
	ctx := context.Background()
	id, _, err := store.StartNewRun(ctx, "", def, 1, json.RawMessage(`{}`), idemKey, tenant, 0)
	if err != nil {
		t.Fatalf("StartNewRun(%s): %v", idemKey, err)
	}
	wf, err := store.ClaimWorkflow(ctx, "worker-1")
	if err != nil || wf == nil || wf.ID != id {
		t.Fatalf("ClaimWorkflow(%s) = %v, %v; want run %s", idemKey, wf, err, id)
	}
	if err := store.FinalizeWorkflowSegment(ctx, id, "worker-1", wf.Generation, nil, "ready", "", "", "", state, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("FinalizeWorkflowSegment(%s): %v", idemKey, err)
	}
	return id
}

func publishedIDs(wfs []WorkflowInstance) []string {
	out := make([]string, 0, len(wfs))
	for _, w := range wfs {
		out = append(out, w.ID)
	}
	sort.Strings(out)
	return out
}

func TestARunIsFoundByWhatItPublished(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			const odd = "a\"b\\c é 100%_"
			runs := map[string]string{
				"open123":   publishRun(t, store, "qsf-1", map[string]string{"order_id": "123", "status": "open"}),
				"closed123": publishRun(t, store, "qsf-2", map[string]string{"order_id": "123", "status": "closed"}),
				"open456":   publishRun(t, store, "qsf-3", map[string]string{"order_id": "456", "status": "open"}),
				"nothing":   publishRun(t, store, "qsf-4", nil),
				"upperKey":  publishRun(t, store, "qsf-5", map[string]string{"ORDER_ID": "123"}),
				"upperVal":  publishRun(t, store, "qsf-6", map[string]string{"order_id": "789", "status": "Open"}),
				"emptyVal":  publishRun(t, store, "qsf-7", map[string]string{"note": ""}),
				"emptyKey":  publishRun(t, store, "qsf-8", map[string]string{"": "v"}),
				"odd":       publishRun(t, store, "qsf-9", map[string]string{"note": odd}),
			}
			want := func(names ...string) []string {
				out := make([]string, 0, len(names))
				for _, n := range names {
					out = append(out, runs[n])
				}
				sort.Strings(out)
				return out
			}

			for _, c := range []struct {
				name  string
				state map[string]string
				want  []string
			}{
				{"one pair selects every run that published it", map[string]string{"order_id": "123"}, want("open123", "closed123")},
				{"two pairs are ANDed", map[string]string{"order_id": "123", "status": "open"}, want("open123")},
				{"a pair no run published selects nothing", map[string]string{"order_id": "999"}, want()},
				{"one pair across different orders", map[string]string{"status": "open"}, want("open123", "open456")},
				{"a KEY that differs only by case is a different key", map[string]string{"order_id": "123"}, want("open123", "closed123")}, // not upperKey
				{"a VALUE that differs only by case is a different value", map[string]string{"status": "open"}, want("open123", "open456")},
				{"the upper-case key is found by itself", map[string]string{"ORDER_ID": "123"}, want("upperKey")},
				{"the upper-case value is found by itself", map[string]string{"status": "Open"}, want("upperVal")},
				{"an empty value is a value", map[string]string{"note": ""}, want("emptyVal")},
				{"a key that was not published is not an empty value", map[string]string{"nope": ""}, want()},
				{"an empty key is a key", map[string]string{"": "v"}, want("emptyKey")},
				{"a value with a quote, a backslash, a percent sign and an accent comes back by equality", map[string]string{"note": odd}, want("odd")},
				{"a LIKE wildcard in the value is not a wildcard", map[string]string{"note": "a\"b\\c é 100%"}, want()},
			} {
				c := c
				t.Run(c.name, func(t *testing.T) {
					filter := WorkflowFilter{QueryState: c.state, Limit: 100}
					got, err := store.ListWorkflows(ctx, filter)
					if err != nil {
						t.Fatalf("ListWorkflows(%v): %v", c.state, err)
					}
					if g := publishedIDs(got); !reflect.DeepEqual(g, c.want) && !(len(g) == 0 && len(c.want) == 0) {
						t.Errorf("ListWorkflows(%v) = %v, want %v", c.state, g, c.want)
					}
					n, err := store.CountWorkflows(ctx, filter)
					if err != nil {
						t.Fatalf("CountWorkflows(%v): %v", c.state, err)
					}
					if n != len(c.want) {
						t.Errorf("CountWorkflows(%v) = %d, want %d: the count and the page must see the same rows", c.state, n, len(c.want))
					}
				})
			}

			t.Run("no filter still lists every run", func(t *testing.T) {
				got, err := store.ListWorkflows(ctx, WorkflowFilter{Limit: 100})
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != len(runs) {
					t.Errorf("an empty QueryState narrowed the listing to %d of %d runs", len(got), len(runs))
				}
			})

			t.Run("it composes with the filters that were already there", func(t *testing.T) {
				got, err := store.ListWorkflows(ctx, WorkflowFilter{QueryState: map[string]string{"order_id": "123"}, Status: "ready", Limit: 100})
				if err != nil {
					t.Fatal(err)
				}
				if g := publishedIDs(got); !reflect.DeepEqual(g, want("open123", "closed123")) {
					t.Errorf("order_id=123 and status=ready = %v, want %v", g, want("open123", "closed123"))
				}
				got, err = store.ListWorkflows(ctx, WorkflowFilter{QueryState: map[string]string{"order_id": "123"}, Status: "done", Limit: 100})
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != 0 {
					t.Errorf("order_id=123 and status=done = %v, want none: the state filter dropped the status one", publishedIDs(got))
				}
			})

			t.Run("it pages with the total order the listing already uses", func(t *testing.T) {
				filter := WorkflowFilter{QueryState: map[string]string{"order_id": "123"}, Limit: 1}
				first, err := store.ListWorkflows(ctx, filter)
				if err != nil || len(first) != 1 {
					t.Fatalf("page 1 = %v, %v", publishedIDs(first), err)
				}
				filter.Offset = 1
				second, err := store.ListWorkflows(ctx, filter)
				if err != nil || len(second) != 1 || second[0].ID == first[0].ID {
					t.Fatalf("page 2 = %v, %v; want the other run", publishedIDs(second), err)
				}
			})

			t.Run("more than the cap is refused, by the store, on this dialect", func(t *testing.T) {
				tooMany := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}
				if _, err := store.ListWorkflows(ctx, WorkflowFilter{QueryState: tooMany}); !errors.Is(err, ErrQueryStateFilterTooManyPairs) {
					t.Errorf("ListWorkflows with %d pairs: err = %v, want ErrQueryStateFilterTooManyPairs", len(tooMany), err)
				}
				if _, err := store.CountWorkflows(ctx, WorkflowFilter{QueryState: tooMany}); !errors.Is(err, ErrQueryStateFilterTooManyPairs) {
					t.Errorf("CountWorkflows with %d pairs: err = %v, want ErrQueryStateFilterTooManyPairs", len(tooMany), err)
				}
				atCap := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"}
				if _, err := store.ListWorkflows(ctx, WorkflowFilter{QueryState: atCap}); err != nil {
					t.Errorf("ListWorkflows at the cap of %d pairs: %v", len(atCap), err)
				}
			})
		})
	}
}
