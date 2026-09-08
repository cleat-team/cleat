package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/auditlog"
	"github.com/google/uuid"
)

// TestAnAuditEventCanActuallyBeInserted is cleat#958.
//
// The audit log had never recorded a row on MySQL. Its migration declares
//
//	postgres    id UUID             PRIMARY KEY DEFAULT gen_random_uuid()
//	sqlserver   id UNIQUEIDENTIFIER PRIMARY KEY DEFAULT NEWID()
//	mysql       id CHAR(36)         PRIMARY KEY               <-- no default
//
// and the insert omitted the column, so every event on MySQL failed with
// "Field 'id' doesn't have a default value". The error was logged and
// swallowed, and an empty audit table is indistinguishable from a quiet
// system -- so it stayed invisible for as long as the plugin has existed.
//
// WHY THE EXISTING ALL-DIALECT TEST DID NOT CATCH IT, which is the part worth
// keeping. TestPluginMigrations_AllDialects runs every plugin's migrations on
// every dialect and then checks that each CREATE TABLE produced a table:
//
//	exists, err := tableExists(ctx, db, backend.dialect, table)
//
// On MySQL that passes. The table is there; it is the INSERT that cannot
// succeed. "The schema was created" and "the schema is usable" are different
// claims, and a table-existence check cannot see a missing default, a wrong
// type, or a constraint nothing can satisfy. This test asks the second
// question for the one statement the plugin actually issues.
func TestAnAuditEventCanActuallyBeInserted(t *testing.T) {
	for _, backend := range pluginTestBackends {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			// Same gate, same wording, as TestPluginMigrations_AllDialects:
			// live for mysql/mssql, provably dead for postgres (whose
			// enabled() hardcodes true, with the real reachability check
			// inside setup -> BootstrapScratchDB, which Fatals on
			// configured-but-unreachable and Skips on nothing-configured).
			if !backend.enabled() {
				t.Skipf("%s not available: set CLEAT_TEST_%s or start a local instance",
					backend.name, strings.ToUpper(backend.name))
			}

			db, teardown := backend.setup(t)
			defer teardown()
			ctx := context.Background()

			lp := &plugin.LoadedPlugin{Plugin: auditlog.New(), Healthy: true}
			if err := plugin.RunMigrations(ctx, db, backend.dialect, nil,
				[]*plugin.LoadedPlugin{lp}); err != nil {
				t.Fatalf("RunMigrations: %v", err)
			}

			// The plugin's own statement, verbatim from recordAudit.
			id := uuid.NewString()
			tenant := uuid.NewString()
			_, err := db.ExecContext(ctx, plugin.Rebind(`
				INSERT INTO audit_events (id, tenant_id, method, path, status_code, user_id, ip_address, user_agent, duration_ms)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			`, backend.dialect), id, tenant, "GET", "/api/workflows", 200, "", "127.0.0.1", "test", 3)
			if err != nil {
				t.Fatalf("inserting an audit event failed: %v\n\n"+
					"The table exists -- TestPluginMigrations_AllDialects asserts that -- "+
					"but a row cannot be written to it. On MySQL this was "+
					"\"Field 'id' doesn't have a default value\", because that dialect's "+
					"migration declares id CHAR(36) PRIMARY KEY with no default while the "+
					"other two generate one. recordAudit logs and swallows this error, so "+
					"the audit log recorded nothing and looked like a quiet system.", err)
			}

			var n int
			if err := db.QueryRowContext(ctx, plugin.Rebind(
				`SELECT count(*) FROM audit_events WHERE id = $1`, backend.dialect), id,
			).Scan(&n); err != nil {
				t.Fatalf("counting the inserted event: %v", err)
			}
			if n != 1 {
				t.Errorf("after a successful insert the audit log holds %d rows with that id, want 1 -- "+
					"the statement reported success and the row is not there", n)
			}
		})
	}
}
