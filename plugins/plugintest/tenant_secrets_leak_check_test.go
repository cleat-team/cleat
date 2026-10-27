package plugintest

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// The leak check's own known-positive (cleat#3484).
//
// "It reports nothing on a clean database" is satisfied by every broken version
// of a detector, and this one has a reason to be broken in exactly that way: on
// SQL Server a connection that names no tenant counts zero rows through the
// same policy that hides them. So this seeds real rows through the real secret
// store, on every configured dialect, under the default tenant and, where the
// dialect has tenants, a second one, and requires the check to NAME them; then
// requires it to name nothing once RemoveTenantSecretsWithPrefix has run, and
// never to name a row whose prefix it was not asked about.
func TestLeakedTenantSecretsNamesWhatIsLeftAndOnlyThat(t *testing.T) {
	const mine, other = "zz-3484-canary.mine.", "zz-3484-canary.other."
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			t.Cleanup(be.Cleanup) // registered first, so it runs after the cleanups below
			ctx := context.Background()
			setupFull(t, be)

			key := make([]byte, 32)
			ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: key})
			if err != nil {
				t.Fatalf("key ring: %v", err)
			}
			store := engine.NewSecretStoreWithRing(be.DB, string(be.Dialect), ring)

			tenants := []string{engine.DefaultTenantUUID}
			if be.Dialect != testutil.DialectMySQL { // MySQL is single-tenant by constraint
				second := uuid.NewString()
				ins := map[testutil.Dialect]string{
					testutil.DialectPostgres: `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
					testutil.DialectMSSQL:    `INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`,
				}[be.Dialect]
				if _, err := be.DB.Exec(ins, second, "cleat-3484-"+second[:8]); err != nil {
					t.Fatalf("seed a second tenant: %v", err)
				}
				del := strings.NewReplacer("INSERT INTO", "DELETE FROM").Replace(strings.SplitN(ins, " (", 2)[0]) +
					map[testutil.Dialect]string{testutil.DialectPostgres: ` WHERE tenant_id = $1`, testutil.DialectMSSQL: ` WHERE tenant_id = @p1`}[be.Dialect]
				t.Cleanup(func() { be.DB.Exec(del, second) }) //nolint:errcheck // best-effort cleanup
				tenants = append(tenants, second)
			}
			// Registered AFTER the tenant cleanup above, so it runs BEFORE it:
			// tenant_secrets has a foreign key to admin.tenants.
			t.Cleanup(func() {
				for _, tn := range tenants {
					RemoveTenantSecretsWithPrefix(t, be.DB, be.Dialect, tn, mine)
					RemoveTenantSecretsWithPrefix(t, be.DB, be.Dialect, tn, other)
				}
			})

			if got, err := LeakedTenantSecrets(ctx, be.DB, be.Dialect, mine, other); err != nil || len(got) != 0 {
				t.Fatalf("before seeding: got %v, %v; want nothing (a dirty shared database makes this test measure the wrong thing)", got, err)
			}

			var want []string
			for _, tn := range tenants {
				tid := uuid.MustParse(tn)
				tctx := auth.WithTenantID(ctx, tid)
				if err := store.PutSecret(tctx, tn, mine+tn[:8], "v"); err != nil {
					t.Fatalf("seed %s: %v", tn, err)
				}
				want = append(want, tn+"/"+mine+tn[:8])
				if err := store.PutSecret(tctx, tn, other+tn[:8], "v"); err != nil {
					t.Fatalf("seed other-prefix row %s: %v", tn, err)
				}
			}

			got, err := LeakedTenantSecrets(ctx, be.DB, be.Dialect, mine)
			if err != nil {
				t.Fatalf("LeakedTenantSecrets: %v", err)
			}
			if !sameSet(got, want) {
				t.Fatalf("LeakedTenantSecrets(%q) = %v, want exactly %v: every tenant's row under the prefix, and none under %q", mine, got, want, other)
			}

			for _, tn := range tenants {
				RemoveTenantSecretsWithPrefix(t, be.DB, be.Dialect, tn, mine)
			}
			if got, err := LeakedTenantSecrets(ctx, be.DB, be.Dialect, mine); err != nil || len(got) != 0 {
				t.Fatalf("after RemoveTenantSecretsWithPrefix: got %v, %v; want nothing", got, err)
			}
			if got, err := LeakedTenantSecrets(ctx, be.DB, be.Dialect, other); err != nil || len(got) != len(tenants) {
				t.Fatalf("the other prefix's rows were removed with the first, or not seen: got %v, %v; want %d", got, err, len(tenants))
			}
		})
	}
}

func TestLeakedTenantSecretsRefusesToMeasureWithNoPrefix(t *testing.T) {
	if _, err := LeakedTenantSecrets(context.Background(), (*sql.DB)(nil), testutil.DialectMySQL); err == nil {
		t.Fatal("no prefix: want an error, not a clean answer")
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

func setupFull(t *testing.T, be testutil.PluginTestBackend) {
	t.Helper()
	switch be.Dialect {
	case testutil.DialectPostgres:
		testutil.SetupFullSchema(t, be.DB, be.Dialect)
	case testutil.DialectMySQL:
		testutil.SetupMySQLFullSchema(t, be.DB)
	case testutil.DialectMSSQL:
		testutil.SetupMSSQLFullSchema(t, be.DB)
	}
}
