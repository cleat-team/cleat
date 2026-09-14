package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// An internal test rather than engine_test, so that back-dating a heartbeat can
// reuse Dialect.intervalExpr. The alternative is sleeping past an expiry
// boundary -- a test that asserts on wall-clock time, which this repository has
// been bitten by often enough to have a rule about it (cleat#1504).
// Back-dating is exact and instant.
func TestWorkersAreEnumerable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tdialect testutil.Dialect
		edialect Dialect
	}{
		{"postgres", testutil.DialectPostgres, DialectPostgres},
		{"mysql", testutil.DialectMySQL, DialectMySQL},
		{"mssql", testutil.DialectMSSQL, DialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.TestDB(t, tc.tdialect)
			ctx := context.Background()
			r := &WorkerRegistry{DB: db, Dialect: tc.edialect}

			// Unique per run: these tests share a database with everything
			// else in the package, and admin.workers is cluster-global with no
			// tenant to scope a fixture by. Every assertion below therefore
			// counts THESE workers rather than all rows -- a bare count(*)
			// would be measuring whatever else is registered.
			idA := fmt.Sprintf("test-a-%d-%s", time.Now().UnixNano(), tc.name)
			idB := fmt.Sprintf("test-b-%d-%s", time.Now().UnixNano(), tc.name)
			t.Cleanup(func() {
				_ = r.Deregister(context.Background(), idA)
				_ = r.Deregister(context.Background(), idB)
			})

			const maxAge = 30 * time.Second
			mine := func(t *testing.T) int {
				t.Helper()
				live, err := r.ListLive(ctx, maxAge)
				if err != nil {
					t.Fatalf("ListLive: %v", err)
				}
				n := 0
				for _, w := range live {
					if w.WorkerID == idA || w.WorkerID == idB {
						n++
					}
				}
				return n
			}

			for _, reg := range []WorkerRegistration{
				{WorkerID: idA, Hostname: "host-a", PID: 111, Concurrency: 10, ConnectionBudget: 100},
				{WorkerID: idB, Hostname: "host-b", PID: 222, Concurrency: 2, ConnectionBudget: 100},
			} {
				if err := r.Register(ctx, reg); err != nil {
					t.Fatalf("Register %s: %v", reg.WorkerID, err)
				}
			}
			if got := mine(t); got != 2 {
				t.Fatalf("after registering two workers, ListLive sees %d of them, want 2", got)
			}

			// The whole point of the table: a worker that holds no workflow is
			// still visible. Neither of these has claimed anything.
			var a WorkerRegistration
			live, err := r.ListLive(ctx, maxAge)
			if err != nil {
				t.Fatalf("ListLive: %v", err)
			}
			for _, w := range live {
				if w.WorkerID == idA {
					a = w
				}
			}
			if a.Hostname != "host-a" || a.PID != 111 || a.Concurrency != 10 || a.ConnectionBudget != 100 {
				t.Errorf("worker A read back as %+v; the diagnostic columns did not survive the round trip", a)
			}
			if a.StartedAt.IsZero() || a.LastHeartbeatAt.IsZero() {
				t.Errorf("worker A has a zero timestamp: started=%v heartbeat=%v", a.StartedAt, a.LastHeartbeatAt)
			}

			if err := r.Heartbeat(ctx, idA); err != nil {
				t.Fatalf("Heartbeat A: %v", err)
			}
			if got := mine(t); got != 2 {
				t.Fatalf("after a heartbeat, ListLive sees %d, want 2", got)
			}

			// Back-date B past the window, using the DATABASE's clock, which is
			// the same clock the heartbeat was written with.
			// The interval is placeholder 1 and worker_id is 2, matching the
			// order they appear in the text. That is not cosmetic: MySQL's `?`
			// binds by APPEARANCE while $N and @pN bind by number, so a
			// statement whose numbering disagrees with its text order binds
			// correctly on two dialects and silently swaps the arguments on
			// the third.
			backdate := `UPDATE ` + r.table() + ` SET last_heartbeat_at = ` +
				tc.edialect.intervalExpr(1) + ` WHERE worker_id = ` + tc.edialect.placeholder(2)
			if _, err := db.ExecContext(ctx, backdate, int(maxAge/time.Second)+60, idB); err != nil {
				t.Fatalf("back-date B: %v", err)
			}

			if got := mine(t); got != 1 {
				t.Fatalf("after back-dating B past the window, ListLive sees %d of mine, want 1", got)
			}
			// CountLive is what the budget will divide by, so it is asserted
			// separately rather than assumed to agree with ListLive.
			countAll, err := r.CountLive(ctx, maxAge)
			if err != nil {
				t.Fatalf("CountLive: %v", err)
			}
			countLong, err := r.CountLive(ctx, maxAge+120*time.Second)
			if err != nil {
				t.Fatalf("CountLive(long): %v", err)
			}
			if countLong <= countAll {
				t.Errorf("a longer window counted %d and the short one %d; "+
					"widening the window must admit the back-dated worker", countLong, countAll)
			}

			n, err := r.SweepExpired(ctx, maxAge)
			if err != nil {
				t.Fatalf("SweepExpired: %v", err)
			}
			if n < 1 {
				t.Errorf("SweepExpired removed %d rows, want at least the one back-dated worker", n)
			}

			// An UPDATE that matches nothing succeeds and reports no error, so
			// without this the caller cannot tell a renewed lease from a lease
			// that is gone.
			if err := r.Heartbeat(ctx, idB); !errors.Is(err, ErrWorkerNotRegistered) {
				t.Errorf("Heartbeat on a swept worker returned %v, want ErrWorkerNotRegistered", err)
			}
			if err := r.Heartbeat(ctx, idA); err != nil {
				t.Errorf("Heartbeat on a live worker returned %v, want nil", err)
			}

			if err := r.Deregister(ctx, idA); err != nil {
				t.Fatalf("Deregister A: %v", err)
			}
			if got := mine(t); got != 0 {
				t.Errorf("after deregistering A and sweeping B, ListLive still sees %d of mine, want 0", got)
			}
		})
	}
}
