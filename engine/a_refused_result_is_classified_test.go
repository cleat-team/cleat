package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestAResultTheStoreRefusesIsClassifiedNotJustReported is cleat#1460.
//
// A workflow result one backend accepts and another refuses used to surface as
// the driver's own text with error_code "unknown" -- after the workflow body and
// its side effects had already run. The operator was shown
//
//	finalize workflow: pq: unsupported Unicode escape sequence (22P05)
//
// which names PostgreSQL's escape handling and not the result.
//
// IT ASSERTS A PREDICATE, NOT A TABLE. Which dialect refuses which payload is
// measured in store_result_rejection.go and is genuinely divergent, but writing
// that matrix into the assertions would make this a census that goes stale when
// a backend version changes its mind -- and it would fail for a reason that is
// not a defect. What must hold is narrower and does not drift:
//
//	every finalize either SUCCEEDS or fails with ErrResultRejected.
//
// "Refused, but unclassified" is the state cleat#1460 is about, and it is the
// only outcome this test rejects.
func TestAResultTheStoreRefusesIsClassifiedNotJustReported(t *testing.T) {
	type payload struct{ label, json string }
	payloads := []payload{
		{"nul-escape", `{"v":"\u0000"}`},
		{"lone-surrogate", `{"v":"\ud800"}`},
		{"deep-nesting", `{"v":` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `}`},
	}

	totalRefusals := 0
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			// No Enabled() guard here, deliberately. Setup already does it, and
			// does it better: it distinguishes "nothing configured" (skip) from
			// "configured and unreachable" (fatal), which a check here would
			// flatten into one skip. The sibling multi-backend tests call
			// Enabled() zero times for the same reason.
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			const defName = "result-rejection"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			start := func(t *testing.T, label string) *WorkflowInstance {
				t.Helper()
				id := fmt.Sprintf("reject-%s-%d", label, time.Now().UnixNano())
				if _, _, err := store.StartNewRun(ctx, id, defName, 1,
					json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
					t.Fatalf("StartNewRun: %v", err)
				}
				claimed, err := store.ClaimWorkflow(ctx, "worker-1")
				if err != nil || claimed == nil {
					t.Fatalf("ClaimWorkflow: %v %v", claimed, err)
				}
				return claimed
			}

			refusals := 0
			for _, p := range payloads {
				claimed := start(t, p.label)
				err := store.FinalizeWorkflowSegment(ctx, claimed.ID, "worker-1",
					claimed.Generation, nil, "done", p.json, "", "", nil, time.Time{})
				if err == nil {
					t.Logf("%-15s accepted by %s", p.label, backend.Name())
					continue
				}
				refusals++

				var ce *CleatError
				if !errors.As(err, &ce) {
					t.Errorf("%s refused the %s result and the error is not classified:\n"+
						"  %v\n\n"+
						"This is cleat#1460 exactly. The caller gets driver text and "+
						"error_code \"unknown\", so nothing says the RESULT was the "+
						"problem, which field, that another backend would have stored "+
						"it, or that the workflow body already ran.",
						backend.Name(), p.label, err)
					continue
				}
				if ce.Code != ErrResultRejected {
					t.Errorf("%s refused the %s result and it was classified %q, not %q:\n  %v",
						backend.Name(), p.label, ce.Code.String(),
						ErrResultRejected.String(), err)
					continue
				}
				// The stored column value is the whole point -- this string is
				// what an operator greps for.
				if got := ce.Code.String(); got != "result_rejected_by_store" {
					t.Errorf("error_code would be stored as %q", got)
				}
				// The message has to say the body already ran. That is the fact
				// that changes what an operator does next, and the driver text
				// never carried it.
				if !strings.Contains(err.Error(), "ALREADY RAN") {
					t.Errorf("%s/%s: the message does not say the workflow body already ran:\n  %v",
						backend.Name(), p.label, err)
				}
			}

			// VACUITY. A backend that accepted all three proves nothing here,
			// and silence would read as success.
			if refusals == 0 {
				t.Logf("NOTE: %s accepted every payload; this leg asserted nothing about "+
					"classification", backend.Name())
			}
			totalRefusals += refusals

			// CONTROL, and the test is unsound without it. wrapRejectedResult
			// returns unrecognised errors unchanged, and an implementation that
			// classified EVERY finalize failure as a refused result would pass
			// every assertion above. ErrFenceLost is the failure most likely to
			// be mislabelled, because it is the common one.
			claimed := start(t, "fence-control")
			err := store.FinalizeWorkflowSegment(ctx, claimed.ID, "worker-1",
				claimed.Generation+99, nil, "done", `{"ok":true}`, "", "", nil, time.Time{})
			if !errors.Is(err, ErrFenceLost) {
				t.Fatalf("the fence control did not lose its fence, so it controls nothing: %v", err)
			}
			var ce *CleatError
			if errors.As(err, &ce) && ce.Code == ErrResultRejected {
				t.Errorf("a LOST FENCE was classified as a refused result on %s.\n\n"+
					"The classification is over-claiming: every finalize failure would "+
					"be reported as a bad workflow result, which is worse than the "+
					"unclassified driver text it replaces -- it is confidently wrong "+
					"rather than merely unhelpful.", backend.Name())
			}
		})
	}

	if totalRefusals == 0 && !testing.Short() {
		t.Log("no backend refused any payload; if no dialect is configured this test " +
			"measured nothing at all")
	}
}
