package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
)

// cleat#3552, B3b-2. The lock a drop takes on the tenant's row is what makes "suspended, with no unsettled run"
// true at the moment of the delete and not just at the moment of the check, and until this file nothing pinned it:
// with the lock removed, every other test in the drop's suite still passed on both dialects (cleat-review
// measured 39 passing and 0 failing under each mutant). The two scenarios below are cleat-review's probe, kept.
//
//	resume vs drop   a resume holds the row, uncommitted, when the drop arrives. The drop must WAIT, and then
//	                 refuse: not_suspended, the tenant still there, no tombstone.
//	two drops        two drops of one suspended tenant at once: exactly one drops it and the other reports
//	                 already_dropped, with no error.
//
// Measured with the lock removed (PostgreSQL FOR UPDATE; SQL Server UPDLOCK, under READ_COMMITTED_SNAPSHOT, which
// is the setting that makes a plain read ignore an uncommitted writer): PostgreSQL drops a tenant that was just
// resumed; SQL Server does the same, and a double drop fails with a primary key violation on the tombstone.
//
// NO ASSERTION HERE DEPENDS ON HOW LONG ANYTHING TOOK. The drop is started while the resume is open, and given
// a bounded moment to show itself if it does NOT wait; what is asserted is the outcome. The bound can only make
// a mutant slip past on a very slow machine, never make correct code fail.

type raceArm struct {
	name, dialect, schema string
	db                    *sql.DB
	ts                    *auth.TenantStore
	resume                string // an UPDATE that resumes $1 / @p1
}

func runDropRaces(t *testing.T, a raceArm) {
	ctx := context.Background()
	ph := "$1"
	if a.dialect == auth.DialectMSSQL {
		ph = "@p1"
	}
	count := func(q string, id uuid.UUID) int {
		t.Helper()
		var n int
		if err := a.db.QueryRowContext(ctx, q+ph, id.String()).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	tombstones := func(id uuid.UUID) int {
		return count(`SELECT count(*) FROM admin.tenant_tombstones WHERE dropped_tenant_id = `, id)
	}
	tenantRows := func(id uuid.UUID) int { return count(`SELECT count(*) FROM admin.tenants WHERE tenant_id = `, id) }
	suspendedTenant := func() uuid.UUID {
		res, err := a.ts.ProvisionTenant(ctx, auth.ProvisionRequest{OrgName: "race-" + uuid.NewString(), Creator: "svc-a"})
		if err != nil {
			t.Fatalf("ProvisionTenant: %v", err)
		}
		if err := a.ts.SetTenantSuspended(ctx, res.Tenant.ID, true); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		return res.Tenant.ID
	}
	drop := func(id uuid.UUID) (auth.DropResult, error) {
		return a.ts.DropTenant(ctx, auth.DropRequest{TenantID: id, DroppedBy: "operator:race", Schema: a.schema})
	}
	type outcome struct {
		res auth.DropResult
		err error
	}

	// --- resume vs drop
	id := suspendedTenant()
	resumeTx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumeTx.ExecContext(ctx, a.resume, id.String()); err != nil {
		t.Fatalf("resume in a transaction: %v", err)
	}
	done := make(chan outcome, 1)
	go func() { r, err := drop(id); done <- outcome{r, err} }()
	// Give a drop that does NOT wait for the resume time to finish; a drop that waits is still waiting here.
	var early *outcome
	select {
	case o := <-done:
		early = &o
	case <-time.After(1500 * time.Millisecond):
	}
	if err := resumeTx.Commit(); err != nil {
		t.Fatalf("commit the resume: %v", err)
	}
	var o outcome
	if early != nil {
		o = *early
	} else {
		select {
		case o = <-done:
		case <-time.After(60 * time.Second):
			t.Fatal("UNMEASURED: the drop never returned after the resume committed")
		}
	}
	var refused *auth.DropRefusedError
	if !errors.As(o.err, &refused) || refused.Reason != auth.DropRefusedNotSuspended {
		t.Errorf("%s: a drop that met an uncommitted resume returned %v (already_dropped=%v); want a refusal, not_suspended",
			a.name, o.err, o.res.AlreadyDropped)
	}
	if tenantRows(id) != 1 || tombstones(id) != 0 {
		t.Errorf("%s: after the resume committed the tenant has %d row(s) and %d tombstone(s); want it kept, with none",
			a.name, tenantRows(id), tombstones(id))
	}
	// Clean up the tenant this scenario left live, through the drop itself.
	if err := a.ts.SetTenantSuspended(ctx, id, true); err != nil {
		t.Fatalf("suspend for cleanup: %v", err)
	}
	if _, err := drop(id); err != nil {
		t.Fatalf("drop for cleanup: %v", err)
	}

	// --- two drops at once
	id2 := suspendedTenant()
	start := make(chan struct{})
	out := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; r, err := drop(id2); out <- outcome{r, err} }()
	}
	close(start)
	r1, r2 := <-out, <-out
	fresh, again, failed := 0, 0, 0
	for _, r := range []outcome{r1, r2} {
		switch {
		case r.err != nil:
			failed++
		case r.res.AlreadyDropped:
			again++
		default:
			fresh++
		}
	}
	if fresh != 1 || again != 1 || failed != 0 {
		t.Errorf("%s: two concurrent drops gave fresh=%d already_dropped=%d errors=%d (%v / %v); want 1, 1, 0",
			a.name, fresh, again, failed, r1.err, r2.err)
	}
	if tenantRows(id2) != 0 || tombstones(id2) != 1 {
		t.Errorf("%s: after two drops the tenant has %d row(s) and %d tombstone(s); want 0 and 1", a.name, tenantRows(id2), tombstones(id2))
	}
}

func TestADropWaitsForAResumeAndForASecondDropOnPostgres(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed cleat#3552 test")
	}
	db := testutil.SuiteTestDB(t, "cleat_worker")
	runDropRaces(t, raceArm{name: "postgres", dialect: auth.DialectPostgres, schema: "public", db: db,
		ts: auth.NewTenantStore(db), resume: `UPDATE admin.tenants SET suspended = false WHERE tenant_id = $1`})
}

func TestADropWaitsForAResumeAndForASecondDropOnSQLServer(t *testing.T) {
	base := os.Getenv("CLEAT_TEST_MSSQL")
	if base == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping database-backed cleat#3552 test")
	}
	// Both settings of READ_COMMITTED_SNAPSHOT. With it ON a plain read ignores an uncommitted writer, which is
	// the setting under which a lock that is only a read does nothing; with it OFF the read blocks and hides that.
	for _, rcsi := range []bool{false, true} {
		t.Run(fmt.Sprintf("rcsi=%v", rcsi), func(t *testing.T) {
			ctx := context.Background()
			adminDB, err := sql.Open("sqlserver", base)
			if err != nil {
				t.Fatal(err)
			}
			defer adminDB.Close()
			const dbName = "cleat_drop_race_3552_test"
			dropDB := fmt.Sprintf("IF DB_ID('%s') IS NOT NULL BEGIN ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s; END", dbName, dbName, dbName)
			if _, err := adminDB.ExecContext(ctx, dropDB); err != nil {
				t.Fatalf("drop stale test database: %v", err)
			}
			if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
				t.Fatalf("create test database: %v", err)
			}
			t.Cleanup(func() { _, _ = adminDB.ExecContext(context.Background(), dropDB) })
			if rcsi {
				if _, err := adminDB.ExecContext(ctx, "ALTER DATABASE "+dbName+" SET READ_COMMITTED_SNAPSHOT ON WITH ROLLBACK IMMEDIATE"); err != nil {
					t.Fatalf("enable READ_COMMITTED_SNAPSHOT: %v", err)
				}
			}
			connStr, err := mssqlConnStrForDB(base, dbName)
			if err != nil {
				t.Fatalf("derive test DSN: %v", err)
			}
			db, err := sql.Open("sqlserver", connStr)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := migration.NewRunner(db, migration.DialectMSSQL, "../../migrations").Run(ctx); err != nil {
				t.Fatalf("apply mssql migrations: %v", err)
			}
			// PRECONDITION: the setting under test is the setting in force, or the two arms are one arm.
			var on bool
			if err := db.QueryRowContext(ctx, "SELECT is_read_committed_snapshot_on FROM sys.databases WHERE name = DB_NAME()").Scan(&on); err != nil || on != rcsi {
				t.Fatalf("PRECONDITION: is_read_committed_snapshot_on=%v, want %v (err %v)", on, rcsi, err)
			}
			ts, err := auth.NewTenantStoreForDialect(db, auth.DialectMSSQL)
			if err != nil {
				t.Fatal(err)
			}
			runDropRaces(t, raceArm{name: fmt.Sprintf("sqlserver rcsi=%v", rcsi), dialect: auth.DialectMSSQL, db: db, ts: ts,
				resume: `UPDATE admin.tenants SET suspended = 0 WHERE tenant_id = @p1`})
		})
	}
}
