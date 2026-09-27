package plugin

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The budget is enforced at ADMISSION -- the moment it would be violated --
// rather than by a sweep that can only shrink an overshoot afterwards.
// cleat#1470.
func TestAdmittingATenantEvictsTheLeastRecentlyUsed(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	tp, _ := newCountingPools(t)
	tp.clock = func() time.Time { return now }

	// Room for exactly three pools at the floor of 2.
	tp.SetConnectionBudget(3 * defaultMinConnsPerTenantPool)

	// Open three, each one minute apart, so their LRU order is unambiguous.
	for _, id := range []string{"oldest", "middle", "newest"} {
		if _, err := tp.For(context.Background(), id); err != nil {
			t.Fatalf("For(%s): %v", id, err)
		}
		now = now.Add(time.Minute)
	}
	if got := len(tp.pools); got != 3 {
		t.Fatalf("expected 3 pools, got %d -- the assertions below are vacuous without them", got)
	}

	// A fourth tenant arrives. The budget has no room, so the LEAST RECENTLY
	// USED pool goes.
	if _, err := tp.For(context.Background(), "fourth"); err != nil {
		t.Fatalf("For(fourth): %v", err)
	}

	tp.mu.Lock()
	defer tp.mu.Unlock()
	if len(tp.pools) > 3 {
		t.Errorf("%d pools live, budget allows 3.\n\n"+
			"Admission did not make room, so the budget bounds nothing: every new tenant "+
			"simply adds a pool, which is the unbounded behaviour cleat#1470 describes.",
			len(tp.pools))
	}
	if _, ok := tp.pools["oldest"]; ok {
		t.Error("the least-recently-used pool survived. Eviction is not ordered by " +
			"lastUsed, so which tenant loses its pool is arbitrary.")
	}
	for _, id := range []string{"middle", "newest", "fourth"} {
		if _, ok := tp.pools[id]; !ok {
			t.Errorf("pool %q was evicted; only the least-recently-used should have been", id)
		}
	}
}

// An unset budget changes nothing, which is what every existing deployment has.
func TestNoBudgetMeansNoEviction(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	tp, _ := newCountingPools(t)
	tp.clock = func() time.Time { return now }
	// Deliberately NOT calling SetConnectionBudget.

	for i := 0; i < 12; i++ {
		if _, err := tp.For(context.Background(), fmt.Sprintf("tenant-%02d", i)); err != nil {
			t.Fatalf("For: %v", err)
		}
		now = now.Add(time.Second)
	}
	if got := len(tp.pools); got != 12 {
		t.Errorf("%d pools live after 12 tenants with no budget set, want 12.\n\n"+
			"The budget is opt-in. A worker that starts evicting because it upgraded has "+
			"gained a behaviour change nobody asked for.", got)
	}
}

// The share is equal across live pools, and never below the floor.
//
// Accounting in whole pools of maxConns would reserve 25 connections for a
// tenant using 2. The owner rejected that: "the accounting unit should not be
// 25 connections".
func TestTheBudgetIsSharedRatherThanReserved(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	tp, _ := newCountingPools(t)
	tp.clock = func() time.Time { return now }
	tp.SetConnectionBudget(30)

	openPool := func(id string) {
		t.Helper()
		if _, err := tp.For(context.Background(), id); err != nil {
			t.Fatalf("For(%s): %v", id, err)
		}
		now = now.Add(time.Second)
	}

	// One tenant: capped by its own per-pool maximum (25), not by the budget.
	openPool("a")
	if got := poolMaxOpen(t, tp, "a"); got != 25 {
		t.Errorf("one tenant got a cap of %d, want 25 (its own maxConns).\n\n"+
			"A lone tenant should not be squeezed to the share it would get if the worker "+
			"were full; the budget is a ceiling, not a reservation.", got)
	}

	// Three tenants: 30/3 = 10 each.
	openPool("b")
	openPool("c")
	for _, id := range []string{"a", "b", "c"} {
		if got := poolMaxOpen(t, tp, id); got != 10 {
			t.Errorf("with 3 pools under a budget of 30, %q has a cap of %d, want 10", id, got)
		}
	}

	// Twenty tenants would be 1 each; the floor holds it at 2, and admission
	// caps the pool count instead.
	for i := 0; i < 20; i++ {
		openPool(fmt.Sprintf("t%02d", i))
	}
	tp.mu.Lock()
	live := len(tp.pools)
	tp.mu.Unlock()
	if live > 30/defaultMinConnsPerTenantPool {
		t.Errorf("%d pools live under a budget of 30 with a floor of %d, want at most %d.\n\n"+
			"Below the floor a pool cannot do useful work, so admitting more tenants trades "+
			"a bounded number of working pools for an unbounded number of stalled ones.",
			live, defaultMinConnsPerTenantPool, 30/defaultMinConnsPerTenantPool)
	}
}

// Setting a budget must not panic, which it did before minPerPool had a
// default: makeRoomLocked divides by it.
//
// Kept as a regression test rather than deleted after the fix, because a zero
// default is exactly the kind of thing a later refactor reintroduces -- and the
// symptom is a panic on the hot path of the first tenant a budgeted worker
// touches.
func TestSettingABudgetDoesNotDivideByZero(t *testing.T) {
	tp, _ := newCountingPools(t)
	tp.SetConnectionBudget(100)
	if _, err := tp.For(context.Background(), "tenant-A"); err != nil {
		t.Fatalf("For: %v", err)
	}
	if tp.minPerPool <= 0 {
		t.Fatalf("minPerPool is %d; makeRoomLocked divides the budget by it", tp.minPerPool)
	}
}

// poolMaxOpen reads back the cap actually applied to one tenant's pool.
func poolMaxOpen(t *testing.T, tp *TenantPools, id string) int {
	t.Helper()
	tp.mu.Lock()
	e, ok := tp.pools[id]
	tp.mu.Unlock()
	if !ok {
		t.Fatalf("pool %q is not live", id)
	}
	<-e.ready
	if e.db == nil {
		t.Fatalf("pool %q has no *sql.DB", id)
	}
	return e.db.Stats().MaxOpenConnections
}
