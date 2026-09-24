package engine

// DeploymentSecretStore against a real database (cleat#1992 part 1). The
// crypto-only tests in a_deployment_secret_domain_is_separate_test.go prove
// seal/open; these prove the SQL each dialect's statement-builder function
// returns actually runs -- column names, placeholder numbering, the
// UPDATE-then-INSERT upsert, and disabled_at's effect on a read -- none of
// which a build or a vet catches.

import (
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

func deploymentStoreForTest(t *testing.T, dialect testutil.Dialect) (*DeploymentSecretStore, *sql.DB) {
	t.Helper()
	db := testutil.TestDB(t, dialect)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, dialect)
	ring := testDeploymentMasterKey(t)
	return NewDeploymentSecretStore(db, string(dialect), ring), db
}

func TestDeploymentSecretStorePutGetRetireRoundTrip(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			s, db := deploymentStoreForTest(t, dialect)
			ctx := t.Context()
			const name = "cleat-1992-round-trip.api_key"

			// ResealDeploymentSecrets (a_deployment_secret_store_db_test.go's
			// other test) scans the WHOLE table unconditionally, by design --
			// there is no tenant to scope it by. A row left behind here, sealed
			// under a ring only this test knows, would show up there as
			// unreadable and fail a test this one never runs alongside on
			// purpose. Delete it regardless of which assertion below fails.
			deleteDeploymentSecretRow := map[testutil.Dialect]string{
				testutil.DialectPostgres: `DELETE FROM deployment_secrets WHERE name = $1`,
				testutil.DialectMySQL:    `DELETE FROM deployment_secrets WHERE name = ?`,
				testutil.DialectMSSQL:    `DELETE FROM deployment_secrets WHERE name = @p1`,
			}[dialect]
			t.Cleanup(func() {
				db.Exec(deleteDeploymentSecretRow, name) //nolint:errcheck // best-effort cleanup
			})

			if _, err := s.GetDeploymentSecret(ctx, name); err != ErrDeploymentSecretNotFound {
				t.Fatalf("GetDeploymentSecret before any write: got %v, want ErrDeploymentSecretNotFound", err)
			}

			// INSERT arm of the upsert.
			if err := s.PutDeploymentSecret(ctx, name, "first-value"); err != nil {
				t.Fatalf("PutDeploymentSecret (insert): %v", err)
			}
			got, err := s.GetDeploymentSecret(ctx, name)
			if err != nil {
				t.Fatalf("GetDeploymentSecret after insert: %v", err)
			}
			if got != "first-value" {
				t.Fatalf("GetDeploymentSecret after insert: got %q, want %q", got, "first-value")
			}

			exists, disabledAt, err := s.DeploymentSecretMeta(ctx, name)
			if err != nil {
				t.Fatalf("DeploymentSecretMeta: %v", err)
			}
			if !exists || disabledAt.Valid {
				t.Fatalf("DeploymentSecretMeta after insert: exists=%v disabledAt.Valid=%v, want true/false", exists, disabledAt.Valid)
			}

			// UPDATE arm of the upsert -- same name, a second value.
			if err := s.PutDeploymentSecret(ctx, name, "second-value"); err != nil {
				t.Fatalf("PutDeploymentSecret (update): %v", err)
			}
			got, err = s.GetDeploymentSecret(ctx, name)
			if err != nil {
				t.Fatalf("GetDeploymentSecret after update: %v", err)
			}
			if got != "second-value" {
				t.Fatalf("GetDeploymentSecret after update: got %q, want %q", got, "second-value")
			}

			// Retire: the row stops resolving, exactly like a name never set,
			// without losing the ciphertext (RetireDeploymentSecret only
			// touches disabled_at -- verified by re-setting below).
			n, err := s.RetireDeploymentSecret(ctx, name)
			if err != nil {
				t.Fatalf("RetireDeploymentSecret: %v", err)
			}
			if n != 1 {
				t.Fatalf("RetireDeploymentSecret: %d rows affected, want 1", n)
			}
			if _, err := s.GetDeploymentSecret(ctx, name); err != ErrDeploymentSecretNotFound {
				t.Fatalf("GetDeploymentSecret after retire: got %v, want ErrDeploymentSecretNotFound", err)
			}
			exists, disabledAt, err = s.DeploymentSecretMeta(ctx, name)
			if err != nil {
				t.Fatalf("DeploymentSecretMeta after retire: %v", err)
			}
			if !exists || !disabledAt.Valid {
				t.Fatalf("DeploymentSecretMeta after retire: exists=%v disabledAt.Valid=%v, want true/true", exists, disabledAt.Valid)
			}

			// A second retire affects zero rows rather than erroring --
			// retire-deployment-secret's "already retired" branch depends on
			// this via DeploymentSecretMeta, not on RetireDeploymentSecret's
			// own return value, but the store's contract is exercised here.
			n, err = s.RetireDeploymentSecret(ctx, name)
			if err != nil {
				t.Fatalf("RetireDeploymentSecret (second time): %v", err)
			}
			if n != 0 {
				t.Fatalf("RetireDeploymentSecret (second time): %d rows affected, want 0", n)
			}

			// set-deployment-secret's documented revival path: writing again
			// clears disabled_at (the same clause putDeploymentSecretUpdateStmt
			// carries for exactly this).
			if err := s.PutDeploymentSecret(ctx, name, "revived-value"); err != nil {
				t.Fatalf("PutDeploymentSecret (revive): %v", err)
			}
			got, err = s.GetDeploymentSecret(ctx, name)
			if err != nil {
				t.Fatalf("GetDeploymentSecret after revive: %v", err)
			}
			if got != "revived-value" {
				t.Fatalf("GetDeploymentSecret after revive: got %q, want %q", got, "revived-value")
			}
		})
	}
}

func TestResealDeploymentSecretsConvergesAndPreservesPlaintext(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	cur := VersionedKey{Version: 2, Key: make32ByteKey("reseal-current")}
	prev := VersionedKey{Version: 1, Key: make32ByteKey("reseal-previous")}
	ring, err := NewKeyRing(cur, prev)
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	oldRing, err := NewKeyRing(prev)
	if err != nil {
		t.Fatalf("NewKeyRing (previous only): %v", err)
	}

	s := NewDeploymentSecretStore(db, string(testutil.DialectPostgres), oldRing)
	ctx := t.Context()
	const name = "cleat-1992-reseal.api_key"
	if err := s.PutDeploymentSecret(ctx, name, "resealed-value"); err != nil {
		t.Fatalf("seed under the previous key: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM deployment_secrets WHERE name = $1`, name) //nolint:errcheck // best-effort cleanup
	})

	rotated := NewDeploymentSecretStore(db, string(testutil.DialectPostgres), ring)

	// Converged() means "a second run would find nothing to do" (no CAS
	// conflicts, no unreadable rows) -- it does NOT mean "nothing needs
	// resealing", so a dry run reporting one row to reseal and zero anomalies
	// is Converged() == true. That is the pre-existing contract
	// DeploymentSecretReseal mirrors from SecretReseal, not something this
	// test redefines.
	dry, err := rotated.ResealDeploymentSecrets(ctx, true)
	if err != nil {
		t.Fatalf("ResealDeploymentSecrets (dry run): %v", err)
	}
	if dry.Resealed != 1 || dry.Changed != 0 || !dry.Converged() {
		t.Fatalf("dry run: Resealed=%d Changed=%d Converged=%v, want 1/0/true", dry.Resealed, dry.Changed, dry.Converged())
	}
	got, err := rotated.GetDeploymentSecret(ctx, name)
	if err != nil || got != "resealed-value" {
		t.Fatalf("GetDeploymentSecret after dry run: got (%q, %v), want (%q, nil) -- dry run must write nothing", got, err, "resealed-value")
	}

	live, err := rotated.ResealDeploymentSecrets(ctx, false)
	if err != nil {
		t.Fatalf("ResealDeploymentSecrets (live): %v", err)
	}
	if live.Resealed != 1 || !live.Converged() {
		t.Fatalf("live run: Resealed=%d Converged=%v, want 1/true", live.Resealed, live.Converged())
	}
	got, err = rotated.GetDeploymentSecret(ctx, name)
	if err != nil || got != "resealed-value" {
		t.Fatalf("GetDeploymentSecret after live reseal: got (%q, %v), want (%q, nil) -- plaintext must round-trip", got, err, "resealed-value")
	}

	// A second run has nothing left to do -- the convergence property a
	// rotation loop relies on to know it can stop.
	again, err := rotated.ResealDeploymentSecrets(ctx, false)
	if err != nil {
		t.Fatalf("ResealDeploymentSecrets (second run): %v", err)
	}
	if again.Resealed != 0 || again.Current != 1 || !again.Converged() {
		t.Fatalf("second run: Resealed=%d Current=%d Converged=%v, want 0/1/true", again.Resealed, again.Current, again.Converged())
	}
}
