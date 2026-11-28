package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3572. The retention arms scope themselves to their store's tenant, and
// retentionLoop ran them through the worker's own store only, so every other
// tenant kept its history for ever -- including under --retention-days, which is
// on by default. These are the routing half, against recording stores and the
// doubles from the_reaper_visits_each_tenants_own_store_test.go: which store is
// asked, which arms, with what cutoff, and what a failure means. The half that
// needs a real database and a role the policy applies to is
// a_stale_tenants_history_is_swept_under_rls_test.go.

// retentionProbe records the cutoff each arm was asked with, and can make an arm fail.
type retentionProbe struct {
	*mockStore
	mu      sync.Mutex
	cutoffs map[string][]time.Time
}

func newRetentionProbe(failing ...string) *retentionProbe {
	p := &retentionProbe{mockStore: &mockStore{}, cutoffs: map[string][]time.Time{}}
	fail := map[string]bool{}
	for _, a := range failing {
		fail[a] = true
	}
	rec := func(arm string) func(context.Context, time.Time) (int64, error) {
		return func(_ context.Context, cutoff time.Time) (int64, error) {
			p.mu.Lock()
			p.cutoffs[arm] = append(p.cutoffs[arm], cutoff)
			p.mu.Unlock()
			if fail[arm] {
				return 0, errors.New(arm + " failed for this tenant")
			}
			return 1, nil
		}
	}
	p.mockStore.deleteExpiredEventsFn = rec("events")
	p.mockStore.clearExpiredCompactionStateFn = rec("compaction")
	p.mockStore.deleteCompletedWorkflowsFn = rec("completed")
	p.mockStore.deleteDeadLetteredWorkflowsFn = rec("dead_lettered")
	return p
}

func (p *retentionProbe) asked(arm string) []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Time(nil), p.cutoffs[arm]...)
}

type ownRetention struct {
	*retentionProbe
	tenants []string
	listErr error
}

func (s *ownRetention) ListTenantIDs(context.Context) ([]string, error) {
	return s.tenants, s.listErr
}

func newRetentionWorker(own *ownRetention, f *perTenantFactory) *Worker {
	w := newTestWorkerFromStore(own)
	w.storeTenantID = tenantSelf
	w.claimAcrossTenants = true
	w.storeFactory = f
	return w
}

var retentionArms = []string{"events", "compaction", "completed", "dead_lettered"}

func TestRetentionSweepsEachOtherTenantThroughThatTenantsOwnStore(t *testing.T) {
	own := &ownRetention{retentionProbe: newRetentionProbe(), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	a, c := newRetentionProbe(), newRetentionProbe()
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: a, "tenant-c": c}}
	w := newRetentionWorker(own, f)
	defer w.cancel()
	sweptAt := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

	w.runRetentionSweepAt(sweptAt, 30, 7, 14)

	// THE ASSERTION. Through the worker's own store alone (the bug) the other
	// tenants' probes are never asked.
	for _, arm := range retentionArms {
		if got := own.asked(arm); len(got) != 1 {
			t.Errorf("the worker's own store was asked %d times for %s, want 1 (the own tenant is not also visited through the factory)", len(got), arm)
		}
	}
	// The cutoffs are the operator's flags applied to ONE clock reading, the same
	// for every tenant as for the worker's own: events and compaction at 30 days,
	// completed at 7, dead-lettered at 14.
	want := map[string]time.Time{
		"events":        sweptAt.Add(-30 * 24 * time.Hour),
		"compaction":    sweptAt.Add(-30 * 24 * time.Hour),
		"completed":     sweptAt.Add(-7 * 24 * time.Hour),
		"dead_lettered": sweptAt.Add(-14 * 24 * time.Hour),
	}
	for name, p := range map[string]*retentionProbe{tenantOther: a, "tenant-c": c} {
		for _, arm := range retentionArms {
			got := p.asked(arm)
			if len(got) != 1 {
				t.Errorf("tenant %s: %s was asked %d times, want once: its history is never swept (cleat#3572)", name, arm, len(got))
				continue
			}
			if !got[0].Equal(want[arm]) {
				t.Errorf("tenant %s: %s was asked with cutoff %v, want %v (the operator's flag against the sweep's one clock reading)", name, arm, got[0], want[arm])
			}
		}
	}
	f.mu.Lock()
	opened, closed := append([]string(nil), f.opened...), f.closed
	f.mu.Unlock()
	if !sameSet(opened, []string{tenantOther, "tenant-c"}) {
		t.Errorf("the factory opened %v, want the two other tenants and not the worker's own", opened)
	}
	if closed != 2 {
		t.Errorf("%d tenant store leases released, want 2", closed)
	}
}

// A flag at 0 is a deliberate decision, and the arms it switches off delete the
// workflow record itself. They must stay off for every tenant, not only the
// worker's own.
func TestRetentionDoesNotRunAnArmThatIsSwitchedOffAgainstAnotherTenant(t *testing.T) {
	own := &ownRetention{retentionProbe: newRetentionProbe(), tenants: []string{tenantSelf, tenantOther}}
	other := newRetentionProbe()
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
	w := newRetentionWorker(own, f)
	defer w.cancel()

	w.runRetentionSweepAt(time.Now(), 30, 0, 0)

	for _, arm := range []string{"completed", "dead_lettered"} {
		if got := other.asked(arm); len(got) != 0 {
			t.Errorf("%s ran against another tenant with its flag at 0: %v", arm, got)
		}
	}
	for _, arm := range []string{"events", "compaction"} {
		if got := other.asked(arm); len(got) != 1 {
			t.Errorf("%s ran %d times against another tenant, want 1 (--retention-days is on)", arm, len(got))
		}
	}
}

// Each flag decides its own arms and no other's. The arms behind --completed-workflow-retention-days
// and --dead-letter-retention-days delete the workflow RECORD itself, so a flag at 0 must keep its arm
// off for another tenant in every combination, not only when the other two flags are the ones set.
// A mutant that gates the dead-letter arm on the completed flag passes (30,0,0) and deletes every
// other tenant's dead-lettered runs under (0,7,0); each flag alone, and in pairs, is what catches it.
func TestEachRetentionFlagDecidesExactlyItsOwnArmsForAnotherTenant(t *testing.T) {
	const events, completed, dead = 30, 7, 14
	for _, tc := range []struct {
		name  string
		flags [3]int // --retention-days, --completed-workflow-retention-days, --dead-letter-retention-days
		ran   map[string]bool
	}{
		{"retention-days alone", [3]int{events, 0, 0}, map[string]bool{"events": true, "compaction": true}},
		{"completed-workflow-retention-days alone", [3]int{0, completed, 0}, map[string]bool{"completed": true}},
		{"dead-letter-retention-days alone", [3]int{0, 0, dead}, map[string]bool{"dead_lettered": true}},
		{"retention-days and completed", [3]int{events, completed, 0}, map[string]bool{"events": true, "compaction": true, "completed": true}},
		{"retention-days and dead-letter", [3]int{events, 0, dead}, map[string]bool{"events": true, "compaction": true, "dead_lettered": true}},
		{"completed and dead-letter", [3]int{0, completed, dead}, map[string]bool{"completed": true, "dead_lettered": true}},
		{"all three", [3]int{events, completed, dead}, map[string]bool{"events": true, "compaction": true, "completed": true, "dead_lettered": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			own := &ownRetention{retentionProbe: newRetentionProbe(), tenants: []string{tenantSelf, tenantOther}}
			other := newRetentionProbe()
			f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
			w := newRetentionWorker(own, f)
			defer w.cancel()
			sweptAt := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

			w.runRetentionSweepAt(sweptAt, tc.flags[0], tc.flags[1], tc.flags[2])

			wantCutoff := map[string]time.Time{
				"events":        sweptAt.Add(-time.Duration(tc.flags[0]) * 24 * time.Hour),
				"compaction":    sweptAt.Add(-time.Duration(tc.flags[0]) * 24 * time.Hour),
				"completed":     sweptAt.Add(-time.Duration(tc.flags[1]) * 24 * time.Hour),
				"dead_lettered": sweptAt.Add(-time.Duration(tc.flags[2]) * 24 * time.Hour),
			}
			for _, arm := range retentionArms {
				got := other.asked(arm)
				if !tc.ran[arm] {
					if len(got) != 0 {
						t.Errorf("flags %v: the %s arm ran against another tenant (cutoffs %v) though its own flag is 0. "+
							"A flag decides its own arms only, and these delete records", tc.flags, arm, got)
					}
					continue
				}
				if len(got) != 1 {
					t.Errorf("flags %v: the %s arm ran %d times against another tenant, want 1", tc.flags, arm, len(got))
				} else if !got[0].Equal(wantCutoff[arm]) {
					t.Errorf("flags %v: the %s arm used cutoff %v, want %v (its own flag against the sweep's one clock reading)",
						tc.flags, arm, got[0], wantCutoff[arm])
				}
				// And the worker's own tenant got exactly the same arms with the same cutoffs.
				if mine := own.asked(arm); len(mine) != 1 || (len(got) == 1 && !mine[0].Equal(got[0])) {
					t.Errorf("flags %v: the worker's own %s arm was asked %v, another tenant %v: they must agree", tc.flags, arm, mine, got)
				}
			}
		})
	}
}

func TestOneTenantsRetentionFailureStopsNeitherItsOtherArmsNorTheNextTenant(t *testing.T) {
	own := &ownRetention{retentionProbe: newRetentionProbe(), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	bad := newRetentionProbe("events")
	good := newRetentionProbe()
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: bad, "tenant-c": good}}
	w := newRetentionWorker(own, f)
	defer w.cancel()
	w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
	troubleBefore := w.lastDBTrouble.Load()

	w.runRetentionSweepAt(time.Now(), 30, 7, 14)

	for _, arm := range []string{"compaction", "completed", "dead_lettered"} {
		if len(bad.asked(arm)) != 1 {
			t.Errorf("the events arm failing for a tenant stopped that tenant's %s arm: the arms are independent", arm)
		}
	}
	for _, arm := range retentionArms {
		if len(good.asked(arm)) != 1 {
			t.Errorf("one tenant's failure stopped the next tenant's %s arm", arm)
		}
	}
	if got := w.lastDBTrouble.Load(); got != troubleBefore {
		t.Error("one tenant's retention failing was recorded as this worker's database trouble")
	}
}

func TestRetentionWhenATenantStoreCannotBeOpenedTheOthersAreStillSwept(t *testing.T) {
	own := &ownRetention{retentionProbe: newRetentionProbe(), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	good := newRetentionProbe()
	f := &perTenantFactory{
		stores:  map[string]engine.WorkflowStore{"tenant-c": good},
		openErr: map[string]error{tenantOther: errors.New("no pool")},
	}
	w := newRetentionWorker(own, f)
	defer w.cancel()

	w.runRetentionSweepAt(time.Now(), 30, 7, 14)

	if len(good.asked("events")) != 1 {
		t.Error("a tenant whose store would not open stopped the others being swept")
	}
}

func TestRetentionVisitsNoOtherTenantWhenAnArmFailsOnTheOwnStore(t *testing.T) {
	for _, arm := range retentionArms {
		t.Run(arm, func(t *testing.T) {
			own := &ownRetention{retentionProbe: newRetentionProbe(arm), tenants: []string{tenantSelf, tenantOther}}
			other := newRetentionProbe()
			f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
			w := newRetentionWorker(own, f)
			defer w.cancel()

			w.runRetentionSweepAt(time.Now(), 30, 7, 14)

			for _, a := range retentionArms {
				if len(other.asked(a)) != 0 {
					t.Errorf("the worker's own %s arm failed (its database is in trouble) and another tenant's %s was swept anyway", arm, a)
				}
			}
		})
	}
}

func TestRetentionStaysOnItsOwnTenantWhenItCannotServeOthers(t *testing.T) {
	other := newRetentionProbe()
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}

	t.Run("claim-across-tenants off", func(t *testing.T) {
		own := &ownRetention{retentionProbe: newRetentionProbe(), tenants: []string{tenantSelf, tenantOther}}
		w := newRetentionWorker(own, f)
		defer w.cancel()
		w.claimAcrossTenants = false
		w.runRetentionSweepAt(time.Now(), 30, 7, 14)
		if len(other.asked("events")) != 0 || len(own.asked("events")) != 1 {
			t.Errorf("own %d, other %d; with --claim-across-tenants off only the own tenant is swept", len(own.asked("events")), len(other.asked("events")))
		}
	})
	t.Run("no store factory", func(t *testing.T) {
		own := &ownRetention{retentionProbe: newRetentionProbe(), tenants: []string{tenantSelf, tenantOther}}
		w := newRetentionWorker(own, nil)
		w.storeFactory = nil
		defer w.cancel()
		w.runRetentionSweepAt(time.Now(), 30, 7, 14)
		if len(own.asked("events")) != 1 {
			t.Errorf("own asked %d times, want 1", len(own.asked("events")))
		}
	})
	t.Run("a listing that fails still sweeps the own tenant", func(t *testing.T) {
		own := &ownRetention{retentionProbe: newRetentionProbe(), listErr: errors.New("admin.tenants unreadable")}
		w := newRetentionWorker(own, f)
		defer w.cancel()
		w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
		troubleBefore := w.lastDBTrouble.Load()
		w.runRetentionSweepAt(time.Now(), 30, 7, 14)
		if len(own.asked("events")) != 1 {
			t.Errorf("own asked %d times, want 1", len(own.asked("events")))
		}
		if w.lastDBTrouble.Load() != troubleBefore {
			t.Error("failing to list tenants was recorded as the worker's database trouble")
		}
	})
}
