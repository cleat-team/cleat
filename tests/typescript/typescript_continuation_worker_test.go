package typescript

// continueAsNew against a real worker and each dialect's real store (cleat#3827, group G), listed in both dialect jobs in
// typescript-tier2.yml. The in-process tests read the suspension the host reports; what only a worker can answer is whether the new run
// STARTS, with the input the guest gave, and ends at the answer that input leads to.
//
// The chain is read through GET /api/workflows/<id>/terminal ("the last run in this id's ContinueAsNew chain as of now"), which is not
// a terminal STATUS: while the chain is still working it names the run that is executing, so this polls on the answer and not on the
// first 200.

import (
	"fmt"
	"testing"
	"time"
)

// chainEnd polls the last run of id's continue-as-new chain until it is terminal, and returns that run's reply.
func chainEnd(t *testing.T, w *worker, key, id string, within time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	var last map[string]any
	for time.Now().Before(deadline) {
		code, r := api(t, w, key, "GET", "/api/workflows/"+id+"/terminal", nil)
		if code == 200 {
			last = r
			if s, _ := r["status"].(string); isTerminal(s) {
				return r
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the chain of %s did not end within %s; last reply %v\nworker log tail:\n%s", id, within, last, tail(w.out.String(), 3000))
	return nil
}

func TestTypeScriptContinueAsNewOnARealWorker(t *testing.T) {
	const src = "tests/typescript/testdata/workflows/continuation.ts"
	e := newWorkerEnv(t, map[string]string{
		"continuation":                src,
		"continuation_versioned_once": src,
	})
	w := startWorker(t, e.workerBin, e.appDSN, e.ownerDSN)

	// 1. A run that continues as new three times, each with the input the guest chose, and then returns: the end of the chain holds
	//    the answer that only the continued runs could have computed, and it is a DIFFERENT run from the one that was started.
	id := startRun(t, w, e.key, "continuation", map[string]any{"n": 1, "limit": 4})
	end := chainEnd(t, w, e.key, id, 120*time.Second)
	if s, _ := end["status"].(string); s != "done" {
		t.Fatalf("the end of the chain is %q, want done: %v\nworker log tail:\n%s", s, end, tail(w.out.String(), 3000))
	}
	if got := resultObject(t, end); got["n"] != float64(4) {
		t.Errorf("the chain ended with %v, want n=4: the new runs did not receive the input the previous run asked for", got)
	}
	if endID, _ := end["id"].(string); endID == id {
		t.Errorf("the end of the chain is the run that was started (%s): nothing continued", id)
	}

	// 2. The versioned form. The run is asked to continue on version 2 and the worker has only version 1 deployed. MEASURED, not
	//    assumed: the worker starts the next run on the version it was on (cmd/cleat-worker/setup.go passes wf.DefVersion to
	//    ContinueAsNew and never reads the suspension's NewVersion), so the chain ends normally on version 1. If a worker ever honours
	//    the requested version this fails, which is the point: groups/continuation.ts and the pull request say it is not honoured.
	vid := startRun(t, w, e.key, "continuation_versioned_once", map[string]any{"n": 0})
	vend := chainEnd(t, w, e.key, vid, 120*time.Second)
	if s, _ := vend["status"].(string); s != "done" {
		t.Fatalf("the versioned chain ended %q: %v\nworker log tail:\n%s", s, vend, tail(w.out.String(), 3000))
	}
	if got := resultObject(t, vend); got["n"] != float64(1) {
		t.Errorf("the versioned chain ended with %v, want n=1", got)
	}
	if v := fmt.Sprint(vend["def_version"]); v != "1" {
		t.Errorf("the continued run is on version %s, want 1: the worker now honours the version the guest asked for, so the 'not honoured' notes in groups/continuation.ts are stale", v)
	}
}
