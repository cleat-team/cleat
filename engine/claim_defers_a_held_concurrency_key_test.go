package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// A run whose concurrency key is held by somebody else must WAIT, not fail.
//
// This is the behavioural half of cleat#1186. The issue's complaint was that a
// concurrency limit was enforced at the HTTP boundary by REJECTING the request:
// a limit that throttles how many runs proceed at once had been implemented as
// a limit on how many runs may be accepted at all. Those are different
// products. The fix moves enforcement into the claim, where "not now" is
// expressible and "never" is not.
//
// # Why the columns are written by hand here
//
// StartNewRun does not yet record concurrency_key on the instance -- that is the
// next change. Seeding the column directly is deliberate: it tests the claim
// predicate on its own, so that when the write path lands, a failure in either
// half is attributable to that half. Without this the predicate would ship
// inert, with all eleven copies unexercised.
//
// # Why the hash is copied rather than computed
//
// The three dialects do not agree on where the hash is made. PostgreSQL hashes
// in SQL (`digest($1, 'sha256')` in AcquireConcurrencyKey); SQL Server hashes in
// Go (sha256.Sum256 in mssql_signals_promises.go). A test that computed the
// digest itself would be asserting one dialect's convention against all three,
// and migration 058's comment records the trap it would walk into -- HASHBYTES
// over NVARCHAR hashes UTF-16, so a SQL-side hash on SQL Server does not equal
// the Go-side hash that actually populated the column. Copying key_hash out of
// the row AcquireConcurrencyKey just wrote sidesteps the question entirely and
// is correct on every dialect by construction.
func TestAClaimDefersARunWhoseConcurrencyKeyIsHeld(t *testing.T) {
	for _, backend := range pluginDepsBackends() {
		t.Run(backend.name, func(t *testing.T) {
			db, store := setupPluginDepsDB(t, backend)
			ctx := context.Background()

			seedRunnableDef(t, store, "defer-on-key-def")

			stamp := time.Now().UnixNano()
			holderIdem := fmt.Sprintf("key-holder-%d", stamp)
			waiterIdem := fmt.Sprintf("key-waiter-%d", stamp)

			holder, _, err := store.StartNewRun(ctx, "", "defer-on-key-def", 1,
				json.RawMessage(`{}`), holderIdem, DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun(holder): %v", err)
			}
			waiter, _, err := store.StartNewRun(ctx, "", "defer-on-key-def", 1,
				json.RawMessage(`{}`), waiterIdem, DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun(waiter): %v", err)
			}

			const key = "the-shared-key"
			acquirer, ok := store.(interface {
				AcquireConcurrencyKey(context.Context, string, string, time.Duration) (bool, error)
				ReleaseConcurrencyKey(context.Context, string, string) (bool, error)
			})
			if !ok {
				t.Fatalf("%s: store does not implement the concurrency-key API", backend.name)
			}
			got, err := acquirer.AcquireConcurrencyKey(ctx, key, holder, time.Minute)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey: %v", err)
			}
			if !got {
				t.Fatalf("AcquireConcurrencyKey returned false on a free key -- the test's premise is broken, not the code under test")
			}

			// Point both runs at the key, taking the hash from the row the
			// acquire just wrote. See the comment above for why.
			//
			// The statement is spelled per dialect because the placeholders
			// differ, and on SQL Server it must run on the connection
			// prepareRawAccess pins: the shipped FILTER PREDICATE hides every
			// row from a session with no tenant context, so an unpinned UPDATE
			// matches zero rows and reports success. The RowsAffected check
			// below is what turns that into a failure instead of a silent pass.
			seed := map[string]string{
				"postgres": `UPDATE workflow_instances SET concurrency_key = $1,
				        concurrency_key_hash = (SELECT key_hash FROM concurrency_keys WHERE key_text = $2)
				  WHERE id = $3`,
				"mysql": `UPDATE workflow_instances SET concurrency_key = ?,
				        concurrency_key_hash = (SELECT key_hash FROM (SELECT key_hash FROM concurrency_keys WHERE key_text = ?) k)
				  WHERE id = ?`,
				"mssql": `UPDATE workflow_instances SET concurrency_key = @p1,
				        concurrency_key_hash = (SELECT key_hash FROM concurrency_keys WHERE key_text = @p2)
				  WHERE id = @p3`,
			}[backend.name]
			if seed == "" {
				t.Fatalf("no seeding statement for backend %q -- a dialect was added without one", backend.name)
			}

			type execer interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			}
			var raw execer = db
			if backend.prepareRawAccess != nil {
				conn := backend.prepareRawAccess(t, db)
				defer conn.Close()
				raw = conn
			}

			for _, id := range []string{holder, waiter} {
				res, err := raw.ExecContext(ctx, seed, key, key, id)
				if err != nil {
					t.Fatalf("seed concurrency key on %s: %v", id, err)
				}
				n, err := res.RowsAffected()
				if err != nil {
					t.Fatalf("RowsAffected: %v", err)
				}
				if n != 1 {
					t.Fatalf("seeding %s updated %d rows, want 1 -- the test did not set up "+
						"the state it goes on to assert about", id, n)
				}
			}

			// The holder is claimable: a run must not be blocked by the key it
			// holds itself. The waiter is not.
			claimed, err := store.ClaimWorkflows(ctx, "worker-1", 10)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			ids := map[string]bool{}
			for _, w := range claimed {
				ids[w.ID] = true
			}
			if !ids[holder] {
				t.Errorf("the run HOLDING the key was not claimed.\n\n"+
					"The predicate's `ck.workflow_id <> workflow_instances.id` arm exists so that a "+
					"re-claim after a lost fence, or a claim after release-and-retry, is not blocked "+
					"by the claimant's own key. claimed=%v", keysOf(ids))
			}
			if ids[waiter] {
				t.Errorf("the run whose key is HELD BY ANOTHER was claimed anyway.\n\n"+
					"This is cleat#1186: the run must wait rather than run concurrently. "+
					"If this fails on one dialect only, compare that dialect's copy of the "+
					"predicate -- there are eleven, guarded by "+
					"TestTheClaimableConcurrencyKeyPredicateIsIdenticalAtEverySite. claimed=%v",
					keysOf(ids))
			}

			// And it is deferral, not rejection: once the key is released the
			// same row becomes claimable with nothing else changed. This is the
			// assertion that distinguishes the fix from the bug -- a run that
			// were merely dropped would also be absent above.
			if _, err := acquirer.ReleaseConcurrencyKey(ctx, key, holder); err != nil {
				t.Fatalf("ReleaseConcurrencyKey: %v", err)
			}
			claimed2, err := store.ClaimWorkflows(ctx, "worker-2", 10)
			if err != nil {
				t.Fatalf("ClaimWorkflows after release: %v", err)
			}
			found := false
			for _, w := range claimed2 {
				if w.ID == waiter {
					found = true
				}
			}
			if !found {
				t.Errorf("after the key was released the deferred run was still not claimed.\n\n" +
					"The run was excluded permanently rather than deferred, which is the same " +
					"user-visible outcome as the rejection cleat#1186 asked us to remove.")
			}
		})
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
