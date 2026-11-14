package engine

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// driveWaitForEvent runs one WaitForEvent across as many segments as it takes,
// the way a worker does: each segment replays the history so far, the loop
// records its next attempt and await, the run suspends, and the next segment
// starts once that await's deadline has passed.
//
// The claim answers found=false until segment foundAtSegment, then found=true.
// Timestamps are VIRTUAL: after each segment the records it wrote are re-stamped
// from a fake clock, and the engine's real clock (which decides whether a recorded
// await has expired) is that same fake clock, so a wait of hours takes
// microseconds. What is recorded is the shape and the TimeoutMs values, which is
// what replay reads.
func driveWaitForEvent(t *testing.T, startHistory []EventRecord, timeoutMs int64, foundAtSegment int) (hist []EventRecord, claimed string, segments int) {
	t.Helper()
	base := time.Now().UnixMilli()
	clock := base
	hist = append([]EventRecord(nil), startHistory...)

	for seg := 0; seg < 10_000; seg++ {
		counter := &eventClaimCounter{}
		if seg >= foundAtSegment {
			counter.outputs = []string{`{"found":true,"payload":"PAID"}`}
		}
		pr := NewPluginRegistry()
		if err := pr.Register("webhook-ingest", "await_webhook", counter.call); err != nil {
			t.Fatal(err)
		}
		now := clock
		eng := NewEngine(nil, &mockCaller{}, WithPluginRegistry(pr), WithWorkflowID("wf-1"),
			WithClock(func() int64 { return now }))
		s := &execSession{
			engine: eng, workflowID: "wf-1", nowMs: base,
			deferrals: map[string]string{}, queryState: map[string]string{},
			isReplay: len(hist) > 0, history: append([]EventRecord(nil), hist...),
		}

		buf := make([]byte, 512)
		ctx := contextWithRawMemBuf(context.Background(), buf)
		packed := s.WaitForEvent(ctx, nil, "webhook-ingest", "await_webhook", "{}",
			`["__evt:payment"]`, timeoutMs, 0, 400)

		prior := len(hist)
		hist = append([]EventRecord(nil), s.history...)
		for i := prior; i < len(hist); i++ {
			hist[i].TimestampMs = clock
		}
		if packed != callSuspendSentinel {
			n := uint32(uint64(packed) >> 40)
			return hist, string(buf[:n]), seg + 1
		}
		last := hist[len(hist)-1]
		if last.EventType != EventTypeAwaitSignals {
			t.Fatalf("segment %d suspended with %s last, want %s", seg, last.EventType, EventTypeAwaitSignals)
		}
		clock = last.TimestampMs + last.TimeoutMs + 1
	}
	t.Fatal("the wait never finished")
	return nil, "", 0
}

// awaitTimeouts returns the TimeoutMs of every await_signals record, in order.
func awaitTimeouts(hist []EventRecord) []int64 {
	var out []int64
	for _, r := range hist {
		if r.EventType == EventTypeAwaitSignals {
			out = append(out, r.TimeoutMs)
		}
	}
	return out
}

func TestEventRecheckDelayDoublesToTheCap(t *testing.T) {
	want := []int64{6000, 12000, 24000, 48000, 96000, 192000, 300000, 300000, 300000}
	for attempt, w := range want {
		if got := eventRecheckDelayMs(attempt); got != w {
			t.Errorf("attempt %d waits %d ms, want %d", attempt, got, w)
		}
	}
	if eventRecheckMaxMs != 300_000 {
		t.Errorf("the cap is %d ms; the decision recorded for cleat#3806 is 300 s", eventRecheckMaxMs)
	}
	// A long-lived wait must not overflow its way past the cap.
	if got := eventRecheckDelayMs(1 << 20); got != eventRecheckMaxMs {
		t.Errorf("attempt 2^20 waits %d ms, want the cap %d", got, eventRecheckMaxMs)
	}
}

// A fresh wait records the schedule, and a wait of a day costs hundreds of events,
// not tens of thousands.
func TestAFreshWaitForEventBacksOffAndItsHistoryStaysBounded(t *testing.T) {
	// 280 misses at the cap is about 23 hours of waiting.
	hist, claimed, segs := driveWaitForEvent(t, nil, 48*3600*1000, 280)
	if claimed != `{"found":true,"payload":"PAID"}` {
		t.Fatalf("claimed %q", claimed)
	}
	awaits := awaitTimeouts(hist)
	ramp := []int64{6000, 12000, 24000, 48000, 96000, 192000}
	for i, w := range ramp {
		if awaits[i] != w {
			t.Fatalf("await %d recorded TimeoutMs %d, want %d (awaits so far: %v)", i, awaits[i], w, awaits[:i+1])
		}
	}
	var virtualMs int64
	for i, w := range awaits {
		if i >= len(ramp) && w != eventRecheckMaxMs {
			t.Fatalf("await %d recorded %d, want the cap %d", i, w, eventRecheckMaxMs)
		}
		virtualMs += w
	}
	if hours := virtualMs / 3_600_000; hours < 22 {
		t.Fatalf("the driver waited only %d h, so the bound below proves nothing", hours)
	}
	// What the 6 s loop would have recorded for the same wait is 2 events per 6 s:
	// 2 * (virtualMs / 6000), about 27,000. This is the cleat#3806 number.
	old := 2 * (virtualMs / 6000)
	if int64(len(hist)) > old/20 {
		t.Errorf("%d events for %d virtual ms; the 6 s loop would have written about %d, "+
			"and this change is worth at least 20x", len(hist), virtualMs, old)
	}
	if segs != 281 {
		t.Errorf("segments = %d, want 281 (280 misses and the claim)", segs)
	}
}

// The compatibility claim: a history the 6 s code recorded replays under this
// code with no plugin call the history does not answer, and returns the same
// claim. The fixture was written by the code BEFORE this change (see its
// _comment), so this is a replay of what shipped, not of what this code writes.
func loadPre3806History(t *testing.T) (hist []EventRecord, claimed string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/wait_for_event_history_pre3806.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Claimed string        `json:"claimed"`
		History []EventRecord `json:"history"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.History) != 15 {
		t.Fatalf("the fixture has %d records, want 15; it was regenerated or damaged", len(doc.History))
	}
	for _, w := range awaitTimeouts(doc.History) {
		if w != 6000 {
			t.Fatalf("the fixture holds an await of %d ms; it must be a history the 6 s code wrote", w)
		}
	}
	return doc.History, doc.Claimed
}

func TestAHistoryRecordedBeforeTheBackoffReplaysUnchanged(t *testing.T) {
	hist, claimed := loadPre3806History(t)
	counter := &eventClaimCounter{}
	s := waitForEventSession(t, counter, hist, true)

	buf := make([]byte, 512)
	got, ok := callWaitForEvent(s, buf)
	if !ok {
		t.Fatalf("replaying the recorded history failed: %q", string(buf[:64]))
	}
	if got != claimed {
		t.Errorf("replay returned %q, the recording claimed %q", got, claimed)
	}
	if counter.calls != 0 {
		t.Fatalf("replay called the plugin %d time(s); a recorded history answers every claim", counter.calls)
	}
	if s.stepCount != len(hist) {
		t.Errorf("replay consumed %d of %d records", s.stepCount, len(hist))
	}
}

// A run that was already waiting when this change shipped: its history ends at an
// await the 6 s code wrote. It resumes, and its next fresh wait is the schedule's
// value for its POSITION (the cap, after seven iterations), not 6 s again.
func TestAnInFlightWaitContinuesOnTheNewScheduleFromWhereItWas(t *testing.T) {
	hist, _ := loadPre3806History(t)
	// Cut after the seventh await: records 0..13 are seven (plugin_call, await) pairs.
	inFlight := hist[:14]
	if inFlight[13].EventType != EventTypeAwaitSignals {
		t.Fatalf("record 13 is %s, want await_signals", inFlight[13].EventType)
	}

	out, claimed, _ := driveWaitForEvent(t, inFlight, 24*3600*1000, 3)
	if claimed != `{"found":true,"payload":"PAID"}` {
		t.Fatalf("claimed %q", claimed)
	}
	awaits := awaitTimeouts(out)
	for i := 0; i < 7; i++ {
		if awaits[i] != 6000 {
			t.Fatalf("recorded await %d was rewritten to %d ms", i, awaits[i])
		}
	}
	if len(awaits) < 9 {
		t.Fatalf("only %d awaits after resuming", len(awaits))
	}
	for i := 7; i < len(awaits); i++ {
		if awaits[i] != eventRecheckMaxMs {
			t.Errorf("await %d after the resume is %d ms, want the cap %d", i, awaits[i], eventRecheckMaxMs)
		}
	}
}

func TestDriveWaitForEventProducesAMultiSegmentHistory(t *testing.T) {
	hist, claimed, segs := driveWaitForEvent(t, nil, 24*3600*1000, 7)
	if claimed != `{"found":true,"payload":"PAID"}` || segs != 8 || len(hist) != 15 {
		t.Fatalf("claimed %q after %d segments and %d events", claimed, segs, len(hist))
	}
}
