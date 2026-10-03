package agentworkflow_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/agentworkflow"
	"github.com/cleat-team/cleat/cleat/cleattest"
)

// The tool-error mode and the approval-gate tool kind (cleat#3022, part B).
//
// THE ERROR MODE IS A CONTRACT, NOT A PREFERENCE, which is why it is
// configurable rather than changed: this workflow has always fed a tool error
// back to the model, and examples/ai-agent-platform asserts that an unknown
// tool FAILS the run. Both are defensible and they are observably different, so
// a caller migrating onto this workflow must not discover the change by
// implication. Each direction is tested, because a mode that silently behaved
// like the other would pass a one-sided test.

// runAdvancing drives Run while advancing the virtual clock. The approval
// wait's sleeps block until simulated time moves, so a test that called Run
// directly would hang -- the same shape examples/ai-agent-platform's harness
// uses, for the same reason.
func runAdvancing(t *testing.T, env *cleattest.TestEnv, in agentworkflow.Input) (agentworkflow.Result, error) {
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

	deadline := time.After(10 * time.Second)
	for {
		select {
		case r := <-done:
			if r.err != nil {
				return agentworkflow.Result{}, r.err
			}
			var res agentworkflow.Result
			if err := json.Unmarshal([]byte(r.out), &res); err != nil {
				t.Fatalf("decode result %q: %v", r.out, err)
			}
			return res, nil
		case <-deadline:
			t.Fatal("Run did not return within 10s of real time")
			return agentworkflow.Result{}, nil
		default:
			env.AdvanceTime(time.Hour)
			time.Sleep(time.Millisecond)
		}
	}
}

// ---- the error mode -------------------------------------------------------

func TestAnUnknownToolFailsTheRunUnderToolErrorFail(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"c1"}, []string{"wire_transfer"}, []string{`{}`}), nil)

	_, err := run(t, env, agentworkflow.Input{
		Message:       "move the money",
		ToolErrorMode: agentworkflow.ToolErrorFail,
		Tools:         loopingTool,
	})
	if err == nil {
		t.Fatal("ToolErrorFail must fail the run when the model invents a tool")
	}
	if !strings.Contains(err.Error(), "wire_transfer") {
		t.Errorf("error = %q, want it to name the tool the model asked for", err)
	}
}

func TestAFailingToolFailsTheRunUnderToolErrorFail(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnCall("s", "o", nil).Return("", errors.New("upstream is down"))
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"c1"}, []string{"t"}, []string{`{}`}), nil)

	_, err := run(t, env, agentworkflow.Input{
		Message:       "go",
		ToolErrorMode: agentworkflow.ToolErrorFail,
		Tools:         loopingTool,
	})
	if err == nil {
		t.Fatal("ToolErrorFail must fail the run when a tool errors")
	}
	if !strings.Contains(err.Error(), "upstream is down") {
		t.Errorf("error = %q, want the tool's own error carried through", err)
	}
}

// The default, asserted against the SAME input that fails above -- so this pair
// cannot both pass unless the mode is actually consulted.
func TestTheDefaultInjectsTheToolErrorToTheModel(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"c1"}, []string{"wire_transfer"}, []string{`{}`}), nil).
		Return(answer("I cannot do that."), nil)

	res, err := run(t, env, agentworkflow.Input{Message: "move the money", Tools: loopingTool})
	if err != nil {
		t.Fatalf("the default must not fail the run on an unknown tool: %v", err)
	}
	if res.Answer != "I cannot do that." {
		t.Errorf("Answer = %q, want the model's second-turn answer", res.Answer)
	}
	if len(res.ToolCalls) != 1 || !strings.Contains(res.ToolCalls[0].Error, "no such tool") {
		t.Errorf("ToolCalls = %+v, want the unknown-tool error recorded on the call", res.ToolCalls)
	}
	// The model must SEE it: a run that recorded the error and told the model
	// nothing would still reach an answer, and the assertion above would pass.
	var second string
	seen := 0
	for _, rec := range env.CallHistory() {
		if rec.Service == "llm" && rec.Operation == "chat" {
			seen++
			if seen == 2 {
				second = rec.Request
			}
		}
	}
	if seen != 2 {
		t.Fatalf("llm.chat called %d times, want 2", seen)
	}
	if !strings.Contains(second, "no such tool") {
		t.Errorf("the second llm.chat request does not carry the tool error, so the model was never told:\n%s", second)
	}
}

func TestAnUnknownToolErrorModeIsRefused(t *testing.T) {
	env := cleattest.NewTestEnv()
	_, err := run(t, env, agentworkflow.Input{Message: "x", ToolErrorMode: "explode"})
	if err == nil {
		t.Fatal("an unrecognised tool_error_mode must be refused, not treated as the default")
	}
	if !strings.Contains(err.Error(), "explode") {
		t.Errorf("error = %q, want it to name the mode it refused", err)
	}
}

// ---- the approval gate ----------------------------------------------------

func approvalTool(maxPolls int) []agentworkflow.Tool {
	return []agentworkflow.Tool{{
		Name: "request_approval", Kind: agentworkflow.KindApproval,
		Plugin: "approvals", Function: "poll", MaxPolls: maxPolls,
	}}
}

func TestAnApprovalToolPollsUntilTheClaimIsFound(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("approvals", "poll").
		Return(`{"found":false}`, nil).
		Return(`{"found":true,"approved":true}`, nil)
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"c1"}, []string{"request_approval"}, []string{`{}`}), nil).
		Return(answer("approved, proceeding."), nil)

	res, err := runAdvancing(t, env, agentworkflow.Input{
		Message: "ask first", Tools: approvalTool(5), MaxSteps: 3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := pluginCalls(env, "approvals", "poll"); got != 2 {
		t.Errorf("approvals.poll called %d times, want 2 -- one miss, then the hit", got)
	}
	// The HIT is what the model receives, not the miss before it.
	found := false
	for _, rec := range env.CallHistory() {
		if rec.Service == "llm" && rec.Operation == "chat" &&
			strings.Contains(rec.Request, `\"approved\":true`) {
			found = true
		}
	}
	if !found {
		t.Errorf("no llm.chat request carried the approval the poll found; result = %+v", res.ToolCalls)
	}
}

// A timeout is a RESULT. The model is told nobody approved, and the run
// continues -- it does not fail, which is the difference that makes an agent
// able to choose another action rather than dying on a human's silence.
func TestAnApprovalToolReturnsATimeoutAsAResult(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("approvals", "poll").Return(`{"found":false}`, nil)
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"c1"}, []string{"request_approval"}, []string{`{}`}), nil).
		Return(answer("nobody approved, so I stopped."), nil)

	res, err := runAdvancing(t, env, agentworkflow.Input{
		Message: "ask first", Tools: approvalTool(3), MaxSteps: 3,
	})
	if err != nil {
		t.Fatalf("a timeout must not fail the run: %v", err)
	}
	if got := pluginCalls(env, "approvals", "poll"); got != 3 {
		t.Errorf("approvals.poll called %d times, want exactly 3 (MaxPolls)", got)
	}
	if res.Answer != "nobody approved, so I stopped." {
		t.Errorf("Answer = %q, want the model's answer after the timeout", res.Answer)
	}

	// And the model was handed the timeout claim, not an empty string.
	sawTimeout := false
	for _, rec := range env.CallHistory() {
		if rec.Service == "llm" && rec.Operation == "chat" && strings.Contains(rec.Request, "timed_out") {
			sawTimeout = true
		}
	}
	if !sawTimeout {
		t.Error("no llm.chat request carried the timeout claim, so the model could not have known it timed out")
	}
}

func TestAnApprovalToolNeedsAPluginAndFunction(t *testing.T) {
	env := cleattest.NewTestEnv()
	_, err := run(t, env, agentworkflow.Input{
		Message: "x",
		Tools:   []agentworkflow.Tool{{Name: "a", Kind: agentworkflow.KindApproval}},
	})
	if err == nil {
		t.Fatal("an approval tool with no plugin or function must be refused at declaration")
	}
	if !strings.Contains(err.Error(), "plugin and function") {
		t.Errorf("error = %q, want it to say what the tool is missing", err)
	}
}
