package scheduler

import (
	"database/sql"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestTheScheduleCommandsSetTheirOwnTenant covers the failure mode that shipped
// for jobqueue in cleat#1511 and was fixed in cleat#1517, before it can ship
// here. cleat#1512.
//
// The three schedule-* commands do not go through plugin.PluginDB: each opens
// its own *sql.DB from --dsn, so engine.beginTenantTx never runs for them and
// nothing sets cleat.tenant_id. The moment `schedules` carries a policy calling
// cleat.assert_tenant_set(), every one of them fails. That is why the policy
// and the CLI change land in the same commit rather than in sequence.
//
// Having tenant_id in the WHERE clause does not save the reads and does not
// save the INSERT: PostgreSQL applies a FOR ALL policy's USING expression as
// the WITH CHECK for INSERT when none is given, so the check runs before there
// is a row to check against.
//
// IT MUST RUN AS A ROLE THAT CANNOT BYPASS RLS, and the attributes are checked
// here rather than assumed. Every plugin suite in this repository connects as a
// superuser, and PostgreSQL exempts superusers from RLS unconditionally -- this
// test written on the ordinary handle would pass against a policy that does
// nothing at all.
func TestTheScheduleCommandsSetTheirOwnTenant(t *testing.T) {
	// A DATABASE OF THIS SUITE'S OWN, not the shared one. cleat#1512.
	//
	// This test ENABLES ROW LEVEL SECURITY on `schedules` and puts a policy on
	// it. Under testutil.TestDB that table is shared with every other package
	// running concurrently, so in a whole-repo run -- which the Tier 2 gate
	// does -- the policy applies to their statements and their writes to this
	// test's rows. The equivalent jobqueue test failed exactly that way, with a
	// message that named neither the sharing nor the policy.
	admin := testutil.SuiteTestDB(t, "scheduler")
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec(`
		CREATE TABLE IF NOT EXISTS schedules (
			tenant_id     UUID NOT NULL,
			id            UUID PRIMARY KEY,
			name          TEXT NOT NULL,
			cron          TEXT NOT NULL,
			workflow_name TEXT NOT NULL,
			input         JSONB DEFAULT '{}',
			enabled       BOOLEAN DEFAULT true,
			last_run_at   TIMESTAMPTZ,
			next_run_at   TIMESTAMPTZ,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("create schedules: %v", err)
	}
	// A row left by an earlier run makes a later count pass for the wrong
	// reason; testutil.TestDB persists between runs.
	if _, err := admin.Exec(`DELETE FROM schedules`); err != nil {
		t.Fatalf("clearing schedules: %v", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE schedules ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE schedules FORCE ROW LEVEL SECURITY`,
		`DROP POLICY IF EXISTS schedules_tenant_isolation ON schedules`,
		`CREATE POLICY schedules_tenant_isolation ON schedules
		     FOR ALL USING (cleat.tenant_row_is_visible(tenant_id))`,
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP POLICY IF EXISTS schedules_tenant_isolation ON schedules`)
		_, _ = admin.Exec(`ALTER TABLE schedules NO FORCE ROW LEVEL SECURITY`)
		_, _ = admin.Exec(`ALTER TABLE schedules DISABLE ROW LEVEL SECURITY`)
	})

	testutil.SetupPostgresRLSRole(t, admin)
	if _, err := admin.Exec(
		`GRANT SELECT, INSERT, UPDATE, DELETE ON schedules TO ` + testutil.PostgresRLSTestRole); err != nil {
		t.Fatalf("granting on schedules: %v", err)
	}

	var isSuper, bypasses bool
	if err := admin.QueryRow(
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1`,
		testutil.PostgresRLSTestRole).Scan(&isSuper, &bypasses); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if isSuper || bypasses {
		t.Fatalf("the RLS test role is superuser=%v bypassrls=%v; either exempts it from every "+
			"policy, so nothing below could fail", isSuper, bypasses)
	}

	dsn := rlsDSNFor(t, admin)
	tenant := uuid.New()

	// POSITIVE CONTROL FIRST: an unscoped INSERT on this connection must be
	// REFUSED. Without this, the commands succeeding below is equally
	// consistent with a table carrying no policy at all.
	low, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open low-privilege connection: %v", err)
	}
	defer low.Close()

	// The commands write through `dsn` and the verification reads through
	// `admin`. Assert they are the same database: if they are not, the command
	// succeeds, the row lands somewhere real, and the read finds nothing --
	// which is indistinguishable from the command being broken.
	var adminDB, lowDB string
	if err := admin.QueryRow(`SELECT current_database()`).Scan(&adminDB); err != nil {
		t.Fatalf("reading the admin connection's database: %v", err)
	}
	if err := low.QueryRow(`SELECT current_database()`).Scan(&lowDB); err != nil {
		t.Fatalf("reading the low-privilege connection's database: %v", err)
	}
	if adminDB != lowDB {
		t.Fatalf("the commands would write to %q and this test reads from %q; "+
			"those must be the same database or every assertion below is meaningless",
			lowDB, adminDB)
	}
	_, bareErr := low.Exec(
		`INSERT INTO schedules (tenant_id, id, name, cron, workflow_name) VALUES ($1,$2,'n','* * * * *','w')`,
		tenant, uuid.New())
	if bareErr == nil {
		t.Fatal("an unscoped INSERT SUCCEEDED, so the policy is not in force and the " +
			"commands below would prove nothing")
	}
	if !strings.Contains(bareErr.Error(), "tenant") {
		t.Fatalf("the unscoped INSERT failed for an unrelated reason: %v", bareErr)
	}

	run := func(t *testing.T, name string, args ...string) error {
		t.Helper()
		for _, c := range (&Plugin{}).RegisterCommands() {
			if c.Name == name {
				return c.Run(append([]string{"--dsn", dsn, "--tenant", tenant.String()}, args...))
			}
		}
		t.Fatalf("%s is not registered; this test addresses a command that no longer exists", name)
		return nil
	}

	// schedule-add is the INSERT, and the one the WITH CHECK semantics bite.
	if err := run(t, "schedule-add",
		"--name", "nightly", "--cron", "0 3 * * *", "--workflow", "wf"); err != nil {
		t.Fatalf("schedule-add failed against a policied schedules table: %v", err)
	}

	var id string
	if err := admin.QueryRow(
		`SELECT id FROM schedules WHERE tenant_id = $1 AND name = 'nightly'`, tenant).Scan(&id); err != nil {
		t.Fatalf("the row schedule-add reported creating is not there: %v", err)
	}

	// schedule-list is the read path through the same helper.
	if err := run(t, "schedule-list"); err != nil {
		t.Fatalf("schedule-list failed against a policied schedules table: %v", err)
	}

	// schedule-delete both reads and writes, and reports "not found" when the
	// policy hides the row rather than erroring -- so a bad scope here fails
	// as a wrong answer rather than a refusal.
	if err := run(t, "schedule-delete", "--id", id); err != nil {
		t.Fatalf("schedule-delete failed against a policied schedules table: %v", err)
	}
	var n int
	if err := admin.QueryRow(
		`SELECT count(*) FROM schedules WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
		t.Fatalf("counting after delete: %v", err)
	}
	if n != 0 {
		t.Errorf("schedules holds %d rows after schedule-delete, want 0", n)
	}
}

func rlsDSNFor(t *testing.T, admin *sql.DB) string {
	t.Helper()
	var dbName string
	if err := admin.QueryRow(`SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatalf("reading the admin connection's database: %v", err)
	}
	u, err := url.Parse(testutil.PostgresTestDSN())
	if err != nil {
		t.Fatalf("PostgresTestDSN is not a URL: %v", err)
	}
	u.Path = "/" + dbName
	dsn, err := testutil.PostgresRLSDSN(u.String())
	if err != nil {
		t.Fatalf("derive RLS role DSN: %v", err)
	}
	return dsn
}
