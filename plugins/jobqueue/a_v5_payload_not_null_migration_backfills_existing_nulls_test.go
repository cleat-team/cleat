// TestV5PayloadNotNullMigrationBackfillsExistingNulls is cleat#2282, MySQL
// only. v4 (cleat#1622) converted task_queue.payload from JSON NOT NULL
// DEFAULT ('{}') to LONGTEXT NULL to preserve large integers -- and its
// MODIFY, restating only what it needed for that change, silently dropped
// BOTH the NOT NULL constraint and the DEFAULT. PostgreSQL and SQL Server
// never had a v4 arm and stayed NOT NULL DEFAULT '{}' throughout.
//
// v5 restores payload's NOT NULL DEFAULT ('{}') on MySQL. A bare "add NOT
// NULL back" migration is unsafe on any database already carrying a row
// whose payload reads NULL under v4's shape -- measured directly (see
// migrations.go's own comment on v5): MySQL 8's strict mode refuses a MODIFY
// to NOT NULL over an existing NULL value with "Data truncated for column
// 'payload' at row 1". v5's Up backfills first for exactly this reason.
//
// This test builds a real v1-v4 database from the plugin's own truncated
// Migrations() (not a hand-copied schema), seeds a row that omits payload --
// the one shape that reads NULL under v4, since no tracked production INSERT
// does this but nothing stops a future one from starting to -- runs v5's
// real UpMySQL text, and asserts both that it succeeds and that the
// previously-NULL row now reads '{}', not that a NOT NULL column merely
// exists.
package jobqueue

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

func TestV5PayloadNotNullMigrationBackfillsExistingNulls(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		if be.Dialect != testutil.DialectMySQL {
			continue
		}
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))

			// This test's whole premise is a database that has NOT reached
			// Version 5 yet. testutil's databases are shared and persistent
			// across a whole test binary, so clean up before running
			// anything, not only after -- the same reasoning
			// a_legacy_awaiter_replay_leaves_at_most_two_rows_test.go gives
			// for the identical pattern.
			plugintest.CleanupPluginSchema(t, be.DB, be.Dialect, "jobqueue", []string{"task_queue"})
			defer plugintest.CleanupPluginSchema(t, be.DB, be.Dialect, "jobqueue", []string{"task_queue"})

			legacy := &truncatedJobqueueMigrations{
				Plugin: New().(*Plugin),
				n:      4,
			}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: legacy, Healthy: true}}); err != nil {
				t.Fatalf("jobqueue v1-v4 migrations on %s: %v", be.Name, err)
			}

			// MySQL has no per-tenant RLS (single-tenant by construction),
			// so a plain connection reaches task_queue directly -- no
			// AcrossAllTenants/ForTenant dance needed, unlike this
			// package's PostgreSQL/SQL Server fixtures.
			fixtureDB := be.CrossTenantConn(t, ctx,
				"cleat#2282: seed a v4-shaped task_queue row with no payload")

			tenantID := uuid.New()
			jobID := uuid.New()
			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`INSERT INTO task_queue (tenant_id, queue_name, job_id, status) VALUES ($1, $2, $3, 'pending')`,
				tenantID, "v5-backfill-test", jobID); err != nil {
				t.Fatalf("seed v4-shaped row on %s: %v", be.Name, err)
			}

			var seededPayload sql.NullString
			if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
				`SELECT payload FROM task_queue WHERE tenant_id = $1 AND job_id = $2`,
				tenantID, jobID).Scan(&seededPayload); err != nil {
				t.Fatalf("read seeded payload on %s: %v", be.Name, err)
			}
			if seededPayload.Valid {
				t.Fatalf("seeded row's payload: got %q, want SQL NULL -- "+
					"the fixture is supposed to reproduce v4's shape, and this means it did not", seededPayload.String)
			}

			var v5SQL string
			for _, m := range legacy.Plugin.Migrations() {
				if m.Version == 5 {
					v5SQL = m.UpMySQL
				}
			}
			if v5SQL == "" {
				t.Fatal("migration v5's UpMySQL is empty -- nothing to run")
			}

			// The real migration text, run directly against the fixture
			// connection -- not through plugin.RunMigrations, which would
			// need the version-tracking row v1-v4 already wrote and is not
			// what this test is about; this is testing the SQL itself
			// against a database that already has the NULL row the bug
			// describes.
			//
			// Split on ";" and exec each statement separately, mirroring
			// plugin.RunMigrations' own execSQLStatements -- a plain
			// database/sql connection (no multiStatements DSN flag, which
			// nothing in this repo sets) cannot run two statements in one
			// Exec call, and v5's Up is UPDATE-then-ALTER, exactly two.
			for _, stmt := range strings.Split(v5SQL, ";") {
				stmt = strings.TrimSpace(stmt)
				if stmt == "" {
					continue
				}
				if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect, stmt); err != nil {
					t.Fatalf("v5 UpMySQL statement %q failed on %s, with a pre-existing NULL payload row present: %v",
						stmt, be.Name, err)
				}
			}

			var gotPayload sql.NullString
			if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
				`SELECT payload FROM task_queue WHERE tenant_id = $1 AND job_id = $2`,
				tenantID, jobID).Scan(&gotPayload); err != nil {
				t.Fatalf("read backfilled payload on %s: %v", be.Name, err)
			}
			if !gotPayload.Valid {
				t.Errorf("backfilled row's payload: got SQL NULL, want '{}' -- v5's UPDATE did not reach it")
			} else if gotPayload.String != "{}" {
				t.Errorf("backfilled row's payload: got %q, want %q", gotPayload.String, "{}")
			}

			var isNullable, columnDefault sql.NullString
			if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
				`SELECT IS_NULLABLE, COLUMN_DEFAULT FROM information_schema.columns
				 WHERE table_schema = DATABASE() AND table_name = 'task_queue' AND column_name = 'payload'`,
			).Scan(&isNullable, &columnDefault); err != nil {
				t.Fatalf("read payload column metadata on %s: %v", be.Name, err)
			}
			if isNullable.String != "NO" {
				t.Errorf("payload IS_NULLABLE after v5: got %q, want %q", isNullable.String, "NO")
			}
			if !columnDefault.Valid {
				t.Errorf("payload COLUMN_DEFAULT after v5: got SQL NULL, want a default -- "+
					"v4's MODIFY dropped it and v5 is supposed to restore it")
			}
		})
	}
}

// truncatedJobqueueMigrations restricts Migrations() to the first n
// entries, so plugin.RunMigrations applies only a prefix of jobqueue's real
// migration list -- the same device
// a_legacy_awaiter_replay_leaves_at_most_two_rows_test.go (plugins/eventtriggers)
// and TestWebhookSourcesDeletedAtMigrationIsIdempotentOnMySQL's sibling tests
// use, so a fixture built to reproduce an old schema shape reads it from the
// plugin's own migration list rather than a second, drifting hand-copy.
type truncatedJobqueueMigrations struct {
	*Plugin
	n int
}

func (p *truncatedJobqueueMigrations) Migrations() []plugin.Migration {
	all := p.Plugin.Migrations()
	return append([]plugin.Migration(nil), all[:p.n]...)
}
