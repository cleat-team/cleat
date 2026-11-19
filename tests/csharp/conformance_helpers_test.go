//go:build cgo

package csharp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// Helpers the group tests share (cleat#4080). They live beside the harness, not in any one group's file, so that a group deleting or
// reshaping its own test file cannot take another group's helpers with it.

// decodeDoc parses an entry's result as a JSON object.
func decodeDoc(t *testing.T, result string) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal([]byte(result), &d); err != nil {
		t.Fatalf("the entry's result is not a JSON object: %v\n%s", err, result)
	}
	return d
}

// opsOf lists the host calls the guest made to the recording caller, as "service/operation", in order.
func opsOf(res conformanceResult) []string {
	out := []string{}
	for _, c := range res.Calls {
		out = append(out, c.Service+"/"+c.Operation)
	}
	return out
}

// mustFinish fails the test unless the run completed, and returns its result document.
func mustFinish(t *testing.T, res conformanceResult) map[string]any {
	t.Helper()
	if res.Err != nil {
		t.Fatalf("the run failed: %v", res.Err)
	}
	if res.Suspended != nil {
		t.Fatalf("the run suspended (%s) where it should have finished", res.Suspended.Reason)
	}
	return decodeDoc(t, res.Result)
}

// moveRecordsBack makes every recorded event of this type look older by ms, so a replayed wait finds its deadline passed. It insists on
// exactly one such record: moving none would leave a replay that never times out, and the test built on it would pass for no reason.
func moveRecordsBack(t *testing.T, hist []engine.EventRecord, typ engine.EventType, ms int64) []engine.EventRecord {
	t.Helper()
	moved := 0
	for i := range hist {
		if hist[i].EventType == typ {
			hist[i].TimestampMs -= ms
			moved++
		}
	}
	if moved != 1 {
		t.Fatalf("expected exactly one %s record to move, found %d in %+v", typ, moved, hist)
	}
	return hist
}

// ---- the helpers' own tests: they are shared, so a wrong one would be wrong in every group ---------------------------------------

func TestCSharpHelpersOpsOfKeepsOrderAndNamesBothParts(t *testing.T) {
	res := conformanceResult{Calls: []recordedCall{{Service: "a", Operation: "x"}, {Service: "b", Operation: "y"}}}
	if got := opsOf(res); len(got) != 2 || got[0] != "a/x" || got[1] != "b/y" {
		t.Errorf("opsOf = %v, want [a/x b/y]", got)
	}
	if got := opsOf(conformanceResult{}); got == nil || len(got) != 0 {
		t.Errorf("opsOf of no calls = %#v, want an empty non-nil slice", got)
	}
}

func TestCSharpHelpersMustFinishReturnsTheDocumentOfACompletedRun(t *testing.T) {
	got := mustFinish(t, conformanceResult{Result: `{"k":"v"}`})
	if got["k"] != "v" {
		t.Errorf("mustFinish = %v, want the decoded result", got)
	}
}

func TestCSharpHelpersMoveRecordsBackMovesOnlyTheNamedType(t *testing.T) {
	hist := []engine.EventRecord{
		{EventType: engine.EventType("a"), TimestampMs: 1000},
		{EventType: engine.EventType("b"), TimestampMs: 1000},
	}
	got := moveRecordsBack(t, hist, engine.EventType("a"), 400)
	if got[0].TimestampMs != 600 || got[1].TimestampMs != 1000 {
		t.Errorf("timestamps = %d, %d; want 600 (moved) and 1000 (untouched)", got[0].TimestampMs, got[1].TimestampMs)
	}
}

// The harness's own tests for what it offers the groups. They need no guest: the caller is a plain Go value.

func TestCSharpHarnessCallerCountsPerOperationAndRecordsAFailedCall(t *testing.T) {
	c := &recordingCaller{fail: func(service, operation string, nth int) error {
		if service == "svc" && operation == "op" && nth == 2 {
			return errors.New("second call fails")
		}
		return nil
	}}
	ctx := context.Background()
	if _, err := c.Call(ctx, "svc", "op", "{}"); err != nil {
		t.Fatalf("call 1 failed: %v", err)
	}
	if _, err := c.Call(ctx, "svc", "other", "{}"); err != nil {
		t.Fatalf("a different operation shares the count: %v", err)
	}
	if _, err := c.Call(ctx, "svc", "op", "{}"); err == nil || err.Error() != "second call fails" {
		t.Fatalf("call 2 of svc/op = %v, want the scripted failure", err)
	}
	if _, err := c.Call(ctx, "svc", "op", "{}"); err != nil {
		t.Fatalf("call 3 failed: %v", err)
	}
	if got := len(c.recorded()); got != 4 {
		t.Errorf("recorded %d calls, want all 4 including the failed one", got)
	}
}

// Replaying a run's own history reproduces its result, which is what a group relies on when it replays a recorded wait.
func TestCSharpHarnessReplayOfARunsOwnHistoryReproducesItsResult(t *testing.T) {
	first := runConformanceEntry(t, "smoke", entryRun{WorkflowID: "wf-replay", Input: `{"x":1}`})
	if first.Err != nil {
		t.Fatalf("the fresh run failed: %v", first.Err)
	}
	again := runConformanceEntry(t, "smoke", entryRun{WorkflowID: "wf-replay", Input: `{"x":1}`, Replay: append([]engine.EventRecord{}, first.History...)})
	if again.Err != nil || again.Result != first.Result {
		t.Errorf("replay gave err=%v result=%q, want the fresh run's %q", again.Err, again.Result, first.Result)
	}
}
