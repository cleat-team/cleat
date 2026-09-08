package engine

import (
	"context"
	"testing"
)

// TestASignalConsumedDuringReplayIsRecordedBeforeItIsConsumed is cleat#933's
// at-most-once half.
//
// The award path has two arms that poll the signal store, record a
// signal_received, and consume the delivery. One is the fresh path; the other
// is inside the replay branch, and it is the ONLY path a signal delivered to a
// SUSPENDED workflow can take -- the ordinary case, not an edge one.
//
// That arm called recordEvent with s.isReplay still true, and recordEvent
// flushes only when it is false (engine/lifecycle.go, "Persist immediately so
// events survive worker crashes"). consumeDelivered then deleted the row. Its
// doc is explicit that this order is the whole point:
//
//	recordEvent persists synchronously ... so by the time this is called the
//	fact that the workflow received this payload is on disk. ... Consuming
//	first and crashing before the event was durable would instead lose the
//	signal outright, with no record anywhere that it ever arrived.
//
// On this arm the premise was false, so the guarantee it supports was not in
// force: between the consume and the end of the segment, a worker death lost
// the signal with no record it had arrived.
//
// WHAT THIS TEST DOES NOT SAY -- and an earlier version of it did -- is that
// the event was never written. It was, at segment end:
//
//	newEvents = resultHistory[len(history):]      cmd/cleat-worker/setup.go
//	execStore.FinalizeWorkflowSegment(..., newEvents, ...)
//
// so reading event_history mid-segment and concluding "lost" reads a moment
// rather than an outcome. Caught by rcownie-ef against the original claim; the
// correction is why TestAReplayConsumedSignalDoesNotBreakTheChecksumChain
// exists, since the chain pointer is the half that segment end does NOT
// repair. This test keeps the narrower claim that is true: the event must be
// durable BEFORE the delivery is consumed, because that ordering is the only
// thing standing between a crash and a lost signal.
func TestASignalConsumedDuringReplayIsRecordedBeforeItIsConsumed(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := newIntentWorkflow(t, ctx, store, "signal-replay-durability")
			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil || wf == nil {
				t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
			}

			sigStore, ok := store.(SignalStore)
			if !ok {
				t.Fatalf("%T does not implement SignalStore", store)
			}
			if err := sigStore.DeliverSignal(ctx, wfID, "a", `{"p":1}`); err != nil {
				t.Fatalf("DeliverSignal: %v", err)
			}

			eng := NewEngine(nil, nil,
				WithDB(rawDBOf(t, store)),
				WithWorkflowStore(store),
				WithSignalStore(sigStore),
				WithWorkflowID(wfID),
				WithWorkerID("worker-1"),
				WithGeneration(wf.Generation),
				WithTenantID(DefaultTenantUUID))

			// The state a worker resumes into: one await_signals in history,
			// the run suspended on it, the delivery waiting in the store.
			//
			// The await row is written to the DATABASE as well as handed to
			// the session, because that is what a resumed worker loads. It
			// also makes the failure legible: the gap this test is about is
			// the missing SECOND row, not an empty table.
			awaitRec := EventRecord{
				Step: 0, EventType: EventTypeAwaitSignals,
				SignalNames: `["a","b"]`, TimeoutMs: 60000,
			}
			if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{awaitRec}); err != nil {
				t.Fatalf("seeding the await event: %v", err)
			}

			s := &execSession{
				engine:     eng,
				workflowID: wfID,
				nowMs:      1000000,
				deferrals:  make(map[string]string),
				queryState: make(map[string]string),
				isReplay:   true,
				history:    []EventRecord{awaitRec},
			}

			buf := make([]byte, 512)
			c := contextWithRawMemBuf(ctx, buf)
			packed := s.DurableAwaitSignals(c, nil, `["a","b"]`, 60000, 0, 200, 256, 200)

			nameLen := uint32((packed >> 48) & 0xFFFFFFFF)
			if got := string(buf[:nameLen]); got != "a" {
				t.Fatalf("the await returned %q, want \"a\" -- the delivery was not picked up "+
					"at all, so the durability question below is not the one being measured", got)
			}

			// The guest has been told it received "a". Two things must now be
			// true together, and the defect is that only the second is.
			hist, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			var recorded *EventRecord
			for i := range hist {
				if hist[i].EventType == EventTypeSignalReceived {
					recorded = &hist[i]
				}
			}

			remaining, _, err := sigStore.PollSignal(ctx, wfID, "a")
			consumed := err == nil && remaining.ID == 0

			if recorded == nil {
				t.Errorf("the workflow was handed signal %q and event_history has no "+
					"signal_received row for it (%d rows: %v).\n\n"+
					"The delivery row was %s. Together those mean the signal is GONE: "+
					"the next replay reads history positionally, finds no signal_received "+
					"after the await, polls the store, and finds nothing there either.\n\n"+
					"recordEvent persists only when the session is not replaying, and the "+
					"replay arm of DurableAwaitSignals called it with isReplay still true.",
					"a", len(hist), eventTypesOf(hist),
					map[bool]string{true: "consumed anyway", false: "left in place"}[consumed])
			}
			if recorded != nil && recorded.SignalPayload != `{"p":1}` {
				t.Errorf("signal_received recorded payload %q, want %q",
					recorded.SignalPayload, `{"p":1}`)
			}
		})
	}
}

func eventTypesOf(hist []EventRecord) []EventType {
	out := make([]EventType, 0, len(hist))
	for _, h := range hist {
		out = append(out, h.EventType)
	}
	return out
}

// TestAReplayConsumedSignalDoesNotBreakTheChecksumChain is cleat#933's
// symptom B, and it is the assertion that survives the correction below.
//
// AN EARLIER READING OF THIS DEFECT SAID THE EVENT WAS "NEVER WRITTEN". That
// is wrong, and the way it is wrong is worth keeping: recordEvent skips its
// immediate flush while replaying, but the worker persists everything appended
// beyond the loaded history at segment end --
//
//	newEvents = resultHistory[len(history):]        cmd/cleat-worker/setup.go:1878
//	execStore.FinalizeWorkflowSegment(..., newEvents, ...)
//
// -- so the row does land, one segment late. An engine-level harness that
// reads event_history mid-segment sees it missing and concludes it is lost;
// that reads a moment, not an outcome. Caught by rcownie-ef against the
// original claim.
//
// WHAT BREAKS THE RUN IS THE OTHER HALF OF THE SAME SKIP, and it is not
// repaired at segment end. recordEvent advances s.lastChecksum only on the
// branch it also flushes on, so an event recorded during replay leaves the
// chain pointer where it was. The NEXT event is flushed immediately, chained
// from the wrong predecessor -- it points past the signal_received to the
// await before it. Verification recomputes the chain over the events actually
// stored, including the one finalize wrote, and the two disagree:
//
//	verify events: workflow ...: step 2: checksum mismatch
//
// which is #933's symptom B verbatim, at the step it reports, over the
// [await_signals, signal_received, await_signals] history it reports.
//
// The test therefore asserts on VerifyWorkflowEvents rather than on row
// presence. Row presence is the property that is only temporarily violated;
// the chain is the one that stays broken and fails the workflow.
func TestAReplayConsumedSignalDoesNotBreakTheChecksumChain(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := newIntentWorkflow(t, ctx, store, "signal-replay-checksum")
			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil || wf == nil {
				t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
			}
			sigStore := store.(SignalStore)

			eng := NewEngine(nil, nil,
				WithDB(rawDBOf(t, store)),
				WithWorkflowStore(store),
				WithSignalStore(sigStore),
				WithWorkflowID(wfID),
				WithWorkerID("worker-1"),
				WithGeneration(wf.Generation),
				WithTenantID(DefaultTenantUUID))
			newSession := func(hist []EventRecord) *execSession {
				return &execSession{
					engine: eng, workflowID: wfID, nowMs: 1000000,
					deferrals: make(map[string]string), queryState: make(map[string]string),
					isReplay: len(hist) > 0, history: hist,
				}
			}
			await := func(s *execSession) string {
				buf := make([]byte, 512)
				packed := s.DurableAwaitSignals(contextWithRawMemBuf(ctx, buf), nil,
					`["a","b"]`, 60000, 0, 200, 256, 200)
				return string(buf[:uint32((packed>>48)&0xFFFFFFFF)])
			}
			// finalizeSegment is what the worker does at the end of a run:
			// persist everything the session appended past the history it
			// loaded. Standing in for cmd/cleat-worker's call so this stays an
			// engine test.
			finalizeSegment := func(s *execSession, loaded int) {
				t.Helper()
				if len(s.history) <= loaded {
					return
				}
				// The whole tail, exactly as the worker passes it -- including
				// events recordEvent already flushed inline. The insert is
				// ON CONFLICT (workflow_id, step) DO UPDATE, so re-offering
				// one is tolerated and does not overwrite its checksum.
				if err := store.AppendEventHistoryBatch(ctx, wfID, s.history[loaded:]); err != nil {
					t.Fatalf("segment-end persist: %v", err)
				}
			}

			// Segment 1: fresh, nothing delivered. Records await_signals at
			// step 0 and suspends.
			s1 := newSession(nil)
			if name := await(s1); name != "" {
				t.Fatalf("segment 1's await returned %q with nothing delivered", name)
			}
			finalizeSegment(s1, 0)

			if err := sigStore.DeliverSignal(ctx, wfID, "a", `{"p":1}`); err != nil {
				t.Fatalf("DeliverSignal: %v", err)
			}

			// Segment 2: resume. The first await finds the delivery in the
			// store; the second has nothing left and records a second
			// await_signals -- the event whose chain pointer is the subject.
			loaded, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			s2 := newSession(loaded)
			if name := await(s2); name != "a" {
				t.Fatalf("segment 2's first await returned %q, want \"a\"", name)
			}
			_ = await(s2)
			finalizeSegment(s2, len(loaded))

			final, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatalf("LoadEventHistory (final): %v", err)
			}
			if got := eventTypesOf(final); len(got) != 3 {
				t.Fatalf("history is %v, want the three events #933 reports "+
					"(await_signals, signal_received, await_signals)", got)
			}

			if err := store.VerifyWorkflowEvents(ctx, wfID); err != nil {
				t.Errorf("the stored event chain does not verify: %v\n\n"+
					"History is %v. The signal_received between the two awaits was "+
					"recorded while the session was still replaying, so recordEvent "+
					"neither flushed it nor advanced s.lastChecksum -- and the second "+
					"await_signals, which IS flushed immediately, was chained from the "+
					"await before the signal instead of from the signal. The row itself "+
					"arrives at segment end; the chain pointer never does.",
					err, eventTypesOf(final))
			}
		})
	}
}
