package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// DurableLog's body was `return 0`. Both backends read the guest's message
// across the WASM boundary and handed it to a function that dropped it, so
// h.LogKV(...) produced nothing anywhere. cleat#1308.
//
// The comment that sat on it said "Log output goes via the worker's
// stdout/stderr capture", which was not true of anything.
//
// WHY THIS IS A DEFECT AND NOT A MISSING FEATURE. docs/workflow-go-constraints.md
// blocks fmt.Println under E015 and directs the author to h.DurableLog()
// instead. So the linter took away a call that printed and recommended one
// that did nothing: following the documented advice LOST the author's logging.
func TestLogKVOutputReachesTheLogger(t *testing.T) {
	var buf bytes.Buffer
	eng := NewEngine(nil, nil, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	s := &execSession{engine: eng, workflowID: "wf-1308", tenantID: DefaultTenantUUID, stepCount: 3}

	if got := s.DurableLog(context.Background(), nil, "payment processed"); got != 0 {
		t.Errorf("DurableLog returned %d, want 0 -- the guest ABI expects 0 and nothing else", got)
	}

	out := buf.String()
	for _, want := range []string{
		"payment processed", // the message itself
		"wf-1308",           // which workflow
		"workflow",          // that it came from guest code rather than the worker
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the log line does not contain %q.\n\nGot: %s", want, out)
		}
	}
}

// A replayed run must not re-emit lines the original already emitted.
//
// The same reason a durable call is not re-issued on replay, applied to output
// instead of effects -- and it is the one determinism property this call has,
// since it records no event to match against.
func TestLogKVDoesNotReEmitOnReplay(t *testing.T) {
	var buf bytes.Buffer
	eng := NewEngine(nil, nil, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	s := &execSession{engine: eng, workflowID: "wf-1308", isReplay: true}

	s.DurableLog(context.Background(), nil, "payment processed")

	if strings.Contains(buf.String(), "payment processed") {
		t.Errorf("a replayed run re-emitted a log line the original execution already emitted.\n\nGot: %s",
			buf.String())
	}
}

// The control for the test above: without it, a DurableLog that emitted
// nothing under ANY condition would pass it.
func TestTheReplayGuardIsNotUnconditional(t *testing.T) {
	var buf bytes.Buffer
	eng := NewEngine(nil, nil, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	fresh := &execSession{engine: eng, workflowID: "wf-1308"}
	replay := &execSession{engine: eng, workflowID: "wf-1308", isReplay: true}

	fresh.DurableLog(context.Background(), nil, "fresh line")
	replay.DurableLog(context.Background(), nil, "replayed line")

	out := buf.String()
	if !strings.Contains(out, "fresh line") {
		t.Errorf("a fresh run emitted nothing, so the replay assertion proves nothing.\n\nGot: %s", out)
	}
	if strings.Contains(out, "replayed line") {
		t.Errorf("the replay guard did not hold.\n\nGot: %s", out)
	}
}

// EventTypeDurableLog still has no producer, and this pins that rather than
// leaving it to be rediscovered.
//
// The type exists (types.go), carries a compaction code and both codec
// directions (compaction.go), has Message/LogLevel/LogKV fields on
// EventRecord with a payload carrier for them (store_events.go), and is
// referenced by four test files -- all of which pass against an event no
// production path emits. Only the recording call is missing, and adding it is
// a replay-compatibility decision (see DurableLog's doc), not an oversight.
//
// When somebody makes that decision and wires it up, this test fails and the
// three documents corrected alongside it need correcting back. That is the
// point: the claim and the code are pinned together rather than drifting apart
// again, which is how they got three documents out of step in the first place.
func TestDurableLogRecordsNoEvent(t *testing.T) {
	var buf bytes.Buffer
	eng := NewEngine(nil, nil, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	s := &execSession{engine: eng, workflowID: "wf-1308"}

	before := len(s.history)
	s.DurableLog(context.Background(), nil, "payment processed")

	if len(s.history) != before {
		t.Errorf("DurableLog recorded an event. If that is intended, three documents now need " +
			"updating -- PRINCIPLES.md, docs/reference/sdk-api.md and " +
			"docs/workflow-go-constraints.md all describe LogKV's durability, and this test " +
			"exists so they are changed in the same breath rather than years later (cleat#1308).")
	}
}
