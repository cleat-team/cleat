package main

// cleat#1984's acceptance section asks for "a startup-refusal test of the
// worker itself" (coordinator, cleat-review round 1) -- distinct from
// TestValidateAmbiguityLookupOps, which proves validateAmbiguityLookupOps
// itself returns an error for an undeclared operation but never proves
// main() reaches an os.Exit(1) over that error rather than logging it and
// continuing to boot. Same shape as
// a_stale_route_signature_refuses_to_boot_test.go's
// TestMainRefusesToStartOnAStaleRouteSignature, reusing its TestMain
// interception and runBootSubprocess/replaceDBName helpers rather than
// duplicating them -- only one TestMain can exist per package.
//
// --ambiguity-lookup's validation (main.go, right after
// --service-endpoints's) runs only after the worker has successfully opened
// its database connection pool -- confirmed by running the compiled binary
// by hand with an unreachable DSN, which refuses earlier ("failed to open
// heartbeat connection pool") and never reaches this check at all. So this
// needs a real, migrated database, the same as the route-signature test.

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func TestWorkerRefusesToBootWithAnUndeclaredAmbiguityLookupOp(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the real worker against a live database")
	}
	admin := dbTestAdminDSN(t)
	if admin == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}

	adb, err := sql.Open("postgres", admin)
	if err != nil {
		t.Fatal(err)
	}
	defer adb.Close()
	if err := adb.Ping(); err != nil {
		t.Fatalf("%s is set but unreachable: %v", admin, err)
	}
	name := fmt.Sprintf("cleat_1984_boot_refusal_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := adb.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("postgres", admin)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = a.Exec(`DROP DATABASE IF EXISTS ` + name)
	})
	ownerDSN := replaceDBName(t, admin, name)

	// Migration is a deploy step (cleat#2117): apply it first, the same way
	// a_stale_route_signature_refuses_to_boot_test.go does, or every
	// subprocess below refuses on "the database has no schema_migrations
	// table" before ever reaching the ambiguity-lookup validation.
	if code, out := runBootSubprocess(t, []string{"--driver=postgres", "--db=" + ownerDSN, "--migrate-only"}, ""); code != 0 {
		t.Fatalf("--migrate-only exited %d:\n%s", code, out)
	}

	owner, err := sql.Open("postgres", ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	dsn := pgAppRoleDSN(t, owner, ownerDSN)

	baseArgs := func() []string {
		return []string{
			"--driver=postgres",
			"--db=" + dsn,
			"--migrate-db=" + ownerDSN,
			fmt.Sprintf("--api-addr=127.0.0.1:%d", freePort(t)),
		}
	}

	// KNOWN-POSITIVE FIRST: the same subprocess, same fresh database, with a
	// VALID --ambiguity-lookup (its operation IS declared
	// --write-ahead-intent-ops) must boot and serve -- otherwise a refusal
	// below could just as well be this test's DSN/migration setup being
	// broken, not the validation under test.
	t.Run("control: declared op, worker starts serving", func(t *testing.T) {
		args := append(baseArgs(),
			"--write-ahead-intent-ops=payment.charge",
			"--ambiguity-lookup=payment.charge=payment.get_by_key")
		code, out := runBootSubprocess(t, args, "")
		if code != -1 {
			t.Fatalf("worker with a declared ambiguity-lookup op exited %d instead of being killed while "+
				"serving -- something else refused to start, which means the assertion below would not be "+
				"testing what it claims to:\n%s", code, out)
		}
	})

	t.Run("worker refuses to start with an undeclared ambiguity-lookup op", func(t *testing.T) {
		args := append(baseArgs(),
			"--ambiguity-lookup=payment.charge=payment.get_by_key")
		code, out := runBootSubprocess(t, args, "")
		if code == -1 {
			t.Fatal("worker with an undeclared ambiguity-lookup op started serving instead of refusing -- " +
				"removing the os.Exit(1) after validateAmbiguityLookupOps in main.go would produce exactly " +
				"this")
		}
		if code == 42 {
			t.Fatalf("main() returned instead of exiting -- validateAmbiguityLookupOps's error must reach "+
				"an os.Exit(1), not merely be logged:\n%s", out)
		}
		if code == 0 {
			t.Fatalf("worker exited 0 with an undeclared ambiguity-lookup op, want a fatal refusal "+
				"(non-zero):\n%s", out)
		}
		if !strings.Contains(out, "payment.charge") {
			t.Errorf("refusal message does not name the offending operation %q:\n%s", "payment.charge", out)
		}
		if !strings.Contains(out, "write-ahead-intent-ops") {
			t.Errorf("refusal message does not mention --write-ahead-intent-ops, so a reader would not know "+
				"what to fix:\n%s", out)
		}
	})
}

// dbTestAdminDSN returns CLEAT_TEST_POSTGRES, falling back to CLEAT_TEST_DB
// -- the same pair a_stale_route_signature_refuses_to_boot_test.go reads
// directly; factored out here only because this file needs the same lookup
// twice as cheaply as that file needs it once.
func dbTestAdminDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("CLEAT_TEST_POSTGRES"); v != "" {
		return v
	}
	return os.Getenv("CLEAT_TEST_DB")
}
