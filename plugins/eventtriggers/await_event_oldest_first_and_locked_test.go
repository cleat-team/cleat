package eventtriggers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// cleat-review on #2645, cleat#2641: the fake-driver regression tests in
// eventtriggers_behavioral_test.go do not test the SQL this PR changed.
// etConn routes the claim by matching a fixed column list, then picks the
// oldest row IN GO regardless of the statement text -- so reverting
// queryOldestUnprocessedEventForClaim's `ORDER BY received_at` back to
// `DESC` in all three dialect arms leaves TestAwaitEventPicksOldestUnprocessedEvent
// PASSING. Confirmed directly: that revert does not turn the fake-driver test
// red. The lock clause (`FOR UPDATE SKIP LOCKED` / `WITH (UPDLOCK, READPAST,
// ROWLOCK)`) is untested anywhere by the fake driver, which ignores it
// entirely.
//
// These tests drive the real statement against a real server of each
// configured dialect, the same reason plugins/plugintest exists (cleat#1133):
// a fake driver that pattern-matches a query string cannot disagree with the
// query, and a lexical check cannot tell a binding error from a syntax error
// on SQL Server. MSSQL is included in the dialect list and simply skips
// itself when CLEAT_TEST_MSSQL is unset -- unmeasured here, not asserted.

// mustInsertIngestedEventAt inserts one ingested_events row with an explicit
// received_at, bypassing the plugin's own NOW()-based insert path so the two
// rows in a test can be placed in a known order regardless of wall-clock
// timing or database round-trip order.
//
// event_data is bound as a STRING, not []byte, matching publish.go's own
// `db.Exec(ctx, ..., string(eventData))` -- measured the hard way on CI's
// real MSSQL, not assumed. A []byte argument here bound as VARBINARY, and
// SQL Server's implicit VARBINARY->NVARCHAR conversion for the NVARCHAR(MAX)
// column re-interprets the bytes rather than decoding them as text, so the
// row that came back read as corrupt JSON: awaitEvent's
// `outJSON, _ := json.Marshal(output)` discarded json.Marshal's error on
// the resulting invalid json.RawMessage and returned "" with a nil error,
// which TestAwaitEventClaimsOldestAcrossDialects/mssql then failed on with
// "unmarshal output: unexpected end of JSON input" -- a failure with no
// error message from awaitEvent itself, because the error was the one this
// helper's own bad argument discarded two layers up. PostgreSQL and MySQL
// tolerated the []byte fine, which is why this was invisible locally.
func mustInsertIngestedEventAt(t *testing.T, ctx context.Context, p *Plugin, id, tenantID uuid.UUID, eventType string, receivedAt time.Time) {
	t.Helper()
	if _, err := p.db.Exec(ctx, `
		INSERT INTO ingested_events (id, tenant_id, event_type, event_data, received_at, processed, status)
		VALUES ($1, $2, $3, $4, $5, false, 'pending')
	`, id, tenantID, eventType, "{}", receivedAt); err != nil {
		t.Fatalf("insert ingested_events %s: %v", id, err)
	}
}

// TestAwaitEventClaimsOldestAcrossDialects is cleat-review's GAP 1, fix half
// one: it seeds the NEWER event first (so a pick-by-insertion-order
// implementation would also get it wrong) and asserts the OLDER one is
// claimed, against a real server of each dialect -- not the fake driver's own
// independent oldest-picking logic.
func TestAwaitEventClaimsOldestAcrossDialects(t *testing.T) {
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
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			p := &Plugin{dialect: dialect, logger: quiet}
			if err := plugin.RunMigrations(context.Background(), db, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("apply migrations: %v", err)
			}
			p.db = &engine.SQLDBAdapter{DB: db, Dialect: dialect}

			tenantID := uuid.New()
			newerID := uuid.New()
			olderID := uuid.New()
			now := time.Now()

			seedCtx := plugin.ForTenant(context.Background(), tenantID)
			// The NEWER row is inserted FIRST, so an implementation that picked
			// by insertion order (or by the pre-#2641 DESC ordering) would also
			// return the wrong one here.
			mustInsertIngestedEventAt(t, seedCtx, p, newerID, tenantID, "order.created", now)
			mustInsertIngestedEventAt(t, seedCtx, p, olderID, tenantID, "order.created", now.Add(-time.Hour))

			ctx := plugin.WithCallContext(seedCtx, &plugin.CallContext{
				TenantID:   tenantID.String(),
				WorkflowID: "wf-oldest-first-" + tc.name,
			})
			out, err := p.awaitEvent(ctx, `{"event_type":"order.created","timeout_ms":1000}`)
			if err != nil {
				t.Fatalf("awaitEvent: %v", err)
			}

			var result awaitEventOutput
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatalf("unmarshal output: %v", err)
			}
			if !result.Found {
				t.Fatal("expected Found=true")
			}
			if result.EventID != olderID.String() {
				t.Errorf("claimed event %s, want the OLDER event %s (newer was %s) -- "+
					"on %s, queryOldestUnprocessedEventForClaim's ORDER BY is not ascending",
					result.EventID, olderID, newerID, tc.name)
			}
		})
	}
}

// TestAwaitEventConcurrentClaimsSkipTheLockedRow is cleat-review's GAP 1, fix
// half two: it opens two transactions directly (bypassing awaitEvent's own
// commit, which would close the window before a second claim could run) and
// asserts they claim DIFFERENT rows. Without FOR UPDATE SKIP LOCKED / WITH
// (UPDLOCK, READPAST, ROWLOCK), the second transaction's plain SELECT sees
// the same uncommitted-but-unprocessed row the first one holds and returns
// it too -- there is nothing else in a bare SELECT that would stop it.
func TestAwaitEventConcurrentClaimsSkipTheLockedRow(t *testing.T) {
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
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			p := &Plugin{dialect: dialect, logger: quiet}
			if err := plugin.RunMigrations(context.Background(), db, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("apply migrations: %v", err)
			}
			p.db = &engine.SQLDBAdapter{DB: db, Dialect: dialect}

			tenantID := uuid.New()
			firstID := uuid.New()
			secondID := uuid.New()
			now := time.Now()

			seedCtx := plugin.ForTenant(context.Background(), tenantID)
			mustInsertIngestedEventAt(t, seedCtx, p, firstID, tenantID, "order.created", now.Add(-2*time.Hour))
			mustInsertIngestedEventAt(t, seedCtx, p, secondID, tenantID, "order.created", now.Add(-time.Hour))

			claim := func() (plugin.PluginTx, uuid.UUID) {
				t.Helper()
				tx, err := p.db.Begin(seedCtx)
				if err != nil {
					t.Fatalf("begin claim tx: %v", err)
				}
				var (
					eventID    uuid.UUID
					eventType  string
					eventData  []byte
					receivedAt time.Time
				)
				err = plugin.ScanRow(tx.QueryRow(seedCtx,
					queryOldestUnprocessedEventForClaim.For(dialect),
					tenantID, "order.created", "", "", ""), &eventID, &eventType, &eventData, &receivedAt)
				if err != nil {
					_ = tx.Rollback()
					t.Fatalf("claim query: %v", err)
				}
				return tx, eventID
			}

			// txA claims and HOLDS its lock -- not committed or rolled back yet,
			// so the row stays locked until the explicit cleanup below.
			txA, gotA := claim()
			defer txA.Rollback()

			// txB claims while txA's lock is still held. On a real server this is
			// SEQUENTIAL, not concurrent -- the point is not a race, it is
			// whether a second claim sees the first row as available.
			txB, gotB := claim()
			defer txB.Rollback()

			if gotA != firstID {
				t.Errorf("first claim got %s, want the oldest event %s", gotA, firstID)
			}
			if gotB == gotA {
				t.Errorf("second claim got the SAME row (%s) the first claim is still holding -- "+
					"on %s, the claim query is not locking the row it selects "+
					"(FOR UPDATE SKIP LOCKED / WITH (UPDLOCK, READPAST, ROWLOCK) is missing or ineffective)",
					gotB, tc.name)
			}
			if gotB != secondID {
				t.Errorf("second claim got %s, want the next-oldest event %s", gotB, secondID)
			}
		})
	}
}

// TestAwaitEventConcurrentClaimsOfDifferentEventTypesDoNotInterfereOnMySQL
// settles a question cleat-review raised rather than assumed: InnoDB's
// locking reads take next-key locks on the index records they SCAN under
// REPEATABLE READ (the default here), not only on the rows a non-indexed
// WHERE clause eventually keeps -- idx_ingested_events_unprocessed is on
// (processed, received_at) alone, so tenant_id and event_type are filtered
// after the index lookup. If that scan locked a row of a DIFFERENT event
// type it passed over, a claim for that other type would see it as locked
// and SKIP LOCKED would skip it, reporting no event where one exists.
//
// Measured directly rather than assumed: claiming "type.x" while it is held
// does not block or skip a concurrent claim for "type.y", even when "type.y"
// sits adjacent to "type.x" in received_at order. This is a measurement at
// this table's current size and index shape, not a proof for every
// possible query plan -- if a future index change makes MySQL choose a
// different scan, this test is what would catch a regression.
func TestAwaitEventConcurrentClaimsOfDifferentEventTypesDoNotInterfereOnMySQL(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectMySQL)
	dialect := plugin.DialectMySQL
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	p := &Plugin{dialect: dialect, logger: quiet}
	if err := plugin.RunMigrations(context.Background(), db, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	p.db = &engine.SQLDBAdapter{DB: db, Dialect: dialect}

	tenantID := uuid.New()
	xID := uuid.New()
	yID := uuid.New()
	now := time.Now()

	seedCtx := plugin.ForTenant(context.Background(), tenantID)
	// Adjacent in received_at, so a scan claiming "type.x" passes right by
	// "type.y" in index order.
	mustInsertIngestedEventAt(t, seedCtx, p, xID, tenantID, "type.x", now.Add(-2*time.Hour))
	mustInsertIngestedEventAt(t, seedCtx, p, yID, tenantID, "type.y", now.Add(-time.Hour))

	claim := func(eventType string) (plugin.PluginTx, uuid.UUID, error) {
		t.Helper()
		tx, err := p.db.Begin(seedCtx)
		if err != nil {
			t.Fatalf("begin claim tx: %v", err)
		}
		var (
			eventID    uuid.UUID
			gotType    string
			eventData  []byte
			receivedAt time.Time
		)
		err = plugin.ScanRow(tx.QueryRow(seedCtx,
			queryOldestUnprocessedEventForClaim.For(dialect),
			tenantID, eventType, "", "", ""), &eventID, &gotType, &eventData, &receivedAt)
		return tx, eventID, err
	}

	txX, gotX, err := claim("type.x")
	if err != nil {
		t.Fatalf("claim type.x: %v", err)
	}
	defer txX.Rollback()
	if gotX != xID {
		t.Fatalf("claimed %s for type.x, want %s", gotX, xID)
	}

	// txX's lock on the "type.x" row is still held here.
	txY, gotY, err := claim("type.y")
	if err != nil {
		t.Fatalf("claiming type.y while an UNRELATED type.x row is locked failed: %v -- "+
			"InnoDB's scan for the type.x claim locked a row it should have passed over", err)
	}
	defer txY.Rollback()
	if gotY != yID {
		t.Errorf("claimed %s for type.y, want %s", gotY, yID)
	}
}
