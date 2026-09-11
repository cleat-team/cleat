// cleat#1170. A duplicate idempotency key presenting a DIFFERENT payload was
// handed the first request's workflow id with alreadyExisted = true, and its own
// input was discarded without a word.
//
// That is quieter than the duplicate execution idempotency keys exist to
// prevent: a duplicate execution leaves a row behind, and a silently discarded
// request leaves nothing anywhere. The caller believes its request ran; the only
// record is a run carrying somebody else's arguments.
//
// EXECUTED, not asserted about source. idempotency_def_scope_test.go checks the
// cleat#1047 half by reading the three stores' source, for the reason it gives:
// the behaviour is one comparison written out once per dialect, and what breaks
// is one store being edited and the others not. That argument holds, and it is
// second best. These run the statements.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestAKeyReusedWithADifferentInputIsRefused(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			const def = "intent-workflow"
			deployDef(t, ctx, store, def)
			tenant := DefaultTenantUUID
			first := json.RawMessage(`{"n":7}`)

			key := "cleat-1170-" + uuid.NewString()
			runID, existed, err := store.StartNewRun(ctx, "", def, 1, first, key, tenant, 0)
			if err != nil {
				t.Fatalf("first start: %v", err)
			}
			if existed {
				t.Fatalf("a fresh key reported already-started; the fixture is not fresh")
			}

			// CONTROL ONE: the same key with the same input still replays.
			// Without it, "the second call was refused" is equally satisfied by
			// a change that refuses every repeat -- which would break the
			// feature rather than fix it.
			same, existed, err := store.StartNewRun(ctx, "", def, 1, first, key, tenant, 0)
			if err != nil {
				t.Fatalf("CONTROL FAILED: replaying a key with the SAME input errored: %v", err)
			}
			if !existed || same != runID {
				t.Fatalf("CONTROL FAILED: replaying a key with the same input returned (%q, existed=%v), want (%q, true)",
					same, existed, runID)
			}

			// CONTROL TWO: formatting is not payload. The digest canonicalises
			// the JSON, so reordered keys and added whitespace are the same
			// request. A byte-wise digest would refuse this, and the refusal
			// would look exactly like the feature working.
			reordered := json.RawMessage(`{ "n" : 7 }`)
			same, existed, err = store.StartNewRun(ctx, "", def, 1, reordered, key, tenant, 0)
			if err != nil {
				t.Fatalf("CONTROL FAILED: the same object with different whitespace was refused: %v.\n\n"+
					"The digest is a property of the VALUE, not of the bytes; a caller that "+
					"reformats its JSON has not made a different request.", err)
			}
			if !existed || same != runID {
				t.Fatalf("CONTROL FAILED: reformatted input returned (%q, existed=%v), want (%q, true)",
					same, existed, runID)
			}

			// THE SUBJECT.
			second := json.RawMessage(`{"n":999}`)
			got, existed, err := store.StartNewRun(ctx, "", def, 1, second, key, tenant, 0)
			if !errors.Is(err, ErrIdempotencyKeyInputMismatch) {
				t.Errorf("a key reused with a DIFFERENT input returned (%q, existed=%v, err=%v); "+
					"want ErrIdempotencyKeyInputMismatch.\n\n"+
					"Replaying here hands the caller a run started with somebody else's "+
					"arguments and says nothing. Nothing in the response, and nothing in the "+
					"store, records that a second request was ever made (cleat#1170).",
					got, existed, err)
			}

			// AND THE RUN IS UNTOUCHED. A refusal that also corrupted the
			// original would satisfy the assertion above.
			wf, err := store.GetWorkflowByID(ctx, runID)
			if err != nil {
				t.Fatalf("read back the first run: %v", err)
			}
			if wf == nil {
				t.Fatalf("the first run is gone after a refused duplicate")
			}
			var stored struct {
				N int `json:"n"`
			}
			if err := json.Unmarshal([]byte(wf.Input), &stored); err != nil {
				t.Fatalf("unmarshal stored input %q: %v", wf.Input, err)
			}
			if stored.N != 7 {
				t.Errorf("the first run's input is now n=%d, want 7 -- the refused request "+
					"changed the run it was refused against", stored.N)
			}
		})
	}
}

// TestAKeyReusedForAnotherDefinitionIsRefused executes the cleat#1047 half that
// idempotency_def_scope_test.go can only assert about source, and pins the two
// refusals as DISTINGUISHABLE: a caller branching on errors.Is must be able to
// tell "wrong workflow" from "wrong arguments", and a single shared error would
// pass every assertion above.
func TestAKeyReusedForAnotherDefinitionIsRefused(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			deployDef(t, ctx, store, "intent-workflow")
			deployDef(t, ctx, store, "some-other-workflow")

			input := json.RawMessage(`{"n":7}`)
			key := "cleat-1047-" + uuid.NewString()

			if _, _, err := store.StartNewRun(ctx, "", "intent-workflow", 1, input, key, DefaultTenantUUID, 0); err != nil {
				t.Fatalf("first start: %v", err)
			}
			_, _, err := store.StartNewRun(ctx, "", "some-other-workflow", 1, input, key, DefaultTenantUUID, 0)
			if !errors.Is(err, ErrIdempotencyKeyDefMismatch) {
				t.Errorf("a key reused for another DEFINITION returned %v, want ErrIdempotencyKeyDefMismatch", err)
			}
			if errors.Is(err, ErrIdempotencyKeyInputMismatch) {
				t.Errorf("the definition mismatch also matches ErrIdempotencyKeyInputMismatch; "+
					"a caller cannot tell the two apart. err=%v", err)
			}
		})
	}
}

// deployDef makes the definition the run's foreign key requires.
func deployDef(t *testing.T, ctx context.Context, store WorkflowStore, name string) {
	t.Helper()
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: name, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef(%s): %v", name, err)
	}
}
