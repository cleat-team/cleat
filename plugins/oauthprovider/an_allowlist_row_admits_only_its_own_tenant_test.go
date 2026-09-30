package oauthprovider

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// TestAllowlistRowAdmitsOnlyItsOwnTenant is cleat#2378: identityAllowed's
// only test double (oauthprovider_behavioral_test.go's fakeConn) answers
// with strings.Contains(query, "FROM oauth_allowed_identities") and then
// reads args positionally -- it never runs the real SQL, so a dropped
// `WHERE tenant_id = $1` or a dropped plugin.ForTenant(ctx, tid) call (the
// scoping oauth_allowed_identities' migration declares via TenantScoped)
// would pass every existing test. This runs identityAllowed for real,
// against Postgres and SQL Server, with a row seeded under a DIFFERENT
// tenant that a broken query could return.
//
// MYSQL IS NOT IN THE DIALECT LIST, and not because oauthprovider skips it
// (identityAllowed is called directly here, not through a pgOnly-gated
// HTTP handler -- see a_real_dialect_login_stores_no_tokens_test.go's own
// comment on that gate). tiers.yaml's D1 (migrations/mysql/
// 038_single_tenant_guard.sql) makes a SECOND tenant impossible to create
// on MySQL at all, and this test needs one to exist in order to seed a row
// under it -- the same exclusion, for the same reason, as engine's
// TestGetSecretRefusesAnotherTenantsRowOnEveryApplicableDialect.
//
// The known-positive (tenant A admits its OWN row) is asserted in the same
// test as the refusal, not split out: a bare "not admitted" proves nothing
// if the table is simply broken for everyone (CLAUDE.md's empty-table trap,
// #1285/#1286).
//
// THE TWO DIALECT ARMS ARE NOT REDUNDANT, AND THE ASYMMETRY IS NOT
// COSMETIC -- do not trim the MSSQL arm as duplicate coverage of the
// Postgres one. testutil.NewPluginTestBackends' Postgres connection is the
// superuser role (bypassrls), so the Postgres subtest can only ever catch a
// dropped `WHERE tenant_id = $1` predicate -- it cannot see a dropped
// plugin.ForTenant(ctx, tid) call at all, since RLS is exactly what that
// call arms and a superuser bypasses RLS regardless. Measured directly
// (cleat-review, 2026-09-30, confirmed independently here): with ForTenant
// removed and the WHERE clause left intact, the Postgres subtest PASSES.
// The MSSQL subtest is the ONLY thing in this file that exercises the
// ForTenant/RLS layer, because SQL Server's SECURITY POLICY applies to
// every principal, not just non-superusers -- it is what fails (closed,
// denying tenant A's own row too) in exactly that scenario. In production,
// on the RLS-subject `cleat_app` role, a dropped ForTenant would instead
// raise via assert_tenant_set() on Postgres rather than pass silently; this
// test's Postgres arm still would not see that either way, because it
// never runs as that role.
func TestAllowlistRowAdmitsOnlyItsOwnTenant(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		if be.Dialect == testutil.DialectMySQL {
			continue
		}
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			seedPlugin := &Plugin{dialect: dialect, logger: quiet}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: seedPlugin, Healthy: true}}); err != nil {
				t.Fatalf("oauthprovider migrations on %s: %v", be.Name, err)
			}
			defer cleanupOauthproviderSchema(t, be.DB, be.Dialect)

			tenantA := uuid.MustParse(engine.DefaultTenantUUID)
			tenantB := uuid.New()
			fixtureDB := be.CrossTenantConn(t, ctx,
				"allowlist cross-tenant fixture: seeds a second tenant and two "+
					"oauth_allowed_identities rows directly")

			tenantsTable := map[testutil.Dialect]string{
				testutil.DialectPostgres: `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
				testutil.DialectMSSQL:    `INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`,
			}[be.Dialect]
			if _, err := fixtureDB.ExecContext(ctx, tenantsTable, tenantB.String(), "cleat-2378-"+tenantB.String()[:8]); err != nil {
				t.Fatalf("seed a second tenant on %s: %v", be.Name, err)
			}

			insertRow := func(tid uuid.UUID, email string) {
				t.Helper()
				if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect, `
					INSERT INTO oauth_allowed_identities (tenant_id, provider, identity_type, identity_value)
					VALUES ($1, $2, $3, $4)
				`, tid, "google", identityTypeEmail, email); err != nil {
					t.Fatalf("seed oauth_allowed_identities row for %s: %v", tid, err)
				}
			}
			insertRow(tenantA, "tenant-a-own@example.com")
			insertRow(tenantB, "tenant-b-only@example.com")

			defer func() {
				bg := context.Background()
				for _, tid := range []uuid.UUID{tenantA, tenantB} {
					if _, err := plugintest.ExecRebound(t, bg, fixtureDB, dialect,
						`DELETE FROM oauth_allowed_identities WHERE tenant_id = $1`, tid); err != nil {
						t.Errorf("cleanup oauth_allowed_identities %s on %s: %v", tid, be.Name, err)
					}
				}
				tenantsDelete := map[testutil.Dialect]string{
					testutil.DialectPostgres: `DELETE FROM admin.tenants WHERE tenant_id = $1`,
					testutil.DialectMSSQL:    `DELETE FROM admin.tenants WHERE tenant_id = @p1`,
				}[be.Dialect]
				if _, err := fixtureDB.ExecContext(bg, tenantsDelete, tenantB.String()); err != nil {
					t.Errorf("cleanup admin.tenants on %s: %v", be.Name, err)
				}
			}()

			p := &Plugin{dialect: dialect, logger: quiet}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}

			// Known-positive: tenant A must admit its OWN row.
			own, ok, err := p.identityAllowed(ctx, tenantA, "google",
				resolvedIdentity{Email: "tenant-a-own@example.com", EmailVerified: true})
			if err != nil {
				t.Fatalf("UNMEASURED: identityAllowed errored on tenant A's own row on %s: %v", be.Name, err)
			}
			if !ok || own.Value != "tenant-a-own@example.com" {
				t.Fatalf("on %s: tenant A did not admit its own allowlisted identity (ok=%v, got %+v)",
					be.Name, ok, own)
			}

			// The refusal under test: tenant A's context, tenant B's row.
			if _, ok, err := p.identityAllowed(ctx, tenantA, "google",
				resolvedIdentity{Email: "tenant-b-only@example.com", EmailVerified: true}); err != nil {
				t.Fatalf("identityAllowed errored probing tenant B's row on %s: %v", be.Name, err)
			} else if ok {
				t.Errorf("on %s: tenant A's identityAllowed admitted an identity allowlisted "+
					"only under a DIFFERENT tenant -- the tenant_id predicate and/or "+
					"TenantScoped RLS did not scope this read", be.Name)
			}
		})
	}
}
