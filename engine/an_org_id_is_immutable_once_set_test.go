package engine

// TestAnOrgIDIsImmutableOnceSet is the regression test for cleat#1898's
// trigger, on all three dialects.
//
// The obvious falsification -- move a tenant to a random UUID that is not a
// real org -- is void on SQL Server: admin.tenants.org_id carries a foreign
// key to admin.orgs, and ALTER COLUMN's constraint check runs ahead of the
// AFTER trigger, so a nonexistent target org fails with
// "The UPDATE statement conflicted with the FOREIGN KEY constraint", never
// reaching trg_tenants_org_id_immutable at all (measured manually before
// writing this test). That is a real refusal, but not the one this file is
// about -- CLAUDE.md's "a control in a different row cannot say why" applies
// here to a single query, not a row: a green here would agree with both "the
// trigger fired" and "the trigger does not exist, the FK did the work",
// which is exactly the ambiguity a falsification must not have. So the
// target of the move is always a SECOND, REAL org, which the FK is
// satisfied by -- isolating the trigger as the only thing that can still
// object.
import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// orgImmutabilityStmts returns the three statements TestAnOrgIDIsImmutableOnceSet
// needs, per dialect: ensuring a second org exists, moving a tenant to it,
// and an unrelated column update to use as the negative control. A function
// so the placeholder and table-qualification differences (the same three
// ways createAPIKeyStmt in auth/tenant_store.go differs) are asserted in one
// place rather than three copies of this test.
func orgImmutabilityStmts(dialect testutil.Dialect) (ensureOrg, moveOrg, setSuspended string) {
	switch dialect {
	case testutil.DialectMySQL:
		return `INSERT INTO orgs (org_id, name) VALUES (?, ?) ON DUPLICATE KEY UPDATE org_id = org_id`,
			`UPDATE tenants SET org_id = ? WHERE tenant_id = ?`,
			`UPDATE tenants SET suspended = ? WHERE tenant_id = ?`
	case testutil.DialectMSSQL:
		return `IF NOT EXISTS (SELECT 1 FROM admin.orgs WHERE org_id = @p1) INSERT INTO admin.orgs (org_id, name) VALUES (@p1, @p2)`,
			`UPDATE admin.tenants SET org_id = @p1 WHERE tenant_id = @p2`,
			`UPDATE admin.tenants SET suspended = @p1 WHERE tenant_id = @p2`
	default:
		return `INSERT INTO admin.orgs (org_id, name) VALUES ($1, $2) ON CONFLICT (org_id) DO NOTHING`,
			`UPDATE admin.tenants SET org_id = $1 WHERE tenant_id = $2`,
			`UPDATE admin.tenants SET suspended = $1 WHERE tenant_id = $2`
	}
}

// selectOrgIDStmt reads a tenant's current org_id, so the cleanup below can
// restore whatever it actually was rather than assuming the default org.
func selectOrgIDStmt(dialect testutil.Dialect) string {
	switch dialect {
	case testutil.DialectMySQL:
		return `SELECT org_id FROM tenants WHERE tenant_id = ?`
	case testutil.DialectMSSQL:
		// CONVERT, not a bare column: SQL Server returns UNIQUEIDENTIFIER in a
		// byte order Go's database/sql does not scan into a string directly --
		// the same reason auth/tenant_store.go's resolveAPIKeyStmt converts it
		// for MSSQL. Scanning it raw here read back "" with no error, so the
		// cleanup below "restored" org_id to an empty string instead of the
		// original value -- caught only by re-querying the container after a
		// falsification, not by any error return.
		return `SELECT CONVERT(NVARCHAR(36), org_id) FROM admin.tenants WHERE tenant_id = @p1`
	default:
		return `SELECT org_id FROM admin.tenants WHERE tenant_id = $1`
	}
}

func TestAnOrgIDIsImmutableOnceSet(t *testing.T) {
	const secondOrgID = "11111111-1111-1111-1111-111111111111"

	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			// t.Cleanup, not defer: registered before the org_id-restore
			// cleanup below, so it runs AFTER it (Cleanup is LIFO). A plain
			// defer here closed db before that cleanup ran, so the "repair"
			// silently no-op'd against a closed connection -- caught only by
			// checking the container's actual row after a deliberate
			// falsification, not by the test going red for the right reason.
			t.Cleanup(func() { db.Close() })
			testutil.SetupFullSchema(t, db, dialect)

			ctx := context.Background()
			ensureOrg, moveOrg, setSuspended := orgImmutabilityStmts(dialect)

			// A real org, not a random UUID -- see the file comment. This is
			// the discriminator: the FK is satisfied, so only the trigger can
			// still refuse.
			if _, err := db.ExecContext(ctx, ensureOrg, secondOrgID, "cleat#1898-immutability-check"); err != nil {
				t.Fatalf("ensure second org exists: %v", err)
			}

			// The default tenant, backfilled to the default org by migration
			// 090 (postgres) / 078 (mysql) / 082 (mssql). MySQL permits only
			// one tenant row ever (tiers.yaml's singleton constraint), so this
			// test uses the row every dialect already has rather than
			// inserting its own -- which means a REGRESSION here (the trigger
			// missing) does not just fail this test, it actually moves that
			// shared row's org_id, corrupting every later test in the same
			// run that assumes it. Restore it via t.Cleanup, unconditionally
			// and best-effort: on a working trigger this UPDATE is a no-op
			// (org_id already equals originalOrgID, so OLD = NEW and the
			// trigger has nothing to object to); on a broken one it repairs
			// what the failing assertion above just did. Read the original
			// value rather than assuming it is the default org, so this does
			// not itself depend on the migration having backfilled correctly.
			var originalOrgID string
			if err := db.QueryRowContext(ctx, selectOrgIDStmt(dialect), DefaultTenantUUID).Scan(&originalOrgID); err != nil {
				t.Fatalf("read the default tenant's current org_id: %v", err)
			}
			t.Cleanup(func() {
				db.ExecContext(context.Background(), moveOrg, originalOrgID, DefaultTenantUUID) //nolint:errcheck // best-effort repair; see comment above
			})

			if _, err := db.ExecContext(ctx, moveOrg, secondOrgID, DefaultTenantUUID); err == nil {
				t.Fatalf("moving the default tenant to a second, REAL org did not fail -- " +
					"org_id must be refused by the database, not merely undocumented as mutable")
			} else if !strings.Contains(err.Error(), "org_id is immutable and cannot be changed") {
				t.Fatalf("the move failed, but not with the trigger's own message -- "+
					"got %v, which means something else (a constraint, a type error) is refusing "+
					"this instead of admin.tenants_org_id_is_immutable / tenants_org_id_immutable / "+
					"trg_tenants_org_id_immutable", err)
			}

			// Negative control: an unrelated column must still be writable.
			// Without this, a version of the trigger that refuses every
			// UPDATE unconditionally would pass the assertion above too.
			if _, err := db.ExecContext(ctx, setSuspended, true, DefaultTenantUUID); err != nil {
				t.Fatalf("an unrelated column update was refused too -- the trigger is firing on "+
					"a benign update, not only on an org_id change: %v", err)
			}
			// Restore it: this is the one tenant row MySQL's schema permits to
			// exist at all, and other tests in this package read it.
			if _, err := db.ExecContext(ctx, setSuspended, false, DefaultTenantUUID); err != nil {
				t.Fatalf("could not restore suspended=false on the default tenant: %v", err)
			}
		})
	}
}
