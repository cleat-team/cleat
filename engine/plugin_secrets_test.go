package engine

// cleat#1992. This file proves the property the owner asked for directly on
// the issue, about the plugin.Secrets adapter in plugin_secrets.go rather
// than about SecretStore underneath it: a plugin handling tenant B's request
// cannot reach tenant A's secret through ANY ARGUMENT, and the named
// per-tenant method (ForTenant) is the only way across.
//
// engine/a_secret_never_crosses_tenants_at_the_store_test.go already proves
// the STORE refuses a mismatched CONTEXT at the database -- RLS on Postgres,
// a SECURITY POLICY on SQL Server. That is a different property from this
// one. Shape A of the #1992 design (GetSecret(ctx, tenantID, name), mirroring
// SecretStore's own signature) would have passed that test too, and still had
// the bug the owner flagged: a plugin serving tenant B's request that calls
// the wrong tenantID as a plain string argument sets that context's tenant
// FROM the argument, so RLS allows it -- the parameter is not a thing RLS can
// check, it is the thing that tells RLS what to check. Shape B, which is
// what pluginSecrets implements, removes the argument instead of trying to
// validate it: Get/Put/Retire take no tenantID at all, so there is nothing
// for a plugin bug to pass wrong on the request path. This file's job is to
// show that is actually true of the adapter's public API, not just of the
// design as described on the issue.
//
// MYSQL IS NOT IN THIS TEST'S DIALECT SET, same reason as the store-level
// test: migration 038's single-tenant unique index means newRotationEnv
// itself only ever seeds ONE tenant on MySQL (see its own comment), so
// e.tenants[1] does not exist there. See the store-level test's header for
// the CI failure (cleat#2159) this was learned from.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestPluginSecretsCannotReachAnotherTenantByAnyArgument is the known-positive
// half deliberately kept in the same test as the refusal: tenant A's own
// request-path read succeeding, and ForTenant(tenantA) independently reading
// the same value, are both what rule out "the whole adapter is broken and
// finds nothing for anyone" standing in for a real refusal.
func TestPluginSecretsCannotReachAnotherTenantByAnyArgument(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			e := newRotationEnv(t, dialect)
			aTenant := e.tenants[0]
			bTenant := e.tenants[1]
			e.claim(t, aTenant, "plugin_secrets_probe")

			ring, err := NewKeyRing(rotV1)
			if err != nil {
				t.Fatalf("NewKeyRing: %v", err)
			}
			secrets := NewPluginSecrets(e.store(ring))

			// Seed tenant A's secret through the request-path Put, exactly as a
			// plugin route handling tenant A's own request would call it.
			if err := secrets.Put(e.ctx(aTenant), "plugin_secrets_probe", "tenant-a-only-value"); err != nil {
				t.Fatalf("Put under tenant A: %v", err)
			}

			// Known-positive #1: tenant A's own request-path Get must succeed,
			// and with the right value, or a "not found" below proves nothing.
			got, err := secrets.Get(e.ctx(aTenant), "plugin_secrets_probe")
			if err != nil {
				t.Fatalf("UNMEASURED: tenant A could not read its own just-written secret: %v", err)
			}
			if got != "tenant-a-only-value" {
				t.Fatalf("tenant A read %q, want %q", got, "tenant-a-only-value")
			}

			// THE CLAIM UNDER TEST. secrets.Get takes no tenantID parameter --
			// the only tenant this call can possibly name is whichever one
			// e.ctx(bTenant) put in ctx. There is no argument left for a plugin
			// bug to pass as aTenant by mistake; this is the whole point of
			// Shape B over Shape A, see the file comment.
			if _, err := secrets.Get(e.ctx(bTenant), "plugin_secrets_probe"); err == nil {
				t.Fatal("tenant B's request-path Get read tenant A's secret")
			} else if !errors.Is(err, ErrSecretNotFound) {
				// Postgres/MSSQL: the database-level policy is what refuses this
				// underneath the adapter, and it typically surfaces as a
				// distinct error rather than a plain not-found -- either is an
				// acceptable refusal, a nil error is not.
				t.Logf("tenant B's read was refused with: %v", err)
			}

			// Known-positive #2, and the other half of "the named method is the
			// only way across": ForTenant(aTenant) -- named, greppable, and
			// nothing like the request-path signature -- reads the SAME row
			// that the request-path call above could not, from a caller (bTenant's
			// own ctx, unchanged) that has no other way to reach it.
			got, err = secrets.ForTenant(aTenant.String()).Get(e.ctx(bTenant), "plugin_secrets_probe")
			if err != nil {
				t.Fatalf("ForTenant(tenantA) could not read tenant A's own secret: %v", err)
			}
			if got != "tenant-a-only-value" {
				t.Fatalf("ForTenant(tenantA) read %q, want %q", got, "tenant-a-only-value")
			}
		})
	}
}

// TestPluginSecretsRetireCannotDisableAnotherTenantsRow is Retire's version
// of the claim above, on the write side rather than the read side: tenant B's
// request-path Retire must not be able to disable tenant A's row, for the
// same reason Get cannot read it -- there is no tenantID argument to name A
// with. RowsAffected is the assertion, not just the error, because a Retire
// that silently affects zero rows and returns nil is indistinguishable from
// success by error alone.
func TestPluginSecretsRetireCannotDisableAnotherTenantsRow(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			e := newRotationEnv(t, dialect)
			aTenant := e.tenants[0]
			bTenant := e.tenants[1]
			e.claim(t, aTenant, "plugin_secrets_retire_probe")

			ring, err := NewKeyRing(rotV1)
			if err != nil {
				t.Fatalf("NewKeyRing: %v", err)
			}
			secrets := NewPluginSecrets(e.store(ring))
			if err := secrets.Put(e.ctx(aTenant), "plugin_secrets_retire_probe", "tenant-a-only-value"); err != nil {
				t.Fatalf("Put under tenant A: %v", err)
			}

			// THE CLAIM UNDER TEST: tenant B's request-path Retire, same name,
			// must affect zero rows -- it has no argument through which it
			// could possibly name tenant A's row.
			n, err := secrets.Retire(e.ctx(bTenant), "plugin_secrets_retire_probe")
			if err != nil {
				t.Fatalf("tenant B's Retire returned an error rather than affecting zero rows: %v", err)
			}
			if n != 0 {
				t.Fatalf("tenant B's request-path Retire disabled %d row(s) of tenant A's secret, want 0", n)
			}

			// Known-positive: tenant A's row must still be readable after
			// tenant B's no-op Retire, or the zero above could mean the row
			// was already gone rather than that it was protected.
			if got, err := secrets.Get(e.ctx(aTenant), "plugin_secrets_retire_probe"); err != nil || got != "tenant-a-only-value" {
				t.Fatalf("UNMEASURED: tenant A's secret did not survive tenant B's Retire attempt: got=%q err=%v", got, err)
			}

			// Tenant A's OWN Retire, through the named per-tenant method this
			// time (ForTenant, exercised here so it is not just Get's escape
			// hatch), must affect exactly one row and disable the read.
			n, err = secrets.ForTenant(aTenant.String()).Retire(e.ctx(bTenant), "plugin_secrets_retire_probe")
			if err != nil {
				t.Fatalf("ForTenant(tenantA).Retire: %v", err)
			}
			if n != 1 {
				t.Fatalf("ForTenant(tenantA).Retire affected %d row(s), want 1", n)
			}
			if _, err := secrets.Get(e.ctx(aTenant), "plugin_secrets_retire_probe"); err == nil {
				t.Fatal("tenant A's secret was still readable after ForTenant(tenantA).Retire")
			}
		})
	}
}

// TestPluginSecretsRequestPathRefusesAnUnmarkedContext is the other half of
// "no argument to get wrong": a ctx that never went through plugin.ForTenant
// at all -- the shape a background loop would produce if it mistakenly
// called the request-path method instead of reaching for ForTenant -- must be
// refused before any query runs, rather than silently scoping to nothing or,
// worse, reaching beginTenantTx's no-transaction fallback and running
// unscoped (see engine/plugindb_tenant.go). No database is needed to show
// this: the adapter's own tenant lookup fails before Get ever calls the
// store.
func TestPluginSecretsRequestPathRefusesAnUnmarkedContext(t *testing.T) {
	secrets := NewPluginSecrets(nil)
	if _, err := secrets.Get(context.Background(), "anything"); err == nil {
		t.Fatal("Get with no tenant in context returned no error")
	} else if !strings.Contains(err.Error(), "no tenant in context") {
		t.Fatalf("Get with no tenant in context: got %v, want a \"no tenant in context\" error", err)
	}
}
