package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
	"github.com/google/uuid"
)

// cleat#3442. --claim-across-tenants (default ON) runs claimGeneral, which tries
// claimRotating first: list tenants from admin.tenants, then
// storeForTenant(t).ClaimWorkflows for each, under that tenant's own RLS
// context. Its pieces have real-database tests and its LOGIC has mock-store
// tests (the_rotating_claim_shares_the_batch_test.go). Nothing ran it end to end
// against a real multi-tenant database with a real per-tenant factory, so a
// defect in how the pieces fit -- the wrong store reaching a tenant, a claim
// leaving its tenant, a suspended tenant served -- was invisible to every test.
//
// What this runs: a Worker built the way main builds one (a store opened for the
// default tenant, the dialect's real StoreFactory, claimAcrossTenants on),
// driven through claimGeneral, which is the call the dispatch loop makes.
//
// What it does NOT run: a workflow's WASM. "Executed under its own tenant" is
// covered as far as the worker's own routing goes -- every claimed run is
// returned by the store opened for that run's tenant, and the same tenant-scoped
// store reads it back -- not by running a guest.
//
// THE THREE ASSERTIONS, and each has a control built into this file, because a
// clean result from a probe that cannot see the bug is the failure this
// repository keeps recording:
//
//  1. Every live tenant's work is claimed, each run exactly once.
//     Control: a factory that ignores the tenant it was asked for and always
//     opens the default tenant's store. Claims then never reach A or B.
//  2. A suspended tenant's work is not claimed.
//     Control: the same scenario with the suspended tenant not marked
//     suspended, which must claim its work (otherwise "not claimed" proves
//     nothing about suspension).
//  3. No tenant's run is claimed in another tenant's context.
//     Control (PostgreSQL): the factory opened over a connection that bypasses
//     RLS. Every tenant's store can then see every row, and the first claim
//     takes other tenants' runs.
//
// Falsified outside the test as well, by breaking the production code -- see the
// pull request.

// rotationRig is one dialect's multi-tenant database and the factories over it.
type rotationRig struct {
	dialect string
	// factory is production-shaped: per-tenant stores that enforce isolation.
	factory engine.StoreFactory
	// bypassFactory opens the same database over a connection that does NOT
	// enforce isolation, or is nil where no such connection can be made. It
	// exists for the third assertion's control only.
	bypassFactory engine.StoreFactory
	// register writes a tenant into admin.tenants, which is what the rotation
	// enumerates. The default tenant is seeded by the migrations.
	register func(t *testing.T, id, name string, suspended bool)
	// reset empties the run tables of this rig's own database.
	reset func(t *testing.T)
}

// claimRecord is one run returned by a ClaimWorkflows call, with the tenant the
// store that returned it was opened for.
type claimRecord struct {
	storeTenant string
	runID       string
	runTenant   string
}

// claimRecorder collects every claim made through the stores it wraps.
type claimRecorder struct {
	mu     sync.Mutex
	claims []claimRecord
}

func (r *claimRecorder) add(c claimRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.claims = append(r.claims, c)
}

// recordingStore forwards to a real store and records what ClaimWorkflows
// returns. ListTenantIDs is forwarded explicitly so the worker's TenantLister
// assertion sees it; that the UNWRAPPED store satisfies the interface is asserted
// separately in the scenario, because the wrapper would otherwise hide a
// regression there.
type recordingStore struct {
	engine.WorkflowStore
	tenant string
	rec    *claimRecorder
}

func (s *recordingStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
	wfs, err := s.WorkflowStore.ClaimWorkflows(ctx, workerID, limit)
	for _, wf := range wfs {
		s.rec.add(claimRecord{storeTenant: s.tenant, runID: wf.ID, runTenant: wf.TenantID})
	}
	return wfs, err
}

func (s *recordingStore) ListTenantIDs(ctx context.Context) ([]string, error) {
	lister, ok := s.WorkflowStore.(engine.TenantLister)
	if !ok {
		return nil, fmt.Errorf("%T is not an engine.TenantLister", s.WorkflowStore)
	}
	return lister.ListTenantIDs(ctx)
}

// rotationRecordingFactory opens real stores through inner and wraps each so its claims
// are recorded against the tenant it was opened for.
type rotationRecordingFactory struct {
	engine.StoreFactory
	rec *claimRecorder
	// misroute, when set, opens the DEFAULT tenant's store whatever tenant was
	// asked for, while still labelling the store with the tenant asked for.
	// That is the defect storeForTenant exists to prevent, built on purpose as
	// assertion 1's control.
	misroute bool
}

func (f *rotationRecordingFactory) OpenStore(ctx context.Context, tenantID string, taskQueues ...string) (engine.WorkflowStore, io.Closer, error) {
	open := tenantID
	if f.misroute {
		open = engine.DefaultTenantUUID
	}
	st, closer, err := f.StoreFactory.OpenStore(ctx, open, taskQueues...)
	if err != nil {
		return nil, nil, err
	}
	return &recordingStore{WorkflowStore: st, tenant: tenantID, rec: f.rec}, closer, nil
}

// rotationScenario is what one run of the scenario observed.
type rotationScenario struct {
	live       map[string]string // run id -> tenant, for tenants that are not suspended
	held       map[string]string // run id -> tenant, for the suspended tenant
	claimCount map[string]int    // run id -> times returned by any claim
	records    []claimRecord
}

const rotationRunsPerTenant = 2

// runRotationScenario seeds the default tenant, two live tenants and one tenant
// that is suspended (unless holdSuspension is false), each with runnable work,
// then drives claimGeneral until nothing more is claimed.
//
// Seeding goes through rig.factory, the production-shaped one, whatever
// claimFactory is: the control that breaks routing must break the CLAIM, not the
// fixture.
func runRotationScenario(t *testing.T, rig *rotationRig, claimFactory engine.StoreFactory, rec *claimRecorder, holdSuspension bool) *rotationScenario {
	t.Helper()
	ctx := context.Background()
	rig.reset(t)

	tenantA, tenantB, tenantS := uuid.NewString(), uuid.NewString(), uuid.NewString()
	rig.register(t, tenantA, "rotation-a-"+tenantA[:8], false)
	rig.register(t, tenantB, "rotation-b-"+tenantB[:8], false)
	rig.register(t, tenantS, "rotation-s-"+tenantS[:8], holdSuspension)

	sc := &rotationScenario{
		live:       map[string]string{},
		held:       map[string]string{},
		claimCount: map[string]int{},
	}
	const defName = "rotation-e2e-3442"
	for _, tenant := range []string{engine.DefaultTenantUUID, tenantA, tenantB, tenantS} {
		st, closer, err := rig.factory.OpenStore(ctx, tenant, "default")
		if err != nil {
			t.Fatalf("seeding: OpenStore(%s): %v", tenant, err)
		}
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("seeding: deploy def for %s: %v", tenant, err)
		}
		for i := 0; i < rotationRunsPerTenant; i++ {
			id := uuid.NewString()
			if _, _, err := st.StartNewRun(ctx, id, defName, 1, json.RawMessage(`{}`),
				"rotation-"+id, tenant, 0); err != nil {
				t.Fatalf("seeding: StartNewRun for %s: %v", tenant, err)
			}
			if tenant == tenantS {
				sc.held[id] = tenant
			} else {
				sc.live[id] = tenant
			}
		}
		_ = closer.Close()
	}

	// The worker, built the way main builds one: a store opened for the DEFAULT
	// tenant by the real factory, that same factory as storeFactory, the flag on.
	processStore, processCloser, err := rig.factory.OpenStore(ctx, engine.DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore(default) for the worker's own store: %v", err)
	}
	defer processCloser.Close()
	if _, ok := processStore.(engine.TenantLister); !ok {
		t.Fatalf("%s: the store the worker holds, %T, is not an engine.TenantLister, so the "+
			"rotation is unavailable and this test would be measuring the fallback", rig.dialect, processStore)
	}

	w := newTestWorkerFromStore(&recordingStore{WorkflowStore: processStore, tenant: engine.DefaultTenantUUID, rec: rec})
	w.id = "rotation-e2e-worker"
	w.storeFactory = claimFactory
	w.storeTenantID = engine.DefaultTenantUUID
	w.taskQueues = []string{"default"}
	w.claimAcrossTenants = true
	w.claimTenantsPerTick = defaultClaimTenantsPerTick

	// A batch smaller than the tenant count, so the per-tenant share and the
	// cursor both matter, and several ticks are needed to drain everything.
	const batch = 4
	idle := 0
	for tick := 0; tick < 24 && idle < 3; tick++ {
		wfs, err := w.claimGeneral(batch)
		if err != nil {
			t.Fatalf("claimGeneral on tick %d: %v", tick, err)
		}
		if len(wfs) == 0 {
			idle++
			continue
		}
		idle = 0
		for _, wf := range wfs {
			sc.claimCount[wf.ID]++
		}
	}
	rec.mu.Lock()
	sc.records = append([]claimRecord(nil), rec.claims...)
	rec.mu.Unlock()
	liveClaimed, heldClaimed := 0, 0
	for id := range sc.live {
		liveClaimed += sc.claimCount[id]
	}
	for id := range sc.held {
		heldClaimed += sc.claimCount[id]
	}
	t.Logf("%s scenario: %d live run(s) seeded, %d claim(s) of them; %d suspended-tenant run(s) seeded, %d claim(s) of them; %d claim(s) recorded in total",
		rig.dialect, len(sc.live), liveClaimed, len(sc.held), heldClaimed, len(sc.records))
	return sc
}

// violations lists every way sc departs from the three assertions, as
// sentences. Empty means the rotation did what it is for.
func (sc *rotationScenario) violations() []string {
	var out []string

	// 1. Every live tenant's work, each run exactly once.
	var missing, repeated []string
	for id, tenant := range sc.live {
		switch n := sc.claimCount[id]; {
		case n == 0:
			missing = append(missing, tenant[:8]+"/"+id[:8])
		case n > 1:
			repeated = append(repeated, fmt.Sprintf("%s/%s x%d", tenant[:8], id[:8], n))
		}
	}
	sort.Strings(missing)
	sort.Strings(repeated)
	if len(missing) > 0 {
		out = append(out, fmt.Sprintf("1: %d live run(s) were never claimed: %v", len(missing), missing))
	}
	if len(repeated) > 0 {
		out = append(out, fmt.Sprintf("1: run(s) claimed more than once: %v", repeated))
	}

	// 2. A suspended tenant's work is not claimed.
	var served []string
	for id, tenant := range sc.held {
		if sc.claimCount[id] > 0 {
			served = append(served, tenant[:8]+"/"+id[:8])
		}
	}
	sort.Strings(served)
	if len(served) > 0 {
		out = append(out, fmt.Sprintf("2: the suspended tenant's run(s) were claimed: %v", served))
	}

	// 3. No claim leaves its tenant.
	var crossed []string
	for _, c := range sc.records {
		if c.runTenant == "" {
			out = append(out, fmt.Sprintf("3: run %s came back with no tenant, so this assertion cannot be made", c.runID))
			continue
		}
		// EqualFold: a tenant id is a UUID, and SQL Server prints a
		// uniqueidentifier in upper case, so the ids ListTenantIDs hands the
		// rotation there are upper case while the rows carry the lower case they
		// were inserted with. Found by this test's first SQL Server run, which
		// reported four "crossed" claims that were all the same tenant.
		if !strings.EqualFold(c.storeTenant, c.runTenant) {
			crossed = append(crossed, fmt.Sprintf("run %s of tenant %s returned by the store for %s",
				c.runID[:8], c.runTenant[:8], c.storeTenant[:8]))
		}
	}
	sort.Strings(crossed)
	if len(crossed) > 0 {
		out = append(out, fmt.Sprintf("3: %d claim(s) crossed tenants: %v", len(crossed), crossed))
	}
	return out
}

// hasPrefix reports whether any violation starts with the given assertion number.
func hasViolation(vs []string, n string) bool {
	for _, v := range vs {
		if strings.HasPrefix(v, n+":") {
			return true
		}
	}
	return false
}

// assertRotationServesEveryTenant is the shared body for the dialects that can
// rotate.
func assertRotationServesEveryTenant(t *testing.T, rig *rotationRig) {
	t.Helper()

	t.Run("production-shaped factory satisfies all three", func(t *testing.T) {
		rec := &claimRecorder{}
		f := &rotationRecordingFactory{StoreFactory: rig.factory, rec: rec}
		sc := runRotationScenario(t, rig, f, rec, true)
		if vs := sc.violations(); len(vs) > 0 {
			t.Errorf("%s: the rotating claim did not serve the tenants correctly:\n  %s",
				rig.dialect, strings.Join(vs, "\n  "))
		}
		// The positive evidence the assertions above are about something: the
		// live tenants really had work, and it was really claimed.
		if len(sc.live) != 3*rotationRunsPerTenant {
			t.Fatalf("seeded %d live runs, want %d", len(sc.live), 3*rotationRunsPerTenant)
		}
		claimed := 0
		for id := range sc.live {
			if sc.claimCount[id] == 1 {
				claimed++
			}
		}
		if claimed != len(sc.live) {
			t.Errorf("claimed %d of %d live runs exactly once", claimed, len(sc.live))
		}
	})

	// CONTROL for assertion 1. Without it, "all claimed" could be satisfied by a
	// claim that sweeps everything regardless of tenant.
	t.Run("control: a factory that ignores the tenant is reported", func(t *testing.T) {
		rec := &claimRecorder{}
		f := &rotationRecordingFactory{StoreFactory: rig.factory, rec: rec, misroute: true}
		sc := runRotationScenario(t, rig, f, rec, true)
		if !hasViolation(sc.violations(), "1") {
			t.Errorf("%s: with every tenant routed to the default tenant's store, work for the "+
				"other tenants should be left unclaimed and the scenario should say so; it reported %v. "+
				"The first subtest's green therefore proves nothing about routing.", rig.dialect, sc.violations())
		}
	})

	// CONTROL for assertion 2. If a tenant that is NOT suspended is claimed,
	// then "the suspended tenant was not claimed" is about suspension and not
	// about the tenant simply having no work.
	t.Run("control: the same tenant, not suspended, is served", func(t *testing.T) {
		rec := &claimRecorder{}
		f := &rotationRecordingFactory{StoreFactory: rig.factory, rec: rec}
		sc := runRotationScenario(t, rig, f, rec, false)
		served := 0
		for id := range sc.held {
			if sc.claimCount[id] > 0 {
				served++
			}
		}
		if served != len(sc.held) || served == 0 {
			t.Errorf("%s: the tenant under test, left unsuspended, had %d of %d run(s) claimed; "+
				"the suspension assertion cannot be told apart from an empty tenant", rig.dialect, served, len(sc.held))
		}
	})

	// CONTROL for assertion 3, where a connection that bypasses isolation can be
	// made. With it, every tenant's store sees every row.
	if rig.bypassFactory != nil {
		t.Run("control: a connection that bypasses isolation is reported", func(t *testing.T) {
			rec := &claimRecorder{}
			f := &rotationRecordingFactory{StoreFactory: rig.bypassFactory, rec: rec}
			sc := runRotationScenario(t, rig, f, rec, true)
			if !hasViolation(sc.violations(), "3") {
				t.Errorf("%s: over a connection that bypasses RLS a tenant's store should be able to claim "+
					"other tenants' runs and the scenario should report it; it reported %v. "+
					"The first subtest's green therefore proves nothing about context.", rig.dialect, sc.violations())
			}
		})
	}
}

// ---- PostgreSQL ----

// newPostgresRotationRig is this file's only PostgreSQL skip site.
func newPostgresRotationRig(t *testing.T) *rotationRig {
	t.Helper()
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed cleat#3442 test")
	}
	owner := testutil.SuiteTestDB(t, "cleat_worker_rotation")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	// PRECONDITION, so that "needs no grant" is measured and not assumed: the
	// role the rotation runs as neither bypasses row-level security nor is a
	// superuser, and holds no EXECUTE on the retired admin.claim_workflows (this
	// test never grants it). If the role could bypass RLS the first assertion
	// would pass for a reason that has nothing to do with the rotation.
	var bypass, super bool
	if err := rlsDB.QueryRow(
		`SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if bypass || super {
		t.Fatalf("the role the rotation runs as has BYPASSRLS=%v SUPERUSER=%v; this test would measure nothing", bypass, super)
	}
	var canExecute bool
	if err := rlsDB.QueryRow(
		`SELECT has_function_privilege(current_user, 'admin.claim_workflows(text, text[], integer)', 'EXECUTE')`).Scan(&canExecute); err != nil {
		// Expected once cleat#3245's B5b deletes the retired function: there is
		// then nothing to hold EXECUTE on. Logged, not skipped silently.
		t.Logf("not checking EXECUTE on admin.claim_workflows (%v)", err)
	} else if canExecute {
		t.Fatalf("the test role can EXECUTE admin.claim_workflows, so it cannot show the rotation needs no such grant")
	}
	return &rotationRig{
		dialect: "postgres",
		// The RLS role is an ordinary non-owning role that PostgreSQL always
		// subjects to row-level security; a superuser would bypass it and the
		// whole test would measure nothing.
		factory:       engine.NewPostgresStoreFactory(rlsDB, "public"),
		bypassFactory: engine.NewPostgresStoreFactory(owner, "public"),
		register: func(t *testing.T, id, name string, suspended bool) {
			t.Helper()
			if _, err := owner.Exec(
				`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, $2, $3)
				 ON CONFLICT (tenant_id) DO UPDATE SET suspended = EXCLUDED.suspended`,
				id, name, suspended); err != nil {
				t.Fatalf("registering tenant %s: %v", name, err)
			}
			t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, id) })
		},
		reset: func(t *testing.T) {
			t.Helper()
			// This database belongs to this test alone (its own suite name), so
			// emptying the run table is safe here in a way it is not in a
			// shared one.
			if _, err := owner.Exec(`DELETE FROM workflow_instances`); err != nil {
				t.Fatalf("emptying workflow_instances: %v", err)
			}
		},
	}
}

func TestARotatingClaimServesEveryTenantOnPostgres(t *testing.T) {
	assertRotationServesEveryTenant(t, newPostgresRotationRig(t))
}

// ---- SQL Server ----

// newMSSQLRotationRig is this file's only SQL Server skip site.
func newMSSQLRotationRig(t *testing.T) *rotationRig {
	t.Helper()
	base := os.Getenv("CLEAT_TEST_MSSQL")
	if base == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping database-backed cleat#3442 test")
	}
	adminDB, err := sql.Open("sqlserver", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	if err := adminDB.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MSSQL is set but SQL Server is unreachable at %s: %v", redactMSSQLDSN(base), err)
	}

	const dbName = "cleat_rotation_3442_test"
	ctx := context.Background()
	if _, err := adminDB.ExecContext(ctx, fmt.Sprintf(
		"IF DB_ID('%s') IS NOT NULL BEGIN ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s; END",
		dbName, dbName, dbName)); err != nil {
		t.Fatalf("drop stale test database: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), fmt.Sprintf(
			"ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s", dbName, dbName))
	})

	connStr, err := mssqlConnStrForDB(base, dbName)
	if err != nil {
		t.Fatalf("derive test DSN: %v", err)
	}
	db, err := sql.Open("sqlserver", connStr)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// The REAL shipped migrations, not a hand-rolled schema: the security
	// policies are the thing under test.
	if err := migration.NewRunner(db, migration.DialectMSSQL, "../../migrations").Run(ctx); err != nil {
		t.Fatalf("apply mssql migrations: %v", err)
	}

	// PRECONDITION, so that "needs no grant" is measured and not assumed. The
	// assertion that carries the weight is the SECOND one below: the shipped
	// dbo.fn_tenant_filter carries no cleat_admin admission. The optional
	// cross_tenant_claim.sql is never applied by the Runner (it sits under
	// migrations/mssql/optional), but this checks the result rather than the
	// directory layout.
	//
	// THE FIRST ONE IS VACUOUS FOR THE DEFAULT DSN. Under sa the principal is
	// dbo, and dbo cannot be made a role member (ALTER ROLE cleat_admin ADD
	// MEMBER dbo fails with Msg 15405 "Cannot use the special principal 'dbo'",
	// measured), so IS_ROLEMEMBER returns 0 whatever the setup. It is kept
	// because a CLEAT_TEST_MSSQL naming any other login makes it meaningful.
	// That sa still does not bypass the policy is shown by the mis-routing
	// control below, not by this check. Pointed out by cleat-review-69 on #3451.
	var member sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT IS_ROLEMEMBER(N'cleat_admin')`).Scan(&member); err != nil {
		t.Fatalf("reading cleat_admin membership: %v", err)
	}
	if !member.Valid || member.Int64 != 0 {
		t.Fatalf("IS_ROLEMEMBER('cleat_admin') = %v for this principal; want 0 (vacuous under sa/dbo, meaningful for any other login)", member)
	}
	var predicate string
	if err := db.QueryRowContext(ctx,
		`SELECT definition FROM sys.sql_modules WHERE object_id = OBJECT_ID(N'dbo.fn_tenant_filter')`).Scan(&predicate); err != nil {
		t.Fatalf("reading dbo.fn_tenant_filter: %v", err)
	}
	if strings.Contains(strings.ToLower(predicate), "cleat_admin") {
		t.Fatalf("dbo.fn_tenant_filter admits cleat_admin, so the optional cross-tenant migration is in effect and "+
			"this test would not be measuring the shipped predicate:\n%s", predicate)
	}

	return &rotationRig{
		dialect: "mssql",
		factory: engine.NewMSSQLStoreFactory(connStr),
		// No bypassing connection is built for SQL Server: its isolation is a
		// session-context predicate, and admitting a principal across tenants
		// is the optional migration this mechanism no longer needs. Assertion 3
		// has no control here; the PostgreSQL arm carries it.
		register: func(t *testing.T, id, name string, suspended bool) {
			t.Helper()
			if _, err := db.ExecContext(ctx,
				`MERGE admin.tenants AS t USING (SELECT @p1 AS tenant_id, @p2 AS name, @p3 AS suspended) AS s
				   ON t.tenant_id = s.tenant_id
				 WHEN MATCHED THEN UPDATE SET suspended = s.suspended
				 WHEN NOT MATCHED THEN INSERT (tenant_id, name, suspended) VALUES (s.tenant_id, s.name, s.suspended);`,
				id, name, suspended); err != nil {
				t.Fatalf("registering tenant %s: %v", name, err)
			}
		},
		reset: func(t *testing.T) {
			t.Helper()
			// A database created for this test alone and dropped after it.
			if _, err := db.ExecContext(ctx, `DELETE FROM dbo.workflow_instances`); err != nil {
				t.Logf("emptying dbo.workflow_instances (tolerated on a fresh database): %v", err)
			}
		},
	}
}

func TestARotatingClaimServesEveryTenantOnSQLServer(t *testing.T) {
	assertRotationServesEveryTenant(t, newMSSQLRotationRig(t))
}

// ---- MySQL ----

// newMySQLRotationRig is this file's only MySQL skip site. It returns the real
// factory and the migrated default tenant's database; no other tenant has a
// database, as in a real deployment (only the default tenant is ever migrated).
func newMySQLRotationRig(t *testing.T) (engine.StoreFactory, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping database-backed cleat#3442 test")
	}
	masterDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open master connection: %v", err)
	}
	t.Cleanup(func() { _ = masterDB.Close() })
	if err := masterDB.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MYSQL is set but MySQL is unreachable: %v", err)
	}
	factory := engine.NewMySQLStoreFactory(masterDB, mysqlBaseDSN(dsn))
	defaultDB, err := factory.CreateTenantDatabase(context.Background(), engine.DefaultTenantUUID)
	if err != nil {
		t.Fatalf("create default tenant database: %v", err)
	}
	testutil.SetupMySQLFullSchema(t, defaultDB)
	return factory, defaultDB
}

// TestARotatingClaimOnMySQLFallsBackToTheWorkersOwnTenant pins what MySQL does,
// which is NOT rotation. MySQLStore is not an engine.TenantLister, so
// claimRotating reports itself unavailable and claimGeneral claims the worker's
// own tenant, warning once. This is the behaviour tiers.yaml records for MySQL
// (multi-tenancy-mysql is NOT SUPPORTED), and a change that made MySQL rotate
// would have to change this test and that entry together.
//
// It cannot show that MySQL rotates, so it supports no widening of the
// cross-tenant-claim claim.
func TestARotatingClaimOnMySQLFallsBackToTheWorkersOwnTenant(t *testing.T) {
	factory, _ := newMySQLRotationRig(t)
	ctx := context.Background()

	st, closer, err := factory.OpenStore(ctx, engine.DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer closer.Close()
	if _, ok := st.(engine.TenantLister); ok {
		t.Fatalf("%T now satisfies engine.TenantLister: MySQL can rotate, so this test's premise, "+
			"and tiers.yaml's cross-tenant-claim entry, are out of date", st)
	}

	const defName = "rotation-e2e-3442-mysql"
	if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy def: %v", err)
	}
	want := map[string]bool{}
	for i := 0; i < rotationRunsPerTenant; i++ {
		id := uuid.NewString()
		if _, _, err := st.StartNewRun(ctx, id, defName, 1, json.RawMessage(`{}`),
			"rotation-"+id, engine.DefaultTenantUUID, 0); err != nil {
			t.Fatalf("StartNewRun: %v", err)
		}
		want[id] = true
	}

	var logs bytes.Buffer
	w := newTestWorkerFromStore(st)
	w.logger = slog.New(slog.NewTextHandler(&logs, nil))
	w.id = "rotation-e2e-worker-mysql"
	w.storeFactory = factory
	w.storeTenantID = engine.DefaultTenantUUID
	w.taskQueues = []string{"default"}
	w.claimAcrossTenants = true
	w.claimTenantsPerTick = defaultClaimTenantsPerTick

	got := map[string]bool{}
	for tick := 0; tick < 6 && len(got) < len(want); tick++ {
		wfs, err := w.claimGeneral(4)
		if err != nil {
			t.Fatalf("claimGeneral: %v", err)
		}
		for _, wf := range wfs {
			if wf.TenantID != engine.DefaultTenantUUID {
				t.Errorf("claimed run %s of tenant %q on MySQL; the fallback serves the default tenant only",
					wf.ID, wf.TenantID)
			}
			got[wf.ID] = true
		}
	}
	for id := range want {
		if !got[id] {
			t.Errorf("the default tenant's run %s was not claimed by the fallback", id)
		}
	}
	if !strings.Contains(logs.String(), "cannot rotate") {
		t.Errorf("the worker fell back without saying so; the log was:\n%s", logs.String())
	}
}
