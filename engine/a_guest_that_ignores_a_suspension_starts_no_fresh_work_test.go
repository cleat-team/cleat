package engine

// cleat#4285, host side. A guest that does NOT stop when told to (an SDK returns the suspension as an ordinary value, or the guest swallows the exception or
// discards the error) goes on to make more calls, and each one is recorded AFTER the pending await and runs AGAIN when the workflow resumes. The worst case is
// an outbound service call. From the moment an execution has suspended, a call that would start fresh work is refused: it runs nothing, records nothing,
// keeps the FIRST suspension, and answers the word ITS OWN SDK decoder recognises.
//
// Every row is run three ways so that it can fail: SUSPENDED (refused, with the word), NOT suspended (the control: the same call does its work, so "nothing
// happened" cannot be the call being inert), and suspended but in the GUEST'S OWN DEFER DRAIN (the cleanup of a stopping run must still go through).
// "Did its work" is one measure for all three: something was dispatched, something was recorded, the step moved, or the suspension changed.
// These drive the real session methods; no real guest, in any SDK, was run (the SDK survey is on the issue).

import (
	"context"
	"testing"
)

type ignoredSuspendRow struct {
	name string
	// call invokes the host call on the session.
	call func(s *execSession) int64
	// word is the answer once suspended; nil means callSuspendSentinel. errCodeOnly compares only the low 32 bits (a result word carrying a message length).
	word        func() int64
	errCodeOnly bool
	// noControl names why the call cannot be shown to do its work in this harness (no store or plugin behind it). The refusal is still asserted.
	noControl string
}

func ignoredSuspendRows() []ignoredSuspendRow {
	ctx := context.Background()
	return []ignoredSuspendRow{
		{name: "DurableCall (an outbound service call)", call: func(s *execSession) int64 { return s.DurableCall(ctx, nil, "svc", "op", `{}`, 0, 1024) }},
		{name: "DurableCallWithRetry", call: func(s *execSession) int64 {
			return s.DurableCallWithRetry(ctx, nil, "svc", "op", `{}`, 3, 10, 200, 100, `[]`, 0, 1024)
		}},
		{name: "DurableCallWithHeartbeat", call: func(s *execSession) int64 {
			return s.DurableCallWithHeartbeat(ctx, nil, "svc", "op", `{}`, 1000, 0, 1024)
		}},
		{name: "DurableSend", call: func(s *execSession) int64 { return s.DurableSend(ctx, nil, "svc", "op", `{}`) }},
		{name: "DurableScheduleInvoke", call: func(s *execSession) int64 { return s.DurableScheduleInvoke(ctx, nil, "svc", "op", `{}`, 1000) }},
		{name: "PluginCall", noControl: "no plugin registered in this harness", call: func(s *execSession) int64 { return s.PluginCall(ctx, nil, "p", "f", `{}`, 0, 1024) }},
		{name: "PluginCallStreaming", noControl: "no plugin registered in this harness", call: func(s *execSession) int64 { return s.PluginCallStreaming(ctx, nil, "p", "f", `{}`, 0, 1024) }},
		{name: "ChildWorkflow", noControl: "no child starter in this harness", call: func(s *execSession) int64 { return s.ChildWorkflow(ctx, nil, "child", `{}`, 0, 64) }},
		{name: "RunDetached", noControl: "no child starter in this harness", call: func(s *execSession) int64 { return s.RunDetached(ctx, nil, "child", `{}`) }},
		{name: "ContinueAsNew", call: func(s *execSession) int64 { return s.ContinueAsNew(ctx, nil, `{}`) }},
		{name: "ContinueAsNewWithVersion", call: func(s *execSession) int64 { return s.ContinueAsNewWithVersion(ctx, nil, `{}`, 2) }},
		{name: "DurablePollUpdate", call: func(s *execSession) int64 { return s.DurablePollUpdate(ctx, nil, 0, 64) }},
		{name: "DurableCompleteUpdate", call: func(s *execSession) int64 { return s.DurableCompleteUpdate(ctx, nil, "u:1:p", "r", "") }},
		{name: "RegisterUpdateHandler (not a stop site: an ordinary error code)", word: func() int64 { return 1 },
			call: func(s *execSession) int64 { return s.RegisterUpdateHandler(ctx, nil, "h") }},
		{name: "DurableSleep (its own suspend status, and the first suspension is kept)", word: func() int64 { return packSleepResult(sleepStatusSuspend, 60000) },
			call: func(s *execSession) int64 { return s.DurableSleep(ctx, nil, 60000) }},
		{name: "AwaitAnyChild (its own suspension word, 1<<62)", word: packAwaitChildResultSuspend, noControl: "no child store in this harness",
			call: func(s *execSession) int64 { return s.AwaitAnyChild(ctx, nil, `["c1","c2"]`, 0, 64) }},
		// ---- the calls on the dispatcher: the spec says what the refusal answers ----
		{name: "side_effect (a stop site: the stop word)", call: func(s *execSession) int64 { return s.SideEffect(ctx, nil, "x", 0, 64) }},
		{name: "acquire_lock", noControl: "no concurrency-key store in this harness", call: func(s *execSession) int64 { return s.AcquireLock(ctx, nil, "k", 1000) }},
		{name: "fetch", noControl: "no fetcher in this harness", call: func(s *execSession) int64 { return s.Fetch(ctx, nil, "GET", "http://x", `{}`, "", 0, 64) }},
		{name: "await_child (its own suspension word, 1<<62)", word: packAwaitChildResultSuspend, noControl: "no child store in this harness",
			call: func(s *execSession) int64 { return s.AwaitChild(ctx, nil, "c1", 0, 64) }},
		{name: "await_all_children (its own suspension word, 1<<62)", word: packAwaitChildResultSuspend, noControl: "no child store in this harness",
			call: func(s *execSession) int64 { return s.AwaitAllChildren(ctx, nil, `["c1"]`, 0, 64) }},
		{name: "list_crons (not a stop site: an ordinary error result)", word: func() int64 { return 1 }, errCodeOnly: true,
			call: func(s *execSession) int64 { return s.ListCrons(ctx, nil, 0, 64) }},
		{name: "release_lock (not a stop site: the ordinary error code)", word: func() int64 { return 1 },
			call: func(s *execSession) int64 { return s.ReleaseLock(ctx, nil, "k") }},
	}
}

// ignoredSuspendUpdates is the smallest update store that gives the two update calls something to do: one pending request, and completion accepted.
type ignoredSuspendUpdates struct{}

func (ignoredSuspendUpdates) GetPendingUpdateRequests(context.Context, string) ([]UpdateRequestInfo, error) {
	return []UpdateRequestInfo{{WorkflowID: "wf-ignored-suspend", RequestID: "u:1:p", UpdateName: "h", Payload: `{}`}}, nil
}
func (ignoredSuspendUpdates) CompleteUpdateRequest(context.Context, string, string, string, string) error {
	return nil
}
func (ignoredSuspendUpdates) ResolvePromise(context.Context, string, string) error { return nil }
func (ignoredSuspendUpdates) RejectPromise(context.Context, string, string) error  { return nil }

type ignoredSuspendWorld struct {
	s      *execSession
	caller *mockCaller
}

func newIgnoredSuspendWorld() *ignoredSuspendWorld {
	caller := &mockCaller{}
	s := &execSession{
		engine:     NewEngine(nil, caller, WithWorkflowID("wf-ignored-suspend"), WithUpdateStore(ignoredSuspendUpdates{}), WithClock(func() int64 { return 1_800_000_000_000 })),
		workflowID: "wf-ignored-suspend", nowMs: 1_800_000_000_000,
		deferrals: map[string]string{}, queryState: map[string]string{},
	}
	return &ignoredSuspendWorld{s: s, caller: caller}
}

// ran reports whether the call did its work, against the state captured before it.
func (w *ignoredSuspendWorld) ran(histLen, step, dispatched int, susp *SuspendError) bool {
	return len(w.s.history) > histLen || w.s.stepCount != step || len(w.caller.calls) > dispatched || w.s.suspendErr != susp
}

func TestAGuestThatIgnoresASuspensionStartsNoFreshWork(t *testing.T) {
	for _, r := range ignoredSuspendRows() {
		t.Run(r.name, func(t *testing.T) {
			want := callSuspendSentinel
			if r.word != nil {
				want = r.word()
			}
			// SUSPENDED: refused.
			w := newIgnoredSuspendWorld()
			first := &SuspendError{Reason: "earlier"}
			w.s.suspendErr = first
			got := r.call(w.s)
			if r.errCodeOnly {
				got, want = got&0xffffffff, want&0xffffffff
			}
			if got != want {
				t.Errorf("answered %#x after a suspension, want %#x", got, want)
			}
			if w.ran(0, 0, 0, first) {
				t.Errorf("did work after a suspension: history=%d step=%d dispatched=%d suspension=%+v", len(w.s.history), w.s.stepCount, len(w.caller.calls), w.s.suspendErr)
			}
			// NOT suspended: the control. The same call does its work.
			if r.noControl == "" {
				c := newIgnoredSuspendWorld()
				c.s.suspendErr = nil
				r.call(c.s)
				if !c.ran(0, 0, 0, nil) {
					t.Errorf("control: the call did nothing while NOT suspended, so its refusal above measured nothing")
				}
			}
			// SUSPENDED, in a defer drain: the cleanup still goes through, whether the GUEST runs it and says so (SetDeferPhase) or the HOST runs it
			// (inDeferDrain, which brackets the host's own call to the guest's defer runner). Two flags, so two arms: removing either exemption must fail one.
			if r.noControl == "" {
				for _, drain := range []struct {
					name string
					set  func(*execSession)
				}{
					{"the guest's own drain (SetDeferPhase)", func(s *execSession) { s.inDeferPhase = true }},
					{"the host's drain (inDeferDrain)", func(s *execSession) { s.inDeferDrain = true }},
				} {
					d := newIgnoredSuspendWorld()
					d.s.suspendErr = first
					drain.set(d.s)
					r.call(d.s)
					if !d.ran(0, 0, 0, first) {
						t.Errorf("a call inside %s was refused: cleanup must still go through", drain.name)
					}
				}
			}
		})
	}
}
