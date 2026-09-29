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

// cleat#2647, P1's read side (cleat#2625). Round 1 (#2646) added key1/key2/
// key3 to the schema and to the PUSH side (signalAwaiters/unregisterAwaiter);
// this is the READ side -- awaitEvent's claim query and its registerAwaiter/
// unregisterAwaiter call sites, which #2646 deliberately left passing
// "","","" because #2645 was concurrently rewriting the same query for
// oldest-first + atomic claim (cleat#2641), and touching it twice at once was
// the collision both streams agreed to avoid.
//
// mustInsertIngestedEventAtWithKey is mustInsertIngestedEventAt
// (await_event_oldest_first_and_locked_test.go) plus a key1 argument, so a
// test can seed two events of the same tenant+type that differ only in their
// correlation key -- the shape this predicate exists to distinguish.
func mustInsertIngestedEventAtWithKey(t *testing.T, ctx context.Context, p *Plugin, id, tenantID uuid.UUID, eventType, key1 string, receivedAt time.Time) {
	t.Helper()
	if _, err := p.db.Exec(ctx, `
		INSERT INTO ingested_events (id, tenant_id, event_type, event_data, key1, received_at, processed, status)
		VALUES ($1, $2, $3, $4, $5, $6, false, 'pending')
	`, id, tenantID, eventType, "{}", key1, receivedAt); err != nil {
		t.Fatalf("insert ingested_events %s: %v", id, err)
	}
}

// mustInsertIngestedEventAtWithKeys is mustInsertIngestedEventAtWithKey with
// both key1 and key2, for TestAwaitEventCorrelatesOnTwoKeysAcrossDialects.
func mustInsertIngestedEventAtWithKeys(t *testing.T, ctx context.Context, p *Plugin, id, tenantID uuid.UUID, eventType, key1, key2 string, receivedAt time.Time) {
	t.Helper()
	if _, err := p.db.Exec(ctx, `
		INSERT INTO ingested_events (id, tenant_id, event_type, event_data, key1, key2, received_at, processed, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, false, 'pending')
	`, id, tenantID, eventType, "{}", key1, key2, receivedAt); err != nil {
		t.Fatalf("insert ingested_events %s: %v", id, err)
	}
}

// TestAwaitEventCorrelatesByKeyAcrossDialects is the same shape as
// TestAKeyedPublishSignalsOnlyTheMatchingAwaiter, on the read side rather
// than the push side: THE CONTROL IS THE SECOND HALF, NOT A SEPARATE TEST.
// Asserting only "the A-991 call claimed the A-991 event" would pass just as
// well against the pre-#2647 code, which matched on (tenant_id, event_type)
// alone and would have claimed WHICHEVER of the two unprocessed rows sorted
// first by received_at -- indistinguishable from correct correlation unless
// something also asserts the OTHER row was left alone.
func TestAwaitEventCorrelatesByKeyAcrossDialects(t *testing.T) {
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
			eventA := uuid.New()
			eventB := uuid.New()
			now := time.Now()

			seedCtx := plugin.ForTenant(context.Background(), tenantID)
			// A-991's event is OLDER, so a call correlating on "B-2" that
			// (wrongly) ignored the key predicate would claim A-991's event
			// instead, via the oldest-first ordering -- the ordering and the
			// key predicate would have to BOTH be right, in different
			// directions, for the wrong implementation to pass by accident.
			mustInsertIngestedEventAtWithKey(t, seedCtx, p, eventA, tenantID, "order.paid", "A-991", now.Add(-time.Hour))
			mustInsertIngestedEventAtWithKey(t, seedCtx, p, eventB, tenantID, "order.paid", "B-2", now)

			// registrationKey (keys.go) has no tenant component -- workflowID
			// + eventType + keys alone -- so a literal workflowID would
			// collide across repeated manual invocations of this test
			// against one persistent database, even though tenantID is
			// fresh. Suffixed with tenantID for exactly that reason.
			ctx := plugin.WithCallContext(seedCtx, &plugin.CallContext{
				TenantID:   tenantID.String(),
				WorkflowID: "wf-correlate-" + tc.name + "-" + tenantID.String(),
			})

			inputJSON, err := json.Marshal(awaitEventInput{
				EventType: "order.paid",
				Keys:      []string{"B-2"},
			})
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}
			out, err := p.awaitEvent(ctx, string(inputJSON))
			if err != nil {
				t.Fatalf("awaitEvent: %v", err)
			}

			var result awaitEventOutput
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatalf("unmarshal output: %v", err)
			}

			// FLOOR: without this, a predicate that matches nothing would
			// make the isolation assertion below pass vacuously.
			if !result.Found {
				t.Fatalf("UNMEASURED: expected Found=true for a call correlated on B-2, got false")
			}
			if result.EventID != eventB.String() {
				t.Errorf("claimed event %s, want B-2's event %s (A-991's was %s) -- "+
					"on %s, the claim query's key predicate is not selecting the "+
					"correlated row", result.EventID, eventB, eventA, tc.name)
			}

			// THE ASSERTION THIS TEST EXISTS FOR: A-991's event, which
			// arrived first and would have won on received_at alone, is
			// still unprocessed -- a keyed claim for one order's event does
			// not consume a different order's event of the same type.
			//
			// SELECT processed, not "SELECT NOT processed": T-SQL has no
			// boolean type, so NOT on a BIT column in a select list is a
			// binding error ("Incorrect syntax near the keyword 'NOT'",
			// 156) rather than the boolean expression it is on the other
			// two dialects -- the same hazard queryUnprocessedEvents'
			// doc comment in queries.go already names for a WHERE clause.
			// Invert in Go instead, which needs no dialect arm.
			//
			// p.db.QueryRow, not a bare db.QueryRowContext: p.db
			// (engine.SQLDBAdapter) is what reads ctx's tenant value (set
			// by plugin.ForTenant, in seedCtx above) and sets the actual
			// MSSQL session context a FORCE RLS table requires -- a plain
			// *sql.DB query carries no such setup and a real row reads
			// back as "sql: no rows in result set", not as evidence the
			// row is gone.
			var processed bool
			if err := p.db.QueryRow(ctx,
				`SELECT processed FROM ingested_events WHERE id = $1`, eventA).
				Scan(&processed); err != nil {
				t.Fatalf("check A-991's event: %v", err)
			}
			if processed {
				t.Errorf("on %s, A-991's event was consumed by a claim correlated on B-2", tc.name)
			}
		})
	}
}

// TestAwaitEventRegistersAndUnregistersWithTheCorrelatedKeysAcrossDialects
// covers the NOT-FOUND path: registerAwaiter/unregisterAwaiter's "","","" is
// gone (#2647), replaced with input.Keys via the same keySlots() validation
// the claim query is matched against. Proven by reading event_awaiters
// directly rather than trusting awaitEvent's own report -- the row registerAwaiter
// writes is the thing a later publish's signalAwaiters will match against,
// so its key1/key2/key3 columns are the property that matters, not whether
// the call returned without error.
func TestAwaitEventRegistersAndUnregistersWithTheCorrelatedKeysAcrossDialects(t *testing.T) {
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
			// Suffixed with tenantID: registrationKey has no tenant
			// component (see the same note on TestAwaitEventCorrelatesByKeyAcrossDialects
			// above), so a literal workflowID collides across repeated
			// manual invocations against one persistent database.
			workflowID := "wf-register-" + tc.name + "-" + tenantID.String()

			// ForTenant, not just WithCallContext: ForTenant is what
			// engine.SQLDBAdapter's tenant-scoped transaction reads to set
			// the actual RLS session context, and WithCallContext only
			// carries CallContext for the plugin's own cc.TenantID/
			// cc.WorkflowID reads -- two separate mechanisms. Missing
			// ForTenant here made registerAwaiter's INSERT run with no
			// tenant context, and MSSQL's FORCE RLS refuses that outright
			// (block predicate conflict, 33504) rather than silently
			// scoping to nothing the way a plain filter predicate would.
			ctx := plugin.WithCallContext(plugin.ForTenant(context.Background(), tenantID), &plugin.CallContext{
				TenantID:   tenantID.String(),
				WorkflowID: workflowID,
			})

			inputJSON, err := json.Marshal(awaitEventInput{
				EventType: "shipment.delayed",
				Keys:      []string{"S-77"},
			})
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}
			out, err := p.awaitEvent(ctx, string(inputJSON))
			if err != nil {
				t.Fatalf("awaitEvent: %v", err)
			}
			var result awaitEventOutput
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatalf("unmarshal output: %v", err)
			}
			if result.Found {
				t.Fatalf("UNMEASURED: expected Found=false against an empty table, got true")
			}

			var key1, key2, key3 string
			if err := p.db.QueryRow(ctx,
				`SELECT key1, key2, key3 FROM event_awaiters WHERE workflow_id = $1 AND event_type = $2`,
				workflowID, "shipment.delayed").Scan(&key1, &key2, &key3); err != nil {
				t.Fatalf("read registered awaiter on %s: %v", tc.name, err)
			}
			if key1 != "S-77" || key2 != "" || key3 != "" {
				t.Errorf("on %s, registered awaiter carries key1=%q key2=%q key3=%q, want key1=\"S-77\" key2=\"\" key3=\"\" -- "+
					"registerAwaiter is not receiving input.Keys", tc.name, key1, key2, key3)
			}

			// Now the matching event arrives; awaitEvent should find it AND
			// unregister the awaiter it just wrote above.
			eventID := uuid.New()
			mustInsertIngestedEventAtWithKey(t, plugin.ForTenant(context.Background(), tenantID), p,
				eventID, tenantID, "shipment.delayed", "S-77", time.Now())

			out, err = p.awaitEvent(ctx, string(inputJSON))
			if err != nil {
				t.Fatalf("second awaitEvent: %v", err)
			}
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatalf("unmarshal second output: %v", err)
			}
			if !result.Found || result.EventID != eventID.String() {
				t.Fatalf("on %s, second awaitEvent did not claim the S-77 event: found=%v id=%s",
					tc.name, result.Found, result.EventID)
			}

			var remaining int
			if err := p.db.QueryRow(ctx,
				`SELECT COUNT(*) FROM event_awaiters WHERE workflow_id = $1 AND event_type = $2`,
				workflowID, "shipment.delayed").Scan(&remaining); err != nil {
				t.Fatalf("count awaiters after claim on %s: %v", tc.name, err)
			}
			if remaining != 0 {
				t.Errorf("on %s, %d awaiter row(s) remain after the correlated event was claimed, want 0 -- "+
					"unregisterAwaiter is not matching on the same keys registerAwaiter wrote", tc.name, remaining)
			}
		})
	}
}

// TestAwaitEventKeyComparisonIsCaseSensitiveAcrossDialects is the dialect
// hazard the design doc's §4.3 names directly: key1/key2/key3 are declared
// with an explicit BINARY collation on every dialect (COLLATE "C" on
// Postgres, utf8mb4_bin on MySQL, Latin1_General_BIN2 on MSSQL) specifically
// because the alternative -- inheriting the server default -- is
// case-insensitive on MySQL 8 (utf8mb4_0900_ai_ci) and would match "ORDER-1"
// against "order-1", two distinct business keys, on one dialect only. This
// proves the migration's explicit collation is actually taking effect on the
// comparison this PR adds, not merely present in the DDL: an event keyed
// "Order-1" must NOT satisfy a claim correlated on "order-1", on any of the
// three dialects.
func TestAwaitEventKeyComparisonIsCaseSensitiveAcrossDialects(t *testing.T) {
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
			eventID := uuid.New()

			seedCtx := plugin.ForTenant(context.Background(), tenantID)
			mustInsertIngestedEventAtWithKey(t, seedCtx, p, eventID, tenantID,
				"case.test", "Order-1", time.Now())

			// Suffixed with tenantID for the same reason as the other two
			// tests in this file: this call reaches awaitEvent's not-found
			// path, which registers an awaiter, and registrationKey has no
			// tenant component.
			ctx := plugin.WithCallContext(seedCtx, &plugin.CallContext{
				TenantID:   tenantID.String(),
				WorkflowID: "wf-case-" + tc.name + "-" + tenantID.String(),
			})
			inputJSON, err := json.Marshal(awaitEventInput{
				EventType: "case.test",
				Keys:      []string{"order-1"},
			})
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}
			out, err := p.awaitEvent(ctx, string(inputJSON))
			if err != nil {
				t.Fatalf("awaitEvent: %v", err)
			}
			var result awaitEventOutput
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatalf("unmarshal output: %v", err)
			}
			if result.Found {
				t.Errorf("on %s, a claim correlated on \"order-1\" matched an event keyed "+
					"\"Order-1\" -- key1's collation is not case-sensitive here", tc.name)
			}
		})
	}
}

// TestAwaitEventCorrelatesOnTwoKeysAcrossDialects is the design doc's §14
// "known-positive: a correct two-key correlation ... builds and matches" row,
// and it is not redundant with TestAwaitEventCorrelatesByKeyAcrossDialects
// above: that test varies only key1, so a predicate that checks key1 but
// silently ignores key2 (dropped from the WHERE clause, or bound to the
// wrong placeholder) would still pass it. Here both seeded events share
// key1="B-2" and differ ONLY on key2 ("west" vs "east"), so claiming the
// right one is evidence key2 is genuinely part of the predicate, not merely
// present in the query text.
func TestAwaitEventCorrelatesOnTwoKeysAcrossDialects(t *testing.T) {
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
			eventWest := uuid.New()
			eventEast := uuid.New()
			now := time.Now()

			seedCtx := plugin.ForTenant(context.Background(), tenantID)
			mustInsertIngestedEventAtWithKeys(t, seedCtx, p, eventWest, tenantID, "order.paid", "B-2", "west", now)
			mustInsertIngestedEventAtWithKeys(t, seedCtx, p, eventEast, tenantID, "order.paid", "B-2", "east", now)

			ctx := plugin.WithCallContext(seedCtx, &plugin.CallContext{
				TenantID:   tenantID.String(),
				WorkflowID: "wf-twokey-" + tc.name + "-" + tenantID.String(),
			})
			inputJSON, err := json.Marshal(awaitEventInput{
				EventType: "order.paid",
				Keys:      []string{"B-2", "west"},
			})
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}
			out, err := p.awaitEvent(ctx, string(inputJSON))
			if err != nil {
				t.Fatalf("awaitEvent: %v", err)
			}
			var result awaitEventOutput
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatalf("unmarshal output: %v", err)
			}
			if !result.Found {
				t.Fatalf("UNMEASURED: expected Found=true for a call correlated on (B-2, west), got false")
			}
			if result.EventID != eventWest.String() {
				t.Errorf("on %s, claimed event %s, want the (B-2, west) event %s (the (B-2, east) "+
					"event was %s) -- key2 is not part of the claim predicate", tc.name, result.EventID,
					eventWest, eventEast)
			}
		})
	}
}
