package blobstore

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// TestAllInFlightWorkflowIDsMSSQL_RejectsACrossTenantContext is the
// known-positive for the guard in background.go: allInFlightWorkflowIDsMSSQL
// must refuse a ctx already marked by AcrossAllTenants rather than let
// ForTenant's silent no-op (see plugin.IsCrossTenant's doc) read every
// tenant's rows under the bypass instead. cleat#2141.
//
// No database needed: the guard must fire before p.db is ever touched, so a
// nil p.db proves it -- reaching past the guard would panic on the nil
// pointer, not return an error.
func TestAllInFlightWorkflowIDsMSSQL_RejectsACrossTenantContext(t *testing.T) {
	p := &Plugin{dialect: plugin.DialectMSSQL, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	marked := plugin.AcrossAllTenants(context.Background(), "test: deliberately marked")
	if _, err := p.allInFlightWorkflowIDsMSSQL(marked); err == nil {
		t.Fatal("allInFlightWorkflowIDsMSSQL(marked ctx) returned no error, want one -- it " +
			"must refuse a cross-tenant-marked ctx instead of letting ForTenant no-op past it")
	}
}

// TestAllInFlightWorkflowIDsMSSQL_AcceptsAnUnmarkedContext is the negative
// control: the guard above must not fire on the ordinary case, or the
// known-positive would be meaningless. p.db is still nil here, so an
// unmarked context must be rejected for a DIFFERENT reason (no db) --
// checked by recovering the panic, not by an early error return.
func TestAllInFlightWorkflowIDsMSSQL_AcceptsAnUnmarkedContext(t *testing.T) {
	p := &Plugin{dialect: plugin.DialectMSSQL, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	defer func() {
		if recover() == nil {
			t.Fatal("allInFlightWorkflowIDsMSSQL(unmarked ctx) did not reach p.db at all -- " +
				"the guard is rejecting more than cross-tenant-marked contexts")
		}
	}()
	_, _ = p.allInFlightWorkflowIDsMSSQL(context.Background())
}
