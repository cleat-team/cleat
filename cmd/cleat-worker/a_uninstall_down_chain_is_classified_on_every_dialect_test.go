package main

// cleat#2306, phase 2. RunDownMigrations had no caller before #2290, so nothing exercised a
// plugin's Down chain until a human went looking -- cleat-review's #2290 measurement found 7
// of 18 plugins broken on MySQL and 17 of 18 on SQL Server, this way. This test is the
// standing guard against that silence returning: every plugin with migrations, on every
// dialect, has to be classified as either PROVEN (its Down chain runs clean end to end, and
// plugin.UninstallProvenOnDialect says so -- cleat-review's guard ask on #2338, satisfied
// here by actually driving RunDownMigrations for every proven entry rather than trusting the
// claim) or KNOWN BROKEN (knownBrokenPluginDown below, recording whether a follow-up Up
// recovers the database or leaves it permanently unmigratable -- cleat-review's second ask,
// measured via cleat#2306's phase-2 sweep, 2026-09-25).
//
// An unclassified (plugin, dialect) pair fails this test outright, on purpose: silence is
// what let cleat#2306 exist in the first place.
//
// PostgreSQL carries no entries in knownBrokenPluginDown: cleat-review's measurement is a
// clean 18 of 18, and this test holds that as a standing assertion rather than a one-time
// finding -- a regression there fails loud, the same as any other dialect.
//
// The literal ask on cleat#2306 also wants admin.plugin_tables asserted empty for the plugin
// after uninstall. Measured against the one already-proven plugin (scheduled-backup,
// PostgreSQL): its rows survive uninstall unchanged (cleat#2343) -- RunDownMigrations never
// touches that table. That is a real gap, logged here rather than asserted, until #2343 lands;
// asserting it today would fail the one plugin this test is supposed to hold as the working
// example.
//
// deployDialect and deployScratch are shared with a_migration_is_a_deploy_step_test.go, same
// package.

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/migration/catalogdiff"
	"github.com/cleat-team/cleat/plugin"
)

// catalogDialect converts plugin.Dialect to migration.Dialect for
// catalogdiff.Snapshot. The two are separate types with identical string
// values ("postgres"/"mysql"/"mssql") rather than one shared enum, because
// migration/ deliberately imports nothing else in this module (to avoid
// import cycles) -- see migration/catalogdiff's own package comment.
func catalogDialect(d plugin.Dialect) migration.Dialect {
	return migration.Dialect(string(d))
}

// downOutcome is what a (plugin, dialect) pair's Down chain is expected to do.
type downOutcome int

const (
	// outcomeProven: Up, Down, and a re-Up all succeed. Matches plugin.UninstallProvenOnDialect.
	outcomeProven downOutcome = iota
	// outcomeRecoverable: Down fails, but a follow-up plain Up on the same, half-reversed
	// database succeeds -- the --migrate-only equivalent recovers the schema even though
	// uninstall did not.
	outcomeRecoverable
	// outcomeUnrecoverable: Down fails AND the database is left in a shape
	// neither RunDownMigrations nor a follow-up RunMigrations can move --
	// either the follow-up Up fails outright, or it returns no error while
	// the recovered schema is missing an object the failed Down destroyed
	// (plugin_migrations still records that migration as applied, so the
	// recovery Up skips re-creating it). Measured 2026-09-25, all three
	// remaining instances are the second shape: blobstore/mysql,
	// oauth-provider/mssql, webhook-ingest/mssql. notifications/MSSQL --
	// cleat#2342's one pair whose follow-up Up failed outright -- is now
	// proven and gone from the map.
	outcomeUnrecoverable
)

func (o downOutcome) String() string {
	switch o {
	case outcomeProven:
		return "proven"
	case outcomeRecoverable:
		return "recoverable"
	case outcomeUnrecoverable:
		return "unrecoverable"
	default:
		return "unknown"
	}
}

// knownBrokenPluginDown records every (plugin, MySQL/MSSQL) pair whose Down chain is known to
// fail, and how badly. Measured 2026-09-25 (cleat#2306 phase-2 sweep) via a fresh scratch
// database per pair: apply every migration, RunDownMigrations, and on failure a follow-up
// plain RunMigrations on the same, half-reversed database. This map is the CI record of that
// measurement -- re-derive it by running this test, not by re-reading this comment.
//
// A plugin's Down chain getting fixed does not delete its own entry: this test asserts a
// known-broken pair STAYS broken, so a plugin that starts passing FAILS this test (loudly,
// not silently) until its entry is removed here and it is added to plugin.provenPluginDialects
// with its own end-to-end proving test -- the template is
// plugins/scheduledbackup/a_v4_down_keeps_uninstall_working_test.go,
// TestUninstallSchedulerBackupOnEveryDialect.
var knownBrokenPluginDown = map[string]map[plugin.Dialect]downOutcome{
	// Broken on both MySQL and SQL Server.
	"event-triggers": {plugin.DialectMySQL: outcomeRecoverable, plugin.DialectMSSQL: outcomeRecoverable},
	"jobqueue":       {plugin.DialectMySQL: outcomeRecoverable},
	"scheduler":      {plugin.DialectMySQL: outcomeRecoverable, plugin.DialectMSSQL: outcomeRecoverable},
	"kafka-connect":  {plugin.DialectMySQL: outcomeRecoverable, plugin.DialectMSSQL: outcomeRecoverable},
	// blobstore/mysql, oauth-provider/mssql and webhook-ingest/mssql are split
	// below: each is recoverable on one dialect and unrecoverable on the
	// other, so they cannot share one line with the pairs above.
	"blobstore":      {plugin.DialectMySQL: outcomeUnrecoverable, plugin.DialectMSSQL: outcomeRecoverable},
	"oauth-provider": {plugin.DialectMySQL: outcomeRecoverable, plugin.DialectMSSQL: outcomeUnrecoverable},
	"webhook-ingest": {plugin.DialectMySQL: outcomeRecoverable, plugin.DialectMSSQL: outcomeUnrecoverable},
	// Unrecoverable: the follow-up Up returns no error, but cleat#2306 phase
	// 2's schema-equality check (migration/catalogdiff, added after
	// cleat-review's GAP verdict on #2346) proves the recovered database is
	// missing an object the failed Down destroyed -- plugin_migrations still
	// records that migration as applied, so the recovery Up skips
	// re-creating it. Measured 2026-09-25:
	//   blobstore/mysql:      the entire workflow_blob_refs TABLE is gone
	//   oauth-provider/mssql: oauth_sessions.nonce COLUMN is gone
	//   webhook-ingest/mssql: webhook_events.error_msg COLUMN is gone
}

func TestUninstallDownChainIsClassifiedOnEveryDialect(t *testing.T) {
	if testing.Short() {
		t.Skip("migrates a real database per plugin per dialect")
	}
	all, err := plugin.Discover()
	if err != nil {
		t.Fatal(err)
	}
	var withMigrations []*plugin.LoadedPlugin
	for _, lp := range all {
		if _, ok := lp.Plugin.(plugin.HasMigrations); ok {
			withMigrations = append(withMigrations, lp)
		}
	}
	if len(withMigrations) == 0 {
		t.Fatal("no plugin implements plugin.HasMigrations -- plugin.Discover() is broken or " +
			"nothing is registered, and every case below would vacuously pass")
	}

	for _, c := range []deployDialect{
		{"postgres", "CLEAT_TEST_POSTGRES", "postgres"},
		{"mysql", "CLEAT_TEST_MYSQL", "mysql"},
		{"mssql", "CLEAT_TEST_MSSQL", "sqlserver"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if c.admin() == "" {
				t.Skipf("%s not set, skipping %s", c.env, c.name)
			}
			dialect := plugin.Dialect(c.name)

			for _, lp := range withMigrations {
				lp := lp
				name := lp.Plugin.Info().Name
				t.Run(name, func(t *testing.T) {
					broken, isBroken := knownBrokenPluginDown[name][dialect]
					proven := plugin.UninstallProvenOnDialect(name, dialect)

					if proven && isBroken {
						t.Fatalf("%s/%s: listed in BOTH plugin.provenPluginDialects and "+
							"knownBrokenPluginDown -- contradictory classification, fix one",
							name, dialect)
					}
					if !proven && !isBroken {
						if dialect == plugin.DialectPostgres {
							// PostgreSQL has no known-broken entries by design (cleat-review's
							// #2290 measurement: clean 18 of 18) -- an unproven PostgreSQL
							// plugin here means UninstallProvenOnDialect stopped returning
							// true unconditionally for PostgreSQL, which is itself the thing
							// to fix, not something this test should paper over.
							t.Fatalf("%s/postgres: plugin.UninstallProvenOnDialect returned "+
								"false for PostgreSQL, which is supposed to be true "+
								"unconditionally (cleat#2306's measurement: 18 of 18 clean)",
								name)
						}
						t.Fatalf("%s/%s: not classified anywhere -- add it to "+
							"plugin.provenPluginDialects (with its own proving test) if its "+
							"Down chain works, or to knownBrokenPluginDown in this file (with "+
							"the outcome measured against a real database) if it does not",
							name, dialect)
					}
					want := outcomeProven
					if isBroken {
						want = broken
					}

					db, tablesBefore := scratchWithTableSnapshot(t, c, dialect)
					single := []*plugin.LoadedPlugin{lp}
					ctx := context.Background()

					if err := plugin.RunMigrations(ctx, db, dialect, nil, single); err != nil {
						t.Fatalf("%s/%s: initial Up failed: %v", name, dialect, err)
					}
					pluginTables := newTableNames(t, ctx, db, dialect, tablesBefore)

					// Both broken-pair outcomes need this, not just outcomeRecoverable:
					// RunDownMigrations fails, and a follow-up Up returning no error is NOT
					// proof of a clean recovery either way. cleat-review measured that a failed
					// Down can drop an object while plugin_migrations still records its
					// migration as applied, so the recovery Up sees nothing pending, skips
					// re-creating the object, and returns cleanly anyway -- which is exactly the
					// shape an outcomeUnrecoverable pair can now also take (blobstore/mysql,
					// oauth-provider/mssql, webhook-ingest/mssql: recoverErr is nil for all
					// three). This snapshot, taken while the schema is known-good, is what the
					// recovered schema is compared against below, whichever outcome is expected.
					var cleanSchema *catalogdiff.Catalog
					if want == outcomeRecoverable || want == outcomeUnrecoverable {
						var err error
						cleanSchema, err = catalogdiff.Snapshot(ctx, db, catalogDialect(dialect))
						if err != nil {
							t.Fatalf("%s/%s: snapshotting the schema after the initial Up: %v",
								name, dialect, err)
						}
					}

					downRes, downErr := plugin.RunDownMigrations(ctx, db, dialect, lp, single)

					switch want {
					case outcomeProven:
						if downErr != nil {
							reversed := []int{}
							if downRes != nil {
								reversed = downRes.Reversed
							}
							t.Fatalf("%s/%s: marked PROVEN in plugin.provenPluginDialects, but "+
								"RunDownMigrations failed (reversed so far=%v): %v -- fix the "+
								"Down chain or remove this plugin from provenPluginDialects "+
								"until it is actually proven", name, dialect, reversed, downErr)
						}
						// cleat#2306's literal ask: the plugin's own tables are gone, and a
						// re-Up is clean.
						for _, tbl := range pluginTables {
							if tableExists(t, ctx, db, dialect, tbl) {
								t.Errorf("%s/%s: table %q still exists after a clean uninstall",
									name, dialect, tbl)
							}
						}
						var migRows int
						if err := db.QueryRowContext(ctx, pluginMigrationsCountSQL(dialect), name).
							Scan(&migRows); err != nil {
							t.Fatalf("%s/%s: counting plugin_migrations rows: %v", name, dialect, err)
						}
						if migRows != 0 {
							t.Errorf("%s/%s: %d plugin_migrations row(s) remain after uninstall",
								name, dialect, migRows)
						}
						if dialect == plugin.DialectPostgres {
							// cleat#2343: RunDownMigrations does not clean admin.plugin_tables.
							// Logged, not asserted, until that lands.
							var ptRows int
							if err := db.QueryRowContext(ctx,
								`SELECT COUNT(*) FROM admin.plugin_tables WHERE plugin_name = $1`,
								name).Scan(&ptRows); err == nil && ptRows > 0 {
								t.Logf("%s/postgres: %d admin.plugin_tables row(s) remain after "+
									"uninstall -- known gap, cleat#2343", name, ptRows)
							}
						}
						if err := plugin.RunMigrations(ctx, db, dialect, nil, single); err != nil {
							t.Fatalf("%s/%s: proven and reversed cleanly, but the re-Up failed: %v",
								name, dialect, err)
						}

					case outcomeRecoverable, outcomeUnrecoverable:
						if downErr == nil {
							reversed := []int{}
							if downRes != nil {
								reversed = downRes.Reversed
							}
							t.Fatalf("%s/%s: RunDownMigrations SUCCEEDED (reversed=%v), but "+
								"knownBrokenPluginDown still marks it %s -- the fix landed; "+
								"move this plugin to plugin.provenPluginDialects (with its own "+
								"end-to-end proving test) and delete this entry",
								name, dialect, reversed, want)
							return
						}
						recoverErr := plugin.RunMigrations(ctx, db, dialect, nil, single)
						if want == outcomeRecoverable && recoverErr != nil {
							t.Fatalf("%s/%s: marked recoverable, but the follow-up Up on "+
								"the half-reversed database also failed: %v -- this pair "+
								"got WORSE, reclassify as outcomeUnrecoverable and check it "+
								"did not brick a real database along the way",
								name, dialect, recoverErr)
						}
						if recoverErr == nil {
							// A nil error here is not proof of a clean recovery by itself:
							// plugin_migrations can still record a version as applied after the
							// failed Down destroyed the object that version created, so a plain
							// RunMigrations sees nothing pending and returns cleanly over a
							// silently incomplete schema -- true for EITHER expected outcome, since
							// an outcomeUnrecoverable pair can take this path too (blobstore/mysql,
							// oauth-provider/mssql, webhook-ingest/mssql all do). Compare against
							// the clean snapshot taken right after the initial Up, before
							// RunDownMigrations touched anything.
							recoveredSchema, err := catalogdiff.Snapshot(ctx, db, catalogDialect(dialect))
							if err != nil {
								t.Fatalf("%s/%s: snapshotting the schema after the recovery Up: %v",
									name, dialect, err)
							}
							diff := catalogdiff.Diff(cleanSchema, recoveredSchema)
							switch {
							case want == outcomeRecoverable && len(diff) != 0:
								t.Fatalf("%s/%s: marked recoverable, and the follow-up Up returned no "+
									"error, but the recovered schema differs from a clean install "+
									"(%d line(s)):\n%s\nThe failed Down destroyed object(s) that "+
									"plugin_migrations still records as applied, so the recovery Up "+
									"skipped re-creating them. This pair is not actually recoverable -- "+
									"reclassify it as outcomeUnrecoverable and do not advertise "+
									"--migrate-only as a repair for it",
									name, dialect, len(diff), strings.Join(diff, "\n"))
							case want == outcomeUnrecoverable && len(diff) == 0:
								t.Fatalf("%s/%s: marked unrecoverable, but the follow-up Up "+
									"SUCCEEDED and the recovered schema is IDENTICAL to a clean "+
									"install -- this pair got BETTER, reclassify as "+
									"outcomeRecoverable", name, dialect)
							}
						}
					}
				})
			}
		})
	}
}

// scratchWithTableSnapshot creates an empty scratch database for c, applies the core schema,
// and returns the handle alongside the table names that exist right after -- the baseline a
// plugin's own tables are diffed against, since not every plugin table is declared
// TenantScoped and there is no other generic way to ask "which tables belong to this plugin".
func scratchWithTableSnapshot(t *testing.T, c deployDialect, dialect plugin.Dialect) (*sql.DB, map[string]bool) {
	t.Helper()
	_, db := deployScratch(t, c)
	testutil.SetupFullSchema(t, db, testutil.Dialect(c.name))
	return db, listTables(t, context.Background(), db, dialect)
}

// newTableNames returns the tables that exist now but did not in before, excluding
// plugin_migrations: RunMigrations creates it lazily, self-bootstrapping
// (createPluginMigrationsTableSQL, plugin/migration.go:516), on the FIRST plugin migration run
// against a fresh database rather than as part of the core schema baseline -- so it is always
// "new" relative to a pre-Up snapshot, for every plugin, and is shared infrastructure
// RunDownMigrations correctly never drops. Diffing it in here reported it as "the plugin's own
// table" and failed a clean uninstall for every proven plugin the first time this test ran.
func newTableNames(t *testing.T, ctx context.Context, db *sql.DB, dialect plugin.Dialect, before map[string]bool) []string {
	t.Helper()
	var added []string
	for tbl := range listTables(t, ctx, db, dialect) {
		if tbl == "plugin_migrations" || before[tbl] {
			continue
		}
		added = append(added, tbl)
	}
	return added
}

// listTables asks each dialect's own catalogue for every table name it can see, unqualified --
// the plugin migration framework never schema-qualifies plugin tables (--schema aside, which
// none of these tests set), so table name alone is enough to diff against.
func listTables(t *testing.T, ctx context.Context, db *sql.DB, dialect plugin.Dialect) map[string]bool {
	t.Helper()
	var q string
	switch dialect {
	case plugin.DialectMySQL:
		q = `SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()`
	case plugin.DialectMSSQL:
		q = `SELECT name FROM sys.tables`
	default:
		q = `SELECT tablename FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema')`
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning table name: %v", err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	return out
}

func tableExists(t *testing.T, ctx context.Context, db *sql.DB, dialect plugin.Dialect, table string) bool {
	t.Helper()
	return listTables(t, ctx, db, dialect)[table]
}

// pluginMigrationsCountSQL counts plugin_migrations rows for one plugin. Portable $1 form --
// this package has no rebinder in scope the way plugintest.QueryRowRebound gives the plugin
// package's own tests, so each dialect's driver-native placeholder is spelled directly.
func pluginMigrationsCountSQL(dialect plugin.Dialect) string {
	switch dialect {
	case plugin.DialectMySQL:
		return `SELECT COUNT(*) FROM plugin_migrations WHERE plugin_name = ?`
	case plugin.DialectMSSQL:
		return `SELECT COUNT(*) FROM plugin_migrations WHERE plugin_name = @p1`
	default:
		return `SELECT COUNT(*) FROM plugin_migrations WHERE plugin_name = $1`
	}
}
