package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// TestADryRunReportsWithoutDeleting is cleat#1457 at the endpoint.
//
// The engine test asserts the preview agrees with the sweep arm for arm. This
// asserts the three things only the HTTP surface can be wrong about:
//
//	the response marks itself dry_run, so a caller cannot read it as a sweep;
//	the rows are still there afterwards;
//	a disabled arm is reported in `skipped` here exactly as in a real sweep.
//
// The third matters more than it looks. retentionSweepResult's own doc comment
// says `skipped` exists so a zero is never ambiguous between "disabled" and
// "found nothing" -- and a PREVIEW is read before deciding to run the dangerous
// thing, so an ambiguous zero there is worse than one in the report afterwards.
func TestADryRunReportsWithoutDeleting(t *testing.T) {
	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	on := true
	oldGate, oldDays, oldDL := enableAdminAPI, completedWorkflowRetentionDays, deadLetterRetentionDays
	days, off := 30, 0
	enableAdminAPI, completedWorkflowRetentionDays, deadLetterRetentionDays = &on, &days, &off
	defer func() {
		enableAdminAPI, completedWorkflowRetentionDays, deadLetterRetentionDays = oldGate, oldDays, oldDL
	}()

	w := &Worker{store: store, ctx: ctx, Metrics: newTestPrometheus(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := &apiServer{worker: w}

	const def = "retention-dry-run"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: def, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	id := "rdr-" + stamp

	// CLAIM ONLY THIS TEST'S RUN (cleat#3603). ClaimWorkflow takes the next ready run on the store's task
	// queues, and the suite database is shared by every test in the package and persists between runs, so
	// a run another test left `ready` on the default queue would be claimed and finalized INSTEAD of the
	// one seeded here. The seeded row would then never complete, and whether the test still passed would
	// depend on whether the stranger's finalized row happened to match the preview too -- which it does or
	// does not according to the database clock's offset from this process's. So this definition gets a
	// queue of its own, and the claim goes through a store that polls only that queue.
	queue := "dry-run-" + stamp

	// A STRANGER IS PLANTED ON THE DEFAULT QUEUE, every run, so the isolation above is exercised instead of
	// merely assumed. It is the realistic leak: a run another test left `ready`. It is cancelled when this
	// test ends, so the test cannot become the leaker it guards against. Started under a definition of its
	// own, because `def` is about to be moved to this test's queue.
	const strangerDef = "retention-dry-run-stranger"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: strangerDef, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy the stranger: %v", err)
	}
	strangerID := "rdr-stranger-" + stamp
	if _, _, err := store.StartNewRun(ctx, strangerID, strangerDef, 1, json.RawMessage(`{}`), "",
		engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun (stranger): %v", err)
	}
	// Both runs are settled on the way out, but only if they are still `ready`: the seeded run normally
	// finishes `done` and keeps that status, and it is the run a FAILING test leaves behind, which is how a
	// test turns into the next test's leak.
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`UPDATE workflow_leases SET status = 'cancelled' WHERE id = ANY($1) AND status = 'ready'`,
			pq.Array([]string{strangerID, "rdr-" + stamp})); err != nil {
			t.Errorf("cancel the planted stranger %s and the seeded run: %v -- they are now ready runs in the shared suite database",
				strangerID, err)
		}
	})
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_defs SET task_queue = $1 WHERE name = $2 AND version = 1 AND tenant_id = $3`,
		queue, def, engine.DefaultTenantUUID); err != nil {
		t.Fatalf("give the definition its own task queue: %v", err)
	}
	if _, _, err := store.StartNewRun(ctx, id, def, 1, json.RawMessage(`{}`), "",
		engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	claimed, err := engine.NewPostgresStore(db, queue).ClaimWorkflow(ctx, "dry-run-worker")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimWorkflow: %v %v", claimed, err)
	}
	// Belt and braces, and what names the cause if the isolation above ever stops holding: a failure here
	// says a run that is not this test's was claimed, instead of the fixture-shaped "the preview reported
	// 0 completed workflows" it would otherwise surface as.
	if claimed.ID != id {
		t.Fatalf("claimed %q, but this test seeded %q on its own task queue %q: the claim reached a run that is not this test's",
			claimed.ID, id, queue)
	}
	if err := store.FinalizeWorkflowSegment(ctx, claimed.ID, "dry-run-worker",
		claimed.Generation, nil, "done", `{"ok":true}`, "", "", nil, time.Time{}); err != nil {
		t.Fatalf("FinalizeWorkflowSegment: %v", err)
	}

	// AGE THE SEEDED ROW. DO NOT WIDEN THE WINDOW. cleat#2423.
	//
	// The predicate is `completed_at < $1` (pgCompletedWorkflows,
	// engine/retention_predicates.go) and the cutoff below is
	// `previewedAt.Add(-window)` with window=1ns, so the seeded row has to be
	// strictly older than an instant one nanosecond ago -- while `completed_at`
	// is written by PostgreSQL's own now().
	//
	// THE TWO CLOCKS ARE NOT THE SAME CLOCK, and that is what decides it. The
	// row's timestamp comes from the DATABASE SERVER; the cutoff comes from
	// time.Now() in THIS PROCESS. Measured on this host, the PostgreSQL
	// container's clock runs AHEAD of the host's by 0.09s to 0.27s:
	//
	//	host=1790445356.920  container=1790445357.006  container-host=+0.086s
	//
	// so the row is stamped up to ~270ms in the cutoff's FUTURE, `completed_at
	// < cutoff` is false, the sweep matches nothing, and the positive control
	// below aborts on a fixture that looks correct.
	//
	// THAT IS DETERMINISTIC, NOT A RACE, and it is why this read as "~1 in 3".
	// Against a freshly dropped suite database it failed 12 times in 12 runs;
	// against a database holding earlier runs' completed rows it passes, because
	// those are minutes old, match the 1ns cutoff easily, and satisfy the count
	// the positive control reads. So the rate was never a coin flip -- it was a
	// function of whether earlier runs had left older rows behind, which is also
	// why WS-3 saw it on fresh databases and not on reused ones.
	//
	// A 1ns window is ALSO below PostgreSQL's resolution, which collapses a
	// nanosecond subtraction to nothing (`now() - interval '0.000000001 seconds'
	// = now()` is true; one microsecond is not). That is a real second fragility
	// of the same window, not the trigger here -- and neither is survivable by a
	// test that has to decide.
	//
	// THE ROW MOVES, NOT THE WINDOW, and the direction is the whole reason: a
	// larger older_than moves the cutoff EARLIER, which makes an
	// already-completed-just-now row LESS likely to match, not more. "Use a
	// bigger window" is the obvious repair and it is backwards. An hour is four
	// orders of magnitude beyond the measured skew and nine beyond the storage
	// resolution, so neither can decide this any more.
	//
	// Not a production defect: the real sweep's window is 30 days, against which
	// 270ms of skew is nothing. The 1ns window exists only here, to mean "match
	// everything already completed".
	// completed_at lives on workflow_payloads (cleat#3245 B1a), which is where
	// the sweep reads it.
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_payloads SET completed_at = $2 WHERE id = $1`,
		id, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("age the seeded row: %v", err)
	}

	decode := func(t *testing.T, body []byte) retentionSweepResult {
		t.Helper()
		var res retentionSweepResult
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("decode: %v (body %s)", err, body)
		}
		return res
	}

	rec := sweepReq(t, s, `{"older_than":"1ns","dry_run":true}`)
	if rec.Code != 200 {
		t.Fatalf("dry run returned %d: %s", rec.Code, rec.Body.String())
	}
	preview := decode(t, rec.Body.Bytes())

	if !preview.DryRun {
		t.Error("the response does not mark itself dry_run. A caller diffing a preview " +
			"against a sweep has nothing in the payload telling the two apart, and the " +
			"counts read as rows that were deleted.")
	}
	// POSITIVE CONTROL. Every assertion below is satisfied by a preview that
	// counted nothing, and so is a preview that is not wired up at all.
	if preview.CompletedWorkflows == 0 {
		t.Fatalf("the preview reported 0 completed workflows with one seeded to match "+
			"at older_than=1ns. Nothing below this line measures anything. body=%s",
			rec.Body.String())
	}
	// The disabled arm must be named, not silently zero.
	foundSkip := false
	for _, sk := range preview.Skipped {
		if sk != "" && len(sk) > 0 && sk[0] == 'd' {
			foundSkip = true
		}
	}
	if !foundSkip {
		t.Errorf("dead_lettered is disabled (--dead-letter-retention-days is 0) and the "+
			"preview did not name it in skipped: %v.\n\nA zero for that arm is then "+
			"ambiguous between 'disabled' and 'nothing matched', in the report an "+
			"operator reads BEFORE running the destructive version.", preview.Skipped)
	}

	// THE ROWS ARE STILL THERE. This is the assertion the whole feature is for,
	// and it is checked through the store rather than the API so a preview that
	// lied in its own response cannot satisfy it.
	stillThere, err := store.CountCompletedWorkflows(ctx, time.Now())
	if err != nil {
		t.Fatalf("CountCompletedWorkflows: %v", err)
	}
	if stillThere < preview.CompletedWorkflows {
		t.Errorf("the dry run deleted rows: %d remain, preview reported %d",
			stillThere, preview.CompletedWorkflows)
	}

	// And the real sweep still works afterwards -- a preview must not leave the
	// store in a state where the sweep cannot run.
	rec2 := sweepReq(t, s, `{"older_than":"1ns"}`)
	if rec2.Code != 200 {
		t.Fatalf("sweep after dry run returned %d: %s", rec2.Code, rec2.Body.String())
	}
	swept := decode(t, rec2.Body.Bytes())
	if swept.DryRun {
		t.Error("a real sweep marked itself dry_run")
	}
	if swept.CompletedWorkflows == 0 {
		t.Error("the sweep after the dry run deleted nothing, so the dry run either " +
			"consumed the rows or the sweep stopped working")
	}
}
