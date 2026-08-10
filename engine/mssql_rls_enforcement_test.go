package engine

// The schema half of IMPROVEMENT-PLAN.md 2.71: until now, no MSSQL test in
// the repo could observe tenant isolation working or failing.
//
// engine/testutil/mssql_schema.go hand-writes its CREATE TABLE statements and
// defines none of the seven CREATE SECURITY POLICY statements
// migrations/mssql/001_schema.sql ships. A cleared session context therefore
// had nothing to act on, and the absence of enforcement looked like success --
// the same shape as the PostgreSQL superuser trap in 1.7.
//
// Turning the policies on for the whole MSSQL suite is not the answer, at
// least not as one step: dbo.fn_tenant_filter has no NULL-session bypass
// (`WHERE @tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER)`),
// so every test that builds a store on a plain pool would start seeing zero
// rows. That is a test-suite migration, and engine/testutil/ is another
// workstream's.
//
// This takes the same shape PostgreSQL already uses for the same problem:
// leave the default test schema alone, and give the tests that care about RLS
// a scope where it is genuinely switched on -- the analogue of
// testutil.OpenPostgresRLSTestDB. The policies are read out of the real
// migration rather than restated here, so the predicate under test is the
// shipped one and cannot drift from it.
//
// Needs a real SQL Server (CLEAT_TEST_MSSQL).

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

var (
	mssqlFilterFnRe = regexp.MustCompile(`(?s)CREATE OR ALTER FUNCTION dbo\.fn_tenant_filter.*?;`)
	mssqlPolicyRe   = regexp.MustCompile(`(?s)CREATE SECURITY POLICY (dbo\.\w+)\s+ADD FILTER PREDICATE dbo\.fn_tenant_filter\(tenant_id\) ON dbo\.(\w+)\s+WITH \(STATE = ON\);`)
)

// mssqlPolicyTablesMissingFromTestSchema records the tenant-scoped tables the
// shipped schema carries and engine/testutil's does not. Listed explicitly so
// that a *new* gap fails this test rather than being silently tolerated: the
// point of the exercise is that a difference between the tested schema and the
// shipped schema must be visible (1.9).
//
// It is now empty, and that is the 2.71 schema residual closing: engine/testutil
// builds the MSSQL schema from migrations/mssql/*.sql, so there is nothing for
// the tested schema to be missing. The variable stays rather than the check
// being deleted -- a future divergence should fail here, which is the whole
// reason it was written as a set rather than a tolerance.
var mssqlPolicyTablesMissingFromTestSchema = []string{}

// enableMSSQLTenantPolicies asserts that the real fn_tenant_filter and the real
// security policies are present, and leaves them alone.
//
// It used to install them and drop them again on cleanup, which was right when
// engine/testutil hand-wrote a schema that had none. Now that the test schema
// IS migrations/mssql/*.sql (2.71), the policies are there from the start and
// this function installing its own copies means dropping the schema's -- so
// every MSSQL test that ran after this one was running against a database
// missing the (now eight, see below) policies, which is precisely the state
// this file exists to prevent.
//
// Asserting rather than installing also makes the test stronger: it now fails
// if the shipped schema stops carrying a policy, instead of quietly supplying
// one of its own.
func enableMSSQLTenantPolicies(t *testing.T, db *sql.DB) {
	t.Helper()

	path := filepath.Join("..", "migrations", "mssql", "001_schema.sql")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(data)

	// 031 adds an eighth policy (dbo.workflow_promises, finding S1/S10's
	// verification) in its own file rather than in 001_schema.sql, so it is
	// invisible to the scan above unless read separately. Folding it in here
	// means this function's "applied" set -- and therefore
	// TestMSSQLTenantIsolation_UnderRealSecurityPolicies's assertions -- covers
	// all eight tables the shipped schema actually protects, not the seven
	// 001_schema.sql originally shipped.
	promisesPath := filepath.Join("..", "migrations", "mssql", "031_workflow_promises_security_policy.sql")
	promisesData, err := os.ReadFile(promisesPath)
	if err != nil {
		t.Fatalf("read %s: %v", promisesPath, err)
	}
	src += "\n" + string(promisesData)

	policies := mssqlPolicyRe.FindAllStringSubmatch(src, -1)
	if len(policies) == 0 {
		t.Fatalf("could not find any CREATE SECURITY POLICY in %s or %s", path, promisesPath)
	}
	if fn := mssqlFilterFnRe.FindString(src); fn == "" {
		t.Fatalf("could not find dbo.fn_tenant_filter in %s -- the migration changed shape "+
			"and this test no longer describes the shipped predicate", path)
	}

	existing := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM sys.tables`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan table name: %v", err)
		}
		existing[name] = true
	}
	rows.Close()

	var applied, missing []string
	for _, m := range policies {
		policyName, table := m[1], m[2]
		if !existing[table] {
			missing = append(missing, table)
			continue
		}
		// The schema is expected to have installed it. Enabled, not merely
		// present: a policy with STATE = OFF filters nothing, and this file
		// exists to prove filtering happens.
		var enabled bool
		err := db.QueryRow(`SELECT is_enabled FROM sys.security_policies WHERE name = @p1`,
			strings.TrimPrefix(policyName, "dbo.")).Scan(&enabled)
		if errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s is absent from the test database. engine/testutil builds the MSSQL "+
				"schema from migrations/mssql/*.sql, so the shipped policy should be here "+
				"already -- either the migration stopped creating it, or something dropped "+
				"it (IMPROVEMENT-PLAN 2.71).", policyName)
		}
		if err != nil {
			t.Fatalf("look up %s: %v", policyName, err)
		}
		if !enabled {
			t.Fatalf("%s exists but is disabled, so it filters nothing", policyName)
		}
		applied = append(applied, policyName)
	}

	sort.Strings(missing)
	if strings.Join(missing, ",") != strings.Join(mssqlPolicyTablesMissingFromTestSchema, ",") {
		t.Errorf("tenant-scoped tables absent from the test schema = %v, expected exactly %v -- "+
			"the tested schema and the shipped schema have drifted (IMPROVEMENT-PLAN.md 1.9, 2.71)",
			missing, mssqlPolicyTablesMissingFromTestSchema)
	}
	if len(applied) == 0 {
		t.Fatal("no security policies were found, so this test cannot observe enforcement")
	}
}

// TestMSSQLTenantIsolation_UnderRealSecurityPolicies is the end-to-end proof
// that was missing for 2.71.
//
// The connection-level fix is verified directly by
// TestMSSQLSessionContext_SurvivesConnectionReuse, which reads SESSION_CONTEXT
// back off a pooled connection. That establishes the mechanism but not the
// consequence. This runs the shipped filter predicate against two tenants'
// stores and asserts each sees only its own rows, repeatedly, so the
// connections are recycled underneath -- which is where 2.71 bit.
func TestMSSQLTenantIsolation_UnderRealSecurityPolicies(t *testing.T) {
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}

	ctx := context.Background()
	adminDB := testutil.MSSQLTestDB(t)
	// t.Cleanup rather than defer, and registered before anything else, so
	// that LIFO ordering closes the pool *after* the policy drop registered
	// by enableMSSQLTenantPolicies. A deferred Close runs first and leaves
	// the filter predicates in place on a database the rest of the binary
	// shares.
	t.Cleanup(func() { adminDB.Close() })
	testutil.SetupMSSQLFullSchema(t, adminDB)
	testutil.CleanupMSSQLTestData(t, adminDB)
	t.Cleanup(func() { testutil.CleanupMSSQLTestData(t, adminDB) })

	const (
		tenantA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
		tenantB = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
	)

	// Unique per run: the database is shared by the whole binary and by
	// repeated local runs, and a fixture collision here would look like a
	// tenancy failure.
	run := uuid.New().String()[:8]

	// Seed both tenants' rows before switching the policies on: a FILTER
	// predicate constrains what a session can see, and these inserts go in on
	// the admin connection which has no tenant.
	seed := func(tenant, suffix string) string {
		t.Helper()
		defName := "rls-def-" + run + "-" + suffix
		if _, err := adminDB.ExecContext(ctx, `
			INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
			VALUES (@p1, 1, 0x0061736d, 1, 1, @p2)`, defName, tenant); err != nil {
			t.Fatalf("seed workflow_def for %s: %v", suffix, err)
		}
		wfID := "rls-wf-" + run + "-" + suffix
		if _, err := adminDB.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
			VALUES (@p1, @p2, 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3)`,
			wfID, defName, tenant); err != nil {
			t.Fatalf("seed workflow_instance for %s: %v", suffix, err)
		}
		return wfID
	}
	wfA := seed(tenantA, "a")
	wfB := seed(tenantB, "b")

	enableMSSQLTenantPolicies(t, adminDB)

	factory := NewMSSQLStoreFactory(dsn)
	defer factory.Close()

	// The same pools OpenStore builds on, for the raw cross-tenant checks
	// below.
	poolA, err := factory.getOrCreateTenantPool(ctx, tenantA)
	if err != nil {
		t.Fatalf("getOrCreateTenantPool(A): %v", err)
	}
	poolB, err := factory.getOrCreateTenantPool(ctx, tenantB)
	if err != nil {
		t.Fatalf("getOrCreateTenantPool(B): %v", err)
	}

	storeA, closerA, err := factory.OpenStore(ctx, tenantA)
	if err != nil {
		t.Fatalf("OpenStore(A): %v", err)
	}
	defer closerA.Close()
	storeB, closerB, err := factory.OpenStore(ctx, tenantB)
	if err != nil {
		t.Fatalf("OpenStore(B): %v", err)
	}
	defer closerB.Close()

	// Three rounds, interleaved, so that after the first each read is served
	// by a connection that has been returned to the pool and reset. Before
	// the 2.71 fix even round 0 fails, because getOrCreateTenantPool's ping
	// already recycled the connection.
	for round := 0; round < 3; round++ {
		for _, tc := range []struct {
			name    string
			store   WorkflowStore
			pool    *sql.DB
			ownWF   string
			otherWF string
		}{
			{"A", storeA, poolA, wfA, wfB},
			{"B", storeB, poolB, wfB, wfA},
		} {
			own, err := tc.store.GetWorkflowByID(ctx, tc.ownWF)
			if err != nil {
				t.Fatalf("round %d: tenant %s reading its own workflow: %v -- "+
					"under the shipped filter predicate a session with no tenant context matches no rows",
					round, tc.name, err)
			}
			if own == nil || own.ID != tc.ownWF {
				t.Fatalf("round %d: tenant %s got %v for its own workflow, want %s",
					round, tc.name, own, tc.ownWF)
			}

			// The cross-tenant direction must not go through the store.
			// MSSQLStore's own SQL carries `tenant_id = @p`, so
			// GetWorkflowByID returns nothing for another tenant's workflow
			// whether or not RLS is doing anything -- asserting on it would
			// pass against a wide-open filter predicate. Checked directly:
			// this statement has no Go-level tenant filter, so the security
			// policy is the only thing that can hide the row.
			var visible int
			if err := tc.pool.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM workflow_instances WHERE id = @p1`, tc.otherWF).Scan(&visible); err != nil {
				t.Fatalf("round %d: tenant %s counting the other tenant's workflow: %v", round, tc.name, err)
			}
			if visible != 0 {
				t.Fatalf("round %d: tenant %s's connection can see the other tenant's workflow %s (%d row(s)) -- "+
					"the filter predicate is not enforcing", round, tc.name, tc.otherWF, visible)
			}
		}
	}

	// And through a list, which is the path 2.71 called out by name.
	list, err := storeA.ListWorkflows(ctx, WorkflowFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListWorkflows(A): %v", err)
	}
	if len(list) == 0 {
		t.Fatal("ListWorkflows for tenant A returned nothing -- this is exactly the 2.71 symptom, " +
			"a tenant-scoped read discarded by RLS because the session context was not set")
	}
	for _, wf := range list {
		if wf.ID == wfB {
			t.Fatalf("ListWorkflows for tenant A returned tenant B's workflow %s", wfB)
		}
	}
}

// TestMSSQLTenantIsolation_WorkflowPromises_UnderRealSecurityPolicies is the
// live verification 031_workflow_promises_security_policy.sql's own header
// asked for and could not get: it was written and merged "UNVERIFIED AGAINST
// A LIVE SQL SERVER" by a stream with no SQL Server instance available.
//
// The thing worth being suspicious of here, spelled out in this repo's own
// history: engine/mssql_signals_promises.go's GetPromise, ListPromises,
// ResolvePromise and RejectPromise all carry an explicit
// `AND tenant_id = @p` predicate of their own. A test that reads cross-tenant
// promise data through *those* methods and finds nothing would pass whether
// or not TenantFilter_Promises does anything at all -- exactly the shape
// tiers.yaml already records happening once for a different table (an MSSQL
// cross-tenant assertion that passed against a wide-open policy because the
// Go-level filter did all the work). CreatePromise is the one write path
// with no Go-level tenant predicate (it is a bare INSERT, tenant_id supplied
// as a column value), which is why the read side here is done the same way --
// raw SQL against the tenant-scoped connection pool, naming no tenant_id
// column anywhere in the query text, so TenantFilter_Promises is the only
// thing that can hide a row.
func TestMSSQLTenantIsolation_WorkflowPromises_UnderRealSecurityPolicies(t *testing.T) {
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}

	ctx := context.Background()
	adminDB := testutil.MSSQLTestDB(t)
	t.Cleanup(func() { adminDB.Close() })
	testutil.SetupMSSQLFullSchema(t, adminDB)
	testutil.CleanupMSSQLTestData(t, adminDB)
	t.Cleanup(func() { testutil.CleanupMSSQLTestData(t, adminDB) })

	const (
		tenantA = "cccccccc-cccc-4ccc-cccc-cccccccccccc"
		tenantB = "dddddddd-dddd-4ddd-dddd-dddddddddddd"
	)
	run := uuid.New().String()[:8]

	seed := func(tenant, suffix string) string {
		t.Helper()
		defName := "rls-promise-def-" + run + "-" + suffix
		if _, err := adminDB.ExecContext(ctx, `
			INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
			VALUES (@p1, 1, 0x0061736d, 1, 1, @p2)`, defName, tenant); err != nil {
			t.Fatalf("seed workflow_def for %s: %v", suffix, err)
		}
		wfID := "rls-promise-wf-" + run + "-" + suffix
		if _, err := adminDB.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
			VALUES (@p1, @p2, 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3)`,
			wfID, defName, tenant); err != nil {
			t.Fatalf("seed workflow_instance for %s: %v", suffix, err)
		}
		return wfID
	}
	wfA := seed(tenantA, "a")
	wfB := seed(tenantB, "b")

	enableMSSQLTenantPolicies(t, adminDB)

	factory := NewMSSQLStoreFactory(dsn)
	defer factory.Close()

	poolA, err := factory.getOrCreateTenantPool(ctx, tenantA)
	if err != nil {
		t.Fatalf("getOrCreateTenantPool(A): %v", err)
	}
	poolB, err := factory.getOrCreateTenantPool(ctx, tenantB)
	if err != nil {
		t.Fatalf("getOrCreateTenantPool(B): %v", err)
	}

	storeA, closerA, err := factory.OpenStore(ctx, tenantA)
	if err != nil {
		t.Fatalf("OpenStore(A): %v", err)
	}
	defer closerA.Close()
	storeB, closerB, err := factory.OpenStore(ctx, tenantB)
	if err != nil {
		t.Fatalf("OpenStore(B): %v", err)
	}
	defer closerB.Close()

	promiseA := "rls-promise-" + run + "-a"
	promiseB := "rls-promise-" + run + "-b"
	if err := storeA.CreatePromise(ctx, wfA, "p", promiseA); err != nil {
		t.Fatalf("CreatePromise(A): %v", err)
	}
	if err := storeB.CreatePromise(ctx, wfB, "p", promiseB); err != nil {
		t.Fatalf("CreatePromise(B): %v", err)
	}

	for round := 0; round < 3; round++ {
		for _, tc := range []struct {
			name         string
			pool         *sql.DB
			ownPromise   string
			otherPromise string
		}{
			{"A", poolA, promiseA, promiseB},
			{"B", poolB, promiseB, promiseA},
		} {
			// Sanity: the tenant can see its own row through the same
			// tenant-id-free query, so a 0 result below means "the policy
			// filtered it out", not "this connection has no session context
			// and sees nothing at all" (the 2.71 failure mode).
			var own int
			if err := tc.pool.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM workflow_promises WHERE promise_id = @p1`, tc.ownPromise).Scan(&own); err != nil {
				t.Fatalf("round %d: tenant %s counting its own promise: %v", round, tc.name, err)
			}
			if own != 1 {
				t.Fatalf("round %d: tenant %s sees %d rows for its own promise %s, want 1 -- "+
					"under the shipped filter predicate a session with no tenant context matches no "+
					"rows, so this would also fail if the session context were simply missing",
					round, tc.name, own, tc.ownPromise)
			}

			// The claim under test: no Go-level tenant_id predicate anywhere
			// in this query's text. If TenantFilter_Promises is not actually
			// enforcing -- STATE = OFF, predicate always true, applied to the
			// wrong table -- this returns 1, not 0.
			var other int
			if err := tc.pool.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM workflow_promises WHERE promise_id = @p1`, tc.otherPromise).Scan(&other); err != nil {
				t.Fatalf("round %d: tenant %s counting the other tenant's promise: %v", round, tc.name, err)
			}
			if other != 0 {
				t.Fatalf("round %d: tenant %s's connection can see the other tenant's promise %s "+
					"(%d row(s)) through a query with no tenant_id predicate of its own -- "+
					"TenantFilter_Promises is not enforcing", round, tc.name, tc.otherPromise, other)
			}
		}
	}
}
