package typescript

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The worker-level half of cleat#3826: a defer segment that suspends and resumes on a REAL WORKER.
//
// The engine-level test (typescript_defer_segment_test.go) proves the segment's semantics and is what lets `javascript`
// into deferSegmentLanguages. It does not prove the path a worker takes, and that gap is exactly why #3826 stayed open:
// the issue's done-when says "a workflow that uses a defer segment suspends and resumes on a worker", and until this
// test nothing crossed the two.
//
// The worker sets the defer phase itself when a run owes one (cmd/cleat-worker/setup.go: `deferPhase :=
// wf.PendingTerminalStatus != ""`), so the way to reach that branch from outside is to make a run owe it: cancel a
// suspended workflow that registered a defer. Everything below is asserted through the run's own HTTP API and through
// the worker's outbound service calls, so it is the worker's behaviour being measured, not a re-run of the engine's.

// callRecorder is the outside world: the worker's service forwarder POSTs /call/<service>/<operation> to it, so a call
// that reached the recorder is a call the guest really made.
type callRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (c *callRecorder) add(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, s)
}

func (c *callRecorder) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *callRecorder) has(s string) bool {
	for _, got := range c.snapshot() {
		if got == s {
			return true
		}
	}
	return false
}

func TestTypeScriptDeferSegmentRunsOnARealWorker(t *testing.T) {
	rec := &callRecorder{}
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(strings.TrimPrefix(r.URL.Path, "/call/"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(svc.Close)

	e := newWorkerEnv(t, map[string]string{
		"defer_terminal": "tests/typescript/testdata/workflows/defer_terminal.ts",
	})
	// --bench-svc-url forwards the fixture's calls here; without a server the call returns "service not configured", and
	// a defer that was refused looks the same from outside as one that ran. The egress guard exempts exactly the endpoint
	// it is about to dial (cmd/cleat-worker/service_egress.go), which is why a loopback httptest server is reachable.
	w := startWorker(t, e.workerBin, e.appDSN, e.ownerDSN, "--bench-svc-url="+svc.URL)

	id := startRun(t, w, e.key, "defer_terminal", map[string]any{})

	// FIRST the ordinary segment's call. A durable call is not made inline: the run records its intent, suspends, and the
	// worker performs it, so waiting for the suspension instead would race the call and read its absence as "the guest
	// never called out" -- which is the assertion this line exists to make impossible.
	callDeadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(callDeadline) && !rec.has("inventory/before") {
		time.Sleep(100 * time.Millisecond)
	}
	if !rec.has("inventory/before") {
		t.Fatalf("the ordinary segment never reached inventory/before, so this test cannot tell a silent segment from a "+
			"guest that stopped calling out. Recorded: %v\nworker log tail:\n%s", rec.snapshot(), tail(w.out.String(), 3000))
	}

	// THEN the resume, which is the word in #3826's done-when this test did not cover until the fixture grew a short
	// sleep: the run stops at that sleep, wakes on the timer, and carries on to `resumed`. The call arriving is the
	// observation -- a run that suspended and never woke cannot make it -- and it is asserted before the cancel so the
	// resume is not inferred from a later state that the cancel could also explain.
	resumeDeadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(resumeDeadline) && !rec.has("inventory/resumed") {
		time.Sleep(100 * time.Millisecond)
	}
	if !rec.has("inventory/resumed") {
		t.Fatalf("the run never resumed from its short sleep: inventory/resumed is missing, recorded %v.\n\n"+
			"A workflow that suspends and is then cancelled has not been shown to RESUME, and that is half of what the "+
			"done-when asks for.\nworker log tail:\n%s", rec.snapshot(), tail(w.out.String(), 3000))
	}

	// Then the long sleep. "A run waiting out a sleep is not held by any worker (it reads ready)" -- the same status this
	// waits for, and the point at which the run can be cancelled.
	var status string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if code, r := api(t, w, e.key, "GET", "/api/workflows/"+id, nil); code == 200 {
			status, _ = r["status"].(string)
			if status == "ready" || status == "suspended" {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if status != "ready" && status != "suspended" {
		t.Fatalf("the run never suspended at its sleep, so it never owed a defer phase; last status %q\nworker log tail:\n%s",
			status, tail(w.out.String(), 3000))
	}
	if rec.has("inventory/cleanup") {
		t.Fatalf("the defer ran while the run was still alive. A suspended run has not finished, and its cleanup must not "+
			"run: firing it here releases work the resumed run still needs. Recorded: %v", rec.snapshot())
	}

	// Preemptive, because the cooperative form only sets a flag: the run is asleep and will not look at it.
	code, r := api(t, w, e.key, "POST", "/api/workflows/"+id+"/cancel",
		map[string]any{"preemptive": true, "reason": "cleat#3826 worker-level defer segment"})
	if code != 200 && code != 201 && code != 202 {
		t.Fatalf("cancel: HTTP %d %v", code, r)
	}

	// waitTerminal returns only after the worker has finalized, which is after the defer phase -- so by here the drain
	// has either run or been given its chance.
	done := waitTerminal(t, w, e.key, id, 120*time.Second)
	if s, _ := done["status"].(string); s != "cancelled" {
		t.Fatalf("the cancelled run's status is %q, want \"cancelled\"; reply %v", s, done)
	}

	got := rec.snapshot()
	if !rec.has("inventory/cleanup") {
		t.Fatalf("the defer segment did not run the defer body: recorded %v, want inventory/cleanup to be among them.\n\n"+
			"This is the worker path: if the engine-level test passes and this one does not, the gap is between the worker's "+
			"defer phase and the guest's drain, not in the guest's decode.\nworker log tail:\n%s",
			got, tail(w.out.String(), 3000))
	}
	for _, call := range got {
		if call == "inventory/after" {
			t.Fatalf("the workflow BODY reached the caller past the stop inside a defer segment: %v.\n\n"+
				"`after` is new work past the recorded frontier, and a segment exists to prevent exactly that work.", got)
		}
	}
}
