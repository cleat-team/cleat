package plugins_test

// cleat#3641 (owner decision D60): the three columns plugins create at worker boot to hold a
// workflow-definition name compare exactly on every dialect that has been decided.
//
//	eventtriggers  event_subscriptions.def_name
//	scheduler      schedules.workflow_name
//	jobqueue       task_queue.def_name
//
// NOTHING IN SQL COMPARES THESE COLUMNS TODAY. They are written and read back in a select list; no
// predicate, join, grouping, ordering, unique index or key mentions them, and no query outside the three
// plugins touches those tables. So this test is about the COLUMN, not a plugin feature: insert
// 'Checkout', probe 'checkout' with plain SQL, and ask whether the column folds case. That is why it
// does not drive the plugins' own queries, and why the CHANGELOG does not call the change a fix.
//
// Per-dialect expectations, and why each is what it is:
//
//	postgres  exact   a deterministic collation
//	mssql     exact   Latin1_General_100_BIN2 after the plugins' SQL Server step (cleat#3641); until then
//	                  the database default, SQL_Latin1_General_CP1_CI_AS, which folds case
//	mysql     exact   utf8mb4_0900_bin after the plugins' MySQL step (cleat#3658); until then the server
//	                  default, utf8mb4_0900_ai_ci, which ignores case AND accent. That difference was PINNED
//	                  here as `FOLDS` until cleat#3658 on purpose -- a pin that nobody has to update is how a
//	                  divergence comes to look like a decision -- and this entry flipping is what that pin
//	                  was for. It went red first, before the step existed.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/eventtriggers"
	"github.com/cleat-team/cleat/plugins/jobqueue"
	"github.com/cleat-team/cleat/plugins/scheduler"
)

type pluginNameColumn struct {
	plugin func() plugin.Plugin
	table  string
	column string
	// insert takes the tenant id and the name, in that order, as the dialect's own placeholders.
	insert map[testutil.Dialect]string
}

func pluginNameColumns() []pluginNameColumn {
	return []pluginNameColumn{
		{
			plugin: eventtriggers.New, table: "event_subscriptions", column: "def_name",
			insert: map[testutil.Dialect]string{
				testutil.DialectPostgres: `INSERT INTO event_subscriptions (tenant_id, event_type, def_name) VALUES ($1, 't', $2)`,
				testutil.DialectMySQL:    `INSERT INTO event_subscriptions (id, tenant_id, event_type, def_name) VALUES (UUID(), ?, 't', ?)`,
				testutil.DialectMSSQL:    `INSERT INTO event_subscriptions (tenant_id, event_type, def_name) VALUES (@p1, 't', @p2)`,
			},
		},
		{
			plugin: scheduler.New, table: "schedules", column: "workflow_name",
			insert: map[testutil.Dialect]string{
				testutil.DialectPostgres: `INSERT INTO schedules (tenant_id, id, name, cron, workflow_name) VALUES ($1, gen_random_uuid(), 'n', '* * * * *', $2)`,
				testutil.DialectMySQL:    `INSERT INTO schedules (tenant_id, id, name, cron, workflow_name) VALUES (?, UUID(), 'n', '* * * * *', ?)`,
				testutil.DialectMSSQL:    `INSERT INTO schedules (tenant_id, id, name, cron, workflow_name) VALUES (@p1, NEWID(), 'n', '* * * * *', @p2)`,
			},
		},
		{
			plugin: jobqueue.New, table: "task_queue", column: "def_name",
			insert: map[testutil.Dialect]string{
				testutil.DialectPostgres: `INSERT INTO task_queue (tenant_id, queue_name, job_id, payload, def_name) VALUES ($1, 'q', gen_random_uuid(), '{}', $2)`,
				testutil.DialectMySQL:    `INSERT INTO task_queue (tenant_id, queue_name, job_id, payload, def_name) VALUES (?, 'q', UUID(), '{}', ?)`,
				testutil.DialectMSSQL:    `INSERT INTO task_queue (tenant_id, queue_name, job_id, payload, def_name) VALUES (@p1, 'q', NEWID(), '{}', @p2)`,
			},
		},
	}
}

type nameColumnExpectation struct {
	collation string // "" means "not asserted" (PostgreSQL has no per-column collation to name here)
	folds     bool
	why       string
}

func nameColumnExpectations() map[testutil.Dialect]nameColumnExpectation {
	return map[testutil.Dialect]nameColumnExpectation{
		testutil.DialectPostgres: {folds: false, why: "a deterministic collation, so = is byte equality"},
		testutil.DialectMSSQL: {collation: "Latin1_General_100_BIN2", folds: false,
			why: "the plugins' SQL Server step (cleat#3641) made the column Latin1_General_100_BIN2; until then it inherited the database default, SQL_Latin1_General_CP1_CI_AS, which folds case"},
		testutil.DialectMySQL: {collation: "utf8mb4_0900_bin", folds: false,
			why: "the plugins' MySQL step (cleat#3658) converted the column to utf8mb4_0900_bin, the collation cleat#3640 measured against PostgreSQL as the only MySQL one that agrees with it on all six probes; before that it inherited the server default, utf8mb4_0900_ai_ci, which ignores case AND accent"},
	}
}

func placeholder(d testutil.Dialect, n int) string {
	switch d {
	case testutil.DialectMySQL:
		return "?"
	case testutil.DialectMSSQL:
		return fmt.Sprintf("@p%d", n)
	}
	return fmt.Sprintf("$%d", n)
}

func columnCollation(ctx context.Context, db *sql.DB, d testutil.Dialect, table, column string) (string, error) {
	var got sql.NullString
	var err error
	switch d {
	case testutil.DialectMSSQL:
		err = db.QueryRowContext(ctx, `SELECT collation_name FROM sys.columns WHERE object_id = OBJECT_ID(@p1) AND name = @p2`, table, column).Scan(&got)
	case testutil.DialectMySQL:
		err = db.QueryRowContext(ctx, `SELECT collation_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`, table, column).Scan(&got)
	default:
		return "", nil
	}
	return got.String, err
}

func TestPluginOwnedWorkflowNameColumnsCompareExactly(t *testing.T) {
	expect := nameColumnExpectations()
	cols := pluginNameColumns()

	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			ctx := context.Background()
			want, ok := expect[be.Dialect]
			if !ok {
				t.Fatalf("no expectation for dialect %q: add one with its reason", be.Dialect)
			}

			var loaded []*plugin.LoadedPlugin
			for _, c := range cols {
				loaded = append(loaded, &plugin.LoadedPlugin{Plugin: c.plugin(), Healthy: true})
			}
			if err := plugin.RunMigrations(ctx, be.DB, plugin.Dialect(be.Dialect), nil, loaded); err != nil {
				t.Fatalf("plugin migrations on %s: %v", be.Name, err)
			}

			for _, c := range cols {
				c := c
				t.Run(c.table+"."+c.column, func(t *testing.T) {
					if want.collation != "" {
						got, err := columnCollation(ctx, be.DB, be.Dialect, c.table, c.column)
						if err != nil {
							t.Fatalf("read the collation of %s.%s on %s: %v", c.table, c.column, be.Name, err)
						}
						if got != want.collation {
							t.Errorf("%s.%s on %s has collation %q, want %q: %s", c.table, c.column, be.Name, got, want.collation, want.why)
						}
					}

					// The behaviour of the COLUMN, with plain SQL: store 'Checkout', then ask for 'checkout'.
					tenant := uuid.NewString()
					conn := be.CrossTenantConn(t, ctx, "cleat#3641 name-column probe")
					defer conn.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = %s`, c.table, placeholder(be.Dialect, 1)), tenant)

					ins, ok := c.insert[be.Dialect]
					if !ok {
						t.Fatalf("no insert for %s on %s", c.table, be.Name)
					}
					if _, err := conn.ExecContext(ctx, ins, tenant, "Checkout"); err != nil {
						t.Fatalf("seed %s on %s: %v", c.table, be.Name, err)
					}
					probe := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE tenant_id = %s AND %s = %s`,
						c.table, placeholder(be.Dialect, 1), c.column, placeholder(be.Dialect, 2))

					// The known-positive: the exact spelling must always match, or a zero below means nothing.
					var exact, lower int
					if err := conn.QueryRowContext(ctx, probe, tenant, "Checkout").Scan(&exact); err != nil {
						t.Fatalf("probe %s on %s: %v", c.table, be.Name, err)
					}
					if exact != 1 {
						t.Fatalf("UNMEASURED: probing %s.%s for the exact spelling 'Checkout' found %d rows, want 1, so the probe for 'checkout' cannot be read", c.table, c.column, exact)
					}
					if err := conn.QueryRowContext(ctx, probe, tenant, "checkout").Scan(&lower); err != nil {
						t.Fatalf("probe %s on %s: %v", c.table, be.Name, err)
					}
					if folds := lower == 1; folds != want.folds {
						t.Errorf("%s.%s on %s: 'checkout' matched %d row(s) stored as 'Checkout' (folds=%v), want folds=%v: %s",
							c.table, c.column, be.Name, lower, folds, want.folds, want.why)
					}
				})
			}
		})
	}
}

// The test above must not pass because nothing ran: every configured backend contributes one subtest
// per column, and the dialect expectations are the only thing that decides each outcome.
func TestPluginOwnedWorkflowNameColumnsListIsNotEmpty(t *testing.T) {
	if n := len(pluginNameColumns()); n != 3 {
		t.Fatalf("the plugin-owned workflow-name columns are %d, want the 3 cleat#3641 names (event_subscriptions.def_name, schedules.workflow_name, task_queue.def_name); if a plugin gained or lost one, update this list and the issues deliberately", n)
	}
	for _, c := range pluginNameColumns() {
		for d := range nameColumnExpectations() {
			if strings.TrimSpace(c.insert[d]) == "" {
				t.Errorf("%s has no insert for dialect %q", c.table, d)
			}
		}
	}
}
