//go:build cgo

package engine

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// IMPROVEMENT-PLAN 3.31 recorded that the post-trap defers get the backend-wide
// budget rather than any per-workflow one, and labelled it "Read off the code,
// not measured -- noted that way deliberately, since every other finding in this
// section came from running something." These tests are that gap closed.
//
// Measured 2026-09-01 with a backend timeout of 2s and no aggregate budget:
//
//	ctx with a 200ms deadline   150ms   ("199.778625ms wall-clock budget")
//	context.Background()        2.001s  ("2s wall-clock budget")
//	3 runaway defers, Background 6.001s -- 2s each, no ceiling
//
// The third line is worse than the section predicted. Every caller passes
// context.Background() on purpose, so cleanup still runs when the workflow's own
// context has expired; the unforeseen consequence is that each RunDefer reaches
// configureStore with nothing to reconcile against and gets a *fresh* copy of
// the backend's per-invocation budget. The worst case therefore grows without
// limit in the number of defers. On a worker that is 30s each
// (DefaultWasmtimeExecutionTimeout, --wasm-instance-timeout), so twenty runaway
// defers hold a worker slot for ten minutes.

// runawayDefersWat exports three defer callbacks that never return, at the
// (i32,i32,i32,i32)->i64 signature the backend's direct-export path uses. No
// cleat.metadata section, so DetectLanguage classifies it "go" and it routes to
// the wasmtime backend; no _start, so Execute takes the direct-export branch
// rather than the Go dispatcher.
const runawayDefersWat = `
(module
  (memory (export "memory") 1)
  (func (export "cleat_defer_defer-1") (param i32 i32 i32 i32) (result i64)
    (loop $inf br $inf) unreachable)
  (func (export "cleat_defer_defer-2") (param i32 i32 i32 i32) (result i64)
    (loop $inf br $inf) unreachable)
  (func (export "cleat_defer_defer-3") (param i32 i32 i32 i32) (result i64)
    (loop $inf br $inf) unreachable)
)
`

// promptDefersWat is the same shape, returning immediately. It is the control's
// module: a budget that bounds the pass must still let every defer in an
// ordinary pass run.
const promptDefersWat = `
(module
  (memory (export "memory") 1)
  (func (export "cleat_defer_defer-1") (param i32 i32 i32 i32) (result i64) (i64.const 0))
  (func (export "cleat_defer_defer-2") (param i32 i32 i32 i32) (result i64) (i64.const 0))
  (func (export "cleat_defer_defer-3") (param i32 i32 i32 i32) (result i64) (i64.const 0))
)
`

// deferBudgetTestBackendTimeout is the per-invocation bound. Every "wall-clock
// budget" figure the host reports is compared against it, so it must stay the
// value the backend is actually built with below.
const deferBudgetTestBackendTimeout = 2 * time.Second

func threeDeferrals() map[string]string {
	return map[string]string{"defer-1": "one", "defer-2": "two", "defer-3": "three"}
}

// deferBudgetEngine returns an engine shaped like the worker's -- a real
// wasmtime backend, no wazero Runtime -- plus a buffer capturing what runDefers
// logs, which is the only channel it reports through.
func deferBudgetEngine(t *testing.T, wat string, opts ...EngineOption) (*Engine, []byte, *bytes.Buffer) {
	t.Helper()
	ctx := context.Background()

	b, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(deferBudgetTestBackendTimeout))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(ctx) })

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	engOpts := append([]EngineOption{
		WithBackends(WasmtimeLanguages, b),
		WithLogger(logger),
	}, opts...)
	return NewEngine(nil, &mockCaller{}, engOpts...), mustWat2Wasm(t, wat), &logs
}

// reportedBudgets pulls the wall-clock budget the host named out of each
// "execution time limit exceeded (<d> wall-clock budget; ...)" it logged.
//
// The budget is the assertable part of this, not the elapsed time: it is what
// configureStore actually reconciled to, reported by the code under test rather
// than timed from outside.
var budgetRE = regexp.MustCompile(`exceeded \(([^ ]+) wall-clock budget`)

func reportedBudgets(t *testing.T, logs string) []time.Duration {
	t.Helper()
	var out []time.Duration
	for _, m := range budgetRE.FindAllStringSubmatch(logs, -1) {
		d, err := time.ParseDuration(m[1])
		if err != nil {
			t.Fatalf("could not parse the budget %q the host reported: %v\n\n"+
				"If resourceLimitError's wording changed, retarget budgetRE. An "+
				"unmatched regex makes every assertion below vacuous.", m[1], err)
		}
		out = append(out, d)
	}
	return out
}

// TestDeferPassIsBoundedInAggregate is the regression test.
//
// The assertion that carries it is not the elapsed time. With one deadline
// shared across the pass, configureStore reconciles each defer against what is
// *left*: the first gets the full backend timeout, the second whatever remains,
// the third essentially nothing. So at most ONE defer can report the full
// per-invocation budget. Without the aggregate bound all three report it,
// because all three start from a fresh copy.
//
// That is a property of the reported budgets, not of the clock, so it does not
// weaken on a loaded machine the way a wall-clock threshold does. The elapsed
// time is checked too, with a wide margin, purely so a pass that somehow reports
// the right budgets while still running long does not slip through.
func TestDeferPassIsBoundedInAggregate(t *testing.T) {
	const passBudget = 3 * time.Second
	eng, wasmBytes, logs := deferBudgetEngine(t, runawayDefersWat, WithDeferPassBudget(passBudget))

	start := time.Now()
	eng.runDefers(context.Background(), wasmBytes, threeDeferrals())
	elapsed := time.Since(start)

	budgets := reportedBudgets(t, logs.String())
	if len(budgets) != 3 {
		t.Fatalf("the host reported %d execution-limit failures, expected 3 (one per "+
			"runaway defer).\n\nLogs:\n%s\n\n"+
			"Fewer than 3 means defers were skipped rather than bounded, which is a "+
			"different and worse behaviour; more means the parse is picking up "+
			"something else.", len(budgets), logs.String())
	}

	var atFullBudget int
	for _, d := range budgets {
		if d >= deferBudgetTestBackendTimeout {
			atFullBudget++
		}
	}
	if atFullBudget > 1 {
		t.Errorf("%d of 3 defers were given the full %s per-invocation budget; at most 1 can be.\n\n"+
			"budgets reported: %v\n\n"+
			"That means each defer started from a fresh copy of the backend's budget "+
			"instead of sharing one deadline across the pass, so the worst case grows "+
			"without limit in the number of defers a workflow registered. On a worker "+
			"the per-invocation budget is 30s.",
			atFullBudget, deferBudgetTestBackendTimeout, budgets)
	}

	// Wide margin on purpose: 3 runaway defers cost 6s unbounded and ~3s
	// bounded, so anything under 5s distinguishes them without being a timing
	// assertion that has to be tuned.
	if elapsed > 5*time.Second {
		t.Errorf("the pass took %v for a %s budget.\n\nbudgets reported: %v",
			elapsed.Round(time.Millisecond), passBudget, budgets)
	}
}

// TestDeferPassRunsEveryDeferWhenTheyFit is the control, and it is the one that
// matters most here: "bound the pass" is trivially satisfiable by running fewer
// defers, which would silently drop the cleanup this whole path exists to
// perform.
//
// It asserts on the absence of failure logs rather than on a count of
// executions, because runDefers reports through nothing else. A defer that
// never ran, one that failed, and one that succeeded were indistinguishable
// before that logging was added (see the comment at the RunDefer call site).
func TestDeferPassRunsEveryDeferWhenTheyFit(t *testing.T) {
	eng, wasmBytes, logs := deferBudgetEngine(t, promptDefersWat, WithDeferPassBudget(3*time.Second))

	eng.runDefers(context.Background(), wasmBytes, threeDeferrals())

	if got := logs.String(); strings.Contains(got, "defer execution failed") {
		t.Errorf("a prompt defer was reported as failed under a 3s pass budget:\n%s\n\n"+
			"The budget must bound a runaway pass without touching an ordinary one.", got)
	}
}

// TestRunDeferHonoursATighterContextDeadline pins the mechanism the fix relies
// on: configureStore takes the tighter of ctx's remaining time and the backend's
// own timeout, so wrapping the pass in a single WithTimeout is sufficient to
// bound every defer inside it.
//
// If this stops holding, the aggregate bound above stops meaning anything --
// and would keep passing, because a pass that ignores its deadline entirely
// still reports one budget per defer.
func TestRunDeferHonoursATighterContextDeadline(t *testing.T) {
	eng, wasmBytes, logs := deferBudgetEngine(t, runawayDefersWat)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := eng.RunDefer(ctx, wasmBytes, "cleat_defer_defer-1", nil)
	if err == nil {
		t.Fatal("RunDefer returned nil for a defer that never returns")
	}
	budgets := reportedBudgets(t, err.Error()+logs.String())
	if len(budgets) != 1 {
		t.Fatalf("expected one reported budget, got %d from: %v", len(budgets), err)
	}
	if budgets[0] >= deferBudgetTestBackendTimeout {
		t.Errorf("RunDefer applied a %v budget under a 200ms context deadline, want the "+
			"tighter of the two.\n\n"+
			"With the backend's own %s winning, wrapping the pass in a deadline would "+
			"bound nothing.", budgets[0], deferBudgetTestBackendTimeout)
	}
}

// TestRunDeferWithoutADeadlineFallsBackToTheBackendBudget is the other half of
// that pair, and the characterisation of what 3.31 described.
//
// A bare context.Background() is what every runDefers caller passes, and on its
// own it gets the backend-wide budget -- correctly, since there is nothing
// tighter to reconcile against. It is only wrong in aggregate, which is what
// the pass budget fixes. Asserting it here keeps the two halves of the story
// visible together: the per-call behaviour is fine, the multiplication was not.
func TestRunDeferWithoutADeadlineFallsBackToTheBackendBudget(t *testing.T) {
	eng, wasmBytes, logs := deferBudgetEngine(t, runawayDefersWat)

	_, err := eng.RunDefer(context.Background(), wasmBytes, "cleat_defer_defer-1", nil)
	if err == nil {
		t.Fatal("RunDefer returned nil for a defer that never returns")
	}
	budgets := reportedBudgets(t, err.Error()+logs.String())
	if len(budgets) != 1 {
		t.Fatalf("expected one reported budget, got %d from: %v", len(budgets), err)
	}
	if budgets[0] != deferBudgetTestBackendTimeout {
		t.Errorf("RunDefer with no deadline applied a %v budget, want the backend's %s",
			budgets[0], deferBudgetTestBackendTimeout)
	}
}

// TestDeferBudgetWatLiteralsCompile is a sanity check on the two WAT literals
// above, so a wasmtime-go upgrade that changes the text format fails loudly here
// rather than turning the tests into skips.
func TestDeferBudgetWatLiteralsCompile(t *testing.T) {
	for name, wat := range map[string]string{
		"runawayDefersWat": runawayDefersWat,
		"promptDefersWat":  promptDefersWat,
	} {
		if _, err := wasmtime.Wat2Wasm(wat); err != nil {
			t.Errorf("%s no longer compiles: %v", name, err)
		}
	}
}
