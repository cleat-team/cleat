package main

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/scheduledbackup"
	"github.com/google/uuid"
)

// TestBackupRefusesWithoutMigrationsAndNamesTheFix is cleat#2292 item 2's
// regression test. Before this, every `cleatctl backup` subcommand hit
// backup_config or backup_history directly, and a deployment that never ran
// scheduledbackup's migrations got the raw driver error (e.g.
// `pq: relation "backup_config" does not exist`) rather than a message
// naming the fix.
//
// Needs a dedicated scratch database, not the shared TestDB: the shared one
// already has scheduledbackup's tables from other tests in this package
// (TestBackupCommandWorksOnEveryDialect and others apply them), and
// requireBackupMigrations resolves the bare table name the way the
// connection would -- it cannot tell "this database never had the
// migration" from "some other test's database already does", so an absent
// table is a premise this test has to build itself. Same pattern as
// audit_test.go's TestAuditVerifyExitStatusesAreDistinct, one package over.
func TestBackupRefusesWithoutMigrationsAndNamesTheFix(t *testing.T) {
	adminDSN := os.Getenv("CLEAT_TEST_POSTGRES")
	if adminDSN == "" {
		adminDSN = os.Getenv("CLEAT_TEST_DB")
	}
	if adminDSN == "" {
		t.Skip("neither CLEAT_TEST_POSTGRES nor CLEAT_TEST_DB is set, skipping")
	}
	ctx := context.Background()
	admin, err := sql.Open("postgres", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("configured PostgreSQL is unreachable: %v", err)
	}
	scratch := "cleat_ctl2292_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+scratch); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	defer func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS " + scratch + " WITH (FORCE)") }()
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + scratch
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// scheduledbackup's v2 migration applies tenant scoping that calls
	// cleat.assert_tenant_set(), a core object -- without the core schema
	// in place first, RunMigrations below fails before it ever reaches the
	// premise this test is about.
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	// Premise: a brand-new database has no backup_config at all, so the
	// refusal below is about the migration check and not some other error.
	var n int
	if err := db.QueryRowContext(ctx,
		"SELECT CASE WHEN to_regclass('backup_config') IS NULL THEN 0 ELSE 1 END").Scan(&n); err != nil {
		t.Fatalf("checking premise: %v", err)
	}
	if n != 0 {
		t.Fatal("premise failed: the scratch database already has backup_config")
	}

	stdout, stderr := withExitPanicOutput(t, func() {
		runBackup(ctx, db, dialectPostgres, []string{"config-list"})
	})
	if !strings.Contains(stderr, "scheduledbackup plugin's migrations have not been applied") {
		t.Errorf("without migrations, stderr does not name the fix:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "cleat-worker --migrate-only") {
		t.Errorf("without migrations, stderr does not say how to fix it:\n%s", stderr)
	}
	if strings.Contains(stderr, "pq:") || strings.Contains(stderr, "relation") {
		t.Errorf("the raw driver error leaked through instead of the named fix:\n%s", stderr)
	}

	// Apply the migrations and confirm the SAME dispatch path now proceeds
	// to the subcommand instead of refusing.
	loaded := []*plugin.LoadedPlugin{{Plugin: scheduledbackup.New(), Healthy: true}}
	if err := plugin.RunMigrations(ctx, db, dialectPostgres.query, nil, loaded); err != nil {
		t.Fatalf("apply scheduledbackup migrations: %v", err)
	}
	stdout, stderr = withExitPanicOutput(t, func() {
		runBackup(ctx, db, dialectPostgres, []string{"config-list"})
	})
	if strings.Contains(stderr, "migrations have not been applied") {
		t.Errorf("after applying migrations, backup still refuses:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stdout, "no backup configs") {
		t.Errorf("after applying migrations, config-list did not run normally:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
}
