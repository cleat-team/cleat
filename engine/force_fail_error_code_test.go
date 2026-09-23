package engine

// cleat#1977 (design doc D5): force-fail's error_code used to be free text
// all the way to the column -- ForceFail (engine/admin_ops.go) passed
// req.ErrorCode straight to store.AdminForceFail with no check, so an
// operator typo became a permanent, unrecognized classification on the row.
// These pin the three acceptance cases from the issue, on every dialect: no
// code stores "operator", an unrecognized code is a 400-shaped
// ErrAdminBadRequest that leaves the row untouched, and a recognized code is
// kept as given.
//
// Unlike engine/admin_force_resolve_test.go's AdminForceFail tests, these go
// through the ForceFail package function, not the store method directly --
// the validation lives in the wrapper, the same place ForceComplete's
// JSON-validity check already lives, and a store-level call bypasses it by
// design (an operator-facing rule, not a storage invariant).

import (
	"context"
	"errors"
	"testing"
)

func TestForceFail_NoErrorCodeStoresOperator(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID, _ := startClaimedWorkflow(t, ctx, store, "ffnc")
		before := mustGetWorkflow(t, ctx, store, wfID)

		if err := ForceFail(ctx, store, wfID, before.Generation, adminTestOperator, "no reason given", ""); err != nil {
			t.Fatalf("ForceFail with no error_code: %v", err)
		}

		after := mustGetWorkflow(t, ctx, store, wfID)
		if after.Status != statusFailed {
			t.Errorf("status = %q, want %q", after.Status, statusFailed)
		}
		if after.ErrorCode != "operator" {
			t.Errorf("error_code = %q, want %q", after.ErrorCode, "operator")
		}
	})
}

func TestForceFail_UnrecognizedErrorCodeIsRefusedAndLeavesTheRowUnchanged(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID, _ := startClaimedWorkflow(t, ctx, store, "ffbad")
		before := mustGetWorkflow(t, ctx, store, wfID)

		err := ForceFail(ctx, store, wfID, before.Generation, adminTestOperator, "oops", "banana")
		if !errors.Is(err, ErrAdminBadRequest) {
			t.Fatalf("ForceFail with error_code=banana: err = %v, want ErrAdminBadRequest", err)
		}

		after := mustGetWorkflow(t, ctx, store, wfID)
		if after.Status != before.Status {
			t.Errorf("status = %q after a refused force-fail, want unchanged %q", after.Status, before.Status)
		}
		if after.ErrorCode != before.ErrorCode {
			t.Errorf("error_code = %q after a refused force-fail, want unchanged %q", after.ErrorCode, before.ErrorCode)
		}
		if after.Generation != before.Generation {
			t.Errorf("generation = %d after a refused force-fail, want unchanged %d", after.Generation, before.Generation)
		}
		if n := countAdminEvents(t, ctx, store, wfID); n != 0 {
			t.Errorf("%d admin_action event(s) recorded for a request rejected before it reached the store, want 0", n)
		}
	})
}

func TestForceFail_ARecognizedErrorCodeIsStoredAsGiven(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID, _ := startClaimedWorkflow(t, ctx, store, "ffok")
		before := mustGetWorkflow(t, ctx, store, wfID)

		if err := ForceFail(ctx, store, wfID, before.Generation, adminTestOperator, "it took too long", "timeout"); err != nil {
			t.Fatalf("ForceFail with error_code=timeout: %v", err)
		}

		after := mustGetWorkflow(t, ctx, store, wfID)
		if after.ErrorCode != "timeout" {
			t.Errorf("error_code = %q, want %q", after.ErrorCode, "timeout")
		}
	})
}
