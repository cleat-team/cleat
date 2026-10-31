package main

// cleat#3638. admin.tenant_tombstones is the one record of where a dropped tenant's own audit chain
// ended; the chain itself is deleted with the tenant, so this row is what lets an earlier copy of
// that chain still be checked. It is write-once by design -- 007 says "a tombstone is written once
// and deleted by an operator or not at all" -- but the application role held UPDATE on it anyway,
// because 001's `ALTER DEFAULT PRIVILEGES IN SCHEMA admin` applies when a table is CREATED and 007's
// own GRANT did not take UPDATE away. 011 (PostgreSQL) and 014 (SQL Server) are the repair.
//
// This runs as the login the worker actually uses -- cleat_app on PostgreSQL, and the cleat_app login
// inside cleat_app_role on SQL Server -- not as the owner. The owner DSN can do anything, which is
// the shape cleat#3634 found: every local run used the owner DSN and a privilege the real login
// lacked went unseen until it did not. a_tombstone_is_deleted_as_the_app_login_test.go makes the same
// point for DELETE and this file is its sibling.
//
// FOUR THINGS ARE ASSERTED, and the middle two are what make the first one mean anything:
//
//  1. the app login's UPDATE is REFUSED, and refused with a permission error rather than some other
//     failure that happens to be an error;
//  2. NEGATIVE CONTROL -- take the REVOKE/DENY away as the owner and the SAME statement succeeds.
//     Without this, a pass says only "an UPDATE failed", which a typo, a missing table or a
//     connection that cannot write anything at all would also produce;
//  3. the restriction is put BACK and the UPDATE is refused again, so a restore that did not restore
//     cannot be mistaken for a fix that works (CLAUDE.md: applying a revert is loud, restoring it is
//     silent);
//  4. SELECT, INSERT and DELETE still work for the app login, so this change is a narrowing of one
//     privilege and not a revocation of the table. The drop itself INSERTs a tombstone and the
//     operator's route DELETEs one, through this same login.

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine/testutil"
)

// rewrittenChainHash is a valid char(64) that differs from chainHash in every character position that
// matters, so "the row did not change" cannot be satisfied by a write that stored the same bytes.
const rewrittenChainHash = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func TestTheAppLoginCannotRewriteATombstone(t *testing.T) {
	for _, dialect := range []string{auth.DialectPostgres, auth.DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			owner := testutil.TestDB(t, testutil.Dialect(dialect))
			ctx := context.Background()

			// lift takes the restriction away and restore puts it back. They are stated per dialect
			// rather than as one "grant" because the two privilege models are different things:
			// PostgreSQL grants UPDATE back, SQL Server removes a DENY that was sitting on top of a
			// schema-level GRANT. Writing them as one abstraction would be the pretence
			// an_app_login_cannot_write_deployment_secrets_test.go's header warns against.
			var appDSN, driver, whoami, lift, restore, unprivileged string
			switch dialect {
			case auth.DialectPostgres:
				appDSN = pgAppRoleDSN(t, owner, testutil.PostgresTestDSN())
				driver, whoami = "postgres", `SELECT current_user`
				// 011's own statements, in reverse and forward order.
				lift = `GRANT UPDATE ON admin.tenant_tombstones TO cleat_app`
				restore = `REVOKE UPDATE ON admin.tenant_tombstones FROM cleat_app`
				unprivileged = `CREATE TABLE admin.zz_3638_probe (x int)`
			default:
				appDSN = mssqlAppRoleDSN(t, owner, os.Getenv("CLEAT_TEST_MSSQL"))
				driver, whoami = "sqlserver", `SELECT USER_NAME()`
				// The DENY is at OBJECT level on top of 001's schema-level GRANT, so REVOKE removes
				// only the DENY and the GRANT underneath it is untouched. That is checked below rather
				// than assumed: cleat#3627 lost a migration's schema grant to exactly this pair
				// issued at the wrong level, and the loss was invisible until a second read.
				lift = `REVOKE UPDATE ON OBJECT::[admin].[tenant_tombstones] FROM cleat_app_role`
				restore = `DENY UPDATE ON OBJECT::[admin].[tenant_tombstones] TO cleat_app_role`
				unprivileged = `CREATE TABLE admin.zz_3638_probe (x int)`
			}

			app, err := sql.Open(driver, appDSN)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { app.Close() })

			// PREMISE. A pass below must be about the app login and not about a connection that can
			// do anything, so say who this is and show that it cannot run DDL.
			var me string
			if err := app.QueryRowContext(ctx, whoami).Scan(&me); err != nil || me != "cleat_app" {
				t.Fatalf("UNMEASURED: the connection is %q (%v), not cleat_app", me, err)
			}
			if _, err := app.ExecContext(ctx, unprivileged); err == nil {
				_, _ = owner.Exec(`DROP TABLE IF EXISTS admin.zz_3638_probe`)
				t.Fatalf("UNMEASURED: cleat_app could run %q, so it is not the least-privilege login this test exists to use", unprivileged)
			}

			ph := func(n int) string {
				if dialect == auth.DialectMSSQL {
					return "@p" + string(rune('0'+n))
				}
				return "$" + string(rune('0'+n))
			}
			target, control := uuid.New(), uuid.New()
			seed := func(id uuid.UUID, hash string) {
				t.Helper()
				if _, err := owner.ExecContext(ctx, `INSERT INTO admin.tenant_tombstones (dropped_tenant_id, name, org_id, created_by, dropped_by, audit_chain_seq, audit_chain_hash)
					VALUES (`+ph(1)+`,`+ph(2)+`,`+ph(3)+`,`+ph(4)+`,`+ph(5)+`,`+ph(6)+`,`+ph(7)+`)`,
					id.String(), "tomb-"+id.String(), uuid.NewString(), "clew-service", "operator:seed", int64(41), hash); err != nil {
					t.Fatalf("seed tombstone: %v", err)
				}
			}
			// storedHash reads the row as the OWNER, so what it reports is the database's state and not
			// something the connection under test could be filtering.
			storedHash := func(id uuid.UUID) string {
				t.Helper()
				var hash sql.NullString
				if err := owner.QueryRowContext(ctx, `SELECT audit_chain_hash FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+ph(1), id.String()).Scan(&hash); err != nil {
					t.Fatalf("read tombstone %s: %v", id, err)
				}
				return strings.TrimSpace(hash.String)
			}
			count := func(id uuid.UUID) int {
				t.Helper()
				var n int
				if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+ph(1), id.String()).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			rewrite := `UPDATE admin.tenant_tombstones SET audit_chain_hash = ` + ph(1) + ` WHERE dropped_tenant_id = ` + ph(2)
			rewriteAs := func(db *sql.DB, id uuid.UUID, hash string) error {
				_, err := db.ExecContext(ctx, rewrite, hash, id.String())
				return err
			}

			t.Cleanup(func() {
				_, _ = owner.Exec(restore) // idempotent: never leave the privilege changed
				for _, id := range []uuid.UUID{target, control} {
					_, _ = owner.Exec(`DELETE FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+ph(1), id.String())
				}
			})
			seed(target, chainHash)
			seed(control, chainHash)

			// KNOWN-POSITIVE for the seed: the owner can rewrite the row, so a refusal below is about
			// the app login's privilege and not about the statement being malformed.
			if err := rewriteAs(owner, target, rewrittenChainHash); err != nil {
				t.Fatalf("the owner login could not rewrite a tombstone, so the statement itself is wrong: %v", err)
			}
			if got := storedHash(target); got != rewrittenChainHash {
				t.Fatalf("the owner's rewrite did not land: hash is %q", got)
			}
			if err := rewriteAs(owner, target, chainHash); err != nil {
				t.Fatalf("restoring the seeded hash: %v", err)
			}

			// 1. THE REFUSAL.
			err = rewriteAs(app, target, rewrittenChainHash)
			if err == nil {
				t.Fatal("the app login rewrote a tombstone; the write-once restriction cleat#3638 adds is not in effect")
			}
			if !mentionsDenial(err) {
				t.Fatalf("the app login's UPDATE failed, but not as a permission error (%v); this test would pass for any other reason an UPDATE can fail", err)
			}
			if got := storedHash(target); got != chainHash {
				t.Fatalf("a refused rewrite changed the row anyway: hash is %q", got)
			}
			t.Logf("running as %s, the UPDATE was refused: %v", me, err)

			// 2. NEGATIVE CONTROL, and it is the whole test. With the restriction lifted the same
			// statement, on the same connection, must succeed -- otherwise "refused" may mean anything.
			if _, err := owner.ExecContext(ctx, lift); err != nil {
				t.Fatalf("UNMEASURED: could not lift the restriction on admin.tenant_tombstones: %v", err)
			}
			if err := rewriteAs(app, target, rewrittenChainHash); err != nil {
				t.Fatalf("with the restriction lifted the app login still could not rewrite a tombstone (%v), so the refusal above was not caused by the restriction", err)
			}
			if got := storedHash(target); got != rewrittenChainHash {
				t.Fatalf("the control write did not land: hash is %q", got)
			}

			// 3. THE RESTORE, verified rather than assumed. On SQL Server this also proves the REVOKE
			// above took only the object-level DENY: a schema grant that had been consumed by it would
			// show up here as an UPDATE that is still allowed.
			if _, err := owner.ExecContext(ctx, restore); err != nil {
				t.Fatalf("could not put the restriction back on admin.tenant_tombstones: %v", err)
			}
			if err := rewriteAs(app, target, chainHash); err == nil {
				t.Fatal("after the restriction was put back the app login could still rewrite a tombstone; the restore did not restore")
			}

			// 4. THE TABLE IS OTHERWISE STILL USABLE by this login -- a narrowing, not a revocation.
			var seen int
			if err := app.QueryRowContext(ctx, `SELECT count(*) FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+ph(1), target.String()).Scan(&seen); err != nil {
				t.Fatalf("the app login could not SELECT a tombstone, which the operator's read route needs: %v", err)
			}
			if seen != 1 {
				t.Fatalf("the app login's SELECT saw %d rows for a tombstone it should see; want 1", seen)
			}
			inserted := uuid.New()
			t.Cleanup(func() {
				_, _ = owner.Exec(`DELETE FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+ph(1), inserted.String())
			})
			if _, err := app.ExecContext(ctx, `INSERT INTO admin.tenant_tombstones (dropped_tenant_id, name, org_id, dropped_by) VALUES (`+ph(1)+`,`+ph(2)+`,`+ph(3)+`,`+ph(4)+`)`,
				inserted.String(), "tomb-"+inserted.String(), uuid.NewString(), "clew-service"); err != nil {
				t.Fatalf("the app login could not INSERT a tombstone, which admin.drop_tenant does as this login: %v", err)
			}
			if _, err := app.ExecContext(ctx, `DELETE FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+ph(1), inserted.String()); err != nil {
				t.Fatalf("the app login could not DELETE a tombstone, which the operator's drop route does as this login: %v", err)
			}
			if count(inserted) != 0 {
				t.Fatalf("the app login's DELETE reported success but the row is still there")
			}
			if count(control) != 1 {
				t.Fatalf("the control tombstone was disturbed: %d rows", count(control))
			}
		})
	}
}

// mentionsDenial keeps the refusal assertion to the failure this test is about. Both drivers say
// "permission" and "denied" in their own words -- PostgreSQL's SQLSTATE 42501 message is
// "permission denied for table tenant_tombstones", SQL Server's is "The UPDATE permission was denied
// on the object 'tenant_tombstones'" -- and matching on either word is enough to exclude the failures
// that look identical at the call site: a table that is not there, a malformed statement, a closed
// connection.
func mentionsDenial(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission") || strings.Contains(msg, "denied")
}
