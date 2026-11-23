package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// cleat#4340, the coordinator's ruling: a cancel or a terminate addressed to a workflow whose current run is closed for good is refused with 409, exactly as a signal to it is, and one
// whose current run is redrivable follows what a signal does. A cooperative cancel used to answer 200 for a `done` or `cancelled` run, which is the silent loss a signal had: the request
// is stored on a run no one will ever run again.
//
// The table is every end state x every verb x both ways of addressing the chain (to the run that is the end, and to an earlier run that continued into it), because each cell is a different
// path: a refusal is made under the lock the verb took, and the forwarded cell is the one that must also NAME the end.
//
//	end state      cancel (pre-emptive)   terminate   cooperative cancel
//	done           refused                refused     refused
//	cancelled      refused                refused     refused
//	failed         refused                refused     accepted      (redrivable: an operator reopens it, then the request is read)
//	terminated     refused                refused     accepted
//	dead_lettered  refused                ACCEPTED    accepted      (terminate is the verb that takes a dead-lettered run off the queue)
func TestACancelOrTerminateAnswersByHowTheChainEnded(t *testing.T) {
	type endState struct {
		name string
		make func(t *testing.T, ctx context.Context, store WorkflowStore, run string)
	}
	claimThen := func(f func(ctx context.Context, store WorkflowStore, run string, gen int64) error) func(t *testing.T, ctx context.Context, store WorkflowStore, run string) {
		return func(t *testing.T, ctx context.Context, store WorkflowStore, run string) {
			t.Helper()
			wf, err := claimSpecific(t, ctx, store, run, "w-end")
			if err != nil {
				t.Fatalf("claim %s: %v", run, err)
			}
			if err := f(ctx, store, run, wf.Generation); err != nil {
				t.Fatalf("settle %s: %v", run, err)
			}
		}
	}
	states := []endState{
		{"done", claimThen(func(ctx context.Context, s WorkflowStore, run string, gen int64) error {
			return s.CompleteWorkflow(ctx, run, "w-end", gen, `{"r":1}`, nil)
		})},
		{"cancelled", func(t *testing.T, ctx context.Context, s WorkflowStore, run string) {
			t.Helper()
			if err := s.CancelWorkflow(ctx, run, "ended by a cancel"); err != nil {
				t.Fatalf("CancelWorkflow(%s): %v", run, err)
			}
		}},
		{"failed", claimThen(func(ctx context.Context, s WorkflowStore, run string, gen int64) error {
			return s.FailWorkflow(ctx, run, "w-end", gen, "boom", "E_BOOM", "op", nil)
		})},
		{"terminated", func(t *testing.T, ctx context.Context, s WorkflowStore, run string) {
			t.Helper()
			if err := s.TerminateWorkflow(ctx, run, "ended by a terminate"); err != nil {
				t.Fatalf("TerminateWorkflow(%s): %v", run, err)
			}
		}},
		{"dead_lettered", claimThen(func(ctx context.Context, s WorkflowStore, run string, gen int64) error {
			return s.MoveToDeadLetterQueue(ctx, run, "w-end", gen, "gave up", "E_GAVE_UP", "op", nil)
		})},
	}

	type verb struct {
		name string
		do   func(ctx context.Context, store WorkflowStore, id string) error
		// accepted maps an end state to whether the verb is accepted; any state not named is refused.
		accepted map[string]bool
		// effect reads whether the verb took effect on the run.
		effect func(t *testing.T, ctx context.Context, store WorkflowStore, run string) bool
	}
	status := func(t *testing.T, ctx context.Context, store WorkflowStore, run, want string) bool {
		t.Helper()
		return statusOf(t, ctx, store, run) == want
	}
	verbs := []verb{
		{"cancel", func(ctx context.Context, s WorkflowStore, id string) error { return s.CancelWorkflow(ctx, id, "cell") },
			map[string]bool{},
			func(t *testing.T, ctx context.Context, s WorkflowStore, run string) bool {
				return status(t, ctx, s, run, statusCancelled)
			}},
		{"terminate", func(ctx context.Context, s WorkflowStore, id string) error {
			return s.TerminateWorkflow(ctx, id, "cell")
		},
			map[string]bool{"dead_lettered": true},
			func(t *testing.T, ctx context.Context, s WorkflowStore, run string) bool {
				return status(t, ctx, s, run, statusTerminated)
			}},
		{"cooperative", func(ctx context.Context, s WorkflowStore, id string) error {
			return s.RequestCancellation(ctx, id, "cell")
		},
			map[string]bool{"failed": true, "terminated": true, "dead_lettered": true},
			func(t *testing.T, ctx context.Context, s WorkflowStore, run string) bool {
				flagged, _, err := s.CheckCancellation(ctx, run)
				if err != nil {
					t.Fatalf("CheckCancellation(%s): %v", run, err)
				}
				return flagged
			}},
	}

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "end-state-parent")

			for _, st := range states {
				st := st
				for _, v := range verbs {
					v := v
					for _, addressedTo := range []string{"the end itself", "an earlier run that continued into it"} {
						addressedTo := addressedTo
						t.Run(st.name+"/"+v.name+"/"+addressedTo, func(t *testing.T) {
							first := settleRunOpen(t, ctx, store, parent)
							second := continueChain(t, ctx, store, first)
							st.make(t, ctx, store, second)

							target := second
							if addressedTo != "the end itself" {
								target = first
							}
							// CONTROL, once per cell: nothing has acted on the end yet, so an "effect" read below is the verb's and not left over from setting the state up.
							if v.name == "cooperative" {
								if flagged, _, _ := store.CheckCancellation(ctx, second); flagged {
									t.Fatalf("CONTROL: the end %s is already flagged before the verb ran", st.name)
								}
							}
							before := statusOf(t, ctx, store, second)

							err := v.do(ctx, store, target)
							if v.accepted[st.name] {
								if err != nil {
									t.Fatalf("%s to a chain whose end is %s = %v, want it accepted (redrivable, as a signal is)", v.name, st.name, err)
								}
								if !v.effect(t, ctx, store, second) {
									t.Errorf("%s was accepted but did not take effect on the end (status was %q, now %q)", v.name, before, statusOf(t, ctx, store, second))
								}
								return
							}
							if !errors.Is(err, ErrAdminStateConflict) {
								t.Fatalf("%s to a chain whose end is %s = %v, want ErrAdminStateConflict (409 state_conflict)", v.name, st.name, err)
							}
							if !strings.Contains(err.Error(), "status="+st.name) {
								t.Errorf("the refusal %q does not name the end's status %q", err, st.name)
							}
							if addressedTo != "the end itself" && !strings.Contains(err.Error(), second) {
								t.Errorf("the refusal %q does not name the run that refused (%s): the caller addressed %s", err, second, first)
							}
							// A refusal leaves the end exactly as it was.
							if after := statusOf(t, ctx, store, second); after != before {
								t.Errorf("a refused %s changed the end from %q to %q", v.name, before, after)
							}
							if v.name == "cooperative" {
								if flagged, _, _ := store.CheckCancellation(ctx, second); flagged {
									t.Errorf("a refused cooperative cancel still flagged the end: the request was stored on a run no one will read")
								}
							}
						})
					}
				}
			}
		})
	}
}
