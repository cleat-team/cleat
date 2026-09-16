package engine

// cleat#1671: an idempotency key whose TTL had passed, and whose row the
// sweeper had not yet collected, made the NEXT start fail outright instead of
// starting a new run.
//
// THE DEFECT IS NOT THE EXPIRY FILTER AND NOT THE ON CONFLICT. It is that
// `RowsAffected() == 0` from the key insert has TWO causes and the code assumed
// one:
//
//	a concurrent starter won the race   -- there is a winner to re-read
//	an expired row is still sitting there -- there is not
//
// Both report no rows affected, on all three dialects and through three
// different idioms (ON CONFLICT DO NOTHING, INSERT IGNORE, INSERT ... WHERE NOT
// EXISTS), because none of them says anything about expiry. The re-read that
// follows filters on `expires_at > now()`, so in the second case it looks for a
// row it cannot see and returns sql.ErrNoRows to the caller.
//
// The fix deletes the dead row before the insert, which collapses the two
// causes into one: a conflict now means a LIVE row, and the re-read's own
// filter will find it.
//
// WHY NOT SIMPLY DROP THE FILTER FROM THE RE-READ. It would make the expired
// row visible and hand the caller a workflow id whose key the TTL already
// retired -- silently joining a run that expired, which is worse than an error.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestAnExpiredIdempotencyKeyStartsANewRun(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	defer testutil.CleanupPostgresTestData(t, db)

	ctx := context.Background()
	const tenant = "d0000000-0000-4000-8000-00000000001a"
	stamp := time.Now().UnixNano()
	def := fmt.Sprintf("idem-expired-%d", stamp)
	key := fmt.Sprintf("order-expired-%d", stamp)

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = db.ExecContext(bg, `DELETE FROM idempotency_keys WHERE tenant_id = $1`, tenant)
		_, _ = db.ExecContext(bg, `DELETE FROM workflow_instances WHERE tenant_id = $1`, tenant)
		_, _ = db.ExecContext(bg, `DELETE FROM workflow_defs WHERE tenant_id = $1`, tenant)
	})
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		 VALUES ($1, 1, $2, 1, 0, $3)`,
		def, []byte{0x00, 0x61, 0x73, 0x6d}, tenant); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}

	store := NewPostgresStore(db).WithTenant(tenant)
	keyHash := sha256.Sum256([]byte(key))

	// An EXPIRED row for this key, seeded directly. That is the whole fixture:
	// it is invisible to the lookup (`expires_at > now()`) and still collides
	// with the insert (whose conflict target is the primary key), which is the
	// branch this test exists for. Deterministic, and it needs no race.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id, def_name)
		 VALUES ($1, $2, now() - INTERVAL '1 hour', $3, $4)`,
		keyHash[:], "wf-expired-predecessor", tenant, def); err != nil {
		t.Fatalf("seed the expired key: %v", err)
	}
	var seeded int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM idempotency_keys WHERE key_hash = $1 AND tenant_id = $2`,
		keyHash[:], tenant).Scan(&seeded); err != nil {
		t.Fatalf("count the seeded key: %v", err)
	}
	if seeded != 1 {
		t.Fatalf("PRECONDITION FAILED: %d of 1 expired rows seeded. With nothing to collide "+
			"with, the insert succeeds and this test measures the ordinary path.", seeded)
	}

	id, existed, err := store.StartNewRunWithOptions(
		ctx, "", def, 1, json.RawMessage(`{}`), key, tenant, 0, StartOptions{})
	if err != nil {
		t.Fatalf("StartNewRunWithOptions over an expired key: %v\n\n"+
			"If this is sql.ErrNoRows, the insert conflicted with the expired row, the "+
			"caller was sent down the concurrent-insert path, and the re-read looked for a "+
			"live row that does not exist. cleat#1671.", err)
	}
	if existed {
		t.Fatalf("the start reported already_started (%s) for a key whose TTL had passed. An "+
			"expired key is a fresh key; reporting the predecessor is the stale-id outcome "+
			"that dropping the re-read's expiry filter would have produced.", id)
	}
	if id == "wf-expired-predecessor" {
		t.Fatalf("the start returned the EXPIRED row's workflow id")
	}

	// And the new run really is the one the key now points at.
	var pointsAt string
	var live bool
	if err := db.QueryRowContext(ctx,
		`SELECT workflow_id, expires_at > now() FROM idempotency_keys
		 WHERE key_hash = $1 AND tenant_id = $2`, keyHash[:], tenant).Scan(&pointsAt, &live); err != nil {
		t.Fatalf("read the key back: %v", err)
	}
	if pointsAt != id || !live {
		t.Errorf("after the start the key points at %q (live=%v), want %q and live -- the "+
			"expired row was removed but not replaced", pointsAt, live, id)
	}
}

// TestConcurrentStartsOnOneKeyStillYieldOneRun is the OTHER arm, and it is the
// one worth writing even though it is the boring half.
//
// The fix deletes a row before the insert. A delete that is not scoped by
// expiry would remove a LIVE row a concurrent starter had just written, and two
// callers would each get their own run for one key -- the exact defect
// migration 010 exists to prevent, reintroduced by the fix for cleat#1671. The
// expired-row test above passes just as happily either way, because it has no
// competitor.
//
// So: real contention, and an assertion that survives any interleaving --
// exactly one caller may be told it started the run, and every caller must be
// given the same id.
func TestConcurrentStartsOnOneKeyStillYieldOneRun(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	defer testutil.CleanupPostgresTestData(t, db)

	ctx := context.Background()
	const tenant = "d0000000-0000-4000-8000-00000000001b"
	stamp := time.Now().UnixNano()
	def := fmt.Sprintf("idem-race-%d", stamp)
	key := fmt.Sprintf("order-race-%d", stamp)

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = db.ExecContext(bg, `DELETE FROM idempotency_keys WHERE tenant_id = $1`, tenant)
		_, _ = db.ExecContext(bg, `DELETE FROM workflow_instances WHERE tenant_id = $1`, tenant)
		_, _ = db.ExecContext(bg, `DELETE FROM workflow_defs WHERE tenant_id = $1`, tenant)
	})
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		 VALUES ($1, 1, $2, 1, 0, $3)`,
		def, []byte{0x00, 0x61, 0x73, 0x6d}, tenant); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}

	const starters = 8
	type outcome struct {
		id      string
		existed bool
		err     error
	}
	results := make([]outcome, starters)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < starters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			store := NewPostgresStore(db).WithTenant(tenant)
			id, existed, err := store.StartNewRunWithOptions(
				ctx, "", def, 1, json.RawMessage(`{}`), key, tenant, 0, StartOptions{})
			results[i] = outcome{id, existed, err}
		}(i)
	}
	close(start)
	wg.Wait()

	fresh, ids := 0, map[string]int{}
	for i, r := range results {
		if r.err != nil {
			t.Errorf("starter %d: %v", i, r.err)
			continue
		}
		if !r.existed {
			fresh++
		}
		ids[r.id]++
	}
	if t.Failed() {
		t.FailNow()
	}
	if fresh != 1 {
		t.Errorf("%d of %d starters were told they started the run, want exactly 1. More than "+
			"one means the expired-row delete removed a LIVE row and two callers each got "+
			"their own run for one key", fresh, starters)
	}
	if len(ids) != 1 {
		t.Errorf("the %d starters were given %d distinct workflow ids (%v), want 1",
			starters, len(ids), ids)
	}

	// The database has to agree: one key, one row, and one workflow.
	var keys, runs int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1`, tenant).Scan(&keys); err != nil {
		t.Fatalf("count keys: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM workflow_instances WHERE tenant_id = $1`, tenant).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if keys != 1 || runs != 1 {
		t.Errorf("after %d concurrent starts on one key the database holds %d key rows and %d "+
			"workflow rows, want 1 and 1", starters, keys, runs)
	}
}

// TestEveryDialectClearsAnExpiredIdempotencyKeyBeforeInserting asserts the fix
// exists in all three stores, and BEFORE the insert in each.
//
// WHY A SOURCE ASSERTION RATHER THAN THREE EXECUTED CASES. The same as
// TestEveryDialectRefusesAnIdempotencyKeyReusedForAnotherDefinition, which is
// this file's neighbour and the precedent: the behaviour is written out once
// per dialect through three different idioms -- ON CONFLICT DO NOTHING, INSERT
// IGNORE, INSERT ... WHERE NOT EXISTS -- and executing it needs all three
// databases. What actually goes wrong is ONE STORE BEING EDITED AND THE OTHERS
// NOT, which a single-dialect run cannot see at all. cleat#1256 is that failure
// with this very table: the sweeper ran on PostgreSQL only, so a key was
// honoured forever on the other two for the life of the deployment.
//
// ORDER IS ASSERTED, NOT JUST PRESENCE. A delete placed after the insert
// removes the row the insert just wrote on the happy path, and leaves the
// defect untouched on the unhappy one. It would satisfy a contains-check.
//
// THE EXPIRY PREDICATE IS ASSERTED TOO, and it is the half that matters most:
// a delete scoped by the key alone fixes the expired case and breaks the
// concurrent one, letting two callers each start a run for one key.
// TestConcurrentStartsOnOneKeyStillYieldOneRun catches that on PostgreSQL; this
// is what catches it on the other two.
func TestEveryDialectClearsAnExpiredIdempotencyKeyBeforeInserting(t *testing.T) {
	for _, c := range []struct{ file, nowExpr, insert string }{
		{"store_lifecycle.go", "now()", "INSERT INTO idempotency_keys"},
		{"mysql_lifecycle.go", "NOW(6)", "INSERT IGNORE INTO idempotency_keys"},
		{"mssql_lifecycle.go", "SYSUTCDATETIME()", "INSERT INTO idempotency_keys"},
	} {
		t.Run(c.file, func(t *testing.T) {
			src := stripGoComments(readSourceForTest(t, c.file))

			del := strings.Index(src, "DELETE FROM idempotency_keys")
			if del < 0 {
				t.Fatalf("%s never deletes an expired idempotency key, so an unswept row "+
					"still blocks the insert and the caller gets sql.ErrNoRows instead of a "+
					"new run (cleat#1671)", c.file)
			}
			ins := strings.Index(src, c.insert)
			if ins < 0 {
				t.Fatalf("%s no longer contains %q -- this test is matching on a form the "+
					"file has stopped using, so it asserts nothing", c.file, c.insert)
			}
			if del > ins {
				t.Errorf("%s deletes the expired key AFTER inserting (delete at %d, insert at "+
					"%d). That removes the row the insert just wrote and leaves cleat#1671 "+
					"in place", c.file, del, ins)
			}

			// The delete statement itself, not merely somewhere in the file.
			stmt := src[del:]
			if end := strings.Index(stmt, "`"); end > 0 {
				stmt = stmt[:end]
			}
			if !strings.Contains(stmt, "expires_at <= "+c.nowExpr) {
				t.Errorf("%s's delete is not scoped by expiry (%q). Scoped by the key alone it "+
					"removes a LIVE row a concurrent starter just wrote, and two callers each "+
					"get their own run for one key -- the defect migration 010 exists to "+
					"prevent", c.file, strings.TrimSpace(stmt))
			}
		})
	}
}
