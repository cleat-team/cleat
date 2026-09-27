package engine

// IMPROVEMENT-PLAN 3.34, handed over by WS-3: a sub-second TTL silently became
// zero on PostgreSQL and SQL Server, so a 500 ms lock was born expired and the
// next caller took it -- two workflows holding the same mutual-exclusion key,
// with nothing logged. MySQL kept the precision but computed the expiry on the
// application's clock and then compared it against the database's.
//
// The decision this encodes, since WS-3 left it open: a TTL means exactly what
// the caller passed, and the database's clock owns it.
//
//   - Exactly what the caller passed, because the guest API is specified in
//     milliseconds -- engine/locking.go passes
//     time.Duration(ttlMs)*time.Millisecond -- so truncating to whole seconds
//     contradicts the contract callers are written against. There is no
//     rounding here, up or down.
//   - The database's clock, because every predicate that reads expires_at
//     compares it against the database clock (`expires_at < now()`,
//     `> SYSUTCDATETIME()`, `<= NOW(6)`). Computing the value on one clock and
//     testing it against another is the skew bug MySQL had, and workers on
//     different hosts have to agree on whether a lock is held.
//
// NO SLEEPS. The intermittent that led WS-3 here was
// TestAcquireConcurrencyKey_Expired, which acquires with a 1 ns TTL, sleeps
// 10 ms and hopes -- asserting per-backend behaviour through a race. These
// tests read the stored expiry back and do arithmetic on it, and check
// exclusion by acquiring twice, which needs no clock at all.
//
// "No sleeps" was not sufficient for determinism, and this file learned that
// the expensive way, twice.
//
// The first version compared the remaining TTL against a hardcoded fraction of
// it (ttl/2) -- a bound on how fast the runner is -- so `Test SQL Server` went
// red on develop on 2026-08-07 with a correct implementation and a slow round
// trip. The fix (2026-09-10) replaced the guessed fraction with an elapsed-time
// budget read from the database's own clock, bracketing the acquire-then-read
// round trip with two extra queries.
//
// SUPERSEDED 2026-09-23 (cleat#2024): a budget is still a wall-clock dependency,
// only a wider one, and it was still not wide enough -- `mssql/500ms` failed
// the same way a third time (2026-08-07, 2026-09-10, 2026-09-23), each time on
// a loaded CI runner rather than a broken implementation. CLAUDE.md: "if an
// assertion depends on wall-clock time, remove the timing rather than widening
// it." So this file stopped measuring elapsed real time entirely.
//
// concurrency_keys carries both acquired_at and expires_at, and every
// production INSERT computes both from the SAME database-side clock reading
// within one statement -- `now() + make_interval(...)` on Postgres (both
// columns read the transaction-stable now()), `NOW(6) + INTERVAL ...` on
// MySQL (both read the statement-stable NOW(6)). So `expires_at - acquired_at`
// is exactly the TTL the caller asked for, independent of how long the INSERT
// took to run or how long it sat in a connection queue first -- there is no
// round trip inside that quantity at all.
//
// SQL Server (cleat#2119) used to rely on the same "runtime constant within
// one statement" property applied across TWO separate call sites --
// acquired_at via the column's own DEFAULT SYSUTCDATETIME(), expires_at via
// an inline SYSUTCDATETIME() in the INSERT...SELECT. Verifying that with 40
// concurrent inserts against a loaded container gave 40-for-40 exact matches
// at the time, and extensive later stress testing (hundreds of concurrent
// acquires, a CPU-throttled container, a concurrent noise writer) still could
// not force the two to disagree -- but a live CI run did, once, by ~4.07ms on
// a 30s TTL, outside this test's own tolerance. Whatever the exact trigger
// (never reproduced locally), relying on a DEFAULT constraint and an inline
// expression seeing the same instant is relying on two separate evaluations
// of a nondeterministic function agreeing, which is the class of thing this
// file's own history says to stop measuring rather than widen the tolerance
// around. AcquireConcurrencyKey now computes SYSUTCDATETIME() into one local
// @now and uses it explicitly for both columns, so there is exactly one clock
// read in the statement and `expires_at - acquired_at` is a pure arithmetic
// identity -- not dependent on same-statement constancy holding across a
// DEFAULT constraint boundary, however reliable that usually is.
//
// That is what the tests below read, in one query, instead of bracketing an
// acquire-then-readback round trip with the database's clock and hoping the
// window was wide enough.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// readConcurrencyKeyTTLMicros returns expires_at - acquired_at, in whole
// microseconds, computed entirely inside the database by a single query --
// cleat#2024. No wall clock, host or database, is read anywhere in this file
// any more: both columns were stamped from the same database-side "now"
// inside the INSERT that created them (see the file header), so their
// difference is exactly the TTL AcquireConcurrencyKey was asked for, whatever
// the round trip to get here cost.
//
// Integer microseconds, not float64 seconds, for the same reason the
// superseded dbNowMicros used to: TIMESTAMPDIFF/DATEDIFF_BIG return exact
// integers, and dividing by 1e6 to get seconds first would reintroduce the
// float rounding this file has already been bitten by once.
//
// Takes the admin handle rather than opening one, because each of the three
// adminDBFor branches opens a fresh *sql.DB and this is called in a loop:
// opening per call ran PostgreSQL out of connections ("sorry, too many clients
// already") under `go test -count=20`.
func readConcurrencyKeyTTLMicros(t *testing.T, db *sql.DB, backend StoreBackend, key string) int64 {
	t.Helper()
	// Looked up by key_text, not by re-deriving the hash in SQL: HASHBYTES over
	// an NVARCHAR parameter hashes UTF-16 and never matches the UTF-8 digest Go
	// stored, which is a way to write a test that reports "no rows" and looks
	// like a product failure.
	var q string
	switch backend.Name() {
	case "postgres":
		q = `SELECT (EXTRACT(EPOCH FROM (expires_at - acquired_at)) * 1000000)::bigint
		     FROM concurrency_keys WHERE key_text = $1`
	case "mysql":
		q = `SELECT TIMESTAMPDIFF(MICROSECOND, acquired_at, expires_at)
		     FROM concurrency_keys WHERE key_text = ?`
	case "mssql":
		q = `SELECT DATEDIFF_BIG(MICROSECOND, acquired_at, expires_at)
		     FROM concurrency_keys WHERE key_text = @p1`
	default:
		t.Fatalf("readConcurrencyKeyTTLMicros: unknown backend %q", backend.Name())
	}

	var micros int64
	if err := db.QueryRow(q, key).Scan(&micros); err != nil {
		t.Fatalf("read concurrency key acquired/expires diff: %v", err)
	}
	return micros
}

// TestConcurrencyKeyTTLKeepsSubSecondPrecision is the defect, on every dialect.
func TestConcurrencyKeyTTLKeepsSubSecondPrecision(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			runID := seedWorkflowForLock(t, store)

			// One admin handle for the whole dialect, reused by every subtest
			// below. See readConcurrencyKeyTTLMicros.
			adminDB := adminDBFor(t, backend)

			for _, ttl := range []time.Duration{
				500 * time.Millisecond,
				999 * time.Millisecond,
				1500 * time.Millisecond,
				30 * time.Second,
			} {
				ttl := ttl
				t.Run(ttl.String(), func(t *testing.T) {
					key := fmt.Sprintf("ttl-%s-%d", ttl, time.Now().UnixNano())

					acquired, err := store.AcquireConcurrencyKey(ctx, key, runID, ttl)
					if err != nil {
						t.Fatalf("AcquireConcurrencyKey: %v", err)
					}
					if !acquired {
						t.Fatal("AcquireConcurrencyKey returned false for a fresh key")
					}
					t.Cleanup(func() { _, _ = store.ReleaseConcurrencyKey(ctx, key, runID) })

					// stored: what the row actually says the TTL was, read in one
					// query with no application-side clock involved. wantMicros is
					// the caller's request in the same units.
					//
					// toleranceMicros is not runner-speed slack -- there is no
					// round trip left inside this quantity for a slow runner to
					// eat into. It exists only for cross-dialect timestamp
					// precision: PostgreSQL and MySQL store microseconds exactly,
					// but DATETIMEOFFSET's native precision is 100 ns and this
					// file has already been burned once (see dbNowMicros' old
					// comment, superseded above) by assuming a rounding difference
					// this small was a bug. 1 ms is generous against that and
					// still two and a half orders of magnitude tighter than the
					// truncation defect this test exists to catch -- see below.
					stored := readConcurrencyKeyTTLMicros(t, adminDB, backend, key)
					wantMicros := ttl.Microseconds()
					const toleranceMicros = 1000
					diff := stored - wantMicros
					if diff < 0 {
						diff = -diff
					}
					if diff > toleranceMicros {
						t.Errorf("stored TTL (expires_at - acquired_at) = %dus, want %dus "+
							"(+/- %dus): the database itself disagrees with the caller about "+
							"how long this lock should be held, independent of anything this "+
							"test host or the round trip to reach it did",
							stored, wantMicros, toleranceMicros)
					}

					// The truncation defect this file exists for (IMPROVEMENT-PLAN
					// 3.34) sends a sub-second TTL's fractional part to zero: a
					// 500ms request would store a 0-second diff, off by 500,000us
					// -- five hundred times toleranceMicros above. There is no
					// speed at which a truncating implementation can pass this
					// assertion, because nothing about it depends on speed.
				})
			}
		})
	}
}

// TestConcurrencyKeyExcludesWhileHeld is the property the TTL exists to serve,
// asserted without reference to any clock: while a key is held, nobody else
// gets it.
func TestConcurrencyKeyExcludesWhileHeld(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			runID := seedWorkflowForLock(t, store)

			// A second workflow, because that is the question mutual exclusion
			// answers. Contending with *itself* asks a different one -- whether
			// the primitive is re-entrant -- and the three dialects disagree
			// about that too (IMPROVEMENT-PLAN 3.35), so asking it here would
			// have made this test about the wrong divergence.
			otherRunID := seedWorkflowForLock(t, store)

			// 500 ms: the value that used to be stored as zero.
			key := fmt.Sprintf("exclusion-%d", time.Now().UnixNano())
			acquired, err := store.AcquireConcurrencyKey(ctx, key, runID, 500*time.Millisecond)
			if err != nil {
				t.Fatalf("first AcquireConcurrencyKey: %v", err)
			}
			if !acquired {
				t.Fatal("first AcquireConcurrencyKey returned false for a fresh key")
			}
			t.Cleanup(func() { _, _ = store.ReleaseConcurrencyKey(ctx, key, runID) })

			again, err := store.AcquireConcurrencyKey(ctx, key, otherRunID, 500*time.Millisecond)
			if err != nil {
				t.Fatalf("second AcquireConcurrencyKey: %v", err)
			}
			if again {
				t.Error("a second workflow acquired the key while the first held it: " +
					"the 500ms TTL was stored as already expired, so this is not " +
					"mutual exclusion")
			}
		})
	}
}

// seedWorkflowForLock creates a workflow for the concurrency key's foreign key
// to point at.
func seedWorkflowForLock(t *testing.T, store WorkflowStore) string {
	t.Helper()
	ctx := context.Background()
	const defName = "concurrency-key-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	runID, _, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`),
		fmt.Sprintf("lock-%d", time.Now().UnixNano()), DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	return runID
}
