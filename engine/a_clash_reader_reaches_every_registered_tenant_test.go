package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3629, owner decision D59-b: the three ways the clash reader (migration 010) can be wrong without
// the publish tests above noticing.
//
//	a sharded publish asks EVERY shard before it writes ANY, so a clash on a later shard does not leave
//	  the earlier ones published
//	a PostgreSQL reader whose owner does NOT bypass row-level security still reaches the tenants that
//	  have an admin.tenants row, restores the caller's tenant setting, and does not see the rest
//	the reader leaves the caller's tenant context as it found it (SQL Server keeps it on the CONNECTION,
//	  which outlives the call, so a procedure that forgot would poison the next statement on a pooled one)

// seedTenantName puts a tenant's own workflow name on one shard, and registers the tenant there, which
// is the state a deploy leaves before the library exists. The owner connection is used: SQL Server
// needs the tenant in the session for the row to pass its block predicate.
func seedTenantName(t *testing.T, owner *sql.DB, d Dialect, tenant, name string, version int) {
	t.Helper()
	ctx := context.Background()
	conn, err := owner.Conn(ctx)
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	defer conn.Close()
	switch d {
	case DialectMSSQL:
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`IF NOT EXISTS (SELECT 1 FROM admin.tenants WHERE tenant_id = @p1) INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES (@p1, @p2, @p2)`, []any{tenant, "t-" + tenant}},
			{`EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, []any{tenant}},
			{`INSERT INTO workflow_defs (tenant_id, name, version, wasm_bytes, abi_version) VALUES (@p1, @p2, @p3, @p4, 2)`, []any{tenant, name, version, []byte("own")}},
		} {
			if _, err := conn.ExecContext(ctx, q.sql, q.args...); err != nil {
				t.Fatalf("UNMEASURED: seeding %s on SQL Server: %v", name, err)
			}
		}
	default:
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES ($1, $2, $2) ON CONFLICT (tenant_id) DO NOTHING`, []any{tenant, "t-" + tenant}},
			{`INSERT INTO workflow_defs (tenant_id, name, version, wasm_bytes, abi_version) VALUES ($1, $2, $3, $4, 2)`, []any{tenant, name, version, []byte("own")}},
		} {
			if _, err := conn.ExecContext(ctx, q.sql, q.args...); err != nil {
				t.Fatalf("UNMEASURED: seeding %s on PostgreSQL: %v", name, err)
			}
		}
	}
}

func TestAShardedPublishThatClashesOnAnyShardWritesNothingOnAnyShard(t *testing.T) {
	for _, tc := range publishDialects {
		t.Run(tc.name, func(t *testing.T) {
			f := newLibraryShardFixture(t, tc)
			ctx := context.Background()
			all := func(lib string) [3]int {
				return [3]int{countLibraryRows(t, f.owner[0], tc.edialect, lib), countLibraryRows(t, f.owner[1], tc.edialect, lib), countLibraryRows(t, f.owner[2], tc.edialect, lib)}
			}
			for _, which := range []int{0, 1, 2} {
				t.Run(fmt.Sprintf("a tenant holds the names on shard %d only", which+1), func(t *testing.T) {
					lib := uniqueLibrary("shardclash")
					seedTenantName(t, f.owner[which], tc.edialect, pfxTenantA, lib+".parent", 1)

					_, err := PublishLibraryBundleToShards(ctx, shardList(f.pub[0], f.pub[1], f.pub[2]), tc.edialect, shardedReq(lib))
					if !errors.Is(err, ErrLibraryPublishRefused) {
						t.Fatalf("publishing %q with a tenant's %s.parent on shard %d: got %v, want ErrLibraryPublishRefused", lib, lib, which+1, err)
					}
					for _, want := range []string{shardList(f.pub[0], f.pub[1], f.pub[2])[which].Name, pfxTenantA, lib + ".parent", "nothing was written to any shard"} {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("the refusal does not say %q:\n%s", want, err)
						}
					}
					if got := all(lib); got != [3]int{} {
						t.Errorf("rows of the library per shard are %v after a refused publish, want none anywhere", got)
					}
				})
			}
			t.Run("control: with no clash anywhere it publishes to every shard", func(t *testing.T) {
				lib := uniqueLibrary("shardclear")
				if _, err := PublishLibraryBundleToShards(ctx, shardList(f.pub[0], f.pub[1], f.pub[2]), tc.edialect, shardedReq(lib)); err != nil {
					t.Fatalf("publish with nothing in the way: %v", err)
				}
				if got := all(lib); got != [3]int{5, 5, 5} {
					t.Errorf("rows per shard are %v, want 5 on each", got)
				}
			})
		})
	}
}

// A PostgreSQL reader whose owner is subject to row-level security. The ordinary rigs connect as a
// superuser, whose functions bypass the tenant policy and read every tenant directly, so they never
// run the branch that walks admin.tenants. This makes the function's owner a role with no bypass, for
// one transaction that is rolled back, and calls it.
func TestAPostgresClashReaderWhoseOwnerCannotBypassRowSecurityWalksTheRegisteredTenants(t *testing.T) {
	rig := newPostgresLibRig(t)
	c := newPrefixCase(t, rig)
	lib := c.library(t, "nobypass")
	for _, id := range []string{pfxTenantA, pfxTenantB} {
		if err := c.deploy(id, lib+".mine", 1); err != nil {
			t.Fatalf("UNMEASURED: seeding for %s: %v", id, err)
		}
	}
	// Only tenant A is registered. Tenant B has rows and no admin.tenants row.
	rig.exec(t, "", `INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES (?, ?, ?) ON CONFLICT (tenant_id) DO NOTHING`, pfxTenantA, "t-"+pfxTenantA, "t-"+pfxTenantA)
	t.Cleanup(func() { rig.exec(t, "", `DELETE FROM admin.tenants WHERE tenant_id = ?`, pfxTenantA) })

	ctx := context.Background()
	tx, err := rig.admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	owner := "zz_clash_reader_owner"
	for _, stmt := range []string{
		`CREATE ROLE ` + owner + ` NOLOGIN NOSUPERUSER NOBYPASSRLS`,
		`GRANT USAGE, CREATE ON SCHEMA ` + rig.schema + ` TO ` + owner,
		`GRANT USAGE ON SCHEMA admin TO ` + owner,
		`GRANT SELECT ON workflow_defs TO ` + owner,
		`GRANT SELECT ON admin.tenants TO ` + owner,
		`ALTER FUNCTION library_clashing_names(text) OWNER TO ` + owner,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("UNMEASURED: %s: %v", stmt, err)
		}
	}
	var bypass bool
	if err := tx.QueryRowContext(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = $1`, owner).Scan(&bypass); err != nil || bypass {
		t.Fatalf("UNMEASURED: the function's owner bypasses row-level security (%v, %v), so this does not run the walk", bypass, err)
	}

	const sentinel = "99999999-9999-4999-8999-999999999999"
	if _, err := tx.ExecContext(ctx, `SELECT set_config('cleat.tenant_id', $1, true)`, sentinel); err != nil {
		t.Fatal(err)
	}
	clashes, err := libraryPrefixClashes(ctx, tx, DialectPostgres, lib)
	if err != nil {
		t.Fatalf("the reader failed under an owner that does not bypass row security: %v", err)
	}
	got := map[string]bool{}
	for _, cl := range clashes {
		got[cl.TenantID] = true
	}
	if !got[pfxTenantA] {
		t.Errorf("the walk did not reach registered tenant A: %v", clashes)
	}
	if got[pfxTenantB] {
		t.Errorf("the walk saw tenant B, which has no admin.tenants row: %v. If this is now true the limitation in migration 010's header is out of date", clashes)
	}
	var after string
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('cleat.tenant_id', true)`).Scan(&after); err != nil || after != sentinel {
		t.Errorf("the caller's cleat.tenant_id is %q after the call, want it put back to %q (%v)", after, sentinel, err)
	}
}

// SQL Server holds the tenant in SESSION_CONTEXT, which belongs to the connection and outlives the call.
// A reader that left its last tenant behind would hand the next statement on a pooled connection a
// tenant it never chose.
func TestTheSQLServerClashReaderLeavesTheCallersTenantContextAsItFoundIt(t *testing.T) {
	rig := newMSSQLLibRig(t)
	c := newPrefixCase(t, rig)
	c.tdialect = testutil.DialectMSSQL
	c.registerTenants(t)
	lib := c.library(t, "ctxrestore")
	if err := c.deploy(pfxTenantA, lib+".mine", 1); err != nil {
		t.Fatalf("UNMEASURED: seeding: %v", err)
	}
	c.openPublisher(t)
	ctx := context.Background()
	conn, err := c.pub.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	read := func() string {
		var v sql.NullString
		if err := conn.QueryRowContext(ctx, `SELECT CAST(SESSION_CONTEXT(N'tenant_id') AS NVARCHAR(100))`).Scan(&v); err != nil {
			t.Fatalf("reading the session context: %v", err)
		}
		return v.String
	}
	for _, before := range []string{"", "88888888-8888-4888-8888-888888888888"} {
		if before != "" {
			if _, err := conn.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, before); err != nil {
				t.Fatal(err)
			}
		}
		if before == "" && read() != "" {
			t.Fatalf("UNMEASURED: a fresh connection already has a tenant context %q", read())
		}
		clashes, err := libraryPrefixClashes(ctx, conn, DialectMSSQL, lib)
		if err != nil || len(clashes) != 1 {
			t.Fatalf("the reader returned %v, %v; want tenant A's one name", clashes, err)
		}
		if after := read(); after != before {
			t.Errorf("the session context was %q before the reader and %q after: the reader left its own tenant behind", before, after)
		}
	}
}
