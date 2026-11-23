package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// cleat#4340. A signal is addressed to a WORKFLOW, and a workflow that continues as new is still the same workflow to its sender: the signal is delivered exactly once to whichever run is
// live, whatever continue-as-new did. Until #4343 a signal to a continued run was accepted and read by no one; #4343 refused it with the successor's id; this is the design's own answer, which is
// to deliver it there.
//
// Three promises, each with its own test, and a fourth that is the reason the others are not enough:
//  1. a signal sent to ANY run of a chain is stored on the chain's CURRENT run;
//  2. a signal already stored on a run that then continues is CARRIED to the successor;
//  3. a chain that ENDED (done, cancelled) still refuses, and one that ended redrivable stores;
//  4. a signal that races the continue's commit lands on exactly one run, whichever side of the commit it falls.

// continueChain claims cur and continues it as new, returning the successor.
func continueChain(t *testing.T, ctx context.Context, store WorkflowStore, cur string) string {
	t.Helper()
	wf, err := claimSpecific(t, ctx, store, cur, "w-chain")
	if err != nil {
		t.Fatalf("claim %s: %v", cur, err)
	}
	next, err := store.ContinueAsNew(ctx, cur, "w-chain", wf.Generation, "intent-workflow", 1, json.RawMessage(`{}`), nil, "", map[string]string{}, 0)
	if err != nil {
		t.Fatalf("ContinueAsNew %s: %v", cur, err)
	}
	return next
}

// drainSignalPayloads reads and consumes every stored delivery of one name on a run, as a run's await path does, and returns their payloads.
func drainSignalPayloads(t *testing.T, ctx context.Context, store WorkflowStore, run, name string) []string {
	t.Helper()
	var got []string
	for i := 0; i < 10000; i++ {
		d, found, err := store.PollSignal(ctx, run, name)
		if err != nil {
			t.Fatalf("PollSignal %s: %v", run, err)
		}
		if !found {
			return got
		}
		got = append(got, d.Payload)
		if err := store.ConsumeSignal(ctx, run, d.ID); err != nil {
			t.Fatalf("ConsumeSignal %s/%d: %v", run, d.ID, err)
		}
	}
	t.Fatalf("drainSignalPayloads(%s) did not terminate", run)
	return nil
}

// 1. FORWARDING. Every run of a chain accepts a signal and the CURRENT run reads it; no earlier run does. The control comes first: a signal sent straight to the live run reads there, so "nothing on
// the old runs" below cannot be what a probe that sees no signal at all would also say.
func TestASignalToARunThatContinuedAsNewIsDeliveredToTheChainsCurrentRun(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "forward-parent")
			idem, ok := store.(SignalIdempotencyStore)
			if !ok {
				t.Fatalf("%T is not a SignalIdempotencyStore", store)
			}

			first := settleRunOpen(t, ctx, store, parent)
			second := continueChain(t, ctx, store, first)
			third := continueChain(t, ctx, store, second)

			if err := store.DeliverSignal(ctx, third, "ping", `"control"`); err != nil {
				t.Fatalf("CONTROL: DeliverSignal to the live run = %v", err)
			}
			if got := drainSignalPayloads(t, ctx, store, third, "ping"); len(got) != 1 || got[0] != `control` {
				t.Fatalf("CONTROL: the live run read %v, want [control]", got)
			}

			for _, addressed := range []struct{ name, id string }{{"first of three", first}, {"second of three", second}} {
				addressed := addressed
				t.Run(addressed.name, func(t *testing.T) {
					if err := store.DeliverSignal(ctx, addressed.id, "ping", `"plain"`); err != nil {
						t.Fatalf("DeliverSignal to %s = %v, want it accepted and delivered to the chain's current run", addressed.name, err)
					}
					replay, err := idem.DeliverSignalIdempotent(ctx, addressed.id, "ping", `"keyed"`, "fwd-key-"+addressed.name)
					if err != nil || replay {
						t.Fatalf("DeliverSignalIdempotent to %s = (replay=%v, err=%v), want a fresh delivery", addressed.name, replay, err)
					}
					// The same key again is a replay, and delivers nothing: forwarding must not make a retry deliver twice.
					replay, err = idem.DeliverSignalIdempotent(ctx, addressed.id, "ping", `"keyed"`, "fwd-key-"+addressed.name)
					if err != nil || !replay {
						t.Errorf("the same key again = (replay=%v, err=%v), want (true, nil)", replay, err)
					}
					for _, old := range []string{first, second} {
						if got := drainSignalPayloads(t, ctx, store, old, "ping"); len(got) != 0 {
							t.Errorf("run %s, which already continued, holds %v: a signal addressed to the chain was stored where nothing reads it", old, got)
						}
					}
					got := drainSignalPayloads(t, ctx, store, third, "ping")
					sort.Strings(got)
					if len(got) != 2 || got[0] != `keyed` || got[1] != `plain` {
						t.Errorf("the chain's current run read %v, want exactly [keyed plain] (once each)", got)
					}
				})
			}
		})
	}
}

// 2. CARRY-OVER. A signal delivered to a run that is still open and then continues must not be lost in the gap: it was answered "delivered", and the successor is the run that will read it.
func TestASignalStoredBeforeAContinueAsNewIsCarriedToTheSuccessor(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "carry-parent")

			first := settleRunOpen(t, ctx, store, parent)
			for i := 0; i < 3; i++ {
				if err := store.DeliverSignal(ctx, first, "ping", fmt.Sprintf(`"n%d"`, i)); err != nil {
					t.Fatalf("DeliverSignal %d: %v", i, err)
				}
			}
			if err := store.DeliverSignal(ctx, first, "other", `"o"`); err != nil {
				t.Fatalf("DeliverSignal other: %v", err)
			}
			second := continueChain(t, ctx, store, first)

			if got := drainSignalPayloads(t, ctx, store, first, "ping"); len(got) != 0 {
				t.Errorf("the run that continued still holds %v", got)
			}
			got := drainSignalPayloads(t, ctx, store, second, "ping")
			if len(got) != 3 || got[0] != "n0" || got[1] != "n1" || got[2] != "n2" {
				t.Errorf("the successor read %v, want [n0 n1 n2] in the order they were sent", got)
			}
			if got := drainSignalPayloads(t, ctx, store, second, "other"); len(got) != 1 || got[0] != "o" {
				t.Errorf("the successor read %v of the other name, want [o]", got)
			}
		})
	}
}

// 3. THE END OF THE CHAIN decides. A chain that ended for good refuses as before, naming where the work went; one whose current run can be reopened stores, as a redrivable run always did.
func TestASignalToAChainThatEndedIsRefusedOrStoredByHowItEnded(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "ended-parent")

			t.Run("the current run finished: refused, naming the end", func(t *testing.T) {
				first := settleRunOpen(t, ctx, store, parent)
				second := continueChain(t, ctx, store, first)
				wf, err := claimSpecific(t, ctx, store, second, "w-end")
				if err != nil {
					t.Fatalf("claim: %v", err)
				}
				if err := store.CompleteWorkflow(ctx, second, "w-end", wf.Generation, `{"r":1}`, nil); err != nil {
					t.Fatalf("CompleteWorkflow: %v", err)
				}
				err = store.DeliverSignal(ctx, first, "ping", `"x"`)
				var ne *WorkflowNotOpenError
				if !errors.As(err, &ne) {
					t.Fatalf("DeliverSignal to a chain whose end is done = %v, want a *WorkflowNotOpenError", err)
				}
				if ne.Status != statusDone || ne.ContinuedTo != second {
					t.Errorf("the refusal names (status %q, continued_to %q), want (done, %q): the chain's END and its state", ne.Status, ne.ContinuedTo, second)
				}
				for _, id := range []string{first, second} {
					if got := drainSignalPayloads(t, ctx, store, id, "ping"); len(got) != 0 {
						t.Errorf("a refused delivery stored %v on %s", got, id)
					}
				}
			})

			t.Run("the current run failed: stored, as for any redrivable run", func(t *testing.T) {
				first := settleRunOpen(t, ctx, store, parent)
				second := continueChain(t, ctx, store, first)
				wf, err := claimSpecific(t, ctx, store, second, "w-end")
				if err != nil {
					t.Fatalf("claim: %v", err)
				}
				if err := store.FailWorkflow(ctx, second, "w-end", wf.Generation, "boom", "E_BOOM", "op", nil); err != nil {
					t.Fatalf("FailWorkflow: %v", err)
				}
				if err := store.DeliverSignal(ctx, first, "ping", `"after-failure"`); err != nil {
					t.Fatalf("DeliverSignal to a chain whose end FAILED = %v, want it accepted (a failed run is redrivable and keeps its signals)", err)
				}
				if got := drainSignalPayloads(t, ctx, store, second, "ping"); len(got) != 1 || got[0] != "after-failure" {
					t.Errorf("the failed run holds %v, want [after-failure]", got)
				}
				if got := drainSignalPayloads(t, ctx, store, first, "ping"); len(got) != 0 {
					t.Errorf("the run that continued holds %v", got)
				}
			})
		})
	}
}

// 4. THE RACE. Senders signal the OLD id while its continue-as-new commits. Each signal lands either before the commit (and is carried) or after it (and is forwarded); what must never happen is
// that it lands on the old run after the carry-over has run, where nothing reads it, or that it is read twice. Every acknowledged signal is read exactly once on the chain's current run, over many
// rounds because the window is milliseconds wide: cleat#1755's race was caught by one round of eight senders in about 4 runs of 10 and by six rounds in 10 of 10.
func TestAnAcknowledgedSignalRacingAContinueAsNewIsReadExactlyOnce(t *testing.T) {
	const rounds, senders = 16, 6
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "race-parent")

			stranded, duplicated, lost, errs := 0, 0, 0, 0
			for round := 0; round < rounds; round++ {
				first := settleRunOpen(t, ctx, store, parent)
				wf, err := claimSpecific(t, ctx, store, first, "w-race")
				if err != nil {
					t.Fatalf("round %d: claim: %v", round, err)
				}

				start := make(chan struct{})
				var wg sync.WaitGroup
				acked := make([]bool, senders)
				sendErr := make([]error, senders)
				for s := 0; s < senders; s++ {
					s := s
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						sendErr[s] = store.DeliverSignal(ctx, first, "ping", fmt.Sprintf(`"r%d-s%d"`, round, s))
						acked[s] = sendErr[s] == nil
					}()
				}
				var second string
				var canErr error
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					second, canErr = store.ContinueAsNew(ctx, first, "w-race", wf.Generation, "intent-workflow", 1, json.RawMessage(`{}`), nil, "", map[string]string{}, 0)
				}()
				close(start)
				wg.Wait()
				if canErr != nil {
					t.Fatalf("round %d: ContinueAsNew: %v", round, canErr)
				}

				want := map[string]bool{}
				for s := 0; s < senders; s++ {
					if sendErr[s] != nil {
						errs++
						t.Errorf("round %d sender %d: %v (a signal to a chain whose current run is open must be accepted)", round, s, sendErr[s])
						continue
					}
					want[fmt.Sprintf("r%d-s%d", round, s)] = true
				}
				onOld := drainSignalPayloads(t, ctx, store, first, "ping")
				onNew := drainSignalPayloads(t, ctx, store, second, "ping")
				stranded += len(onOld)
				seen := map[string]int{}
				for _, p := range onNew {
					seen[p]++
				}
				for _, p := range onOld {
					seen[p]++
				}
				for p, n := range seen {
					if n > 1 {
						duplicated++
						t.Errorf("round %d: %q was read %d times", round, p, n)
					}
				}
				for p := range want {
					if seen[p] == 0 {
						lost++
					}
				}
				if len(onOld) != 0 {
					t.Errorf("round %d: %d acknowledged signal(s) are stored on the OLD run %s, which has continued and is read by no one: %v", round, len(onOld), first, onOld)
				}
			}
			if stranded+duplicated+lost+errs != 0 {
				t.Errorf("over %d rounds of %d senders: %d stranded on the old run, %d read twice, %d lost, %d refused", rounds, senders, stranded, duplicated, lost, errs)
			}
		})
	}
}

// 5. THE KEY FOLLOWS THE SIGNAL. An idempotency key is deleted with the run it was recorded against when that run is swept. A signal forwarded to the current run is held THERE, so its key must be recorded
// against that run: recorded against the addressed one, which is done and sweepable, the sweep would delete the key while the signal still sits on a live run, and a retry under it would deliver the signal
// a second time. Observable without reading the table: after the sweep, the same key presented to the run that holds the signal is still a replay.
func TestAKeyedSignalForwardedToTheCurrentRunSurvivesTheSweepOfTheRunItWasAddressedTo(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "key-sweep-parent")
			idem, ok := store.(SignalIdempotencyStore)
			if !ok {
				t.Fatalf("%T is not a SignalIdempotencyStore", store)
			}

			first := settleRunOpen(t, ctx, store, parent)
			second := continueChain(t, ctx, store, first)
			replay, err := idem.DeliverSignalIdempotent(ctx, first, "ping", `"once"`, "sweep-key")
			if err != nil || replay {
				t.Fatalf("the keyed delivery = (replay=%v, err=%v), want a first delivery", replay, err)
			}

			// The sweep deletes every completed run, which includes `first` and takes the keys recorded against it. `second` is open and stays.
			if _, err := store.DeleteCompletedWorkflows(ctx, time.Now().Add(time.Hour)); err != nil {
				t.Fatalf("DeleteCompletedWorkflows: %v", err)
			}
			if wf, err := store.GetWorkflowByID(ctx, first); err == nil && wf != nil {
				t.Fatalf("CONTROL: the sweep did not remove the completed run %s, so this test measures nothing", first)
			}

			replay, err = idem.DeliverSignalIdempotent(ctx, second, "ping", `"once"`, "sweep-key")
			if err != nil || !replay {
				t.Errorf("the same key to the run that holds the signal = (replay=%v, err=%v), want (true, nil): the sweep deleted the key and a retry would deliver twice", replay, err)
			}
			if got := drainSignalPayloads(t, ctx, store, second, "ping"); len(got) != 1 || got[0] != "once" {
				t.Errorf("the current run holds %v, want exactly [once]", got)
			}
		})
	}
}
