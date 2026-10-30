package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/lib/pq"
)

// The package's tests share databases that persist between runs
// (testutil.SuiteTestDB), so a run a test leaves `ready` is claimable by the
// next test that reads "the next ready run", and by every later invocation
// (cleat#3603, cleat#3614). A full run left 19, from 11 tests in 8 files.
//
// Two halves, because the first alone does not hold: settleRunsLeftBehind is
// what a test that deliberately leaves a run behind (a worker shut down
// mid-run hands its run back to `ready`; a start that is only accepted) calls
// to clean up after itself, and runGuarded below fails the package when any run
// was left, so the next leaker is named by its ids rather than found by the
// test it breaks.

// suiteDatabasePattern matches every database SuiteTestDB hands this package:
// the shared "cleat_worker" one and the per-test "cleat_worker_<what>" ones.
const suiteDatabasePattern = `cleat\_test\_cleat\_worker%`

// leftoverStatuses are the states of a run no worker has finished with. A run
// in either is one a later claim can take.
var leftoverStatuses = []string{"ready", "terminating"}

// settleRunsLeftBehind registers a cleanup that terminates every run db holds
// in a leftover state that was not there when it was called. Call it right
// after SuiteTestDB, in a test that cannot avoid leaving a run behind.
//
// It names runs by what appeared during the test, not by the ids the test
// knows, because the runs worth settling include ones the code under test
// made in another tenant.
func settleRunsLeftBehind(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	before, err := leftoverRunIDs(ctx, db)
	if err != nil {
		t.Fatalf("listing the runs already ready: %v", err)
	}
	t.Cleanup(func() {
		ids := newer(before, mustLeftover(t, db))
		if len(ids) == 0 {
			return
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE workflow_leases SET status = 'terminated'
			WHERE id = ANY($1) AND status = ANY($2)`, pq.Array(ids), pq.Array(leftoverStatuses)); err != nil {
			t.Errorf("settling %d run(s) the test left behind (%s): %v", len(ids), strings.Join(ids, ", "), err)
		}
	})
}

func mustLeftover(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	got, err := leftoverRunIDs(context.Background(), db)
	if err != nil {
		t.Errorf("listing the runs left behind: %v", err)
	}
	return got
}

// leftoverRunIDs is every run in a leftover state, in any tenant: the suite
// connection owns the tables, so row-level security does not hide a tenant's.
func leftoverRunIDs(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM workflow_leases WHERE status = ANY($1)`, pq.Array(leftoverStatuses))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// newer is the ids in now that are not in before, sorted.
func newer(before, now map[string]bool) []string {
	var ids []string
	for id := range now {
		if !before[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// leftoverRunsByDatabase lists, for every suite database, the runs in a
// leftover state, as "<database>/<run id>". It reports an error rather than an
// empty list when it cannot look.
func leftoverRunsByDatabase(ctx context.Context, base string) (map[string]bool, error) {
	admin, err := sql.Open("postgres", base)
	if err != nil {
		return nil, err
	}
	defer admin.Close()
	rows, err := admin.QueryContext(ctx, `SELECT datname FROM pg_database WHERE datname LIKE $1 ORDER BY 1`, suiteDatabasePattern)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := map[string]bool{}
	for _, name := range names {
		u, err := url.Parse(base)
		if err != nil {
			return nil, err
		}
		u.Path = "/" + name
		db, err := sql.Open("postgres", u.String())
		if err != nil {
			return nil, err
		}
		var has sql.NullString
		err = db.QueryRowContext(ctx, `SELECT to_regclass('workflow_leases')::text`).Scan(&has)
		if err == nil && has.Valid {
			var ids map[string]bool
			if ids, err = leftoverRunIDs(ctx, db); err == nil {
				for id := range ids {
					out[name+"/"+id] = true
				}
			}
		}
		db.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	return out, nil
}

// runGuarded is m.Run with the ready-run guard around it, for the package's
// TestMain (a_stale_route_signature_refuses_to_boot_test.go, which owns it
// because it must run before test flag parsing in its subprocess mode). It
// fails the package when it leaves a run ready. It compares the leftover runs
// before and after, so a run an earlier invocation left (the databases persist)
// is not charged to this one.
//
// It does not fail when it cannot look: with no PostgreSQL configured every
// database test skips itself, and a guard that failed there would fail a
// machine that has nothing to measure. It says UNMEASURED on stderr instead,
// so a green run that measured nothing reads as one.
func runGuarded(m *testing.M) int {
	flag.Parse()
	if f := flag.Lookup("test.list"); testing.Short() || (f != nil && f.Value.String() != "") {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	base := testutil.PostgresTestDSN()
	before, err := leftoverRunsByDatabase(ctx, base)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ready-run guard: UNMEASURED, no PostgreSQL to look at (%v)\n", err)
		return m.Run()
	}

	code := m.Run()

	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	after, err := leftoverRunsByDatabase(ctx, base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ready-run guard: UNMEASURED after the run (%v)\n", err)
		return code
	}
	if left := newer(before, after); len(left) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: this package left %d run(s) ready or terminating in the shared suite databases:\n  %s\n"+
			"A run left there is claimable by the next test that reads \"the next ready run\" (cleat#3614).\n"+
			"A test that cannot avoid leaving one calls settleRunsLeftBehind(t, db) right after SuiteTestDB.\n",
			len(left), strings.Join(left, "\n  "))
		if code == 0 {
			code = 1
		}
	}
	return code
}
