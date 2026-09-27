package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// check-db's runtime counts come from the database cleat-worker actually runs
// in, which on MySQL is not the one --db names.
//
// cleat#1956's audit. MySQL has no schemas-inside-a-database and no row-level
// security, so a tenant's per-tenant tables live in a database of their own,
// cleat_<tenant-id>. The migration set is applied to the BASE database too, so
// `workflow_instances` exists there as well -- empty. An unqualified
// `SELECT ... FROM workflow_instances` on the --db connection therefore did not
// fail. It resolved, against the wrong database, and answered zero. Measured
// before the fix, on a deployment with one instance in the tenant database:
//
//	cleatctl check-db  ->  INSTANCES: 0 total ... STATUS: healthy
//
// which is a post-incident tool reporting health about a database nothing runs
// in.
//
// THE ASSERTION IS THE COUNT, NOT ITS SIGN, and that is what makes this test a
// control rather than a coincidence. It arranges for the two databases to hold
// DIFFERENT numbers of rows and then requires the printed figure to be the
// tenant database's. "Greater than zero" would pass on a shared test database
// that happened to have rows in the base one -- passing for the reason the bug
// existed. The pre-fix code prints the base count, so this goes red on exactly
// the revert.
//
// MySQL only, deliberately. There is nothing to check on PostgreSQL or SQL
// Server: the qualifier is empty there and the statement is character-for-
// character the one that shipped, which TestEveryInlineStatementParsesOnPostgres
// covers. A three-dialect loop here would add two subtests that assert the
// absence of a mechanism those dialects do not have.
func TestCheckDBCountsTheTenantsDatabaseOnMySQL(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectMySQL)
	ctx := context.Background()

	// NOT A SKIP, and scripts/check-skips.sh is right to refuse one here.
	//
	// testutil.TestDB has already decided MySQL was asked for -- it skips on
	// its own if it was not -- so reaching this line means a MySQL database is
	// in play. But TestDB falls back to a BUILT-IN DEFAULT DSN when the
	// variable is unset (engine/testutil/mysql_schema.go:80), and this test
	// needs the DSN *string*: tenantScopedDB builds the per-tenant database's
	// DSN from it. So an empty variable here is not "MySQL is unavailable", it
	// is "TestDB is talking to a server this test cannot name" -- a
	// configuration the harness cannot act on, which check-skips.sh's case (b)
	// says must fail loudly rather than pass as a skip.
	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		t.Fatalf("CLEAT_TEST_MYSQL is unset while testutil.TestDB returned a MySQL database, " +
			"so it connected via its built-in default and this test has no DSN to derive the " +
			"per-tenant database from. Set CLEAT_TEST_MYSQL to the same server.")
	}

	// The DEFAULT tenant, because that is the only one check-db can report on:
	// it takes no tenant argument and reads defaultTenantID. Not a limitation
	// this test invents -- see the comment on that constant.
	tenant := defaultTenantID

	// The tenant's own database, created and schema-applied the way production
	// does it at cmd/cleat-worker startup. tenantScopedDB rather than
	// tenantRuntimeQualifier here on purpose: this is the arranging half and it
	// is allowed to create. The command under test uses the read-only one.
	tenantDB, err := dialectMySQL.tenantScopedDB(ctx, db, dsn, tenant)
	if err != nil {
		t.Fatalf("tenantScopedDB: %v", err)
	}
	testutil.SetupMySQLFullSchema(t, tenantDB)

	// A row in the TENANT database and none matching it in the base one. The
	// def row is required: workflow_instances has a foreign key onto
	// workflow_defs, which is also what stops this test inventing a shape the
	// engine would not produce.
	defName := fmt.Sprintf("CheckDBProbe%d", time.Now().UnixNano())
	if _, err := tenantDB.ExecContext(ctx,
		`INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id) VALUES (?, 1, ?, ?)`,
		defName, []byte{0}, tenant,
	); err != nil {
		t.Fatalf("seed workflow_defs in the tenant database: %v", err)
	}
	if _, err := tenantDB.ExecContext(ctx,
		`INSERT INTO workflow_instances (id, def_name, def_version, status, tenant_id)
		 VALUES (?, ?, 1, 'ready', ?)`,
		defName, defName, tenant,
	); err != nil {
		t.Fatalf("seed workflow_instances in the tenant database: %v", err)
	}

	// The two populations, read directly. These are the numbers the assertion
	// discriminates between, so they are measured rather than assumed -- a
	// shared test database may hold anything in either.
	var tenantRows, baseRows int64
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_instances`).Scan(&tenantRows); err != nil {
		t.Fatalf("count workflow_instances in the tenant database: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_instances`).Scan(&baseRows); err != nil {
		t.Fatalf("count workflow_instances in the base database: %v", err)
	}
	if tenantRows == baseRows {
		t.Fatalf("the two databases hold the same number of workflow_instances (%d), so this "+
			"test cannot tell which one check-db read. The seed above should have made them "+
			"differ; if the base database is accumulating the same rows, the fixture is wrong "+
			"rather than the subject", tenantRows)
	}

	stdout, _ := withExitPanicOutput(t, func() {
		runCheckDB(ctx, db, dialectMySQL, dsn, nil)
	})

	// Which database it says it read.
	wantDB := "cleat_" + strings.ReplaceAll(tenant, "-", "_")
	if !strings.Contains(stdout, "RUNTIME DATA: "+wantDB) {
		t.Errorf("check-db does not say which database its runtime figures came from; "+
			"expected a RUNTIME DATA line naming %s:\n%s", wantDB, stdout)
	}

	// And the count itself.
	m := regexp.MustCompile(`INSTANCES: (\d+) total`).FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("no INSTANCES line in check-db's output:\n%s", stdout)
	}
	got, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("unreadable instance count %q: %v", m[1], err)
	}
	if got != tenantRows {
		t.Errorf("check-db reported %d workflow instances; the tenant database %s holds %d "+
			"and the base database holds %d.\n\n"+
			"Reading %d means the count came from the base database -- the database --db "+
			"names, which the migration set also creates these tables in, so the query "+
			"resolves there and answers about a database cleat-worker never runs in. "+
			"cleat#1956.\n%s",
			got, wantDB, tenantRows, baseRows, baseRows, stdout)
	}
}
