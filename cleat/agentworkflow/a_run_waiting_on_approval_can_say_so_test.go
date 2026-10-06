package agentworkflow_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/agentworkflow"
	"github.com/cleat-team/cleat/cleat/cleattest"
)

// cleat#3170: the approval gate cleat#3022 added has no observability. A
// caller watching a live run must be able to see that it is waiting on a
// human, since when, and how many times it has polled -- the same gap
// examples/ai-agent-platform's hand-written loop closed for itself and that
// migrating onto this workflow silently lost (cleat#2980).
//
// THESE TESTS CANNOT ALL USE env.QueryState AFTER Run RETURNS. Most of the
// new query-state values are TRANSIENT -- overwritten by the next status the
// loop reaches, same as the run's own "status" key always has been. Two are
// not: approval_wait and approval_wait_since are written exactly once, before
// the first poll, and nothing in this package ever touches them again, so
// they survive to be read after the run finishes. approval_polls is close but
// not quite the same shape -- it is rewritten on every miss, so what survives
// is its LAST value, which is still meaningful (the miss count immediately
// before the hit, or MaxPolls on a timeout). Where a value genuinely cannot
// survive to completion (the live "awaiting_approval" status itself), the
// test observes it WHILE the run is still in progress, the same way
// runAdvancing already has to run the workflow concurrently with the virtual
// clock.

// runObservingStatus is runAdvancing (the sibling test file's helper) with
// one addition: it snapshots QueryState("status") on every tick, so a test
// can assert a TRANSIENT value was reached at some point during the run, not
// only what the key reads after Run has already moved past it.
func runObservingStatus(t *testing.T, env *cleattest.TestEnv, in agentworkflow.Input) (agentworkflow.Result, error, []string) {
	t.Helper()
	type outcome struct {
		out string
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		out, err := agentworkflow.Run(env.H(), input(t, in))
		done <- outcome{out, err}
	}()

	var seen []string
	record := func() {
		if v, ok := env.QueryState(agentworkflow.QueryKeyStatus); ok {
			if len(seen) == 0 || seen[len(seen)-1] != v {
				seen = append(seen, v)
			}
		}
	}

	deadline := time.After(10 * time.Second)
	for {
		record()
		select {
		case r := <-done:
			record()
			if r.err != nil {
				return agentworkflow.Result{}, r.err, seen
			}
			res, err := decodeResult(t, r.out)
			return res, err, seen
		case <-deadline:
			t.Fatal("Run did not return within 10s of real time")
			return agentworkflow.Result{}, nil, seen
		default:
			env.AdvanceTime(time.Hour)
			time.Sleep(time.Millisecond)
		}
	}
}

func TestAnApprovalWaitRecordsWhenItWasEnteredAndHowManyPolls(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("approvals", "poll").
		Return(`{"found":false}`, nil).
		Return(`{"found":false}`, nil).
		Return(`{"found":true,"approved":true}`, nil)
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"c1"}, []string{"request_approval"}, []string{`{}`}), nil).
		Return(answer("approved, proceeding."), nil)

	_, err, statuses := runObservingStatus(t, env, agentworkflow.Input{
		Message: "ask first", Tools: approvalTool(5), MaxSteps: 3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	wait, ok := env.QueryState(agentworkflow.QueryKeyApprovalWait)
	if !ok || wait != "entered" {
		t.Errorf("approval_wait = %q, %v, want \"entered\"", wait, ok)
	}

	since, ok := env.QueryState(agentworkflow.QueryKeyApprovalWaitSince)
	if !ok {
		t.Fatal("approval_wait_since was never written")
	}
	if _, perr := time.Parse(time.RFC3339, since); perr != nil {
		t.Errorf("approval_wait_since = %q, not RFC3339: %v", since, perr)
	}

	// Two misses before the hit on the third poll.
	polls, ok := env.QueryState(agentworkflow.QueryKeyApprovalPolls)
	if !ok || polls != "2" {
		t.Errorf("approval_polls = %q, %v, want \"2\" (two misses before the hit)", polls, ok)
	}

	if !containsStr(statuses, agentworkflow.QueryStatusAwaitingApproval) {
		t.Errorf("status never observed as %q while the run was live; observed %v",
			agentworkflow.QueryStatusAwaitingApproval, statuses)
	}
}

// THE ABSENCE IS THE SIGNAL. A run approved on its very first poll must not
// report any misses -- approval_polls is written only on a miss, so it stays
// unset here, and a version that wrote it unconditionally (e.g. "1" for the
// first poll regardless of outcome) would fail this.
func TestApprovalPollsIsUnsetWhenTheFirstPollHits(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("approvals", "poll").Return(`{"found":true,"approved":true}`, nil)
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"c1"}, []string{"request_approval"}, []string{`{}`}), nil).
		Return(answer("approved, proceeding."), nil)

	_, err, _ := runObservingStatus(t, env, agentworkflow.Input{
		Message: "ask first", Tools: approvalTool(5), MaxSteps: 3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if v, ok := env.QueryState(agentworkflow.QueryKeyApprovalPolls); ok {
		t.Errorf("approval_polls = %q, want unset: the first poll hit, so there was no miss to count", v)
	}
	// approval_wait must still be set -- the wait was genuinely entered, a
	// first-poll hit is not the same thing as never having asked.
	if wait, ok := env.QueryState(agentworkflow.QueryKeyApprovalWait); !ok || wait != "entered" {
		t.Errorf("approval_wait = %q, %v, want \"entered\" even on an immediate hit", wait, ok)
	}
}

func TestApprovalTimeoutIsVisibleAndDistinctFromAHit(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("approvals", "poll").Return(`{"found":false}`, nil)
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"c1"}, []string{"request_approval"}, []string{`{}`}), nil).
		Return(answer("nobody approved, so I stopped."), nil)

	res, err, statuses := runObservingStatus(t, env, agentworkflow.Input{
		Message: "ask first", Tools: approvalTool(3), MaxSteps: 3,
	})
	if err != nil {
		t.Fatalf("a timeout must not fail the run: %v", err)
	}
	if res.Answer != "nobody approved, so I stopped." {
		t.Errorf("Answer = %q, want the model's post-timeout answer", res.Answer)
	}

	if polls, ok := env.QueryState(agentworkflow.QueryKeyApprovalPolls); !ok || polls != "3" {
		t.Errorf("approval_polls = %q, %v, want \"3\" (MaxPolls, all misses)", polls, ok)
	}
	if !containsStr(statuses, agentworkflow.QueryStatusAwaitingApproval) {
		t.Errorf("status never observed as %q; observed %v", agentworkflow.QueryStatusAwaitingApproval, statuses)
	}
}

// THE LIVE "status" KEY ENDS WHERE Result.Status DOES, so a caller who only
// ever reads the live key during a poller still lands on an answer
// Result.Status agrees with -- the two must never disagree once the run has
// actually finished.
func TestTheLiveStatusKeyAgreesWithResultStatusAtCompletion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input agentworkflow.Input
		stub  func(env *cleattest.TestEnv)
		want  string
	}{
		{
			name:  "done",
			input: agentworkflow.Input{Message: "what is the answer?"},
			stub: func(env *cleattest.TestEnv) {
				env.OnPluginCall("llm", "chat").Return(answer("42"), nil)
			},
			want: agentworkflow.StatusDone,
		},
		{
			name:  "budget_exceeded",
			input: agentworkflow.Input{Message: "ask", Budget: 0.02, Tools: loopingTool, MaxSteps: 6},
			stub: func(env *cleattest.TestEnv) {
				env.OnPluginCall("llm", "chat").
					Return(callsCosting("0.03", []string{"c1"}, []string{"t"}, []string{`{}`}), nil)
				env.OnCall("s", "o", nil).Return("ok", nil)
			},
			want: agentworkflow.StatusBudgetExceeded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := cleattest.NewTestEnv()
			tc.stub(env)
			res, err := run(t, env, tc.input)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Status != tc.want {
				t.Fatalf("Result.Status = %q, want %q", res.Status, tc.want)
			}
			if live, ok := env.QueryState(agentworkflow.QueryKeyStatus); !ok || live != tc.want {
				t.Errorf("live status = %q, %v, want %q to match Result.Status", live, ok, tc.want)
			}
		})
	}
}

// A FAILED RUN IS VISIBLE TOO, not just a silent error returned to whoever
// started it -- a caller watching query state should not have to wait for
// the run to be reaped to learn it died.
func TestAFailedRunIsVisibleInTheLiveStatus(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"c1"}, []string{"wire_transfer"}, []string{`{}`}), nil)

	_, err := run(t, env, agentworkflow.Input{
		Message:       "move the money",
		ToolErrorMode: agentworkflow.ToolErrorFail,
		Tools:         loopingTool,
	})
	if err == nil {
		t.Fatal("ToolErrorFail must fail the run on an unknown tool")
	}
	if live, ok := env.QueryState(agentworkflow.QueryKeyStatus); !ok || live != agentworkflow.QueryStatusFailed {
		t.Errorf("live status = %q, %v, want %q", live, ok, agentworkflow.QueryStatusFailed)
	}
}

func containsStr(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func decodeResult(t *testing.T, out string) (agentworkflow.Result, error) {
	t.Helper()
	var res agentworkflow.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode result %q: %v", out, err)
	}
	return res, nil
}
