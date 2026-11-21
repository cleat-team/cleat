package engine

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cleat#4069. A parent's await on a child reads GetChildResult, which resolves a continue-as-new chain to its last run (cleat#955) and then reads that run's row in a SEPARATE statement. The
// chain can move on in between, and the row read is then a run the chain has LEFT: a continued run is finalised `done` with an empty result, so the parent is handed a SETTLED outcome carrying
// `{}` for a child that has not finished, and records it durably in its own history. The same hazard #4061 was for GetTerminalRun, with a worse consequence (cleat#4068 fixed that one).
//
// This is the reproduction, run on the real stores. One goroutine advances the child's chain with ClaimWorkflow + ContinueAsNew and finishes the last run with {"final":true}; four goroutines
// poll GetChildResult on the FIRST run's id. The truth is fixed, so the only correct settled answer is {"final":true}, and ANY other Completed answer is the hazard.
//
// It has a NEGATIVE CONTROL and the control is checked: a chain that never moves must read zero anomalies, and the number of hops actually executed is compared with the number asked for, because the
// first control on cleat#4069 ran 40 hops where it meant zero (a test that read 0 as "unset") and reported hundreds of anomalies as if they were the control.

type childRaceConfig struct {
	name   string
	hops   int
	rounds int
}

type childRaceResult struct {
	polls, anomalies, hopsRun int64
	firstAnomaly              string
}

func runChildResultRace(t *testing.T, store WorkflowStore, parent string, cfg childRaceConfig) childRaceResult {
	t.Helper()
	ctx := context.Background()
	const want = `{"final":true}`
	starter, ok := store.(interface {
		StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error)
	})
	if !ok {
		t.Fatalf("%T cannot start a child workflow", store)
	}
	childStore, ok := store.(ChildWorkflowStore)
	if !ok {
		t.Fatalf("%T is not a ChildWorkflowStore", store)
	}
	var res childRaceResult
	var mu sync.Mutex
	for round := 0; round < cfg.rounds; round++ {
		child, err := starter.StartChildWorkflow(ctx, parent, "intent-workflow", `{}`, 1, "TERMINATE", 0)
		if err != nil {
			t.Fatalf("%s round %d: StartChildWorkflow: %v", cfg.name, round, err)
		}
		done := make(chan struct{})
		var wg sync.WaitGroup
		for p := 0; p < 4; p++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-done:
						return
					default:
					}
					out, err := childStore.GetChildResult(ctx, child)
					if err != nil {
						t.Errorf("%s: GetChildResult: %v", cfg.name, err)
						return
					}
					atomic.AddInt64(&res.polls, 1)
					if out.Completed && out.Result != want {
						if atomic.AddInt64(&res.anomalies, 1) == 1 {
							mu.Lock()
							res.firstAnomaly = out.Result
							mu.Unlock()
						}
					}
				}
			}()
		}
		cur := child
		for i := 0; i < cfg.hops; i++ {
			wf, err := claimSpecific(t, ctx, store, cur, "w-child")
			if err != nil {
				t.Fatalf("%s round %d hop %d: claim %s: %v", cfg.name, round, i, cur, err)
			}
			next, err := store.ContinueAsNew(ctx, cur, "w-child", wf.Generation, "intent-workflow", 1, json.RawMessage(`{}`), nil, "", map[string]string{}, 0)
			if err != nil {
				t.Fatalf("%s round %d hop %d: ContinueAsNew: %v", cfg.name, round, i, err)
			}
			atomic.AddInt64(&res.hopsRun, 1)
			cur = next
		}
		last, err := claimSpecific(t, ctx, store, cur, "w-child")
		if err != nil {
			t.Fatalf("%s round %d: claim final run: %v", cfg.name, round, err)
		}
		if err := store.FinalizeWorkflowSegment(ctx, cur, "w-child", last.Generation, nil, "done", want, "", "", map[string]string{}, time0()); err != nil {
			t.Fatalf("%s round %d: finalize: %v", cfg.name, round, err)
		}
		close(done)
		wg.Wait()
		// And the settled answer is the real one, once the chain has stopped.
		out, err := childStore.GetChildResult(ctx, child)
		if err != nil || !out.Completed || out.Result != want {
			t.Fatalf("%s round %d: after the chain finished, GetChildResult = (%q, completed=%v, %v), want (%q, true)", cfg.name, round, out.Result, out.Completed, err, want)
		}
	}
	return res
}

func TestAChildResultIsNeverASettledAnswerReadFromARunTheChainHasLeft(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			parent := newIntentWorkflow(t, context.Background(), store, "child-race-parent")
			start := time.Now()
			for _, cfg := range []childRaceConfig{
				{"CONTROL: the chain never moves", 0, 4},
				{"the chain moves 40 times per round", 40, 5},
			} {
				r := runChildResultRace(t, store, parent, cfg)
				t.Logf("%s: %d anomalies in %d polls, %d hops run (%d asked for), first anomalous result %q", cfg.name, r.anomalies, r.polls, r.hopsRun, int64(cfg.hops*cfg.rounds), r.firstAnomaly)
				if want := int64(cfg.hops * cfg.rounds); r.hopsRun != want {
					t.Fatalf("%s: ran %d hops, asked for %d: the row did not measure the configuration it names", cfg.name, r.hopsRun, want)
				}
				if r.polls < 50 {
					t.Fatalf("%s: only %d polls, too few to tell a hazard from its absence", cfg.name, r.polls)
				}
				if r.anomalies != 0 {
					t.Errorf("%s: %d of %d polls returned a SETTLED answer that is not the child's result (first: %q). A parent awaiting this child would record it in its own history.", cfg.name, r.anomalies, r.polls, r.firstAnomaly)
				}
			}
			t.Logf("%s took %s", backend.Name(), time.Since(start).Round(time.Millisecond))
		})
	}
}
