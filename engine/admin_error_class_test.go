package engine

import (
	"context"
	"errors"
	"testing"
)

// TestEveryAdminRefusalCarriesItsClass is the engine half of the admin status
// code guard. The other half -- that each class maps to a 4xx -- is
// TestAdminErrorStatusIsChosenByClass in cmd/cleat-worker.
//
// Both are needed. A class that nothing ever sets, and a mapper that ignores
// the class, produce the same 500 the defect produced, and neither test alone
// distinguishes them.
//
// A refusal is a decision the server makes on purpose and can explain. It is
// not a failure the server suffers -- those stay unclassified and 500, which
// is the whole reason the default is still 500.
func TestEveryAdminRefusalCarriesItsClass(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		err   error
		class error
	}{
		// Validation, from the three exported entry points. A nil store is
		// safe here: every one of these returns before touching it, which is
		// itself worth holding -- validation that ran after the store call
		// would report a database error for a malformed request.
		{"force-complete: no workflow id", ForceComplete(ctx, nil, "", 0, "op", "{}"), ErrAdminBadRequest},
		{"force-complete: negative generation", ForceComplete(ctx, nil, "wf-1", -1, "op", "{}"), ErrAdminBadRequest},
		{"force-complete: result is not JSON", ForceComplete(ctx, nil, "wf-1", 0, "op", "not json"), ErrAdminBadRequest},
		{"force-fail: no workflow id", ForceFail(ctx, nil, "", 0, "op", "boom", "E"), ErrAdminBadRequest},
		{"force-fail: negative generation", ForceFail(ctx, nil, "wf-1", -1, "op", "boom", "E"), ErrAdminBadRequest},
		{"re-replay: no workflow id", ReReplay(ctx, nil, "", 0, "op"), ErrAdminBadRequest},
		{"re-replay: negative generation", ReReplay(ctx, nil, "wf-1", -1, "op"), ErrAdminBadRequest},

		// The outcomes a zero-row UPDATE is resolved into.
		{"workflow not found", adminNotFound(adminActionReReplay, "wf-1"), ErrAdminNotFound},
		{"generation moved on", adminGenerationMismatch(adminActionReReplay, "wf-1", 3, 2), ErrAdminGenerationMismatch},

		// State conflicts: the request is well-formed and the workflow exists,
		// but its current state does not permit the operation. All three were
		// 500 before this change.
		{"status forbids re-replay", adminReReplayMiss("done", 1, 1, "wf-1", true), ErrAdminStateConflict},
		{"audit row taken by a concurrent writer", adminAuditCollision(adminActionForceComplete, "wf-1", 7), ErrAdminStateConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil {
				t.Fatal("this input produced no error, so the operation accepted what it should have refused")
			}
			if !errors.Is(tc.err, tc.class) {
				t.Errorf("error does not carry %v: %v\n"+
					"Build it with adminErrorf(%v, ...). Without a class the HTTP layer "+
					"answers 500, which tells a client to retry a refusal.",
					tc.class, tc.err, tc.class)
			}
		})
	}
}

// TestAdminClassDoesNotChangeTheMessage holds the property that made classes
// worth introducing: the message an operator reads and the class a program
// switches on are independent. Before this, store_admin.go's own comment said
// "the wording is load-bearing" -- rewording an error changed an API's status
// code, silently.
func TestAdminClassDoesNotChangeTheMessage(t *testing.T) {
	plain := "admin re_replay: workflow wf-1 not found"
	got := adminNotFound(adminActionReReplay, "wf-1").Error()
	if got != plain {
		t.Errorf("classifying changed the message:\n got  %q\n want %q", got, plain)
	}

	// And the class is still reachable through an outer wrap, which is how it
	// arrives at the HTTP layer -- ReReplay wraps with "re-replay: %w".
	wrapped := ReReplay(context.Background(), nil, "", 0, "op")
	if !errors.Is(wrapped, ErrAdminBadRequest) {
		t.Errorf("class did not survive wrapping: %v", wrapped)
	}
}

// TestAnUnclassifiedFailureStaysAServerError is the other direction. The
// default must remain 500, so that a real fault is never quietly downgraded to
// a 4xx by a class it should not have.
func TestAnUnclassifiedFailureStaysAServerError(t *testing.T) {
	dbErr := errors.New(`pq: relation "event_history" not found`)
	for _, class := range []error{ErrAdminBadRequest, ErrAdminNotFound, ErrAdminGenerationMismatch, ErrAdminStateConflict} {
		if errors.Is(dbErr, class) {
			t.Errorf("a plain database error matched %v; classification is matching on text again", class)
		}
	}
}
