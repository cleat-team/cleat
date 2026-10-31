package main

// cleat#3627, under the login the worker actually runs as.
//
// The engine tests for the two library reads connect as a role that CAN read what they read: the database
// owner on SQL Server, a non-superuser application role on PostgreSQL. That is the shape of the defect cleat#3634
// found, where a new read of library.* passed everywhere as the owner and failed 11 SQL Server tests as the
// tenant login, because a grant was missing. So this runs both routes behind the REAL route table, the REAL
// production store factory and the least-privilege application login (cleat_app, in cleat_app_role on SQL
// Server), the helpers the worker-boots-as-the-app-role tests use.
//
// It says which role ran by asserting it, not by logging it. PREMISE first: the connection IS cleat_app and
// cannot create a table, so a pass is about that login and not about a connection that can do anything. Then
// the grants themselves are taken away, one at a time, and the read must FAIL with a permission error rather
// than succeed or answer empty: that is the negative control that makes the passing run mean the grants are
// there. The tenant boundary is asserted again through the routes with two tenants.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

func TestTheLibraryReadsRunAsTheAppLogin(t *testing.T) {
	for _, dialect := range []string{auth.DialectPostgres, auth.DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			owner := testutil.TestDB(t, testutil.Dialect(dialect))
			testutil.SetupFullSchema(t, owner, testutil.Dialect(dialect))

			var appDSN, driver, whoami, d3 string
			var factory engine.StoreFactory
			var edialect engine.Dialect
			var denyCatalogue, restoreCatalogue, denyInstalls, restoreInstalls string
			switch dialect {
			case auth.DialectPostgres:
				appDSN = pgAppRoleDSN(t, owner, testutil.PostgresTestDSN())
				driver, whoami, edialect = "postgres", `SELECT current_user`, engine.DialectPostgres
				denyCatalogue = `REVOKE SELECT ON library.bundles, library.bundle_members, library.members FROM cleat_app`
				restoreCatalogue = `GRANT SELECT ON library.bundles, library.bundle_members, library.members TO cleat_app`
				denyInstalls = `REVOKE SELECT ON tenant_library_installs FROM cleat_app`
				restoreInstalls = `GRANT SELECT ON tenant_library_installs TO cleat_app`
				d3 = "$1"
			default:
				appDSN = mssqlAppRoleDSN(t, owner, os.Getenv("CLEAT_TEST_MSSQL"))
				driver, whoami, edialect = "sqlserver", `SELECT USER_NAME()`, engine.DialectMSSQL
				denyCatalogue = `DENY SELECT ON SCHEMA::[library] TO cleat_app_role`
				// GRANT, not REVOKE. A DENY and a GRANT on one object are ONE permission row, so DENY replaces the
				// schema-level grant migration 005 made, and REVOKE then deletes the row: the grant is gone for good
				// (measured: this test's first version took cleat_app_role's SELECT on [library] away for the rest of
				// the database's life). The installs table below is different: its DENY sits on top of the dbo
				// schema-level grant, a separate row, so REVOKE there removes only the DENY.
				restoreCatalogue = `GRANT SELECT ON SCHEMA::[library] TO cleat_app_role`
				denyInstalls = `DENY SELECT ON dbo.tenant_library_installs TO cleat_app_role`
				restoreInstalls = `REVOKE SELECT ON dbo.tenant_library_installs FROM cleat_app_role`
				d3 = "@p1"
			}
			app, err := sql.Open(driver, appDSN)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { app.Close() })
			switch dialect {
			case auth.DialectPostgres:
				factory = engine.NewPostgresStoreFactory(app, "public")
			default:
				factory = engine.NewMSSQLStoreFactory(appDSN)
			}

			// PREMISE: which login ran, asserted. cleat_app, and it cannot create a table.
			var me string
			if err := app.QueryRowContext(ctx, whoami).Scan(&me); err != nil || me != "cleat_app" {
				t.Fatalf("UNMEASURED: the connection is %q (%v), not cleat_app", me, err)
			}
			if _, err := app.ExecContext(ctx, `CREATE TABLE zz_3627_probe (x int)`); err == nil {
				_, _ = owner.Exec(`DROP TABLE IF EXISTS zz_3627_probe`)
				t.Fatalf("UNMEASURED: cleat_app could create a table, so it is not the least-privilege login this test exists to use")
			}
			t.Logf("%s: both reads run as %s (PREMISE asserted: cleat_app, no CREATE TABLE)", dialect, me)

			// Seed as the owner: a published library with two bundles, and one install per tenant.
			library := "lib-3627-app-" + uuid.NewString()[:8]
			tenantA, tenantB := uuid.NewString(), uuid.NewString()
			member := func(v int) engine.PublishLibraryMember {
				return engine.PublishLibraryMember{Name: library + ".order", Version: v, WasmBytes: []byte{0, 'a', 's', 'm', byte(v)}, ABIVersion: 2, MinVersion: 1}
			}
			for _, b := range []struct {
				bv string
				v  int
			}{{"1.0.0", 1}, {"2.0.0", 2}} {
				if _, err := engine.PublishLibraryBundle(ctx, owner, edialect, engine.PublishLibraryRequest{
					Library: library, BundleVersion: b.bv, PublishedBy: "operator@example.com", Members: []engine.PublishLibraryMember{member(b.v)},
				}); err != nil {
					t.Fatalf("UNMEASURED: publish %s@%s: %v", library, b.bv, err)
				}
			}
			scoped := func(tenant string, fn func(c *sql.Conn)) {
				c, err := owner.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				if dialect == auth.DialectMSSQL {
					_, _ = c.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant)
				}
				fn(c)
			}
			t.Cleanup(func() {
				_, _ = owner.Exec(restoreCatalogue)
				_, _ = owner.Exec(restoreInstalls)
				for _, tn := range []string{tenantA, tenantB} {
					scoped(tn, func(c *sql.Conn) {
						_, _ = c.ExecContext(ctx, `DELETE FROM tenant_library_installs WHERE tenant_id = `+d3, tn)
						_, _ = c.ExecContext(ctx, `DELETE FROM workflow_defs WHERE tenant_id = `+d3, tn)
					})
				}
				_, _ = owner.Exec(`DELETE FROM library.bundle_members WHERE library = ` + sqlLit(library))
				_, _ = owner.Exec(`DELETE FROM library.bundles WHERE library = ` + sqlLit(library))
				_, _ = owner.Exec(`DELETE FROM library.members WHERE library = ` + sqlLit(library))
			})
			install := func(tenant, bundleVersion, by string) {
				t.Helper()
				st, closer, err := factory.OpenStore(ctx, tenant)
				if err != nil {
					t.Fatalf("UNMEASURED: open a store for %s: %v", tenant, err)
				}
				defer closer.Close()
				if _, err := st.(engine.LibraryInstaller).InstallLibraryBundle(ctx, engine.InstallLibraryRequest{Library: library, BundleVersion: bundleVersion, InstalledBy: by}); err != nil {
					t.Fatalf("UNMEASURED: install %s@%s for %s as the app login: %v", library, bundleVersion, tenant, err)
				}
			}
			install(tenantA, "1.0.0", "key-of-a")
			install(tenantB, "2.0.0", "key-of-b")

			api := newTestAPIServer(&mockStore{})
			api.factory = factory
			get := func(tenant, path string) (int, string) {
				mux := registerRoutes(http.NewServeMux(), api)
				req := newGET(path, tenant)
				rec := serveLibraryRead(mux, req)
				return rec.Code, rec.Body.String()
			}

			// 1. Installs, through the routes, as the app login: each tenant its own and no other's.
			for tenant, want := range map[string]string{tenantA: "1.0.0", tenantB: "2.0.0"} {
				code, body := get(tenant, "/api/library-installs")
				if code != http.StatusOK {
					t.Fatalf("%s: GET /api/library-installs as %s answered %d: %s", dialect, me, code, body)
				}
				var got struct {
					Installs []engine.InstalledLibrary `json:"installs"`
				}
				if err := json.Unmarshal([]byte(body), &got); err != nil || len(got.Installs) != 1 || got.Installs[0].Library != library || got.Installs[0].BundleVersion != want {
					t.Errorf("%s: tenant %s lists %s, want exactly %s@%s", dialect, tenant, body, library, want)
				}
			}
			// 2. Catalogue, as the app login: both bundles, and nobody's name for who published.
			code, body := get(tenantA, "/api/libraries?library="+library)
			if code != http.StatusOK {
				t.Fatalf("%s: GET /api/libraries as %s answered %d: %s", dialect, me, code, body)
			}
			var cat struct {
				Bundles []engine.LibraryBundleSummary `json:"bundles"`
			}
			if err := json.Unmarshal([]byte(body), &cat); err != nil || len(cat.Bundles) != 2 || cat.Bundles[0].BundleVersion != "2.0.0" {
				t.Errorf("%s: catalogue = %s, want the two bundles newest first", dialect, body)
			}
			if strings.Contains(body, "operator@example.com") {
				t.Errorf("%s: the catalogue names who published: %s", dialect, body)
			}

			// NEGATIVE CONTROLS: take each grant away from the app login; the read must FAIL, not answer.
			for _, tc := range []struct{ name, deny, restore, path string }{
				{"the library schema grant", denyCatalogue, restoreCatalogue, "/api/libraries?library=" + library},
				{"the installs table grant", denyInstalls, restoreInstalls, "/api/library-installs"},
			} {
				if _, err := owner.Exec(tc.deny); err != nil {
					t.Fatalf("UNMEASURED: could not take %s away: %v", tc.name, err)
				}
				code, body := get(tenantA, tc.path)
				if _, rerr := owner.Exec(tc.restore); rerr != nil {
					t.Fatalf("could not give %s back: %v", tc.name, rerr)
				}
				if code != http.StatusInternalServerError {
					t.Errorf("%s: without %s, GET %s answered %d %s; want a 500 from a permission error, not an answer", dialect, tc.name, tc.path, code, body)
				}
			}
			if code, _ := get(tenantA, "/api/libraries?library="+library); code != http.StatusOK {
				t.Errorf("%s: after the grants were given back the catalogue answered %d: the restore did not restore", dialect, code)
			}
		})
	}
}

// sqlLit quotes a value this test generated itself (hex and hyphens) for a statement the dialects share.
func sqlLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// newGET and serveLibraryRead build one authenticated GET and serve it through a mux.
func newGET(path, tenant string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	id := uuid.MustParse(tenant)
	return req.WithContext(auth.WithSubject(auth.WithTenantID(req.Context(), id), "key-of-"+tenant[:8]))
}

func serveLibraryRead(mux *http.ServeMux, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}
