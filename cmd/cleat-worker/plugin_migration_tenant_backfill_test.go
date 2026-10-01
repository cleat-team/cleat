package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// cleat#2828: a plugin migration version that writes (UPDATE/INSERT/DELETE)
// to a table an EARLIER version of the same plugin already declared
// TenantScoped runs into that table's own row-level security -- applied in
// the earlier version's own transaction, by plugin/migration.go's
// applyTenantScoping (PostgreSQL FORCE ROW LEVEL SECURITY) and
// applyTenantScopingMSSQL (a SQL Server SECURITY POLICY with FILTER and
// BLOCK predicates) -- because the connection running a later migration
// carries no tenant context. On PostgreSQL that is
// cleat.assert_tenant_set() RAISING "cleat.tenant_id is not set", even
// against an empty table: the predicate is STABLE, so Postgres can evaluate
// it before any row is examined. On SQL Server it is a BLOCK PREDICATE
// silently discarding the write's effect on the FILTER side, or refusing
// it outright on the BLOCK side -- see plugins/eventtriggers/migrations.go
// v6's own UpMSSQL comment, "Reproduced directly."
//
// THE BYPASS ALREADY EXISTS AND IS ALREADY PROVEN: engine/plugindb_tenant.go's
// markCrossTenantOnTx enters it for the ordinary cross-tenant sweep path --
// PostgreSQL SET LOCAL ROLE cleat_sweep (every TenantScoped table's own
// applyTenantScoping already installs a `<table>_cross_tenant` policy
// granting that role unrestricted access, plus the GRANT it needs), SQL
// Server EXEC sp_set_session_context @key = N'cross_tenant'. A migration
// backfill just needs to enter the SAME bypass before its DML, which v6's
// UpMSSQL arm already does (cleat#2625's schema change) -- this guard exists
// because v6's Up (PostgreSQL) arm, touching the SAME table with the SAME
// shape of UPDATE two lines away, does not.
//
// THIS GUARD DOES NOT KNOW WHETHER A BYPASS IS CORRECT, only whether one is
// PRESENT: it looks for the literal marker text the two sanctioned
// mechanisms produce (SET LOCAL ROLE cleat_sweep; sp_set_session_context
// naming cross_tenant) anywhere in the version's own Up/UpMSSQL, not that it
// precedes the DML it protects -- SET LOCAL's scope is the whole
// transaction regardless of statement order, so a marker anywhere in the
// same Up string is sufficient in practice, and demanding textual ordering
// would be precision this check cannot actually verify from a Go string
// (splitStatements, not this test, decides execution order against the
// server). A version that is compliant by construction (its bypass marker
// present, whether or not it happens to run the DML first) reads as safe,
// which is what actually matters.
//
// THE SWEEP THIS RAN AGAINST: every git grep "TenantScoped:" hit under
// plugins/ as of develop at e773969d (2026-09-30), cross-checked by two
// independent methods -- a structural parse of each migrations.go's ordered
// Migration{} literals, and a blunt grep for UPDATE/INSERT/DELETE across
// every migrations.go with each hit classified by hand. Both found exactly
// one unguarded instance ON THAT TREE: eventtriggers v6's Up.
//
// THAT TREE IS NOT THE ONLY ONE THAT MATTERS. #2822, open and not yet
// merged, adds eventtriggers v8 -- a second Postgres-arm instance of the
// identical gap, on the same table (ingested_events, TenantScoped since v4):
// its UpMSSQL arm carries the sp_set_session_context bypass (added fixing a
// round-1 review finding, cleat#2822) and its Up arm does not. Confirmed by
// reading #2822's diff directly (`grep -c
// 'cleat_sweep\|SET LOCAL ROLE\|set_config'` over the full patch: 0), not
// assumed from the PR description -- a census over a tree that is about to
// change is exactly the kind of number this file elsewhere warns rotates
// fastest. This guard's fix must cover v6 AND v8 together, or it goes red
// on develop the moment #2822 merges with only v6 addressed. The population
// this test actually examines at whatever tree it runs against is printed
// in its own log output rather than trusted from this comment, for the same
// reason.
func TestNoPluginMigrationWritesATenantScopedTableWithoutTheCrossTenantBypass(t *testing.T) {
	loaded, err := plugin.Discover()
	if err != nil {
		t.Fatalf("discovering registered plugins: %v", err)
	}
	if len(loaded) < 10 {
		t.Fatalf("only %d plugins registered; the import block in main.go is the "+
			"source of this set, so this guard is reporting on nothing", len(loaded))
	}

	type gap struct {
		plugin, dialect, table string
		version                int
		scopedAtVersion        int
		lines                  []string
	}
	var gaps []gap

	withMigrations := 0
	tablesTracked := 0
	for _, lp := range loaded {
		p, ok := lp.Plugin.(plugin.HasMigrations)
		if !ok {
			continue
		}
		withMigrations++
		name := lp.Plugin.Info().Name

		migs := append([]plugin.Migration(nil), p.Migrations()...)
		sort.Slice(migs, func(i, j int) bool { return migs[i].Version < migs[j].Version })

		scopedAsOf := map[string]int{}
		for _, m := range migs {
			for table, scopedVer := range scopedAsOf {
				if m.Version <= scopedVer {
					continue
				}
				if lines := dmlLinesAgainstTable(m.Up, table); len(lines) > 0 && !strings.Contains(m.Up, "SET LOCAL ROLE cleat_sweep") {
					gaps = append(gaps, gap{name, "postgres", table, m.Version, scopedVer, lines})
				}
				if m.UpMSSQL != "" {
					if lines := dmlLinesAgainstTable(m.UpMSSQL, table); len(lines) > 0 && !mssqlCrossTenantRe.MatchString(m.UpMSSQL) {
						gaps = append(gaps, gap{name, "mssql", table, m.Version, scopedVer, lines})
					}
				}
			}
			for _, table := range m.TenantScoped {
				tablesTracked++
				if _, already := scopedAsOf[table]; !already {
					scopedAsOf[table] = m.Version
				}
			}
		}
	}

	if withMigrations < 10 {
		t.Fatalf("only %d of %d linked plugins implement HasMigrations; this guard "+
			"examines migrations, so that is a population it cannot report on",
			withMigrations, len(loaded))
	}
	if tablesTracked == 0 {
		t.Fatalf("0 TenantScoped table declarations found across %d plugins with migrations -- "+
			"this guard has nothing to check, which is not the same as nothing being wrong",
			withMigrations)
	}
	t.Logf("checked %d linked plugins with migrations, %d TenantScoped table declarations tracked",
		withMigrations, tablesTracked)

	for _, g := range gaps {
		archName := "Up"
		if g.dialect == "mssql" {
			archName = "UpMSSQL"
		}
		t.Errorf("plugin %q migration v%d (%s) writes to %q, TenantScoped since v%d, with no "+
			"cross-tenant bypass in its own %s -- this statement will hit that table's row-level "+
			"security under a connection with no tenant context:\n  %s\n\n"+
			"  PostgreSQL: add `SET LOCAL ROLE cleat_sweep;` to this version's Up (every "+
			"TenantScoped table's own migration already installs the `<table>_cross_tenant` "+
			"policy and GRANT that role needs; SET LOCAL is transaction-scoped and needs no "+
			"explicit clear).\n"+
			"  SQL Server: add `EXEC sp_set_session_context @key = N'cross_tenant', @value = "+
			"N'<reason>';` before the write, and clear it (@value = NULL) before this arm ends -- "+
			"see plugins/eventtriggers/migrations.go v6's UpMSSQL for the worked example, which "+
			"already does this. cleat#2828.",
			g.plugin, g.version, g.dialect, g.table, g.scopedAtVersion,
			archName, strings.Join(g.lines, "\n  "))
	}
}

var (
	dmlVerbRe          = regexp.MustCompile(`(?i)\b(UPDATE|INSERT\s+INTO|DELETE\s+FROM)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	mssqlCrossTenantRe = regexp.MustCompile(`(?i)sp_set_session_context\s*@key\s*=\s*N?'cross_tenant'`)
)

// dmlLinesAgainstTable returns every line of sql that runs an UPDATE/INSERT
// INTO/DELETE FROM the named table (exact identifier match, so
// "event_awaiters_backup" does not match a check for "event_awaiters").
func dmlLinesAgainstTable(sql, table string) []string {
	var lines []string
	for _, m := range dmlVerbRe.FindAllStringSubmatchIndex(sql, -1) {
		tableStart, tableEnd := m[4], m[5]
		if !strings.EqualFold(sql[tableStart:tableEnd], table) {
			continue
		}
		lineStart := strings.LastIndexByte(sql[:m[0]], '\n') + 1
		lineEnd := strings.IndexByte(sql[m[0]:], '\n')
		if lineEnd == -1 {
			lineEnd = len(sql)
		} else {
			lineEnd += m[0]
		}
		lines = append(lines, strings.TrimSpace(sql[lineStart:lineEnd]))
	}
	return lines
}
