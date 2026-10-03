package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// cleat#3009.
//
// WorkflowFilter.ExcludeDefNames has to be applied BY THE QUERY, alongside every
// other filter, so that the listing and the count that accompanies it are
// computed over the same rows. The defect it replaces was a caller-side filter
// applied to a page AFTER offset and limit: pages came back shorter than the
// limit asked for, and any total derived from them was approximate.
//
// So the assertion that matters is not "the row is absent" -- a post-page filter
// achieves that too -- but that the exclusion happens BEFORE the limit, which is
// what a full page beside an exact count demonstrates.
func TestExcludingDefinitionsFiltersBeforePaging(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			// Three definitions, one of them internal, and a run of each. The
			// internal one is created by the same deploy path a caller uses, so
			// the class is read back through the store rather than injected.
			for _, d := range []struct {
				name     string
				exposure ExposureClass
			}{
				{"keep-a", ExposureAuth},
				{"hidden", ExposureInternal},
				{"keep-b", ExposureAuth},
			} {
				if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: d.name, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
					ABIVersion: 1, MinVersion: 1, Exposure: d.exposure,
				}); err != nil {
					t.Fatalf("DeployWorkflowDef(%s): %v", d.name, err)
				}
				id := fmt.Sprintf("%s-%d", d.name, time.Now().UnixNano())
				if _, _, err := store.StartNewRun(ctx, id, d.name, 1,
					json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
					t.Fatalf("StartNewRun(%s): %v", d.name, err)
				}
			}

			// THE CONTROL, FIRST. It proves the limit bites at all -- without it,
			// "two rows came back under a limit of two" is satisfied by a store
			// that ignores limits entirely, and the exclusion below would be
			// measuring nothing.
			control, err := store.ListWorkflows(ctx, WorkflowFilter{Limit: 2})
			if err != nil {
				t.Fatalf("ListWorkflows (control): %v", err)
			}
			if len(control) != 2 {
				t.Fatalf("CONTROL FAILED: an unfiltered list with Limit=2 returned %d rows, want 2 -- so a full "+
					"page below would not show that the exclusion ran before the limit", len(control))
			}
			controlTotal, err := store.CountWorkflows(ctx, WorkflowFilter{})
			if err != nil {
				t.Fatalf("CountWorkflows (control): %v", err)
			}
			if controlTotal != 3 {
				t.Fatalf("CONTROL FAILED: an unfiltered count returned %d, want 3 -- the fixture did not seed "+
					"three runs, so nothing below is comparable", controlTotal)
			}

			filter := WorkflowFilter{ExcludeDefNames: []string{"hidden"}, Limit: 2}

			// THE PROPERTY: a FULL page. Two rows under a limit of two, from a
			// population of three of which one is excluded. A post-page filter
			// could not produce this: it would fetch two rows (possibly one of
			// them hidden), drop the hidden one, and return one.
			page, err := store.ListWorkflows(ctx, filter)
			if err != nil {
				t.Fatalf("ListWorkflows: %v", err)
			}
			if len(page) != 2 {
				t.Errorf("a limit of 2 returned %d rows with one definition excluded, want 2 -- the exclusion is "+
					"being applied AFTER the limit, which is the defect", len(page))
			}
			for _, wf := range page {
				if wf.DefName == "hidden" {
					t.Errorf("an excluded definition came back: %s", wf.ID)
				}
			}

			// AND THE COUNT AGREES WITH THE PAGE: same filter, same rows, so the
			// total a caller reports is exact rather than approximate.
			total, err := store.CountWorkflows(ctx, filter)
			if err != nil {
				t.Fatalf("CountWorkflows: %v", err)
			}
			if total != 2 {
				t.Errorf("the count says %d where the same filter's page holds %d rows -- the two are computed "+
					"over different row sets, which is exactly what moving the filter into the query fixed",
					total, len(page))
			}

			// An exclusion list can be several names, and it must not disturb the
			// other filters it is ANDed with.
			both, err := store.CountWorkflows(ctx, WorkflowFilter{
				ExcludeDefNames: []string{"hidden"},
				DefName:         "keep-a",
			})
			if err != nil {
				t.Fatalf("CountWorkflows (exclude + include): %v", err)
			}
			if both != 1 {
				t.Errorf("exclusion ANDed with an exact definition name returned %d, want 1", both)
			}
		})
	}
}
