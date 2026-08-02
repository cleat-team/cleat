package workflow

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/cleat/cleattest"
)

// --- Counting HostCalls mock ---

type signalCall struct {
	targetRunID string
	signalName  string
	payload     string
}

type countingHostCalls struct {
	responses []PluginCallOutput
	callIdx   int
	signals   []signalCall
}

func (c *countingHostCalls) pluginCall(pluginName, functionName, inputJSON string) (string, error) {
	if c.callIdx >= len(c.responses) {
		return "", fmt.Errorf("unexpected call #%d (only %d responses registered)", c.callIdx, len(c.responses))
	}
	resp := c.responses[c.callIdx]
	c.callIdx++
	return mustMarshalJSON(resp), nil
}

func (c *countingHostCalls) signalWorkflow(targetRunID, signalName, payload string) error {
	c.signals = append(c.signals, signalCall{targetRunID, signalName, payload})
	return nil
}

func newCountingHostCalls(responses []PluginCallOutput) (*countingHostCalls, cleat.HostCalls) {
	ch := &countingHostCalls{responses: responses}
	h := cleat.NewHostCalls(cleat.HostCallsOptions{
		PluginCall:     ch.pluginCall,
		SignalWorkflow: ch.signalWorkflow,
	})
	return ch, h
}

func mustMarshalJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("json.Marshal: %v", err))
	}
	return string(b)
}

// passResp is a PluginCallOutput for a successful phase (used for both work
// and review phases).
var passResp = PluginCallOutput{ExitCode: 0, ReviewOutcome: "PASS"}

// --- Tests ---

func TestTaskLifecycleWorkflowHappyPath(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("clew-executor", "run_phase").Return(mustMarshalJSON(PluginCallOutput{
		ExitCode: 0, PhaseChanged: true, ReviewOutcome: "PASS",
	}), nil)

	result, err := HandleIncident(env.H(), "clew-test-001", "/tmp/t", "clew", "/tmp", "", "", "")
	if err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	var out TaskOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshaling output: %v", err)
	}
	if out.FinalPhase != "done" {
		t.Errorf("expected done, got %s", out.FinalPhase)
	}
	if len(out.PhasesCompleted) != 5 {
		t.Errorf("expected 5 phases completed, got %d: %v", len(out.PhasesCompleted), out.PhasesCompleted)
	}
	if out.TotalPluginCalls != 5 {
		t.Errorf("expected 5 calls, got %d", out.TotalPluginCalls)
	}
}

func TestReviewLoopSHOULDFIXThenPASS(t *testing.T) {
	// Sequence: [PASS(explore), PASS(plan), SHOULD_FIX(review_plan),
	//             PASS(re-plan), PASS(re-review), PASS(implement), PASS(review_impl)]
	responses := []PluginCallOutput{
		passResp, // 0: explore
		passResp, // 1: plan
		{ExitCode: 0, ReviewOutcome: "SHOULD_FIX"}, // 2: review_plan → loop
		passResp, // 3: re-plan (round 1)
		passResp, // 4: re-review (round 1) → PASS
		passResp, // 5: implement
		passResp, // 6: review_impl
	}
	ch, h := newCountingHostCalls(responses)

	result, err := HandleIncident(h, "clew-test-002", "/tmp/t", "clew", "/tmp", "", "", "")
	if err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	var out TaskOutput
	json.Unmarshal([]byte(result), &out)
	if out.FinalPhase != "done" {
		t.Errorf("expected done, got %s", out.FinalPhase)
	}
	if out.TotalPluginCalls != 7 {
		t.Errorf("expected 7 calls, got %d", out.TotalPluginCalls)
	}
	if ch.callIdx != 7 {
		t.Errorf("expected 7 calls consumed, got %d", ch.callIdx)
	}
}

func TestReviewLoopMaxRoundsExhausted(t *testing.T) {
	// Sequence: PASS(explore), PASS(plan), SHOULD_FIX(review_plan),
	// then 3 rounds of [PASS(re-plan), SHOULD_FIX(re-review)] → 9 total
	responses := []PluginCallOutput{
		passResp, // 0: explore
		passResp, // 1: plan
		{ExitCode: 0, ReviewOutcome: "SHOULD_FIX"}, // 2: review_plan → loop
		passResp, // 3: re-plan round 1
		{ExitCode: 0, ReviewOutcome: "SHOULD_FIX"}, // 4: re-review round 1
		passResp, // 5: re-plan round 2
		{ExitCode: 0, ReviewOutcome: "SHOULD_FIX"}, // 6: re-review round 2
		passResp, // 7: re-plan round 3
		{ExitCode: 0, ReviewOutcome: "SHOULD_FIX"}, // 8: re-review round 3
	}
	ch, h := newCountingHostCalls(responses)

	result, err := HandleIncident(h, "clew-test-003", "/tmp/t", "clew", "/tmp", "", "", "")
	if err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	var out TaskOutput
	json.Unmarshal([]byte(result), &out)
	if out.FinalPhase != "failed" {
		t.Errorf("expected failed, got %s", out.FinalPhase)
	}
	if out.TotalPluginCalls != 9 {
		t.Errorf("expected 9 calls, got %d", out.TotalPluginCalls)
	}
	if ch.callIdx != 9 {
		t.Errorf("expected 9 calls consumed, got %d", ch.callIdx)
	}
	// Neither review_plan should be in completed (review never passed).
	if len(out.PhasesCompleted) != 2 {
		t.Errorf("expected 2 phases completed, got %d: %v", len(out.PhasesCompleted), out.PhasesCompleted)
	}
}

func TestBLOCKEROnFirstReview(t *testing.T) {
	responses := []PluginCallOutput{
		passResp,                                // 0: explore
		passResp,                                // 1: plan
		{ExitCode: 0, ReviewOutcome: "BLOCKER"}, // 2: review_plan → BLOCKER
	}
	ch, h := newCountingHostCalls(responses)

	result, err := HandleIncident(h, "clew-test-004", "/tmp/t", "clew", "/tmp", "", "", "")
	if err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	var out TaskOutput
	json.Unmarshal([]byte(result), &out)
	if out.FinalPhase != "failed" {
		t.Errorf("expected failed, got %s", out.FinalPhase)
	}
	if out.TotalPluginCalls != 3 {
		t.Errorf("expected 3 calls, got %d", out.TotalPluginCalls)
	}
	if len(out.PhasesCompleted) != 2 {
		t.Errorf("expected 2 phases completed (explore, plan), got %d: %v", len(out.PhasesCompleted), out.PhasesCompleted)
	}
	if ch.callIdx != 3 {
		t.Errorf("expected 3 calls consumed, got %d", ch.callIdx)
	}
}

func TestBLOCKERDuringReviewLoop(t *testing.T) {
	// First review SHOULD_FIX → re-plan PASS → second review BLOCKER.
	responses := []PluginCallOutput{
		passResp, // 0: explore
		passResp, // 1: plan
		{ExitCode: 0, ReviewOutcome: "SHOULD_FIX"}, // 2: review_plan → loop
		passResp,                                // 3: re-plan round 1
		{ExitCode: 0, ReviewOutcome: "BLOCKER"}, // 4: re-review round 1 → BLOCKER
	}
	ch, h := newCountingHostCalls(responses)

	result, err := HandleIncident(h, "clew-test-005", "/tmp/t", "clew", "/tmp", "", "", "")
	if err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	var out TaskOutput
	json.Unmarshal([]byte(result), &out)
	if out.FinalPhase != "failed" {
		t.Errorf("expected failed, got %s", out.FinalPhase)
	}
	if out.TotalPluginCalls != 5 {
		t.Errorf("expected 5 calls, got %d", out.TotalPluginCalls)
	}
	if ch.callIdx != 5 {
		t.Errorf("expected 5 calls consumed, got %d", ch.callIdx)
	}
}

func TestPluginError(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("clew-executor", "run_phase").Return("", fmt.Errorf("executor down"))

	result, err := HandleIncident(env.H(), "clew-test-006", "/tmp/t", "clew", "/tmp", "", "", "")
	if err != nil {
		t.Fatalf("workflow should not propagate plugin errors: %v", err)
	}

	var out TaskOutput
	json.Unmarshal([]byte(result), &out)
	if out.FinalPhase != "blocked" {
		t.Errorf("expected blocked, got %s", out.FinalPhase)
	}
	if out.TotalPluginCalls != 1 {
		t.Errorf("expected 1 call, got %d", out.TotalPluginCalls)
	}
}

func TestNonZeroExitCode(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("clew-executor", "run_phase").Return(mustMarshalJSON(PluginCallOutput{
		ExitCode: 1, ReviewOutcome: "PASS",
	}), nil)

	result, err := HandleIncident(env.H(), "clew-test-007", "/tmp/t", "clew", "/tmp", "", "", "")
	if err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	var out TaskOutput
	json.Unmarshal([]byte(result), &out)
	if out.FinalPhase != "failed" {
		t.Errorf("expected failed, got %s", out.FinalPhase)
	}
}

func TestRePlanNonZeroExit(t *testing.T) {
	// Review SHOULD_FIX → re-plan crashes (exit_code=1).
	responses := []PluginCallOutput{
		passResp, // 0: explore
		passResp, // 1: plan
		{ExitCode: 0, ReviewOutcome: "SHOULD_FIX"}, // 2: review_plan → loop
		{ExitCode: 1, ReviewOutcome: "PASS"},       // 3: re-plan → crash
	}
	ch, h := newCountingHostCalls(responses)

	result, err := HandleIncident(h, "clew-test-008", "/tmp/t", "clew", "/tmp", "", "", "")
	if err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	var out TaskOutput
	json.Unmarshal([]byte(result), &out)
	if out.FinalPhase != "failed" {
		t.Errorf("expected failed, got %s", out.FinalPhase)
	}
	if out.TotalPluginCalls != 4 {
		t.Errorf("expected 4 calls, got %d", out.TotalPluginCalls)
	}
	if ch.callIdx != 4 {
		t.Errorf("expected 4 calls consumed, got %d", ch.callIdx)
	}
}

func TestExploreNeedsDecomposition(t *testing.T) {
	responses := []PluginCallOutput{
		{ExitCode: 0, PhaseChanged: true, NewPhase: "waiting_on_children"},
	}
	ch, h := newCountingHostCalls(responses)

	result, err := HandleIncident(h, "clew-test-009", "/tmp/t", "clew", "/tmp", "", "", "")
	if err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	var out TaskOutput
	json.Unmarshal([]byte(result), &out)
	if out.FinalPhase != "waiting_on_children" {
		t.Errorf("expected waiting_on_children, got %s", out.FinalPhase)
	}
	if out.TotalPluginCalls != 1 {
		t.Errorf("expected 1 call, got %d", out.TotalPluginCalls)
	}
	if ch.callIdx != 1 {
		t.Errorf("expected 1 call consumed, got %d", ch.callIdx)
	}
}

func TestExploreSkipDone(t *testing.T) {
	responses := []PluginCallOutput{
		{ExitCode: 0, PhaseChanged: true, NewPhase: "done"},
	}
	_, h := newCountingHostCalls(responses)

	result, err := HandleIncident(h, "clew-test-010", "/tmp/t", "clew", "/tmp", "", "", "")
	if err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	var out TaskOutput
	json.Unmarshal([]byte(result), &out)
	if out.FinalPhase != "done" {
		t.Errorf("expected done, got %s", out.FinalPhase)
	}
	if out.TotalPluginCalls != 1 {
		t.Errorf("expected 1 call, got %d", out.TotalPluginCalls)
	}
}

func TestParentSignalOnSuccess(t *testing.T) {
	responses := []PluginCallOutput{
		passResp, passResp, passResp, passResp, passResp,
	}
	ch, h := newCountingHostCalls(responses)

	result, err := HandleIncident(h, "clew-test-011", "/tmp/t", "clew", "/tmp", "", "", "parent-run-123")
	if err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	var out TaskOutput
	json.Unmarshal([]byte(result), &out)
	if out.FinalPhase != "done" {
		t.Errorf("expected done, got %s", out.FinalPhase)
	}
	if len(ch.signals) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(ch.signals))
	}
	sig := ch.signals[0]
	if sig.targetRunID != "parent-run-123" {
		t.Errorf("expected targetRunID parent-run-123, got %s", sig.targetRunID)
	}
	if sig.signalName != "child_done" {
		t.Errorf("expected signalName child_done, got %s", sig.signalName)
	}
	if sig.payload != "clew-test-011" {
		t.Errorf("expected payload clew-test-011, got %s", sig.payload)
	}
}

func TestParentSignalNotSentOnBlocked(t *testing.T) {
	responses := []PluginCallOutput{
		passResp,                                // 0: explore
		passResp,                                // 1: plan
		{ExitCode: 0, ReviewOutcome: "BLOCKER"}, // 2: review_plan → BLOCKER
	}
	ch, h := newCountingHostCalls(responses)

	result, err := HandleIncident(h, "clew-test-012", "/tmp/t", "clew", "/tmp", "", "", "parent-run-123")
	if err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	var out TaskOutput
	json.Unmarshal([]byte(result), &out)
	if out.FinalPhase != "failed" {
		t.Errorf("expected failed, got %s", out.FinalPhase)
	}
	if len(ch.signals) != 0 {
		t.Errorf("expected 0 signals, got %d", len(ch.signals))
	}
}

func TestParentSignalNotSentWhenEmpty(t *testing.T) {
	responses := []PluginCallOutput{
		passResp, passResp, passResp, passResp, passResp,
	}
	ch, h := newCountingHostCalls(responses)

	result, err := HandleIncident(h, "clew-test-013", "/tmp/t", "clew", "/tmp", "", "", "")
	if err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	var out TaskOutput
	json.Unmarshal([]byte(result), &out)
	if out.FinalPhase != "done" {
		t.Errorf("expected done, got %s", out.FinalPhase)
	}
	if len(ch.signals) != 0 {
		t.Errorf("expected 0 signals, got %d", len(ch.signals))
	}
}

func TestInvalidInputJSON(t *testing.T) {
	env := cleattest.NewTestEnv()
	_, err := HandleIncident(env.H(), "", "", "", "", "", "", "")
	if err != nil {
		t.Fatal("expected HandleIncident to succeed even with empty args")
	}
}
