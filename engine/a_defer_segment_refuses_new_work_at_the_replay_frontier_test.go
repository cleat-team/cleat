package engine

// cleat#4143. A defer segment exists to run a terminated workflow's defers and nothing else (IMPROVEMENT-PLAN 3.83), so the body's new
// work is refused with callSuspendSentinel. That refusal has to be reached from BOTH halves of every host call:
//
//	the non-replay half   the session has left replay, the body is past the end of its history
//	the replay half       the session is still in replay when the body's first new call arrives
//
// A segment over a run that has history starts in replay (isReplay = len(history) > 0), and a terminate's or cancel's segment is never
// armed to leave it early (WithStopAtLastStoppable is for failed and dead_lettered component segments), so the body's first new call
// reaches the host through the REPLAY half, at the frontier. Before this change the refusal sat only on the other half for these six
// entry points, and the call went through: measured on a real worker, a terminated workflow's segment made the body's next call and
// never ran the registered cleanup (cmd/cleat-worker/a_terminated_workflows_defer_segment_does_not_make_its_next_call_test.go).
//
// FOUR SESSIONS PER ENTRY POINT, because each one rules out a way for the table to pass without testing anything:
//
//	control          an ordinary session at the replay frontier. The call must NOT be refused: it shows the probe sees work happen.
//	known positive   a segment past the frontier (the non-replay half). The call must be refused: it shows the probe sees a refusal.
//	frontier         a segment AT the replay frontier. The question: refused, and nothing recorded, nothing sent.
//	defer body       the same segment while the host drains the guest's own defers (inDeferDrain). It must NOT be refused: a cleanup
//	                 has to be able to make its calls, and the refusal must not take that away.

import (
	"context"
	"testing"
	"time"
)

type frontierEntry struct {
	name string
	call func(ctx context.Context, s *execSession) int64
	// service is true when the entry point reaches the mockCaller, so "nothing sent" can be asserted too.
	service bool
}

func frontierEntries() []frontierEntry {
	return []frontierEntry{
		{"DurableCall", func(ctx context.Context, s *execSession) int64 {
			return s.DurableCall(ctx, nil, "svc", "op", `{}`, 0, 4096)
		}, true},
		{"DurableCallWithRetry", func(ctx context.Context, s *execSession) int64 {
			return s.DurableCallWithRetry(ctx, nil, "svc", "op", `{}`, 3, 10, 200, 100, `[]`, 0, 4096)
		}, true},
		{"DurableCallWithHeartbeat", func(ctx context.Context, s *execSession) int64 {
			return s.DurableCallWithHeartbeat(ctx, nil, "svc", "op", `{}`, int64(time.Hour/time.Millisecond), 0, 4096)
		}, true},
		{"PluginCall", func(ctx context.Context, s *execSession) int64 {
			return s.PluginCall(ctx, nil, "plug", "fn", `{}`, 0, 4096)
		}, false},
		{"PluginCallStreaming", func(ctx context.Context, s *execSession) int64 {
			return s.PluginCallStreaming(ctx, nil, "plug", "fn", `{}`, 0, 4096)
		}, false},
		{"SideEffect", func(ctx context.Context, s *execSession) int64 {
			return s.SideEffect(ctx, nil, `"x"`, 0, 4096)
		}, false},
		{"AcquireLock", func(ctx context.Context, s *execSession) int64 {
			return s.AcquireLock(ctx, nil, "frontier-key", int64(time.Hour/time.Millisecond))
		}, false},
	}
}

type frontierSession struct {
	deferPhase bool
	replaying  bool
	draining   bool
}

func (f frontierSession) build(caller *mockCaller) *execSession {
	opts := []EngineOption{WithWorkflowID("frontier")}
	if f.deferPhase {
		opts = append(opts, WithDeferPhase())
	}
	return &execSession{
		engine:       NewEngine(nil, caller, opts...),
		workflowID:   "frontier",
		nowMs:        time.Now().UnixMilli(),
		deferrals:    map[string]string{},
		queryState:   map[string]string{},
		isReplay:     f.replaying,
		inDeferDrain: f.draining,
	}
}

func TestADeferSegmentRefusesNewWorkAtTheReplayFrontier(t *testing.T) {
	for _, e := range frontierEntries() {
		e := e
		run := func(t *testing.T, f frontierSession) (got int64, s *execSession, caller *mockCaller) {
			t.Helper()
			caller = &mockCaller{}
			s = f.build(caller)
			ctx := ctxWithMem(context.Background(), make([]byte, 1<<16))
			return e.call(ctx, s), s, caller
		}
		refused := func(got int64) bool { return got == callSuspendSentinel }

		t.Run(e.name+"/control: an ordinary session at the replay frontier is not refused", func(t *testing.T) {
			got, _, _ := run(t, frontierSession{replaying: true})
			if refused(got) {
				t.Fatalf("an ordinary session was refused (%#x), so this probe cannot tell work from a refusal", got)
			}
		})

		t.Run(e.name+"/known positive: a segment past the frontier is refused", func(t *testing.T) {
			got, _, _ := run(t, frontierSession{deferPhase: true})
			if !refused(got) {
				t.Fatalf("a segment past the frontier was not refused (returned %#x, want %#x): the probe cannot see the refusal it exists to find", got, callSuspendSentinel)
			}
		})

		t.Run(e.name+"/a segment AT the replay frontier is refused and records and sends nothing", func(t *testing.T) {
			got, s, caller := run(t, frontierSession{deferPhase: true, replaying: true})
			if !refused(got) {
				t.Errorf("%s at the replay frontier of a defer segment was NOT refused (returned %#x, want the stop sentinel %#x).\n\n"+
					"The segment's body made new work: the refusal sits on the non-replay half only, and a segment over a run with history "+
					"arrives here still in replay.", e.name, got, callSuspendSentinel)
			}
			if len(s.history) != 0 {
				t.Errorf("a refused call recorded %d event(s): %v", len(s.history), s.history)
			}
			if e.service && len(caller.calls) != 0 {
				t.Errorf("the service was reached %d time(s) by a call the segment should have refused: %v", len(caller.calls), caller.calls)
			}
		})

		t.Run(e.name+"/a defer body at the frontier is not refused", func(t *testing.T) {
			got, _, _ := run(t, frontierSession{deferPhase: true, replaying: true, draining: true})
			if refused(got) {
				t.Fatalf("a defer body's own call was refused at the frontier (%#x): the host drains the guest's defers with inDeferDrain set, "+
					"and the refusal must not take their calls away", got)
			}
		})
	}
}
