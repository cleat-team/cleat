package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3552, B2a. CreateTenant and CreateOrg used to refuse SQL Server because they asked
// the database for the new id with RETURNING, which SQL Server spells OUTPUT. They now make
// the id in Go and INSERT it, so the two dialects differ only in the placeholder.
//
// Two layers, because they fail differently. The statement test needs no database and says
// what shape each dialect gets. The database test is the one that can be wrong about SQL
// Server: it creates an org and a tenant, reads the row back, then goes on to the next step
// of the real handshake (a key for the tenant, resolved back to it), so a creation that
// "succeeds" but writes something nothing can use is a failure here.

func TestTheCreateStatementsSupplyTheirOwnIdOnEveryDialect(t *testing.T) {
	for _, tc := range []struct {
		dialect     string
		placeholder string
	}{
		{DialectPostgres, "$1"},
		{DialectMSSQL, "@p1"},
	} {
		tenant, terr := createTenantStmt(tc.dialect)
		org, oerr := createOrgStmt(tc.dialect)
		if terr != nil || oerr != nil {
			t.Fatalf("%s: tenant %v, org %v", tc.dialect, terr, oerr)
		}
		for name, stmt := range map[string]string{"tenant": tenant, "org": org} {
			up := strings.ToUpper(stmt)
			if strings.Contains(up, "RETURNING") || strings.Contains(up, "OUTPUT") {
				t.Errorf("%s %s statement asks the database for the id: %s", tc.dialect, name, stmt)
			}
			if !strings.Contains(stmt, tc.placeholder) {
				t.Errorf("%s %s statement does not use %s: %s", tc.dialect, name, tc.placeholder, stmt)
			}
		}
		// The id is the FIRST column of each, so the caller's first argument is the id.
		if !strings.Contains(tenant, "(tenant_id, name, display_name, org_id)") {
			t.Errorf("%s tenant statement does not lead with tenant_id: %s", tc.dialect, tenant)
		}
		if !strings.Contains(org, "(org_id, name)") {
			t.Errorf("%s org statement does not lead with org_id: %s", tc.dialect, org)
		}
	}

	// MySQL is refused, and says why.
	if _, err := createTenantStmt(DialectMySQL); err == nil || !strings.Contains(err.Error(), "mysql") {
		t.Errorf("createTenantStmt(mysql) = %v, want a refusal naming the dialect", err)
	}
	if _, err := createOrgStmt(DialectMySQL); err == nil || !strings.Contains(err.Error(), "mysql") {
		t.Errorf("createOrgStmt(mysql) = %v, want a refusal naming the dialect", err)
	}
}

func TestAnOrgAndATenantAreCreatedAndUsableOnPostgresAndSQLServer(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			db := testutil.TestDB(t, testutil.Dialect(dialect))
			ts, err := NewTenantStoreForDialect(db, dialect)
			if err != nil {
				t.Fatalf("NewTenantStoreForDialect(%q): %v", dialect, err)
			}
			ctx := context.Background()

			ph := func(n int) string {
				if dialect == DialectMSSQL {
					return "@p" + string(rune('0'+n))
				}
				return "$" + string(rune('0'+n))
			}
			// Both names are unique across the deployment, and a database this test does not
			// recreate may hold a previous run's rows.
			suffix := uuid.New().String()
			orgName := "b2a-org-" + suffix
			tenantName := "b2a-tenant-" + suffix

			oid, err := ts.CreateOrg(ctx, orgName)
			if err != nil {
				t.Fatalf("CreateOrg on %s: %v", dialect, err)
			}
			if oid == uuid.Nil {
				t.Fatal("CreateOrg returned the nil uuid")
			}
			var tids []uuid.UUID
			t.Cleanup(func() {
				for _, tid := range tids {
					_, _ = db.Exec(`DELETE FROM admin.tenant_api_keys WHERE tenant_id = `+ph(1), tid.String())
					_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = `+ph(1), tid.String())
				}
				_, _ = db.Exec(`DELETE FROM admin.orgs WHERE org_id = `+ph(1), oid.String())
			})

			tid, err := ts.CreateTenant(ctx, tenantName, "B2a Display", oid)
			if err != nil {
				t.Fatalf("CreateTenant on %s: %v", dialect, err)
			}
			tids = append(tids, tid)
			if tid == uuid.Nil {
				t.Fatal("CreateTenant returned the nil uuid")
			}

			// The id handed back is the id stored. The failure this guards is a statement that
			// lets the database choose one: the caller would hold an id nothing has.
			var gotID, gotName, gotDisplay, gotOrg string
			q := `SELECT CAST(tenant_id AS VARCHAR(36)), name, display_name, CAST(org_id AS VARCHAR(36)) FROM admin.tenants WHERE tenant_id = ` + ph(1)
			if dialect == DialectPostgres {
				q = `SELECT tenant_id::text, name, display_name, org_id::text FROM admin.tenants WHERE tenant_id = $1`
			}
			if err := db.QueryRow(q, tid.String()).Scan(&gotID, &gotName, &gotDisplay, &gotOrg); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					// Not a fixture problem: the row this call created is under another id.
					t.Fatalf("CreateTenant returned %s but no tenant has that id on %s", tid, dialect)
				}
				t.Fatalf("UNMEASURED: read the tenant back on %s: %v", dialect, err)
			}
			if !strings.EqualFold(gotID, tid.String()) || gotName != tenantName || gotDisplay != "B2a Display" ||
				!strings.EqualFold(gotOrg, oid.String()) {
				t.Errorf("stored (%s, %s, %s, %s), want (%s, %s, B2a Display, %s)",
					gotID, gotName, gotDisplay, gotOrg, tid, tenantName, oid)
			}

			// A second tenant under the same org gets a different id, and the name is unique
			// across the deployment, so reusing it is refused.
			tid2, err := ts.CreateTenant(ctx, tenantName+"-second", "", oid)
			if err != nil {
				t.Fatalf("a second tenant under the same org: %v", err)
			}
			tids = append(tids, tid2)
			if tid2 == tid {
				t.Error("two tenants were given the same id")
			}
			if _, err := ts.CreateTenant(ctx, tenantName, "", oid); err == nil {
				t.Error("a duplicate tenant name was accepted")
			}
			if _, err := ts.CreateOrg(ctx, orgName); err == nil {
				t.Error("a duplicate org name was accepted")
			}

			// The next step of the real handshake: a key for the new tenant, resolved back to it.
			raw := GenerateAPIKey()
			if err := ts.CreateAPIKey(ctx, tid, "b2a", raw); err != nil {
				t.Fatalf("CreateAPIKey for the new tenant on %s: %v", dialect, err)
			}
			resolved, err := ts.ResolveTenantFromAPIKey(ctx, sha256Hash(raw))
			if err != nil {
				t.Fatalf("ResolveTenantFromAPIKey on %s: %v", dialect, err)
			}
			if resolved != tid {
				t.Errorf("the key resolves to %s, want the tenant it was made for, %s", resolved, tid)
			}
		})
	}
}
