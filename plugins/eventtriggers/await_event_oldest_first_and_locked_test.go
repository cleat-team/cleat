package eventtriggers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
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

// mustInsertIngestedEventWithData is mustInsertIngestedEventAt with an
// explicit event_data, for tests that need to seed a row that cannot
// round-trip through json.Marshal -- see
// TestAwaitEventMarshalFailureLeavesEventUnconsumed.
func mustInsertIngestedEventWithData(t *testing.T, ctx context.Context, p *Plugin, id, tenantID uuid.UUID, eventType, eventData string, receivedAt time.Time) {
	t.Helper()
	if _, err := p.db.Exec(ctx, `
		INSERT INTO ingested_events (id, tenant_id, event_type, event_data, received_at, processed, status)
		VALUES ($1, $2, $3, $4, $5, false, 'pending')
	`, id, tenantID, eventType, eventData, receivedAt); err != nil {
		t.Fatalf("insert ingested_events %s: %v", id, err)
	}
}

// TestAwaitEventMarshalFailureLeavesEventUnconsumed is cleat#2654: a row
// whose event_data cannot round-trip through json.Marshal (corrupted, e.g.
// by the VARBINARY->NVARCHAR conversion cleat#2645's own CI run hit) used to
// be marshaled AFTER the claim transaction committed, so the failure was
// reported as an error while the event was already durably consumed and
// gone forever. This crosses the commit boundary deliberately: a test that
// only checks the returned error cannot see that difference, because both
// the fixed and unfixed code return an error here -- what distinguishes
// them is whether the row is still there to claim afterward.
//
// MSSQL-ONLY, not table-driven, and not an oversight: event_data is JSONB on
// Postgres and JSON on MySQL, and both dialects validate JSON syntax at
// INSERT and refuse this test's malformed literal before awaitEvent ever
// sees it (measured: Postgres returns "pq: invalid input syntax for type
// json (22P02)" on the seeding insert itself). MSSQL's event_data is
// NVARCHAR(MAX), a plain text column with no such check, which is exactly
// why the ORIGINAL corruption this issue cites (cleat#2645's CI failure) was
// only ever observed on that dialect -- this test reproduces the same
// precondition, not a weaker stand-in for it.
func TestAwaitEventMarshalFailureLeavesEventUnconsumed(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectMSSQL)
	dialect := plugin.DialectMSSQL
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	p := &Plugin{dialect: dialect, logger: quiet}
	if err := plugin.RunMigrations(context.Background(), db, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	p.db = &engine.SQLDBAdapter{DB: db, Dialect: dialect}

	tenantID := uuid.New()
	eventID := uuid.New()
	seedCtx := plugin.ForTenant(context.Background(), tenantID)

	// Not valid JSON, so the struct embedding it as json.RawMessage fails to
	// marshal -- verified directly: json.Marshal on
	// awaitEventOutput{EventData: json.RawMessage("not valid json{")} returns
	// a non-nil error and empty bytes.
	mustInsertIngestedEventWithData(t, seedCtx, p, eventID, tenantID,
		"order.corrupt", "not valid json{", time.Now().Add(-time.Hour))

	ctx := plugin.WithCallContext(seedCtx, &plugin.CallContext{
		TenantID:   tenantID.String(),
		WorkflowID: "wf-marshal-failure-mssql",
	})
	// Asserted against the specific marshal error, not just "some error":
	// the property under test is ORDERING (a marshal failure leaves the row
	// unconsumed), and a bare err != nil passes just as well if awaitEvent
	// failed for an unrelated reason before ever reaching the claim -- e.g.
	// a broken claim query or a bad input -- where processed is ALSO false
	// for a reason that has nothing to do with this fix. That version of
	// the test cannot tell "the fix works" from "it fell over earlier".
	if _, err := p.awaitEvent(ctx, `{"event_type":"order.corrupt","timeout_ms":1000}`); err == nil ||
		!strings.Contains(err.Error(), "marshal await_event output") {
		t.Fatalf("awaitEvent: expected the marshal error, got: %v", err)
	}

	// The property under test: NOT consumed, so a later claim can still
	// retry it. Asserted directly against the row rather than by calling
	// awaitEvent again, since a second call would hit the same marshal
	// failure and prove nothing beyond the first call.
	var processed bool
	row := p.db.QueryRow(seedCtx,
		`SELECT processed FROM ingested_events WHERE id = $1`, eventID)
	if err := plugin.ScanRow(row, &processed); err != nil {
		t.Fatalf("query processed flag: %v", err)
	}
	if processed {
		t.Fatal("event was marked processed despite the output failing to marshal -- " +
			"it is now durably consumed and unrecoverable, the exact failure cleat#2654 describes")
	}
}

// TestAwaitEventClaimsOldestAcrossDialects is cleat-review's GAP 1, fix half
// one, UPDATED for cleat#2652: it used to seed the NEWER event first (so a
// pick-by-insertion-order implementation would also get it wrong) and assert
// the OLDER one was claimed. That inverted the moment #2652 moved the claim
// query's ORDER BY from received_at to seq -- the whole point of that
// change is that received_at (NOW()/SYSUTCDATETIME() at transaction-START)
// is not reliably monotonic under concurrent commits, so ordering now
// follows seq (assigned once, at INSERT, in the order INSERT statements are
// issued), not the wall clock. This seeds the two events with received_at
// values deliberately out of step with insertion order -- the row inserted
// FIRST carries the LATER received_at -- and asserts the FIRST-INSERTED one
// is claimed, against a real server of each dialect, not the fake driver's
// own independent oldest-picking logic (which still keys off received_at;
// see the file header above).
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
			insertedFirst := uuid.New()
			insertedSecond := uuid.New()
			now := time.Now()

			seedCtx := plugin.ForTenant(context.Background(), tenantID)
			// insertedFirst carries the LATER received_at and insertedSecond
			// the EARLIER one -- out of step with insertion order on purpose.
			// An implementation that still ordered by received_at would
			// return insertedSecond here; seq ordering returns insertedFirst.
			mustInsertIngestedEventAt(t, seedCtx, p, insertedFirst, tenantID, "order.created", now)
			mustInsertIngestedEventAt(t, seedCtx, p, insertedSecond, tenantID, "order.created", now.Add(-time.Hour))

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
			if result.EventID != insertedFirst.String() {
				t.Errorf("claimed event %s, want the FIRST-INSERTED event %s (second-inserted, "+
					"with the earlier received_at, was %s) -- on %s, "+
					"queryOldestUnprocessedEventForClaim's ORDER BY is not seq ascending",
					result.EventID, insertedFirst, insertedSecond, tc.name)
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
				// MSSQL goes through the real production path
				// (claimOldestUnprocessedEventMSSQL, claim.go) rather than a
				// single locked SELECT -- cleat#2821/#2866 replaced the
				// latter because it does not lock only the row it returns on
				// this dialect. Calling the unexported function directly
				// (same package) keeps this test exercising the actual
				// mechanism instead of a hand-rolled duplicate of it.
				if dialect == plugin.DialectMSSQL {
					claimed, err := claimOldestUnprocessedEventMSSQL(seedCtx, tx, tenantID.String(), "order.created", "", "", "")
					if err != nil {
						_ = tx.Rollback()
						t.Fatalf("claim query: %v", err)
					}
					if claimed == nil {
						_ = tx.Rollback()
						t.Fatalf("claim query: no claimable row found")
					}
					return tx, claimed.EventID
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
					"on %s, the claim mechanism is not locking the row it selects",
					gotB, tc.name)
			}
			if gotB != secondID {
				t.Errorf("second claim got %s, want the next-oldest event %s", gotB, secondID)
			}
		})
	}
}

// TestAwaitEventOldestClaimDoesNotStarveASecondOldestEventAtScale is
// cleat#2821/#2866, and it is the test TestAwaitEventConcurrentClaimsSkipTheLockedRow
// above cannot be -- cleat-review's finding on #2869: that test passes on
// unfixed develop MSSQL, on a fresh database, because two rows belonging to
// one tenant are too few to trigger the bug. #2821's own isolation needed a
// realistic cross-tenant backlog OLDER than the claiming tenant's own rows
// before the starvation reproduced; a near-empty table happens to dodge it,
// which is exactly how Version 8's migration comment (migrations.go) first
// mis-stated the existing index as a sufficient fix.
//
// Drives the real tryClaim (claim.go), not a hand-rolled query, holding A's
// claim open through its beforeCommit hook -- a genuine second goroutine and
// a genuine second transaction on a genuine second connection, not the
// sequential same-goroutine simulation the test above uses. Runs on all
// three dialects: Postgres and MySQL are not expected to ever have exhibited
// this (FOR UPDATE SKIP LOCKED locks only the row it returns on both), and
// this test is what would catch a regression there too.
func TestAwaitEventOldestClaimDoesNotStarveASecondOldestEventAtScale(t *testing.T) {
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
			oldestID := uuid.New()
			secondID := uuid.New()
			now := time.Now()
			seedCtx := plugin.ForTenant(context.Background(), tenantID)

			// The cross-tenant backlog cleat#2821's own isolation needed --
			// without it, this case does not distinguish fixed from unfixed
			// code on any dialect.
			const backlog = 200
			for i := 0; i < backlog; i++ {
				otherTenant := uuid.New()
				otherCtx := plugin.ForTenant(context.Background(), otherTenant)
				mustInsertIngestedEventAt(t, otherCtx, p, uuid.New(), otherTenant, "order.created",
					now.Add(-5*time.Hour-time.Duration(i)*time.Second))
			}

			mustInsertIngestedEventAt(t, seedCtx, p, oldestID, tenantID, "order.created", now.Add(-2*time.Hour))
			mustInsertIngestedEventAt(t, seedCtx, p, secondID, tenantID, "order.created", now.Add(-time.Hour))

			// cleat-review's finding on #2869: SQL Server's plan cache carries
			// this test's own isolation hazard. Run alone against a fresh
			// database, the unfixed (pre-#2869) query plan starves B
			// deterministically -- but run as part of the whole package, as
			// CI does, a plan CACHED by an earlier test's identical query text
			// (TestAwaitEventConcurrentClaimsSkipTheLockedRow, same statement,
			// same shape) gets reused here and happens not to exhibit the
			// starvation, so the unfixed code passed 2/2 run that way. A
			// DATABASE-scoped clear (not server-wide, to avoid disturbing any
			// other database sharing this instance) forces a fresh
			// optimization for this test's own statement regardless of what
			// ran before it in the same process. Postgres/MySQL have no
			// equivalent concept here and need no such step.
			if tc.td == testutil.DialectMSSQL {
				if _, err := p.db.Exec(context.Background(),
					`ALTER DATABASE SCOPED CONFIGURATION CLEAR PROCEDURE_CACHE`); err != nil {
					t.Fatalf("clear MSSQL procedure cache: %v", err)
				}
			}

			holding := make(chan struct{})
			release := make(chan struct{})
			type claimResult struct {
				claimed *ClaimedEvent
				err     error
			}
			doneA := make(chan claimResult, 1)

			go func() {
				claimed, err := tryClaim(seedCtx, p.db, dialect, tenantID.String(), "order.created", "", "", "",
					func(c *ClaimedEvent) error {
						close(holding)
						<-release
						return nil
					})
				doneA <- claimResult{claimed, err}
			}()

			<-holding // A has claimed and is holding its transaction open, uncommitted

			claimedB, errB := tryClaim(seedCtx, p.db, dialect, tenantID.String(), "order.created", "", "", "", nil)

			close(release)
			resA := <-doneA

			if resA.err != nil {
				t.Fatalf("claim A: %v", resA.err)
			}
			if resA.claimed == nil || resA.claimed.EventID != oldestID {
				t.Fatalf("claim A got %+v, want the oldest event %s", resA.claimed, oldestID)
			}
			if errB != nil {
				t.Fatalf("claim B: %v", errB)
			}
			if claimedB == nil {
				t.Fatalf("claim B found NOTHING while A's claim on the oldest row was still open -- "+
					"on %s, a concurrent claim is starved by a lock on a DIFFERENT row it never "+
					"needed (cleat#2821)", tc.name)
			}
			if claimedB.EventID != secondID {
				t.Errorf("claim B got %s, want the second-oldest event %s", claimedB.EventID, secondID)
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
