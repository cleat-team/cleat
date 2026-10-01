// TestMySQLV4BackfillSurvivesARerun pins cleat-review's G2 finding on
// cleat#2268 round 1. RunMigrations wraps each migration version in a
// transaction (plugin/migration.go, pinnedtx.Begin), and on Postgres and SQL
// Server that transaction's DDL rolls back together with everything else in
// it on a failure. MySQL's CREATE TABLE commits implicitly regardless of the
// surrounding transaction -- so a crash between v4's CREATE TABLE and its
// backfill (or between the backfill and plugin_migrations recording version
// 4 as applied) leaves event_stream_head present and populated, but v4
// un-recorded. The next boot re-runs v4's Up block against a database that
// already has it: CREATE TABLE IF NOT EXISTS is harmless, but the original
// backfill was a bare INSERT, which dies on event_stream_head's PRIMARY key
// the moment any row from the first attempt survived -- a worker that
// cannot boot (cleat-review, cleat#2268 round 1; the same class as
// cleat#2223).
//
// This simulates that crash by deleting v4's plugin_migrations tracking row
// after a real, successful migration run -- the same "delete the tracking
// row, re-migrate" shape cleat#2881 uses -- and re-running RunMigrations.
// MySQL only: Postgres and SQL Server's transactional DDL make this
// scenario unreachable there (a failure mid-v4 rolls the CREATE TABLE back
// too, so "table exists, tracking row missing" cannot arise), and the
// backfill's ON DUPLICATE KEY UPDATE is itself MySQL-only SQL.
package eventstore

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestMySQLV4BackfillSurvivesARerun(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set")
	}

	ctx := context.Background()
	db := testutil.MySQLTestDB(t)
	defer db.Close()

	testutil.SetupFullSchema(t, db, testutil.DialectMySQL)

	p := &Plugin{dialect: plugin.DialectMySQL, logger: slog.Default(), config: Config{MaxEventSize: 1 << 20}}
	lp := []*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}

	if err := plugin.RunMigrations(ctx, db, plugin.DialectMySQL, nil, lp); err != nil {
		t.Fatalf("first migration run: %v", err)
	}
	p.db = &engine.SQLDBAdapter{DB: db, Dialect: plugin.DialectMySQL}

	// A stream with real history, so the backfill has a non-trivial
	// MAX(sequence) to recompute on the re-run rather than vacuously
	// re-deriving 1.
	tenantID := uuid.MustParse(engine.DefaultTenantUUID)
	streamID := "rerun-backfill-stream-" + uuid.New().String()
	for i := 0; i < 3; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO event_stream (tenant_id, stream_id, sequence, event) VALUES (?, ?, ?, ?)`,
			tenantID.String(), streamID, i+1, `{"n":1}`); err != nil {
			t.Fatalf("seed event_stream row %d: %v", i+1, err)
		}
	}

	// Simulate the crash window: v4 applied (its CREATE TABLE and backfill
	// already ran and are visible), its own completion never recorded.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM plugin_migrations WHERE plugin_name = ? AND version = ?`,
		p.Info().Name, 4); err != nil {
		t.Fatalf("delete v4 tracking row: %v", err)
	}

	// The re-run must not fail. Before G2's fix, this died on
	// "Error 1062 (23000): Duplicate entry ... for key
	// 'event_stream_head.PRIMARY'" -- a worker that cannot boot.
	if err := plugin.RunMigrations(ctx, db, plugin.DialectMySQL, nil, lp); err != nil {
		t.Fatalf("re-run after v4's tracking row was lost: %v", err)
	}

	var head int64
	if err := db.QueryRowContext(ctx,
		`SELECT head_sequence FROM event_stream_head WHERE tenant_id = ? AND stream_id = ?`,
		tenantID.String(), streamID).Scan(&head); err != nil {
		t.Fatalf("read back head_sequence: %v", err)
	}
	if head != 3 {
		t.Errorf("head_sequence = %d after the re-run, want 3 (the backfill must not have "+
			"corrupted an existing, correct value)", head)
	}

	// v4 must be recorded as applied again, or every future boot repeats
	// this same re-run forever.
	var applied bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM plugin_migrations WHERE plugin_name = ? AND version = ?)`,
		p.Info().Name, 4).Scan(&applied); err != nil {
		t.Fatalf("check v4 tracking row: %v", err)
	}
	if !applied {
		t.Error("v4 is not recorded as applied after the re-run")
	}
}
