package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// cleat#4340. A signal sent to a run that can never be reopened (done, cancelled, or one that continued as new) used to be ACCEPTED, stored under that run's own id and read by no one, with the sender told
// "delivered". It is now refused, in the statement that would have stored it, with a typed error naming the state (and, for a run that continued as new, the run to use instead).
//
// It is NOT refused for failed, terminated or dead_lettered, which an operator can reopen under the same id with their stored signals in place: "signal a dead-lettered run, then retry it" works today and
// removing it is the owner's decision (cleat#4345). TestASignalToARedrivableRunIsAcceptedAndReadAfterTheRedrive pins that side, because a rule tested only on the side it refuses would not notice
// someone widening it back to all five.

// settleRun puts a fresh child of parent into the named settled state by the real store call that does it, and returns its id.
func settleRun(t *testing.T, ctx context.Context, store WorkflowStore, parent, how string) string {
	t.Helper()
	starter := store.(interface {
		StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error)
	})
	id, err := starter.StartChildWorkflow(ctx, parent, "intent-workflow", `{}`, 1, "TERMINATE", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}
	claim := func() *WorkflowInstance {
		wf, err := claimSpecific(t, ctx, store, id, "w-settle")
		if err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
		return wf
	}
	switch how {
	case "done":
		wf := claim()
		if err := store.CompleteWorkflow(ctx, id, "w-settle", wf.Generation, `{"r":1}`, nil); err != nil {
			t.Fatalf("CompleteWorkflow: %v", err)
		}
	case "failed":
		wf := claim()
		if err := store.FailWorkflow(ctx, id, "w-settle", wf.Generation, "boom", "E_BOOM", "op", nil); err != nil {
			t.Fatalf("FailWorkflow: %v", err)
		}
	case "dead_lettered":
		wf := claim()
		if err := store.MoveToDeadLetterQueue(ctx, id, "w-settle", wf.Generation, "dead", "E_DEAD", "op", nil); err != nil {
			t.Fatalf("MoveToDeadLetterQueue: %v", err)
		}
	case "terminated":
		if err := store.TerminateWorkflow(ctx, id, "terminated by test"); err != nil {
			t.Fatalf("TerminateWorkflow: %v", err)
		}
	case "cancelled":
		if err := store.CancelWorkflow(ctx, id, "cancelled by test"); err != nil {
			t.Fatalf("CancelWorkflow: %v", err)
		}
	default:
		t.Fatalf("settleRun: unknown way %q", how)
	}
	return id
}

func TestASignalToARunThatHasClosedForGoodIsRefusedAndStoresNothing(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "settled-signal-parent")
			idem, ok := store.(SignalIdempotencyStore)
			if !ok {
				t.Fatalf("%T is not a SignalIdempotencyStore", store)
			}

			// CONTROL, and it comes first: a signal to an OPEN run is stored and readable, so "nothing stored" below cannot be what a probe that sees no signal at all would also say.
			open := settleRunOpen(t, ctx, store, parent)
			if err := store.DeliverSignal(ctx, open, "ping", `"x"`); err != nil {
				t.Fatalf("CONTROL: DeliverSignal to an open run = %v", err)
			}
			if _, found, err := store.PollSignal(ctx, open, "ping"); err != nil || !found {
				t.Fatalf("CONTROL: a signal delivered to an open run is not readable on it (found=%v err=%v)", found, err)
			}

			for _, how := range []string{"done", "cancelled"} {
				how := how
				t.Run(how, func(t *testing.T) {
					id := settleRun(t, ctx, store, parent, how)

					for _, path := range []string{"DeliverSignal", "DeliverSignalIdempotent"} {
						var err error
						var replay bool
						if path == "DeliverSignal" {
							err = store.DeliverSignal(ctx, id, "ping", `"x"`)
						} else {
							replay, err = idem.DeliverSignalIdempotent(ctx, id, "ping", `"x"`, "key-"+how)
						}
						var ne *WorkflowNotOpenError
						if !errors.As(err, &ne) {
							t.Fatalf("%s to a %s run = %v, want a *WorkflowNotOpenError: the signal was accepted and would be read by no one", path, how, err)
						}
						if !errors.Is(err, ErrWorkflowNotOpen) || errors.Is(err, ErrWorkflowNotFound) {
							t.Errorf("%s: errors.Is(NotOpen)=%v errors.Is(NotFound)=%v, want true and false", path, errors.Is(err, ErrWorkflowNotOpen), errors.Is(err, ErrWorkflowNotFound))
						}
						if ne.WorkflowID != id || ne.Status != how {
							t.Errorf("%s: error names (%q, %q), want (%q, %q)", path, ne.WorkflowID, ne.Status, id, how)
						}
						if ne.ContinuedTo != "" {
							t.Errorf("%s: a run that settled as %s names a successor %q: it did not continue", path, how, ne.ContinuedTo)
						}
						if replay {
							t.Errorf("%s reported replay=true for a refused delivery", path)
						}
						if _, found, err := store.PollSignal(ctx, id, "ping"); err != nil || found {
							t.Fatalf("%s to a %s run returned an error AND stored the signal (found=%v err=%v): the refusal must write nothing", path, how, found, err)
						}
					}

					// The refused keyed delivery must not have SPENT its token: the same key sent to an open run is a first delivery, not a replay.
					replay, err := idem.DeliverSignalIdempotent(ctx, open, "again", `"y"`, "key-"+how)
					if err != nil || replay {
						t.Errorf("after a refused delivery the same key to an open run gave (replay=%v, err=%v), want a fresh delivery: the refusal spent the idempotency key", replay, err)
					}
				})
			}
			// A run that CONTINUED AS NEW used to be refused here, naming the end of its chain. It is no longer refused: a signal addressed to it is delivered to that end (cleat#4340,
			// TestASignalToARunThatContinuedAsNewIsDeliveredToTheChainsCurrentRun), and a chain whose end is done or cancelled is refused by TestASignalToAChainThatEndedIsRefusedOrStoredByHowItEnded.
		})
	}
}

// settleRunOpen starts a child and leaves it open.
func settleRunOpen(t *testing.T, ctx context.Context, store WorkflowStore, parent string) string {
	t.Helper()
	id, err := store.(interface {
		StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error)
	}).StartChildWorkflow(ctx, parent, "intent-workflow", `{}`, 1, "TERMINATE", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}
	return id
}

// THE EXISTENCE ORACLE. NotOpen names a run's state, so it must be reachable ONLY for a run the caller's own tenant can see. A SETTLED run in another tenant has to read exactly as a missing one
// does (cleat#2218, cleat#2227): ErrWorkflowNotFound, never NotOpen, or the new error tells a caller which of another tenant's runs have finished.
func TestASignalToAnotherTenantsSettledRunStaysNotFound(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			mt, ok := backend.(MultiTenantStoreBackend)
			if !ok {
				t.Fatalf("%s backend does not implement MultiTenantStoreBackend", backend.Name())
			}
			const tenantA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			const tenantB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
			storeA, teardownA := mt.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := mt.SetupForTenant(t, tenantB)
			defer teardownB()
			ctx := context.Background()
			// The definition and the run live in tenant A's own store, as in a_signal_to_an_id_the_caller_cannot_touch_writes_nothing_test.go: a run under another tenant cannot reference the default tenant's.
			def := &WorkflowDef{Name: "settled-signal-oracle", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1}
			if err := storeA.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store A: %v", err)
			}
			settledA, _, err := storeA.StartNewRun(ctx, "", def.Name, 1, json.RawMessage(`{}`), "settled-oracle-a-1", tenantA, 0)
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}
			wfA, err := claimSpecific(t, ctx, storeA, settledA, "w-oracle")
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if err := storeA.CompleteWorkflow(ctx, settledA, "w-oracle", wfA.Generation, `{"r":1}`, nil); err != nil {
				t.Fatalf("CompleteWorkflow: %v", err)
			}

			// CONTROL: tenant A, the owner, IS told the run is not open, so the foreign result below is a different answer and not the same one.
			if err := storeA.DeliverSignal(ctx, settledA, "ping", `"x"`); !errors.Is(err, ErrWorkflowNotOpen) {
				t.Fatalf("CONTROL: the owner's DeliverSignal to its own settled run = %v, want ErrWorkflowNotOpen", err)
			}
			err = storeB.DeliverSignal(ctx, settledA, "ping", `"x"`)
			if !errors.Is(err, ErrWorkflowNotFound) || errors.Is(err, ErrWorkflowNotOpen) {
				t.Fatalf("tenant B's DeliverSignal to tenant A's SETTLED run = %v, want ErrWorkflowNotFound and NOT NotOpen: anything else tells B which of A's runs have finished", err)
			}
			if idem, ok := storeB.(SignalIdempotencyStore); ok {
				_, err := idem.DeliverSignalIdempotent(ctx, settledA, "ping", `"x"`, "oracle-key")
				if !errors.Is(err, ErrWorkflowNotFound) || errors.Is(err, ErrWorkflowNotOpen) {
					t.Fatalf("tenant B's DeliverSignalIdempotent to tenant A's settled run = %v, want ErrWorkflowNotFound and NOT NotOpen", err)
				}
			}
		})
	}
}

// The other side of the rule. A signal to a run that is settled but REDRIVABLE is accepted and stored, as before, and is read after the redrive: that is the pattern the owner has not decided to remove.
func TestASignalToARedrivableRunIsAcceptedAndReadAfterTheRedrive(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "redrivable-signal-parent")
			idem, ok := store.(SignalIdempotencyStore)
			if !ok {
				t.Fatalf("%T is not a SignalIdempotencyStore", store)
			}
			for _, how := range []string{"failed", "terminated", "dead_lettered"} {
				how := how
				t.Run("accepted and stored: "+how, func(t *testing.T) {
					if !IsRedrivableStatus(how) {
						t.Fatalf("%s is not redrivable by the engine's own list (reReplayableStatuses): this test's premise is stale", how)
					}
					id := settleRun(t, ctx, store, parent, how)
					if err := store.DeliverSignal(ctx, id, "ping", `"plain"`); err != nil {
						t.Fatalf("DeliverSignal to a %s run = %v, want it accepted (cleat#4345 decides whether that stays)", how, err)
					}
					if _, found, err := store.PollSignal(ctx, id, "ping"); err != nil || !found {
						t.Fatalf("the signal to a %s run was accepted but is not stored (found=%v err=%v)", how, found, err)
					}
					replay, err := idem.DeliverSignalIdempotent(ctx, id, "keyed", `"k"`, "redrivable-"+how)
					if err != nil || replay {
						t.Fatalf("DeliverSignalIdempotent to a %s run = (replay=%v, %v), want a first delivery", how, replay, err)
					}
					if _, found, _ := store.PollSignal(ctx, id, "keyed"); !found {
						t.Errorf("the keyed signal to a %s run is not stored", how)
					}
				})
			}

			// THE PIN, in the words of the ruling: a signal before a retry is read after the retry.
			t.Run("a signal sent before a retry is read after the retry", func(t *testing.T) {
				id := settleRun(t, ctx, store, parent, "dead_lettered")
				if err := store.DeliverSignal(ctx, id, "before-retry", `"sent-while-dead-lettered"`); err != nil {
					t.Fatalf("DeliverSignal to the dead-lettered run = %v", err)
				}
				if err := store.RetryWorkflow(ctx, id); err != nil {
					t.Fatalf("RetryWorkflow: %v", err)
				}
				d, found, err := store.PollSignal(ctx, id, "before-retry")
				if err != nil || !found {
					t.Fatalf("after the retry the signal sent before it is not there (found=%v err=%v): the redrive lost it", found, err)
				}
				if !strings.Contains(d.Payload, "sent-while-dead-lettered") {
					t.Errorf("the signal read after the retry carries %q, want the payload that was sent before it", d.Payload)
				}
				// and the run is open again, so a signal now is accepted exactly as for any open run
				if err := store.DeliverSignal(ctx, id, "after-retry", `"x"`); err != nil {
					t.Errorf("DeliverSignal after the retry = %v, want it accepted", err)
				}
			})
		})
	}
}
