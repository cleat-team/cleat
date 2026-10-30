package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3552, B3b-2b, under the login the worker actually runs as.
//
// The other tombstone tests connect as the database OWNER (postgres, sa), which can do anything, so they cannot say
// whether the worker's own login may DELETE from admin.tenant_tombstones. That is the shape of the defect cleat#3634
// found: every local run used the owner DSN, and a grant the real login lacked went unseen until it did not. This
// runs the route and the store as cleat_app (PostgreSQL) and as the cleat_app login in cleat_app_role (SQL Server),
// the same helpers the worker-boots-as-the-app-role tests use.

func TestATombstoneIsDeletedAsTheAppLogin(t *testing.T) {
	for _, dialect := range []string{auth.DialectPostgres, auth.DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			owner := testutil.TestDB(t, testutil.Dialect(dialect))
			ctx := context.Background()

			var appDSN, driver, whoami, revoke, restore, unprivileged string
			switch dialect {
			case auth.DialectPostgres:
				appDSN = pgAppRoleDSN(t, owner, testutil.PostgresTestDSN())
				driver, whoami = "postgres", `SELECT current_user`
				// Migration 007 grants cleat_app SELECT, INSERT and DELETE on the tombstone table. It also holds UPDATE
				// there, through 001's ALTER DEFAULT PRIVILEGES IN SCHEMA admin, so UPDATE is not a usable probe of
				// "least privilege"; the DDL below is.
				revoke = `REVOKE DELETE ON admin.tenant_tombstones FROM cleat_app`
				restore = `GRANT DELETE ON admin.tenant_tombstones TO cleat_app`
				unprivileged = `CREATE TABLE admin.zz_3552_probe (x int)`
			default:
				appDSN = mssqlAppRoleDSN(t, owner, os.Getenv("CLEAT_TEST_MSSQL"))
				driver, whoami = "sqlserver", `SELECT USER_NAME()`
				// The grant is schema-level on [admin]; a table-level DENY wins over it, and REVOKE removes the DENY.
				revoke = `DENY DELETE ON admin.tenant_tombstones TO cleat_app_role`
				restore = `REVOKE DELETE ON admin.tenant_tombstones FROM cleat_app_role`
				unprivileged = `CREATE TABLE admin.zz_3552_probe (x int)`
			}
			app, err := sql.Open(driver, appDSN)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { app.Close() })

			// PREMISE, so that a pass is about the app login and not about a connection that can do anything.
			var me string
			if err := app.QueryRowContext(ctx, whoami).Scan(&me); err != nil || me != "cleat_app" {
				t.Fatalf("UNMEASURED: the connection is %q (%v), not cleat_app", me, err)
			}
			if _, err := app.ExecContext(ctx, unprivileged); err == nil {
				_, _ = owner.Exec(`DROP TABLE IF EXISTS admin.zz_3552_probe`)
				t.Fatalf("UNMEASURED: cleat_app could run %q, so it is not the least-privilege login this test exists to use", unprivileged)
			}

			ph := func(n int) string {
				if dialect == auth.DialectMSSQL {
					return "@p" + string(rune('0'+n))
				}
				return "$" + string(rune('0'+n))
			}
			ts, err := auth.NewTenantStoreForDialect(app, dialect)
			if err != nil {
				t.Fatal(err)
			}
			target, control := uuid.New(), uuid.New()
			orgName := "b3b2b-app-" + uuid.NewString()
			seed := func(id uuid.UUID, seq any, hash any) {
				t.Helper()
				if _, err := owner.ExecContext(ctx, `INSERT INTO admin.tenant_tombstones (dropped_tenant_id, name, org_id, created_by, dropped_by, audit_chain_seq, audit_chain_hash)
					VALUES (`+ph(1)+`,`+ph(2)+`,`+ph(3)+`,`+ph(4)+`,`+ph(5)+`,`+ph(6)+`,`+ph(7)+`)`,
					id.String(), "tomb-"+id.String(), uuid.NewString(), "clew-service", "operator:seed", seq, hash); err != nil {
					t.Fatalf("seed tombstone: %v", err)
				}
			}
			count := func(id uuid.UUID) int {
				var n int
				if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+ph(1), id.String()).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			t.Cleanup(func() {
				_, _ = owner.Exec(restore) // idempotent: never leave the grant changed
				for _, id := range []uuid.UUID{target, control} {
					_, _ = owner.Exec(`DELETE FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+ph(1), id.String())
					_, _ = owner.Exec(`DELETE FROM admin.tenant_api_keys WHERE tenant_id = `+ph(1), id.String())
					_, _ = owner.Exec(`DELETE FROM admin.tenants WHERE tenant_id = `+ph(1), id.String())
				}
				_, _ = owner.Exec(`DELETE FROM admin.orgs WHERE name = `+ph(1), orgName)
			})
			seed(target, int64(41), chainHash)
			seed(control, nil, nil)

			// NEGATIVE CONTROL for the grant itself. With DELETE taken away the app login cannot delete, and the store
			// says so as an error that is NOT "no tombstone". This is the failure a missing grant produces, and
			// it is what proves the passing run below is about the grant being there.
			if _, err := owner.Exec(revoke); err != nil {
				t.Fatalf("UNMEASURED: could not take DELETE away from the app login: %v", err)
			}
			_, err = ts.DeleteTombstone(ctx, target)
			if _, rerr := owner.Exec(restore); rerr != nil {
				t.Fatalf("could not give DELETE back to the app login: %v", rerr)
			}
			if err == nil || errors.Is(err, auth.ErrTombstoneNotFound) {
				t.Fatalf("without DELETE the app login's DeleteTombstone = %v; want a permission error, not success or 'not found'", err)
			}
			if count(target) != 1 {
				t.Fatalf("a refused delete removed the tombstone")
			}
			t.Logf("without DELETE, running as %s, the store refused: %v", me, err)

			// The route, as the app login, behind the real handler: the reply equals the row read as the owner.
			var name, org string
			var seq int64
			var hash, droppedBy sql.NullString
			orgCol := "org_id::text"
			if dialect == auth.DialectMSSQL {
				orgCol = "CONVERT(NVARCHAR(36), org_id)"
			}
			if err := owner.QueryRowContext(ctx, `SELECT name, `+orgCol+`, dropped_by, audit_chain_seq, audit_chain_hash FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+ph(1), target.String()).
				Scan(&name, &org, &droppedBy, &seq, &hash); err != nil {
				t.Fatal(err)
			}
			f, _ := buildProvisionHTTP(t, nil)
			f.api.provisioner = ts
			w := doDrop(f.chainFixture, tombPath(target.String()), f.adminKey, target.String())
			if w.Code != http.StatusOK {
				t.Fatalf("DELETE as the app login = %d %s; want 200", w.Code, w.Body.String())
			}
			var got tombstoneReply
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			chain, _ := got.Tombstone["audit_chain"].(map[string]any)
			gotOrg, _ := uuid.Parse(got.Tombstone["org_id"].(string))
			wantOrg, _ := uuid.Parse(org)
			if got.Tombstone["name"] != name || gotOrg != wantOrg || got.Tombstone["dropped_by"] != droppedBy.String ||
				chain["seq"] != float64(seq) || chain["hash"] != strings.TrimSpace(hash.String) {
				t.Errorf("the reply is not the row: %v vs name=%q org=%s dropped_by=%q seq=%d hash=%s", got.Tombstone, name, org, droppedBy.String, seq, hash.String)
			}
			if count(target) != 0 || count(control) != 1 {
				t.Errorf("tombstones after the delete: target=%d control=%d; want 0 and 1", count(target), count(control))
			}
			if w := doDrop(f.chainFixture, tombPath(target.String()), f.adminKey, target.String()); w.Code != http.StatusNotFound {
				t.Errorf("a repeat as the app login = %d; want 404", w.Code)
			}

			// And the id can be provisioned again by the same login: the point of the route.
			if res, err := ts.ProvisionTenant(ctx, auth.ProvisionRequest{TenantID: target, OrgName: orgName, Creator: "clew-service"}); err != nil || res.Tenant.ID != target {
				t.Errorf("provisioning the id as the app login after the delete = %+v, %v", res, err)
			}
		})
	}
}
