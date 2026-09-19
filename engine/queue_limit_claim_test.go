package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// A registered queue's concurrency_limit is enforced by the claim, not merely
// declared. These tests are the behavioural half of cleat#1116; the predicate
// text is held identical at every site by
// TestTheClaimableConcurrencyKeyPredicateIsIdenticalAtEverySite, and these hold
// the semantics that text encodes -- a registered queue admits at most its
// limit, whether the claims arrive one at a time or concurrently.

// queueClaimTestDB returns the raw *sql.DB behind a store, so a test can
// register a queue (QueueStore) and read queue_holders directly. It reaches
// into the unexported field the same way truncateAll and describeClaimState do.
func queueClaimTestDB(t *testing.T, store WorkflowStore) *sql.DB {
	t.Helper()
	switch s := store.(type) {
	case *PostgresStore:
		return s.db
	case *MySQLStore:
		return s.db
	case *MSSQLStore:
		return s.db
	default:
		t.Fatalf("unexpected store type %T", store)
		return nil
	}
}

// countQueueHolders counts live queue_holders rows for a queue. It returns an
// error rather than calling t.Fatalf so the sampler below can run it from a
// non-test goroutine.
func countQueueHolders(ctx context.Context, db *sql.DB, dialect, name string) (int, error) {
	var q string
	switch dialect {
	case "postgres":
		q = `SELECT count(*) FROM queue_holders WHERE queue_name = $1 AND expires_at > now()`
	case "mysql":
		q = `SELECT count(*) FROM queue_holders WHERE queue_name = ? AND expires_at > NOW(6)`
	case "mssql":
		q = `SELECT count(*) FROM queue_holders WHERE queue_name = @p1 AND expires_at > SYSUTCDATETIME()`
	default:
		return 0, fmt.Errorf("unknown dialect %q", dialect)
	}
	var n int
	if err := db.QueryRowContext(ctx, q, name).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// claimQueueConcurrencyKeyStarter is the subset of StartNewRunWithConcurrencyKey
// the tests below assert every backend implements -- a store that cannot record
// a concurrency key on the row is a regression, so the assertion is Fatal, not
// Skip (the same rule the bare-key claim tests use).
type claimQueueConcurrencyKeyStarter interface {
	StartNewRunWithConcurrencyKey(context.Context, string, string, int, json.RawMessage, string, string, int, string) (string, bool, error)
}

func TestARegisteredQueueAdmitsAtMostItsLimit(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			const limit = 3
			name := queueTestName("sem")
			db := queueClaimTestDB(t, store)
			if err := NewQueueStore(db, backend.Name()).CreateQueue(ctx, DefaultTenantUUID, name, limit); err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}

			starter, ok := store.(claimQueueConcurrencyKeyStarter)
			if !ok {
				t.Fatalf("%T cannot record a concurrency key", store)
			}
			for i := 0; i < 10; i++ {
				if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
					fmt.Sprintf("sem-%d", i), "test-workflow", 1, json.RawMessage(`{}`),
					"", DefaultTenantUUID, 0, name); err != nil {
					t.Fatalf("start run %d: %v", i, err)
				}
			}

			claimed, err := store.ClaimWorkflows(ctx, "worker-1", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			if len(claimed) != limit {
				t.Fatalf("claimed %d, want %d (the declared queue limit)", len(claimed), limit)
			}
			if got := mustCountQueueHolders(t, ctx, db, backend.Name(), name); got != limit {
				t.Fatalf("queue_holders = %d after one claim, want %d", got, limit)
			}

			// The deferred runs are deferred, not lost: finishing one holder
			// frees a slot, and the next claim takes exactly one more.
			if err := store.TerminateWorkflow(ctx, claimed[0].ID, "done"); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}
			next, err := store.ClaimWorkflows(ctx, "worker-2", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows after release: %v", err)
			}
			if len(next) != 1 {
				t.Fatalf("after releasing one holder, claimed %d, want 1", len(next))
			}
		})
	}
}

func TestARegisteredQueueLimitHoldsUnderConcurrentClaims(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			// Small limit, so the race window is as wide as the design allows.
			const limit = 2
			name := queueTestName("sem-race")
			db := queueClaimTestDB(t, store)
			if err := NewQueueStore(db, backend.Name()).CreateQueue(ctx, DefaultTenantUUID, name, limit); err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}

			starter, ok := store.(claimQueueConcurrencyKeyStarter)
			if !ok {
				t.Fatalf("%T cannot record a concurrency key", store)
			}
			const total = 16
			for i := 0; i < total; i++ {
				if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
					fmt.Sprintf("sem-race-%d", i), "test-workflow", 1, json.RawMessage(`{}`),
					"", DefaultTenantUUID, 0, name); err != nil {
					t.Fatalf("start run %d: %v", i, err)
				}
			}

			// A sampler reads the live holder count the whole time the claims
			// run. The assertion is the max it saw, not the final tally: the
			// serialisation point exists so the count never exceeds the limit at
			// ANY committed instant, not merely so it settles there.
			var (
				mu        sync.Mutex
				maxHold   int
				sampleErr error
			)
			stop := make(chan struct{})
			samplerDone := make(chan struct{})
			go func() {
				defer close(samplerDone)
				for {
					select {
					case <-stop:
						return
					default:
					}
					n, err := countQueueHolders(ctx, db, backend.Name(), name)
					if err != nil {
						mu.Lock()
						sampleErr = err
						mu.Unlock()
						return
					}
					mu.Lock()
					if n > maxHold {
						maxHold = n
					}
					mu.Unlock()
					time.Sleep(100 * time.Microsecond)
				}
			}()

			const workers = 8
			var (
				wg      sync.WaitGroup
				claimed []string
			)
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					for {
						// A claim limit equal to the queue limit, not the whole
						// population: with a limit large enough to swallow every
						// runnable row, one claimer's SKIP LOCKED candidate scan
						// takes them all and no two claims ever contend on the
						// queue. Splitting the candidate set is what makes the
						// queue-row lock actually decide something.
						got, err := store.ClaimWorkflows(ctx, fmt.Sprintf("worker-%d", id), limit)
						if err != nil {
							mu.Lock()
							sampleErr = err
							mu.Unlock()
							return
						}
						if len(got) == 0 {
							return
						}
						mu.Lock()
						for _, wf := range got {
							claimed = append(claimed, wf.ID)
						}
						mu.Unlock()
					}
				}(w)
			}
			wg.Wait()
			close(stop)
			<-samplerDone

			if sampleErr != nil {
				t.Fatalf("claim or sampler failed: %v", sampleErr)
			}
			if len(claimed) != limit {
				t.Fatalf("claimed %d distinct workflows under concurrency, want %d (the declared queue limit)", len(claimed), limit)
			}
			if maxHold > limit {
				t.Fatalf("sampler observed %d concurrent holders, exceeding the declared limit %d.\n\n"+
					"This is the race the queues-row lock exists to prevent: two concurrent claims "+
					"each read a free slot and both insert. The count must happen under the locked "+
					"queue row, not on the candidate predicate's snapshot.", maxHold, limit)
			}
			if got := mustCountQueueHolders(t, ctx, db, backend.Name(), name); got != limit {
				t.Fatalf("queue_holders = %d after concurrent claims, want %d", got, limit)
			}
		})
	}
}

func mustCountQueueHolders(t *testing.T, ctx context.Context, db *sql.DB, dialect, name string) int {
	t.Helper()
	n, err := countQueueHolders(ctx, db, dialect, name)
	if err != nil {
		t.Fatalf("count queue_holders: %v", err)
	}
	return n
}
