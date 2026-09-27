package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/plugin"

	"github.com/cleat-team/cleat/engine/testutil"
)

// What a caller's JSON is worth after it has been through a PLUGIN column on
// MySQL. cleat#1622, the plugin half of cleat#1022.
//
// MySQL's JSON type keeps an integer as INT64 or UINT64 and falls back to
// DOUBLE when it fits neither, so a value outside [-2^63, 2^64-1] -- and any
// decimal needing more precision than a float64 holds -- is REWRITTEN on the
// way in, with no error and no log. Plugin tables are a separate migration
// system and were deliberately out of scope for migrations/mysql/070.
//
// THE COMPARISON IS ON BYTES, NEVER ON DECODED NUMBERS. A test that decodes to
// float64 recovers 2^64 exactly and is therefore blind PRECISELY at the
// boundary it exists to test -- a test that cannot fail in the region that
// matters, which is worse than no test at all. Every assertion here is a string
// comparison of what the column gave back.
//
// THE CASES STRADDLE THE CLIFF BY ONE, following the engine's sibling test: the
// range is asymmetric, [-2^63, 2^64-1], so a test written against BIGINT would
// sit nowhere near the edge and pass while the boundary moved underneath it.
func TestAPluginCallersJSONSurvivesTheColumn(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectMySQL)
	ctx := context.Background()

	// ESTABLISH THE PRECONDITION RATHER THAN ASSUME IT. The tables below are
	// created by the plugins' own migrations, and nothing else in this package
	// applies them. This test passed locally only because I had run
	// TestPluginMigrations_AllDialects first, by hand, in the same database --
	// a setup step the test did not own and CI had no reason to perform. There
	// it reported `UNMEASURED: ... Table 'cleat.event_stream' doesn't exist`
	// for all 81 cases, which is the right answer to give and the reason those
	// failures say UNMEASURED rather than asserting a defect.
	//
	// Running the real migrations is also stronger than creating the tables
	// here would be: what is under test IS the migration's column type, so a
	// fixture table of my own would assert my CREATE TABLE and nothing else.
	plugins, err := plugin.Discover()
	if err != nil {
		t.Fatalf("UNMEASURED: plugin.Discover: %v", err)
	}
	var migratable []*plugin.LoadedPlugin
	for _, lp := range plugins {
		if !lp.Healthy {
			continue
		}
		if _, ok := lp.Plugin.(plugin.HasMigrations); !ok {
			continue
		}
		if lp.Plugin.Info().Name == "pgvector" {
			continue // PostgreSQL-only, as in TestPluginMigrations_AllDialects
		}
		migratable = append(migratable, lp)
	}
	if len(migratable) < 10 {
		t.Fatalf("UNMEASURED: only %d migratable plugins discovered; the tables "+
			"under test would not be created", len(migratable))
	}
	if err := plugin.RunMigrations(ctx, db, plugin.DialectMySQL, nil, migratable); err != nil {
		t.Fatalf("UNMEASURED: RunMigrations: %v", err)
	}

	payloads := []struct{ name, json string }{
		// Control. If this degrades, the harness is broken rather than the
		// column, and every row below means nothing.
		{"small int", `{"v":42}`},

		{"2^63-1 (int64 max)", `{"v":9223372036854775807}`},
		{"2^63 (past int64)", `{"v":9223372036854775808}`},
		{"2^64-1 (uint64 max)", `{"v":18446744073709551615}`},
		{"2^64 (past uint64)", `{"v":18446744073709551616}`},
		{"-2^63 (int64 min)", `{"v":-9223372036854775808}`},
		{"-2^63-1 (past int64 min)", `{"v":-9223372036854775809}`},

		// Integers are not a special case; they are the half somebody looked at.
		{"decimal past float64", `{"v":0.12345678901234567}`},
		{"thirty digits", `{"v":123456789012345678901234567890}`},
	}

	// Every column cleat#1622 converts, with a minimal INSERT for its table.
	// Derived from information_schema (NOT NULL, no default) rather than from
	// the migration source -- the source-derived list missed three columns.
	columns := []struct {
		name    string
		insert  string // one $-free MySQL statement with a single ? for the value
		read    string
		cleanup string

		// parentInsert/parentCleanup seed and remove a row this case's insert
		// depends on via a foreign key, keyed on the same tenant value. Empty
		// for every column except webhook_delivery.payload: migrations.go v7
		// added an enforced webhook_id -> webhook_config(id) foreign key on
		// MySQL (cleat#2222 -- MySQL's inline column-level REFERENCES is
		// accepted syntax but was never enforced before v7), so an insert
		// with no matching webhook_config row now fails the fixture rather
		// than testing the column.
		parentInsert  string
		parentCleanup string
	}{
		{"event_stream.event",
			"INSERT INTO event_stream (tenant_id, stream_id, sequence, event) VALUES (?, 'p1622', 1, ?)",
			"SELECT event FROM event_stream WHERE tenant_id = ?",
			"DELETE FROM event_stream WHERE tenant_id = ?",
			"", ""},
		{"event_subscriptions.input_template",
			"INSERT INTO event_subscriptions (id, tenant_id, event_type, def_name, input_template) VALUES (UUID(), ?, 'e', 'd', ?)",
			"SELECT input_template FROM event_subscriptions WHERE tenant_id = ?",
			"DELETE FROM event_subscriptions WHERE tenant_id = ?",
			"", ""},
		{"kv_store.value",
			"INSERT INTO kv_store (tenant_id, `key`, value) VALUES (?, 'k', ?)",
			"SELECT value FROM kv_store WHERE tenant_id = ?",
			"DELETE FROM kv_store WHERE tenant_id = ?",
			"", ""},
		{"schedules.input",
			"INSERT INTO schedules (tenant_id, id, name, cron, workflow_name, input) VALUES (?, UUID(), 'n', '* * * * *', 'w', ?)",
			"SELECT input FROM schedules WHERE tenant_id = ?",
			"DELETE FROM schedules WHERE tenant_id = ?",
			"", ""},
		{"feature_flags.rules",
			"INSERT INTO feature_flags (tenant_id, id, `key`, rules) VALUES (?, UUID(), 'k', ?)",
			"SELECT rules FROM feature_flags WHERE tenant_id = ?",
			"DELETE FROM feature_flags WHERE tenant_id = ?",
			"", ""},
		{"task_queue.input",
			"INSERT INTO task_queue (tenant_id, queue_name, job_id, input) VALUES (?, 'q', UUID(), ?)",
			"SELECT input FROM task_queue WHERE tenant_id = ?",
			"DELETE FROM task_queue WHERE tenant_id = ?",
			"", ""},
		{"task_queue.payload",
			"INSERT INTO task_queue (tenant_id, queue_name, job_id, payload) VALUES (?, 'q', UUID(), ?)",
			"SELECT payload FROM task_queue WHERE tenant_id = ?",
			"DELETE FROM task_queue WHERE tenant_id = ?",
			"", ""},
		{"webhook_events.payload",
			"INSERT INTO webhook_events (id, source_id, tenant_id, payload) VALUES (UUID(), UUID(), ?, ?)",
			"SELECT payload FROM webhook_events WHERE tenant_id = ?",
			"DELETE FROM webhook_events WHERE tenant_id = ?",
			"", ""},
		{"webhook_delivery.payload",
			"INSERT INTO webhook_delivery (id, webhook_id, event_type, payload) VALUES (UUID(), ?, 'e', ?)",
			"SELECT payload FROM webhook_delivery WHERE webhook_id = ?",
			"DELETE FROM webhook_delivery WHERE webhook_id = ?",
			"INSERT INTO webhook_config (tenant_id, id, url) VALUES (UUID(), ?, 'https://example.com')",
			"DELETE FROM webhook_config WHERE id = ?"},
	}

	for _, col := range columns {
		for _, p := range payloads {
			t.Run(col.name+"/"+p.name, func(t *testing.T) {
				// A fresh tenant per case, so rows never collide and each read
				// returns exactly what this case wrote.
				tenant := uuid.NewString()

				// Delete what this case wrote. Rows are tenant-isolated and CI
				// databases are per-job service containers, so leaving them
				// breaks nothing today -- but a shared local database runs
				// this 81 times per invocation, and an unbounded table is a
				// trap for whoever writes the next test against it.
				//
				// The child row's cleanup is registered AFTER the parent's,
				// so t.Cleanup (LIFO) runs it FIRST -- the parent must
				// outlive every row that references it.
				if col.parentCleanup != "" {
					t.Cleanup(func() { db.ExecContext(ctx, col.parentCleanup, tenant) })
				}
				t.Cleanup(func() { db.ExecContext(ctx, col.cleanup, tenant) })

				if col.parentInsert != "" {
					if _, err := db.ExecContext(ctx, col.parentInsert, tenant); err != nil {
						t.Fatalf("UNMEASURED: insert parent row for %s: %v", col.name, err)
					}
				}

				if _, err := db.ExecContext(ctx, col.insert, tenant, p.json); err != nil {
					t.Fatalf("UNMEASURED: insert into %s: %v", col.name, err)
				}
				var got string
				if err := db.QueryRowContext(ctx, col.read, tenant).Scan(&got); err != nil {
					t.Fatalf("UNMEASURED: read back %s: %v", col.name, err)
				}
				if got == p.json {
					return
				}
				// The property is byte equality: LONGTEXT stores the caller's
				// bytes verbatim. But say WHICH difference this is, because a
				// JSON column produces both and they are not equally serious.
				// Before the migration every case fails here, the control
				// included, since a JSON column reformats `{"v":42}` to
				// `{"v": 42}`. A message that called that "narrowed" would be
				// wrong about the control and would teach the next reader to
				// distrust the real ones.
				kind := "REFORMATTED it (whitespace only; the value survived)"
				if stripSpace(got) != stripSpace(p.json) {
					kind = "NARROWED it -- the value itself changed"
				}
				t.Errorf("%s %s:\n  sent   %s\n  stored %s", col.name, kind, p.json, got)
			})
		}
	}
}

// stripSpace removes every space so a formatting difference can be told from a
// value difference. Not a JSON parse on purpose: decoding to compare would
// reintroduce the float64 blindness this whole test exists to avoid.
func stripSpace(s string) string { return strings.ReplaceAll(s, " ", "") }
