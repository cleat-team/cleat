package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A registered queue's rate_limit is enforced by the claim, not merely
// declared -- the behavioural half of cleat#1918, the way
// queue_limit_claim_test.go is the behavioural half of cleat#1116. The
// predicate text is held identical at every site by
// TestTheClaimableConcurrencyKeyPredicateIsIdenticalAtEverySite's rate arm;
// these hold the semantics that text encodes.

// countQueueRateTokens counts live queue_rate_tokens rows for a queue.
func countQueueRateTokens(ctx context.Context, db *sql.DB, dialect, name string) (int, error) {
	var q string
	switch dialect {
	case "postgres":
		q = `SELECT count(*) FROM queue_rate_tokens WHERE queue_name = $1 AND expires_at > now()`
	case "mysql":
		q = `SELECT count(*) FROM queue_rate_tokens WHERE queue_name = ? AND expires_at > NOW(6)`
	case "mssql":
		q = `SELECT count(*) FROM queue_rate_tokens WHERE queue_name = @p1 AND expires_at > SYSUTCDATETIME()`
	default:
		return 0, fmt.Errorf("unknown dialect %q", dialect)
	}
	var n int
	if err := db.QueryRowContext(ctx, q, name).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// TestARegisteredQueueRateLimitAdmitsAtMostItsLimit is the failing-twin shape
// flagged in cleat#1918's review: a rate-limited queue and an otherwise
// identical UNLIMITED queue are seeded with the same population and claimed
// in the same run. If the fixture were too small to exercise the limit at
// all -- too few candidates, or a limit no claim would ever reach -- the
// limited arm would pass by coincidence and this test would not catch a
// limiter that silently did nothing. The unlimited arm is what rules that
// out: it must admit MORE than the limit in the same claim that the limited
// arm does not exceed it, so the two arms can only both pass if the limit is
// actually doing the refusing.
func TestARegisteredQueueRateLimitAdmitsAtMostItsLimit(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			const (
				rateLimit         = 3
				ratePeriodSeconds = 3600 // long enough that no token expires mid-test
				concurrencyLimit  = 100  // high enough that concurrency never gates first
				perQueue          = 10
			)
			db := queueClaimTestDB(t, store)
			qs := NewQueueStore(db, backend.Name())

			limitedName := queueTestName("rate-limited")
			rl, rp := rateLimit, ratePeriodSeconds
			if err := qs.CreateQueue(ctx, DefaultTenantUUID, limitedName, concurrencyLimit, &rl, &rp); err != nil {
				t.Fatalf("CreateQueue(limited): %v", err)
			}

			unlimitedName := queueTestName("rate-unlimited")
			if err := qs.CreateQueue(ctx, DefaultTenantUUID, unlimitedName, concurrencyLimit, nil, nil); err != nil {
				t.Fatalf("CreateQueue(unlimited): %v", err)
			}

			starter, ok := store.(claimQueueConcurrencyKeyStarter)
			if !ok {
				t.Fatalf("%T cannot record a concurrency key", store)
			}
			for i := 0; i < perQueue; i++ {
				if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
					fmt.Sprintf("rate-limited-%d", i), "test-workflow", 1, json.RawMessage(`{}`),
					"", DefaultTenantUUID, 0, limitedName); err != nil {
					t.Fatalf("start limited run %d: %v", i, err)
				}
				if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
					fmt.Sprintf("rate-unlimited-%d", i), "test-workflow", 1, json.RawMessage(`{}`),
					"", DefaultTenantUUID, 0, unlimitedName); err != nil {
					t.Fatalf("start unlimited run %d: %v", i, err)
				}
			}

			claimed, err := store.ClaimWorkflows(ctx, "worker-1", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}

			// A claimed WorkflowInstance does not carry its concurrency key
			// back, so which queue admitted what is read from store state --
			// the same discriminator queue_limit_claim_test.go uses
			// (mustCountQueueHolders), applied here to the rate token table
			// too.
			gotLimited := mustCountQueueRateTokens(t, ctx, db, backend.Name(), limitedName)
			if gotLimited != rateLimit {
				t.Fatalf("rate-limited queue admitted %d, want exactly %d (the declared rate limit)", gotLimited, rateLimit)
			}
			limitedHolders := mustCountQueueHolders(t, ctx, db, backend.Name(), limitedName)
			if limitedHolders != rateLimit {
				t.Fatalf("rate-limited queue claimed %d workflows, want exactly %d", limitedHolders, rateLimit)
			}

			// The failing twin: prove the fixture actually distinguishes
			// "enforced" from "coincidentally under the limit" by requiring
			// the unlimited arm to admit MORE than the limit in this same run.
			unlimitedHolders := mustCountQueueHolders(t, ctx, db, backend.Name(), unlimitedName)
			if unlimitedHolders <= rateLimit {
				t.Fatalf("unlimited queue claimed %d workflows, want more than %d -- "+
					"if this arm does not exceed the limit too, the fixture cannot tell a real "+
					"limiter from one that does nothing", unlimitedHolders, rateLimit)
			}
			if unlimitedHolders != perQueue {
				t.Fatalf("unlimited queue claimed %d workflows, want all %d (no rate limit, no concurrency pressure at %d)",
					unlimitedHolders, perQueue, concurrencyLimit)
			}

			if len(claimed) != rateLimit+perQueue {
				t.Fatalf("claimed %d total, want %d (limited queue's %d plus unlimited queue's %d)",
					len(claimed), rateLimit+perQueue, rateLimit, perQueue)
			}

			// The deferred runs are deferred, not lost: nothing frees a rate
			// token before its window closes (ratePeriodSeconds is 3600s), so
			// a second claim admits none more from the limited queue.
			next, err := store.ClaimWorkflows(ctx, "worker-2", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows after first claim: %v", err)
			}
			for _, wf := range next {
				if strings.HasPrefix(wf.ID, "rate-limited-") {
					t.Fatalf("second claim took %s from the rate-limited queue; its window has not closed", wf.ID)
				}
			}
		})
	}
}

func mustCountQueueRateTokens(t *testing.T, ctx context.Context, db *sql.DB, dialect, name string) int {
	t.Helper()
	n, err := countQueueRateTokens(ctx, db, dialect, name)
	if err != nil {
		t.Fatalf("count queue_rate_tokens: %v", err)
	}
	return n
}
