package main

// set-deployment-secret, retire-deployment-secret and reseal-deployment-secrets
// (cleat#1992 part 1), driven the way an operator drives them: env and flags
// in, exit status and stdout out. engine/a_deployment_secret_store_db_test.go
// covers the store the three commands are built on; this covers what the
// commands do WITH it -- refusals, stdin/--from-file, and exit codes a script
// can trust -- which those tests cannot see, since they never go through flag
// parsing, the environment or os.Exit.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

func deploymentSecretCommandFixture(t *testing.T) *sql.DB {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM deployment_secrets WHERE name LIKE 'cleat-1992-ctl-%'`) //nolint:errcheck // best-effort cleanup
	})
	return db
}

// hasReportLine matches a printResealDeploymentSecrets line by its label and
// value, tolerant of the exact column width %-22s pads to -- which is a
// formatting detail this test should not have to track.
func hasReportLine(report, label, value string) bool {
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, label) && strings.TrimSpace(strings.TrimPrefix(line, label)) == value {
			return true
		}
	}
	return false
}

func writeValueFile(t *testing.T, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(p, []byte(value), 0o600); err != nil {
		t.Fatalf("write value file: %v", err)
	}
	return p
}

func TestSetDeploymentSecretRefusesWithNoMasterKey(t *testing.T) {
	setRingEnv(t, "", "", "", "")
	db := deploymentSecretCommandFixture(t)
	valueFile := writeValueFile(t, "sk-should-not-be-written")

	stdout, stderr, code := runCapturingExit(t, func() {
		runSetDeploymentSecret(context.Background(), db, dialectPostgres,
			[]string{"--name", "cleat-1992-ctl-no-key", "--from-file", valueFile})
	})
	if code != 1 {
		t.Fatalf("exit code: got %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "CLEAT_SECRET_MASTER_KEY") {
		t.Errorf("stderr does not mention the missing key: %s", stderr)
	}

	store := engine.NewDeploymentSecretStore(db, "postgres", nil)
	if _, err := store.GetDeploymentSecret(context.Background(), "cleat-1992-ctl-no-key"); err != engine.ErrDeploymentSecretNotFound {
		t.Fatalf("a refused set must write nothing: GetDeploymentSecret returned %v", err)
	}
}

func TestSetDeploymentSecretRefusesAnEmptyValue(t *testing.T) {
	setRingEnv(t, ringKeyB64(1), "", "", "")
	db := deploymentSecretCommandFixture(t)
	valueFile := writeValueFile(t, "")

	_, stderr, code := runCapturingExit(t, func() {
		runSetDeploymentSecret(context.Background(), db, dialectPostgres,
			[]string{"--name", "cleat-1992-ctl-empty", "--from-file", valueFile})
	})
	if code != 1 {
		t.Fatalf("exit code: got %d, want 1", code)
	}
	if !strings.Contains(stderr, "empty") {
		t.Errorf("stderr does not say the value was empty: %s", stderr)
	}
}

func TestSetAndRetireDeploymentSecretCommandsRoundTrip(t *testing.T) {
	setRingEnv(t, ringKeyB64(7), "", "", "")
	db := deploymentSecretCommandFixture(t)
	ctx := context.Background()
	const name = "cleat-1992-ctl-round-trip"
	valueFile := writeValueFile(t, "sk-command-level-value")

	stdout, stderr, code := runCapturingExit(t, func() {
		runSetDeploymentSecret(ctx, db, dialectPostgres, []string{"--name", name, "--from-file", valueFile})
	})
	if code != 0 {
		t.Fatalf("set-deployment-secret exited %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, name) {
		t.Errorf("set-deployment-secret's own report does not name the secret: %s", stdout)
	}

	ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: ringKeyRaw(7)})
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	store := engine.NewDeploymentSecretStore(db, "postgres", ring)
	got, err := store.GetDeploymentSecret(ctx, name)
	if err != nil || got != "sk-command-level-value" {
		t.Fatalf("GetDeploymentSecret after set-deployment-secret: got (%q, %v), want (%q, nil)", got, err, "sk-command-level-value")
	}

	// retire-deployment-secret needs no master key -- it touches disabled_at
	// only (retiredeploymentsecret.go's doc comment).
	setRingEnv(t, "", "", "", "")
	stdout, stderr, code = runCapturingExit(t, func() {
		runRetireDeploymentSecret(ctx, db, dialectPostgres, []string{"--name", name})
	})
	if code != 0 {
		t.Fatalf("retire-deployment-secret exited %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Retired") {
		t.Errorf("retire-deployment-secret's report does not say it retired the secret: %s", stdout)
	}
	if _, err := store.GetDeploymentSecret(ctx, name); err != engine.ErrDeploymentSecretNotFound {
		t.Fatalf("GetDeploymentSecret after retire: got %v, want ErrDeploymentSecretNotFound", err)
	}

	// A second retire is a no-op that still exits 0 and says so, mirroring
	// retire-secret's own "already retired" branch.
	stdout, _, code = runCapturingExit(t, func() {
		runRetireDeploymentSecret(ctx, db, dialectPostgres, []string{"--name", name})
	})
	if code != 0 {
		t.Fatalf("retire-deployment-secret (second time) exited %d, want 0", code)
	}
	if !strings.Contains(stdout, "already retired") {
		t.Errorf("second retire's report does not say the secret was already retired: %s", stdout)
	}

	// retire-deployment-secret against a name that was never set: exit 0,
	// informational, and it must not error just because there is nothing there.
	stdout, _, code = runCapturingExit(t, func() {
		runRetireDeploymentSecret(ctx, db, dialectPostgres, []string{"--name", "cleat-1992-ctl-never-set"})
	})
	if code != 0 {
		t.Fatalf("retire-deployment-secret on an unset name exited %d, want 0", code)
	}
	if !strings.Contains(stdout, "No deployment secret named") {
		t.Errorf("retire on an unset name does not say so: %s", stdout)
	}
}

func TestResealDeploymentSecretsCommandDryRunThenLive(t *testing.T) {
	setRingEnv(t, ringKeyB64(9), "", "", "")
	db := deploymentSecretCommandFixture(t)
	ctx := context.Background()
	const name = "cleat-1992-ctl-reseal"
	valueFile := writeValueFile(t, "sk-reseal-me")

	if _, _, code := runCapturingExit(t, func() {
		runSetDeploymentSecret(ctx, db, dialectPostgres, []string{"--name", name, "--from-file", valueFile})
	}); code != 0 {
		t.Fatalf("seeding set-deployment-secret failed, exit %d", code)
	}

	// Rotate: version 2 current, version 1 (the default the seed step above
	// used, since it set no explicit CLEAT_SECRET_MASTER_KEY_VERSION) previous,
	// same key bytes as the seed so the previous key actually opens the row.
	setRingEnv(t, ringKeyB64(2), "2", ringKeyB64(9), "1")

	stdout, stderr, code := runCapturingExit(t, func() {
		runResealDeploymentSecrets(ctx, db, dialectPostgres, []string{"--dry-run"})
	})
	// A dry run that found work to do exits non-zero (resealdeploymentsecrets.go:
	// "A dry run that found work is also non-zero"), which is what a script polls
	// on to decide whether a rotation is complete.
	if code != 1 {
		t.Fatalf("reseal-deployment-secrets --dry-run exited %d, want 1 (work found)\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !hasReportLine(stdout, "would reseal:", "1") {
		t.Errorf("dry run report does not show one row to reseal: %s", stdout)
	}
	if !hasReportLine(stdout, "unreadable:", "0") {
		t.Errorf("dry run report shows an unreadable row, want none: %s", stdout)
	}

	ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 2, Key: ringKeyRaw(2)})
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	store := engine.NewDeploymentSecretStore(db, "postgres", ring)
	if _, err := store.GetDeploymentSecret(ctx, name); err == nil {
		t.Fatal("dry run must write nothing: the row should still be unreadable under the new-only ring")
	}

	stdout, stderr, code = runCapturingExit(t, func() {
		runResealDeploymentSecrets(ctx, db, dialectPostgres, nil)
	})
	if code != 0 {
		t.Fatalf("reseal-deployment-secrets exited %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	got, err := store.GetDeploymentSecret(ctx, name)
	if err != nil || got != "sk-reseal-me" {
		t.Fatalf("GetDeploymentSecret after live reseal: got (%q, %v), want (%q, nil)", got, err, "sk-reseal-me")
	}
}
