package plugin

import "sort"

// cleat#2306. cleat-review measured uninstall-then-migrate for every loaded
// plugin, fresh database each time, 2026-09-25: PostgreSQL reversed all 18
// cleanly, MySQL failed 7 of 18 (a generic `DROP COLUMN/INDEX IF EXISTS`
// MySQL rejects outright), and SQL Server failed 17 of 18 (a security policy
// left referencing the table it should have been dropped before, a bare
// `DROP INDEX`, orphaned default constraints, stale syntax). None of that had
// ever run before #2290 gave RunDownMigrations its first caller.
//
// A failed reversal is not a no-op: RunDownMigrations executes newest-applied
// first and stops at the first error, so a broken Down partway through a
// chain leaves the schema in neither shape. On SQL Server that has already
// bricked a database beyond repair -- notifications' partial reversal left
// "Cannot find the object webhook_delivery (4902)" with no further migration
// able to run.
//
// Most of the rest DO recover with a follow-up --migrate-only, on either
// dialect, but not all: cleat#2306 phase 2's schema-equality check
// (cmd/cleat-worker/a_uninstall_down_chain_is_classified_on_every_dialect_test.go,
// migration/catalogdiff) found three pairs where the follow-up run returns
// no error yet the recovered schema is missing an object the failed Down
// destroyed -- plugin_migrations still records that migration as applied, so
// the recovery run has nothing pending to re-apply. Measured 2026-09-25:
// blobstore/MySQL (loses the workflow_blob_refs table), oauth-provider/MSSQL
// (loses oauth_sessions.nonce), webhook-ingest/MSSQL (loses
// webhook_events.error_msg). Those three are classified
// outcomeUnrecoverable in knownBrokenPluginDown, same as a pair whose
// follow-up run fails outright -- "the command exited 0" is not the bar,
// "the schema matches a clean install" is. Do not tell an operator
// --migrate-only repairs an uninstall for any (plugin, dialect) pair without
// checking that file's current classification first.
//
// So --uninstall-plugin refuses up front on MySQL and SQL Server for any
// plugin whose Down chain is not proven, end to end, by its own test against
// a real database of that dialect --
// plugins/scheduledbackup/a_v4_down_keeps_uninstall_working_test.go is the
// template (TestUninstallSchedulerBackupOnEveryDialect). PostgreSQL carries
// no such gate: cleat-review's measurement above is a clean 18 of 18, and
// nothing here second-guesses it.
//
// provenPluginDialects is therefore a claim about TEST COVERAGE, not about
// the SQL -- add a plugin's name here in the SAME PR as the test that proves
// it, never ahead of that test landing. cleat#2306's phase 2 is the
// table-driven version of this across every plugin;
// TestUninstallDownChainIsClassifiedOnEveryDialect
// (cmd/cleat-worker/a_uninstall_down_chain_is_classified_on_every_dialect_test.go)
// IS that proving test -- it drives RunMigrations, RunDownMigrations and a
// re-Up for every plugin listed here, on every dialect, against a real
// database, and fails if any of them stops passing. A plugin no longer needs
// its own dedicated file (scheduled-backup's predates the harness and stays
// as belt-and-suspenders); it needs a passing subtest in that one.
//
// The ten MySQL entries below beyond scheduled-backup were added
// 2026-09-25, moved out of that harness's own knownBrokenPluginDown map: the
// phase-2 sweep (cleat#2306) found their Down chains already clean on
// MySQL, matching cleat-review's #2290 measurement, and the harness itself
// is what proves it now.
var provenPluginDialects = map[string]map[Dialect]bool{
	"scheduled-backup": {DialectMySQL: true, DialectMSSQL: true},
	"audit-log":        {DialectMySQL: true},
	"datadog-export":   {DialectMySQL: true},
	"eventstore":       {DialectMySQL: true},
	"feature-flags":    {DialectMySQL: true},
	"kvstore":          {DialectMySQL: true},
	"notifications":    {DialectMySQL: true},
	"pagerduty-alert":  {DialectMySQL: true},
	"rate-limiter":     {DialectMySQL: true},
	"slack-notify":     {DialectMySQL: true},
	"tenant-quota":     {DialectMySQL: true},
}

// UninstallProvenOnDialect reports whether pluginName's Down chain is
// verified end to end for dialect, per provenPluginDialects above.
// PostgreSQL is always true (cleat#2306's measurement, not this list); MySQL
// and SQL Server are true only for a plugin this file names.
//
// This does not gate RunDownMigrations itself -- a test proving a plugin's
// Down chain (this file's own template, and cleat#2306's phase 2) has to be
// able to call RunDownMigrations against an UNPROVEN plugin and see its real
// error, not this function's refusal. The gate belongs at the operator
// entrypoint, cmd/cleat-worker/main.go's --uninstall-plugin handler, which is
// the only thing between this function and a live database an operator
// cannot walk back from a MSSQL bricking.
func UninstallProvenOnDialect(pluginName string, dialect Dialect) bool {
	if dialect != DialectMySQL && dialect != DialectMSSQL {
		return true
	}
	return provenPluginDialects[pluginName][dialect]
}

// ProvenPluginNamesForDialect lists, sorted, the plugins UninstallProvenOnDialect
// allows on dialect -- for an operator-facing refusal message to name what
// IS verified, not just what is not.
func ProvenPluginNamesForDialect(dialect Dialect) []string {
	var names []string
	for name, byDialect := range provenPluginDialects {
		if byDialect[dialect] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
