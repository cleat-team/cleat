package main

// cleat#3843. A compiled workflow, run by a real worker on wasmtime against a real PostgreSQL, with its history
// compacted while it is suspended and then resumed, finishes with the right RESULT.
//
// Nothing else in the tree proves that. The compaction tests assert the store and the state they produce; the
// replay that consumes them was never run against a real guest, which is why #3837 (a compacted run failed on its
// next resume) and #3838 (a run compacted twice finished `done` with a wrong result and reported nothing) shipped
// and sat for releases. The workflow is testdata/compaction/sigsum, chosen because its result depends on the
// recorded signal payloads: 30 signals of 3*i+1, sum 1335. On the code before #3838's fix the same run gave 1713 on
// develop and on v0.5.0 and 1491 on the 0.6.0 release branch, and a workflow of timed-out awaits, which the first
// attempt used, gave the right answer on all of them.
//
// WHAT IS DRIVEN AND WHAT IS NOT. Compaction runs through the worker's own compactOnce, once between each signal,
// at --compaction-threshold 10: that is the code the ticker runs, on a deterministic schedule, so this takes
// seconds and does not depend on a timer. The ticker itself and the --compaction-interval / --compaction-threshold
// flag wiring (compactionLoop) are NOT exercised here. PostgreSQL only; the other dialects are covered at the store
// level by TestARunCompactedRepeatedlyReplaysTheHistoryItRecordedOnEveryStore and
// TestACompactedRunVerifiesAndResumesOnEveryStore.
//
// CHECKSUM VERIFICATION IS SWITCHED ON EXPLICITLY. The worker enables it only when disableChecksumVerification is
// a non-nil pointer to false (the flag's own default), and the Worker the other tests here build leaves the pointer
// nil, which means OFF. Without it #3837 does not fail, because the chain it breaks is never checked on resume.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

const (
	sigSumSignals = 30
	sigSumResult  = `{"sum":1335,"count":30}` // sum of 3*i+1 for i in 0..29
)

func TestACompactedRunFinishesWithTheRightResultOnARealWorker(t *testing.T) {
	wasmBytes := buildFixture(t, "testdata/compaction/sigsum")
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}

	// every is how many signals arrive between compaction ticks (0 = compaction off). The issue's own schedule is
	// a 10 s tick over signals 3 s apart, which is about every third; every signal is the densest case. They
	// compact different amounts of history between replays, and a defect can hide from one and not the other.
	for _, c := range []struct {
		name    string
		every   int
		compact bool
	}{{"compaction off (control)", 0, false}, {"compaction every signal", 1, true}, {"compaction every third signal", 3, true}} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			db := testutil.SuiteTestDB(t, "cleat_worker")
			store := engine.NewPostgresStore(db)

			defName := fmt.Sprintf("sigsum-%d", time.Now().UnixNano())
			if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
				Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
				ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			wfID := fmt.Sprintf("sigsum-run-%d", time.Now().UnixNano())
			if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
				json.RawMessage(`"go"`), "", engine.DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			logs := &syncBuffer{}
			worker := newRealDeferPhaseWorker(t, db, store, logs)
			worker.id = "sigsum-worker"
			worker.compactionThreshold = 10
			verify := false // disableChecksumVerification=false: verification ON, as the shipped flag default
			worker.disableChecksumVerification = &verify

			// runUntilIdle runs the workflow on this worker until it has nothing ready: suspended on its next
			// await, or finished.
			runUntilIdle := func() {
				t.Helper()
				deadline := time.Now().Add(30 * time.Second)
				for time.Now().Before(deadline) {
					claimed, err := store.ClaimWorkflows(ctx, worker.id, 50)
					if err != nil {
						t.Fatalf("ClaimWorkflows: %v", err)
					}
					ran := false
					for _, wf := range claimed {
						if wf.ID != wfID {
							if err := store.ReleaseWorkflow(ctx, wf.ID, worker.id, wf.Generation, wf.NextWakeAt); err != nil {
								t.Logf("releasing %s, which this test does not own: %v", wf.ID, err)
							}
							continue
						}
						worker.inflight.Store(wfID, wf)
						runExecuteWorkflow(worker, wf)
						ran = true
					}
					if ran {
						return
					}
					time.Sleep(50 * time.Millisecond)
				}
				t.Fatalf("the run was never claimable (status %q)\nlogs:\n%s", statusOf(t, ctx, store, wfID), logs.String())
			}

			compactedAt := map[int]bool{}
			runUntilIdle() // up to its first await
			for i := 0; i < sigSumSignals; i++ {
				if st := statusOf(t, ctx, store, wfID); st == "failed" || st == "done" {
					break
				}
				if c.compact && i%c.every == c.every-1 {
					worker.compactOnce()
					if cs, err := store.LoadCompactionState(ctx, wfID); err != nil {
						t.Fatalf("LoadCompactionState: %v", err)
					} else if cs != nil {
						compactedAt[cs.CompactedStep] = true
					}
				}
				if err := store.DeliverSignal(ctx, wfID, "n", strconv.Itoa(3*i+1)); err != nil {
					t.Fatalf("DeliverSignal %d: %v", i, err)
				}
				runUntilIdle()
			}

			wf, err := store.GetWorkflowByID(ctx, wfID)
			if err != nil || wf == nil {
				t.Fatalf("GetWorkflowByID: %v, %v", err, wf)
			}
			if wf.Status != "done" {
				t.Fatalf("status = %q (error %q), want \"done\"; compactions seen at steps %v\nlogs:\n%s",
					wf.Status, wf.Error, compactedAt, tail(logs.String(), 3000))
			}
			if !sameJSON(wf.Result, sigSumResult) {
				t.Fatalf("result = %s, want %s: the run finished \"done\" with the wrong answer, which is what a replay handed "+
					"the wrong history looks like; compactions seen at steps %v", wf.Result, sigSumResult, compactedAt)
			}

			if !c.compact {
				if cs, err := store.LoadCompactionState(ctx, wfID); err != nil || cs != nil {
					t.Errorf("the control compacted: state %v, err %v; it proves nothing about compaction if it did", cs, err)
				}
				return
			}
			// A run that never compacted passes the assertion above for the wrong reason. Two DISTINCT boundaries
			// means at least two compactions happened, so the second one's rebuild is what the result depends on.
			if len(compactedAt) < 2 {
				t.Errorf("the run was compacted at %d distinct step(s) %v, want at least 2: it did not exercise a second compaction", len(compactedAt), compactedAt)
			}
		})
	}
}

// sameJSON reports whether two JSON documents are equal as values, so key order and spacing do not matter.
func sameJSON(got, want string) bool {
	var g, w any
	if json.Unmarshal([]byte(got), &g) != nil || json.Unmarshal([]byte(want), &w) != nil {
		return got == want
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	return string(gb) == string(wb)
}

// tail returns the last n bytes of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
