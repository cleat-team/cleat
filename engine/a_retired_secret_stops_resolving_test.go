package engine

// TestARetiredSecretStopsResolvingAndSetSecretRevivesIt is the regression
// test for cleat#1989, on all three dialects: tenant_secrets.disabled_at has
// existed since migrations 081/069/073 and nothing set it or checked it, so a
// secret retired by hand kept resolving and there was no supported way to
// retire one at all.
//
// There was no existing database-backed test for PutSecret/GetSecret against
// a real tenant_secrets table on any dialect before this one -- the existing
// tests in tenant_secrets_test.go all exercise seal/open directly against a
// store built with a nil *sql.DB. This is the first to go through the actual
// SQL on all three, and it is what caught a second, pre-existing defect:
// tenant_secrets carries a security policy on SQL Server
// (migrations/mssql/073), and since migrations/mssql/075 removed the
// cleat_admin bypass from the shared predicate, NOTHING -- including a plain
// sa connection -- can read or write it without SESSION_CONTEXT('tenant_id')
// set to the row's own tenant first. A context with no tenant made PutSecret's
// INSERT succeed and every subsequent read, including this test's own
// GetSecret, see zero rows. tenantctx.With below is the fix, matching
// cmd/cleatctl/setsecret.go and cmd/cleatctl/retiresecret.go, both of which
// had the same gap on SQL Server before this issue.
import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/google/uuid"
)

func TestARetiredSecretStopsResolvingAndSetSecretRevivesIt(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			t.Cleanup(func() { db.Close() })
			testutil.SetupFullSchema(t, db, dialect)

			const name = "cleat-1989-retirement-check"
			ctx := tenantctx.With(context.Background(), uuid.MustParse(DefaultTenantUUID))
			master := testMaster(t)

			store, err := NewSecretStore(db, string(dialect), master)
			if err != nil {
				t.Fatalf("NewSecretStore: %v", err)
			}

			// Cleanup first (LIFO), so it runs LAST regardless of where this
			// test fails: a leftover row from a failed prior run must not
			// make PutSecret's insert-vs-update branch below run the wrong
			// arm for a reason unrelated to this test.
			t.Cleanup(func() {
				// ctx, not context.Background(): on SQL Server an unscoped
				// connection sees no rows to delete, so this would silently
				// leave the row behind for the RLS reason this whole file is
				// about, and the leftover would only surface as an unrelated
				// PRIMARY KEY violation the next time this test runs.
				db.ExecContext(ctx, //nolint:errcheck // best-effort cleanup
					deleteSecretStmtForTest(dialect), DefaultTenantUUID, name)
			})
			if _, err := db.ExecContext(ctx, deleteSecretStmtForTest(dialect), DefaultTenantUUID, name); err != nil {
				t.Fatalf("pre-test cleanup of a leftover row: %v", err)
			}

			if err := store.PutSecret(ctx, DefaultTenantUUID, name, "sk-live-original"); err != nil {
				t.Fatalf("PutSecret: %v", err)
			}
			got, err := store.GetSecret(ctx, DefaultTenantUUID, name)
			if err != nil {
				t.Fatalf("GetSecret before retiring: %v", err)
			}
			if got != "sk-live-original" {
				t.Fatalf("GetSecret before retiring: got %q, want %q", got, "sk-live-original")
			}

			exists, disabledAt, err := store.SecretMeta(ctx, DefaultTenantUUID, name)
			if err != nil {
				t.Fatalf("SecretMeta before retiring: %v", err)
			}
			if !exists {
				t.Fatalf("SecretMeta reports no row, but PutSecret/GetSecret both just succeeded against it")
			}
			if disabledAt.Valid {
				t.Fatalf("a freshly-set secret is already reported retired, at %v", disabledAt.Time)
			}

			// THE FIX, PART 1: retiring stops resolution.
			n, err := store.RetireSecret(ctx, DefaultTenantUUID, name)
			if err != nil {
				t.Fatalf("RetireSecret: %v", err)
			}
			if n != 1 {
				t.Fatalf("RetireSecret reported %d rows affected, want 1", n)
			}

			if _, err := store.GetSecret(ctx, DefaultTenantUUID, name); err != ErrSecretNotFound {
				t.Fatalf("GetSecret after retiring: got err=%v, want ErrSecretNotFound -- "+
					"a retired secret must fail resolution the same way a missing one does", err)
			}

			exists, disabledAt, err = store.SecretMeta(ctx, DefaultTenantUUID, name)
			if err != nil {
				t.Fatalf("SecretMeta after retiring: %v", err)
			}
			if !exists {
				t.Fatalf("SecretMeta reports no row after retiring -- retiring must not delete it, only set disabled_at")
			}
			if !disabledAt.Valid {
				t.Fatalf("SecretMeta reports disabled_at is NULL right after RetireSecret reported 1 row changed")
			}

			// Idempotent: retiring an already-retired secret changes nothing
			// and says so via rows-affected, rather than erroring or
			// silently bumping disabled_at to a new timestamp.
			n, err = store.RetireSecret(ctx, DefaultTenantUUID, name)
			if err != nil {
				t.Fatalf("RetireSecret (second call): %v", err)
			}
			if n != 0 {
				t.Fatalf("RetireSecret on an already-retired secret reported %d rows affected, want 0", n)
			}

			// THE FIX, PART 2: set-secret revives it. Not the same value --
			// deliberately, since a real rotation writes a new one, and this
			// also confirms PutSecret's UPDATE branch (name already exists)
			// is what ran, not a second INSERT racing the retired row's
			// primary key.
			if err := store.PutSecret(ctx, DefaultTenantUUID, name, "sk-live-rotated"); err != nil {
				t.Fatalf("PutSecret (revival): %v", err)
			}
			got, err = store.GetSecret(ctx, DefaultTenantUUID, name)
			if err != nil {
				t.Fatalf("GetSecret after revival: %v", err)
			}
			if got != "sk-live-rotated" {
				t.Fatalf("GetSecret after revival: got %q, want %q", got, "sk-live-rotated")
			}
			exists, disabledAt, err = store.SecretMeta(ctx, DefaultTenantUUID, name)
			if err != nil {
				t.Fatalf("SecretMeta after revival: %v", err)
			}
			if !exists {
				t.Fatalf("SecretMeta reports no row after revival")
			}
			if disabledAt.Valid {
				t.Fatalf("SecretMeta reports disabled_at is still set after set-secret revived it, at %v", disabledAt.Time)
			}
		})
	}
}

// deleteSecretStmtForTest is intentionally separate from anything in
// tenant_secrets.go: it exists only so this test can guarantee a clean slate
// before asserting on rows-affected counts, which is not a thing production
// code needs to do.
func deleteSecretStmtForTest(dialect testutil.Dialect) string {
	switch dialect {
	case testutil.DialectMySQL:
		return `DELETE FROM tenant_secrets WHERE tenant_id = ? AND name = ?`
	case testutil.DialectMSSQL:
		return `DELETE FROM tenant_secrets WHERE tenant_id = @p1 AND name = @p2`
	default:
		return `DELETE FROM tenant_secrets WHERE tenant_id = $1 AND name = $2`
	}
}
