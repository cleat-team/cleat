package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// cleat#4340, the second half of what #4379 did for signals. A cancel and a terminate are addressed to a WORKFLOW, and a workflow that continues as new is still the same workflow
// to the one who cancels it: the verb acts on the run that is live, exactly once, whatever continue-as-new did.
//
//  1. a pre-emptive cancel, a terminate and a cooperative cancel sent to ANY run of a chain act on the chain's CURRENT run, and no earlier run is touched;
//  2. a cooperative cancel requested on a run that then continues is CARRIED to the successor (a successor created without it is the cancel that was lost);
//  3. a chain that ENDED answers as it always did (a settled run refuses a pre-emptive verb), now naming the run that refused;
//  4. a verb that races the continue's commit lands on exactly one run, and never leaves a successor that is open after an acknowledged cancel.

// statusOf reads a run's status through the tenant-scoped store.
func statusOf(t *testing.T, ctx context.Context, store WorkflowStore, id string) string {
	t.Helper()
	wf, err := store.GetWorkflowByID(ctx, id)
	if err != nil || wf == nil {
		t.Fatalf("GetWorkflowByID(%s) = (%v, %v)", id, wf, err)
	}
	return wf.Status
}

// settleVerb is one of the two pre-emptive verbs, with the status it must leave and the receipt method that says where it went.
type settleVerb struct {
	name   string
	status string
	do     func(ctx context.Context, store WorkflowStore, id, reason string) error
	// receipt is nil when the store has no receipt form.
	receipt func(ctx context.Context, store WorkflowStore, id, reason string) (SettleReceipt, error)
}

var settleVerbs = []settleVerb{
	{
		name: "cancel", status: statusCancelled,
		do: func(ctx context.Context, s WorkflowStore, id, reason string) error {
			return s.CancelWorkflow(ctx, id, reason)
		},
		receipt: func(ctx context.Context, s WorkflowStore, id, reason string) (SettleReceipt, error) {
			return s.(SettleReceiptStore).CancelWorkflowReceipt(ctx, id, reason)
		},
	},
	{
		name: "terminate", status: statusTerminated,
		do: func(ctx context.Context, s WorkflowStore, id, reason string) error {
			return s.TerminateWorkflow(ctx, id, reason)
		},
		receipt: func(ctx context.Context, s WorkflowStore, id, reason string) (SettleReceipt, error) {
			return s.(SettleReceiptStore).TerminateWorkflowReceipt(ctx, id, reason)
		},
	},
}

// 1. FORWARDING, pre-emptive. Every run of a chain accepts the verb and the CURRENT run is the one that is settled; the runs that already continued are left as they were (`done`).
// The control first: the verb sent straight to the live run of a chain settles it, so "the old runs are untouched" below cannot be a verb that settles nothing at all.
func TestACancelOrTerminateToARunThatContinuedAsNewActsOnTheChainsCurrentRun(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "settle-forward-parent")
			if _, ok := store.(SettleReceiptStore); !ok {
				t.Fatalf("%T is not a SettleReceiptStore", store)
			}

			for _, verb := range settleVerbs {
				verb := verb
				t.Run(verb.name, func(t *testing.T) {
					// CONTROL: straight to the live run of a chain of three.
					c1 := settleRunOpen(t, ctx, store, parent)
					c2 := continueChain(t, ctx, store, c1)
					c3 := continueChain(t, ctx, store, c2)
					if err := verb.do(ctx, store, c3, "control"); err != nil {
						t.Fatalf("CONTROL: %s to the live run = %v", verb.name, err)
					}
					if got := statusOf(t, ctx, store, c3); got != verb.status {
						t.Fatalf("CONTROL: the live run is %q after %s, want %q", got, verb.name, verb.status)
					}

					for _, addressed := range []string{"first of three", "second of three"} {
						addressed := addressed
						t.Run(addressed, func(t *testing.T) {
							first := settleRunOpen(t, ctx, store, parent)
							second := continueChain(t, ctx, store, first)
							third := continueChain(t, ctx, store, second)
							target := first
							if addressed == "second of three" {
								target = second
							}

							rec, err := verb.receipt(ctx, store, target, "from the old id")
							if err != nil {
								t.Fatalf("%s to %s = %v, want it accepted and applied to the chain's current run", verb.name, addressed, err)
							}
							if rec.SettledRun != third || !rec.Forwarded {
								t.Errorf("receipt = %+v, want SettledRun %s and Forwarded", rec, third)
							}
							if got := statusOf(t, ctx, store, third); got != verb.status {
								t.Errorf("the chain's current run is %q, want %q", got, verb.status)
							}
							for _, old := range []string{first, second} {
								if got := statusOf(t, ctx, store, old); got != statusDone {
									t.Errorf("run %s, which already continued, is now %q: a verb addressed to the chain touched it", old, got)
								}
							}
							// Addressed to the live run itself, the receipt says it was not forwarded.
							f2 := settleRunOpen(t, ctx, store, parent)
							rec, err = verb.receipt(ctx, store, f2, "direct")
							if err != nil || rec.Forwarded || rec.SettledRun != f2 {
								t.Errorf("a direct %s: receipt %+v err %v, want SettledRun %s and not forwarded", verb.name, rec, err, f2)
							}
						})
					}
				})
			}
		})
	}
}

// 1b. FORWARDING, cooperative. The flag is set on the CURRENT run, and on no earlier one.
func TestACooperativeCancelToARunThatContinuedAsNewFlagsTheChainsCurrentRun(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "coop-forward-parent")

			first := settleRunOpen(t, ctx, store, parent)
			second := continueChain(t, ctx, store, first)
			third := continueChain(t, ctx, store, second)

			rec, err := store.(SettleReceiptStore).RequestCancellationReceipt(ctx, first, "please stop")
			if err != nil {
				t.Fatalf("RequestCancellation to the first of three = %v", err)
			}
			if rec.SettledRun != third || !rec.Forwarded {
				t.Errorf("receipt = %+v, want SettledRun %s and Forwarded", rec, third)
			}
			if flagged, reason, err := store.CheckCancellation(ctx, third); err != nil || !flagged || reason != "please stop" {
				t.Errorf("the current run: (flagged=%v, reason=%q, err=%v), want (true, \"please stop\", nil)", flagged, reason, err)
			}
			for _, old := range []string{first, second} {
				if flagged, _, err := store.CheckCancellation(ctx, old); err != nil || flagged {
					t.Errorf("run %s, which already continued: (flagged=%v, err=%v), want it untouched", old, flagged, err)
				}
			}
		})
	}
}

// 2. CARRY-OVER. A cooperative cancel requested on a run that is still open and then continues must not be lost in the gap: the guest may never have looked at the flag, and a
// successor created without it is a cancel that was acknowledged and then forgotten.
func TestACooperativeCancelRequestedBeforeAContinueAsNewIsCarriedToTheSuccessor(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "coop-carry-parent")

			// CONTROL: with no request pending, the successor is not flagged, so a flag on it below is the carry-over and not a default.
			plain := settleRunOpen(t, ctx, store, parent)
			plainNext := continueChain(t, ctx, store, plain)
			if flagged, _, err := store.CheckCancellation(ctx, plainNext); err != nil || flagged {
				t.Fatalf("CONTROL: a successor with no request pending is flagged=%v err=%v", flagged, err)
			}

			first := settleRunOpen(t, ctx, store, parent)
			if err := store.RequestCancellation(ctx, first, "stop before you continue"); err != nil {
				t.Fatalf("RequestCancellation: %v", err)
			}
			second := continueChain(t, ctx, store, first)
			if flagged, reason, err := store.CheckCancellation(ctx, second); err != nil || !flagged || reason != "stop before you continue" {
				t.Errorf("the successor: (flagged=%v, reason=%q, err=%v), want the request carried with its reason", flagged, reason, err)
			}
		})
	}
}

// 3. THE END OF THE CHAIN decides. A chain that ended for good refuses a pre-emptive verb as before, naming the run that refused; one whose current run can be redriven is acted on.
func TestACancelOrTerminateToAChainThatEndedAnswersByHowItEnded(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "settle-ended-parent")

			for _, verb := range settleVerbs {
				verb := verb
				t.Run(verb.name+": the current run finished", func(t *testing.T) {
					first := settleRunOpen(t, ctx, store, parent)
					second := continueChain(t, ctx, store, first)
					wf, err := claimSpecific(t, ctx, store, second, "w-end")
					if err != nil {
						t.Fatalf("claim: %v", err)
					}
					if err := store.CompleteWorkflow(ctx, second, "w-end", wf.Generation, `{"r":1}`, nil); err != nil {
						t.Fatalf("CompleteWorkflow: %v", err)
					}
					err = verb.do(ctx, store, first, "too late")
					if !errors.Is(err, ErrAdminStateConflict) {
						t.Fatalf("%s to a chain whose end is done = %v, want ErrAdminStateConflict", verb.name, err)
					}
					for _, want := range []string{first, second, "status=done"} {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("the refusal %q does not name %q: it must say which run refused and why", err, want)
						}
					}
					if got := statusOf(t, ctx, store, second); got != statusDone {
						t.Errorf("a refused %s changed the end to %q", verb.name, got)
					}
				})
			}

			t.Run("terminate: the current run is dead-lettered, which terminate exists to take off the queue", func(t *testing.T) {
				first := settleRunOpen(t, ctx, store, parent)
				second := continueChain(t, ctx, store, first)
				wf, err := claimSpecific(t, ctx, store, second, "w-dl")
				if err != nil {
					t.Fatalf("claim: %v", err)
				}
				if err := store.MoveToDeadLetterQueue(ctx, second, "w-dl", wf.Generation, "gave up", "E_GAVE_UP", "op", nil); err != nil {
					t.Fatalf("MoveToDeadLetterQueue: %v", err)
				}
				if err := store.TerminateWorkflow(ctx, first, "take it off the queue"); err != nil {
					t.Fatalf("TerminateWorkflow to a chain whose end is dead-lettered = %v, want it accepted (dead_lettered -> terminated is the one exception to 'settled is final')", err)
				}
				if got := statusOf(t, ctx, store, second); got != statusTerminated {
					t.Errorf("the dead-lettered end is %q, want terminated", got)
				}
			})

			t.Run("cancel: the current run is already cancelled", func(t *testing.T) {
				first := settleRunOpen(t, ctx, store, parent)
				second := continueChain(t, ctx, store, first)
				if err := store.CancelWorkflow(ctx, first, "once"); err != nil {
					t.Fatalf("the first cancel: %v", err)
				}
				err := store.CancelWorkflow(ctx, first, "twice")
				if !errors.Is(err, ErrAdminStateConflict) || !strings.Contains(err.Error(), second) {
					t.Errorf("a second cancel = %v, want ErrAdminStateConflict naming %s", err, second)
				}
			})

			t.Run("an unknown run is still not found", func(t *testing.T) {
				if err := store.CancelWorkflow(ctx, "no-such-run", "x"); !errors.Is(err, ErrWorkflowNotFound) {
					t.Errorf("CancelWorkflow(unknown) = %v, want ErrWorkflowNotFound", err)
				}
			})
		})
	}
}

// 4. THE RACE, pre-emptive. Several cancels address the OLD id while its continue-as-new commits. Exactly one outcome per round: either a cancel wins the old run's lease (it settles
// it and bumps its generation, so the continue's completion update finds no row and creates NO successor), or the continue commits first (the old run is `done` and every cancel
// walks to the successor and settles THAT, once). What must never happen is a successor that is OPEN after a cancel was acknowledged, a cancel acknowledged twice, or both runs
// settled. Sixteen rounds because the window is milliseconds wide (cleat#1755: one round of eight caught its race in about 4 runs of 10, six rounds in 10 of 10).
func TestACancelRacingAContinueAsNewSettlesExactlyOneRunAndLeavesNoOpenSuccessor(t *testing.T) {
	const rounds, senders = 16, 4
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "settle-race-parent")
			succ, ok := store.(interface {
				successorOfRun(context.Context, string) (string, error)
			})
			if !ok {
				t.Fatalf("%T has no successorOfRun", store)
			}

			wonByCancel, wonByContinue := 0, 0
			for round := 0; round < rounds; round++ {
				first := settleRunOpen(t, ctx, store, parent)
				wf, err := claimSpecific(t, ctx, store, first, "w-race")
				if err != nil {
					t.Fatalf("round %d: claim: %v", round, err)
				}
				start := make(chan struct{})
				var wg sync.WaitGroup
				sendErr := make([]error, senders)
				for s := 0; s < senders; s++ {
					s := s
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						sendErr[s] = store.CancelWorkflow(ctx, first, fmt.Sprintf("r%d-s%d", round, s))
					}()
				}
				var second string
				var contErr error
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					second, contErr = store.ContinueAsNew(ctx, first, "w-race", wf.Generation, "intent-workflow", 1, json.RawMessage(`{}`), nil, "", map[string]string{}, 0)
				}()
				close(start)
				wg.Wait()

				acked := 0
				for s, e := range sendErr {
					switch {
					case e == nil:
						acked++
					case errors.Is(e, ErrAdminStateConflict):
						// a later cancel meeting a run that is already settled: the verb is not applied twice
					default:
						t.Errorf("round %d sender %d: %v, want nil or ErrAdminStateConflict", round, s, e)
					}
				}
				if acked != 1 {
					t.Errorf("round %d: %d cancels were acknowledged, want exactly 1 (a cancel applied twice, or none)", round, acked)
				}

				next, err := succ.successorOfRun(ctx, first)
				if err != nil {
					t.Fatalf("round %d: successorOfRun: %v", round, err)
				}
				firstStatus := statusOf(t, ctx, store, first)
				switch {
				case contErr == nil:
					wonByContinue++
					if next != second || firstStatus != statusDone {
						t.Errorf("round %d: the continue committed, but first is %q and successor %q (returned %q)", round, firstStatus, next, second)
						continue
					}
					if got := statusOf(t, ctx, store, second); got != statusCancelled {
						t.Errorf("round %d: the continue committed first and the cancel was acknowledged, but the successor is %q: an OPEN successor after an acknowledged cancel", round, got)
					}
				default:
					wonByCancel++
					if !errors.Is(contErr, ErrFenceLost) {
						t.Errorf("round %d: the continue failed with %v, want ErrFenceLost (the cancel took its lease)", round, contErr)
					}
					if next != "" {
						t.Errorf("round %d: a cancel won the old run, but a successor %s exists", round, next)
					}
					if firstStatus != statusCancelled {
						t.Errorf("round %d: the continue lost, but the old run is %q, want cancelled", round, firstStatus)
					}
				}
			}
			t.Logf("%d rounds: the cancel won the lease in %d, the continue in %d", rounds, wonByCancel, wonByContinue)
		})
	}
}

// 4b. THE RACE, cooperative. Requests address the OLD id while its continue commits. The continue never fails for want of a flag (a request does not take the lease), so every round
// commits a successor, and an acknowledged request must be on it: either set before the commit and carried, or after and forwarded.
func TestACooperativeCancelRacingAContinueAsNewAlwaysReachesTheSuccessor(t *testing.T) {
	const rounds, senders = 16, 4
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "coop-race-parent")

			lost := 0
			for round := 0; round < rounds; round++ {
				first := settleRunOpen(t, ctx, store, parent)
				wf, err := claimSpecific(t, ctx, store, first, "w-race")
				if err != nil {
					t.Fatalf("round %d: claim: %v", round, err)
				}
				start := make(chan struct{})
				var wg sync.WaitGroup
				sendErr := make([]error, senders)
				for s := 0; s < senders; s++ {
					s := s
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						sendErr[s] = store.RequestCancellation(ctx, first, "r")
					}()
				}
				var second string
				var contErr error
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					second, contErr = store.ContinueAsNew(ctx, first, "w-race", wf.Generation, "intent-workflow", 1, json.RawMessage(`{}`), nil, "", map[string]string{}, 0)
				}()
				close(start)
				wg.Wait()
				if contErr != nil {
					t.Fatalf("round %d: ContinueAsNew: %v (a cooperative request must not stop a continue)", round, contErr)
				}
				for s, e := range sendErr {
					if e != nil {
						t.Errorf("round %d sender %d: %v, want the request accepted", round, s, e)
					}
				}
				if flagged, _, err := store.CheckCancellation(ctx, second); err != nil || !flagged {
					lost++
					t.Errorf("round %d: %d requests were acknowledged and the successor is not flagged (flagged=%v err=%v): the cancel was lost", round, senders, flagged, err)
				}
			}
			if lost != 0 {
				t.Errorf("over %d rounds, %d successors were created un-cancelled after an acknowledged request", rounds, lost)
			}
		})
	}
}
