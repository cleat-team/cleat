package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3571. --heartbeat-max-connections gives the worker's heartbeat its own
// pool so a burst of execution traffic cannot queue a beat behind it (cleat#2009).
// heartbeatAndFenceInFlight sent only the worker's OWN tenant's runs through that
// pool; every other tenant's beat went through storeForTenant, which on
// PostgreSQL is a struct on the shared execution *sql.DB and on SQL Server is a
// per-tenant execution pool. So the protection covered one tenant, and a worker
// serving others (--claim-across-tenants) had it for the runs it was least
// likely to need it for.
//
// # What this measures
//
// The execution pool is a one-connection pool whose only connection is held for
// the whole test, so any statement sent through it queues. A run of the given
// tenant is claimed and in flight; the real heartbeatAndFenceInFlight is called
// with a real isolated heartbeat pool wired in. It must land, must not fence the
// run, and must move heartbeat_at -- the column the reaper reads. A beat that
// queued behind the held connection does none of those: it runs out its
// deadline and reports failure.
//
// The worker's OWN tenant is the known-positive. Its beat already took the
// isolated pool before this change, so it passes on the old code, which is what
// shows the saturation is the only difference between the two arms.
//
// # What it does not cover
//
// Saturation is one cause of a late beat. A paused process, a degraded network
// path and a slow disk are not helped by a second pool, and nothing here claims
// they are. SQL Server's per-tenant execution pool is modelled as a plain
// one-connection pool scoped through WithTenant, which is the same thing the
// factory builds minus a connector that sets the tenant on connect; the store
// asserts its tenant per transaction either way (beginTxWithContext).

const poolTenant3571 = "44444444-0000-4000-8000-000000003571"

// poolFactory3571 is a StoreFactory whose every store lives on ONE pool, the way
// PostgresStoreFactory's do. It is how the test says "this is the execution pool".
type poolFactory3571 struct {
	mu     sync.Mutex
	pool   *sql.DB
	scoped func(pool *sql.DB, tenant string) engine.WorkflowStore
	opened []string
}

func (f *poolFactory3571) OpenStore(_ context.Context, tenantID string, _ ...string) (engine.WorkflowStore, io.Closer, error) {
	f.mu.Lock()
	f.opened = append(f.opened, tenantID)
	f.mu.Unlock()
	return f.scoped(f.pool, tenantID), closerFunc(func() error { return nil }), nil
}
func (f *poolFactory3571) DriverName() string      { return "pool3571" }
func (f *poolFactory3571) Dialect() engine.Dialect { return engine.DialectPostgres }

func (f *poolFactory3571) openedTenants() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.opened...)
}

func TestATenantsBeatDoesNotQueueBehindTheExecutionPoolUnderRLS(t *testing.T) {
	owner := testutil.SuiteTestDB(t, "cleat_worker_hbpool3571")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	var bypass, super bool
	if err := rlsDB.QueryRow(
		`SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if bypass || super {
		t.Fatalf("the RLS role has BYPASSRLS=%v SUPERUSER=%v; this test would measure nothing", bypass, super)
	}
	rig := postgresHBRig(t, owner, rlsDB, true, poolTenant3571)
	t.Run("a tenant the worker does not serve as its own", func(t *testing.T) {
		runABeatAgainstASaturatedExecutionPool(t, rig, poolTenant3571)
	})
	t.Run("known-positive: the worker's own tenant, whose beat already had the pool", func(t *testing.T) {
		runABeatAgainstASaturatedExecutionPool(t, rig, engine.DefaultTenantUUID)
	})
}

// The SQL Server arm is its own test so that an unset CLEAT_TEST_MSSQL skips it
// without taking the PostgreSQL arm with it.
func TestATenantsBeatDoesNotQueueBehindTheExecutionPoolUnderRLSOnSQLServer(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping the SQL Server cleat#3571 test")
	}
	rig := mssqlHBRig(t, "55555555-0000-4000-8000-000000003571")
	t.Run("a tenant the worker does not serve as its own", func(t *testing.T) {
		runABeatAgainstASaturatedExecutionPool(t, rig, rig.tenant)
	})
	t.Run("known-positive: the worker's own tenant, whose beat already had the pool", func(t *testing.T) {
		runABeatAgainstASaturatedExecutionPool(t, rig, engine.DefaultTenantUUID)
	})
}

func runABeatAgainstASaturatedExecutionPool(t *testing.T, rig *hbRig, runTenant string) {
	t.Helper()
	ctx := context.Background()
	if runTenant != engine.DefaultTenantUUID {
		rig.register(t, runTenant)
	}

	// The run is set up through the rig's own, unsaturated store.
	var setup engine.WorkflowStore = rig.ownStore
	if runTenant != engine.DefaultTenantUUID {
		st, closeSetup, err := rig.factory.OpenStore(ctx, runTenant, "default")
		if err != nil {
			t.Fatalf("opening the tenant's store: %v", err)
		}
		defer closeSetup.Close()
		setup = st
	}
	wasmBytes := buildDeferFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	defName := "hb-pool-3571-" + runTenant[len(runTenant)-4:]
	if err := setup.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	wfID := fmt.Sprintf("hb-pool-3571-%d", time.Now().UnixNano())
	if _, _, err := setup.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
		json.RawMessage(`{"__entry_point":"TwoSequentialCalls"}`), "", runTenant, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	const workerID = "hb-pool-3571-worker"
	claimed := claimOne(t, ctx, setup, workerID, wfID)
	if claimed.TenantID != runTenant {
		t.Fatalf("claimed run belongs to %q, want %q", claimed.TenantID, runTenant)
	}

	// Two more pools on the same database, one connection each: the execution
	// pool, and the heartbeat pool the worker was started with.
	execDB := rig.newPool(t)
	execDB.SetMaxOpenConns(1)
	execDB.SetMaxIdleConns(1)
	hbDB := rig.newPool(t)
	hbDB.SetMaxOpenConns(1)
	hbDB.SetMaxIdleConns(1)

	factory := &poolFactory3571{pool: execDB, scoped: rig.scoped}
	w := newTestWorkerFromStore(rig.scoped(execDB, engine.DefaultTenantUUID))
	defer w.cancel()
	w.id = workerID
	w.storeTenantID = engine.DefaultTenantUUID
	w.storeFactory = factory
	w.heartbeatStore = rig.scoped(hbDB, engine.DefaultTenantUUID)
	w.inflight.Store(wfID, claimed)

	// Saturate the execution pool, and confirm it is held before the beat runs:
	// InUse is the fact, not that a goroutine was started.
	hold, err := execDB.Conn(ctx)
	if err != nil {
		t.Fatalf("taking the execution pool's only connection: %v", err)
	}
	defer hold.Close()
	if got := execDB.Stats().InUse; got != 1 {
		t.Fatalf("the execution pool reports %d connection(s) in use, want 1: the saturation did not take hold", got)
	}
	// And the heartbeat pool is NOT: otherwise a pass says nothing about isolation.
	if got := hbDB.Stats().InUse; got != 0 {
		t.Fatalf("the heartbeat pool reports %d connection(s) in use before the beat, want 0", got)
	}

	atClaim := rig.heartbeatAt(t, runTenant, wfID)
	time.Sleep(30 * time.Millisecond) // so the clock differs between the claim and the beat

	start := time.Now()
	landed := w.heartbeatAndFenceInFlight()
	took := time.Since(start)

	if !landed {
		t.Errorf("the beat did not land after %v with the execution pool saturated. A beat that waits behind execution "+
			"traffic is what the heartbeat pool exists to prevent, and it covers only the worker's own tenant's runs "+
			"(cleat#3571)", took)
	}
	if w.runIsFenced(wfID, claimed.Generation) {
		t.Errorf("a healthy run held by this worker was fenced")
	}
	if at := rig.heartbeatAt(t, runTenant, wfID); !at.After(atClaim) {
		t.Errorf("heartbeat_at did not move (%s then %s): the reaper reads this column, so a run whose beat queues "+
			"behind the execution pool is reclaimable while it is still executing", atClaim, at)
	}
	// The beat must not have taken a connection from the execution pool: if it
	// had, it would still be waiting. Opening a store on it is not that, so the
	// check is on the tenants the factory was asked for, which is the routing the
	// fix changes.
	if runTenant != engine.DefaultTenantUUID {
		if got := factory.openedTenants(); len(got) != 0 {
			t.Errorf("the beat opened %v on the execution pool's factory; it should have used the heartbeat pool", got)
		}
	}
}
