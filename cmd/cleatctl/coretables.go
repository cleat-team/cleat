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
	"admin.plugin_tables",
	"admin.tenant_api_keys",
	"admin.tenant_roles",
	"admin.tenants",
	"concurrency_keys",
	"event_history",
	"idempotency_keys",
	"plugin_defs",
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
