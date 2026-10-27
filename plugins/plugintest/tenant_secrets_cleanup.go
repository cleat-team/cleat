package plugintest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// RemoveTenantSecretsWithPrefix deletes the tenant_secrets rows of one tenant
// whose name starts with prefix. cleat#3474.
//
// WHY A TEST NEEDS THIS. A plugin test that creates a source or a webhook
// through the plugin's own route seals a signing secret into tenant_secrets
// under the test's throwaway key ring. NewPluginTestBackends' Cleanup only
// closes the connection, and the MySQL and SQL Server databases are shared by
// every package, so the row outlives the test. The reseal command is global and
// every other process holds a different ring, so it reports the row as
// unreadable and exits 1: TestResealSecretsCommandConvergesAndIsIdempotent
// failed whenever it ran after plugins/notifications or plugins/webhookingest.
// Measured on a three-dialect run: those two packages left 3 and 11 rows on
// MySQL, 3 and 10 on PostgreSQL, 3 and 9 on SQL Server.
//
// BY PREFIX, NOT BY SNAPSHOT OR WIPE. Only engine may blanket-wipe a MySQL or
// SQL Server database (TestBlanketNonPostgresCleanupHasOneCaller), and a test
// does not know the ids its route generated. The names are the plugin's own
// (`<plugin>.<purpose>.<uuid>`), so a prefix reaches exactly what the plugin
// wrote and nothing a neighbouring package seeded.
//
// SQL Server hides the rows from a connection that has not named the tenant,
// and the setting and the DELETE must share one connection or the DELETE
// affects zero rows and reports success; so it pins one.
//
// Register it AFTER `defer be.Cleanup()`: deferred calls run last-in
// first-out, and this one needs the connection open.
func RemoveTenantSecretsWithPrefix(t *testing.T, db *sql.DB, dialect testutil.Dialect, tenantID, prefix string) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Errorf("RemoveTenantSecretsWithPrefix(%q): acquire a connection: %v", prefix, err)
		return
	}
	defer conn.Close()

	// LEFT(name, n) = prefix is spelled the same on all three dialects, and
	// unlike LIKE it has no wildcard characters to escape: the prefixes carry
	// underscores. n is the prefix's own length, formatted in rather than bound.
	var stmt string
	switch dialect {
	case testutil.DialectPostgres:
		stmt = fmt.Sprintf(`DELETE FROM tenant_secrets WHERE tenant_id = $1 AND LEFT(name, %d) = $2`, len(prefix))
	case testutil.DialectMySQL:
		stmt = fmt.Sprintf(`DELETE FROM tenant_secrets WHERE tenant_id = ? AND LEFT(name, %d) = ?`, len(prefix))
	case testutil.DialectMSSQL:
		stmt = fmt.Sprintf(`DELETE FROM tenant_secrets WHERE tenant_id = @p1 AND LEFT(name, %d) = @p2`, len(prefix))
		if _, err := conn.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenantID); err != nil {
			t.Errorf("RemoveTenantSecretsWithPrefix(%q): scope the connection to tenant %s: %v", prefix, tenantID, err)
			return
		}
	default:
		t.Errorf("RemoveTenantSecretsWithPrefix(%q): unknown dialect %v", prefix, dialect)
		return
	}
	if _, err := conn.ExecContext(ctx, stmt, tenantID, prefix); err != nil {
		t.Errorf("RemoveTenantSecretsWithPrefix(%q): %v", prefix, err)
	}
}
