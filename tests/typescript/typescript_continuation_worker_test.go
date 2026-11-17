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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/wasm"
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
	e := newWorkerEnv(t, map[string]string{"continuation": src})
	w := startWorker(t, e.workerBin, e.appDSN, e.ownerDSN)

	// A run that continues as new three times, each with the input the guest chose, and then returns: the end of the chain holds the
	// answer that only the continued runs could have computed, and it is a DIFFERENT run from the one that was started.
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
}

// cleat#4041 (owner-approved 2026-11-17): the run that follows continueAsNew(input, {version}) starts ON that version, and a version that
// is not deployed FAILS the run loudly. Two versions of one definition are deployed (the same entry in two files, differing only in the
// `via` the run reports), so a run starts on the LATEST, version 2, and the versions it asks for are told apart by where it lands.
//
// The ask is mostly for version 1, which the old behaviour could not satisfy: it started the next run on the current version, 2, whatever
// was requested, so asking for 2 would have passed against the defect.
func TestTypeScriptContinueAsNewStartsTheNextRunOnTheRequestedVersion(t *testing.T) {
	const v1 = "tests/typescript/testdata/workflows/continuation_chain_v1.ts"
	const v2 = "tests/typescript/testdata/workflows/continuation_chain_v2.ts"
	e := newWorkerEnv(t, map[string]string{"chain": v1})
	res := build(t, e.root, v2, "chain")
	if res.exit != 0 {
		t.Fatalf("building %s: build.sh exited %d:\n%s", v2, res.exit, res.output)
	}
	// Version 2 is STAMPED into the module. cleatctl assigns the next free number, but deploy-workflow (the deploy tool for MySQL and SQL
	// Server) takes the version embedded in the binary and only auto-increments when there is none, and a TypeScript build embeds 1: so
	// without the stamp the second deploy on those dialects is version 1 again, and this test measured exactly that.
	raw, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := wasm.ReadMetadata(raw)
	if err != nil {
		t.Fatalf("reading the module's metadata to stamp version 2: %v", err)
	}
	meta.WorkflowVersion = 2
	stamped, err := wasm.WriteMetadata(raw, meta)
	if err != nil {
		t.Fatal(err)
	}
	v2Path := filepath.Join(t.TempDir(), "chain-v2.wasm")
	if err := os.WriteFile(v2Path, stamped, 0o644); err != nil {
		t.Fatal(err)
	}
	deployWorkflow(t, e.ctl, e.ownerDSN, "chain", v2Path)
	w := startWorker(t, e.workerBin, e.appDSN, e.ownerDSN)

	run := func(input map[string]any) (startID string, end map[string]any) {
		id := startRun(t, w, e.key, "chain", input)
		return id, chainEnd(t, w, e.key, id, 120*time.Second)
	}
	versionOf := func(r map[string]any) string { return fmt.Sprint(r["def_version"]) }

	// Precondition: a run starts on the latest version. Without it the first case below would not discriminate.
	startID, end := run(map[string]any{"n": 0, "version": 2})
	if first := getRun(t, w, e.key, startID); versionOf(first) != "2" {
		t.Fatalf("a fresh run started on version %s, want 2 (the latest): the cases below would prove nothing", versionOf(first))
	}
	if s, _ := end["status"].(string); s != "done" || resultObject(t, end)["via"] != "v2" {
		t.Fatalf("asking for version 2 from version 2: status %v, result %v; want done on v2", end["status"], end["result"])
	}

	// 1. A DIFFERENT, deployed version is honoured. Fails on the old behaviour, which landed on 2.
	_, end = run(map[string]any{"n": 0, "version": 1})
	if s, _ := end["status"].(string); s != "done" {
		t.Fatalf("asking for version 1: the chain ended %q: %v\nworker log tail:\n%s", s, end, tail(w.out.String(), 3000))
	}
	if got := resultObject(t, end); got["via"] != "v1" || versionOf(end) != "1" {
		t.Errorf("asked for version 1 and landed on def_version %s running %v: the worker ignored the requested version", versionOf(end), got["via"])
	}

	// 2. The plain form (no version) stays on the current version, here 2. Unchanged by the fix, and what a worker that read NewVersion
	//    wrongly (for example treating 0 as "version 0") would break.
	_, end = run(map[string]any{"n": 0})
	if got := resultObject(t, end); got["via"] != "v2" || versionOf(end) != "2" {
		t.Errorf("a plain continueAsNew landed on def_version %s running %v, want 2 and v2", versionOf(end), got["via"])
	}

	// 3. A version that is not deployed FAILS the run, naming the version, and does not fall back to the current one. Nothing is written
	//    for the refused continuation, so the chain's last run is the one that was started, failed.
	startID, end = run(map[string]any{"n": 0, "version": 99})
	if s, _ := end["status"].(string); s != "failed" {
		t.Fatalf("asking for undeployed version 99: the chain ended %q, want failed: %v", s, end)
	}
	if endID, _ := end["id"].(string); endID != startID {
		t.Errorf("a refused continuation started a new run (%s after %s): it must leave nothing written", endID, startID)
	}
	msg := fmt.Sprint(end["error"], end["error_message"])
	for _, want := range []string{"version 99", `"chain"`, "not deployed", "rather than continued on version 2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure %q does not say %q", msg, want)
		}
	}
}

// getRun reads one run by id (not the end of its chain).
func getRun(t *testing.T, w *worker, key, id string) map[string]any {
	t.Helper()
	code, r := api(t, w, key, "GET", "/api/workflows/"+id, nil)
	if code != 200 {
		t.Fatalf("GET %s: HTTP %d %v", id, code, r)
	}
	return r
}
