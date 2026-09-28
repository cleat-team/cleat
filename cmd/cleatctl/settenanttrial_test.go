package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	mssqldriver "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/tenantlifecycle"
	"github.com/google/uuid"
)

// TestSetTenantTrialWorksOnEveryDialect is the acceptance test cleat-review
// asked for on cleat#2534 (PR #2590): a trial set via cleatctl takes effect
// (is readable back, and correctly chooses INSERT vs UPDATE) on all three
// dialects -- modeled directly on TestQuotaCommandWorksOnEveryDialect
// (quota_test.go), which is the shape cleat-review named as the one to copy.
//
// This is the test that exercises tenantTrialConnFor's MSSQL branch at all.
// Every verification before this test existed ran on Postgres alone, where
// tenantTrialConnFor returns the bare pool -- the SQL Server
// SESSION_CONTEXT('tenant_id') pinning (setMSSQLTenantKey) had never once
// executed.
func TestSetTenantTrialWorksOnEveryDialect(t *testing.T) {
	for _, tc := range []struct {
		name string
		td   testutil.Dialect
		d    dialect
	}{
		{"postgres", testutil.DialectPostgres, dialectPostgres},
		{"mysql", testutil.DialectMySQL, dialectMySQL},
		{"mssql", testutil.DialectMSSQL, dialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.TestDB(t, tc.td)
			ctx := context.Background()

			loaded := []*plugin.LoadedPlugin{{Plugin: tenantlifecycle.New(), Healthy: true}}
			if err := plugin.RunMigrations(ctx, db, tc.d.query, nil, loaded); err != nil {
				t.Fatalf("apply tenantlifecycle migrations on %s: %v", tc.name, err)
			}

			// tenant_trials carries no FK to tenants (migrations.go), so an
			// invented UUID is fine -- the point of this test is the write
			// path, not lookupTenantName's separate admin.tenants read,
			// which runSetTenantTrial itself already does before ever
			// calling writeTenantTrial.
			tenant := uuid.New()

			// tenantTrialConnFor, not db directly -- see this test's own doc
			// comment above. Falsify by swapping this for `db` directly and
			// watching the mssql subtest go red (see
			// TestSetTenantTrialFailsClosedWithoutMSSQLSessionPinning below,
			// which does exactly that as a standing regression guard rather
			// than a one-off manual check).
			exec, closeExec, err := tenantTrialConnFor(ctx, db, tc.d, tenant.String())
			if err != nil {
				t.Fatalf("tenantTrialConnFor: %v", err)
			}
			defer closeExec()

			got, err := readTenantTrial(ctx, exec, tc.d, tenant)
			if err != nil {
				t.Fatalf("reading a tenant with no trial row: %v", err)
			}
			if got.existed {
				t.Fatalf("a fresh tenant already has a trial row: %+v", got)
			}

			// First write: create. expiresAt is truncated to whole seconds --
			// MySQL's DATETIME(6) and Postgres/MSSQL's timestamp types don't
			// need it, but comparing round-tripped values with sub-second
			// jitter from time.Now() is not what this test is about.
			first := time.Now().UTC().AddDate(0, 0, 14).Truncate(time.Second)
			if err := writeTenantTrial(ctx, exec, tc.d, tenant, first); err != nil {
				t.Fatalf("creating a trial: %v", err)
			}

			got2, err := readTenantTrial(ctx, exec, tc.d, tenant)
			if err != nil {
				t.Fatalf("reading after create: %v", err)
			}
			if !got2.existed {
				t.Fatalf("readTenantTrial after create: no row")
			}
			if got2.handled {
				t.Fatalf("a freshly-created trial is already handled=true")
			}
			if !got2.expiresAt.Equal(first) {
				t.Fatalf("readTenantTrial after create: expires_at = %v, want %v", got2.expiresAt, first)
			}

			// Simulate the sweep marking it handled (plugins/tenantlifecycle/
			// background.go's markTrialHandledSQL is unexported and does the
			// same thing; the statement text is duplicated here rather than
			// exported for one test to reach), so the update below has
			// something real to reset -- an update over a row that was
			// never handled cannot tell "handled stayed false" from "handled
			// was reset to false". Bound as an arg, not spelled per dialect
			// in the SQL text: writeTenantTrial already binds a bool this
			// way for the same statement (`handled = $2`, `false`) and it
			// works on all three, so there's nothing dialect-specific to
			// get right here.
			markStmt, markArgs, err := tc.d.rebindArgs(
				`UPDATE tenant_trials SET handled = $1 WHERE tenant_id = $2`, true, tenant)
			if err != nil {
				t.Fatalf("rebinding the mark-handled statement: %v", err)
			}
			if _, err := exec.ExecContext(ctx, markStmt, markArgs...); err != nil {
				t.Fatalf("marking the trial handled (simulating a prior sweep): %v", err)
			}
			if handled, err := readTenantTrial(ctx, exec, tc.d, tenant); err != nil {
				t.Fatalf("reading after marking handled: %v", err)
			} else if !handled.handled {
				t.Fatalf("markTrialHandledSQL did not set handled=true; test fixture is broken")
			}

			// Second write: update, extending the trial. Per
			// setTenantTrialUsage's own documented contract ("Extending an
			// already-expired-and-suspended tenant's trial resets it back
			// into the unhandled state"), this must flip handled back to
			// false -- not just move expires_at.
			second := first.Add(7 * 24 * time.Hour)
			if err := writeTenantTrial(ctx, exec, tc.d, tenant, second); err != nil {
				t.Fatalf("updating a trial: %v", err)
			}
			got3, err := readTenantTrial(ctx, exec, tc.d, tenant)
			if err != nil {
				t.Fatalf("reading after update: %v", err)
			}
			if !got3.expiresAt.Equal(second) {
				t.Fatalf("readTenantTrial after update: expires_at = %v, want %v", got3.expiresAt, second)
			}
			if got3.handled {
				t.Fatalf("readTenantTrial after update: handled = true, want false " +
					"(extending a trial must reset it back into the unhandled state, " +
					"per this command's own usage text)")
			}
		})
	}
}

// TestSetTenantTrialFailsClosedWithoutMSSQLSessionPinning is the
// falsification cleat-review asked for directly: "swap tenantTrialConnFor
// for the bare pool and watch the MSSQL arm go red." Rather than a one-off
// manual check, this pins it as a standing regression guard -- if a future
// refactor of tenantTrialConnFor (or of the security policy it exists to
// satisfy) ever makes the bare pool succeed on MSSQL, this test starts
// failing FOR THE WRONG REASON, which is exactly the signal that the
// falsification this PR performed manually needs to be re-run.
func TestSetTenantTrialFailsClosedWithoutMSSQLSessionPinning(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectMSSQL)
	ctx := context.Background()

	loaded := []*plugin.LoadedPlugin{{Plugin: tenantlifecycle.New(), Healthy: true}}
	if err := plugin.RunMigrations(ctx, db, dialectMSSQL.query, nil, loaded); err != nil {
		t.Fatalf("apply tenantlifecycle migrations: %v", err)
	}

	tenant := uuid.New()
	expiresAt := time.Now().UTC().AddDate(0, 0, 14)

	// db directly, deliberately bypassing tenantTrialConnFor's
	// SESSION_CONTEXT('tenant_id') pinning -- this is the "bare pool"
	// cleat-review's suggested falsification names. tenant_trials'
	// TenantScoped security policy has no bypass (migrations.go), so a
	// write through the bare pool must be refused.
	err := writeTenantTrial(ctx, db, dialectMSSQL, tenant, expiresAt)
	if err == nil {
		t.Fatalf("writeTenantTrial through the bare pool (no SESSION_CONTEXT) succeeded on MSSQL; " +
			"want the security policy to refuse it. Either tenant_trials' policy regressed, or " +
			"tenantTrialConnFor's pinning is no longer the only thing making set-tenant-trial work " +
			"on MSSQL -- re-run the falsification this test automates before trusting either")
	}

	// Not just ANY error -- cleat-review's finding: a missing table or a
	// connection problem would also make err non-nil and pass a bare
	// `err == nil` check without ever exercising the security policy at
	// all. 33504 is SQL Server's specific "target object ... has a block
	// predicate that conflicts with this operation" -- classified by
	// NUMBER, not by matching the message text, for the same reason
	// engine/mssql_errors.go's mssqlErrNumber gives: a substring match can
	// hit digits the driver interpolates from unrelated data (a workflow
	// id, a row count) rather than the error number itself.
	var msErr mssqldriver.Error
	if !errors.As(err, &msErr) || msErr.Number != 33504 {
		t.Fatalf("writeTenantTrial through the bare pool failed, but not with the block-predicate "+
			"refusal (33504) -- got %v. This test exists to prove the SECURITY POLICY is what "+
			"refuses the write, not some other failure (a missing table, a connection error) that "+
			"would pass a bare `err == nil` check without ever exercising it", err)
	}
}

// tenantTrial mirrors one row of tenant_trials, for reading it back here --
// runSetTenantTrial itself never reads expires_at/handled back, it only
// writes, so nothing in the CLI's own path needs this (cleat-review on
// #2590: moved out of settenanttrial.go, since only tests reference it).
// Mirrors quotaRow.
type tenantTrial struct {
	expiresAt time.Time
	handled   bool
	existed   bool
}

// readTenantTrial reads one tenant's trial row -- see tenantTrial.
func readTenantTrial(ctx context.Context, exec quotaExecer, d dialect, tenantID uuid.UUID) (tenantTrial, error) {
	stmt, stmtArgs, err := d.rebindArgs(
		`SELECT expires_at, handled FROM tenant_trials WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return tenantTrial{}, err
	}
	var tt tenantTrial
	err = exec.QueryRowContext(ctx, stmt, stmtArgs...).Scan(&tt.expiresAt, &tt.handled)
	if err == sql.ErrNoRows {
		return tenantTrial{}, nil
	}
	if err != nil {
		return tenantTrial{}, err
	}
	tt.existed = true
	return tt, nil
}
