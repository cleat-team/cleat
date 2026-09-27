package plugin

import (
	"sort"
	"testing"
)

// TestUninstallProvenOnDialectPostgresIsAlwaysProven pins cleat-review's
// 2026-09-25 measurement (cleat#2306): PostgreSQL reversed all 18 plugins
// cleanly, so UninstallProvenOnDialect must never refuse PostgreSQL --
// including for a plugin name this file has never heard of, since a typo in
// provenPluginDialects must not silently gate PostgreSQL too.
func TestUninstallProvenOnDialectPostgresIsAlwaysProven(t *testing.T) {
	for _, name := range []string{"scheduled-backup", "blobstore", "no-such-plugin-at-all"} {
		if !UninstallProvenOnDialect(name, DialectPostgres) {
			t.Errorf("UninstallProvenOnDialect(%q, DialectPostgres) = false, want true -- "+
				"cleat#2306's measurement found PostgreSQL clean on all 18 plugins, and this "+
				"function must not gate a dialect the measurement never implicated", name)
		}
	}
}

// TestUninstallProvenOnDialectRefusesAnUnlistedPluginOnMySQLAndMSSQL is the
// KNOWN-POSITIVE this file's gate exists for: cleat-review measured
// "blobstore" as one of the plugins whose uninstall fails on both MySQL and
// SQL Server, and it carries no end-to-end Down test today. If this ever
// reports true, either blobstore has been listed in provenPluginDialects
// without the test cleat#2306 requires, or the gate itself has stopped
// gating.
func TestUninstallProvenOnDialectRefusesAnUnlistedPluginOnMySQLAndMSSQL(t *testing.T) {
	for _, d := range []Dialect{DialectMySQL, DialectMSSQL} {
		if UninstallProvenOnDialect("blobstore", d) {
			t.Errorf("UninstallProvenOnDialect(\"blobstore\", %v) = true, want false -- "+
				"blobstore has no end-to-end uninstall test and cleat-review measured its "+
				"Down broken on this dialect (cleat#2306)", d)
		}
		if UninstallProvenOnDialect("a-plugin-that-does-not-exist", d) {
			t.Errorf("UninstallProvenOnDialect(<unknown>, %v) = true, want false -- an "+
				"unrecognised name must refuse, not default open", d)
		}
	}
}

// TestUninstallProvenOnDialectAllowsScheduledBackupOnMySQLAndMSSQL is the
// happy path: scheduled-backup is the one plugin with a real end-to-end
// uninstall test (TestUninstallSchedulerBackupOnEveryDialect), so it must be
// the one plugin this gate lets through on MySQL and SQL Server.
func TestUninstallProvenOnDialectAllowsScheduledBackupOnMySQLAndMSSQL(t *testing.T) {
	for _, d := range []Dialect{DialectMySQL, DialectMSSQL} {
		if !UninstallProvenOnDialect("scheduled-backup", d) {
			t.Errorf("UninstallProvenOnDialect(\"scheduled-backup\", %v) = false, want true -- "+
				"plugins/scheduledbackup/a_v4_down_keeps_uninstall_working_test.go proves this "+
				"end to end on every dialect", d)
		}
	}
}

// TestProvenPluginNamesForDialectIsSortedAndMatchesTheGate cross-checks
// ProvenPluginNamesForDialect against UninstallProvenOnDialect itself rather
// than repeating the literal list, so the two cannot silently drift apart --
// the operator-facing refusal message (cmd/cleat-worker/main.go) is built
// from the names function, and the decision to refuse from the gate
// function; if they disagreed the message would either omit a plugin that
// really is allowed or claim one that is not.
func TestProvenPluginNamesForDialectIsSortedAndMatchesTheGate(t *testing.T) {
	for _, d := range []Dialect{DialectMySQL, DialectMSSQL} {
		names := ProvenPluginNamesForDialect(d)
		if !sort.StringsAreSorted(names) {
			t.Errorf("ProvenPluginNamesForDialect(%v) = %v, not sorted", d, names)
		}
		for _, name := range names {
			if !UninstallProvenOnDialect(name, d) {
				t.Errorf("ProvenPluginNamesForDialect(%v) lists %q, but "+
					"UninstallProvenOnDialect(%q, %v) = false", d, name, name, d)
			}
		}
	}
	// PostgreSQL is proven for everything without being listed anywhere --
	// the names function describes provenPluginDialects, not the gate's
	// PostgreSQL fast path, so it must come back empty rather than "every
	// plugin that exists".
	if names := ProvenPluginNamesForDialect(DialectPostgres); len(names) != 0 {
		t.Errorf("ProvenPluginNamesForDialect(DialectPostgres) = %v, want empty -- "+
			"PostgreSQL's proven-ness comes from UninstallProvenOnDialect's fast path, "+
			"not from provenPluginDialects, so nothing should be listed here", names)
	}
}
