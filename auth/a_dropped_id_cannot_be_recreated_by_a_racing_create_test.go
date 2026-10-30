package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
)

// cleat#3552, B3b-2. A create of a tenant id cannot slip in behind a drop of the same id (cleat-review, by
// reading, on cleat#3608).
//
// ProvisionTenant refuses a dropped id up front, before it resolves the org, so a refused request leaves no org
// behind. That check alone is a time-of-check gap: a drop can commit AFTER it and BEFORE the insert, at which
// point the tenant row is gone, the id looks free, and the tenant is created under an id the tombstone says is
// dead. The guard is a second check INSIDE the insert's transaction, after the insert.
//
// The interleave is forced with afterTombstonePrecheck, which runs in exactly that gap: it drops the tenant, and
// ProvisionTenant then carries on with a precheck that has already passed. Without the in-transaction check the
// create succeeds and the tenant is live next to its own tombstone.

func TestACreateCannotReuseAnIdThatWasDroppedAfterItsFirstCheckOnPostgres(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed cleat#3552 test")
	}
	db := testutil.SuiteTestDB(t, "auth_provision_race")
	runReuseRace(t, db, DialectPostgres, "public")
}

func TestACreateCannotReuseAnIdThatWasDroppedAfterItsFirstCheckOnSQLServer(t *testing.T) {
	base := os.Getenv("CLEAT_TEST_MSSQL")
	if base == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping database-backed cleat#3552 test")
	}
	ctx := context.Background()
	adminDB, err := sql.Open("sqlserver", base)
	if err != nil {
		t.Fatal(err)
	}
	defer adminDB.Close()
	const dbName = "cleat_auth_reuse_3552_test"
	dropDB := fmt.Sprintf("IF DB_ID('%s') IS NOT NULL BEGIN ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s; END", dbName, dbName, dbName)
	if _, err := adminDB.ExecContext(ctx, dropDB); err != nil {
		t.Fatalf("drop stale test database: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() { _, _ = adminDB.ExecContext(context.Background(), dropDB) })
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("database", dbName)
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migration.NewRunner(db, migration.DialectMSSQL, "../migrations").Run(ctx); err != nil {
		t.Fatalf("apply mssql migrations: %v", err)
	}
	runReuseRace(t, db, DialectMSSQL, "")
}

func runReuseRace(t *testing.T, db *sql.DB, dialect, schema string) {
	ctx := context.Background()
	ts, err := NewTenantStoreForDialect(db, dialect)
	if err != nil {
		t.Fatal(err)
	}
	ph := "$1"
	if dialect == DialectMSSQL {
		ph = "@p1"
	}
	count := func(q string, arg any) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, q+ph, arg).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	// A suspended tenant with an explicit id, ready to be dropped.
	id := uuid.New()
	orgName := "reuse-" + uuid.NewString()
	if _, err := ts.ProvisionTenant(ctx, ProvisionRequest{TenantID: id, OrgName: orgName, Creator: "svc-a"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := ts.SetTenantSuspended(ctx, id, true); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	// The create under test: the same id, arriving while the tenant still exists. The drop lands AFTER its
	// first tombstone check and before its insert.
	var dropErr error
	dropped := false
	ts.afterTombstonePrecheck = func() {
		_, dropErr = ts.DropTenant(ctx, DropRequest{TenantID: id, DroppedBy: "operator:race", Schema: schema})
		dropped = true
	}
	_, err = ts.ProvisionTenant(ctx, ProvisionRequest{TenantID: id, OrgName: orgName, Creator: "svc-a"})
	ts.afterTombstonePrecheck = nil

	// PRECONDITION: the interleave happened, or this test measured nothing.
	if !dropped || dropErr != nil {
		t.Fatalf("PRECONDITION: the drop ran=%v err=%v; the gap was not exercised", dropped, dropErr)
	}
	var conflict *ProvisionConflictError
	if !errors.As(err, &conflict) || conflict.Reason != ConflictTenantIDDropped {
		t.Errorf("a create that raced a drop of its own id returned %v; want tenant_id_dropped", err)
	}
	if n := count(`SELECT count(*) FROM admin.tenants WHERE tenant_id = `, id.String()); n != 0 {
		t.Errorf("the id is live again (%d tenant row(s)) next to its own tombstone", n)
	}
	if n := count(`SELECT count(*) FROM admin.tenant_tombstones WHERE dropped_tenant_id = `, id.String()); n != 1 {
		t.Errorf("tombstones = %d; want 1", n)
	}
	if n := count(`SELECT count(*) FROM admin.tenant_api_keys WHERE tenant_id = `, id.String()); n != 0 {
		t.Errorf("a refused create left %d key(s) under the dropped id", n)
	}

	// CONTROL: the same hook with nothing dropped creates normally, so the refusal above is the tombstone's.
	other := uuid.New()
	ts.afterTombstonePrecheck = func() {}
	_, err = ts.ProvisionTenant(ctx, ProvisionRequest{TenantID: other, OrgName: orgName, Creator: "svc-a"})
	ts.afterTombstonePrecheck = nil
	if err != nil {
		t.Errorf("CONTROL: a create under a fresh id: %v", err)
	}
}
