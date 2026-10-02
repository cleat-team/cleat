package main

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// fakeStaleHolderReaper answers ListStaleHolders and reclaims from a fixed set
// of rows, each carrying an AGE, applying whatever timeout it is called with.
// That is the whole point: it lets a test drive cleat#2196's two-window split by
// asking what happens to a row at 1.5 windows old versus one at 2.5, which no
// real store can be asked without waiting.
type fakeStaleHolderReaper struct {
	rows []agedHolder

	// listTimeouts records every window ListStaleHolders was called with, in
	// order, so a test can assert the second read really is the doubled one
	// rather than a second copy of the first.
	listTimeouts []time.Duration

	// lastExclude is the exclude the reclaim was actually handed.
	lastExclude []engine.GenerationKey
	reclaimed   int
}

type agedHolder struct {
	key    engine.GenerationKey
	holder string
	age    time.Duration
}

func (f *fakeStaleHolderReaper) ListStaleHolders(_ context.Context, timeout time.Duration, limit int) ([]engine.StaleHold, error) {
	f.listTimeouts = append(f.listTimeouts, timeout)
	var out []engine.StaleHold
	for _, r := range f.rows {
		if r.age >= timeout {
			out = append(out, engine.StaleHold{Key: r.key, AssignedTo: r.holder})
		}
	}
	// Oldest first, then the limit -- the same ORDER BY heartbeat_at / LIMIT the
	// real stores use, which is what makes the doubled set a subset of the
	// normal one rather than merely overlapping it.
	sort.Slice(out, func(i, j int) bool { return out[i].Key.WorkflowID < out[j].Key.WorkflowID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStaleHolderReaper) ReapStaleInstancesExcept(_ context.Context, timeout time.Duration, _ int, exclude []engine.GenerationKey) (int, error) {
	f.lastExclude = exclude
	excluded := make(map[engine.GenerationKey]struct{}, len(exclude))
	for _, k := range exclude {
		excluded[k] = struct{}{}
	}
	f.reclaimed = 0
	for _, r := range f.rows {
		if r.age >= timeout {
			if _, ok := excluded[r.key]; !ok {
				f.reclaimed++
			}
		}
	}
	return f.reclaimed, nil
}

func testWorker() *Worker {
	return &Worker{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// testKey builds a GenerationKey whose WorkflowID doubles as its sort order.
func testKey(id string) engine.GenerationKey {
	return engine.GenerationKey{WorkflowID: id, Generation: 1}
}

// TestReaperAsksOnlyTheRowsInsideTheVetoWindow is cleat#2196's one-extra-window
// BOUND, tested directly: a row one window stale is asked about and can be
// vetoed; a row two windows stale is not asked at all, so no holder -- honest or
// lying -- can hold its own reclaim past that.
func TestReaperAsksOnlyTheRowsInsideTheVetoWindow(t *testing.T) {
	const window = 10 * time.Second
	near := testKey("run-near")
	far := testKey("run-far")

	f := &fakeStaleHolderReaper{rows: []agedHolder{
		{key: near, holder: "w-near", age: 15 * time.Second}, // 1.5 windows
		{key: far, holder: "w-far", age: 25 * time.Second},   // 2.5 windows
	}}

	rows, err := testWorker().staleRowsToAsk(context.Background(), f, window)
	if err != nil {
		t.Fatalf("staleRowsToAsk: %v", err)
	}
	if len(rows) != 1 || rows[0].Key != near {
		t.Fatalf("staleRowsToAsk asked about %v, want exactly the 1.5-window row %v", rows, near)
	}

	want := []time.Duration{window, 2 * window}
	if len(f.listTimeouts) != len(want) {
		t.Fatalf("ListStaleHolders called %d times, want %d (%v)", len(f.listTimeouts), len(want), f.listTimeouts)
	}
	for i, wnt := range want {
		if f.listTimeouts[i] != wnt {
			t.Errorf("ListStaleHolders call %d used window %v, want %v", i+1, f.listTimeouts[i], wnt)
		}
	}
}

// TestAHeldRowIsExcludedAndASilentOneIsNot walks the whole veto decision: the
// row whose holder answers "yes, still mine" is excluded from the sweep, and the
// row whose holder says nothing at all -- the killed-worker case -- is not.
func TestAHeldRowIsExcludedAndASilentOneIsNot(t *testing.T) {
	const window = 10 * time.Second
	held := testKey("run-a-held")
	killed := testKey("run-b-killed")

	f := &fakeStaleHolderReaper{rows: []agedHolder{
		{key: held, holder: "w-alive", age: 12 * time.Second},
		{key: killed, holder: "w-killed", age: 12 * time.Second},
	}}
	w := testWorker()

	rows, err := w.staleRowsToAsk(context.Background(), f, window)
	if err != nil {
		t.Fatalf("staleRowsToAsk: %v", err)
	}
	questions := w.holderQuestions(rows, map[string]string{
		"w-alive":  "w-alive.headless",
		"w-killed": "w-killed.headless",
	})

	// Holders are asked in PARALLEL, so the stub's bookkeeping needs a lock --
	// the first version of this test used a bare map and the race detector
	// crashed it with "concurrent map writes" only when run alongside the other
	// tests in the package.
	var mu sync.Mutex
	asked := map[string]int{}
	ask := func(_ context.Context, address, _ string, _ int64) bool {
		mu.Lock()
		asked[address]++
		mu.Unlock()
		return address == "w-alive.headless"
	}
	exclude := w.askHolders(context.Background(), questions, ask)

	mu.Lock()
	defer mu.Unlock()
	if len(exclude) != 1 || exclude[0] != held {
		t.Fatalf("exclude = %v, want exactly the held row %v", exclude, held)
	}
	if asked["w-alive.headless"] != 1 || asked["w-killed.headless"] != 1 {
		t.Errorf("asked = %v, want each live holder asked exactly once", asked)
	}

	// And the exclusion survives into the reclaim: the held row is left alone,
	// the silent one is taken.
	n, err := f.ReapStaleInstancesExcept(context.Background(), window, 200, exclude)
	if err != nil {
		t.Fatalf("ReapStaleInstancesExcept: %v", err)
	}
	if n != 1 || f.reclaimed != 1 {
		t.Errorf("reclaimed %d rows, want 1 (the silent holder's run, within the normal window)", n)
	}
	for _, k := range f.lastExclude {
		if k == killed {
			t.Errorf("the killed holder's row was excluded, so a dead worker's run is NOT reclaimed within the normal window")
		}
	}
}

// TestARowWhoseHolderPublishedNoAddressIsStillReclaimed pins the additive
// direction: every failure to obtain an address is "no veto", never a hold. A
// worker outside Kubernetes, or one whose row has aged out, must not have its
// runs pinned by a channel it was never part of.
func TestARowWhoseHolderPublishedNoAddressIsStillReclaimed(t *testing.T) {
	const window = 10 * time.Second
	f := &fakeStaleHolderReaper{rows: []agedHolder{
		{key: testKey("run-a"), holder: "w-unaddressable", age: 12 * time.Second},
		{key: testKey("run-b"), holder: "w-addressed", age: 12 * time.Second},
	}}
	w := testWorker()

	rows, err := w.staleRowsToAsk(context.Background(), f, window)
	if err != nil {
		t.Fatalf("staleRowsToAsk: %v", err)
	}
	// The registry knows only one of the two holders, and knows it with an
	// empty address -- the "--worker-service-name unset" shape.
	questions := w.holderQuestions(rows, map[string]string{
		"w-addressed":     "w-addressed.headless",
		"w-unaddressable": "",
	})
	if len(questions) != 1 || questions[0].holder != "w-addressed" {
		t.Fatalf("questions = %v, want only the holder with a published address", questions)
	}

	var mu sync.Mutex
	calls := 0
	ask := func(_ context.Context, address, _ string, _ int64) bool {
		mu.Lock()
		calls++
		mu.Unlock()
		if address != "w-addressed.headless" {
			t.Errorf("asked %q, want only the addressed holder", address)
		}
		return false
	}
	if exclude := w.askHolders(context.Background(), questions, ask); len(exclude) != 0 {
		t.Errorf("exclude = %v, want none -- nobody answered held", exclude)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("asked %d holders, want 1", calls)
	}
}

// TestTheRegistryOutlivesTheWindowAReaperCanAskIn is the guard for the defect
// that made this whole channel structurally unable to fire.
//
// The holders the reaper must resolve are exactly the ones whose heartbeats have
// stopped, so their registry rows age at the same rate as their runs. The lease
// (membershipStaleAfter) is no longer than one reclaim window at every heartbeat
// a worker will start with, so sweeping at the lease deletes the address before
// the run is even eligible -- and swept is swept: no maxAge passed to ListLive
// brings the row back. Retention must therefore cover the window in which a row
// can still be ASKED about, which ends at 2*reclaimAfter.
func TestTheRegistryOutlivesTheWindowAReaperCanAskIn(t *testing.T) {
	for _, hb := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 30 * time.Second, 149 * time.Second} {
		t.Run(hb.String(), func(t *testing.T) {
			reclaim := reclaimWindow(0, hb)
			lease := membershipStaleAfter(hb)
			retention := workerRegistryRetention(hb, reclaim)

			if retention <= 2*reclaim {
				t.Errorf("retention %v does not outlive 2*reclaimAfter (%v): a row can be asked about until then", retention, 2*reclaim)
			}
			if retention <= lease {
				t.Errorf("retention %v is not longer than the lease %v, so it sweeps where the old code did", retention, lease)
			}
		})
	}

	// THE FINDING ITSELF, pinned at the DEFAULT heartbeat because it is a fact
	// about these two windows rather than an invariant of their formulas: the
	// lease is 10s and one reclaim window is 14.5s, so the old sweep deleted a
	// holder's address 4.5s BEFORE its run was even eligible. (At a 1s heartbeat
	// the reclaimed window's own 10s floor makes the two equal, which is why
	// this is asserted here rather than in the loop above.) If it ever stops
	// holding, workerRegistryRetention could collapse back to the lease -- and
	// this, not a comment, is what would say so.
	lease := membershipStaleAfter(5 * time.Second)
	reclaim := reclaimWindow(0, 5*time.Second)
	if lease >= reclaim {
		t.Errorf("at the default 5s heartbeat the lease (%v) is no longer shorter than one reclaim window (%v); "+
			"re-derive whether workerRegistryRetention still needs to differ from it", lease, reclaim)
	}

	// AND THE OTHER END OF THE BOUNDARY, which is not "less bad" but worse: at 2s
	// and below, reclaimWindow's own 10s floor makes the lease and the reclaim
	// window EQUAL, so a holder's row is swept at the very instant its run
	// becomes eligible and there is no window to ask in at all. Both halves are
	// asserted because the retention cannot be expressed as "the lease plus a
	// bit" -- the relationship changes sign at this boundary, and it was found
	// by this test rather than assumed.
	for _, hb := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second} {
		if got, want := membershipStaleAfter(hb), reclaimWindow(0, hb); got != want {
			t.Errorf("at hb=%v the lease (%v) and the reclaim window (%v) are no longer equal; "+
				"re-derive where the two windows cross", hb, got, want)
		}
	}
}
