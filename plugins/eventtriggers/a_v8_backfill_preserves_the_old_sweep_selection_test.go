package eventtriggers

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// seededPreV8Row names one of the four rows seedPreV8Rows writes, so the
// test can check which of ITS OWN rows the sweep selected rather than a
// bare count -- queryUnprocessedEvents is a genuinely cross-tenant scan of
// the OLDEST 100 undispatched rows (ORDER BY seq LIMIT 100, mirroring
// processBatch), not an unbounded one. On a database that already holds 97 or
// more such rows the four rows this test seeds fall outside the first 100 and
// the pending one reads as unselected (cleat#3529), which is why every arm now
// gets a database of its own. A bare row count is still not this test's to
// claim.
type seededPreV8Row struct {
	id     uuid.UUID
	status string
}

// v7OnlyPlugin exposes only migrations 1-7, so a test can apply the schema
// AS IT EXISTED BEFORE Version 8, seed rows the way a real deployment's
// history would look, and only then apply Version 8 -- the only way to
// exercise a migration's backfill rather than its steady-state behaviour.
type v7OnlyPlugin struct{ *Plugin }

func (v v7OnlyPlugin) Migrations() []plugin.Migration {
	all := v.Plugin.Migrations()
	return all[:7]
}

// TestV8BackfillPreservesTheOldSweepSelection is the regression test for
// cleat-review's round-1 GAP on cleat#2663/#2822: Version 8 added
// dispatch_processed with no backfill, and queryUnprocessedEvents dropped
// the `status = 'pending' OR status IS NULL` predicate that used to be the
// ONLY thing excluding a row from the dispatch sweep. On a fresh database
// every row gets dispatch_processed correctly from birth, so no fresh-DB
// test -- including this package's other Version 8 tests -- can see the
// bug: a deployment upgrading from before Version 8 would have its entire
// ingested_events history default to dispatch_processed = false, and
// processBatch would re-dispatch every event it ever ingested, including
// ones an awaiter already consumed and ones already dead-lettered,
// restarting whatever workflows they had triggered.
//
// Measured before the fix (cleat-review, PG16, real v1-v7 SQL): develop's
// own predicate selects 1 of 4 seeded rows (the pending one); Version 8
// with no backfill selected all 4. This test seeds the same four states
// through real tenant-scoped writes (SQLDBAdapter.Exec, the same path
// production code uses -- not a raw *sql.DB.Exec, which a BLOCK security
// policy on Postgres/MSSQL would simply refuse) and asserts the SAME thing
// develop would have selected, after a real Version 8 migration with its
// backfill applies.
func TestV8BackfillPreservesTheOldSweepSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		td   testutil.Dialect
	}{
		{"postgres", testutil.DialectPostgres},
		{"mysql", testutil.DialectMySQL},
		{"mssql", testutil.DialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sqlDB *sql.DB
			switch tc.td {
			case testutil.DialectMSSQL:
				// testutil.TestDB(t, testutil.DialectMSSQL) hands back a
				// connection to ONE physical database shared by every test in
				// this package's whole `go test` invocation. plugin_migrations
				// is keyed on (plugin_name, version), and v7OnlyPlugin's Info()
				// returns the real Plugin's name via Go method promotion -- so
				// this test's partial (v1-v7) migration state is indistinguishable
				// from, and poisonable by, any other test in this package that
				// applies the real plugin's full migrations against the same
				// shared database (cleat#2871: another test in this package
				// commits the real plugin's v1-v8 migrations first, so this
				// test's "migrate to v7" step is a silent no-op against an
				// already-v8 table, and the backfill this test exists to
				// exercise never runs). MSSQL gets a fresh database of its own.
				sqlDB = freshMSSQLDatabase(t)
			case testutil.DialectMySQL:
				// This said "MySQL and Postgres are not known to have this
				// failure" until cleat#2880 (slice 1) added
				// TestV1V3IndexesAreIdempotentOnMySQL to this same package,
				// which also applies the real plugin's full migrations
				// against this same shared MySQL testutil.TestDB -- and,
				// running first alphabetically ("a_v1..." before "a_v8..."),
				// reliably poisons this test's "migrate to v7" precondition
				// to a silent no-op, exactly cleat#2871's MSSQL mechanism.
				// A first fix here used plugintest.CleanupPluginSchema (this
				// package's TestALegacyAwaiterReplayLeavesAtMostTwoRowsAndUnregisterRemovesBoth's
				// remedy for the same shape), but that is isolation by
				// discipline: it depends on every OTHER full-migration test
				// in the package remembering to clean up too, and this
				// plugin's own TestRunDueBackupsDispatchesExactlyOnceAndAdvancesNextRunAt-shaped
				// sibling tests elsewhere in this repo (scheduledbackup) do
				// not. Matching MSSQL's own remedy -- a fresh database,
				// isolation by construction -- is what
				// plugins/webhookingest's TestV3V4V7MigrationsAreIdempotentOnMySQL
				// (cleat#2223) already does for the identical reason, citing
				// this same cleat#2871 precedent.
				sqlDB = freshMySQLDatabase(t)
			case testutil.DialectPostgres:
				// This arm alone used testutil.TestDB, the database shared by
				// every test in the package's whole `go test` invocation and,
				// run after run, by the machine's long-lived test server
				// (cleat#3529). plugin_migrations is keyed on (plugin_name,
				// version), so once ANY run had applied this plugin's full
				// migrations there, "migrate to v7" and "migrate to v8" below
				// were silent no-ops, the backfill never ran, and the three
				// terminal rows stayed selectable: the arm passed once on a
				// never-used database and failed every time after. Rows left
				// behind there also fill the sweep's LIMIT 100. Isolation by
				// construction, as for the two arms above.
				sqlDB = freshPostgresDatabase(t)
			default:
				t.Fatalf("unknown dialect %q", tc.td)
			}
			dialect := plugin.Dialect(string(tc.td))
			real := &Plugin{dialect: dialect}
			db := &engine.SQLDBAdapter{DB: sqlDB, Dialect: dialect}

			if err := plugin.RunMigrations(context.Background(), sqlDB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: v7OnlyPlugin{real}, Healthy: true}}); err != nil {
				t.Fatalf("migrate to v7: %v", err)
			}

			tenantID := uuid.New()
			tenantCtx := plugin.ForTenant(context.Background(), tenantID)
			seeded := seedPreV8Rows(t, tenantCtx, db, tenantID)

			// Apply Version 8. Its backfill must run against the rows just
			// seeded, exactly as it would against a real deployment's
			// pre-existing history on upgrade.
			if err := plugin.RunMigrations(context.Background(), sqlDB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: real, Healthy: true}}); err != nil {
				t.Fatalf("migrate to v8: %v", err)
			}

			// queryUnprocessedEvents is a genuinely cross-tenant scan in
			// production (processBatch runs it under AcrossAllTenants) --
			// reading it through a bare, unscoped connection would have RLS's
			// FILTER predicate hide every row and pass for the wrong reason.
			// It is LIMITed to the oldest 100 undispatched rows, which cannot
			// hide this test's four rows only because the database is this
			// test's own (cleat#3529); the check below is keyed on the FOUR
			// IDs this test itself seeded, never on the result set's size.
			sweepCtx := plugin.AcrossAllTenants(context.Background(), "test: cross-tenant dispatch sweep")
			rows, err := db.Query(sweepCtx, queryUnprocessedEvents.For(dialect))
			if err != nil {
				t.Fatalf("queryUnprocessedEvents: %v", err)
			}
			defer rows.Close()
			selected := map[uuid.UUID]bool{}
			for rows.Next() {
				var (
					id, tid    uuid.UUID
					eventType  string
					eventData  []byte
					retryCount int
				)
				if err := plugin.ScanRow(rows, &id, &tid, &eventType, &eventData, &retryCount); err != nil {
					t.Fatalf("scan: %v", err)
				}
				selected[id] = true
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}

			// Exactly the pending row -- the one develop's own sweep would
			// also have selected. Selecting any terminal row means the
			// backfill missed it (the re-dispatch hazard); not selecting
			// the pending row means it was over-backfilled (a live event
			// silently never retried).
			for _, r := range seeded {
				want := r.status == "pending"
				if got := selected[r.id]; got != want {
					t.Errorf("%s: row seeded with status=%q, selected=%v, want %v -- "+
						"the backfill does not preserve develop's own selection",
						tc.name, r.status, got, want)
				}
			}
		})
	}
}

// seedPreV8Rows inserts one row per terminal state a real pre-Version-8
// deployment's ingested_events could hold, all older than the sweep's 10s
// cutoff, through the same tenant-scoped SQLDBAdapter.Exec path production
// code uses -- required on Postgres/MSSQL, where a raw, unscoped write is
// refused outright by the table's own BLOCK security policy.
func seedPreV8Rows(t *testing.T, ctx context.Context, db *engine.SQLDBAdapter, tenantID uuid.UUID) []seededPreV8Row {
	t.Helper()
	dialect := db.Dialect
	var receivedAtExpr string
	switch dialect {
	case plugin.DialectMySQL:
		receivedAtExpr = "NOW() - INTERVAL 1 HOUR"
	case plugin.DialectMSSQL:
		receivedAtExpr = "DATEADD(hour, -1, SYSUTCDATETIME())"
	default:
		receivedAtExpr = "NOW() - INTERVAL '1 hour'"
	}
	insertSQL := `INSERT INTO ingested_events (id, tenant_id, event_type, event_data, received_at, processed, status)
		VALUES ($1, $2, 't', '{}', ` + receivedAtExpr + `, $3, $4)`

	rows := []struct {
		processed bool
		status    string
	}{
		{true, "completed"},
		{true, "dead_letter"},
		{true, "consumed"},
		{false, "pending"},
	}
	seeded := make([]seededPreV8Row, 0, len(rows))
	for _, r := range rows {
		id := uuid.New()
		if _, err := db.Exec(ctx, insertSQL, id.String(), tenantID.String(), r.processed, r.status); err != nil {
			t.Fatalf("seed pre-v8 row (status=%s): %v", r.status, err)
		}
		seeded = append(seeded, seededPreV8Row{id: id, status: r.status})
	}
	return seeded
}

// freshPostgresDatabase creates a uniquely-named PostgreSQL database on the
// same server CLEAT_TEST_POSTGRES / CLEAT_TEST_DB points at, builds the test
// schema in it, and returns a *sql.DB connected to that database alone -- see
// the comment at this function's one call site for why this arm needs its own
// database rather than the one testutil.TestDB shares. The schema comes from
// testutil.SetupMinimalSchema, whose name is historical: it applies the whole
// schema for the dialect, as SetupMySQLFullSchema and SetupMSSQLFullSchema do
// for the other two arms.
//
// An unreachable server is a skip only when nobody configured one, exactly as
// testutil.TestDB treats it: a configured DSN that does not connect is a
// failure of the configuration, and skipping would hide it. The new database's
// DSN is derived from testutil.PostgresTestDSN, which already carries the
// application_name tag the foreign-session probe uses to recognise this
// process's own connections. DROP DATABASE ... WITH (FORCE) needs PostgreSQL 13
// or later; CI runs 16.
func freshPostgresDatabase(t *testing.T) *sql.DB {
	t.Helper()
	adminDSN := testutil.PostgresTestDSN()
	configured := os.Getenv("CLEAT_TEST_POSTGRES") != "" || os.Getenv("CLEAT_TEST_DB") != ""
	unavailable := t.Skipf
	if configured {
		unavailable = t.Fatalf
	}
	admin, err := sql.Open("postgres", adminDSN)
	if err != nil {
		unavailable("open admin postgres connection: %v", err)
		return nil
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		unavailable("ping admin postgres connection: %v", err)
		return nil
	}

	u, err := url.Parse(adminDSN)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		admin.Close()
		t.Fatalf("the PostgreSQL test DSN is not a postgres:// URL, so a fresh database cannot be addressed from it (parse error: %v)", err)
	}

	name := "cleat_test_3529_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		admin.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	// Registered in this order so t.Cleanup's LIFO runs the pool close first,
	// then the drop, then the admin close: the drop must not find the test's own
	// connections still attached, and must not run on a closed admin handle.
	t.Cleanup(func() { admin.Close() })
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`); err != nil {
			t.Logf("cleanup: drop database %s: %v", name, err)
		}
	})

	u.Path = "/" + name
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatalf("open fresh postgres database %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping fresh postgres database %s: %v", name, err)
	}
	testutil.SetupMinimalSchema(t, db, testutil.DialectPostgres)
	return db
}

// freshMySQLDatabase creates a uniquely-named MySQL database on the same
// server CLEAT_TEST_MYSQL points at, builds the package's full test schema
// in it, and returns a *sql.DB connected to that database alone. Mirrors
// plugins/webhookingest's identical helper (cleat#2223), which itself cites
// plugin/a_concurrent_plugin_migrators_are_serialised_test.go's pattern
// (cleat#2117) rather than introducing a new one -- see this function's one
// call site for why this test needs its own database rather than the one
// testutil.TestDB shares across the package.
func freshMySQLDatabase(t *testing.T) *sql.DB {
	t.Helper()
	admin := os.Getenv("CLEAT_TEST_MYSQL")
	if admin == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
	adb, err := sql.Open("mysql", admin)
	if err != nil {
		t.Fatalf("open admin mysql connection: %v", err)
	}
	if err := adb.Ping(); err != nil {
		adb.Close()
		t.Fatalf("ping admin mysql connection: %v", err)
	}

	cfg, err := mysql.ParseDSN(admin)
	if err != nil {
		adb.Close()
		t.Fatalf("parse CLEAT_TEST_MYSQL: %v", err)
	}

	name := fmt.Sprintf("cleat_test_2880_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := adb.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		adb.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	// Registered before the drop below, so t.Cleanup's LIFO order runs the
	// drop FIRST and closes adb LAST -- the reverse leaves the drop trying
	// to Exec on an already-closed connection ("sql: database is closed").
	t.Cleanup(func() { adb.Close() })
	t.Cleanup(func() {
		if _, err := adb.Exec("DROP DATABASE IF EXISTS `" + name + "`"); err != nil {
			t.Logf("cleanup: drop database %s: %v", name, err)
		}
	})

	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("open fresh mysql database %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping fresh mysql database %s: %v", name, err)
	}
	testutil.SetupMySQLFullSchema(t, db)
	return db
}

// freshMSSQLDatabase creates a uniquely-named MSSQL database on the same
// server CLEAT_TEST_MSSQL points at, builds the package's full test schema
// in it, and returns a *sql.DB connected to that database alone -- see the
// comment at this function's one call site for why this test needs its own
// database rather than the one testutil.TestDB shares across the package.
func freshMSSQLDatabase(t *testing.T) *sql.DB {
	t.Helper()
	adminDSN := os.Getenv("CLEAT_TEST_MSSQL")
	if adminDSN == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping MSSQL tests")
	}
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse CLEAT_TEST_MSSQL: %v", err)
	}

	admin, err := sql.Open("sqlserver", adminDSN)
	if err != nil {
		t.Fatalf("open admin mssql connection: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("ping admin mssql connection: %v", err)
	}

	dbName := "cleat_test_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	if _, err := admin.Exec("CREATE DATABASE [" + dbName + "]"); err != nil {
		t.Fatalf("create database %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		dropAdmin, err := sql.Open("sqlserver", adminDSN)
		if err != nil {
			t.Logf("cleanup: reopen admin connection to drop %s: %v", dbName, err)
			return
		}
		defer dropAdmin.Close()
		// Forcibly evicts this test's own connection pool against dbName, so
		// DROP DATABASE does not fail with "database is in use" (MSSQL error
		// 3702) when the *sql.DB below has not finished closing all of its
		// pooled connections yet.
		if _, err := dropAdmin.Exec("ALTER DATABASE [" + dbName + "] SET SINGLE_USER WITH ROLLBACK IMMEDIATE"); err != nil {
			t.Logf("cleanup: set single_user on %s: %v", dbName, err)
			return
		}
		if _, err := dropAdmin.Exec("DROP DATABASE [" + dbName + "]"); err != nil {
			t.Logf("cleanup: drop database %s: %v", dbName, err)
		}
	})

	q := u.Query()
	q.Set("database", dbName)
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatalf("open fresh mssql database %s: %v", dbName, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping fresh mssql database %s: %v", dbName, err)
	}
	testutil.SetupMinimalSchema(t, db, testutil.DialectMSSQL)
	return db
}
