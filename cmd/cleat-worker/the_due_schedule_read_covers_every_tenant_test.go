package main

import (
	"context"
	"errors"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// GetDueSchedules on a queuedTenantStore returns one schedule named after the
// tenant, so a test can see which tenants were actually read.
func (s *queuedTenantStore) GetDueSchedules(context.Context) ([]engine.Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scheduleReads++
	if s.scheduleErr != nil {
		return nil, s.scheduleErr
	}
	return []engine.Schedule{{Name: s.tenantID, TenantID: s.tenantID}}, nil
}

func tenantsIn(schedules []engine.Schedule) map[string]int {
	out := map[string]int{}
	for _, s := range schedules {
		out[s.TenantID]++
	}
	return out
}

// EVERY tenant is read on every tick, not a share of them.
//
// This is the one place the rotating claim's bargain does not carry over. Work
// that waits a tick is work that waits a tick; a cron schedule that waits a
// tick has fired late, and with enough tenants a minutely schedule quietly
// becomes an every-few-minutes one. The schedule loop runs on a 15-second
// ticker against minute-granularity cron, which is what makes reading all of
// them affordable.
//
// So this test exists to fail if someone later applies the claim's per-tick
// ceiling here, which would look like a consistency improvement and would be a
// correctness regression.
func TestTheDueScheduleReadCoversEveryTenant(t *testing.T) {
	// Deliberately more tenants than the claim's per-tick ceiling, so a
	// rotation would be visibly short.
	backlog := map[string]int{}
	var want []string
	for _, id := range []string{
		"t01", "t02", "t03", "t04", "t05", "t06", "t07", "t08", "t09", "t10",
		"t11", "t12", "t13", "t14", "t15", "t16", "t17", "t18", "t19", "t20",
	} {
		backlog[id] = 0
		want = append(want, id)
	}
	w, _, _ := newRotatingWorker(t, backlog)
	if w.claimTenantsPerTick >= len(want) {
		t.Fatalf("this test needs more tenants (%d) than the claim's per-tick ceiling (%d), "+
			"or it cannot tell a full pass from a rotation", len(want), w.claimTenantsPerTick)
	}

	due, err := w.dueSchedulesByTenant()
	if err != nil {
		t.Fatalf("dueSchedulesByTenant: %v", err)
	}
	got := tenantsIn(due)
	for _, id := range want {
		if got[id] != 1 {
			t.Errorf("tenant %s contributed %d due schedules, want 1 -- the read did not "+
				"cover every tenant", id, got[id])
		}
	}
	if len(got) != len(want) {
		t.Errorf("read %d tenants, want %d", len(got), len(want))
	}
}

// A tenant whose store will not open, or whose read fails, does not stop the
// others' cron.
func TestABrokenTenantDoesNotStopTheOthersCron(t *testing.T) {
	w, factory, _ := newRotatingWorker(t, map[string]int{"a": 0, "b": 0, "c": 0})
	factory.openErr["b"] = errors.New("tenant b is unreachable")
	factory.stores["c"].scheduleErr = errors.New("read failed")

	due, err := w.dueSchedulesByTenant()
	if err != nil {
		t.Fatalf("dueSchedulesByTenant returned an error while a tenant was readable: %v", err)
	}
	got := tenantsIn(due)
	if got["a"] != 1 {
		t.Errorf("tenant a's schedules were lost to another tenant's failure: %v", got)
	}
	for _, id := range []string{"b", "c"} {
		if got[id] != 0 {
			t.Errorf("tenant %s contributed schedules despite failing", id)
		}
	}
}

// When no tenant could be read at all, the error is returned.
//
// The complement of the test above, and the reason it is not enough to always
// swallow: (nil, nil) reads to the schedule loop as "nothing is due", which is
// indistinguishable from a quiet period, so a database outage would look like
// a deployment with no cron.
func TestADueScheduleReadThatReachedNobodyReportsWhy(t *testing.T) {
	w, factory, _ := newRotatingWorker(t, map[string]int{"a": 0, "b": 0})
	factory.openErr["a"] = errors.New("down")
	factory.openErr["b"] = errors.New("down")

	if _, err := w.dueSchedulesByTenant(); err == nil {
		t.Fatal("read nothing from any tenant and reported success; the schedule loop " +
			"cannot tell that from a tick with nothing due")
	}
}

// A worker that cannot read per tenant says so rather than pretending.
func TestADueScheduleReadRefusesWhatItCannotDo(t *testing.T) {
	t.Run("no factory", func(t *testing.T) {
		w, _, _ := newRotatingWorker(t, map[string]int{"a": 0})
		w.storeFactory = nil
		if _, err := w.dueSchedulesByTenant(); !errors.Is(err, errRotatingClaimUnavailable) {
			t.Fatalf("err = %v; want errRotatingClaimUnavailable", err)
		}
	})
	t.Run("store cannot list tenants", func(t *testing.T) {
		w, _, _ := newRotatingWorker(t, map[string]int{"a": 0})
		w.store = &mockStore{}
		if _, err := w.dueSchedulesByTenant(); !errors.Is(err, errRotatingClaimUnavailable) {
			t.Fatalf("err = %v; want errRotatingClaimUnavailable", err)
		}
	})
}

// dueSchedules prefers the per-tenant read, and honours claim-strategy=global.
func TestDueSchedulesPrefersThePerTenantRead(t *testing.T) {
	t.Run("reads per tenant when it can", func(t *testing.T) {
		w, _, ls := newRotatingWorker(t, map[string]int{"a": 0, "b": 0})
		if _, err := w.dueSchedules(); err != nil {
			t.Fatalf("dueSchedules: %v", err)
		}
		if ls.lists != 1 {
			t.Errorf("tenant list read %d times; want 1 -- dueSchedules did not take the "+
				"per-tenant path", ls.lists)
		}
	})

	t.Run("honours claim-strategy=global", func(t *testing.T) {
		w, _, ls := newRotatingWorker(t, map[string]int{"a": 0, "b": 0})
		w.claimStrategy = claimStrategyGlobal
		if _, err := w.dueSchedules(); err != nil {
			t.Fatalf("dueSchedules: %v", err)
		}
		if ls.lists != 0 {
			t.Errorf("tenant list read %d times under claim-strategy=global; want 0", ls.lists)
		}
	})
}
