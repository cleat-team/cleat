package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

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
	id := "rdr-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, _, err := store.StartNewRun(ctx, id, def, 1, json.RawMessage(`{}`), "",
		engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	claimed, err := store.ClaimWorkflow(ctx, "dry-run-worker")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimWorkflow: %v %v", claimed, err)
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
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_instances SET completed_at = $2 WHERE id = $1`,
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
