package main

// cleat#3678 (D65). scopedRead now pins a connection on SQL Server, sets the tenant on it, reads inside a
// transaction on it, and clears the tenant afterwards. This runs it AS THE APPLICATION LOGIN, on every
// dialect it applies to, because that is the login the shipped worker uses and an owner connection would not
// notice a read the policy refuses: PostgreSQL as cleat_app (non-superuser, so RLS applies) and SQL Server
// as a login in cleat_app_role.
//
// Two tenants each own a host name. As the application login, a scoped read for tenant A must see A's host,
// must not see B's, and must read no other tenant's rows. Seeded as the owner, so the policy cannot be what
// let the rows in.

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine/testutil"
)

// mssqlAppLoginURL gives u a login of its own in cleat_app_role, named per run (a fixed name collides when
// two test processes share one server), and drops it when the test ends. u must already name the database.
// viewServerState adds VIEW SERVER STATE for a test that names a connection through sys.dm_exec_connections; it
// is a grant for the probe and does not touch anything scopedRead needs.
func mssqlAppLoginURL(t *testing.T, admin *sql.DB, u *url.URL, viewServerState bool) {
	t.Helper()
	ctx := context.Background()
	login := "cleat_scoped_" + strings.ReplaceAll(uuid.NewString()[:13], "-", "")
	const pw = "Scoped3678!pw1"
	stmts := []string{
		`CREATE LOGIN [` + login + `] WITH PASSWORD = N'` + pw + `', CHECK_POLICY = OFF`,
		`CREATE USER [` + login + `] FOR LOGIN [` + login + `]`,
		`ALTER ROLE [cleat_app_role] ADD MEMBER [` + login + `]`,
	}
	for _, q := range stmts {
		if _, err := admin.ExecContext(ctx, q); err != nil {
			t.Fatalf("UNMEASURED: setup: %v\n%s", err, q)
		}
	}
	if viewServerState {
		// A server-scope grant must be made from master. The connection that did it is DISCARDED, not
		// pooled, so no later statement on admin finds itself in master.
		conn, err := admin.Conn(ctx)
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		_, err = conn.ExecContext(ctx, `USE master`)
		if err == nil {
			_, err = conn.ExecContext(ctx, `GRANT VIEW SERVER STATE TO [`+login+`]`)
		}
		_ = conn.Raw(func(any) error { return sqldriver.ErrBadConn })
		_ = conn.Close()
		if err != nil {
			t.Fatalf("UNMEASURED: granting VIEW SERVER STATE: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`ALTER ROLE [cleat_app_role] DROP MEMBER [` + login + `]`,
			`DROP USER [` + login + `]`,
			`DROP LOGIN [` + login + `]`,
		} {
			_, _ = admin.ExecContext(context.Background(), q)
		}
	})
	u.User = url.UserPassword(login, pw)
}

// mssqlDatabaseURL returns the test DSN with the database set to the one admin is on.
func mssqlDatabaseURL(t *testing.T, admin *sql.DB) *url.URL {
	t.Helper()
	var dbName string
	if err := admin.QueryRowContext(context.Background(), `SELECT DB_NAME()`).Scan(&dbName); err != nil {
		t.Fatalf("UNMEASURED: reading the database name: %v", err)
	}
	u, err := url.Parse(os.Getenv("CLEAT_TEST_MSSQL"))
	if err != nil {
		t.Fatalf("UNMEASURED: parsing the DSN: %v", err)
	}
	qv := u.Query()
	qv.Set("database", dbName)
	u.RawQuery = qv.Encode()
	return u
}

func TestAScopedReadAsTheTenantLoginSeesOnlyItsOwnHosts(t *testing.T) {
	ctx := context.Background()
	hostA := "a-" + uuid.NewString() + ".scoped.invalid"
	hostB := "b-" + uuid.NewString() + ".scoped.invalid"
	tenantA, tenantB := uuid.New(), uuid.New()

	// tenant_domains.tenant_id is a foreign key to admin.tenants, so both tenants are provisioned first, as
	// the owner, and removed after the domain rows.
	provision := func(t *testing.T, owner *sql.DB, dialect string) {
		t.Helper()
		ts, err := auth.NewTenantStoreForDialect(owner, dialect)
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		ph := func(n int) string {
			if dialect == auth.DialectMSSQL {
				return "@p" + string(rune('0'+n))
			}
			return "$" + string(rune('0'+n))
		}
		for _, id := range []uuid.UUID{tenantA, tenantB} {
			id := id
			org := "scopedread-3678-" + uuid.NewString()
			if _, err := ts.ProvisionTenant(ctx, auth.ProvisionRequest{TenantID: id, OrgName: org, Creator: "scopedread-test"}); err != nil {
				t.Fatalf("UNMEASURED: provisioning a tenant: %v", err)
			}
			t.Cleanup(func() {
				_, _ = owner.ExecContext(context.Background(), `DELETE FROM admin.tenant_api_keys WHERE tenant_id = `+ph(1), id.String())
				_, _ = owner.ExecContext(context.Background(), `DELETE FROM admin.tenants WHERE tenant_id = `+ph(1), id.String())
				_, _ = owner.ExecContext(context.Background(), `DELETE FROM admin.orgs WHERE name = `+ph(1), org)
			})
		}
	}

	check := func(t *testing.T, app *sql.DB, dialect string) {
		t.Helper()
		ts, err := auth.NewTenantStoreForDialect(app, dialect)
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		for _, c := range []struct {
			name   string
			host   string
			tenant uuid.UUID
			want   bool
		}{
			{"its own host", hostA, tenantA, true},
			{"the other tenant's own host", hostB, tenantB, true},
			{"a host the other tenant owns", hostB, tenantA, false},
			{"a host owned by the first tenant, asked as the second", hostA, tenantB, false},
		} {
			got, err := ts.TenantForHost(ctx, c.host, c.tenant)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if got != c.want {
				t.Errorf("TenantForHost for %s = %v, want %v", c.name, got, c.want)
			}
		}
		// Twice in a row on a pool of one: the second read must not be affected by the first one's tenant.
		app.SetMaxOpenConns(1)
		for i := 0; i < 2; i++ {
			got, err := ts.TenantForHost(ctx, hostB, tenantA)
			if err != nil || got {
				t.Errorf("repeat %d: TenantForHost(a host the other tenant owns) = %v, %v, want false, nil", i, got, err)
			}
		}
	}

	t.Run("postgres", func(t *testing.T) {
		owner := testutil.SuiteTestDB(t, "cleat_worker")
		provision(t, owner, auth.DialectPostgres)
		for _, r := range []struct {
			host   string
			tenant uuid.UUID
		}{{hostA, tenantA}, {hostB, tenantB}} {
			if _, err := owner.ExecContext(ctx, `INSERT INTO tenant_domains (hostname, tenant_id) VALUES ($1, $2)`, r.host, r.tenant.String()); err != nil {
				t.Fatalf("UNMEASURED: seeding: %v", err)
			}
		}
		t.Cleanup(func() {
			_, _ = owner.ExecContext(context.Background(), `DELETE FROM tenant_domains WHERE hostname IN ($1, $2)`, hostA, hostB)
		})
		check(t, testutil.OpenPostgresRLSTestDB(t, owner), auth.DialectPostgres)
	})

	t.Run("sqlserver", func(t *testing.T) {
		admin := testutil.TestDB(t, testutil.DialectMSSQL)
		provision(t, admin, auth.DialectMSSQL)
		for _, r := range []struct {
			host   string
			tenant uuid.UUID
		}{{hostA, tenantA}, {hostB, tenantB}} {
			// As the owner, with the tenant's own context set on a pinned connection so the block predicate
			// admits the insert.
			conn, err := admin.Conn(ctx)
			if err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			_, err = conn.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, r.tenant.String())
			if err == nil {
				_, err = conn.ExecContext(ctx, `INSERT INTO tenant_domains (hostname, tenant_id) VALUES (@p1, @p2)`, r.host, r.tenant.String())
			}
			_, _ = conn.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = NULL`)
			_ = conn.Close()
			if err != nil {
				t.Fatalf("UNMEASURED: seeding: %v", err)
			}
		}
		t.Cleanup(func() {
			for _, r := range []struct {
				host   string
				tenant uuid.UUID
			}{{hostA, tenantA}, {hostB, tenantB}} {
				if conn, err := admin.Conn(context.Background()); err == nil {
					_, _ = conn.ExecContext(context.Background(), `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, r.tenant.String())
					_, _ = conn.ExecContext(context.Background(), `DELETE FROM tenant_domains WHERE hostname = @p1`, r.host)
					_, _ = conn.ExecContext(context.Background(), `EXEC sp_set_session_context @key = N'tenant_id', @value = NULL`)
					_ = conn.Close()
				}
			}
		})
		u := mssqlDatabaseURL(t, admin)
		mssqlAppLoginURL(t, admin, u, false)
		app, err := sql.Open("sqlserver", u.String())
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		t.Cleanup(func() { app.Close() })
		check(t, app, auth.DialectMSSQL)
	})
}
