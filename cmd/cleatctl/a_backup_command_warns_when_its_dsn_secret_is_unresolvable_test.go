package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/scheduledbackup"
	"github.com/google/uuid"
)

// TestBackupConfigCreateWarnsWhenDSNSecretIsUnresolvable is cleat#2246 item
// 1, retargeted from the tenant-facing HTTP 503 that item originally asked
// for onto the operator CLI that replaced it (#2290 deleted the HTTP API
// entirely). config-create, config-update --enabled and run all warn to
// stderr, rather than succeeding silently, when scheduledbackup.dsn is not
// set or has been retired -- both shapes GetDeploymentSecret's own query
// treats as "not found" (WHERE ... AND disabled_at IS NULL).
//
// Drives config-create against real PostgreSQL, since the property under
// test is what warnIfBackupDSNUnresolvable's query finds in a real
// deployment_secrets table, not just what the Go code does with a fake.
func TestBackupConfigCreateWarnsWhenDSNSecretIsUnresolvable(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping a real database test in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ctx := context.Background()

	loaded := []*plugin.LoadedPlugin{{Plugin: scheduledbackup.New(), Healthy: true}}
	if err := plugin.RunMigrations(ctx, db, dialectPostgres.query, nil, loaded); err != nil {
		t.Fatalf("apply scheduledbackup migrations: %v", err)
	}
	t.Cleanup(func() { cleanBackupDSNSecret(t, db) })
	cleanBackupDSNSecret(t, db) // deployment_secrets is a global table, not tenant-scoped or per-test

	name := "warn-" + strings.ReplaceAll(uuid.New().String(), "-", "")[:12]

	// Known-positive: no secret at all. This is the exact scenario item 1
	// names -- an operator creating a config on a deployment that has never
	// set scheduledbackup.dsn.
	_, stderr := withExitPanicOutput(t, func() {
		runBackupConfigCreate(ctx, db, dialectPostgres, []string{
			"--name", name, "--cron", "0 0 * * *",
		})
	})
	if !strings.Contains(stderr, "scheduledbackup.dsn is not set") {
		t.Fatalf("config-create with no secret set: stderr does not warn about it:\n%s", stderr)
	}

	// Set the secret for real, through the same store the plugin itself
	// reads from at runtime -- not a fake, so this exercises the actual
	// DeploymentSecretMeta query against the row PutDeploymentSecret writes.
	ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: ringKeyRaw(0x42)})
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	store := engine.NewDeploymentSecretStore(db, "postgres", ring)
	if err := store.PutDeploymentSecret(ctx, "scheduledbackup.dsn", "postgres://backup-target"); err != nil {
		t.Fatalf("PutDeploymentSecret: %v", err)
	}

	// Negative control: the same config, same operation, now that the
	// secret resolves. If this warned too, the check above would be
	// unconditional and prove nothing about the secret's actual state.
	name2 := "warn2-" + strings.ReplaceAll(uuid.New().String(), "-", "")[:12]
	_, stderr = withExitPanicOutput(t, func() {
		runBackupConfigCreate(ctx, db, dialectPostgres, []string{
			"--name", name2, "--cron", "0 0 * * *",
		})
	})
	if strings.Contains(stderr, "scheduledbackup.dsn") {
		t.Fatalf("config-create with the secret set still warned about it:\n%s", stderr)
	}

	// Retiring the secret must warn again: GetDeploymentSecret's own query
	// filters disabled_at IS NULL, so a retired secret and a missing one are
	// the same failure from backupDSN's point of view, and the check has to
	// treat them the same way.
	if _, err := store.RetireDeploymentSecret(ctx, "scheduledbackup.dsn"); err != nil {
		t.Fatalf("RetireDeploymentSecret: %v", err)
	}
	name3 := "warn3-" + strings.ReplaceAll(uuid.New().String(), "-", "")[:12]
	_, stderr = withExitPanicOutput(t, func() {
		runBackupConfigCreate(ctx, db, dialectPostgres, []string{
			"--name", name3, "--cron", "0 0 * * *",
		})
	})
	if !strings.Contains(stderr, "was retired") {
		t.Fatalf("config-create with a retired secret: stderr does not say so:\n%s", stderr)
	}
}

// TestBackupConfigUpdateOnlyWarnsWhenEnabling confirms config-update's DSN
// check is conditional on --enabled: a config-update that does not touch
// enablement (or that disables) has no reason to warn about a resource the
// config will not use.
func TestBackupConfigUpdateOnlyWarnsWhenEnabling(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping a real database test in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ctx := context.Background()

	loaded := []*plugin.LoadedPlugin{{Plugin: scheduledbackup.New(), Healthy: true}}
	if err := plugin.RunMigrations(ctx, db, dialectPostgres.query, nil, loaded); err != nil {
		t.Fatalf("apply scheduledbackup migrations: %v", err)
	}
	t.Cleanup(func() { cleanBackupDSNSecret(t, db) })
	cleanBackupDSNSecret(t, db)

	name := "upd-" + strings.ReplaceAll(uuid.New().String(), "-", "")[:12]
	withExitPanicOutput(t, func() {
		runBackupConfigCreate(ctx, db, dialectPostgres, []string{
			"--name", name, "--cron", "0 0 * * *", "--disabled",
		})
	})
	id, err := resolveConfigID(ctx, db, dialectPostgres, "", name)
	if err != nil {
		t.Fatalf("resolveConfigID: %v", err)
	}

	// Known-positive: no secret, but only changing the cron -- must NOT warn.
	_, stderr := withExitPanicOutput(t, func() {
		runBackupConfigUpdate(ctx, db, dialectPostgres, []string{"--id", id.String(), "--cron", "0 12 * * *"})
	})
	if strings.Contains(stderr, "scheduledbackup.dsn") {
		t.Fatalf("config-update with no --enabled warned about the DSN secret anyway:\n%s", stderr)
	}

	// Now enable it, still with no secret set -- this must warn.
	_, stderr = withExitPanicOutput(t, func() {
		runBackupConfigUpdate(ctx, db, dialectPostgres, []string{"--id", id.String(), "--enabled"})
	})
	if !strings.Contains(stderr, "scheduledbackup.dsn is not set") {
		t.Fatalf("config-update --enabled with no secret set did not warn:\n%s", stderr)
	}
}

// TestBackupRunWarnsWhenDSNSecretIsUnresolvable covers the third call site:
// requesting an immediate run warns if the secret it will need within 60s
// is not resolvable, since by the time the background loop tries to use it
// there is nowhere left to report the problem to.
//
// The config here must be ENABLED, unlike this file's other two tests --
// cleat#2292 item 3 gave "run" its own --enabled gate, which refuses
// outright and never reaches this warning at all. That refusal has its own
// test, TestBackupRunRefusesADisabledConfig (backup_test.go); this one is
// about the DSN check specifically, so a disabled config here would prove
// nothing about it.
func TestBackupRunWarnsWhenDSNSecretIsUnresolvable(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping a real database test in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ctx := context.Background()

	loaded := []*plugin.LoadedPlugin{{Plugin: scheduledbackup.New(), Healthy: true}}
	if err := plugin.RunMigrations(ctx, db, dialectPostgres.query, nil, loaded); err != nil {
		t.Fatalf("apply scheduledbackup migrations: %v", err)
	}
	t.Cleanup(func() { cleanBackupDSNSecret(t, db) })
	cleanBackupDSNSecret(t, db)

	name := "run-" + strings.ReplaceAll(uuid.New().String(), "-", "")[:12]
	withExitPanicOutput(t, func() {
		runBackupConfigCreate(ctx, db, dialectPostgres, []string{
			"--name", name, "--cron", "0 0 * * *",
		})
	})
	id, err := resolveConfigID(ctx, db, dialectPostgres, "", name)
	if err != nil {
		t.Fatalf("resolveConfigID: %v", err)
	}

	// Known-positive: no secret set.
	_, stderr := withExitPanicOutput(t, func() {
		runBackupRun(ctx, db, dialectPostgres, []string{"--id", id.String()})
	})
	if !strings.Contains(stderr, "scheduledbackup.dsn is not set") {
		t.Fatalf("backup run with no secret set did not warn:\n%s", stderr)
	}

	// Negative control: set the secret, request run again, must not warn.
	ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: ringKeyRaw(0x42)})
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	store := engine.NewDeploymentSecretStore(db, "postgres", ring)
	if err := store.PutDeploymentSecret(ctx, "scheduledbackup.dsn", "postgres://backup-target"); err != nil {
		t.Fatalf("PutDeploymentSecret: %v", err)
	}
	_, stderr = withExitPanicOutput(t, func() {
		runBackupRun(ctx, db, dialectPostgres, []string{"--id", id.String()})
	})
	if strings.Contains(stderr, "scheduledbackup.dsn") {
		t.Fatalf("backup run with the secret set still warned about it:\n%s", stderr)
	}
}

// cleanBackupDSNSecret deletes the scheduledbackup.dsn row outright.
// deployment_secrets carries no tenant_id and is not reset between tests the
// way tenant-scoped tables are (testutil.CleanupPostgresTestData's DELETEs
// are keyed on tenant-owned tables) -- and RetireDeploymentSecret only sets
// disabled_at, it cannot restore "never set", which is the state these
// tests need to start from. Run before AND after each test: before, so a
// row a previous test (or a previous run of this one) left behind cannot be
// mistaken for this test's own "not set" case; after, so the next test
// starts clean regardless of which assertion in this one failed first.
func cleanBackupDSNSecret(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`DELETE FROM deployment_secrets WHERE name = $1`, "scheduledbackup.dsn"); err != nil {
		t.Fatalf("cleaning up scheduledbackup.dsn: %v", err)
	}
}
