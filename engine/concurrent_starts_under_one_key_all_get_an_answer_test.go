package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// cleat#1753. Eight concurrent starts sharing one idempotency key must each
// receive an answer: one creator, seven replays, and NO errors.
//
// WHAT THE PORTED SUITE SAW. dbos-transact-py's
// test_concurrent_starts_under_one_key_elect_exactly_one_creator, on MySQL
// only, against develop:
//
//	not every racer was answered 201: [201, 201, 201, 500, 201, 201, 201, 201]
//
// The election itself was correct -- one creator, no double execution. What
// failed was the REPORT: since cleat#1169 a replay repeats the winner's
// response verbatim, so a loser receiving a server error is a real divergence
// rather than a cosmetic one.
//
// WHY EIGHT RACERS AND SEVERAL ROUNDS. The window is a few milliseconds wide,
// so a single race is a coin toss. MEASURED against the defect itself, by
// disabling the retry in mysql_lifecycle.go and running the mysql arm ten
// times:
//
//	one round of 8 racers    4 failures in 10 runs   (~40% detection)
//	rounds below, same test  10 failures in 10 runs
//
// A regression test that catches its own defect 40% of the time lets a
// reintroduction through most of the time, which is worse than useless: it is
// a green that reads as coverage. Each round is an independent race under a
// fresh key, so the per-run miss probability multiplies down.
//
// It still FAILS strongly and PASSES weakly -- absence of a race is not
// evidence -- which is why the fix it guards is argued from the code path and
// not from this test going green.
//
// SO THE ASSERTION IS ON THE ERRORS, NOT ON THE COUNTS. "Exactly one creator"
// is already covered elsewhere and holds on every dialect; it is asserted here
// only to prove the racers really did contend. The property this test exists
// for is that NO racer gets an error, because the error is the divergence.
func TestConcurrentStartsUnderOneKeyAllGetAnAnswer(t *testing.T) {
	const (
		racers = 8
		// Six rounds takes the per-run miss probability from ~60% to under 5%.
		rounds = 6
	)

	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			defName := "concurrent-starts-" + backend.Name()
			seedWorkflowDef(t, ctx, store, defName)

			for round := 0; round < rounds; round++ {
				raceOneKey(t, ctx, store, defName, round, racers)
			}
		})
	}
}

// raceOneKey releases `racers` concurrent starts against one fresh key and
// asserts every one of them was answered.
//
// A FRESH KEY PER ROUND, so the rounds are independent races rather than one
// race followed by cheap replays -- after the first round a reused key has a
// committed winner and every later start takes the uncontended lookup, which
// never reaches the branch under test.
func raceOneKey(t *testing.T, ctx context.Context, store WorkflowStore, defName string, round, racers int) {
	t.Helper()

	// The run ids carry the round too. They did not at first, and round 2's
	// winner then collided with round 1's committed row on
	// workflow_instances_pkey -- a fixture defect that reads exactly like the
	// product defect this test is for, since both surface as "a racer was
	// refused". Postgres caught it, which is the dialect that has nothing
	// wrong with it.
	key := fmt.Sprintf("one-key-many-racers-%d", round)
	input := json.RawMessage(`{"n":1}`)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ids     []string
		replays int
		errs    []error
		start   = make(chan struct{})
	)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release together, so the window is actually contended
			id, replayed, err := store.StartNewRun(ctx,
				fmt.Sprintf("wf-racer-%d-%d", round, i), defName, 1, input, key,
				DefaultTenantUUID, 0)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ids = append(ids, id)
			if replayed {
				replays++
			}
		}(i)
	}
	close(start)
	wg.Wait()

	t.Logf("answered=%d replays=%d errors=%d", len(ids), replays, len(errs))

	// The precondition. If the racers did not actually contend -- all
	// eight serialised, say -- then seven replays would not appear and
	// a clean error count would mean nothing.
	if len(errs) == 0 && replays != racers-1 {
		t.Fatalf("UNMEASURED: %d replay(s) among %d racers, want %d. The starts did "+
			"not contend, so a zero error count says nothing about the race.",
			replays, racers, racers-1)
	}

	if len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("a racer was refused: %v", err)
			// Name the mechanism when it is the known one, so the next
			// reader does not have to rediscover which lock it was.
			if isDeadlockOrLockWait(err) {
				t.Logf("  ^ this is the lock conflict from cleat#1753: concurrent " +
					"DELETE-then-INSERT on one key, where InnoDB gap-locks the " +
					"gap a non-matching DELETE scans")
			}
		}
		t.Fatalf("%d of %d racers got an error; all of them must get an answer, "+
			"because a replay repeats the winner's response (cleat#1169)",
			len(errs), racers)
	}

	// All the same run, which is the election itself.
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("round %d: racers were given different run ids (%q and %q): one key "+
				"elected more than one creator", round, ids[0], id)
		}
	}
}

// isDeadlockOrLockWait reports the two InnoDB conditions that concurrent
// DELETE-then-INSERT on one key produces, so a failure names its mechanism
// rather than leaving a bare driver error.
//
// String matching, deliberately: the driver's typed error is dialect-specific
// and this test runs on three. It is used only to LABEL a failure that has
// already been decided, never to decide one -- so a miss costs a less helpful
// message and nothing else.
func isDeadlockOrLockWait(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "deadlock") ||
		strings.Contains(s, "lock wait timeout") ||
		strings.Contains(s, "1213") ||
		strings.Contains(s, "1205")
}

// seedWorkflowDef inserts the row StartNewRun's task-queue lookup reads.
//
// Written per dialect rather than through a rebind helper, matching
// a_miscased_name_matches_on_two_of_three_dialects_test.go: the placeholder
// syntax is the only difference and spelling it out keeps the three visible
// next to each other.
func seedWorkflowDef(t *testing.T, ctx context.Context, store WorkflowStore, defName string) {
	t.Helper()
	db := rawDBOf(t, store)
	var q string
	switch store.(type) {
	case *PostgresStore:
		q = `INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		     VALUES ($1, 1, $2, 1, 0, $3)`
	case *MySQLStore:
		q = `INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		     VALUES (?, 1, ?, 1, 0, ?)`
	default:
		q = `INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		     VALUES (@p1, 1, @p2, 1, 0, @p3)`
	}
	if _, err := db.ExecContext(ctx, q, defName,
		[]byte{0x00, 0x61, 0x73, 0x6d}, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_defs for %s: %v", defName, err)
	}
}
