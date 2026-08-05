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
// tests read the stored expiry back and do arithmetic on it, which is
// deterministic, and check exclusion by acquiring twice, which needs no clock
// at all.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// readConcurrencyKeyExpiry returns how far in the future the stored expiry is,
// measured against the database's own clock so the assertion does not depend on
// the test host's.
func readConcurrencyKeyExpiry(t *testing.T, backend StoreBackend, key string) time.Duration {
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

	db := adminDBFor(t, backend)
	var secondsRemaining float64
	if err := db.QueryRow(q, key).Scan(&secondsRemaining); err != nil {
		t.Fatalf("read concurrency key expiry: %v", err)
	}
	return time.Duration(secondsRemaining * float64(time.Second))
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
					t.Cleanup(func() { _ = store.ReleaseConcurrencyKey(ctx, key) })

					remaining := readConcurrencyKeyExpiry(t, backend, key)
					if remaining <= 0 {
						t.Fatalf("a %s lock was stored already expired (%s remaining): "+
							"the next caller takes it, and two workflows hold the same key",
							ttl, remaining)
					}
					// Generous lower bound, exact upper bound: the round trip
					// costs real time, so the remainder is slightly less than
					// the TTL. It must never exceed it.
					if remaining > ttl {
						t.Errorf("a %s lock expires in %s, which is longer than asked for", ttl, remaining)
					}
					if remaining < ttl/2 {
						t.Errorf("a %s lock expires in %s -- the TTL was truncated, not merely "+
							"reduced by the round trip", ttl, remaining)
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
