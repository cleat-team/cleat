package eventtriggers

import (
	"context"
	"os"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#3873. ingested_events.event_data was still a JSON column on MySQL when step 5 (cleat#1622) converted
// input_template, so a number outside what MySQL's JSON type keeps exactly was rewritten in every event the plugin
// stored. Step 13 does for event_data what step 5 did.
//
// EVERY COMPARISON IS OF BYTES, as in TestAnEventBodyIsStoredAsPublished, and the control comes first: a scratch
// table with a JSON column in the SAME database must narrow the value used below. Without it a MySQL that kept
// the digits everywhere would make every arm pass having measured nothing.
const (
	v13Body = `{"x":123456789012345678901234567890,"d":0.12345678901234567890123}`
	// the JSON type also re-renders the text: keys sorted, a space after each colon and comma
	v13Narrowed = `{"d": 0.12345678901234568, "x": 1.2345678901234566e29}`
)

func TestV13StoresAnEventBodyAsPublishedOnMySQL(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
	for _, be := range testutil.NewPluginTestBackends(t) {
		if be.Dialect != testutil.DialectMySQL {
			continue
		}
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			ctx := context.Background()
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: plugin.DialectMySQL}
			if err := plugin.RunMigrations(ctx, be.DB, plugin.DialectMySQL, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}
			var up, down string
			for _, m := range p.Migrations() {
				if m.Version == 13 {
					up, down = m.UpMySQL, m.DownMySQL
				}
			}
			if up == "" || down == "" {
				t.Fatal("step 13 has no MySQL up or down; there is nothing to test")
			}

			// The test database outlives a run (a developer's does), so the rows this test writes are removed on
			// the way in and on the way out: a second run must not collide with the first's.
			clean := func() { be.DB.ExecContext(ctx, `DELETE FROM ingested_events WHERE id LIKE 'v13-%'`) }
			clean()
			defer clean()

			columnType := func() string {
				var typ string
				if err := be.DB.QueryRowContext(ctx, `SELECT DATA_TYPE FROM information_schema.COLUMNS
					WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ingested_events' AND COLUMN_NAME = 'event_data'`).Scan(&typ); err != nil {
					t.Fatalf("read event_data's type: %v", err)
				}
				return typ
			}
			insert := func(id, body string) error {
				_, err := be.DB.ExecContext(ctx, `INSERT INTO ingested_events (id, tenant_id, event_type, event_data)
					VALUES (?, '00000000-0000-0000-0000-000000000001', 'probe', ?)`, id, body)
				return err
			}
			stored := func(id string) string {
				var got string
				if err := be.DB.QueryRowContext(ctx, `SELECT event_data FROM ingested_events WHERE id = ?`, id).Scan(&got); err != nil {
					t.Fatalf("read back %s: %v", id, err)
				}
				return got
			}

			// THE CONTROL: the JSON type narrows this exact value. If it did not, nothing below could tell a
			// fixed column from an unfixed one.
			if _, err := be.DB.ExecContext(ctx, `CREATE TABLE v13_control (id INT PRIMARY KEY, j JSON, t LONGTEXT)`); err != nil {
				t.Fatalf("control table: %v", err)
			}
			defer be.DB.ExecContext(ctx, `DROP TABLE IF EXISTS v13_control`)
			if _, err := be.DB.ExecContext(ctx, `INSERT INTO v13_control VALUES (1, ?, ?)`, v13Body, v13Body); err != nil {
				t.Fatalf("control insert: %v", err)
			}
			var j, tx string
			if err := be.DB.QueryRowContext(ctx, `SELECT j, t FROM v13_control WHERE id = 1`).Scan(&j, &tx); err != nil {
				t.Fatalf("control read: %v", err)
			}
			if j != v13Narrowed || tx != v13Body {
				t.Fatalf("UNMEASURED: the control did not behave as the defect says.\n  JSON column: %s (want %s)\n  LONGTEXT:    %s (want %s)", j, v13Narrowed, tx, v13Body)
			}

			t.Run("after the step the column is LONGTEXT and keeps the digits", func(t *testing.T) {
				if got := columnType(); got != "longtext" {
					t.Fatalf("event_data is %q after step 13, want longtext", got)
				}
				if err := insert("v13-a", v13Body); err != nil {
					t.Fatalf("insert: %v", err)
				}
				if got := stored("v13-a"); got != v13Body {
					t.Errorf("event_data is not what was published.\n  published: %s\n  stored:    %s", v13Body, got)
				}
			})

			t.Run("the CHECK refuses what the JSON type refused", func(t *testing.T) {
				if err := insert("v13-bad", `{"x":`); err == nil {
					t.Error("a body that is not JSON was stored: LONGTEXT gave up the JSON type's validation and nothing replaced it")
				}
			})

			t.Run("a column already converted is left alone, however often the step re-runs", func(t *testing.T) {
				runStatements(t, ctx, be, up)
				runStatements(t, ctx, be, up)
				if got := stored("v13-a"); got != v13Body {
					t.Errorf("a re-run changed a stored body: %s", got)
				}
			})

			t.Run("down restores the JSON column and up converts it again keeping each row's text", func(t *testing.T) {
				runStatements(t, ctx, be, down)
				if got := columnType(); got != "json" {
					t.Fatalf("event_data is %q after down, want json", got)
				}
				if err := insert("v13-b", `{"a":1}`); err != nil {
					t.Fatalf("insert under the JSON column: %v", err)
				}
				before := stored("v13-b")
				runStatements(t, ctx, be, up)
				if got := columnType(); got != "longtext" {
					t.Fatalf("event_data is %q after up, want longtext", got)
				}
				if got := stored("v13-b"); got != before {
					t.Errorf("the conversion changed a row's stored text: %q -> %q", before, got)
				}
				if err := insert("v13-c", v13Body); err != nil {
					t.Fatalf("insert after up: %v", err)
				}
				if got := stored("v13-c"); got != v13Body {
					t.Errorf("after down and up, event_data is not what was published: %s", got)
				}
			})
		})
	}
}
