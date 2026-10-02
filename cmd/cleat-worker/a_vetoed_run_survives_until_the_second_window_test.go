package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// TestAVetoedRunSurvivesOneMoreWindowAndAKilledHolderDoesNot is cleat#2196's
// acceptance criterion, against a real database and a real HTTP peer:
//
//	"A worker whose heartbeats are artificially delayed, but which answers the
//	 holds endpoint, keeps its runs for up to N windows. A worker that is killed
//	 loses them within the normal window."
//
// The delayed worker here is a real /internal/holds listener whose registry row
// has been back-dated BEYOND THE MEMBERSHIP LEASE but inside the registry
// retention -- which is the case the whole channel exists for, and the case a
// seeded-and-never-swept row would quietly fail to reproduce. The killed worker
// is the same row with nobody listening at the address it published.
//
// Both directions are asserted, because either alone passes against a broken
// channel: a test that only checks the veto fires also passes if the reaper
// never reclaims anything, and a test that only checks a dead holder's run is
// reclaimed is the pre-#2196 behaviour, which needs no channel at all.
func TestAVetoedRunSurvivesOneMoreWindowAndAKilledHolderDoesNot(t *testing.T) {
	// No guard of its own on the DSN or on -short: SuiteTestDB already skips
	// when nothing is configured, FAILS when something was configured and is
	// unreachable, and skips in short mode. A raw os.Getenv check here would be
	// the weaker test -- it would skip over a DSN that was set and broken, which
	// is the one case scripts/check-skips.sh exists to stop being invisible.
	db := testutil.SuiteTestDB(t, "cleat_worker_veto")
	ctx := context.Background()
	store := engine.NewPostgresStore(db)

	const defName = "veto-channel-def"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	// A 1s heartbeat puts reclaimAfter (the window a row must be stale by to be
	// reclaimable at all) at its 10s floor, so the ask window is [10s, 20s): a
	// row 15s stale is asked about, and one 25s stale is not.
	const heartbeat = time.Second
	const secret = "acceptance-test-shared-secret"

	// The delayed holder: a real listener that will answer "yes, still mine".
	holder := &Worker{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	runID := fmt.Sprintf("veto-run-%d", time.Now().UnixNano())
	const generation = 1
	holder.inflight.Store(runID, &engine.WorkflowInstance{ID: runID, Generation: generation})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := newInternalHoldsServer("", secret, holder)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	holderAddress := ln.Addr().String()

	reg := &engine.WorkerRegistry{DB: db, Dialect: engine.DialectPostgres}
	const holderID = "veto-holder-1"
	if err := reg.Register(ctx, engine.WorkerRegistration{
		WorkerID: holderID, Hostname: "veto-holder", Address: holderAddress,
		PID: 1, Concurrency: 1, ConnectionBudget: 1,
	}); err != nil {
		t.Fatalf("registering the holder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM admin.workers WHERE worker_id = $1`, holderID)
	})

	// THE HEARTBEATS ARE DELAYED: the holder's registry row is aged past the
	// membership lease (max(2*heartbeat, 10s) == 10s) while staying inside the
	// registry retention. This is the state the channel exists for, and the one
	// the old sweep (the lease) deleted -- at which point no maxAge passed to
	// ListLive can bring the address back, and the veto can never fire.
	if _, err := db.ExecContext(ctx, `
		UPDATE admin.workers SET last_heartbeat_at = now() - interval '15 seconds'
		WHERE worker_id = $1`, holderID); err != nil {
		t.Fatalf("back-dating the holder's registry row: %v", err)
	}

	seedRun := func(staleBy string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input,
			                                assigned_to, heartbeat_at, generation, tenant_id)
			VALUES ($1, $2, 1, 'running', '{}', $3, now() - $4::interval, $5, $6)`,
			runID, defName, holderID, staleBy, generation, engine.DefaultTenantUUID); err != nil {
			t.Fatalf("seeding the run: %v", err)
		}
	}
	ageRun := func(staleBy string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
			UPDATE workflow_instances SET heartbeat_at = now() - $2::interval WHERE id = $1`,
			runID, staleBy); err != nil {
			t.Fatalf("ageing the run: %v", err)
		}
	}
	statusOf := func() string {
		t.Helper()
		var status string
		if err := db.QueryRowContext(ctx,
			`SELECT status FROM workflow_instances WHERE id = $1`, runID).Scan(&status); err != nil {
			t.Fatalf("reading status of %s: %v", runID, err)
		}
		return status
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	})

	// The reaper: a real Worker against the real store, with the channel
	// configured (a secret to authenticate with, and the registry to resolve
	// addresses from).
	w := newRealBackgroundLoopWorker(t, db, store, "veto-reaper")
	w.heartbeatInterval = heartbeat
	w.reclaimTimeout = 0 // derive: reclaimAfter = 10s at a 1s heartbeat
	w.internalAuthSecret = secret
	w.workerRegistry = reg
	seedRecentlyConfirmedHealthy(w)

	seedRun("15 seconds")
	w.reapOnce()
	if got := statusOf(); got != "running" {
		t.Fatalf("the holder answered that it still holds run %s, but the reaper reclaimed it anyway: status = %q, want running", runID, got)
	}

	// Past TWO windows, so it is no longer asked about at all -- this is the
	// bound: an honest holder cannot hold its own run past one extra window,
	// and neither can a lying one.
	ageRun("25 seconds")
	w.reapOnce()
	if got := statusOf(); got == "running" {
		t.Fatalf("run %s is two windows stale, so the veto must no longer cover it, but it was left running", runID)
	}

	// THE KILLED HOLDER: nobody is listening at the address it published. Its
	// run must be reclaimed within the NORMAL window -- one window stale, not
	// two. Unreachable and dead are deliberately the same from here.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("stopping the holder's listener: %v", err)
	}
	ageRun("15 seconds")
	w.reapOnce()
	if got := statusOf(); got == "running" {
		t.Fatalf("run %s belongs to a holder that cannot be reached at all, and is one window stale; "+
			"a killed worker must lose its runs within the normal window, but it was left running", runID)
	}
}
