package engine

// cleat#4152, group G1a of docs/design/replay-step-dispatcher.md: a characterization of SideEffect, SideEffectRecorded and DurableDefer,
// RECORDED FROM THE CODE AS IT WAS BEFORE THEY WENT THROUGH step() (develop db2a7044, no production change), and kept so that moving them is
// checkable as "changes where the code lives and nothing else" -- and so that every place it does NOT stay the same is a row that changes and is
// said so in the PR.
//
// Each call is driven through a set of situations and the result is one string per (call, situation): the packed result, the bytes the call wrote
// to the guest's buffer (a raw buffer in the context, so they ARE observable here, which the promise characterization could not do), whether the
// session is still replaying, the step count, the events the call RECORDED (not the ones it consumed), and for a defer the table of registered
// defers. A row that changes after the migration is a behaviour change.
//
// What a row cannot say: the timestamp of a recorded event (masked), and anything the wasm backends add around the call.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

const g1BufLen = 256

// g1Call performs one host call on s with a context that carries a raw guest buffer, and reports what it wrote.
type g1Call struct {
	name string
	// call performs the call once; buf is the guest's memory.
	call func(ctx context.Context, s *execSession) int64
}

var g1Calls = []g1Call{
	{"side_effect", func(ctx context.Context, s *execSession) int64 {
		return s.SideEffect(ctx, nil, "computed-now", 0, g1BufLen)
	}},
	{"side_effect_recorded", func(ctx context.Context, s *execSession) int64 { return s.SideEffectRecorded(ctx, nil, 0, g1BufLen) }},
	{"defer", func(ctx context.Context, s *execSession) int64 {
		return s.DurableDefer(ctx, nil, "the-cleanup", 0, g1BufLen)
	}},
}

// g1Session builds a session over an engine with no database, optionally replaying history.
func g1Session(history []EventRecord, replaying bool, opts ...EngineOption) *execSession {
	return &execSession{
		engine:     NewEngine(nil, nil, opts...),
		workflowID: "wf-g1", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: replaying, history: history,
	}
}

func g1Describe(history []EventRecord, from int) string {
	var parts []string
	for _, r := range history[from:] {
		parts = append(parts, fmt.Sprintf("%s/step=%d/value=%q/defer=%q/desc=%q", r.EventType, r.Step, r.SideEffectResult, r.DeferID, r.DeferDescription))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func g1Deferrals(s *execSession) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.deferrals))
	for k := range s.deferrals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, s.deferrals[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// g1Observe runs the call and renders everything observable about it, without anything that varies between runs.
func g1Observe(gc g1Call, s *execSession) string {
	buf := make([]byte, g1BufLen)
	ctx := context.WithValue(context.Background(), wasmMemBufKey{}, buf)
	before := len(s.history)
	res := gc.call(ctx, s)
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return fmt.Sprintf("res=%#x wrote=%q isReplay=%v step=%d recorded=%s defers=%s", res, string(buf[:n]), s.isReplay, s.stepCount, g1Describe(s.history, before), g1Deferrals(s))
}

// g1Own is the record a fresh, healthy run of this call leaves, the history every replay situation starts from. The side-effect and the defer
// are built by hand so that the VALUE in the record differs from what the replaying guest computes, which is what the call exists for.
func g1Own(gc g1Call) []EventRecord {
	switch gc.name {
	case "defer":
		return []EventRecord{{Step: 0, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "recorded-cleanup"}}
	default: // side_effect and its lookup both read a side_effect event
		return []EventRecord{{Step: 0, EventType: EventTypeSideEffect, SideEffectResult: "recorded-value"}}
	}
}

func g1Characterize(t *testing.T, gc g1Call) map[string]string {
	t.Helper()
	got := map[string]string{}
	own := func() []EventRecord { return append([]EventRecord(nil), g1Own(gc)...) }
	other := []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "{}"}}

	// fresh: a first run with nothing in history.
	got["fresh"] = g1Observe(gc, g1Session(nil, false))

	// replay: the call meets its own record.
	got["replay"] = g1Observe(gc, g1Session(own(), true))

	// frontier: the call meets its own record, then the same call is made again past the end of history.
	s2 := g1Session(own(), true)
	g1Observe(gc, s2)
	got["frontier"] = g1Observe(gc, s2)

	// divergence: the record at this step is another call's. The step count after it says whether the record was consumed.
	got["divergence"] = g1Observe(gc, g1Session(append([]EventRecord(nil), other...), true))

	// segment, fresh: a defer segment's body past the end of its (empty) history.
	got["segment_fresh"] = g1Observe(gc, g1Session(nil, true, WithDeferPhase()))

	// segment, frontier: the segment replays this call's record and the body then makes it again.
	s5 := g1Session(own(), true, WithDeferPhase())
	g1Observe(gc, s5)
	got["segment_frontier"] = g1Observe(gc, s5)

	// armed segment, fresh: a stop-at-last-stoppable segment over a component guest (cleat#4079), body past its history.
	s6 := g1Session(nil, true, WithDeferPhase())
	s6.stopArmed = true
	got["armed_segment_fresh"] = g1Observe(gc, s6)

	// armed segment, frontier.
	s7 := g1Session(own(), true, WithDeferPhase())
	s7.stopArmed = true
	g1Observe(gc, s7)
	got["armed_segment_frontier"] = g1Observe(gc, s7)

	// a refused event write (cleat#4160): the session is stopped, fresh.
	s8 := g1Session(nil, false)
	s8.eventWriteFailure = fmt.Errorf("event write refused")
	got["write_failed_fresh"] = g1Observe(gc, s8)

	// a refused event write, at the replay frontier.
	s9 := g1Session(own(), true)
	s9.eventWriteFailure = fmt.Errorf("event write refused")
	g1Observe(gc, s9)
	got["write_failed_frontier"] = g1Observe(gc, s9)

	// shutdown observed, fresh.
	down := make(chan struct{})
	close(down)
	s10 := g1Session(nil, false, WithShutdownSignal(down))
	got["shutdown_fresh"] = g1Observe(gc, s10)

	// a debugger's ReplayQuit (cleat debug) at the call's own record: the callback aborts the replay as the record is consumed. The lookup answers
	// the stop sentinel there (component_callbacks.go documents it as the contract), the other two answer 0.
	quit := g1Session(own(), true)
	quit.stepCallback = func(int, *EventRecord, map[string]string) ReplayStepAction { return ReplayQuit }
	got["replay_quit"] = g1Observe(gc, quit)

	// A defer record written before DeferID and the description were recorded: replay rebuilds the id ("defer-<step>") and takes the
	// description from the argument, so a history written long ago still hands the guest the id it was first given.
	if gc.name == "defer" {
		legacy := []EventRecord{{Step: 0, EventType: EventTypeDefer}}
		got["replay_legacy_record"] = g1Observe(gc, g1Session(legacy, true))
	}

	return got
}

// g1Golden is the table recorded against the code as it was (develop db2a7044), one string per (call, situation). It is the reference the
// migration is checked against: a row that changes is a behaviour change, and the PR that changes it says so. The ones that are expected to
// change when the calls move onto step() are named in that PR's body, not here.
var g1Golden = map[string]map[string]string{
	"side_effect": {
		"armed_segment_fresh":    `res=0x80000000 wrote="" isReplay=false step=0 recorded=[] defers={}`,
		"armed_segment_frontier": `res=0x80000000 wrote="" isReplay=false step=1 recorded=[] defers={}`,
		"divergence":             `res=0x4100000001 wrote="replay divergence at step 0: expected side_effect event, got call" isReplay=true step=1 recorded=[] defers={}`,
		"fresh":                  `res=0xc00000000 wrote="computed-now" isReplay=false step=1 recorded=[side_effect/step=0/value="computed-now"/defer=""/desc=""] defers={}`,
		"frontier":               `res=0xc00000000 wrote="computed-now" isReplay=false step=2 recorded=[side_effect/step=1/value="computed-now"/defer=""/desc=""] defers={}`,
		"replay":                 `res=0xe00000000 wrote="recorded-value" isReplay=true step=1 recorded=[] defers={}`,
		"replay_quit":            `res=0x0 wrote="" isReplay=true step=1 recorded=[] defers={}`,
		"segment_fresh":          `res=0x80000000 wrote="" isReplay=false step=0 recorded=[] defers={}`,
		"segment_frontier":       `res=0x80000000 wrote="" isReplay=false step=1 recorded=[] defers={}`,
		"shutdown_fresh":         `res=0x80000000 wrote="" isReplay=false step=0 recorded=[] defers={}`,
		"write_failed_fresh":     `res=0x80000000 wrote="" isReplay=false step=0 recorded=[] defers={}`,
		"write_failed_frontier":  `res=0x80000000 wrote="" isReplay=false step=1 recorded=[] defers={}`,
	},
	"side_effect_recorded": {
		"armed_segment_fresh":    `res=0x80000000 wrote="" isReplay=true step=0 recorded=[] defers={}`,
		"armed_segment_frontier": `res=0x80000000 wrote="" isReplay=true step=1 recorded=[] defers={}`,
		"divergence":             `res=0x4100000001 wrote="replay divergence at step 0: expected side_effect event, got call" isReplay=true step=0 recorded=[] defers={}`,
		"fresh":                  `res=0x2 wrote="" isReplay=false step=0 recorded=[] defers={}`,
		"frontier":               `res=0x2 wrote="" isReplay=true step=1 recorded=[] defers={}`,
		"replay":                 `res=0xe00000000 wrote="recorded-value" isReplay=true step=1 recorded=[] defers={}`,
		"replay_quit":            `res=0x80000000 wrote="" isReplay=true step=1 recorded=[] defers={}`,
		"segment_fresh":          `res=0x80000000 wrote="" isReplay=true step=0 recorded=[] defers={}`,
		"segment_frontier":       `res=0x80000000 wrote="" isReplay=true step=1 recorded=[] defers={}`,
		"shutdown_fresh":         `res=0x80000000 wrote="" isReplay=false step=0 recorded=[] defers={}`,
		"write_failed_fresh":     `res=0x80000000 wrote="" isReplay=false step=0 recorded=[] defers={}`,
		"write_failed_frontier":  `res=0x80000000 wrote="" isReplay=true step=1 recorded=[] defers={}`,
	},
	"defer": {
		"armed_segment_fresh":    `res=0x80000000 wrote="" isReplay=false step=0 recorded=[] defers={}`,
		"armed_segment_frontier": `res=0x80000000 wrote="" isReplay=false step=1 recorded=[] defers={defer-0="recorded-cleanup"}`,
		"divergence":             "res=0xbf00000001 wrote=\"replay divergence at step 0: expected defer, got call.\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=0 recorded=[] defers={}",
		"fresh":                  `res=0x700000000 wrote="defer-0" isReplay=false step=1 recorded=[defer/step=0/value=""/defer="defer-0"/desc="the-cleanup"] defers={defer-0="the-cleanup"}`,
		"frontier":               `res=0x700000000 wrote="defer-1" isReplay=false step=2 recorded=[defer/step=1/value=""/defer="defer-1"/desc="the-cleanup"] defers={defer-0="recorded-cleanup",defer-1="the-cleanup"}`,
		"replay":                 `res=0x700000000 wrote="defer-0" isReplay=true step=1 recorded=[] defers={defer-0="recorded-cleanup"}`,
		"replay_legacy_record":   `res=0x700000000 wrote="defer-0" isReplay=true step=1 recorded=[] defers={defer-0="the-cleanup"}`,
		"replay_quit":            `res=0x0 wrote="" isReplay=true step=1 recorded=[] defers={}`,
		"segment_fresh":          `res=0x700000000 wrote="defer-0" isReplay=false step=1 recorded=[defer/step=0/value=""/defer="defer-0"/desc="the-cleanup"] defers={defer-0="the-cleanup"}`,
		"segment_frontier":       `res=0x700000000 wrote="defer-1" isReplay=false step=2 recorded=[defer/step=1/value=""/defer="defer-1"/desc="the-cleanup"] defers={defer-0="recorded-cleanup",defer-1="the-cleanup"}`,
		"shutdown_fresh":         `res=0x700000000 wrote="defer-0" isReplay=false step=1 recorded=[defer/step=0/value=""/defer="defer-0"/desc="the-cleanup"] defers={defer-0="the-cleanup"}`,
		"write_failed_fresh":     `res=0x700000000 wrote="defer-0" isReplay=false step=1 recorded=[defer/step=0/value=""/defer="defer-0"/desc="the-cleanup"] defers={defer-0="the-cleanup"}`,
		"write_failed_frontier":  `res=0x700000000 wrote="defer-1" isReplay=false step=2 recorded=[defer/step=1/value=""/defer="defer-1"/desc="the-cleanup"] defers={defer-0="recorded-cleanup",defer-1="the-cleanup"}`,
	},
}

func TestG1CallsBehaveAsRecordedBeforeTheDispatcher(t *testing.T) {
	for _, gc := range g1Calls {
		gc := gc
		t.Run(gc.name, func(t *testing.T) {
			want, ok := g1Golden[gc.name]
			if !ok {
				t.Fatalf("no golden rows for %s", gc.name)
			}
			got := g1Characterize(t, gc)
			for sit, w := range want {
				if g, ok := got[sit]; !ok {
					t.Errorf("%s/%s: situation no longer exercised", gc.name, sit)
				} else if g != w {
					t.Errorf("%s/%s changed:\n  was: %s\n  now: %s", gc.name, sit, w, g)
				}
			}
			for sit := range got {
				if _, ok := want[sit]; !ok {
					t.Errorf("%s/%s: a situation with no golden row", gc.name, sit)
				}
			}
		})
	}
}
