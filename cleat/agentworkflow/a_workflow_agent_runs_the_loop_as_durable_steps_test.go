package agentworkflow_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/cleat/agentworkflow"
	"github.com/cleat-team/cleat/cleat/cleattest"
)

// The agent loop, tested through the host-side harness.
//
// WHAT THESE CANNOT COVER, stated so the gap is not mistaken for coverage:
// cleattest constructs its own fully populated HostCalls, so it says nothing
// about whether the workflow's compiled module IMPORTS the host calls this
// package makes on the caller's behalf. That is exactly the failure the
// //cleat:require directive guards, and it is guarded elsewhere, at the build
// level (testdata/agentguest). A green run here is not evidence about the
// wiring.

// answer builds an llm.chat response that finishes the loop.
func answer(content string) string {
	return `{"choices":[{"message":{"role":"assistant","content":` + quote(content) +
		`},"finish_reason":"stop"}],"usage":{"total_tokens":11},"model":"test-model"}`
}

// calls builds an llm.chat response that asks for tool calls.
func calls(ids []string, names []string, args []string) string {
	var b strings.Builder
	b.WriteString(`{"choices":[{"message":{"role":"assistant","content":"thinking","tool_calls":[`)
	for i := range names {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":` + quote(ids[i]) + `,"type":"function","function":{"name":` + quote(names[i]) +
			`,"arguments":` + quote(args[i]) + `}}`)
	}
	b.WriteString(`]},"finish_reason":"tool_calls"}],"usage":{"total_tokens":7},"model":"test-model"}`)
	return b.String()
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func input(t *testing.T, in agentworkflow.Input) string {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return string(b)
}

func run(t *testing.T, env *cleattest.TestEnv, in agentworkflow.Input) (agentworkflow.Result, error) {
	t.Helper()
	out, err := agentworkflow.Run(env.H(), input(t, in))
	if err != nil {
		return agentworkflow.Result{}, err
	}
	var res agentworkflow.Result
	if uerr := json.Unmarshal([]byte(out), &res); uerr != nil {
		t.Fatalf("unmarshal result %q: %v", out, uerr)
	}
	return res, nil
}

func TestAnAgentWithNoToolsAnswersInOneStep(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("llm", "chat").Return(answer("42"), nil)

	res, err := run(t, env, agentworkflow.Input{Message: "what is the answer?"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "42" {
		t.Errorf("Answer = %q, want %q", res.Answer, "42")
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1 -- a model that answers without calling a tool is done", res.Steps)
	}
	if len(res.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %v, want none", res.ToolCalls)
	}
}

// The three tool kinds are the point of the design: the same tool list drives
// whatever the workflow can reach, and the model is not told which. Each kind
// gets its own test because a table-driven one would not catch a kind that
// dispatches to the wrong host call.
func TestAServiceToolIsADurableCall(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnCall("weather", "get", nil).Return(`{"temp_c":21}`, nil)
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"call_1"}, []string{"lookup_weather"}, []string{`{"city":"Tokyo"}`}), nil).
		Return(answer("21C in Tokyo"), nil)

	res, err := run(t, env, agentworkflow.Input{
		Message: "how warm is Tokyo?",
		Tools: []agentworkflow.Tool{{
			Name: "lookup_weather", Kind: agentworkflow.KindService,
			Service: "weather", Operation: "get",
		}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	env.AssertCalled(t, "weather", "get")
	if res.Answer != "21C in Tokyo" {
		t.Errorf("Answer = %q", res.Answer)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d, want 2 -- one tool round then the answer", res.Steps)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "lookup_weather" {
		t.Fatalf("ToolCalls = %+v", res.ToolCalls)
	}
	if res.ToolCalls[0].Result != `{"temp_c":21}` {
		t.Errorf("recorded tool result = %q, want the service's response", res.ToolCalls[0].Result)
	}
}

func TestAPluginToolIsAPluginCall(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("email-notify", "send").Return(`{"id":"msg_1"}`, nil)
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"call_1"}, []string{"send_email"}, []string{`{"to":"a@b"}`}), nil).
		Return(answer("sent"), nil)

	res, err := run(t, env, agentworkflow.Input{
		Message: "email them",
		Tools: []agentworkflow.Tool{{
			Name: "send_email", Kind: agentworkflow.KindPlugin,
			Plugin: "email-notify", Function: "send",
		}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ToolCalls[0].Result != `{"id":"msg_1"}` {
		t.Errorf("tool result = %q, want the plugin's response", res.ToolCalls[0].Result)
	}
}

func TestAWorkflowToolStartsAndAwaitsAChild(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnChildWorkflow("summarise").Return(`{"summary":"short"}`, nil)
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"call_1"}, []string{"summarise_it"}, []string{`{"text":"long"}`}), nil).
		Return(answer("done"), nil)

	res, err := run(t, env, agentworkflow.Input{
		Message: "summarise this",
		Tools: []agentworkflow.Tool{{
			Name: "summarise_it", Kind: agentworkflow.KindWorkflow, Workflow: "summarise",
		}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Asserted on the RECORDED child call, not on the result text: a tool that
	// silently did nothing would still let the model say "done".
	hist := env.ChildWorkflowCallHistory()
	if len(hist) != 1 || hist[0].Name != "summarise" {
		t.Fatalf("child workflow calls = %+v, want exactly one to %q", hist, "summarise")
	}
	if res.ToolCalls[0].Result != `{"summary":"short"}` {
		t.Errorf("tool result = %q, want the child's result -- ChildWorkflow returns a runID, so this only holds if AwaitChild ran too", res.ToolCalls[0].Result)
	}
}

func TestTheStepBudgetIsEnforcedWithAStablePrefix(t *testing.T) {
	env := cleattest.NewTestEnv()
	// The model never answers: it asks for a tool on every turn. The LAST
	// response repeats, so this is an unbounded tool-calling model.
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"call_1"}, []string{"t"}, []string{`{}`}), nil)

	_, err := run(t, env, agentworkflow.Input{
		Message:  "loop forever",
		MaxSteps: 3,
		Tools: []agentworkflow.Tool{{
			Name: "t", Kind: agentworkflow.KindService, Service: "s", Operation: "o",
		}},
	})
	if err == nil {
		t.Fatal("a model that never answers must not succeed")
	}
	if !strings.HasPrefix(err.Error(), "agent: exceeded max steps") {
		t.Errorf("error = %q, want the stable %q prefix a caller can match on", err, "agent: exceeded max steps")
	}
}

// A malformed tool is a CONFIGURATION error and must be caught before the model
// is asked anything -- otherwise it surfaces later as a confusing model
// failure, and the operator debugs the prompt.
func TestAMalformedToolIsRejectedBeforeTheLlmIsCalled(t *testing.T) {
	env := cleattest.NewTestEnv()

	_, err := run(t, env, agentworkflow.Input{
		Message: "hi",
		Tools: []agentworkflow.Tool{{
			Name: "broken", Kind: agentworkflow.KindService, Service: "s", // no Operation
		}},
	})
	if err == nil {
		t.Fatal("a service tool with no operation must be rejected")
	}
	if len(env.CallHistory()) != 0 {
		t.Errorf("the llm was called for an invalid config: %+v", env.CallHistory())
	}
}

// THE PROPERTY THE WHOLE DESIGN RESTS ON, tested rather than asserted in a
// doc comment: a second execution of the same run must make the SAME host
// calls, so that the engine's positional replay finds the event it expects.
//
// Replay here is transport-level -- a call with the same function and args
// returns the recorded response -- so a nondeterministic branch inside the
// loop surfaces as a call the first run never made, which is exactly how it
// would surface on a real resume. The conversation is rebuilt from the
// replayed responses rather than stored, so this also pins that the rebuild is
// faithful: if a message differed, the llm.chat request body would differ and
// the divergence would show.
func TestTheLoopReplaysWithoutDivergence(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnCall("weather", "get", nil).Return(`{"temp_c":21}`, nil)
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"call_1"}, []string{"lookup_weather"}, []string{`{"city":"Tokyo"}`}), nil).
		Return(answer("21C in Tokyo"), nil)

	// TWO tools, deliberately. The tools array goes into every llm.chat
	// request, so with two of them an implementation that built it by
	// iterating a map would order it differently on the second run and the
	// request body would not match the recorded one. With one tool the order
	// cannot differ and this test could not see that -- which is the whole
	// class of bug it exists to catch.
	in := agentworkflow.Input{
		Message: "how warm is Tokyo?",
		Tools: []agentworkflow.Tool{
			{
				Name: "lookup_weather", Kind: agentworkflow.KindService,
				Service: "weather", Operation: "get",
			},
			{
				Name: "notify", Kind: agentworkflow.KindPlugin,
				Plugin: "email-notify", Function: "send",
			},
		},
	}

	env.EnableReplay()
	res, err := run(t, env, in)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if res.Answer != "21C in Tokyo" {
		t.Fatalf("Answer = %q", res.Answer)
	}

	// REPLAYED MANY TIMES, and the round count is a MEASUREMENT rather than
	// belt and braces.
	//
	// Measured 2026-10-02 against a mutation that built the tools array by
	// iterating a Go map: ONE round caught it, but not reliably -- and the
	// naive model ("two tools, so a coin flip per round") is wrong. Go
	// randomises the START OFFSET within a bucket, so for two entries landing
	// in the same bucket the recorded order survives ~7 rounds in 8. Ten
	// rounds therefore missed the mutation about a quarter of the time (it
	// passed on attempt 2 of 3), which is exactly the shape this repo records
	// for cleat#1755: a regression test that catches its own defect less than
	// half the time is a green that reads as coverage.
	//
	// MEASURED at 40 rounds, not inferred from the model above: the mutation
	// failed this test in 12 of 12 consecutive attempts. Re-derive with
	// `go test ./cleat/agentworkflow/ -run TestTheLoopReplaysWithoutDivergence
	// -count=12` against the map-iterating mutation, and expect 12 failures.
	// Rounds are cheap -- the whole suite is well under a second -- so the
	// count is chosen for detection, not for speed.
	const rounds = 40
	for i := 0; i < rounds; i++ {
		env.StartReplay()
		res, err := run(t, env, in)
		if err != nil {
			t.Fatalf("replayed run %d: %v", i+1, err)
		}
		env.AssertReplayDivergence(t, 0)
		if res.Answer != "21C in Tokyo" {
			t.Errorf("round %d: Answer on replay = %q, want the first run's", i+1, res.Answer)
		}
	}
}

// The per-language surface, which is the whole reason the loop is a workflow:
// start the child, await it. Asserted on the RECORDED child call, because a
// wrapper that silently started nothing would still let a stubbed response
// come back and look like success.
func TestRunAsChildStartsAndAwaitsTheAgent(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnChildWorkflow(agentworkflow.ChildName).
		Return(`{"answer":"hi","steps":1,"tool_calls":null}`, nil)

	res, err := agentworkflow.RunAsChild(env.H(), agentworkflow.Input{
		Provider: "openai", Model: "gpt-4o-mini",
		Tools: []agentworkflow.Tool{{
			Name: "t", Kind: agentworkflow.KindService, Service: "s", Operation: "o",
		}},
	}, "hello")
	if err != nil {
		t.Fatalf("RunAsChild: %v", err)
	}
	if res.Answer != "hi" {
		t.Errorf("Answer = %q", res.Answer)
	}

	hist := env.ChildWorkflowCallHistory()
	if len(hist) != 1 || hist[0].Name != agentworkflow.ChildName {
		t.Fatalf("child calls = %+v, want exactly one to %q", hist, agentworkflow.ChildName)
	}
	// The message and the tool list have to REACH the child; a wrapper that
	// dropped either would still return the stub's answer.
	for _, want := range []string{`"message":"hello"`, `"name":"t"`, `"kind":"service"`} {
		if !strings.Contains(hist[0].InputJSON, want) {
			t.Errorf("the child's input %q does not carry %s", hist[0].InputJSON, want)
		}
	}
}

// A tool the model invented is reported BACK to it rather than failing the
// run: the model can correct itself, and the run is still recorded so an
// operator can see it happening.
func TestAnUnknownToolIsReportedToTheModel(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"call_1"}, []string{"nonexistent"}, []string{`{}`}), nil).
		Return(answer("recovered"), nil)

	res, err := run(t, env, agentworkflow.Input{Message: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "recovered" {
		t.Errorf("Answer = %q -- the loop must continue after a bad tool name", res.Answer)
	}
	if len(res.ToolCalls) != 1 || !strings.Contains(res.ToolCalls[0].Error, "no such tool") {
		t.Errorf("ToolCalls = %+v, want the invented name recorded as an error", res.ToolCalls)
	}
}
