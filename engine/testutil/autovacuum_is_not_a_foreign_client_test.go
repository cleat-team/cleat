package testutil

import (
	"database/sql"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestABackgroundWorkerIsNotAForeignClient pins the backend_type predicate that
// cleat#1469's gate needed and shipped without.
//
// PostgreSQL's own background workers are rows in pg_stat_activity like anything
// else. An autovacuum WORKER running inside a database carries that database's
// datname, an empty application_name and a NULL client_addr, so it satisfied
// every predicate the probe had and was reported as a stranger -- failing
// unrelated PRs at whichever suite happened to arrive while it ran. The launcher
// does not, because its datname is NULL, which is why this survived review: the
// false positive exists only while a worker is actually running.
//
// AUTOVACUUM CANNOT BE SUMMONED ON DEMAND, so this stages a PARALLEL WORKER,
// which is the same thing in the way that matters: backend_type is not
// 'client backend' and datname IS this database.
//
// THE ASSERTION IS A COUNT UNDER ONE application_name, not an empty list. The
// leader connection driving the parallel query is a real foreign client backend
// and SHOULD be reported -- it is another connection this process did not tag.
// Its workers inherit its application_name, so without the backend_type
// predicate they are reported too, and the count under that name goes from 1 to
// 1+N. That difference is the whole test.
func TestABackgroundWorkerIsNotAForeignClient(t *testing.T) {
	if db := TestDB(t, DialectPostgres); db == nil {
		t.Fatal("no PostgreSQL test database")
	}

	const leaderApp = "cleat-test-parallel-leader"

	dsn := os.Getenv("CLEAT_TEST_POSTGRES")
	if dsn == "" {
		dsn = os.Getenv("CLEAT_TEST_DB")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing the test DSN: %v", err)
	}
	q := u.Query()
	q.Set("application_name", leaderApp)
	u.RawQuery = q.Encode()

	leader, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatalf("opening the leader connection: %v", err)
	}
	defer leader.Close()
	leader.SetMaxOpenConns(1) // exactly one leader, so the expected count is exactly 1

	if _, err := leader.Exec(`CREATE TABLE IF NOT EXISTS zz_parallel_probe AS
		SELECT g AS i FROM generate_series(1, 200000) g`); err != nil {
		t.Fatalf("seeding the parallel table: %v", err)
	}
	defer func() { _, _ = leader.Exec(`DROP TABLE IF EXISTS zz_parallel_probe`) }()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			conn, err := leader.Conn(t.Context())
			if err != nil {
				return
			}
			for _, s := range []string{
				"SET max_parallel_workers_per_gather = 2",
				"SET parallel_setup_cost = 0",
				"SET parallel_tuple_cost = 0",
				"SET min_parallel_table_scan_size = 0",
			} {
				if _, err := conn.ExecContext(t.Context(), s); err != nil {
					conn.Close()
					return
				}
			}
			var n int64
			_ = conn.QueryRowContext(t.Context(), `SELECT count(*) FROM zz_parallel_probe`).Scan(&n)
			conn.Close()
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	// Wait until a parallel worker is actually visible in this database. Until
	// one is, the predicate has nothing to exclude and a pass means nothing.
	probe, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("opening the observer connection: %v", err)
	}
	defer probe.Close()

	var seen bool
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && !seen {
		var n int
		if err := probe.QueryRow(`SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND backend_type = 'parallel worker'`).Scan(&n); err == nil && n > 0 {
			seen = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !seen {
		t.Fatal("no parallel worker became visible within 30s, so this test cannot tell a " +
			"working predicate from a missing one. Deliberately fatal rather than skipped: " +
			"a green run here would report that background workers are filtered without " +
			"ever having seen one.")
	}

	// THE ASSERTION. Exactly one session should be reported under the leader's
	// application_name -- the leader itself. Its parallel workers inherit that
	// name, so if backend_type is not filtered they are counted too.
	foreign, basis, ok := ForeignSessions(DialectPostgres)
	if !ok {
		t.Fatalf("the probe could not answer at all: %s", basis)
	}
	var under int
	for _, f := range foreign {
		if strings.Contains(f, leaderApp) {
			under++
		}
	}
	if under != 1 {
		t.Errorf("the probe reported %d sessions under application_name=%q, want exactly 1 "+
			"(the leader connection).\n\nreported:\n  %s\n\n"+
			"More than one means its PARALLEL WORKERS were counted as clients. A worker "+
			"backend is not another test run and cannot delete a fixture; the same hole "+
			"reports an autovacuum worker as a stranger and fails whichever suite arrives "+
			"while it runs. cleat#1469.",
			under, leaderApp, strings.Join(foreign, "\n  "))
	}
}
