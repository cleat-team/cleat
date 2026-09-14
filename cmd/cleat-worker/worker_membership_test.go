package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// TestTheShareFollowsTheLiveWorkerCount exercises the LOOP BODY against a real
// registry, not the policy type in isolation.
//
// connection_share_test.go already pins the rule with a fake clock and no
// database. What that cannot show is whether the loop feeds it the right
// number -- whether the count it observes is the live one, whether a sweep
// runs, whether the share reaches the pools. Those are the joins where this
// would silently do nothing, and cleat#1470's EvictIdle is the standing
// example of a correct function that no caller reached for months.
func TestTheShareFollowsTheLiveWorkerCount(t *testing.T) {
	db := testutil.SuiteTestDB(t, "cleat_worker")
	ctx := context.Background()
	reg := &engine.WorkerRegistry{DB: db, Dialect: engine.DialectPostgres}

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	const budget = 200
	const hold = 2 * time.Minute

	me := fmt.Sprintf("share-me-%d", time.Now().UnixNano())
	other := fmt.Sprintf("share-other-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = reg.Deregister(context.Background(), me)
		_ = reg.Deregister(context.Background(), other)
	})

	w := &Worker{
		id:              me,
		ctx:             ctx,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:         newTestPrometheus(),
		workerRegistry:  reg,
		connectionShare: newConnectionShare(budget, hold, clk.now),
		// A small fixed census so the arithmetic below is legible: a share of
		// 200 leaves 190 for tenant pools, a share of 100 leaves 90.
		connectionBudgetParts: connectionBudget{Core: 10},
	}

	if err := w.registerInWorkerRegistry(ctx); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Alone (as far as this worker's own registration goes, plus whatever else
	// the suite database holds). The assertions below are all RELATIVE, because
	// admin.workers is cluster-global and this test cannot assume it is the
	// only registrant -- a fixed expected share would measure the suite rather
	// than the rule.
	w.membershipTick(hold)
	alone := w.connectionShare.Current()
	if alone < 1 {
		t.Fatalf("share after the first tick is %d, want at least the floor of 1", alone)
	}

	// A second worker joins. The share must shrink on this very tick.
	if err := reg.Register(ctx, engine.WorkerRegistration{WorkerID: other, Hostname: "elsewhere"}); err != nil {
		t.Fatalf("register other: %v", err)
	}
	w.membershipTick(hold)
	joined := w.connectionShare.Current()
	if joined >= alone {
		t.Fatalf("a worker joined and the share went %d -> %d; it must shrink immediately, "+
			"because being slow to shrink is what overcommits the database", alone, joined)
	}

	// It leaves. The share must NOT grow yet.
	if err := reg.Deregister(ctx, other); err != nil {
		t.Fatalf("deregister other: %v", err)
	}
	w.membershipTick(hold)
	if got := w.connectionShare.Current(); got != joined {
		t.Errorf("the share grew to %d the instant the count dropped, want it held at %d -- "+
			"a crashed worker stops heartbeating long before its connections are released",
			got, joined)
	}
	if pending, _ := w.connectionShare.Pending(); pending <= joined {
		t.Errorf("no larger share is pending after a worker left (pending=%d, current=%d)",
			pending, joined)
	}

	// Past the hold-down, it may grow.
	clk.advance(hold + time.Second)
	w.membershipTick(hold)
	if got := w.connectionShare.Current(); got <= joined {
		t.Errorf("the share is %d after the hold-down elapsed, want more than %d", got, joined)
	}

	// THE HEARTBEAT, and it has to be made to matter.
	//
	// The obvious assertion -- "after several ticks this worker is still in its
	// own live set" -- CANNOT FAIL, and that is measured rather than suspected:
	// with the Heartbeat call removed the test stayed green. Register put the
	// row there and the test runs in a fraction of the window, so the row is
	// live either way. It was asserting Register, not Heartbeat.
	//
	// Back-dating this worker's own row past the window makes the heartbeat
	// load-bearing: with it removed, the worker's own sweep deletes it and it
	// never comes back.
	//
	// What this does NOT pin, having tried: the order of heartbeat and sweep.
	// Reversing them leaves the test green, because a swept worker's next
	// heartbeat returns ErrWorkerNotRegistered and membershipTick re-registers.
	// That is a real robustness property rather than an accident, so it is
	// recorded here instead of being asserted as an ordering the code does not
	// actually depend on.
	if _, err := db.ExecContext(ctx,
		`UPDATE admin.workers SET last_heartbeat_at = now() - interval '1 hour'
		 WHERE worker_id = $1`, me); err != nil {
		t.Fatalf("back-date self: %v", err)
	}
	w.membershipTick(hold)

	live, err := reg.ListLive(ctx, hold)
	if err != nil {
		t.Fatalf("ListLive: %v", err)
	}
	found := false
	for _, x := range live {
		if x.WorkerID == me {
			found = true
		}
	}
	if !found {
		t.Errorf("after back-dating its own row an hour and ticking, this worker is not " +
			"live. Either the heartbeat is not reaching the registry, or the tick sweeps " +
			"before it heartbeats and the worker swept itself")
	}
}

// The wiring guard. The test above proves membershipTick works; this proves
// something calls it, which is the half that silently rots.
func TestWorkerMembershipIsWiredAtStartup(t *testing.T) {
	strip := func(path string) string {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var out []string
		for _, l := range strings.Split(string(src), "\n") {
			if i := strings.Index(l, "//"); i >= 0 {
				l = l[:i]
			}
			out = append(out, l)
		}
		return strings.Join(out, "\n")
	}

	for _, c := range []struct{ file, want, why string }{
		{"main.go", "*clusterConnectionBudgetFlag", "the flag is read rather than a constant"},
		{"main.go", "workerRegistry.Register(", "the worker registers BEFORE it counts"},
		{"main.go", "newConnectionShare(", "the share is constructed"},
		{"setup.go", `w.launchLoop("worker_membership"`, "the loop actually runs"},
		{"worker_membership.go", "SetConnectionBudget(", "the share reaches the pools"},
		{"worker_membership.go", "Deregister(", "shutdown releases the share"},
		{"worker_membership.go", "SweepExpired(", "expired workers stop counting"},
	} {
		if !strings.Contains(strip(c.file), c.want) {
			t.Errorf("%s does not contain %q -- %s.\n\n"+
				"A registry nothing writes to reports an empty cluster, and an empty "+
				"cluster divides the budget by one. cleat#1470 already shipped one "+
				"complete function with no caller (EvictIdle); this is the guard for "+
				"the same failure in cleat#1487.", c.file, c.want, c.why)
		}
	}
}
