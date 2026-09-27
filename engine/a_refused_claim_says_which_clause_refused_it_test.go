package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// ClaimWorkflow returns (nil, nil) when its predicate matches nothing. That is
// correct -- "the row is there and did not match" is not an error -- and it
// makes a failing claim assertion say nothing at all:
//
//	same_claim_exclusion_test.go:161: the released workflow was not
//	re-claimable: wf=<nil> err=<nil>
//
// Five clauses can exclude a row and the message names none of them.
// cleat#1391 is an intermittent, order-sensitive failure of exactly this
// shape: it does not reproduce on demand, so the highest-value change is that
// the NEXT occurrence -- in CI, or in somebody's suite run -- arrives with the
// row already dumped rather than needing to be reproduced first.
//
// This does not fix #1391 and is not claimed to. It makes the failure
// informative.
//
// WHY IT DUMPS EVERY ROW AND THE DATABASE'S OWN now(). The two hypotheses that
// need distinguishing are residue (a row from an earlier test that the
// predicate rejects) and clock (`next_wake_at <= now()` compared against the
// DATABASE's clock while the test wrote the value from the HOST's). One shows
// as unexpected rows, the other as a next_wake_at in the future relative to
// the now() printed beside it. Neither is visible without both.
func describeUnclaimableRows(t *testing.T, store WorkflowStore) string {
	t.Helper()

	var db *sql.DB
	var q, nowQ, keysQ string
	switch s := store.(type) {
	case *PostgresStore:
		db = s.db
		q = `SELECT id, status, COALESCE(assigned_to,'<null>'), COALESCE(task_queue,'<null>'),
		            COALESCE(next_wake_at::text,'<null>'), COALESCE(concurrency_key_hash,'<null>'), generation
		     FROM workflow_instances ORDER BY id`
		nowQ = `SELECT now()::text`
		keysQ = `SELECT key_hash, workflow_id, expires_at::text FROM concurrency_keys ORDER BY key_hash`
	case *MySQLStore:
		db = s.db
		q = `SELECT id, status, COALESCE(assigned_to,'<null>'), COALESCE(task_queue,'<null>'),
		            COALESCE(CAST(next_wake_at AS CHAR),'<null>'), COALESCE(concurrency_key_hash,'<null>'), generation
		     FROM workflow_instances ORDER BY id`
		nowQ = `SELECT CAST(NOW(6) AS CHAR)`
		keysQ = `SELECT key_hash, workflow_id, CAST(expires_at AS CHAR) FROM concurrency_keys ORDER BY key_hash`
	case *MSSQLStore:
		db = s.db
		q = `SELECT id, status, COALESCE(assigned_to,'<null>'), COALESCE(task_queue,'<null>'),
		            COALESCE(CONVERT(NVARCHAR(64), next_wake_at, 126),'<null>'),
		            COALESCE(concurrency_key_hash,'<null>'), generation
		     FROM workflow_instances ORDER BY id`
		nowQ = `SELECT CONVERT(NVARCHAR(64), SYSUTCDATETIME(), 126)`
		keysQ = `SELECT key_hash, workflow_id, CONVERT(NVARCHAR(64), expires_at, 126) FROM concurrency_keys ORDER BY key_hash`
	default:
		return fmt.Sprintf("\n(no row dump: unrecognised store %T)", store)
	}

	var b strings.Builder
	var dbNow string
	if err := db.QueryRow(nowQ).Scan(&dbNow); err != nil {
		fmt.Fprintf(&b, "\n  database now(): <query failed: %v>", err)
	} else {
		// The database's clock, not the host's. next_wake_at is compared
		// against this one, and the test writes it from the other.
		fmt.Fprintf(&b, "\n  database now(): %s", dbNow)
	}

	rows, err := db.Query(q)
	if err != nil {
		fmt.Fprintf(&b, "\n  workflow_instances: <query failed: %v>", err)
		return b.String()
	}
	defer rows.Close()
	n := 0
	fmt.Fprintf(&b, "\n  workflow_instances (every clause ClaimWorkflow tests):")
	for rows.Next() {
		var id, status, assigned, queue, wake, ckHash string
		var generation int64
		if err := rows.Scan(&id, &status, &assigned, &queue, &wake, &ckHash, &generation); err != nil {
			fmt.Fprintf(&b, "\n    <scan failed: %v>", err)
			break
		}
		n++
		fmt.Fprintf(&b, "\n    id=%s status=%s assigned_to=%s task_queue=%s next_wake_at=%s ck_hash=%s gen=%d",
			id, status, assigned, queue, wake, ckHash, generation)
	}
	if n == 0 {
		// Its own case, because it means something different: the claim found
		// nothing because there is nothing, so the row was never written or
		// was deleted -- not because a clause rejected it.
		fmt.Fprintf(&b, "\n    <none -- the table is empty, so no clause rejected anything>")
	}

	keyRows, err := db.Query(keysQ)
	if err == nil {
		defer keyRows.Close()
		k := 0
		for keyRows.Next() {
			var hash, wfID, expires string
			if err := keyRows.Scan(&hash, &wfID, &expires); err != nil {
				break
			}
			if k == 0 {
				fmt.Fprintf(&b, "\n  concurrency_keys (a live one held by ANOTHER workflow excludes the row):")
			}
			k++
			fmt.Fprintf(&b, "\n    key_hash=%s workflow_id=%s expires_at=%s", hash, wfID, expires)
		}
	}
	return b.String()
}
