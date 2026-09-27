package jobqueue

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// emptyTenantListDB is a plugin.PluginDB whose Query always succeeds with
// zero rows, standing in for admin.tenants holding no tenants. It exists so
// the context-guard tests below can assert on the REAL result
// sweepAbandonedJobsPerTenant returns once past the guard (0, the total over
// zero tenants) instead of on a nil p.db panic, which proves only that some
// code path was reached, not which one or with what outcome. cleat#2181 (b).
type emptyTenantListDB struct{}

func (emptyTenantListDB) Begin(ctx context.Context) (plugin.PluginTx, error) {
	return nil, nil
}

func (emptyTenantListDB) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	return 0, nil
}

func (emptyTenantListDB) Query(ctx context.Context, query string, args ...any) (plugin.Rows, error) {
	return emptyRows{}, nil
}

func (emptyTenantListDB) QueryRow(ctx context.Context, query string, args ...any) plugin.RowScanner {
	return nil
}

func (emptyTenantListDB) Ping(ctx context.Context) error { return nil }

// emptyRows is a plugin.Rows with no rows in it.
type emptyRows struct{}

func (emptyRows) Scan(dest ...any) error { return nil }
func (emptyRows) Next() bool             { return false }
func (emptyRows) Close() error           { return nil }
func (emptyRows) Err() error             { return nil }

// TestSweepAbandonedJobsPerTenant_RejectsACrossTenantContext is the
// known-positive for the guard in background.go: sweepAbandonedJobsPerTenant
// must refuse a ctx already marked by AcrossAllTenants rather than let
// ForTenant's silent no-op (see plugin.IsCrossTenant's doc) run every
// tenant's statement under the bypass instead. cleat#2141.
//
// Uses emptyTenantListDB rather than a nil p.db: if the guard were ever
// removed, this must fail as "= 0, want -1" -- an assertion pointing
// straight at the missing guard -- not panic on a nil pointer somewhere
// inside plugin.AllTenantIDs, which would still fail the test but say
// nothing about why.
func TestSweepAbandonedJobsPerTenant_RejectsACrossTenantContext(t *testing.T) {
	p := &Plugin{dialect: plugin.DialectMSSQL, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), db: emptyTenantListDB{}}
	marked := plugin.AcrossAllTenants(context.Background(), "test: deliberately marked")
	if n := p.sweepAbandonedJobsPerTenant(marked); n != -1 {
		t.Fatalf("sweepAbandonedJobsPerTenant(marked ctx) = %d, want -1 -- it must refuse a "+
			"cross-tenant-marked ctx instead of letting ForTenant no-op past it", n)
	}
}

// TestSweepAbandonedJobsPerTenant_AcceptsAnUnmarkedContext is the negative
// control: the guard above must not fire on the ordinary case, or the
// known-positive would be meaningless. With emptyTenantListDB standing in
// for a database with no tenants, an unmarked context must reach all the way
// through and return 0 (the total over zero tenants), not -1 -- checked by
// that explicit result, not by recovering a nil-pointer panic (which used to
// be this test's only signal, and would have passed just as well if the
// function returned early for some unrelated wrong reason).
func TestSweepAbandonedJobsPerTenant_AcceptsAnUnmarkedContext(t *testing.T) {
	p := &Plugin{dialect: plugin.DialectMSSQL, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), db: emptyTenantListDB{}}
	if n := p.sweepAbandonedJobsPerTenant(context.Background()); n != 0 {
		t.Fatalf("sweepAbandonedJobsPerTenant(unmarked ctx) = %d, want 0 -- the guard must not "+
			"fire on an ordinary context, and a database with no tenants sweeps nothing", n)
	}
}
