package webhookingest

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestVersion10RetiresTheLegacySignalPushAcrossDialects is cleat#2689:
// signal_workflow_id/signal_name (webhook_sources) and retry_count/
// last_retry_at (webhook_events) were written and read exclusively by
// background.go's retry sweep (processBatch/retryEvent/markRetryFailed),
// deleted alongside this migration -- the static, non-correlated 1:1
// binding they implemented has no case left that correlated await_webhook
// (cleat#2649/cleat#2697) does not already serve better. See migrations.go's
// Version 10 comment for the full reasoning.
//
// This test proves the SCHEMA change landed correctly on every dialect,
// reading each dialect's own catalog rather than trusting a nil error from
// RunMigrations: all four columns are gone, and webhook_events' status/
// error_msg/processed -- still written by handleDeleteSource's cancellation
// -- survive untouched.
func TestVersion10RetiresTheLegacySignalPushAcrossDialects(t *testing.T) {
	for _, tc := range []struct {
		name string
		td   testutil.Dialect
	}{
		{"postgres", testutil.DialectPostgres},
		{"mysql", testutil.DialectMySQL},
		{"mssql", testutil.DialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.TestDB(t, tc.td)
			dialect := plugin.Dialect(string(tc.td))

			p := &Plugin{dialect: dialect}
			if err := plugin.RunMigrations(context.Background(), db, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("apply migrations: %v", err)
			}

			checks := []struct {
				table, column string
				wantExists    bool
			}{
				{"webhook_sources", "signal_workflow_id", false},
				{"webhook_sources", "signal_name", false},
				{"webhook_events", "retry_count", false},
				{"webhook_events", "last_retry_at", false},
				// Survivors: still written by handleDeleteSource's
				// cancellation (routes.go) and read by GET /ingest/events.
				{"webhook_events", "status", true},
				{"webhook_events", "error_msg", true},
				{"webhook_events", "processed", true},
			}

			for _, c := range checks {
				var query string
				switch tc.td {
				case testutil.DialectPostgres:
					query = `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)`
				case testutil.DialectMySQL:
					query = `SELECT COUNT(*) > 0 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`
				case testutil.DialectMSSQL:
					query = `SELECT CASE WHEN EXISTS(SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(@p1) AND name = @p2) THEN 1 ELSE 0 END`
				}
				var exists bool
				if err := db.QueryRowContext(context.Background(), query, c.table, c.column).Scan(&exists); err != nil {
					t.Fatalf("check %s.%s: %v", c.table, c.column, err)
				}
				if exists != c.wantExists {
					t.Errorf("on %s, %s.%s exists=%v, want %v", tc.name, c.table, c.column, exists, c.wantExists)
				}
			}
		})
	}
}
