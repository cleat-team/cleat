package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// cleat#2209. signalPluginWorkflow and signalPluginWorkflowWithAuth wrap
// plugin.Environment.SignalWorkflow, which webhookingest (routes.go,
// background.go) and eventtriggers (publish.go) call with a ctx that
// ALREADY carries the target's tenant, via plugin.ForTenant -- set from the
// webhook source's own TenantID or from a retry loop's saved one. Before
// this fix, the closures installed in main() ignored that context entirely
// and called DeliverSignal (and, under --require-signal-auth,
// GetAllowedSignalCallers) on the process-wide store still scoped to the
// worker's default tenant.
//
// On Postgres and SQL Server that meant: the INSERT into workflow_signals
// is not blocked by a FILTER predicate (it does not gate INSERT, only
// SELECT/UPDATE/DELETE -- see cleat#2187's investigation), so it succeeded,
// tagging the row with the DEFAULT tenant. The UPDATE that actually wakes
// the target workflow, though, IS filtered -- so it matched zero rows and
// returned no error. SignalWorkflow reported success, webhookingest and
// eventtriggers both marked the triggering event completed, and the target
// workflow never woke. This file proves both halves: that the signal now
// reaches the right row, and that a mismatched tenant fails loudly instead
// of repeating that silent no-op.
//
// Real stores, not mocks, for the same reason as
// a_plugin_start_workflow_is_tenant_scoped_test.go: the defect is in what
// the STORE does under a foreign tenant's context, which a mock cannot show.

// TestSignalPluginWorkflow_PostgresScopesToTheTargetsTenant is the
// PostgreSQL half.
func TestSignalPluginWorkflow_PostgresScopesToTheTargetsTenant(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed cleat#2209 test")
	}
	db := testutil.SuiteTestDB(t, "cleat_worker")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, db)
	factory := engine.NewPostgresStoreFactory(rlsDB, "public")
	ctx := context.Background()

	tenantDefault := engine.DefaultTenantUUID
	tenantA := uuid.New()
	tenantB := uuid.New()

	// The process-wide store: opened for the default tenant, exactly as
	// main() opens it and exactly what plugin.Environment.SignalWorkflow
	// closes over.
	processStore, processCloser, err := factory.OpenStore(ctx, tenantDefault, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer processCloser.Close()

	deployAndStart := func(tenant string) string {
		t.Helper()
		st, closer, err := factory.OpenStore(ctx, tenant, "default")
		if err != nil {
			t.Fatalf("OpenStore(%s): %v", tenant, err)
		}
		defer closer.Close()
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: "signal-2209", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("deploy def for %s: %v", tenant, err)
		}
		runID, _, err := st.StartNewRun(ctx, "", "signal-2209", 1, json.RawMessage(`{}`),
			"start-"+tenant, tenant, 0)
		if err != nil {
			t.Fatalf("start run for %s: %v", tenant, err)
		}
		return runID
	}

	defaultRunID := deployAndStart(tenantDefault)
	runB := deployAndStart(tenantB.String())

	storeB, closerB, err := factory.OpenStore(ctx, tenantB.String(), "default")
	if err != nil {
		t.Fatalf("OpenStore(B): %v", err)
	}
	defer closerB.Close()

	wfBefore, err := storeB.GetWorkflowByID(ctx, runB)
	if err != nil {
		t.Fatalf("tenant B reading back its own run before signalling: %v", err)
	}
	if wfBefore == nil {
		t.Fatal("tenant B's own run is not visible to a store scoped to tenant B")
	}

	// KNOWN-POSITIVE CONTROL: no tenant in ctx at all -- the pass-through
	// case tenantctx.From's other callers in this package use (see
	// dbServiceCaller.resolveSecrets) -- must still deliver to the process
	// store's own (default) tenant. This is the backward-compatible path:
	// a caller with no tenant to give has no wrong tenant to guard against.
	if err := signalPluginWorkflow(ctx, processStore, defaultRunID, "sig", `{"n":1}`); err != nil {
		t.Fatalf("KNOWN-POSITIVE CONTROL FAILED: signalling the default tenant's own run with no "+
			"tenant in ctx: %v", err)
	}

	// THE FIX under test: signalling tenant B's run, through the SAME
	// process-wide store still opened for the default tenant, with ctx
	// carrying tenant B -- exactly how webhookingest and eventtriggers call
	// env.SignalWorkflow (plugin.ForTenant(ctx, source.TenantID) and
	// plugin.ForTenant(context.Background(), tenantID) respectively).
	if err := signalPluginWorkflow(plugin.ForTenant(ctx, tenantB), processStore, runB, "sig", `{"n":2}`); err != nil {
		t.Fatalf("signalPluginWorkflow for tenant B's own run failed: %v -- cleat#2209 unfixed", err)
	}

	wfAfter, err := storeB.GetWorkflowByID(ctx, runB)
	if err != nil {
		t.Fatalf("tenant B reading back its own run after signalling: %v", err)
	}
	if !wfAfter.NextWakeAt.After(wfBefore.NextWakeAt) {
		t.Errorf("next_wake_at did not move forward after signalling tenant B's run (before=%v "+
			"after=%v) -- the write did not reach the actual target row", wfBefore.NextWakeAt, wfAfter.NextWakeAt)
	}
	delivery, found, err := storeB.PollSignal(ctx, runB, "sig")
	if err != nil {
		t.Fatalf("PollSignal(B's run): %v", err)
	}
	if !found {
		t.Fatal("tenant B's own scoped store cannot see the signal delivered to its own run")
	}
	if delivery.Payload != `{"n":2}` {
		t.Errorf("delivered payload = %q, want {\"n\":2}", delivery.Payload)
	}

	// NEGATIVE CONTROL: tenant A, a THIRD tenant with no relationship to B,
	// must not be able to signal into B's run merely by naming its workflow
	// id -- and must fail LOUDLY, not repeat the silent no-op cleat#2209
	// exists to fix.
	err = signalPluginWorkflow(plugin.ForTenant(ctx, tenantA), processStore, runB, "sig-from-a", `{}`)
	if err == nil {
		t.Error("tenant A was able to signal into tenant B's run -- cleat#2209 unfixed, or the " +
			"fail-loud check on zero rows affected regressed")
	}

	// The rejected attempt must not leave a phantom signal row behind:
	// deliverSignalTx's INSERT and its UPDATE run in one transaction, and
	// the UPDATE failing must roll back the INSERT with it.
	//
	// Polled as tenant A, not tenant B: the INSERT above (if the rollback
	// did not happen) is tagged with the CALLER's tenant -- s.tenantID inside
	// deliverSignalTx, which for this negative control is A -- not the
	// target workflow's own tenant B. storeB is scoped to B, so RLS would
	// hide a phantom row tagged A from it regardless of whether the rollback
	// worked, and the assertion would pass either way. storeA is scoped to
	// the tenant the row would actually be tagged with, so it is the only
	// store that can tell "rolled back" apart from "wrote it, then hid it".
	storeA, closerA, err := factory.OpenStore(ctx, tenantA.String(), "default")
	if err != nil {
		t.Fatalf("OpenStore(A): %v", err)
	}
	defer closerA.Close()
	if _, found, err := storeA.PollSignal(ctx, runB, "sig-from-a"); err != nil {
		t.Fatalf("PollSignal(sig-from-a) as tenant A: %v", err)
	} else if found {
		t.Error("a signal tenant A could not deliver still left a row in workflow_signals -- the " +
			"failed delivery was not rolled back atomically")
	}
}

// TestSignalPluginWorkflow_PostgresShardedStoreScopesToTheTargetsTenant is
// the sharded-store half: DeliverSignal routes by workflow ID to one shard
// (ShardedStore.DeliverSignal, sharded_store.go), so the fix here is
// entirely ShardedStore.WithTenant's -- there is no signal-specific
// sharding logic to get wrong, but a store type assertion inside
// scopeToTenant that stopped matching *engine.ShardedStore would silently
// leave every shard's own store scoped to whatever tenant it was opened
// for, which is exactly the defect this test exists to catch.
func TestSignalPluginWorkflow_PostgresShardedStoreScopesToTheTargetsTenant(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed cleat#2209 test")
	}
	db := testutil.SuiteTestDB(t, "cleat_worker")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, db)
	factory := engine.NewPostgresStoreFactory(rlsDB, "public")
	ctx := context.Background()

	tenantDefault := engine.DefaultTenantUUID
	tenantA := uuid.New()
	tenantB := uuid.New()

	// Two shards, both backed by the SAME RLS-scoped connection. ShardedStore
	// routes by a hash of the workflow ID and has no notion of "how many
	// real databases" beyond the WorkflowStore values it is given -- what
	// this test exercises is the WRAPPER's own per-shard rescoping loop
	// (WithTenant iterating every shard, DeliverSignal routing to whichever
	// one a given workflow ID hashes to), which needs more than one shard
	// entry to be exercised at all. A single shard would not catch an
	// off-by-one in that loop.
	shard0, _, err := factory.OpenStore(ctx, tenantDefault, "default")
	if err != nil {
		t.Fatalf("OpenStore(shard0, default): %v", err)
	}
	shard1, _, err := factory.OpenStore(ctx, tenantDefault, "default")
	if err != nil {
		t.Fatalf("OpenStore(shard1, default): %v", err)
	}
	ss, err := engine.NewShardedStore(
		[]engine.ShardConfig{{Name: "shard-0"}, {Name: "shard-1"}},
		[]engine.WorkflowStore{shard0, shard1},
		[]func() error{func() error { return nil }, func() error { return nil }},
	)
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}

	deployAndStart := func(tenant string) string {
		t.Helper()
		st, closer, err := factory.OpenStore(ctx, tenant, "default")
		if err != nil {
			t.Fatalf("OpenStore(%s): %v", tenant, err)
		}
		defer closer.Close()
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: "signal-2209-sharded", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("deploy def for %s: %v", tenant, err)
		}
		// Through ss.WithTenant(tenant), not ss or st directly: ss is opened
		// scoped to the default tenant on every shard (the same way the
		// process-wide store in main() is), and every shard's own session
		// stays that way until something re-scopes it -- writing tenant B's
		// row through the still-default-scoped ss the way the PLAIN test
		// above uses a freshly-opened per-tenant st would hit the exact RLS
		// violation this whole fix exists to prevent, before the test even
		// gets to what it means to check. StartNewRun on a ShardedStore
		// generates its own UUID and routes by it, and that is the id
		// DeliverSignal must route to the same shard by later.
		//
		// "start-sharded-", not "start-": idempotency keys are scoped to
		// (tenant_id, key_hash), not to def name, and this suite shares one
		// database and the same tenantDefault constant across tests -- the
		// plain (non-sharded) test above already spends "start-"+tenantDefault
		// against a different def name, and reusing it here would fail with
		// ErrIdempotencyKeyDefMismatch instead of exercising the start path.
		runID, _, err := ss.WithTenant(tenant).StartNewRun(ctx, "", "signal-2209-sharded", 1, json.RawMessage(`{}`),
			"start-sharded-"+tenant, tenant, 0)
		if err != nil {
			t.Fatalf("start run for %s via ShardedStore: %v", tenant, err)
		}
		return runID
	}

	defaultRunID := deployAndStart(tenantDefault)
	runB := deployAndStart(tenantB.String())

	storeB, closerB, err := factory.OpenStore(ctx, tenantB.String(), "default")
	if err != nil {
		t.Fatalf("OpenStore(B): %v", err)
	}
	defer closerB.Close()

	wfBefore, err := storeB.GetWorkflowByID(ctx, runB)
	if err != nil {
		t.Fatalf("tenant B reading back its own run before signalling: %v", err)
	}
	if wfBefore == nil {
		t.Fatal("tenant B's own run is not visible to a store scoped to tenant B")
	}

	if err := signalPluginWorkflow(ctx, ss, defaultRunID, "sig", `{"n":1}`); err != nil {
		t.Fatalf("KNOWN-POSITIVE CONTROL FAILED: signalling the default tenant's own run on a "+
			"sharded store with no tenant in ctx: %v", err)
	}

	if err := signalPluginWorkflow(plugin.ForTenant(ctx, tenantB), ss, runB, "sig", `{"n":2}`); err != nil {
		t.Fatalf("signalPluginWorkflow for tenant B's own run on a sharded store failed: %v -- "+
			"cleat#2209 unfixed for ShardedStore", err)
	}

	wfAfter, err := storeB.GetWorkflowByID(ctx, runB)
	if err != nil {
		t.Fatalf("tenant B reading back its own run after signalling: %v", err)
	}
	if !wfAfter.NextWakeAt.After(wfBefore.NextWakeAt) {
		t.Errorf("next_wake_at did not move forward after signalling tenant B's run on a sharded "+
			"store (before=%v after=%v)", wfBefore.NextWakeAt, wfAfter.NextWakeAt)
	}
	if _, found, err := storeB.PollSignal(ctx, runB, "sig"); err != nil {
		t.Fatalf("PollSignal(B's run): %v", err)
	} else if !found {
		t.Fatal("tenant B's own scoped store cannot see the signal delivered to its own run")
	}

	err = signalPluginWorkflow(plugin.ForTenant(ctx, tenantA), ss, runB, "sig-from-a", `{}`)
	if err == nil {
		t.Error("tenant A was able to signal into tenant B's run on a sharded store -- cleat#2209 " +
			"unfixed for ShardedStore, or the fail-loud check regressed")
	}
}

// TestSignalPluginWorkflow_MSSQLScopesToTheTargetsTenant is the SQL Server
// half. Unlike the cleat#2187 MSSQL test, no temporary BLOCK PREDICATE is
// needed here: the statement this fix protects is an UPDATE
// (workflow_instances, inside deliverSignalTx), and SQL Server's shipped
// FILTER predicate already gates UPDATE -- it is only INSERT that a FILTER
// predicate lets through unchecked (cleat#2187's finding). So the real
// shipped migrations, unmodified, are enough to prove a mismatched
// SESSION_CONTEXT matches zero rows and that the fix now reports that
// loudly instead of the previous silent success.
func TestSignalPluginWorkflow_MSSQLScopesToTheTargetsTenant(t *testing.T) {
	base := os.Getenv("CLEAT_TEST_MSSQL")
	if base == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping database-backed cleat#2209 test")
	}
	adminDB, err := sql.Open("sqlserver", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer adminDB.Close()
	if err := adminDB.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MSSQL is set but SQL Server is unreachable at %s: %v",
			redactMSSQLDSN(base), err)
	}

	const dbName = "cleat_signal_2209_test"
	ctx := context.Background()
	if _, err := adminDB.ExecContext(ctx,
		fmt.Sprintf("IF DB_ID('%s') IS NOT NULL BEGIN ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s; END",
			dbName, dbName, dbName)); err != nil {
		t.Fatalf("drop stale test database: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(),
			fmt.Sprintf("ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s",
				dbName, dbName))
	})

	connStr, err := mssqlConnStrForDB(base, dbName)
	if err != nil {
		t.Fatalf("derive test DSN: %v", err)
	}
	testDB, err := sql.Open("sqlserver", connStr)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer testDB.Close()

	if err := migration.NewRunner(testDB, migration.DialectMSSQL, "../../migrations").Run(ctx); err != nil {
		t.Fatalf("apply mssql migrations: %v", err)
	}

	factory := engine.NewMSSQLStoreFactory(connStr)

	tenantDefault := engine.DefaultTenantUUID
	tenantA := uuid.New()
	tenantB := uuid.New()

	processStore, processCloser, err := factory.OpenStore(ctx, tenantDefault, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer processCloser.Close()

	deployAndStart := func(tenant string) string {
		t.Helper()
		st, closer, err := factory.OpenStore(ctx, tenant, "default")
		if err != nil {
			t.Fatalf("OpenStore(%s): %v", tenant, err)
		}
		defer closer.Close()
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: "signal-2209-mssql", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("deploy def for %s: %v", tenant, err)
		}
		runID, _, err := st.StartNewRun(ctx, "", "signal-2209-mssql", 1, json.RawMessage(`{}`),
			"start-"+tenant, tenant, 0)
		if err != nil {
			t.Fatalf("start run for %s: %v", tenant, err)
		}
		return runID
	}

	defaultRunID := deployAndStart(tenantDefault)
	runB := deployAndStart(tenantB.String())

	storeB, closerB, err := factory.OpenStore(ctx, tenantB.String(), "default")
	if err != nil {
		t.Fatalf("OpenStore(B): %v", err)
	}
	defer closerB.Close()

	wfBefore, err := storeB.GetWorkflowByID(ctx, runB)
	if err != nil {
		t.Fatalf("tenant B reading back its own run before signalling: %v", err)
	}
	if wfBefore == nil {
		t.Fatal("tenant B's own run is not visible to a store scoped to tenant B")
	}

	if err := signalPluginWorkflow(ctx, processStore, defaultRunID, "sig", `{"n":1}`); err != nil {
		t.Fatalf("KNOWN-POSITIVE CONTROL FAILED: signalling the default tenant's own run with no "+
			"tenant in ctx: %v", err)
	}

	if err := signalPluginWorkflow(plugin.ForTenant(ctx, tenantB), processStore, runB, "sig", `{"n":2}`); err != nil {
		t.Fatalf("signalPluginWorkflow for tenant B's own run failed: %v -- cleat#2209 unfixed on MSSQL", err)
	}

	wfAfter, err := storeB.GetWorkflowByID(ctx, runB)
	if err != nil {
		t.Fatalf("tenant B reading back its own run after signalling: %v", err)
	}
	if !wfAfter.NextWakeAt.After(wfBefore.NextWakeAt) {
		t.Errorf("next_wake_at did not move forward after signalling tenant B's run (before=%v "+
			"after=%v) -- the write did not reach the actual target row", wfBefore.NextWakeAt, wfAfter.NextWakeAt)
	}
	delivery, found, err := storeB.PollSignal(ctx, runB, "sig")
	if err != nil {
		t.Fatalf("PollSignal(B's run): %v", err)
	}
	if !found {
		t.Fatal("tenant B's own scoped store cannot see the signal delivered to its own run")
	}
	if delivery.Payload != `{"n":2}` {
		t.Errorf("delivered payload = %q, want {\"n\":2}", delivery.Payload)
	}

	err = signalPluginWorkflow(plugin.ForTenant(ctx, tenantA), processStore, runB, "sig-from-a", `{}`)
	if err == nil {
		t.Error("tenant A was able to signal into tenant B's run on MSSQL -- cleat#2209 unfixed, " +
			"or the fail-loud check on zero rows affected regressed")
	}
	// Polled as tenant A, not B -- see the identical comment in the
	// PostgreSQL test above. A phantom row here would carry s.tenantID from
	// deliverSignalTx, i.e. A, and a store scoped to B cannot see it.
	storeA, closerA, err := factory.OpenStore(ctx, tenantA.String(), "default")
	if err != nil {
		t.Fatalf("OpenStore(A): %v", err)
	}
	defer closerA.Close()
	if _, found, err := storeA.PollSignal(ctx, runB, "sig-from-a"); err != nil {
		t.Fatalf("PollSignal(sig-from-a) as tenant A: %v", err)
	} else if found {
		t.Error("a signal tenant A could not deliver still left a row in workflow_signals -- the " +
			"failed delivery was not rolled back atomically")
	}

	// GetAllowedSignalCallers half of cleat#2209: on a store re-scoped to
	// tenant B, this must read B's own row, not whatever the underlying
	// connection's pool happened to have baked in at connect time
	// (mssql_signals_promises.go, same shape as GetWorkflowByID/ListVersions
	// before cleat#2187 fixed those two).
	if err := storeB.SetAllowedSignalCallers(ctx, runB, []string{"caller-b"}); err != nil {
		t.Fatalf("SetAllowedSignalCallers(B): %v", err)
	}
	scopedToB := scopeToTenant(processStore, tenantB.String())
	callers, err := scopedToB.GetAllowedSignalCallers(ctx, runB)
	if err != nil {
		t.Fatalf("GetAllowedSignalCallers via a store scoped to B: %v", err)
	}
	if len(callers) != 1 || callers[0] != "caller-b" {
		t.Errorf("GetAllowedSignalCallers via a store scoped to B = %v, want [caller-b] -- read the "+
			"wrong tenant's row", callers)
	}
}

// TestSignalPluginWorkflowWithAuth_OnlyAnAllowedCallerCanSignal exercises
// signalPluginWorkflowWithAuth itself, not signalPluginWorkflow.
//
// Every other test in this file calls signalPluginWorkflow -- the
// --require-signal-auth wrapper installed in main() calls
// signalPluginWorkflowWithAuth instead, and nothing above goes through it.
// A signalPluginWorkflowWithAuth that forgot to scope its own store (using
// `store` where it should use `scoped`) would stay green against every test
// above, because none of them exercise this function.
//
// The failure mode an unscoped helper produces here is not "wrong tenant's
// row" the way the plain-signal tests catch it -- it is a LEGITIMATE,
// allowed caller getting denied: GetAllowedSignalCallers on the wrong
// tenant's scope reads no row for runB, PostgresStore.GetAllowedSignalCallers
// treats that as sql.ErrNoRows and returns an empty list with no error (see
// its doc comment in engine/store_signals.go), and signalCallerAllowed(nil,
// anything) is always false. So it fails CLOSED, silently, which is worse
// than failing open: nothing crashes, nothing logs "denied wrongly", a
// legitimate webhook caller just stops being able to signal.
func TestSignalPluginWorkflowWithAuth_OnlyAnAllowedCallerCanSignal(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed cleat#2209 test")
	}
	db := testutil.SuiteTestDB(t, "cleat_worker")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, db)
	factory := engine.NewPostgresStoreFactory(rlsDB, "public")
	ctx := context.Background()

	tenantDefault := engine.DefaultTenantUUID
	tenantB := uuid.New()

	// The process-wide store, opened for the default tenant -- exactly what
	// main()'s --require-signal-auth closure closes over, same as the other
	// tests in this file.
	processStore, processCloser, err := factory.OpenStore(ctx, tenantDefault, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer processCloser.Close()

	storeB, closerB, err := factory.OpenStore(ctx, tenantB.String(), "default")
	if err != nil {
		t.Fatalf("OpenStore(B): %v", err)
	}
	defer closerB.Close()

	if err := storeB.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: "signal-2209-auth", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy def: %v", err)
	}
	runB, _, err := storeB.StartNewRun(ctx, "", "signal-2209-auth", 1, json.RawMessage(`{}`),
		"start-auth-"+tenantB.String(), tenantB.String(), 0)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if err := storeB.SetAllowedSignalCallers(ctx, runB, []string{"good-plugin"}); err != nil {
		t.Fatalf("SetAllowedSignalCallers: %v", err)
	}

	authCtx := plugin.ForTenant(ctx, tenantB)

	// THE FIX under test: through the SAME process-wide store still opened
	// for the default tenant, with ctx carrying tenant B -- exactly how
	// main()'s --require-signal-auth closure calls
	// signalPluginWorkflowWithAuth. An allowed caller naming B's own run
	// must succeed.
	if err := signalPluginWorkflowWithAuth(authCtx, processStore, runB, "sig-allowed", `{"ok":true}`,
		"good-plugin"); err != nil {
		t.Fatalf("signalPluginWorkflowWithAuth for the allowed caller failed: %v -- the helper is "+
			"not scoping to tenant B, or the auth check regressed", err)
	}
	if _, found, err := storeB.PollSignal(ctx, runB, "sig-allowed"); err != nil {
		t.Fatalf("PollSignal(sig-allowed): %v", err)
	} else if !found {
		t.Error("the allowed caller's signal did not reach tenant B's run")
	}

	// A caller NOT in allowed_signals must be denied, even naming the same
	// (correctly-scoped) workflow the allowed caller just succeeded against
	// -- so a scoping bug that denied everyone cannot be mistaken for this
	// case passing.
	err = signalPluginWorkflowWithAuth(authCtx, processStore, runB, "sig-denied", `{}`, "other-plugin")
	if err == nil {
		t.Error("signalPluginWorkflowWithAuth allowed a caller not in allowed_signals")
	}
	if _, found, err := storeB.PollSignal(ctx, runB, "sig-denied"); err != nil {
		t.Fatalf("PollSignal(sig-denied): %v", err)
	} else if found {
		t.Error("a denied caller's signal still reached tenant B's run")
	}
}
