package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
)

// cleat#1671 on every dialect, behaviourally.
//
// WHY THIS EXISTS NOW. #1671's own test is PostgreSQL only -- it takes
// testutil.TestDB(t, testutil.DialectPostgres) directly -- so MySQL and SQL
// Server were covered for this property by ONE THING: a source-text guard
// asserting that the DELETE appears before the INSERT in the file.
//
// That guard is a proxy. It pins the shape of the fix rather than the property,
// and cleat#1753 needed the shape changed: MySQL's unconditional
// delete-before-insert gap-locks the range a non-matching DELETE scans, which
// deadlocked every concurrent starter. The sweep now runs inside the conflict
// branch instead, where it matches a row and takes a record lock.
//
// Relaxing the proxy without replacing it would have left the property with no
// check at all on two dialects. This is the replacement, and it is strictly
// stronger: it runs the actual statements against the actual engines.
//
// THE PROPERTY. An idempotency key whose TTL has passed, whose row the sweeper
// has not yet collected, must start a NEW run -- not fail, and not replay the
// expired predecessor. The two ways it goes wrong are different errors:
//
//	sql.ErrNoRows     the insert conflicted with the dead row and the
//	                  re-read, which filters on expiry, could not see it
//	alreadyExisted    the dead row was treated as a live winner, so the
//	                  caller silently joined a run whose key had retired
func TestAnExpiredIdempotencyKeyStartsANewRunOnEveryDialect(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			defName := "idem-expired-" + backend.Name()
			seedWorkflowDef(t, ctx, store, defName)

			const key = "order-expired-every-dialect"
			keyHash := sha256.Sum256([]byte(key))

			seedExpiredIdempotencyKey(t, ctx, store, keyHash[:], defName, "wf-expired-predecessor")

			// PRECONDITION. With nothing to collide with, the insert simply
			// succeeds and this test measures the ordinary path.
			if n := countIdempotencyRows(t, ctx, store, keyHash[:]); n != 1 {
				t.Fatalf("UNMEASURED: %d of 1 expired rows seeded, so the insert has nothing "+
					"to conflict with and the expired branch is never reached", n)
			}

			id, existed, err := store.StartNewRun(ctx, "", defName, 1,
				json.RawMessage(`{}`), key, DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun over an expired key: %v\n\n"+
					"sql.ErrNoRows here means the insert conflicted with the dead row and the "+
					"re-read, which filters on expiry, looked for a row it cannot see. "+
					"cleat#1671.", err)
			}
			if existed {
				t.Fatalf("the start reported already_started (%s) for a key whose TTL had "+
					"passed: the caller silently joined a run whose key had retired", id)
			}
			if id == "wf-expired-predecessor" {
				t.Fatalf("the start returned the EXPIRED predecessor's id, so the dead row was " +
					"read as a live winner")
			}
		})
	}
}

// seedExpiredIdempotencyKey writes a row whose TTL has already passed.
//
// An hour in the past, not a second: a short offset races the clock skew
// between the test process and the database, and a fixture that is sometimes
// live is a fixture that sometimes measures the ordinary path while reporting
// the same green.
func seedExpiredIdempotencyKey(t *testing.T, ctx context.Context, store WorkflowStore, keyHash []byte, defName, wfID string) {
	t.Helper()
	db := rawDBOf(t, store)
	var q string
	switch store.(type) {
	case *PostgresStore:
		q = `INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id, def_name)
		     VALUES ($1, $2, now() - INTERVAL '1 hour', $3, $4)`
	case *MySQLStore:
		q = `INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id, def_name)
		     VALUES (?, ?, DATE_SUB(NOW(6), INTERVAL 1 HOUR), ?, ?)`
	default:
		q = `INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id, def_name)
		     VALUES (@p1, @p2, DATEADD(HOUR, -1, SYSUTCDATETIME()), @p3, @p4)`
	}
	if _, err := db.ExecContext(ctx, q, keyHash, wfID, DefaultTenantUUID, defName); err != nil {
		t.Fatalf("seed the expired idempotency key: %v", err)
	}
}

func countIdempotencyRows(t *testing.T, ctx context.Context, store WorkflowStore, keyHash []byte) int {
	t.Helper()
	db := rawDBOf(t, store)
	q := `SELECT count(*) FROM idempotency_keys WHERE key_hash = ?`
	switch store.(type) {
	case *PostgresStore:
		q = `SELECT count(*) FROM idempotency_keys WHERE key_hash = $1`
	case *MSSQLStore:
		q = `SELECT count(*) FROM idempotency_keys WHERE key_hash = @p1`
	}
	var n int
	if err := db.QueryRowContext(ctx, q, keyHash).Scan(&n); err != nil {
		t.Fatalf("count idempotency rows: %v", err)
	}
	return n
}
