package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#3552, B3b-2. Dropping a tenant on a real database, on both dialects that support tenants.
//
// One scenario, run per dialect, because the properties are the same and only the fixture differs. The
// order matters: every REFUSAL is proved to leave the tenant and its rows exactly as they were BEFORE the
// drop that succeeds, so "nothing was deleted" is measured on rows that a successful drop does delete.
//
//	not suspended        -> refused, nothing changed
//	unsettled runs       -> refused with the count, nothing changed (non-zero on BOTH dialects)
//	wrong creator        -> the same not-found as a missing tenant, and no information about the runs
//	a drop that fails    -> rolled back as a whole: tombstone included (atomicity)
//	the drop             -> tombstone with the chain head, tenant and its rows and key gone
//	a bystander          -> every row it owns survives (the negative control)
//	a repeat             -> already_dropped, nothing changed
//	reuse of the id      -> refused, and no org left behind; a different id is fine (the control)
//	a start              -> refused, and the reason names the drop, not "no versions deployed"

const chainHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// dropFixture is what differs between dialects.
type dropFixture struct {
	dialect      string
	db           *sql.DB // the admin connection
	ts           *auth.TenantStore
	schema       string
	processStore engine.WorkflowStore
	deploy       func(tenant uuid.UUID)

	// countRows counts a tenant's rows in a table the way a reader scoped to that tenant sees them.
	countRows func(table string, tenant uuid.UUID) int
	// settle marks every run of the tenant done.
	settle func(tenant uuid.UUID)
	// seedChain gives the tenant an audit chain head.
	seedChain func(tenant uuid.UUID, seq int64)
	// chainHeadCount counts the head rows a tenant has, from the admin connection.
	chainHeadCount func(tenant uuid.UUID) int
	// failingDrop makes admin.drop_tenant fail INSIDE the transaction, after the tombstone is written.
	failingDrop func(req *auth.DropRequest) (restore func())
}

func (fx *dropFixture) scalar(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := fx.db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (fx *dropFixture) q(pg, ms string) string {
	if fx.dialect == auth.DialectPostgres {
		return pg
	}
	return ms
}

func (fx *dropFixture) tombstones(t *testing.T, tenant uuid.UUID) int {
	return fx.scalar(t, fx.q(`SELECT count(*) FROM admin.tenant_tombstones WHERE dropped_tenant_id = $1::uuid`,
		`SELECT count(*) FROM admin.tenant_tombstones WHERE dropped_tenant_id = @p1`), tenant.String())
}

func (fx *dropFixture) tenantRows(t *testing.T, tenant uuid.UUID) int {
	return fx.scalar(t, fx.q(`SELECT count(*) FROM admin.tenants WHERE tenant_id = $1::uuid`,
		`SELECT count(*) FROM admin.tenants WHERE tenant_id = @p1`), tenant.String())
}

func (fx *dropFixture) orgsNamed(t *testing.T, name string) int {
	return fx.scalar(t, fx.q(`SELECT count(*) FROM admin.orgs WHERE name = $1`,
		`SELECT count(*) FROM admin.orgs WHERE name = @p1`), name)
}

func (fx *dropFixture) runs(t *testing.T, tenant uuid.UUID) int {
	// PostgreSQL keeps one workflow_instances row per run for every status, and SQL Server too.
	return fx.countRows("workflow_instances", tenant)
}

func TestATenantIsDroppedOnPostgres(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed cleat#3552 test")
	}
	db := testutil.SuiteTestDB(t, "cleat_worker")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, db)
	factory := engine.NewPostgresStoreFactory(rlsDB, "public")
	ctx := context.Background()

	// The audit plugin's chain table, in a schema of its own and registered the way a plugin registers
	// it, so the drop reads and deletes it exactly as it would the real one. If the suite database ever
	// holds the real one, this test would be reading a table it did not make: say so loudly.
	if n := scalarOn(t, db, `SELECT count(*) FROM admin.plugin_tables WHERE table_name = 'audit_chain_heads'`); n != 0 {
		t.Fatalf("the suite database already registers %d audit_chain_heads table(s); this test builds its own and would read the wrong one", n)
	}
	for _, stmt := range []string{
		`CREATE SCHEMA IF NOT EXISTS zz3552drop`,
		`CREATE TABLE zz3552drop.audit_chain_heads (tenant_id uuid PRIMARY KEY, seq bigint NOT NULL, hash char(64) NOT NULL)`,
		`INSERT INTO admin.plugin_tables (plugin_name, table_name, schema_name, tenant_scoped) VALUES ('zz3552drop', 'audit_chain_heads', 'zz3552drop', true)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM admin.plugin_tables WHERE plugin_name = 'zz3552drop'`)
		_, _ = db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS zz3552drop CASCADE`)
	})

	ts := auth.NewTenantStore(db)
	processStore, pc, err := factory.OpenStore(ctx, engine.DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer pc.Close()

	fx := &dropFixture{
		dialect: auth.DialectPostgres, db: db, ts: ts, schema: "public", processStore: processStore,
		deploy: func(tn uuid.UUID) {
			st, c, err := factory.OpenStore(ctx, tn.String(), "default")
			if err != nil {
				t.Fatalf("OpenStore(%s): %v", tn, err)
			}
			defer c.Close()
			if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
				Name: "drop-def", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("deploy for %s: %v", tn, err)
			}
		},
		countRows: func(table string, tn uuid.UUID) int {
			// The superuser connection: row-level security does not apply to it, so the count is of the
			// tenant's rows and not of the connection's view.
			return scalarOn(t, db, fmt.Sprintf(`SELECT count(*) FROM %s WHERE tenant_id = $1::uuid`, table), tn.String())
		},
		settle: func(tn uuid.UUID) {
			if _, err := db.ExecContext(ctx, `UPDATE workflow_leases SET status = 'done' WHERE tenant_id = $1::uuid`, tn.String()); err != nil {
				t.Fatalf("settle: %v", err)
			}
		},
		seedChain: func(tn uuid.UUID, seq int64) {
			if _, err := db.ExecContext(ctx, `INSERT INTO zz3552drop.audit_chain_heads (tenant_id, seq, hash) VALUES ($1::uuid, $2, $3)`, tn.String(), seq, chainHash); err != nil {
				t.Fatalf("seed chain: %v", err)
			}
		},
		chainHeadCount: func(tn uuid.UUID) int {
			return scalarOn(t, db, `SELECT count(*) FROM zz3552drop.audit_chain_heads WHERE tenant_id = $1::uuid`, tn.String())
		},
		failingDrop: func(req *auth.DropRequest) func() {
			// A schema that does not exist makes admin.drop_tenant raise, after the tombstone is written.
			req.Schema = "no_such_schema_3552"
			return func() {}
		},
	}
	runDropScenario(t, fx)
}

func TestATenantIsDroppedOnSQLServer(t *testing.T) {
	base := os.Getenv("CLEAT_TEST_MSSQL")
	if base == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping database-backed cleat#3552 test")
	}
	adminDB, err := sql.Open("sqlserver", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	// A cleanup and not a defer, and registered BEFORE the one that drops the database: cleanups run last in,
	// first out, and a deferred Close runs when the test function returns, which is before any cleanup, so the
	// DROP DATABASE below would find a closed connection and its error is not read.
	t.Cleanup(func() { _ = adminDB.Close() })
	if err := adminDB.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MSSQL is set but SQL Server is unreachable at %s: %v", redactMSSQLDSN(base), err)
	}
	const dbName = "cleat_drop_3552_test"
	ctx := context.Background()
	if _, err := adminDB.ExecContext(ctx,
		fmt.Sprintf("IF DB_ID('%s') IS NOT NULL BEGIN ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s; END", dbName, dbName, dbName)); err != nil {
		t.Fatalf("drop stale test database: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(),
			fmt.Sprintf("ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s", dbName, dbName))
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
	if _, err := testDB.ExecContext(ctx,
		`CREATE TABLE dbo.audit_chain_heads (tenant_id uniqueidentifier NOT NULL PRIMARY KEY, seq bigint NOT NULL, hash char(64) NOT NULL)`); err != nil {
		t.Fatalf("create the chain table: %v", err)
	}

	ts, err := auth.NewTenantStoreForDialect(testDB, auth.DialectMSSQL)
	if err != nil {
		t.Fatalf("NewTenantStoreForDialect: %v", err)
	}
	factory := engine.NewMSSQLStoreFactory(connStr)
	processStore, pc, err := factory.OpenStore(ctx, engine.DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer pc.Close()

	// scoped runs fn on one connection whose session context is the tenant's, which is the only way
	// SQL Server's row policy lets a tenant's rows be seen.
	scoped := func(tn uuid.UUID, fn func(c *sql.Conn)) {
		c, err := testDB.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		defer c.Close()
		if _, err := c.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, sql.Named("p1", tn.String())); err != nil {
			t.Fatalf("set the session key: %v", err)
		}
		fn(c)
	}

	fx := &dropFixture{
		dialect: auth.DialectMSSQL, db: testDB, ts: ts, processStore: processStore,
		deploy: func(tn uuid.UUID) {
			st, c, err := factory.OpenStore(ctx, tn.String(), "default")
			if err != nil {
				t.Fatalf("OpenStore(%s): %v", tn, err)
			}
			defer c.Close()
			if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
				Name: "drop-def", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("deploy for %s: %v", tn, err)
			}
		},
		countRows: func(table string, tn uuid.UUID) (n int) {
			scoped(tn, func(c *sql.Conn) {
				if err := c.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE tenant_id = @p1`, table), tn.String()).Scan(&n); err != nil {
					t.Fatalf("count %s: %v", table, err)
				}
			})
			return n
		},
		settle: func(tn uuid.UUID) {
			scoped(tn, func(c *sql.Conn) {
				if _, err := c.ExecContext(ctx, `UPDATE workflow_instances SET status = 'done' WHERE tenant_id = @p1`, tn.String()); err != nil {
					t.Fatalf("settle: %v", err)
				}
			})
		},
		seedChain: func(tn uuid.UUID, seq int64) {
			if _, err := testDB.ExecContext(ctx, `INSERT INTO dbo.audit_chain_heads (tenant_id, seq, hash) VALUES (@p1, @p2, @p3)`, tn.String(), seq, chainHash); err != nil {
				t.Fatalf("seed chain: %v", err)
			}
		},
		chainHeadCount: func(tn uuid.UUID) int {
			return scalarOn(t, testDB, `SELECT count(*) FROM dbo.audit_chain_heads WHERE tenant_id = @p1`, tn.String())
		},
		failingDrop: func(req *auth.DropRequest) func() {
			// admin.drop_tenant deletes from every table with a tenant_id column, so a table whose DELETE
			// trigger raises makes it fail after the tombstone is written.
			for _, stmt := range []string{
				`CREATE TABLE dbo.zz3552_block (tenant_id uniqueidentifier NOT NULL)`,
				`CREATE TRIGGER dbo.zz3552_block_trg ON dbo.zz3552_block FOR DELETE AS THROW 50002, 'blocked on purpose', 1`,
			} {
				if _, err := testDB.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			return func() { _, _ = testDB.ExecContext(ctx, `DROP TABLE IF EXISTS dbo.zz3552_block`) }
		},
	}
	// The blocking table needs a row for the tenant, or its DELETE fires no trigger.
	origFail := fx.failingDrop
	fx.failingDrop = func(req *auth.DropRequest) func() {
		restore := origFail(req)
		if _, err := testDB.ExecContext(ctx, `INSERT INTO dbo.zz3552_block (tenant_id) VALUES (@p1)`, req.TenantID.String()); err != nil {
			t.Fatalf("seed the blocking row: %v", err)
		}
		return restore
	}
	runDropScenario(t, fx)
}

func scalarOn(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func runDropScenario(t *testing.T, fx *dropFixture) {
	ctx := context.Background()
	const creator, stranger = "svc-a", "svc-b"

	prov := func(orgName string, id uuid.UUID) uuid.UUID {
		t.Helper()
		res, err := fx.ts.ProvisionTenant(ctx, auth.ProvisionRequest{TenantID: id, OrgName: orgName, Creator: creator})
		if err != nil {
			t.Fatalf("ProvisionTenant: %v", err)
		}
		return res.Tenant.ID
	}
	orgName := "drop-org-" + uuid.NewString()
	target, bystander := prov(orgName, uuid.Nil), prov(orgName, uuid.Nil)

	// Each has a permanent key, a deployed definition and runs; the bystander is the negative control.
	keys := map[uuid.UUID]string{target: auth.GenerateAPIKey(), bystander: auth.GenerateAPIKey()}
	resolves := func(tn uuid.UUID) bool {
		h := sha256.Sum256([]byte(keys[tn]))
		got, err := fx.ts.ResolveTenantFromAPIKey(ctx, h[:])
		return err == nil && got == tn
	}
	for tn, k := range keys {
		if err := fx.ts.CreateAPIKey(ctx, tn, "drop test", k); err != nil {
			t.Fatalf("CreateAPIKey: %v", err)
		}
		fx.deploy(tn)
	}
	start := func(tn uuid.UUID, key string) (string, error) {
		return startPluginWorkflow(ctx, fx.processStore, plugin.StartRequest{
			DefName: "drop-def", Input: json.RawMessage(`{}`), IdempotencyKey: key, TenantID: tn.String(),
		})
	}
	for _, k := range []string{"t1", "t2"} {
		if id, err := start(target, k); err != nil || id == "" {
			t.Fatalf("start a run for the target: id=%q err=%v", id, err)
		}
	}
	if id, err := start(bystander, "b1"); err != nil || id == "" {
		t.Fatalf("start a run for the bystander: id=%q err=%v", id, err)
	}
	fx.seedChain(target, 41)
	fx.seedChain(bystander, 7)

	// The fixture reaches its own rows. Without these, every "nothing was deleted" below could be true of
	// rows nobody could see.
	if got := fx.runs(t, target); got != 2 {
		t.Fatalf("FIXTURE: the target has %d run row(s); want 2", got)
	}
	if got := fx.runs(t, bystander); got != 1 {
		t.Fatalf("FIXTURE: the bystander has %d run row(s); want 1", got)
	}
	if !resolves(target) || !resolves(bystander) {
		t.Fatalf("FIXTURE: a key does not resolve to its tenant before any drop")
	}

	untouched := func(when string) {
		t.Helper()
		if fx.tenantRows(t, target) != 1 || fx.runs(t, target) != 2 || !resolves(target) || fx.chainHeadCount(target) != 1 {
			t.Errorf("%s: the target was changed (tenant rows=%d runs=%d key=%v chain=%d)", when,
				fx.tenantRows(t, target), fx.runs(t, target), resolves(target), fx.chainHeadCount(target))
		}
		if n := fx.tombstones(t, target); n != 0 {
			t.Errorf("%s: %d tombstone(s) written for a drop that did not happen", when, n)
		}
	}
	req := auth.DropRequest{TenantID: target, DroppedBy: creator, OnlyCreator: creator, Schema: fx.schema}

	// --- not suspended
	_, err := fx.ts.DropTenant(ctx, req)
	var refused *auth.DropRefusedError
	if !errors.As(err, &refused) || refused.Reason != auth.DropRefusedNotSuspended {
		t.Fatalf("an unsuspended tenant: %v; want not_suspended", err)
	}
	untouched("not suspended")

	if err := fx.ts.SetTenantSuspended(ctx, target, true); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	// --- unsettled runs: refused WITH THE COUNT, and the count is not zero on either dialect.
	_, err = fx.ts.DropTenant(ctx, req)
	if !errors.As(err, &refused) || refused.Reason != auth.DropRefusedUnsettledRuns || refused.Unsettled != 2 {
		t.Fatalf("a suspended tenant with two unsettled runs: %v; want unsettled_runs with count 2", err)
	}
	untouched("unsettled runs")

	// --- the wrong creator learns nothing: not-found, not the 409 above.
	wrong := req
	wrong.OnlyCreator = stranger
	if _, err := fx.ts.DropTenant(ctx, wrong); !errors.Is(err, auth.ErrTenantNotFound) {
		t.Errorf("another principal's drop: %v; want ErrTenantNotFound and no word about the runs", err)
	}
	untouched("a drop by the wrong creator")

	fx.settle(target)
	if fx.runs(t, target) != 2 {
		t.Fatalf("settling deleted runs")
	}

	// --- a drop that fails INSIDE the transaction rolls the whole thing back, tombstone included.
	failing := req
	restore := fx.failingDrop(&failing)
	_, err = fx.ts.DropTenant(ctx, failing)
	restore()
	if err == nil || errors.As(err, &refused) || errors.Is(err, auth.ErrTenantNotFound) {
		t.Fatalf("a drop whose admin.drop_tenant fails: %v; want that failure", err)
	}
	untouched("a drop that failed inside the transaction")

	// --- the drop.
	res, err := fx.ts.DropTenant(ctx, req)
	if err != nil {
		t.Fatalf("the drop: %v", err)
	}
	if res.AlreadyDropped {
		t.Errorf("the first drop reported already_dropped")
	}
	tb := res.Tombstone
	if tb.TenantID != target || tb.DroppedBy != creator || tb.CreatedBy != creator || tb.Name == "" || tb.OrgID == uuid.Nil {
		t.Errorf("tombstone = %+v", tb)
	}
	if tb.AuditChainSeq == nil || *tb.AuditChainSeq != 41 || tb.AuditChainHash == nil || *tb.AuditChainHash != chainHash {
		t.Errorf("the tombstone's chain head = %v %v; want 41 %s: the chain is deleted with the tenant, so this is its only record",
			tb.AuditChainSeq, tb.AuditChainHash, chainHash)
	}
	if fx.tombstones(t, target) != 1 {
		t.Errorf("tombstone rows = %d; want 1", fx.tombstones(t, target))
	}
	if fx.tenantRows(t, target) != 0 || fx.runs(t, target) != 0 || fx.chainHeadCount(target) != 0 || resolves(target) {
		t.Errorf("the tenant was not dropped (tenant rows=%d runs=%d chain=%d key resolves=%v)",
			fx.tenantRows(t, target), fx.runs(t, target), fx.chainHeadCount(target), resolves(target))
	}

	// --- NEGATIVE CONTROL: the bystander kept every row it had.
	if fx.tenantRows(t, bystander) != 1 || fx.runs(t, bystander) != 1 || fx.chainHeadCount(bystander) != 1 || !resolves(bystander) || fx.tombstones(t, bystander) != 0 {
		t.Errorf("the bystander was touched (tenant rows=%d runs=%d chain=%d key=%v tombstones=%d)",
			fx.tenantRows(t, bystander), fx.runs(t, bystander), fx.chainHeadCount(bystander), resolves(bystander), fx.tombstones(t, bystander))
	}

	// --- a repeat changes nothing and says so; who may see it is decided as for the tenant.
	again, err := fx.ts.DropTenant(ctx, req)
	if err != nil || !again.AlreadyDropped || again.Tombstone.DroppedBy != creator || again.Tombstone.AuditChainSeq == nil || *again.Tombstone.AuditChainSeq != 41 {
		t.Errorf("a repeat: %+v, %v; want already_dropped with the same tombstone", again, err)
	}
	if _, err := fx.ts.DropTenant(ctx, wrong); !errors.Is(err, auth.ErrTenantNotFound) {
		t.Errorf("another principal repeating the drop: %v; want ErrTenantNotFound", err)
	}
	anyone := req
	anyone.OnlyCreator = ""
	if r, err := fx.ts.DropTenant(ctx, anyone); err != nil || !r.AlreadyDropped {
		t.Errorf("an unrestricted repeat: %+v, %v; want already_dropped", r, err)
	}
	if fx.tombstones(t, target) != 1 {
		t.Errorf("a repeat changed the tombstones: %d", fx.tombstones(t, target))
	}

	// --- the default tenant is never droppable here.
	if _, err := fx.ts.DropTenant(ctx, auth.DropRequest{TenantID: uuid.Nil, DroppedBy: creator, Schema: fx.schema}); err == nil || errors.Is(err, auth.ErrTenantNotFound) {
		t.Errorf("dropping the default tenant: %v; want a refusal", err)
	}

	// --- the id cannot be reused. Refused BEFORE the org is resolved, so no org is left behind.
	fresh := "reuse-org-" + uuid.NewString()
	_, err = fx.ts.ProvisionTenant(ctx, auth.ProvisionRequest{TenantID: target, OrgName: fresh, Creator: creator})
	var conflict *auth.ProvisionConflictError
	if !errors.As(err, &conflict) || conflict.Reason != auth.ConflictTenantIDDropped {
		t.Fatalf("creating a tenant under a dropped id: %v; want tenant_id_dropped", err)
	}
	if n := fx.orgsNamed(t, fresh); n != 0 {
		t.Errorf("a refused create left %d org(s) named %q behind", n, fresh)
	}
	// CONTROL: the same org name under a new id is created, so the refusal above was about the id.
	if _, err := fx.ts.ProvisionTenant(ctx, auth.ProvisionRequest{TenantID: uuid.New(), OrgName: fresh, Creator: creator}); err != nil {
		t.Errorf("a create under a new id: %v", err)
	}
	if n := fx.orgsNamed(t, fresh); n != 1 {
		t.Errorf("the control create left %d org(s); want 1", n)
	}

	// --- a start for the dropped tenant is refused, and for the right reason. Its definition was deleted
	// with it, so "no versions deployed" would also be an error: the message is what tells them apart.
	_, err = start(target, "after")
	if err == nil || !strings.Contains(err.Error(), "dropped") {
		t.Errorf("a start for a dropped tenant: %v; want a refusal that says it was dropped", err)
	}
	if fx.runs(t, target) != 0 {
		t.Errorf("a start for a dropped tenant wrote %d row(s) under its id", fx.runs(t, target))
	}
	// CONTROL: the same start for the bystander works, so the refusal was about the tenant.
	if id, err := start(bystander, "b2"); err != nil || id == "" {
		t.Errorf("the bystander's start after the drop: id=%q err=%v", id, err)
	}
}
