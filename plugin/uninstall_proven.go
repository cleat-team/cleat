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
// able to run. MySQL at least recovers with --migrate-only; SQL Server does
// not recover at all.
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
// table-driven version of this across every plugin; as each one passes, its
// name moves here (or, once phase 2's own test subsumes this file, this map
// is replaced by that test's own record of what it proved).
var provenPluginDialects = map[string]map[Dialect]bool{
	"scheduled-backup": {DialectMySQL: true, DialectMSSQL: true},
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
