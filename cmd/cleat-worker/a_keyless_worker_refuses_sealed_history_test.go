package main

// cleat#2324: a pod deployed WITHOUT --encrypt-sensitive-payloads has no key
// ring, so the read path does not decrypt at all and treats sealed columns
// as data. Measured on a real worker (tests/crash, crashcall fixture): a
// worker started with no encryption flags, reading a run another worker
// sealed, ends FAILED by default (a checksum mismatch -- misleading, but
// closed) or DONE on ciphertext with --disable-checksum-verification.
//
// checkPayloadEncryptionState (setup.go) is the fix: a database remembers,
// via payload_encryption_ever_enabled, that some worker once had a key ring
// configured, and any worker that starts without one refuses outright. This
// runs the REAL main() -- in a subprocess, exactly as
// a_stale_route_signature_refuses_to_boot_test.go does for cleat#2277's
// boot refusal -- against a live postgres, reusing that file's TestMain
// interception and runBootSubprocess/replaceDBName/pgAppRoleDSN/freePort
// helpers rather than duplicating them.

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// writeRandomEncryptionKeyFile writes a fresh base64-encoded 256-bit AES key
// to a file under t.TempDir() and returns its path.
func writeRandomEncryptionKeyFile(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "payload-key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMainRefusesToStartWithoutAKeyOnceEncryptionHasEverBeenEnabled(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the real worker against a live database")
	}
	admin := os.Getenv("CLEAT_TEST_POSTGRES")
	if admin == "" {
		admin = os.Getenv("CLEAT_TEST_DB")
	}
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
	name := fmt.Sprintf("cleat_2324_keyless_refusal_%d", time.Now().UnixNano()%1_000_000_000)
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

	// Migration is a deploy step (cleat#2117): apply the schema (including
	// 008_payload_encryption_ever_enabled.sql) before any subprocess below.
	if code, out := runBootSubprocess(t, []string{"--driver=postgres", "--db=" + ownerDSN, "--migrate-only"}, ""); code != 0 {
		t.Fatalf("--migrate-only exited %d:\n%s", code, out)
	}

	owner, err := sql.Open("postgres", ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	dsn := pgAppRoleDSN(t, owner, ownerDSN)

	keyFile := writeRandomEncryptionKeyFile(t)

	// KNOWN-POSITIVE FIRST, same shape as the stale-route-signature test
	// above it in this package: a fresh database, no encryption ever
	// configured, booted with NO encryption flags at all must start serving
	// cleanly -- otherwise a refusal below could just as well be this test's
	// setup being broken (e.g. a migration that failed to grant cleat_app
	// access to the new table) rather than the mechanism cleat#2324 adds.
	t.Run("control: fresh database, no encryption ever configured, keyless worker starts serving", func(t *testing.T) {
		code, out := runBootRefusalSubprocess(t, dsn, ownerDSN, "")
		if code != -1 {
			t.Fatalf("keyless worker against a database that never had encryption configured exited %d "+
				"instead of being killed while serving -- something unrelated is refusing to start, which "+
				"means the assertion below would not be testing what it claims to:\n%s", code, out)
		}
	})

	// A worker WITH the key ring configured must start cleanly, and this
	// marks the database (checkPayloadEncryptionState's write side) for the
	// next subtest to observe.
	t.Run("a worker with --encrypt-sensitive-payloads marks the database and starts serving", func(t *testing.T) {
		args := []string{
			"--driver=postgres",
			"--db=" + dsn,
			"--migrate-db=" + ownerDSN,
			fmt.Sprintf("--api-addr=127.0.0.1:%d", freePort(t)),
			"--encrypt-sensitive-payloads",
			"--encryption-key-file=" + keyFile,
		}
		code, out := runBootSubprocess(t, args, "")
		if code != -1 {
			t.Fatalf("worker with --encrypt-sensitive-payloads exited %d instead of being killed while "+
				"serving -- it should start cleanly, marking the database as it goes:\n%s", code, out)
		}
	})

	// THE REGRESSION: the same database, now marked, with a worker that has
	// NO key ring at all -- it must refuse rather than silently proceed to
	// read sealed columns as plaintext.
	t.Run("a keyless worker refuses to start once encryption has ever been enabled on this database", func(t *testing.T) {
		code, out := runBootRefusalSubprocess(t, dsn, ownerDSN, "")
		if code == -1 {
			t.Fatal("keyless worker against a database that was marked as having encryption configured " +
				"started serving instead of refusing -- removing checkPayloadEncryptionState's call in " +
				"main.go, or its os.Exit(1), would produce exactly this")
		}
		if code == 0 {
			t.Fatalf("worker exited 0 against a database with encryption marked, want a fatal refusal "+
				"(non-zero):\n%s", out)
		}
		if !strings.Contains(out, "cleat#2324") {
			t.Errorf("refusal message does not cite cleat#2324, so a reader would not know why the worker "+
				"refused or where to read about it:\n%s", out)
		}
		if !strings.Contains(out, "encrypt-sensitive-payloads") {
			t.Errorf("refusal message does not name the --encrypt-sensitive-payloads flag, so a reader "+
				"would not know how to fix it:\n%s", out)
		}
	})
}
