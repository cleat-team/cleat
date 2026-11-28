package plugintest

import (
	"context"
	"database/sql"
	"net/url"
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

			if got, err := leakedRows(ctx, be.DB, be.Dialect, mine, other); err != nil || len(got) != 0 {
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

			got, err := leakedRows(ctx, be.DB, be.Dialect, mine)
			if err != nil {
				t.Fatalf("LeakedTenantSecrets: %v", err)
			}
			if !sameSet(got, want) {
				t.Fatalf("LeakedTenantSecrets(%q) = %v, want exactly %v: every tenant's row under the prefix, and none under %q", mine, got, want, other)
			}

			for _, tn := range tenants {
				RemoveTenantSecretsWithPrefix(t, be.DB, be.Dialect, tn, mine)
			}
			if got, err := leakedRows(ctx, be.DB, be.Dialect, mine); err != nil || len(got) != 0 {
				t.Fatalf("after RemoveTenantSecretsWithPrefix: got %v, %v; want nothing", got, err)
			}
			if got, err := leakedRows(ctx, be.DB, be.Dialect, other); err != nil || len(got) != len(tenants) {
				t.Fatalf("the other prefix's rows were removed with the first, or not seen: got %v, %v; want %d", got, err, len(tenants))
			}
		})
	}
}

func TestLeakedTenantSecretsRefusesToMeasureWithNoPrefix(t *testing.T) {
	if _, err := leakedRows(context.Background(), (*sql.DB)(nil), testutil.DialectMySQL); err == nil {
		t.Fatal("no prefix: want an error, not a clean answer")
	}
}

// leakedRows is leakedTenantSecrets without the table-present flag, for the
// cases that only care what was named.
func leakedRows(ctx context.Context, db *sql.DB, dialect testutil.Dialect, prefixes ...string) ([]string, error) {
	found, _, err := leakedTenantSecrets(ctx, db, dialect, prefixes...)
	return found, err
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

// The case cleat-review-69 found in cleat#3491: the migrations create a schema
// named for the role, so for that role the search_path starts somewhere other
// than the schema holding tenant_secrets. A table lookup that compared
// current_schema() with it said "no table" and the check returned clean
// without reading a row, with a leaked row present. This opens a SECOND
// connection pool whose search_path starts at an empty schema and requires the
// check to see the row all the same.
func TestLeakedTenantSecretsFindsTheTableWhenTheSearchPathStartsElsewhere(t *testing.T) {
	const prefix = "zz-3484-searchpath."
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		if be.Dialect != testutil.DialectPostgres {
			be.Cleanup()
			continue
		}
		t.Run(be.Name, func(t *testing.T) {
			t.Cleanup(be.Cleanup)
			ctx := context.Background()
			setupFull(t, be)

			const probe = "cleat3484_searchpath_probe"
			if _, err := be.DB.Exec(`CREATE SCHEMA IF NOT EXISTS ` + probe); err != nil {
				t.Fatalf("create the empty schema: %v", err)
			}
			t.Cleanup(func() { be.DB.Exec(`DROP SCHEMA IF EXISTS ` + probe) }) //nolint:errcheck // best-effort cleanup

			u, err := url.Parse(testutil.PostgresTestDSN())
			if err != nil {
				t.Fatalf("parse the DSN: %v", err)
			}
			q := u.Query()
			q.Set("search_path", probe+",public")
			u.RawQuery = q.Encode()
			far, err := sql.Open("postgres", u.String())
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { far.Close() })
			var cur string
			if err := far.QueryRow(`SELECT current_schema()`).Scan(&cur); err != nil || cur != probe {
				t.Fatalf("precondition: current_schema() = %q, %v; want %q (this test would not exercise the case)", cur, err, probe)
			}

			key := make([]byte, 32)
			ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: key})
			if err != nil {
				t.Fatalf("key ring: %v", err)
			}
			store := engine.NewSecretStoreWithRing(be.DB, string(be.Dialect), ring)
			tn := engine.DefaultTenantUUID
			t.Cleanup(func() { RemoveTenantSecretsWithPrefix(t, be.DB, be.Dialect, tn, prefix) })
			if err := store.PutSecret(auth.WithTenantID(ctx, uuid.MustParse(tn)), tn, prefix+"x", "v"); err != nil {
				t.Fatalf("seed: %v", err)
			}

			got, present, err := leakedTenantSecrets(ctx, far, testutil.DialectPostgres, prefix)
			if err != nil {
				t.Fatalf("leakedTenantSecrets: %v", err)
			}
			if !present || len(got) != 1 {
				t.Fatalf("search_path %s,public: present=%v got=%v; want the table found and the row named", probe, present, got)
			}
		})
	}
}
