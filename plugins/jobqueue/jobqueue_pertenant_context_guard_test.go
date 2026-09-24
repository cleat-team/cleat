package jobqueue

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// TestSweepAbandonedJobsPerTenant_RejectsACrossTenantContext is the
// known-positive for the guard in background.go: sweepAbandonedJobsPerTenant
// must refuse a ctx already marked by AcrossAllTenants rather than let
// ForTenant's silent no-op (see plugin.IsCrossTenant's doc) run every
// tenant's statement under the bypass instead. cleat#2141.
//
// No database needed: the guard must fire before p.db is ever touched, so a
// nil p.db proves it -- reaching past the guard would panic on the nil
// pointer, not return -1.
func TestSweepAbandonedJobsPerTenant_RejectsACrossTenantContext(t *testing.T) {
	p := &Plugin{dialect: plugin.DialectMSSQL, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	marked := plugin.AcrossAllTenants(context.Background(), "test: deliberately marked")
	if n := p.sweepAbandonedJobsPerTenant(marked); n != -1 {
		t.Fatalf("sweepAbandonedJobsPerTenant(marked ctx) = %d, want -1 -- it must refuse a "+
			"cross-tenant-marked ctx instead of letting ForTenant no-op past it", n)
	}
}

// TestSweepAbandonedJobsPerTenant_AcceptsAnUnmarkedContext is the negative
// control: the guard above must not fire on the ordinary case, or the
// known-positive would be meaningless. p.db is still nil here, so a real
// call would panic; a plain context.Background() must be REJECTED FOR A
// DIFFERENT REASON (no db) rather than by the cross-tenant guard, which this
// checks by recovering the panic and confirming it, not a returned -1.
func TestSweepAbandonedJobsPerTenant_AcceptsAnUnmarkedContext(t *testing.T) {
	p := &Plugin{dialect: plugin.DialectMSSQL, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	defer func() {
		if recover() == nil {
			t.Fatal("sweepAbandonedJobsPerTenant(unmarked ctx) did not reach p.db at all -- " +
				"the guard is rejecting more than cross-tenant-marked contexts")
		}
	}()
	p.sweepAbandonedJobsPerTenant(context.Background())
}
