package typescript

// The cron bindings against each dialect's REAL schedule store, on a real worker (cleat#3827, group F).
//
// The in-process tests have no WorkflowStore, so they can show the argument checks, the "no workflow store configured" answer and
// the guard, and nothing about a schedule that exists. What a guest actually gets back from `scheduleCron`, `listCrons` and
// `deleteCron` is whatever PostgreSQL, MySQL and SQL Server's stores say, read here once per dialect (CLEAT_TS_DIALECT), listed in
// both dialect jobs in typescript-tier2.yml.
//
// A SCHEDULE OUTLIVES THE RUN THAT MADE IT, so this test is careful to leave none behind: the workflow deletes its own and the
// test asserts it is gone, and a t.Cleanup removes anything still addressed to this run's target through the worker's HTTP API,
// pass or fail. On MySQL and SQL Server the default tenant's data lives in a database that outlives the scratch one, so a schedule
// left by a failed run would still be there for the next one (and would fire at its target on its next occurrence). The target is
// a workflow that is never deployed, so a schedule that did survive would fire at nothing.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// rawAPI is api() for replies that are not a JSON object (the schedule list is an array).
func rawAPI(t *testing.T, w *worker, key, method, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+w.addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// schedulesFor returns the names of the tenant's schedules whose target is `target`, read through the worker's HTTP API.
func schedulesFor(t *testing.T, w *worker, key, target string) []string {
	t.Helper()
	code, body := rawAPI(t, w, key, "GET", "/api/schedules")
	if code != 200 {
		t.Fatalf("GET /api/schedules: HTTP %d %s", code, body)
	}
	var list []struct {
		Name    string `json:"name"`
		DefName string `json:"def_name"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("/api/schedules is not a JSON array: %v\n%s", err, body)
	}
	var names []string
	for _, s := range list {
		if s.DefName == target {
			names = append(names, s.Name)
		}
	}
	return names
}

func TestTypeScriptCronSchedulesOnARealWorker(t *testing.T) {
	const src = "tests/typescript/testdata/workflows/schedules.ts"
	e := newWorkerEnv(t, map[string]string{"schedules_round_trip": src})
	w := startWorker(t, e.workerBin, e.appDSN, e.ownerDSN)

	target := fmt.Sprintf("never-deployed-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, name := range schedulesFor(t, w, e.key, target) {
			rawAPI(t, w, e.key, "DELETE", "/api/schedules/"+name)
		}
	})

	done := waitTerminal(t, w, e.key, startRun(t, w, e.key, "schedules_round_trip", map[string]any{"target": target}), 90*time.Second)
	if s, _ := done["status"].(string); s != "done" {
		t.Fatalf("run ended %q: %v\nworker log tail:\n%s", s, done, tail(w.out.String(), 3000))
	}
	got := resultObject(t, done)

	id, _ := got["id"].(string)
	if id == "" {
		t.Fatalf("scheduleCron resolved with no id: %v", got)
	}
	// The schedule the guest read back through listCrons: its own, with the fields it asked for, in the SDK's case.
	listed, _ := got["listed"].([]any)
	if len(listed) != 1 {
		t.Fatalf("listCrons showed %d entries for the id %s, want exactly 1: %v", len(listed), id, got["listed"])
	}
	c, _ := listed[0].(map[string]any)
	if c["workflowName"] != target || c["cronExpr"] != "0 0 1 1 *" || c["timezone"] != "Europe/Berlin" || c["enabled"] != true {
		t.Errorf("listed schedule %v, want workflowName=%s cronExpr=\"0 0 1 1 *\" timezone=Europe/Berlin enabled=true", c, target)
	}
	if in, _ := c["input"].(map[string]any); in["n"] != float64(1) || in["tag"] != "x" {
		t.Errorf("listed input %v, want the {n:1, tag:\"x\"} that was scheduled, parsed", c["input"])
	}
	if got["afterDeleteCount"] != float64(0) {
		t.Errorf("after deleteCron the schedule was still listed %v time(s)", got["afterDeleteCount"])
	}
	// The workflow deleted a second time and the run still finished, which is the retry-safety the engine promises; and the
	// schedule is gone from the STORE too, which the guest's own listing could not show if listCrons were wrong.
	if left := schedulesFor(t, w, e.key, target); len(left) != 0 {
		t.Errorf("%d schedule(s) addressed to %s still in the store after the run deleted its own: %v", len(left), target, left)
	}
	if msg, _ := got["badExpr"].(string); !strings.Contains(msg, "field(s)") || strings.Contains(msg, "\x00") {
		t.Errorf("a bad expression was rejected with %q, want the scheduler's reason", msg)
	}
}
