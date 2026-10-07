package main

// cleat#2897's startup-refusal case, same shape and same reasoning as
// an_ambiguity_lookup_for_an_undeclared_op_refuses_to_boot_test.go's
// TestWorkerRefusesToBootWithAnUndeclaredAmbiguityLookupOp: proving
// validateIdempotencyKeyOps itself refuses is not the same as proving
// main() reaches an os.Exit(1) over that error rather than logging it and
// continuing to boot.

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func TestWorkerRefusesToBootWithAMisconfiguredIdempotencyKeyOp(t *testing.T) {
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
	name := fmt.Sprintf("cleat_2897_boot_refusal_%d", time.Now().UnixNano()%1_000_000_000)
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
			"--api-addr=127.0.0.1:0",
		}
	}

	// KNOWN-POSITIVE FIRST, same reasoning as the ambiguity-lookup test: a
	// VALID --idempotency-key-ops must boot and serve, or a refusal below
	// could just as well be this test's own setup being broken.
	t.Run("control: declared op, worker starts serving", func(t *testing.T) {
		args := append(baseArgs(),
			"--write-ahead-intent-ops=payment.charge",
			"--idempotency-key-ops=payment.charge")
		code, out := runBootSubprocess(t, args, "")
		if code != -1 {
			t.Fatalf("worker with a declared idempotency-key-ops entry exited %d instead of being "+
				"killed while serving -- something else refused to start:\n%s", code, out)
		}
	})

	t.Run("worker refuses to start with an undeclared idempotency-key-ops entry", func(t *testing.T) {
		args := append(baseArgs(), "--idempotency-key-ops=payment.charge")
		code, out := runBootSubprocess(t, args, "")
		if code == -1 {
			t.Fatal("worker with an undeclared idempotency-key-ops entry started serving instead of refusing")
		}
		if code == 42 {
			t.Fatalf("main() returned instead of exiting -- validateIdempotencyKeyOps's error must reach "+
				"an os.Exit(1):\n%s", out)
		}
		if code == 0 {
			t.Fatalf("worker exited 0, want a fatal refusal:\n%s", out)
		}
		if !strings.Contains(out, "payment.charge") || !strings.Contains(out, "write-ahead-intent-ops") {
			t.Errorf("refusal message does not name the offending operation or --write-ahead-intent-ops:\n%s", out)
		}
	})

	t.Run("worker refuses to start with an op declared for both mechanisms", func(t *testing.T) {
		args := append(baseArgs(),
			"--write-ahead-intent-ops=payment.charge",
			"--idempotency-key-ops=payment.charge",
			"--ambiguity-lookup=payment.charge=payment.get_by_key")
		code, out := runBootSubprocess(t, args, "")
		if code == -1 {
			t.Fatal("worker with payment.charge declared under BOTH --idempotency-key-ops and " +
				"--ambiguity-lookup started serving instead of refusing")
		}
		if code == 0 {
			t.Fatalf("worker exited 0, want a fatal refusal:\n%s", out)
		}
		if !strings.Contains(out, "payment.charge") {
			t.Errorf("refusal message does not name the conflicting operation:\n%s", out)
		}
	})
}
