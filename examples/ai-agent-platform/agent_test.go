package aiagentplatform

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/cleattest"
)

// setupEnv creates a test environment and wires its HostCalls into the
// package-level h, which the workflow reaches through rather than taking a
// HostCalls parameter. Without this line `h` is nil and the first PluginCall
// panics -- see cleat/runtime.go, and note that the panic names the runtime
// rather than the test.
func setupEnv() *cleattest.TestEnv {
	env := cleattest.NewTestEnv()
	h = env.H()
	return env
}

// modelReply builds one `llm.chat` response in the wire shape the plugin
// forwards. Every stub in this file goes through it, so a change to the shape
// is one edit rather than eight.
func modelReply(content string, cost float64, calls ...ToolCall) string {
	msg := map[string]any{"role": "assistant", "content": content}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	b, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": msg, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120},
		"cost":    cost,
		"model":   "agent-stub-1",
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func toolCall(id, name, args string) ToolCall {
	return ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: args}}
}

// run drives RunAgent to completion while advancing the simulated clock.
//
// It has to: the settle step is a DurableSleep, and a durable sleep waits on
// SIMULATED time -- real timers do not move it, so a test that simply called
// RunAgent would block until the deadline.
func run(t *testing.T, env *cleattest.TestEnv, in AgentInput) (AgentOutput, error) {
	t.Helper()

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := RunAgent(env.H(), string(raw))
		done <- result{out, err}
	}()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case r := <-done:
			if r.err != nil {
				return AgentOutput{}, r.err
			}
			var out AgentOutput
			if err := json.Unmarshal([]byte(r.out), &out); err != nil {
				t.Fatalf("decode agent output %q: %v", r.out, err)
			}
			return out, nil
		case <-deadline:
			t.Fatal("RunAgent did not return within 10s of real time")
			return AgentOutput{}, nil
		default:
			// Generous per tick, for the reason integration-hub's helper gives:
			// the settle window is tens of seconds of simulated time and a
			// fine-grained tick spends its budget on iterations.
			env.AdvanceTime(time.Hour)
			time.Sleep(time.Millisecond)
		}
	}
}

func baseInput() AgentInput {
	return AgentInput{
		TenantID:    "11111111-1111-1111-1111-111111111111",
		Task:        "Summarise yesterday's incidents.",
		MaxSteps:    6,
		BudgetUSD:   1.00,
		ArtifactKey: "incidents/report.md",
	}
}

// modelCalls returns how many times the workflow asked the model anything.
func modelCalls(env *cleattest.TestEnv) int {
	n := 0
	for _, c := range env.CallHistory() {
		if c.Service == "llm" && c.Operation == "chat" {
			n++
		}
	}
	return n
}

// ---- The loop ----

func TestAgentAnswersWithoutTools(t *testing.T) {
	env := setupEnv()
	env.OnPluginCall("llm", "chat").Return(modelReply("Two incidents, both resolved.", 0.004), nil)
	env.OnPluginCall("blobstore", "put").Return(`{"key":"incidents/report.md","sha256":"ab","size":32}`, nil)

	out, err := run(t, env, baseInput())
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	if out.Status != "done" {
		t.Errorf("status = %q, want done", out.Status)
	}
	if out.Output != "Two incidents, both resolved." {
		t.Errorf("output = %q, want the model's text", out.Output)
	}
	if out.Steps != 1 {
		t.Errorf("steps = %d, want 1: a completion with no tool call ends the loop", out.Steps)
	}
	if out.SpentUSD != 0.004 {
		t.Errorf("spent = %v, want the response's cost", out.SpentUSD)
	}
	if out.Tokens != 120 {
		t.Errorf("tokens = %d, want the response's usage", out.Tokens)
	}
	env.AssertCalled(t, "blobstore", "put")
}

// THE CEILING BOUNDS THE RUN, and the assertion is a COUNT rather than a status.
//
// The model is stubbed to request a tool on every turn, so the loop has no way
// to finish on its own and two things could end it: the budget, or MaxSteps.
// `budget_exceeded` alone does not distinguish them -- a loop that ignored the
// ceiling and hit MaxSteps would never report it, but a loop that checked only
// once at the end would report it having spent everything.
//
// So the assertion is on the number of model calls, and the number is exact: at
// $0.01 a call against a $0.03 budget it is three. Remove the ceiling and it
// becomes MaxSteps (six); change `>=` to `>` and it becomes four. Both
// mutations leave the reported status reading `budget_exceeded`.
func TestAgentStopsAtItsCeilingBeforeTheNextCall(t *testing.T) {
	env := setupEnv()
	// Every turn asks to save a report, so the loop never finishes on its own.
	env.OnPluginCall("llm", "chat").Return(modelReply("", 0.01,
		toolCall("c1", "save_report", `{"body":"draft"}`)), nil)
	env.OnPluginCall("blobstore", "put").Return(`{"key":"report","sha256":"ab","size":5}`, nil)

	in := baseInput()
	in.BudgetUSD = 0.03

	out, err := run(t, env, in)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	if out.Status != "budget_exceeded" {
		t.Fatalf("status = %q, want budget_exceeded", out.Status)
	}
	if out.SpentUSD != 0.03 {
		t.Errorf("spent = %v, want exactly the 0.03 ceiling", out.SpentUSD)
	}
	// 0.01 per call against a 0.03 ceiling: three calls reach the ceiling, and
	// the fourth is refused BEFORE it is made.
	if got := modelCalls(env); got != 3 {
		t.Errorf("the model was called %d times, want 3: at $0.01 a call against a "+
			"$0.03 ceiling, exactly three calls are affordable.\n"+
			"A larger count means the ceiling did not bound the loop -- with the check "+
			"removed it runs to MaxSteps (6), and with `>=` weakened to `>` it makes a "+
			"fourth call and spends $0.04 against $0.03.", got)
	}
}

func TestAgentFeedsAToolResultBackToTheModel(t *testing.T) {
	env := setupEnv()
	env.OnPluginCall("blobstore", "put").Return(`{"key":"report","sha256":"ab","size":5}`, nil)

	// First turn asks for the tool, so the second turn's request must carry the
	// tool result; the stub for the second turn is the same one, but the LOOP's
	// behaviour is what the assertions below read.
	env.OnPluginCall("llm", "chat").Return(modelReply("Saved.", 0.002,
		toolCall("c1", "save_report", `{"body":"draft"}`)), nil)

	out, err := run(t, env, baseInput())
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if !contains(out.ToolsUsed, "save_report") {
		t.Errorf("tools_used = %v, want save_report", out.ToolsUsed)
	}

	// The second request must carry the assistant's tool_call AND the tool's
	// reply. A loop that appended only the tool message would send a
	// conversation the provider refuses; one that appended neither would loop
	// on the same turn forever.
	history := env.CallHistory()
	var second string
	seen := 0
	for _, c := range history {
		if c.Service == "llm" && c.Operation == "chat" {
			seen++
			if seen == 2 {
				second = c.Request
			}
		}
	}
	if second == "" {
		t.Fatal("the model was called only once; the loop did not continue after the tool result")
	}
	if !strings.Contains(second, `"role":"tool"`) {
		t.Errorf("the second request carries no tool message:\n%s", second)
	}
	if !strings.Contains(second, "save_report") {
		t.Errorf("the second request does not carry the tool call the model made:\n%s", second)
	}
}

// ---- The human wait ----

func TestAgentProceedsWhenAHumanApproves(t *testing.T) {
	env := setupEnv()
	env.OnPluginCall("llm", "chat").Return(modelReply("Rolled back.", 0.003,
		toolCall("c1", "request_approval", `{"reason":"roll back the deploy"}`)), nil)
	env.OnPluginCall("event-triggers", "await_event").
		Return(`{"found":true,"event_id":"e1","event_data":{"approved":true,"note":"go ahead"}}`, nil)
	env.OnPluginCall("blobstore", "put").Return(`{"key":"k","sha256":"ab","size":1}`, nil)

	in := baseInput()
	in.MaxSteps = 1

	out, err := run(t, env, in)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if out.Status != "done" {
		t.Errorf("status = %q, want done", out.Status)
	}
	if !contains(out.ToolsUsed, "request_approval") {
		t.Errorf("tools_used = %v, want request_approval", out.ToolsUsed)
	}
	env.AssertCalled(t, "event-triggers", "await_event")
}

// A DENIAL IS A RESULT, NOT AN ERROR, and the assertion is on what the MODEL
// was told rather than on the run's status.
//
// Measuring that needs a second turn: the denial is only proven to have been
// fed back if a later request carries it. MaxSteps=2 gets one, at the cost of
// the model asking for approval twice, rather than approving on turn one and
// summarising on turn two -- this test predates cleat#2522's fix and was not
// rewritten to use it (still a correct test either way; see
// TestAgentApprovesOnTheFirstPoll for why sequencing the stub now can, but
// this file does not yet, drive that turn-taking shape directly).
func TestAgentReportsADenialRatherThanFailing(t *testing.T) {
	env := setupEnv()
	env.OnPluginCall("llm", "chat").Return(modelReply("", 0.003,
		toolCall("c1", "request_approval", `{"reason":"delete the database"}`)), nil)
	env.OnPluginCall("event-triggers", "await_event").
		Return(`{"found":true,"event_id":"e1","event_data":{"approved":false,"note":"no"}}`, nil)
	env.OnPluginCall("blobstore", "put").Return(`{"key":"k","sha256":"ab","size":1}`, nil)

	in := baseInput()
	in.MaxSteps = 2

	out, err := run(t, env, in)
	if err != nil {
		t.Fatalf("a denial must be a result the model can act on, not an error: %v", err)
	}
	if out.Status != "done" {
		t.Errorf("status = %q, want done: the run finished, having been told no", out.Status)
	}

	// The second request must carry the denial, because that is what lets the
	// model choose a different action. A loop that dropped a "no" would look
	// identical from the run list and would leave the model retrying it.
	var second string
	seen := 0
	for _, c := range env.CallHistory() {
		if c.Service == "llm" && c.Operation == "chat" {
			seen++
			if seen == 2 {
				second = c.Request
			}
		}
	}
	if second == "" {
		t.Fatal("the model was called only once; the denial was never fed back")
	}

	// DECODE IT. The first version of this assertion was a substring search for
	// `"approved":false`, and it failed on a request that carried
	// `{\"approved\":false,\"note\":\"no\"}` -- the denial IS there, escaped
	// inside a JSON string, where a raw-text search for the unescaped form
	// cannot see it. A tool result is a string field, so what the model reads
	// is the DECODED value, and that is what to assert on.
	var req struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(second), &req); err != nil {
		t.Fatalf("the recorded request is not valid JSON: %v", err)
	}
	var fedBack string
	for _, m := range req.Messages {
		if m.Role == "tool" && m.Name == "request_approval" {
			fedBack = m.Content
		}
	}
	if fedBack == "" {
		t.Fatalf("no tool message came back to the model:\n%s", second)
	}
	var decision struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal([]byte(fedBack), &decision); err != nil {
		t.Fatalf("the tool message is not the approval decision: %q", fedBack)
	}
	if decision.Approved {
		t.Errorf("the model was told the action was approved; it was denied: %q", fedBack)
	}
}

// ONE PATH THIS FILE STILL DOES NOT REACH, though the tool it needed now
// exists. cleat#2522.
//
// **In `requestApproval`.** It polls: it asks for the event, and on
// `found:false` it sleeps and asks again. The branch where the FIRST poll
// misses and a LATER one hits is the branch a real deployment takes most often
// -- a human takes minutes, not microseconds -- and nothing here reaches it.
//
// **`OnPluginCall(...).Return(...)` can now express that miss-then-hit
// sequence** -- a second `.Return` for the same plugin+function used to be
// unreachable (`pluginCallImpl` matched only the first-registered stub,
// forever) and now answers the second call, with the last registered
// response repeating after that. What is below is still the two tests
// written against the OLD limitation, bounding the loop with `MaxSteps`
// instead of sequencing `event-triggers/await_event`'s answer -- they remain
// correct tests of the turn-taking behaviour they assert on, so they were
// left as they are rather than rewritten for its own sake. What they do NOT
// do, and what a new test using the sequencing above could, is drive
// `requestApproval`'s actual miss-then-hit branch end to end; that is
// unclaimed follow-up work, not done here.
func TestAgentApprovesOnTheFirstPoll(t *testing.T) {
	env := setupEnv()
	env.OnPluginCall("llm", "chat").Return(modelReply("", 0.001,
		toolCall("c1", "request_approval", `{"reason":"ship it"}`)), nil)
	env.OnPluginCall("event-triggers", "await_event").
		Return(`{"found":true,"event_id":"e1","event_data":{"approved":true,"note":""}}`, nil)
	env.OnPluginCall("blobstore", "put").Return(`{"key":"k","sha256":"ab","size":1}`, nil)

	in := baseInput()
	in.MaxSteps = 1

	if _, err := run(t, env, in); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	polls := 0
	for _, c := range env.CallHistory() {
		if c.Service == "event-triggers" && c.Operation == "await_event" {
			polls++
		}
	}
	if polls != 1 {
		t.Errorf("await_event was called %d times, want 1: a hit on the first poll "+
			"must not sleep and poll again", polls)
	}
}

// ---- Refusals ----

func TestAgentRefusesAToolItDoesNotHave(t *testing.T) {
	env := setupEnv()
	env.OnPluginCall("llm", "chat").Return(modelReply("", 0.001,
		toolCall("c1", "wire_transfer", `{"to":"somewhere"}`)), nil)
	env.OnPluginCall("blobstore", "put").Return(`{"key":"k","sha256":"ab","size":1}`, nil)

	_, err := run(t, env, baseInput())
	if err == nil {
		t.Fatal("an unregistered tool was accepted; the agent ran a tool that does not exist")
	}
	if !strings.Contains(err.Error(), "wire_transfer") {
		t.Errorf("error = %v, want it to name the tool the model invented", err)
	}
	if !strings.Contains(err.Error(), "search_docs") {
		t.Errorf("error = %v, want it to name what IS available, so the prompt can be fixed", err)
	}
}

func TestAgentRequiresATenant(t *testing.T) {
	env := setupEnv()
	in := baseInput()
	in.TenantID = ""

	_, err := run(t, env, in)
	if err == nil {
		t.Fatal("a run with no tenant_id was accepted; the spend ceiling is per tenant")
	}
	if !strings.Contains(err.Error(), "tenant_id") {
		t.Errorf("error = %v, want it to name the missing field", err)
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
