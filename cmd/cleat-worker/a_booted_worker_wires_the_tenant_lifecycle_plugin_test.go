package main

// cleat#2534. Pins plugin.Environment.SetTenantSuspended's assignment inside
// main() the same way TestABootedWorkerDisablesAnExpiredOAuthMintedKey pins
// RevokeExpiredOAuthAPIKeys, for the same reason that test's own comment
// gives: a unit test constructing plugin.Environment{} as a literal proves
// nothing about whether main() actually sets the field -- it is a closure
// built inside main(), and a shim for it would be a second implementation of
// the collection step that could pass while the real one broke.
//
// TWO TENANTS, DELIBERATELY -- not one. plugins/tenantlifecycle's own
// TestSweep_MultiBackend already makes the point that a 1-tenant fixture
// cannot distinguish a real filter from a loop that suspends unconditionally;
// this boot test inherits the same requirement one layer up, where it is
// cheaper to get wrong and easier not to notice: a worker that suspended
// every tenant on every tick would still turn this test green with a single
// fixture.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
)

// TestABootedWorkerSuspendsAnExpiredTrial pins SetTenantSuspended's
// assignment into plugin.Environment.
func TestABootedWorkerSuspendsAnExpiredTrial(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the worker binary")
	}
	c := deployDialect{"postgres", "CLEAT_TEST_POSTGRES", "postgres"}
	if c.admin() == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}
	bin, ownerDSN, owner := buildWorker(t, c)
	_ = masterKey(t)
	appDSN := pgAppRoleDSN(t, owner, ownerDSN)

	ctx := context.Background()
	past := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(1 * time.Hour)

	// The subject: the default tenant, which already exists, with an expired,
	// unhandled trial.
	expiredTenant := uuid.MustParse(engine.DefaultTenantUUID)

	// The control: a second, real tenant whose trial has NOT expired. Seeded
	// directly rather than through auth.TenantStore.CreateTenant (Postgres-
	// only, but this test is Postgres-only anyway) -- a minimal row is all
	// SetTenantSuspended's own WHERE tenant_id = $1 needs to find it.
	liveTenant := uuid.New()
	if _, err := owner.ExecContext(ctx,
		`INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES ($1, $2, $2)`,
		liveTenant, "cleat-2534-control-"+liveTenant.String()); err != nil {
		t.Fatalf("seed control tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = owner.ExecContext(context.Background(), `DELETE FROM admin.tenants WHERE tenant_id = $1`, liveTenant)
	})

	seedTrial := func(tenantID uuid.UUID, expiresAt time.Time) {
		t.Helper()
		if _, err := owner.ExecContext(ctx,
			`INSERT INTO tenant_trials (tenant_id, expires_at, handled) VALUES ($1, $2, false)`,
			tenantID, expiresAt); err != nil {
			t.Fatalf("seed tenant_trials for %s: %v", tenantID, err)
		}
		// buildWorker -> deployScratch gives this test its own fresh, dropped-
		// on-cleanup database, so this row cannot leak into another test's
		// run the way it can in engine/testutil's shared-DB multidb tests --
		// but it can still leak into a SECOND RUN of this test against the
		// same scratch database if a prior run's own cleanup did not
		// complete (a killed test process, a Ctrl-C). Cleaning up explicitly
		// costs nothing and removes that one remaining way to contaminate a
		// fixture.
		t.Cleanup(func() {
			_, _ = owner.ExecContext(context.Background(), `DELETE FROM tenant_trials WHERE tenant_id = $1`, tenantID)
		})
	}

	isSuspended := func(tenantID uuid.UUID) (bool, error) {
		var suspended bool
		if err := owner.QueryRowContext(ctx,
			`SELECT suspended FROM admin.tenants WHERE tenant_id = $1`, tenantID).Scan(&suspended); err != nil {
			return false, fmt.Errorf("reading suspended for %s: %w", tenantID, err)
		}
		return suspended, nil
	}

	// Migrations first, so tenant_trials exists before this test seeds it --
	// buildWorker's own --migrate-only pass runs core AND plugin migrations
	// (plugin.RunMigrations is called from the same startup path RunMigrations
	// core does; see setup.go).
	seedTrial(expiredTenant, past)
	seedTrial(liveTenant, future)

	observe := func() error {
		// A liveness deadline, not a timing assertion, matching the two
		// neighbouring boot tests in this package: Run sweeps once
		// immediately on startup (plugins/tenantlifecycle/background.go,
		// matching oauthprovider's own), so a row present at boot is caught
		// well inside this deadline without waiting for the 60s ticker.
		deadline := time.Now().Add(30 * time.Second)
		for {
			suspended, err := isSuspended(expiredTenant)
			if err != nil {
				return err
			}
			if suspended {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the booted worker never suspended the tenant with an expired trial "+
					"(%s, tenant_trials.expires_at in the past, handled=false) within 30s. Either "+
					"pluginEnv's SetTenantSuspended assignment is gone -- a nil grant is logged and "+
					"the row is left for the next tick, which never arrives here as a suspension -- "+
					"or the background loop that calls it never started. Both leave the suite green "+
					"without this test", expiredTenant)
			}
			time.Sleep(500 * time.Millisecond)
		}

		// The control, asserted only once the sweep has demonstrably run:
		// the tenant whose trial has NOT expired must not be suspended. A
		// worker that suspends every tenant regardless of expiry would pass
		// the assertion above and fail this one.
		if suspended, err := isSuspended(liveTenant); err != nil {
			return err
		} else if suspended {
			return fmt.Errorf("the sweep suspended a tenant whose trial has NOT expired (%s, "+
				"expires_at an hour in the future). Its predicate is `expires_at < now() AND "+
				"handled = false`; this row is half of what makes the assertion above mean "+
				"anything -- a worker that suspends unconditionally would satisfy it too", liveTenant)
		}
		return nil
	}

	args := []string{"--driver=postgres", "--db=" + appDSN, "--migrate-db=" + ownerDSN}
	var key string
	var probeErr error
	ok, out := hostMatchServes(t, bin, args, &key, func(_, _ string) { probeErr = observe() })
	if !ok {
		t.Fatalf("the worker did not boot:\n%s", out)
	}
	if probeErr != nil {
		t.Fatalf("%v\n\nWorker output:\n%s", probeErr, out)
	}
}
