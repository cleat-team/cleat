package main

// coreTables is every table cleat's own migrations create, schema-qualified.
//
// It is the population `check-db` reports on, and it is NOT maintained by
// hand: TestCoreTablesMatchTheMigrations parses migrations/postgres/*.sql and
// fails if this slice and those files disagree in either direction. The list it
// replaced had drifted both ways at once -- four names that never existed, and
// ten real tables it omitted -- which is what a hand-kept copy of a schema does
// given enough time. cleat#1216.
//
// PLUGIN tables are deliberately absent. A plugin's tables exist only where
// that plugin is installed, so requiring them would report a correct database
// as incomplete -- the same false alarm this list is being fixed for, arriving
// from the opposite direction.
var coreTables = []string{
	"admin.orgs",
	"admin.plugin_tables",
	"admin.operator_api_keys",
	"admin.tenant_api_keys",
	"admin.tenant_roles",
	"admin.tenant_egress_allow",
	"admin.tenants",
	"admin.workers",
	"concurrency_keys",
	"event_history",
	"idempotency_keys",
	"payload_encryption_ever_enabled",
	"plugin_defs",
	"deployment_secrets",
	"queues",
	"queue_holders",
	"queue_rate_tokens",
	"slack_workspace",
	"tenant_domains",
	"tenant_secrets",
	"tenant_settings",
	"workflow_defs",
	"workflow_instances",
	"workflow_memory_samples",
	"workflow_memory_stats",
	"workflow_promises",
	"workflow_routing",
	"workflow_schedules",
	"workflow_signals",
	"workflow_tags",
	"workflow_update_requests",
}

// postgresOnlyTables is the subset of coreTables that exists ONLY in
// migrations/postgres, by design rather than by omission.
//
// Two readers of coreTables need to agree on this, not just the test:
// TestQualifiedTableMatchesEachDialectsMigrations (dialect_tables_test.go)
// checks every coreTables entry against all three dialects' migrations, which
// is right for a table every dialect is supposed to have and wrong for one
// that genuinely does not exist on two of them -- that test's own doc comment
// already distinguishes "the mapping is wrong" from "this dialect is
// genuinely missing the table", and this set is how it tells the two apart
// instead of reporting the second as the first. runCheckDB (checkdb.go) loops
// over the same slice to report operator-facing "TABLES: N accessible, M
// missing" output, and without the same skip it reported a false MISSING on
// every healthy non-PostgreSQL deployment the moment this entry existed --
// found by running check-db against a real, freshly-migrated MySQL database
// (cleat-review2, #2924 round 7).
//
// payload_encryption_ever_enabled (cleat#2324, migrations/postgres/008) is
// the first entry: encrypt-sensitive-payloads is a PostgreSQL-only feature --
// MySQL and SQL Server have no equivalent key-ring flag and no migration
// defining this table -- so a MySQL or SQL Server database lacking it is
// correct, not incomplete.
var postgresOnlyTables = map[string]bool{
	"payload_encryption_ever_enabled": true,
}
