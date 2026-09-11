package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// A run started with a concurrency key carries that key on its own row, written
// by the INSERT that created it.
//
// cleat#1186. The claim predicate added in #1214 reads
// workflow_instances.concurrency_key_hash; until something writes it the column
// is NULL on every production row and the predicate defers nothing. This is the
// write side.
//
// # Why the INSERT and not a second statement
//
// The row is created 'ready' with next_wake_at already in the past, so a poller
// can claim it the instant it commits. A run claimed BEFORE its key was
// recorded is precisely the case the predicate exists to prevent, so a
// two-statement version would leave a window whose width is the scheduler's
// poll interval. There is no window here because there is no second statement.
// The test cannot observe that window directly -- it asserts the postcondition
// and the comment carries the reason.
//
// # The hash is compared against one computed in Go, on every dialect
//
// PostgreSQL hashes in SQL (digest($7,'sha256')) and the other two hash in Go.
// Asserting all three against the same Go-side sha256 is what makes that split
// checkable rather than assumed: if PostgreSQL's digest ever disagreed with
// Go's, the key would be written but never match a lookup, and nothing else
// would say so.
func TestAStartWithAConcurrencyKeyRecordsItOnTheRow(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			starter, ok := store.(interface {
				StartNewRunWithConcurrencyKey(context.Context, string, string, int, json.RawMessage, string, string, int, string) (string, bool, error)
			})
			if !ok {
				t.Fatalf("%T cannot record a concurrency key", store)
			}

			const key = "nightly-sync"
			want := sha256.Sum256([]byte(key))

			id := fmt.Sprintf("ck-start-%d", time.Now().UnixNano())
			got, existed, err := starter.StartNewRunWithConcurrencyKey(ctx, id, "test-workflow", 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0, key)
			if err != nil {
				t.Fatalf("StartNewRunWithConcurrencyKey: %v", err)
			}
			if existed {
				t.Fatalf("a fresh run reported already-existing")
			}

			text, hash := readConcurrencyKeyColumns(t, store, got)
			if text != key {
				t.Errorf("concurrency_key = %q, want %q.\n\nWithout it the claim predicate has "+
					"nothing to match and the run is never deferred -- cleat#1186's whole subject.", text, key)
			}
			if string(hash) != string(want[:]) {
				t.Errorf("concurrency_key_hash = %x, want %x.\n\nA hash that disagrees with Go's "+
					"sha256 is worse than a missing one: the key is written, the lookup never "+
					"matches, and nothing errors. Migration 058 records this happening on SQL "+
					"Server via HASHBYTES over NVARCHAR.", hash, want[:])
			}
		})

		t.Run(backend.Name()+"/no key leaves both columns NULL", func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			// The ordinary path, which is the overwhelming majority of runs.
			// NULL rather than '' matters: the predicate's fast arm is
			// `concurrency_key_hash IS NULL`, and an empty string is not NULL.
			id := fmt.Sprintf("ck-none-%d", time.Now().UnixNano())
			got, _, err := store.StartNewRun(ctx, id, "test-workflow", 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			text, hash := readConcurrencyKeyColumns(t, store, got)
			if text != "" || hash != nil {
				t.Errorf("a run started with no key has concurrency_key=%q hash=%x, want both NULL.\n\n"+
					"An empty string is not NULL, so the predicate's `IS NULL` fast arm would stop "+
					"matching and every ordinary run would take the subquery.", text, hash)
			}
		})
	}
}

// readConcurrencyKeyColumns reads the two columns straight from the row, rather
// than through GetWorkflowByID.
//
// WorkflowInstance does not carry them, and adding fields to it to satisfy a
// test would put them on every JSON response the API serves -- a visible API
// change made for an invisible reason. The columns are what this test is about,
// so it reads the columns.
func readConcurrencyKeyColumns(t *testing.T, store WorkflowStore, id string) (string, []byte) {
	t.Helper()
	db := rawDBOf(t, store)
	q := map[Dialect]string{
		DialectPostgres: `SELECT concurrency_key, concurrency_key_hash FROM workflow_instances WHERE id = $1`,
		DialectMySQL:    `SELECT concurrency_key, concurrency_key_hash FROM workflow_instances WHERE id = ?`,
		DialectMSSQL:    `SELECT concurrency_key, concurrency_key_hash FROM workflow_instances WHERE id = @p1`,
	}[dialectOf(t, store)]
	if q == "" {
		t.Fatalf("no query for %T", store)
	}
	var text sql.NullString
	var hash []byte
	if err := db.QueryRowContext(context.Background(), q, id).Scan(&text, &hash); err != nil {
		t.Fatalf("reading the concurrency key columns of %s: %v", id, err)
	}
	return text.String, hash
}
