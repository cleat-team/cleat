package migration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

// --schema names the PostgreSQL schema cleat builds into, and until cleat#1287
// it named only where the runtime connection LOOKED. The migrations went to
// public regardless, because nineteen of the forty-four files opened with
// `SET search_path = public;` and twenty-five did not -- so a non-default
// schema split the core schema against itself and the run died partway:
//
//	migration 020_event_intent.sql: execute: pq: relation "event_history"
//	does not exist (42P01)
//
// 020 is simply the first file in version order that does not pin.
//
// WHAT MAKES THIS TEST ABLE TO FAIL, which is worth stating because the
// original report's measurement could not. That one reproduced what the two
// code paths do -- a hand-written CREATE TABLE on one connection and a SELECT
// on another -- and concluded plugin tables miss while core tables land. It
// was faithful to the lines it was built from and wrong about the system,
// because the real migration set never completes. This runs the real runner
// over the real migrations directory, so the failure it would have to explain
// away is a failure of the thing itself.
//
// The control matters as much: the same assertions under the default schema
// must still pass, or "it works now" is indistinguishable from "it builds
// somewhere else now".
func TestMigrationsHonourANonDefaultSchema(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		schema string // as passed to WithSchema
		want   string // where the tables must end up
	}{
		{"default", "", "public"},
		{"non-default", "cleat_prod", "cleat_prod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newScratchDB(t, "cleat_schema_"+strings.ReplaceAll(tc.name, "-", "_"))

			if err := migration.NewRunner(db, migration.DialectPostgres, migrationsRoot(t)).
				WithSchema(tc.schema).Run(ctx); err != nil {
				t.Fatalf("run with WithSchema(%q): %v", tc.schema, err)
			}

			// The floor. An empty schema counts zero for the same reason a
			// correctly-built one counts zero in the schema it did NOT build
			// into, so "none in public" alone proves nothing.
			got := tablesBySchema(t, db)
			if got[tc.want] < 10 {
				t.Fatalf("only %d tables in %s; the run reported success but "+
					"built almost nothing (all schemas: %v)", got[tc.want], tc.want, got)
			}
			for schema, n := range got {
				if schema == tc.want || schema == "admin" || schema == "cleat" {
					continue
				}
				t.Errorf("%d table(s) landed in %q, not in the configured %q: %v",
					n, schema, tc.want, got)
			}

			// The bookkeeping has to follow the tables. In the other schema it
			// would read as empty on the next boot and every migration would
			// be applied a second time.
			var trackingSchema string
			if err := db.QueryRowContext(ctx,
				`SELECT schemaname FROM pg_tables WHERE tablename = 'schema_migrations'`).
				Scan(&trackingSchema); err != nil {
				t.Fatalf("locating schema_migrations: %v", err)
			}
			if trackingSchema != tc.want {
				t.Errorf("schema_migrations is in %q, tables are in %q", trackingSchema, tc.want)
			}

			// A second run must be a no-op. If the runner recorded its
			// versions somewhere it cannot read them back from, this is where
			// that shows up -- as a re-apply, which for 001 means a pile of
			// "already exists".
			if err := migration.NewRunner(db, migration.DialectPostgres, migrationsRoot(t)).
				WithSchema(tc.schema).Run(ctx); err != nil {
				t.Fatalf("second run with WithSchema(%q): %v", tc.schema, err)
			}

			// And what the worker's runtime pool would see: unqualified names
			// resolved through search_path, which is what dsnWithSchema sets
			// on it. This is the half the original report measured, and it is
			// the half that has to be true for any of the above to matter.
			runtimeConn, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("runtime connection: %v", err)
			}
			defer runtimeConn.Close()
			if _, err := runtimeConn.ExecContext(ctx, "SET search_path = "+tc.want); err != nil {
				t.Fatalf("set search_path: %v", err)
			}
			var n int
			if err := runtimeConn.QueryRowContext(ctx,
				`SELECT count(*) FROM workflow_instances`).Scan(&n); err != nil {
				t.Fatalf("runtime SELECT through search_path=%s: %v", tc.want, err)
			}
		})
	}
}

func tablesBySchema(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	rows, err := db.Query(`
		SELECT schemaname, count(*) FROM pg_tables
		WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
		GROUP BY schemaname`)
	if err != nil {
		t.Fatalf("count tables by schema: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[s] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}
