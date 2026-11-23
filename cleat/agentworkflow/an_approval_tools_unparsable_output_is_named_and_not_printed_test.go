package agentworkflow_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/cleat/agentworkflow"
	"github.com/cleat-team/cleat/cleat/cleattest"
)

// cleat#4390. An approval tool that answers with something that is not JSON used to put its raw output into the error text, and that text goes to TWO places
// by default (a tool error does not end the run; the model is told, and the caller is shown):
//
//   - the tool message sent to the LLM provider on the next turn, a third party, and
//   - the ToolCallRecord the run returns.
//
// With ToolErrorFail it is the run's own error, which is stored. The output is whatever the approval plugin produced, and by then the engine may have decrypted it.
// It is now named by its size and a hash. The expected text is written out literally; its size and hash were computed outside Go (hashlib).
//
// What is asserted is the REQUEST the model is sent, not only the returned record: the record can be right while the model is still handed the body.
func TestAnApprovalToolsUnparsableOutputIsNamedAndNotPrinted(t *testing.T) {
	const marker = "MARKER-APPR-4390"
	env := cleattest.NewTestEnv()
	env.OnPluginCall("approvals", "poll").Return(`not json `+marker, nil)
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"c1"}, []string{"request_approval"}, []string{`{"reason":"ship it"}`}), nil).
		Return(answer("done."), nil)
	tools := []agentworkflow.Tool{{
		Name: "request_approval", Kind: agentworkflow.KindApproval,
		Plugin: "approvals", Function: "poll",
		StaticArgs: json.RawMessage(`{"event_type":"agent.approval","timeout_ms":0}`),
	}}
	res, err := run(t, env, agentworkflow.Input{Message: "ask first", Tools: tools, MaxSteps: 3})
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: the run itself failed (%v); a tool error is meant to reach the model, so this measures nothing", err)
	}

	// the record the caller is shown
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Error == "" {
		t.Fatalf("PRECONDITION FAILED: no tool error was recorded, so this row measures nothing: %+v", res.ToolCalls)
	}
	rec := res.ToolCalls[0]
	for what, text := range map[string]string{"Error": rec.Error, "Result": rec.Result} {
		if strings.Contains(text, marker) || strings.Contains(text, "not json") {
			t.Errorf("the recorded tool %s prints the approval tool's output: %q", what, text)
		}
	}
	if want := `returned unparsable JSON [redacted: 25 bytes, sha256=3cdee00658d3ff29]: `; !strings.Contains(rec.Error, want) {
		t.Errorf("the recorded error does not name the output.\n  got:  %q\n  want it to contain: %q", rec.Error, want)
	}

	// what the LLM provider is sent: every request to llm.chat, and the second one carries the tool's error
	sent := 0
	for _, c := range env.CallHistory() {
		if c.Service != "llm" {
			continue
		}
		sent++
		if strings.Contains(c.Request, marker) {
			t.Errorf("a request to the LLM provider carries the approval tool's output (%d chars of request)", len(c.Request))
		}
	}
	if sent < 2 {
		t.Fatalf("PRECONDITION FAILED: %d llm.chat requests, so the tool's error never reached the model and this half measures nothing", sent)
	}
}
