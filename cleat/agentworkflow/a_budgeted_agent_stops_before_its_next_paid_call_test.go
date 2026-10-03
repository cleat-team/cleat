package agentworkflow_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/cleat/agentworkflow"
	"github.com/cleat-team/cleat/cleat/cleattest"
)

// The budget, tenant and artifact surface (cleat#3022, part A).
//
// WHY THESE ASSERT ON THE CALL COUNT AND NOT ONLY ON THE STATUS. Input.Budget
// is a ceiling only if it is checked BEFORE the call it guards. A budget
// checked afterwards still reports StatusBudgetExceeded -- on a run that had
// already overshot by one turn's cost. The status alone passes on exactly that
// implementation, which is the one the example's own check exists to prevent,
// so the count is the assertion that can disagree.

// callsCosting is calls(), plus the `cost` the llm plugin reports -- the
// quantity Input.Budget is measured against. cost is a JSON literal ("0.5")
// rather than a float64 so the test does not depend on how Go formats one.
func callsCosting(cost string, ids, names, args []string) string {
	var b strings.Builder
	b.WriteString(`{"choices":[{"message":{"role":"assistant","content":"thinking","tool_calls":[`)
	for i := range names {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":` + quote(ids[i]) + `,"type":"function","function":{"name":` + quote(names[i]) +
			`,"arguments":` + quote(args[i]) + `}}`)
	}
	b.WriteString(`]},"finish_reason":"tool_calls"}],"usage":{"total_tokens":7},"cost":`)
	b.WriteString(cost)
	b.WriteString(`,"model":"test-model"}`)
	return b.String()
}

// answerCosting is answer(), plus a cost.
func answerCosting(content, cost string) string {
	return `{"choices":[{"message":{"role":"assistant","content":` + quote(content) +
		`},"finish_reason":"stop"}],"usage":{"total_tokens":11},"cost":` + cost + `,"model":"test-model"}`
}

// pluginCalls counts this env's calls to one plugin+function.
func pluginCalls(env *cleattest.TestEnv, plugin, function string) int {
	n := 0
	for _, rec := range env.CallHistory() {
		if rec.Service == plugin && rec.Operation == function {
			n++
		}
	}
	return n
}

// loopingTool is one service tool the model can keep asking for, so a test can
// drive the loop past any number of turns without the model ever answering.
var loopingTool = []agentworkflow.Tool{{
	Name: "t", Kind: agentworkflow.KindService, Service: "s", Operation: "o",
}}

func TestABudgetStopsTheRunBeforeTheNextPaidCall(t *testing.T) {
	env := cleattest.NewTestEnv()
	// The model never answers: it asks for a tool on every turn, at $0.50 a turn.
	env.OnPluginCall("llm", "chat").
		Return(callsCosting("0.5", []string{"c1"}, []string{"t"}, []string{`{}`}), nil)

	res, err := run(t, env, agentworkflow.Input{
		Message:  "spend",
		MaxSteps: 10, // well past what the budget allows, so the BUDGET is what stops it
		Budget:   1.5,
		Tools:    loopingTool,
	})
	if err != nil {
		t.Fatalf("a budget stop is an OUTCOME, not an error; Run returned %v", err)
	}
	if res.Status != agentworkflow.StatusBudgetExceeded {
		t.Errorf("Status = %q, want %q", res.Status, agentworkflow.StatusBudgetExceeded)
	}
	if got := pluginCalls(env, "llm", "chat"); got != 3 {
		t.Errorf("llm.chat called %d times, want exactly 3 ($1.50 at $0.50 a turn).\n"+
			"A 4th call means the loop kept spending after the ceiling was REACHED -- the comparison "+
			"is `>=`, not `>`. (The turn that reaches the ceiling is unavoidable either way: its "+
			"cost is only known after it returns.)", got)
	}
	if res.Cost != 1.5 {
		t.Errorf("Cost = %v, want 1.5", res.Cost)
	}
	if res.Steps != 3 {
		t.Errorf("Steps = %d, want 3", res.Steps)
	}
}

// A budget of 0 means UNBOUNDED, not "stop immediately". The seam's condition
// is `in.Budget > 0 && ...`, and this is the case that pins the first half --
// without it, a plausible implementation that stopped on `Cost >= Budget`
// would refuse every unbudgeted run.
func TestABudgetOfZeroIsUnboundedNotImmediate(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("llm", "chat").Return(answerCosting("done", "9.99"), nil)

	res, err := run(t, env, agentworkflow.Input{Message: "hi", Budget: 0})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != agentworkflow.StatusDone {
		t.Errorf("Status = %q, want %q -- a zero budget must not stop a run", res.Status, agentworkflow.StatusDone)
	}
	if res.Answer != "done" {
		t.Errorf("Answer = %q, want %q", res.Answer, "done")
	}
}

// The unbudgeted path is what every existing caller uses, so it must be
// unchanged: one turn, the answer, the cost accumulated.
func TestAnUnbudgetedRunIsUnchanged(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("llm", "chat").Return(answerCosting("42", "0.25"), nil)

	res, err := run(t, env, agentworkflow.Input{Message: "what is the answer?"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != agentworkflow.StatusDone {
		t.Errorf("Status = %q, want %q", res.Status, agentworkflow.StatusDone)
	}
	if res.Answer != "42" {
		t.Errorf("Answer = %q, want %q", res.Answer, "42")
	}
	if res.Cost != 0.25 {
		t.Errorf("Cost = %v, want 0.25", res.Cost)
	}
	if got := pluginCalls(env, "llm", "chat"); got != 1 {
		t.Errorf("llm.chat called %d times, want 1", got)
	}
}

func TestAnArtifactIsWrittenUnderTheKeyTheCallerChose(t *testing.T) {
	const body = "the report body"
	env := cleattest.NewTestEnv()
	env.OnPluginCall("llm", "chat").Return(answerCosting(body, "0"), nil)
	env.OnPluginCall("blobstore", "put").Return(`{"key":"acme-report"}`, nil)

	res, err := run(t, env, agentworkflow.Input{Message: "write it", ArtifactKey: "acme-report"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ArtifactKey != "acme-report" {
		t.Errorf("ArtifactKey = %q, want %q", res.ArtifactKey, "acme-report")
	}
	if got := pluginCalls(env, "blobstore", "put"); got != 1 {
		t.Fatalf("blobstore.put called %d times, want 1", got)
	}

	// The payload, not only the count: the key the caller chose, and the
	// answer base64'd in the shape the bundled blobstore plugin takes.
	var put struct {
		Key  string `json:"key"`
		Data string `json:"data"`
	}
	for _, rec := range env.CallHistory() {
		if rec.Service == "blobstore" && rec.Operation == "put" {
			if err := json.Unmarshal([]byte(rec.Request), &put); err != nil {
				t.Fatalf("blobstore.put request %q is not JSON: %v", rec.Request, err)
			}
		}
	}
	if put.Key != "acme-report" {
		t.Errorf("put key = %q, want %q -- the caller's key, not a fixed one", put.Key, "acme-report")
	}
	if want := base64.StdEncoding.EncodeToString([]byte(body)); put.Data != want {
		t.Errorf("put data = %q, want %q", put.Data, want)
	}
}

// The negative control for the artifact: with no key, nothing is written. A
// write guarded by the wrong condition passes the test above and fails this one.
func TestNoArtifactIsWrittenWithoutAKey(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("llm", "chat").Return(answerCosting("body", "0"), nil)

	res, err := run(t, env, agentworkflow.Input{Message: "no artifact"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := pluginCalls(env, "blobstore", "put"); got != 0 {
		t.Errorf("blobstore.put called %d times with no artifact_key, want 0", got)
	}
	if res.ArtifactKey != "" {
		t.Errorf("ArtifactKey = %q, want empty", res.ArtifactKey)
	}
}

// A run stopped by its budget produced no answer, so there is nothing to write.
// This is the interaction between the two halves of the surface, and it is the
// one a naive "write the result at the end" would get wrong.
func TestABudgetedRunWritesNoArtifact(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("llm", "chat").
		Return(callsCosting("1", []string{"c1"}, []string{"t"}, []string{`{}`}), nil)

	res, err := run(t, env, agentworkflow.Input{
		Message: "spend", MaxSteps: 10, Budget: 1, Tools: loopingTool, ArtifactKey: "k",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != agentworkflow.StatusBudgetExceeded {
		t.Fatalf("Status = %q, want %q", res.Status, agentworkflow.StatusBudgetExceeded)
	}
	if got := pluginCalls(env, "blobstore", "put"); got != 0 {
		t.Errorf("blobstore.put called %d times on a budget-stopped run, want 0 -- there is no answer to write", got)
	}
}

// TenantID is echoed and NOT required. Both halves matter: an echo that is not
// asserted can be dropped silently, and a requirement that crept in would
// refuse every caller without a tenant -- which is most of them.
func TestATenantIsEchoedAndNotRequired(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("llm", "chat").Return(answerCosting("ok", "0"), nil)

	withTenant, err := run(t, env, agentworkflow.Input{Message: "hi", TenantID: "acme"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if withTenant.TenantID != "acme" {
		t.Errorf("TenantID = %q, want %q echoed back", withTenant.TenantID, "acme")
	}

	noTenant := cleattest.NewTestEnv()
	noTenant.OnPluginCall("llm", "chat").Return(answerCosting("ok", "0"), nil)
	res, err := run(t, noTenant, agentworkflow.Input{Message: "hi"})
	if err != nil {
		t.Fatalf("a run with no tenant must succeed -- TenantID is echoed, not required: %v", err)
	}
	if res.TenantID != "" {
		t.Errorf("TenantID = %q, want empty", res.TenantID)
	}
}
