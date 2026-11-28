package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3572. CountEventHistoryTotal and its siblings scope themselves to their
// store's tenant (an explicit tenant_id predicate, and row-level security besides),
// and the metrics sweep ran them through the worker's own store only, so
// cleat_event_history_row_count described one tenant on a worker serving several.
//
// # This is that scenario reduced to a test
//
// The survivor's own tenant is the default one. The gauge is read after a first
// pass; a tenant other than the default is then given event rows; a second pass
// must move the gauge by exactly that many. The suite database is shared, so the
// test measures the CHANGE rather than an absolute count. The worker's own tenant,
// given rows the same way, is the known-positive: the old code moves the gauge for
// it, so the difference between the two subtests is whose rows they are and
// nothing else. The cleat_app connection is asserted not to bypass the policy.
//
// PostgreSQL only: MetricsStore is implemented by the PostgreSQL store alone (the
// sweep says so at startup on the other dialects).
func TestAnotherTenantsEventRowsMoveThePublishedCountUnderRLS(t *testing.T) {
	owner := testutil.SuiteTestDB(t, "cleat_worker_metrics_sweep")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	var bypass, super bool
	if err := rlsDB.QueryRow(
		`SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if bypass || super {
		t.Fatalf("the RLS role has BYPASSRLS=%v SUPERUSER=%v; this test would measure nothing", bypass, super)
	}
	const other = "66666666-0000-4000-8000-000035720005"
	rig := postgresHBRig(t, owner, rlsDB, true, other)

	run := func(t *testing.T, tenant string) {
		t.Helper()
		if tenant != engine.DefaultTenantUUID {
			rig.register(t, tenant)
		}
		const prefix = "metrics-3572-"
		clear := func() {
			_, _ = owner.Exec(`DELETE FROM event_history WHERE tenant_id = $1 AND workflow_id LIKE $2`, tenant, prefix+"%")
		}
		clear()
		t.Cleanup(clear)

		w := newTestWorkerFromStore(rig.ownStore)
		defer w.cancel()
		w.id = "metrics-3572-survivor"
		w.storeTenantID = engine.DefaultTenantUUID
		w.storeFactory = rig.factory
		w.claimAcrossTenants = true
		w.stallThreshold = time.Minute
		w.keyExpiryWindow = time.Hour
		ms, ok := rig.ownStore.(MetricsStore)
		if !ok {
			t.Fatalf("%T does not implement MetricsStore", rig.ownStore)
		}

		w.metricsSweepOnce(ms)
		before, ok := gauge(scrape(t, w), "cleat_event_history_row_count")
		if !ok {
			t.Fatal("the first pass published no cleat_event_history_row_count: the premise of this test does not hold")
		}

		const added = 7
		for i := 0; i < added; i++ {
			if _, err := owner.Exec(`INSERT INTO event_history (workflow_id, step, tenant_id, event_type) VALUES ($1, $2, $3, 'call')`,
				fmt.Sprintf("%s%d", prefix, i), 0, tenant); err != nil {
				t.Fatalf("seeding event row %d: %v", i, err)
			}
		}

		w.metricsSweepOnce(ms)
		after, _ := gauge(scrape(t, w), "cleat_event_history_row_count")
		if got := after - before; got != added {
			t.Fatalf("cleat_event_history_row_count moved by %v after tenant %s gained %d event rows, want %d. The count scopes itself to its "+
				"store's tenant and the sweep ran it through the worker's own store only (cleat#3572)", got, tenant, added, added)
		}
	}

	t.Run("a non-default tenant's rows, counted by a worker whose own tenant is the default", func(t *testing.T) {
		run(t, other)
	})
	t.Run("the worker's own tenant's rows (the control: the old code counts these too)", func(t *testing.T) {
		run(t, engine.DefaultTenantUUID)
	})
}
