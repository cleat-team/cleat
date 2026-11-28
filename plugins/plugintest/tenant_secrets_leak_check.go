package plugintest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// leakedTenantSecrets lists the tenant_secrets rows on db whose name starts
// with any of prefixes, as "tenant/name". cleat#3484.
//
// This is the detector for the defect cleat#3474 was: a plugin test that seals
// a signing secret on a shared database and leaves it there. The reseal command
// found it only when its own test happened to run AFTER the leaking package on
// the same database, which CI never does (separate jobs, separate databases).
// This reads the rows themselves, so it does not depend on order.
//
// NO ANSWER IS NOT A CLEAN ANSWER, in three places, because each reads zero for
// a reason that is not "nothing leaked":
//   - SQL Server filters tenant_secrets to the tenant in SESSION_CONTEXT, so a
//     connection that names none counts 0 rows through the same policy that
//     hides them. It is read once per tenant in admin.tenants (tenant_secrets
//     has a foreign key to it, so no row can name a tenant that is not
//     listed), each on a connection pinned to that tenant, and an empty tenant
//     list is an error, not a clean result.
//   - PostgreSQL applies row-level security to a role that is not a superuser,
//     so a non-superuser reads 0 the same way. That is an error too.
//   - A database that has no tenant_secrets table has nothing to leak: that is
//     a clean answer, and the only one reached without reading a row.
//
// It also reports whether the table was there at all, so a caller can SAY "no
// tenant_secrets table" instead of printing the same nothing it prints when it
// read the table and found no row.
//
// Unexported on purpose: RunAndCheckTenantSecretLeaks is the one caller outside
// this package's tests, and an exported function whose only callers are tests is
// what the dead-export guard exists to refuse.
func leakedTenantSecrets(ctx context.Context, db *sql.DB, dialect testutil.Dialect, prefixes ...string) (found []string, tablePresent bool, err error) {
	if len(prefixes) == 0 {
		return nil, false, fmt.Errorf("leakedTenantSecrets: no prefix given, so it would measure nothing")
	}
	has, err := hasTenantSecretsTable(ctx, db, dialect)
	if err != nil {
		return nil, false, err
	}
	if !has {
		return nil, false, nil
	}
	found, err = readLeaked(ctx, db, dialect, prefixes)
	return found, true, err
}

func readLeaked(ctx context.Context, db *sql.DB, dialect testutil.Dialect, prefixes []string) ([]string, error) {

	var found []string
	switch dialect {
	case testutil.DialectPostgres:
		var super string
		if err := db.QueryRowContext(ctx, `SELECT current_setting('is_superuser')`).Scan(&super); err != nil {
			return nil, fmt.Errorf("leakedTenantSecrets: is_superuser: %w", err)
		}
		if super != "on" {
			return nil, fmt.Errorf("leakedTenantSecrets: UNMEASURED: the connection is not a superuser, so row-level security would hide other tenants' rows and read as a clean result")
		}
		for _, p := range prefixes {
			rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT tenant_id::text, name FROM tenant_secrets WHERE LEFT(name, %d) = $1`, len(p)), p)
			if err != nil {
				return nil, fmt.Errorf("leakedTenantSecrets: %w", err)
			}
			found, err = appendPairs(found, rows)
			if err != nil {
				return nil, err
			}
		}
	case testutil.DialectMySQL:
		for _, p := range prefixes {
			rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT tenant_id, name FROM tenant_secrets WHERE LEFT(name, %d) = ?`, len(p)), p)
			if err != nil {
				return nil, fmt.Errorf("leakedTenantSecrets: %w", err)
			}
			found, err = appendPairs(found, rows)
			if err != nil {
				return nil, err
			}
		}
	case testutil.DialectMSSQL:
		trows, err := db.QueryContext(ctx, `SELECT CAST(tenant_id AS NVARCHAR(36)) FROM admin.tenants`)
		if err != nil {
			return nil, fmt.Errorf("leakedTenantSecrets: list tenants: %w", err)
		}
		var tenants []string
		for trows.Next() {
			var id string
			if err := trows.Scan(&id); err != nil {
				trows.Close()
				return nil, err
			}
			tenants = append(tenants, id)
		}
		if err := trows.Err(); err != nil {
			trows.Close()
			return nil, err
		}
		trows.Close()
		if len(tenants) == 0 {
			return nil, fmt.Errorf("leakedTenantSecrets: UNMEASURED: admin.tenants lists no tenant, so no connection could be scoped to read tenant_secrets and the count would be 0 whatever it holds")
		}
		for _, tenant := range tenants {
			conn, err := db.Conn(ctx)
			if err != nil {
				return nil, fmt.Errorf("leakedTenantSecrets: %w", err)
			}
			if _, err := conn.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant); err != nil {
				conn.Close()
				return nil, fmt.Errorf("leakedTenantSecrets: scope to tenant %s: %w", tenant, err)
			}
			for _, p := range prefixes {
				rows, err := conn.QueryContext(ctx, fmt.Sprintf(`SELECT CAST(tenant_id AS NVARCHAR(36)), name FROM tenant_secrets WHERE LEFT(name, %d) = @p1`, len(p)), p)
				if err != nil {
					conn.Close()
					return nil, fmt.Errorf("leakedTenantSecrets: %w", err)
				}
				found, err = appendPairs(found, rows)
				if err != nil {
					conn.Close()
					return nil, err
				}
			}
			conn.Close()
		}
	default:
		return nil, fmt.Errorf("leakedTenantSecrets: unknown dialect %v", dialect)
	}
	sort.Strings(found)
	return found, nil
}

func appendPairs(out []string, rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	for rows.Next() {
		var tenant, name string
		if err := rows.Scan(&tenant, &name); err != nil {
			return nil, err
		}
		// SQL Server renders a uniqueidentifier in upper case.
		out = append(out, strings.ToLower(tenant)+"/"+name)
	}
	return out, rows.Err()
}

func hasTenantSecretsTable(ctx context.Context, db *sql.DB, dialect testutil.Dialect) (bool, error) {
	var q string
	switch dialect {
	case testutil.DialectPostgres:
		// to_regclass resolves the name the way the statement below will, through
		// the connection's search_path. information_schema compared against
		// current_schema() did not: the migrations create a schema named for the
		// role, so for that role current_schema() is not the schema holding the
		// table, the lookup said "no table", and the check returned clean without
		// reading a row (found in review of cleat#3491).
		q = `SELECT CASE WHEN to_regclass('tenant_secrets') IS NULL THEN 0 ELSE 1 END`
	case testutil.DialectMySQL:
		q = `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'tenant_secrets'`
	case testutil.DialectMSSQL:
		q = `SELECT CASE WHEN OBJECT_ID(N'tenant_secrets', N'U') IS NULL THEN 0 ELSE 1 END`
	default:
		return false, fmt.Errorf("unknown dialect %v", dialect)
	}
	var n int
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return false, fmt.Errorf("leakedTenantSecrets: look for tenant_secrets: %w", err)
	}
	return n > 0, nil
}

// RunAndCheckTenantSecretLeaks is a TestMain body: it runs the package's tests,
// then fails the package if any tenant_secrets row whose name starts with one
// of prefixes is left on a SHARED database the run was configured with
// (CLEAT_TEST_POSTGRES or CLEAT_TEST_DB, CLEAT_TEST_MYSQL, CLEAT_TEST_MSSQL).
//
//	func TestMain(m *testing.M) {
//		os.Exit(plugintest.RunAndCheckTenantSecretLeaks(m, "notifications.webhook_secret."))
//	}
//
// A dialect whose variable is unset is not checked: there is no shared database
// to have leaked into. A dialect whose variable IS set and cannot be read is
// reported UNMEASURED and fails the package, because a canary that cannot look
// is not one that found nothing. -short skips it with the rest of the database
// tests. Per-suite PostgreSQL databases (testutil.SuiteTestDB) are not checked:
// they persist by design and nothing else reads them.
func RunAndCheckTenantSecretLeaks(m *testing.M, prefixes ...string) int {
	code := m.Run()
	if testing.Short() {
		return code
	}
	ctx := context.Background()
	failed := false
	for _, c := range []struct {
		dialect testutil.Dialect
		driver  string
		dsn     func() string
	}{
		{testutil.DialectPostgres, "postgres", func() string {
			if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
				return ""
			}
			return testutil.PostgresTestDSN()
		}},
		{testutil.DialectMySQL, "mysql", func() string { return os.Getenv("CLEAT_TEST_MYSQL") }},
		{testutil.DialectMSSQL, "sqlserver", func() string { return os.Getenv("CLEAT_TEST_MSSQL") }},
	} {
		dsn := c.dsn()
		if dsn == "" {
			continue
		}
		db, err := sql.Open(c.driver, dsn)
		if err == nil {
			err = db.PingContext(ctx)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "UNMEASURED: tenant_secrets leak check on %s: %v\n  This is a failure of the check, not a finding about the tests (cleat#3484).\n", c.dialect, err)
			failed = true
			continue
		}
		leaked, present, err := leakedTenantSecrets(ctx, db, c.dialect, prefixes...)
		db.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "UNMEASURED: tenant_secrets leak check on %s: %v\n  This is a failure of the check, not a finding about the tests (cleat#3484).\n", c.dialect, err)
			failed = true
			continue
		}
		if len(leaked) == 0 {
			// A clean result must be visible as a result: without this line a log
			// cannot tell "looked at every tenant and found nothing" from "never
			// looked" (cleat#3484 review).
			if present {
				fmt.Fprintf(os.Stderr, "tenant_secrets leak check: %s: read tenant_secrets, no row with prefix %q left\n", c.dialect, prefixes)
			} else {
				fmt.Fprintf(os.Stderr, "tenant_secrets leak check: %s: no tenant_secrets table on this database, nothing to leak\n", c.dialect)
			}
		}
		if len(leaked) > 0 {
			fmt.Fprintf(os.Stderr, "LEAKED tenant_secrets on %s: %d row(s) left on the shared database by this package's tests (cleat#3484):\n", c.dialect, len(leaked))
			for _, l := range leaked {
				fmt.Fprintf(os.Stderr, "  %s\n", l)
			}
			fmt.Fprintf(os.Stderr, "  Sealed under a throwaway key ring, they make the global reseal command exit 1 for every other process.\n  Remove them with plugintest.RemoveTenantSecretsWithPrefix, deferred after be.Cleanup().\n")
			failed = true
		}
	}
	if failed && code == 0 {
		return 1
	}
	return code
}
