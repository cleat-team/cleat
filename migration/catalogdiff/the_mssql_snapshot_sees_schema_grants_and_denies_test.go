package catalogdiff

// cleat#2446's census. mssql.go's grants query read dp.class = 1
// (OBJECT_OR_COLUMN) and dp.state = 'G' (GRANT) only. Neither matches
// migrations/mssql/008_app_login.sql's cleat_app_role security model: its
// four GRANTs are SCHEMA-scoped (class = 3; major_id is a schema_id, not an
// object_id, so the old query's JOIN to sys.objects matched none of them),
// and its one DENY -- the statement that stops cleat_app_role writing
// dbo.deployment_secrets, overriding the schema GRANT above it -- is
// state = 'D', which the old filter excluded outright.
//
// Measured on a database built from the current chain before this fix:
// Snapshot's Grants was empty for a role carrying eleven live GRANT/DENY
// rows, and -mode=diff reported 0 differences between that database and a
// second one with every one of those eleven permissions explicitly revoked.
// Same method as the_mssql_snapshot_sees_the_attributes_it_selects_test.go
// (cleat#2447) and the_mysql_snapshot_sees_a_trigger_test.go: establish the
// difference independently of Snapshot, then require Diff to report it.

import (
	"database/sql"
	"testing"
)

func TestTheMSSQLSnapshotSeesSchemaGrantsAndDenies(t *testing.T) {
	// 1. A SCHEMA-level GRANT (class = 3). The old query's JOIN to
	// sys.objects on major_id only matches class = 1 rows -- a schema-level
	// grant's major_id is a schema_id, which never matches an object_id, so
	// the row simply does not appear in the result set at all.
	mssqlDiffCase(t, "schema-level-grant",
		`CREATE ROLE r_2446_schema;`,
		`CREATE ROLE r_2446_schema;
		 GRANT SELECT ON SCHEMA::dbo TO r_2446_schema;`,
		"GRANT SELECT ON SCHEMA dbo",
		func(t *testing.T, a, b *sql.DB) string {
			var cntA, cntB int
			q := `SELECT COUNT(*) FROM sys.database_permissions dp
			       JOIN sys.database_principals g ON g.principal_id = dp.grantee_principal_id
			       WHERE g.name = 'r_2446_schema' AND dp.class = 3`
			if err := a.QueryRow(q).Scan(&cntA); err != nil {
				t.Fatalf("read A's schema-permission count: %v", err)
			}
			if err := b.QueryRow(q).Scan(&cntB); err != nil {
				t.Fatalf("read B's schema-permission count: %v", err)
			}
			if cntA == cntB {
				return ""
			}
			return "A has 0 schema-level grants on r_2446_schema, B has 1"
		})

	// 2. An object-level DENY (state = 'D'), overriding a GRANT on the same
	// object -- the exact shape of 008_app_login.sql's security model: a
	// role that can read a table but has writes explicitly denied. The old
	// query's state = 'G' filter saw the GRANT and never looked for the DENY
	// that overrides it.
	mssqlDiffCase(t, "object-level-deny-overrides-grant",
		`CREATE TABLE t (id INT PRIMARY KEY);
		 CREATE ROLE r_2446_deny;
		 GRANT SELECT, UPDATE ON t TO r_2446_deny;`,
		`CREATE TABLE t (id INT PRIMARY KEY);
		 CREATE ROLE r_2446_deny;
		 GRANT SELECT, UPDATE ON t TO r_2446_deny;
		 DENY UPDATE ON t TO r_2446_deny;`,
		"DENY UPDATE ON OBJECT dbo.t",
		func(t *testing.T, a, b *sql.DB) string {
			var cntA, cntB int
			q := `SELECT COUNT(*) FROM sys.database_permissions dp
			       JOIN sys.database_principals g ON g.principal_id = dp.grantee_principal_id
			       WHERE g.name = 'r_2446_deny' AND dp.state = 'D'`
			if err := a.QueryRow(q).Scan(&cntA); err != nil {
				t.Fatalf("read A's deny count: %v", err)
			}
			if err := b.QueryRow(q).Scan(&cntB); err != nil {
				t.Fatalf("read B's deny count: %v", err)
			}
			if cntA == cntB {
				return ""
			}
			return "A has 0 DENY rows for r_2446_deny, B has 1"
		})
}
