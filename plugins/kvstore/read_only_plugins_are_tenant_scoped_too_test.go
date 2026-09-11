package kvstore

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// A read-only plugin must be able to read a TenantScoped table. cleat#1285.
//
// There are two plugin.PluginDB implementations over the pool -- SQLDBAdapter
// and ReadOnlyDB -- and #1280 scoped only the first. The consequence was not a
// leak but a hard failure: a plugin granted DatabaseAccessReadOnly
// (cmd/cleat-worker/main.go, getPluginReadOnlyDB) got
//
//	cleat.tenant_id is not set -- tenant context required for RLS-scoped query
//
// on every read, WITH a tenant in the request context, because nothing set the
// value its policy filters on.
//
// Nothing was broken when this was found -- kvstore takes read-write and no
// built-in plugin declares read-only -- which is the argument for the test
// rather than against it. #1278's work spreads TenantScoped to the remaining
// plugins, and the first read-only adopter would have hit this with nothing
// pointing at the cause.
func TestAReadOnlyPluginCanReadATenantScopedTable(t *testing.T) {
	pg := postgresBackend(t)
	defer pg.Cleanup()

	ctx := context.Background()
	loaded := []*plugin.LoadedPlugin{{Plugin: &Plugin{}, Healthy: true}}
	if err := plugin.RunMigrations(ctx, pg.DB, plugin.DialectPostgres, nil, loaded); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	rlsDB := testutil.OpenPostgresRLSTestDB(t, pg.DB)
	defer func() { _ = rlsDB.Close() }()

	tenant := uuid.New()
	ctxT := auth.WithTenantID(context.Background(), tenant)
	rw := &engine.SQLDBAdapter{DB: rlsDB, Dialect: plugin.DialectPostgres}
	ro := &engine.ReadOnlyDB{Inner: rlsDB, Dialect: plugin.DialectPostgres}

	// Seed through the read-write adapter. THE ROW IS LOAD-BEARING: a policy
	// USING clause is a row-level predicate, so on an empty table it is never
	// evaluated, assert_tenant_set() never fires, and a read succeeds whether
	// the policy is right, wrong or absent. The first probe written for this
	// bug ran count(*) against a freshly-migrated table, reported both
	// adapters succeeding, and measured nothing.
	if _, err := rw.Exec(ctxT,
		`INSERT INTO kv_store (tenant_id, key, value) VALUES ($1, $2, $3)`,
		tenant.String(), "ro-visible", `"v"`); err != nil {
		t.Fatalf("seed a row to make the policy observable: %v", err)
	}

	t.Run("with a tenant in context it can read", func(t *testing.T) {
		var n int
		if err := ro.QueryRow(ctxT, `SELECT count(*) FROM kv_store`).Scan(&n); err != nil {
			t.Fatalf("a read-only plugin could not read its own tenant's rows: %v\n\n"+
				"ReadOnlyDB is not setting cleat.tenant_id, so the policy installed "+
				"by TenantScoped refuses every statement it issues.", err)
		}
		if n != 1 {
			t.Errorf("read-only adapter counted %d rows, want 1", n)
		}
	})

	t.Run("Query is scoped as well as QueryRow", func(t *testing.T) {
		rows, err := ro.Query(ctxT, `SELECT key FROM kv_store`)
		if err != nil {
			t.Fatalf("read-only Query: %v", err)
		}
		defer func() { _ = rows.Close() }()
		seen := 0
		for rows.Next() {
			seen++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterating: %v", err)
		}
		if seen != 1 {
			t.Errorf("read-only Query returned %d rows, want 1", seen)
		}
	})

	// Fail-closed is preserved, not traded away for the fix.
	t.Run("with no tenant in context it is still refused", func(t *testing.T) {
		var n int
		err := ro.QueryRow(context.Background(), `SELECT count(*) FROM kv_store`).Scan(&n)
		if err == nil {
			t.Fatal("a read-only statement with no tenant in context was allowed; " +
				"the fix for #1285 must not scope a statement that has no tenant")
		}
		if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
			t.Fatalf("refused, but not by the tenant policy: %v", err)
		}
	})

	// The adapter is still read-only. A tenant-scoped transaction must not
	// become a writable one.
	t.Run("it is still read-only", func(t *testing.T) {
		if _, err := ro.Exec(ctxT, `DELETE FROM kv_store`); err == nil {
			t.Error("ReadOnlyDB.Exec was allowed")
		}
		tx, err := ro.Begin(ctxT)
		if err != nil {
			t.Fatalf("read-only Begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(ctxT,
			`INSERT INTO kv_store (tenant_id, key, value) VALUES ($1,$2,$3)`,
			tenant.String(), "written", `"v"`); err == nil {
			t.Error("a write succeeded inside a read-only tenant-scoped transaction")
		}
	})
}
