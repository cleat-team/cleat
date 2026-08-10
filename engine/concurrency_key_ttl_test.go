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
// the expensive way. The read-back arithmetic still compared the remaining TTL
// against a hardcoded fraction of it (ttl/2), which is a bound on how fast the
// runner is -- so `Test SQL Server` went red on develop on 2026-08-07 with a
// correct implementation and a slow round trip. The lower bound is now derived
// from the elapsed time rather than guessed; see the comment on it below.
//
// And that elapsed time is read from the database's clock, not the host's, for
// the same reason the expiry is: bracketing with time.Now() was tried first and
// was still wrong by 1-2 ms on SQL Server. Every instant this file reasons about
// now comes from one clock, which is the property the code under test is
// supposed to have.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// readConcurrencyKeyExpiry returns how far in the future the stored expiry is,
// measured against the database's own clock so the assertion does not depend on
// the test host's.
// Takes the admin handle rather than opening one, because each of the three
// adminDBFor branches opens a fresh *sql.DB and this is called in a loop:
// opening per call ran PostgreSQL out of connections ("sorry, too many clients
// already") under `go test -count=20`.
func readConcurrencyKeyExpiry(t *testing.T, db *sql.DB, backend StoreBackend, key string) time.Duration {
	t.Helper()
	// Looked up by key_text, not by re-deriving the hash in SQL: HASHBYTES over
	// an NVARCHAR parameter hashes UTF-16 and never matches the UTF-8 digest Go
	// stored, which is a way to write a test that reports "no rows" and looks
	// like a product failure.
	var q string
	switch backend.Name() {
	case "postgres":
		q = `SELECT EXTRACT(EPOCH FROM (expires_at - now())) FROM concurrency_keys
		     WHERE key_text = $1`
	case "mysql":
		q = `SELECT TIMESTAMPDIFF(MICROSECOND, NOW(6), expires_at) / 1000000
		     FROM concurrency_keys WHERE key_text = ?`
	case "mssql":
		q = `SELECT DATEDIFF_BIG(MICROSECOND, SYSUTCDATETIME(), expires_at) / 1000000.0
		     FROM concurrency_keys WHERE key_text = @p1`
	default:
		t.Fatalf("readConcurrencyKeyExpiry: unknown backend %q", backend.Name())
	}

	var secondsRemaining float64
	if err := db.QueryRow(q, key).Scan(&secondsRemaining); err != nil {
		t.Fatalf("read concurrency key expiry: %v", err)
	}
	return time.Duration(secondsRemaining * float64(time.Second))
}

// dbNowMicros reads the database's own clock as whole microseconds since the
// Unix epoch.
//
// Only ever used to subtract one reading from another, and only readings from
// the same database -- so the epoch, the column type and the session time zone
// all cancel and none of them has to be got right.
//
// An integer rather than a time.Time, because scanning a timestamp column into
// time.Time works on all three drivers only if the MySQL DSN carries
// parseTime=true -- which the CI DSNs and testutil's default set, but a
// developer's own CLEAT_TEST_MYSQL need not. A test that fails with
// "unsupported Scan" on someone's laptop teaches them nothing about locks.
//
// An integer rather than float64 seconds, because the difference of two of
// these is compared against a duration a few milliseconds long, and the epoch
// is about 1.78e15 microseconds. In float64 seconds that value quantises to
// roughly a quarter of a microsecond, and subtracting two of them cancels away
// the significant digits: the SQL Server subtests failed by 17-100 ns against a
// correct implementation, which is numerical noise wearing a bug's clothing.
// Microseconds as int64 stay exact until well past the year 250000.
func dbNowMicros(t *testing.T, db *sql.DB, backend StoreBackend) int64 {
	t.Helper()
	var q string
	switch backend.Name() {
	case "postgres":
		// clock_timestamp(), not now(): now() is the *transaction* start time in
		// PostgreSQL, so two readings inside one transaction would be identical
		// and the elapsed time would measure as zero -- which would make the
		// bound below stricter than reality rather than looser, i.e. flaky in
		// the direction that fails a correct implementation.
		q = `SELECT (EXTRACT(EPOCH FROM clock_timestamp()) * 1000000)::bigint`
	case "mysql":
		q = `SELECT TIMESTAMPDIFF(MICROSECOND, '1970-01-01 00:00:00', NOW(6))`
	case "mssql":
		q = `SELECT DATEDIFF_BIG(MICROSECOND, '1970-01-01', SYSUTCDATETIME())`
	default:
		t.Fatalf("dbNowMicros: unknown backend %q", backend.Name())
	}

	var micros int64
	if err := db.QueryRow(q).Scan(&micros); err != nil {
		t.Fatalf("read database clock: %v", err)
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
			// below. See readConcurrencyKeyExpiry.
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

					// Bracket the acquire and the read-back with two readings of
					// the database's clock. Their difference is an upper bound on
					// the database-side elapsed time between the stored expiry
					// being computed and being read -- which is exactly the
					// quantity that eats into `remaining`. See the lower-bound
					// assertion below.
					before := dbNowMicros(t, adminDB, backend)
					acquired, err := store.AcquireConcurrencyKey(ctx, key, runID, ttl)
					if err != nil {
						t.Fatalf("AcquireConcurrencyKey: %v", err)
					}
					if !acquired {
						t.Fatal("AcquireConcurrencyKey returned false for a fresh key")
					}
					t.Cleanup(func() { _ = store.ReleaseConcurrencyKey(ctx, key) })

					remaining := readConcurrencyKeyExpiry(t, adminDB, backend, key)
					dbElapsed := time.Duration(dbNowMicros(t, adminDB, backend)-before) * time.Microsecond

					if remaining <= 0 {
						t.Fatalf("a %s lock was stored already expired (%s remaining): "+
							"the next caller takes it, and two workflows hold the same key",
							ttl, remaining)
					}
					// Exact upper bound: the round trip costs real time, so the
					// remainder is slightly less than the TTL. It must never
					// exceed it.
					if remaining > ttl {
						t.Errorf("a %s lock expires in %s, which is longer than asked for", ttl, remaining)
					}
					// Exact lower bound, and it is exact rather than generous on
					// purpose. This was `remaining < ttl/2`, which asserts that
					// the read-back finished within 250 ms of the acquire for the
					// 500 ms case -- a property of the runner, not of the code. It
					// failed on `Test SQL Server` on develop on 2026-08-07
					// (run 31145314648, "a 500ms lock expires in 202.46ms") with
					// nothing wrong: the round trip took ~298 ms on a loaded
					// runner, and the stored TTL was correct.
					//
					// `ttl - dbElapsed` needs no slack and no tuning. expires_at
					// is db_now_at_acquire + ttl and `remaining` is measured
					// against db_now_at_read, so remaining == ttl - (database
					// elapsed between those two instants), and `dbElapsed`
					// brackets that from outside. A correct implementation cannot
					// violate it however slow the machine is; a truncating one
					// violates it by the whole truncated remainder.
					//
					// Measured on the database's clock and not the host's, which
					// is not a detail: bracketing with time.Now() instead failed
					// here by 1-2 ms on SQL Server, because the container's clock
					// and the host's do not advance at quite the same rate over a
					// 10 ms window. That is small, but it is unbounded in the
					// wrong direction -- it is drift, so it grows with whatever
					// the machine is doing -- and this file exists because of a
					// bug about exactly which clock owns an expiry.
					//
					// Both sub-second cases are caught by `remaining <= 0` above
					// regardless, since truncation to whole seconds sends them to
					// zero -- which is the defect this file was written for.
					if remaining < ttl-dbElapsed {
						t.Errorf("a %s lock expires in %s, and only %s of database time elapsed "+
							"between storing it and reading it back -- the TTL was truncated, not "+
							"merely reduced by the time in flight", ttl, remaining, dbElapsed)
					}
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
			t.Cleanup(func() { _ = store.ReleaseConcurrencyKey(ctx, key) })

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
