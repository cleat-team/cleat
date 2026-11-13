package testutil

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func uniqueLeaseTestID(label string) string {
	return fmt.Sprintf("testutil-lease-%s-%d", label, time.Now().UnixNano())
}

// TestWorkflowLeasesRequiresAMatchingInstanceRow proves workflow_leases' FK onto
// workflow_instances is real, which is what makes SeedRun's three-row layout
// meaningful rather than a write into tables with no constraints.
func TestWorkflowLeasesRequiresAMatchingInstanceRow(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	id := uniqueLeaseTestID("orphan")

	_, err := db.Exec(`INSERT INTO workflow_leases (id) VALUES ($1)`, id)
	if err == nil {
		t.Fatal("inserting a workflow_leases row with no matching workflow_instances row succeeded, want a foreign key violation")
	}
}

// TestSeedRunNeverShowsAHalfWrittenRun is cleat#3733. A run is claimable once
// its workflow_leases row is visible, and the claim reads def_name from
// workflow_payloads through a LEFT JOIN, so a lease with no payload row yet
// fails the claim with "converting NULL to string is unsupported". Seeded
// through a pool the three inserts autocommit separately, so a concurrent
// reader sees exactly that state; the soak test hit it about once in 40,000
// runs and failed the nightly twice.
//
// The observer here asks the question directly -- a lease with no payload row
// -- rather than racing a claimer for it, which is what makes it cheap enough
// to run on every push.
//
// It runs the same seeders twice. The first phase seeds with the three inserts
// autocommitted, and must OBSERVE a half-written run: that is the known-positive
// that stops the second phase passing because the observer cannot see anything.
// If the first phase times out, nothing below it was measured and the test says so.
func TestSeedRunNeverShowsAHalfWrittenRun(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	db.SetMaxOpenConns(16)

	// workflow_payloads_def_fkey needs the definition row. Without it the
	// payload insert FAILS, which leaves a lease with no payload for a reason
	// that has nothing to do with atomicity -- and the control below would
	// "observe" it and pass.
	if _, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points)
		VALUES ('test-def', 1, '\x00', '{}') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seeding workflow_defs(test-def, 1): %v", err)
	}

	orphans := func(prefix string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`
			SELECT count(*) FROM workflow_leases l
			LEFT JOIN workflow_payloads p ON p.id = l.id
			WHERE l.id LIKE $1 AND p.id IS NULL`, prefix+"%").Scan(&n); err != nil {
			t.Fatalf("observer query: %v", err)
		}
		return n
	}

	// phase seeds from several goroutines and polls for a half-written run
	// until stop() says so or the deadline passes. It returns the number of
	// half-written observations, the polls made, and the runs seeded.
	phase := func(label string, seed func(ctx context.Context, id string) error, deadline time.Duration, stopOnFirst bool) (seen, polls, seeded int64) {
		t.Helper()
		prefix := fmt.Sprintf("testutil-halfrun-%s-%d-", label, time.Now().UnixNano())
		t.Cleanup(func() {
			for _, q := range []string{
				`DELETE FROM workflow_payloads WHERE id LIKE $1`,
				`DELETE FROM workflow_leases WHERE id LIKE $1`,
				`DELETE FROM workflow_instances WHERE id LIKE $1`,
			} {
				if _, err := db.Exec(q, prefix+"%"); err != nil {
					t.Errorf("cleanup %q: %v", q, err)
				}
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()

		var seq, nSeen, nPolls atomic.Int64
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ctx.Err() == nil {
					id := fmt.Sprintf("%s%d", prefix, seq.Add(1))
					// Not ctx: the deadline would roll back a transaction mid-commit
					// and report it as "already committed or rolled back", an error
					// about the harness rather than the seeding.
					if err := seed(context.Background(), id); err != nil {
						t.Errorf("seeding %s: %v", id, err)
						cancel()
						return
					}
				}
			}()
		}
		for ctx.Err() == nil {
			nPolls.Add(1)
			if n := orphans(prefix); n > 0 {
				nSeen.Add(int64(n))
				if stopOnFirst {
					cancel()
				}
			}
		}
		wg.Wait()
		return nSeen.Load(), nPolls.Load(), seq.Load()
	}

	// Known-positive: the three inserts on the pool, each autocommitted.
	autocommit := func(ctx context.Context, id string) error {
		return insertRunRows(ctx, db, RunSeed{ID: id, DefName: "test-def"})
	}
	seen, polls, seeded := phase("autocommit", autocommit, 30*time.Second, true)
	if seen == 0 {
		t.Fatalf("PRECONDITION FAILED: autocommitted seeding was never observed half-written in %d polls over %d runs, "+
			"so the observer cannot see the state this test exists to forbid. Nothing below was measured.", polls, seeded)
	}
	t.Logf("control: autocommitted seeding was observed half-written after %d polls, %d runs", polls, seeded)

	// What SeedRun does: three inserts, one transaction.
	atomicSeed := func(ctx context.Context, id string) error {
		return seedRunOn(ctx, db, RunSeed{ID: id, DefName: "test-def"})
	}
	seen, polls, seeded = phase("atomic", atomicSeed, 3*time.Second, false)
	if seeded == 0 || polls == 0 {
		t.Fatalf("PRECONDITION FAILED: the atomic phase seeded %d runs over %d polls. Nothing was measured.", seeded, polls)
	}
	if seen != 0 {
		t.Errorf("SeedRun left a lease with no payload row visible to another session %d time(s) over %d polls and %d runs; "+
			"a concurrent claim would fail with a NULL def_name (cleat#3733)", seen, polls, seeded)
	}
}
