package jobqueue

import (
	"database/sql"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestTheEnqueueCommandSetsItsOwnTenant is a regression test for a defect this
// plugin's own tenant-scoping change shipped. cleat#1512.
//
// `cleat jobqueue-enqueue` does not go through plugin.PluginDB. It opens its
// own *sql.DB from --dsn, so engine.beginTenantTx never runs for it and nothing
// was ever going to set cleat.tenant_id. The moment task_queue got a policy
// calling cleat.assert_tenant_set(), the command's bare INSERT started failing
// with "cleat.tenant_id is not set".
//
// Having the tenant in the VALUES list does not help. PostgreSQL applies a
// FOR ALL policy's USING expression as the WITH CHECK for INSERT when no
// WITH CHECK is given, so the check runs before there is a row to check.
//
// HOW IT WAS MISSED, because that generalises and the fix does not: the survey
// that found which call sites needed marking matched `.db.(Exec|Query|QueryRow)`.
// commands.go opens a DIFFERENT handle and calls Exec on that, so it was not in
// the population at all. The scan answered "which calls on the plugin's handle
// need a tenant" while appearing to answer "which statements need a tenant".
// Any plugin whose CLI writes its own tables has this shape; scheduler's does.
//
// IT MUST RUN AS A ROLE THAT CANNOT BYPASS RLS. PostgreSQL exempts superusers
// unconditionally, and every plugin suite here connects as one, so this test
// written on the ordinary handle would pass against a completely broken policy.
func TestTheEnqueueCommandSetsItsOwnTenant(t *testing.T) {
	// A DATABASE OF THIS SUITE'S OWN, not the shared one. cleat#1512.
	//
	// This test ENABLES ROW LEVEL SECURITY on task_queue and creates a policy
	// on it. On testutil.TestDB that table is shared with every other package
	// running at the same time, so under a whole-repo run -- which is what the
	// Tier 2 gate does -- the policy applies to their statements too, and their
	// DELETEs apply to this test's rows. It passed in isolation and failed
	// there, reporting "the command reported success without the row landing",
	// which names neither the sharing nor the policy.
	//
	// SuiteTestDB exists for exactly this and makes the isolation the default
	// rather than something each cleanup site has to get right.
	admin := testutil.SuiteTestDB(t, "jobqueue")
	t.Cleanup(func() { admin.Close() })

	// The policy the TenantScoped migration installs. Applied directly so the
	// test does not depend on plugin migration ordering.
	if _, err := admin.Exec(`
		CREATE TABLE IF NOT EXISTS task_queue (
			tenant_id    UUID NOT NULL,
			queue_name   TEXT NOT NULL,
			job_id       UUID NOT NULL,
			payload      JSONB NOT NULL DEFAULT '{}',
			status       TEXT NOT NULL DEFAULT 'pending',
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			started_at   TIMESTAMPTZ,
			completed_at TIMESTAMPTZ,
			PRIMARY KEY (tenant_id, queue_name, job_id)
		)`); err != nil {
		t.Fatalf("create task_queue: %v", err)
	}
	// Start from a known state: a row left by an earlier run makes a later
	// count pass for the wrong reason.
	if _, err := admin.Exec(`DELETE FROM task_queue`); err != nil {
		t.Fatalf("clearing task_queue: %v", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE task_queue ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE task_queue FORCE ROW LEVEL SECURITY`,
		`DROP POLICY IF EXISTS task_queue_tenant_isolation ON task_queue`,
		`CREATE POLICY task_queue_tenant_isolation ON task_queue
		     FOR ALL USING (cleat.tenant_row_is_visible(tenant_id))`,
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP POLICY IF EXISTS task_queue_tenant_isolation ON task_queue`)
		_, _ = admin.Exec(`ALTER TABLE task_queue NO FORCE ROW LEVEL SECURITY`)
		_, _ = admin.Exec(`ALTER TABLE task_queue DISABLE ROW LEVEL SECURITY`)
	})

	lowPrivDSN := rlsDSNFor(t, admin)
	testutil.SetupPostgresRLSRole(t, admin)
	if _, err := admin.Exec(
		`GRANT SELECT, INSERT, UPDATE, DELETE ON task_queue TO ` + testutil.PostgresRLSTestRole); err != nil {
		t.Fatalf("granting on task_queue: %v", err)
	}

	// THE SENSOR MUST BE LIVE. A superuser or a BYPASSRLS role passes every
	// assertion below whatever the policy says, so this is checked rather than
	// assumed.
	var isSuper, bypasses bool
	if err := admin.QueryRow(
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1`,
		testutil.PostgresRLSTestRole).Scan(&isSuper, &bypasses); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if isSuper || bypasses {
		t.Fatalf("the RLS test role is superuser=%v bypassrls=%v; either exempts it from "+
			"every policy, and this test could not fail", isSuper, bypasses)
	}

	// POSITIVE CONTROL, first: a bare INSERT on this connection must be
	// REFUSED. That is what proves the policy is live and that the command
	// below succeeding means something. Without it, a green run is equally
	// consistent with a table that has no policy on it at all.
	low, err := sql.Open("postgres", lowPrivDSN)
	if err != nil {
		t.Fatalf("open low-privilege connection: %v", err)
	}
	defer low.Close()

	// THE TWO CONNECTIONS MUST BE ON THE SAME DATABASE, and this is asserted
	// rather than assumed because the failure it guards against is silent.
	//
	// The command writes through lowPrivDSN and the verification below reads
	// through admin. If those resolve to different databases -- which is
	// possible whenever the suite is on a per-package database and the DSN is
	// derived from the environment rather than from the connection -- the
	// command succeeds, the row lands somewhere real, and the read finds
	// nothing. The symptom is "the command reported success without the row
	// landing", which points at the command and not at the DSN.
	var adminDB, lowDB string
	if err := admin.QueryRow(`SELECT current_database()`).Scan(&adminDB); err != nil {
		t.Fatalf("reading the admin connection's database: %v", err)
	}
	if err := low.QueryRow(`SELECT current_database()`).Scan(&lowDB); err != nil {
		t.Fatalf("reading the low-privilege connection's database: %v", err)
	}
	if adminDB != lowDB {
		t.Fatalf("the command would write to %q and this test reads from %q.\n\n"+
			"Those must be the same database or the assertions below are meaningless. "+
			"The low-privilege DSN is derived from testutil.PostgresTestDSN() with the "+
			"admin connection's database name swapped in; if the suite is on a "+
			"per-package database and that swap did not take, this is where it shows.",
			lowDB, adminDB)
	}
	_, bareErr := low.Exec(
		`INSERT INTO task_queue (tenant_id, queue_name, job_id) VALUES ($1,$2,$3)`,
		uuid.New(), "q", uuid.New())
	if bareErr == nil {
		t.Fatal("a bare INSERT with no tenant set SUCCEEDED. The policy is not in force, so " +
			"the command succeeding below would prove nothing.")
	}
	if !strings.Contains(bareErr.Error(), "tenant") {
		t.Fatalf("the bare INSERT failed, but not for the reason this test is about: %v", bareErr)
	}

	// THE CLAIM: the command sets its own tenant and the insert lands.
	tenant := uuid.New()
	cmds := (&Plugin{}).RegisterCommands()
	var enqueue func([]string) error
	for _, c := range cmds {
		if c.Name == "jobqueue-enqueue" {
			enqueue = c.Run
		}
	}
	if enqueue == nil {
		t.Fatal("jobqueue-enqueue command not registered; this test is addressing a command " +
			"that no longer exists under that name")
	}
	if err := enqueue([]string{
		"--dsn", lowPrivDSN,
		"--tenant", tenant.String(),
		"--queue", "the-cli-queue",
		"--payload", `{"k":"v"}`,
	}); err != nil {
		t.Fatalf("jobqueue-enqueue failed against a policied task_queue: %v\n\n"+
			"This is the defect the test exists for: the command opens its own *sql.DB, so "+
			"nothing sets cleat.tenant_id for it unless the command does.", err)
	}

	var n int
	if err := admin.QueryRow(
		`SELECT count(*) FROM task_queue WHERE tenant_id = $1 AND queue_name = 'the-cli-queue'`,
		tenant).Scan(&n); err != nil {
		t.Fatalf("counting the enqueued row: %v", err)
	}
	if n != 1 {
		t.Errorf("task_queue holds %d rows for this tenant, want 1 -- the command reported "+
			"success without the row landing", n)
	}
}

// rlsDSNFor builds a DSN for the low-privilege role against the database the
// admin handle is actually on, which is not necessarily the one
// PostgresTestDSN names once a suite has its own database.
func rlsDSNFor(t *testing.T, admin *sql.DB) string {
	t.Helper()
	var dbName string
	if err := admin.QueryRow(`SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatalf("reading the admin connection's database: %v", err)
	}
	u, err := url.Parse(testutil.PostgresTestDSN())
	if err != nil {
		t.Fatalf("PostgresTestDSN is not a URL, so the low-privilege DSN cannot be derived: %v", err)
	}
	u.Path = "/" + dbName
	dsn, err := testutil.PostgresRLSDSN(u.String())
	if err != nil {
		t.Fatalf("derive RLS role DSN: %v", err)
	}
	return dsn
}
