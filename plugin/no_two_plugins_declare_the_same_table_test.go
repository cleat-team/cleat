package plugin_test

import (
	"sort"
	"strings"
	"testing"
)

// Plugin tables share one flat namespace, so two plugins declaring the same
// table name is a silent collision. cleat#1288.
//
// WHY IT IS SILENT. Every plugin migration uses CREATE TABLE IF NOT EXISTS,
// which checks the NAME and not the SHAPE. The second plugin's CREATE is a
// no-op that reports success, the migration runner records the version as
// applied so it never re-runs, and the plugin then loads and starts querying a
// table it does not recognise. The failure surfaces much later as
// `column "owner" does not exist`, with nothing connecting it to the cause.
// Since cleat#1280 the loser can also inherit the winner's row-level security
// policy, filtering on a tenant_id it may not have.
//
// WHAT THIS COVERS, AND WHAT IT DELIBERATELY DOES NOT. This is a compile-time
// property of the plugins in this repository: it fails CI before a colliding
// pair can ship. It cannot see a THIRD-PARTY plugin, which is the case the
// issue is actually worried about -- those names are not in this tree, and the
// only thing that would catch them is a check inside RunMigrations. That was
// considered and not built here: RunMigrations failing is os.Exit(1) on every
// worker (cmd/cleat-worker/main.go), so a parse quirk in some plugin's DDL
// would be a fleet-wide outage, traded against a collision that does not
// currently exist. The structural fix that removes the question entirely is a
// schema per plugin -- docs/plugin-table-handling.md 3.1, which also resolves
// cleat#1287 and cleat#1289. #1288 stays open for that half.
//
// The rule is stronger than "differing shapes collide": no two plugins may
// declare the same table name at all. Two plugins sharing a table identically
// is still two owners for one relation, with no way to tell which migration
// created it or what uninstalling either should do.
func TestNoTwoPluginsDeclareTheSameTable(t *testing.T) {
	type decl struct {
		plugin  string
		version int
		arm     string
		cols    []string
	}
	byTable := map[string][]decl{}

	files := pluginMigrationFiles(t)
	if len(files) < 10 {
		t.Fatalf("found %d plugins/*/migrations.go; the scan is broken and this "+
			"guard would pass vacuously", len(files))
	}

	for _, file := range files {
		for _, m := range migrationsIn(t, file) {
			for arm, sql := range m.arms {
				for table, cols := range createTableColumns(sql, arm) {
					byTable[table] = append(byTable[table], decl{m.plugin, m.version, arm, cols})
				}
			}
		}
	}

	// A guard that examined nothing reports no collisions. "0 collisions" and
	// "0 tables examined" are the same observation otherwise.
	if len(byTable) == 0 {
		t.Fatal("no CREATE TABLE was found in any plugin migration; the scan is " +
			"broken, and an empty result here is indistinguishable from a clean one")
	}

	names := make([]string, 0, len(byTable))
	for tbl := range byTable {
		names = append(names, tbl)
	}
	sort.Strings(names)

	for _, table := range names {
		owners := map[string]bool{}
		for _, d := range byTable[table] {
			owners[d.plugin] = true
		}
		if len(owners) < 2 {
			continue
		}

		plugins := make([]string, 0, len(owners))
		for p := range owners {
			plugins = append(plugins, p)
		}
		sort.Strings(plugins)

		// Report whether the shapes agree, because the two cases fail
		// differently: differing shapes surface as a column error at first
		// query, identical shapes as two plugins quietly writing one table.
		shapes := map[string]string{}
		for _, d := range byTable[table] {
			shapes[d.plugin] = strings.Join(d.cols, ",")
		}
		distinct := map[string]bool{}
		for _, s := range shapes {
			distinct[s] = true
		}
		how := "with identical columns, so both will read and write one relation"
		if len(distinct) > 1 {
			how = "with DIFFERENT columns, so the loser's CREATE TABLE IF NOT EXISTS " +
				"is a silent no-op recorded as applied, and it will fail at first " +
				"query with a missing-column error"
		}

		t.Errorf("table %q is declared by %d plugins (%s), %s.\n\n"+
			"Plugin tables share one unqualified namespace, so a name is a "+
			"repository-wide resource. Rename one, or give the plugins their own "+
			"schemas (docs/plugin-table-handling.md 3.1).",
			table, len(plugins), strings.Join(plugins, ", "), how)
	}

	t.Logf("checked %d distinct plugin table names for cross-plugin collisions", len(byTable))
}
