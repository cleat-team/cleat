// insertEventIdempotent runs on SQL Server, scoped to its tenant.
//
// cleat#2920. This statement was BELIEVED covered, and the belief is written
// down: eventtriggers_dialect_arms_multidb_test.go:74 explains why it is not an
// Arm and names what covers it instead, including
//
//	TestPublishEventCarriesItsOwnTenant (PublishEvent, which calls
//	insertEventIdempotent, under real RLS enforcement)
//
// That test is POSTGRES-ONLY -- testutil.DialectPostgres,
// testutil.OpenPostgresRLSTestDB, testutil.PostgresRLSTestRole -- so the
// citation is true and silent about the dialect, inside a file whose entire
// subject is dialects. And it is not a mistake that could be repaired by
// changing a dialect constant: it exists to pin PublishEvent's SELF-SCOPING,
// which it proves by asserting that a tenantless read RAISES. That is
// PostgreSQL's cleat.assert_tenant_set(); SQL Server cannot raise from a filter
// predicate, so the same assertion there is unfalsifiable rather than merely
// unwritten.
//
// So this file does not restate that claim. It runs PublishEvent on each
// dialect -- the tenant travels as an argument and PublishEvent applies it
// itself with plugin.ForTenant -- and asserts the row landed, which is the part
// that had no dialect coverage at all.
package eventtriggers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestInsertEventIdempotentRunsOnEveryDialect(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{}
			// Init sets the package-level currentDialect, which publish.go uses
			// to pick the arm -- so a test that skips this runs whatever
			// dialect the last test left behind.
			if err := p.Init(ctx, &plugin.Environment{Dialect: dialect, Logger: quiet}); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}
			db := &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			eventID := uuid.New()

			matched, err := PublishEvent(ctx, db, quiet,
				&plugin.Environment{Dialect: dialect, Logger: quiet},
				eventID, tenantID, "cleat2920.test", json.RawMessage(`{"k":"v"}`), nil)
			if err != nil {
				t.Fatalf("PublishEvent on %s: %v", be.Name, err)
			}
			if matched != 0 {
				t.Errorf("matched %d subscriptions on %s, want 0 -- none was seeded", matched, be.Name)
			}

			// The row, read through CrossTenantConn rather than be.DB: on SQL
			// Server a tenant-scoped read with no session context is filtered
			// to nothing SILENTLY, which would make this assertion pass on
			// Postgres and fail here for a reason that is not the statement.
			readConn := be.CrossTenantConn(t, ctx,
				"cleat#2920: reading ingested_events, which SQL Server filters silently through be.DB")
			var n int
			if err := readConn.QueryRowContext(ctx,
				plugin.Rebind("SELECT COUNT(*) FROM ingested_events WHERE id = $1", dialect),
				eventID).Scan(&n); err != nil {
				t.Fatalf("count ingested_events on %s: %v", be.Name, err)
			}
			if n == 0 {
				t.Errorf("insertEventIdempotent on %s: PublishEvent returned no error but left no "+
					"ingested_events row for event %s", be.Name, eventID)
			}

			// And the idempotency the statement is named for: the same event
			// id published twice must not produce a second row. Without this
			// the test would pass against a plain INSERT that happens to be
			// correct on the first call.
			if _, err := PublishEvent(ctx, db, quiet,
				&plugin.Environment{Dialect: dialect, Logger: quiet},
				eventID, tenantID, "cleat2920.test", json.RawMessage(`{"k":"v"}`), nil); err != nil {
				t.Fatalf("second PublishEvent on %s: %v", be.Name, err)
			}
			if err := readConn.QueryRowContext(ctx,
				plugin.Rebind("SELECT COUNT(*) FROM ingested_events WHERE id = $1", dialect),
				eventID).Scan(&n); err != nil {
				t.Fatalf("re-count ingested_events on %s: %v", be.Name, err)
			}
			if n != 1 {
				t.Errorf("insertEventIdempotent on %s: publishing the same event id twice left %d rows, want 1", be.Name, n)
			}
		})
	}
}
