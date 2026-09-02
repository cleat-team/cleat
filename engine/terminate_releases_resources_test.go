package engine

import (
	"context"
	"testing"
	"time"
)

// Terminating a workflow releases the resources it held, on every dialect.
//
// releaseWorkflowResources' own doc comment states the contract:
//
//	runs the two best-effort cleanups that follow EVERY commit which takes a
//	workflow out of the runnable set: completion, failure, termination,
//	continue-as-new, and the admin actions
//
// PostgresStore.TerminateWorkflow (engine/db.go) and MSSQLStore's
// (engine/mssql_operations.go) call it after their commit.
// MySQLStore.TerminateWorkflow (engine/mysql_ops.go) does not call it at all --
// it execs the UPDATE and returns.
//
// The cost is bounded but real: concurrency_keys.expires_at is NOT NULL and the
// worker's reaper deletes expired rows, so the slot is not leaked forever. It is
// held until the key's TTL, and live workflows queue behind it for that whole
// window, on a tier-1 dialect, while the other two release it immediately.
//
// This asserts the released state rather than the call, because the call is the
// implementation and the released slot is the property. A dialect that released
// the key by some other route would rightly pass.
func TestTerminateWorkflowReleasesConcurrencyKeys(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			deployConcurrencyTestWorkflows(t, store, "wf-term", "wf-next")

			// wf-term takes the only slot for key-term.
			acquired, err := store.AcquireConcurrencyKey(ctx, "key-term", "wf-term", time.Hour)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey: %v", err)
			}
			if !acquired {
				t.Fatal("the first acquire must succeed, or the rest of this test is vacuous")
			}

			// Control: while wf-term holds it, nobody else can.
			taken, err := store.AcquireConcurrencyKey(ctx, "key-term", "wf-next", time.Hour)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey (contended): %v", err)
			}
			if taken {
				t.Fatal("a second workflow acquired a held key, so this test cannot " +
					"tell a released slot from one that was never held")
			}

			if err := store.TerminateWorkflow(ctx, "wf-term", "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}

			// The property: the slot is free for the next workflow.
			freed, err := store.AcquireConcurrencyKey(ctx, "key-term", "wf-next", time.Hour)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey (after terminate): %v", err)
			}
			if !freed {
				t.Errorf("terminating wf-term did not release its concurrency key.\n\n" +
					"releaseWorkflowResources' contract names termination explicitly, " +
					"and PostgresStore and MSSQLStore call it after their commit. If " +
					"this dialect's TerminateWorkflow execs the UPDATE and returns, the " +
					"slot stays held until concurrency_keys.expires_at and every " +
					"workflow queued on this key waits out that TTL for nothing.")
			}
		})
	}
}
