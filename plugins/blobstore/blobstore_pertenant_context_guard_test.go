package blobstore

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
// allInFlightWorkflowIDsMSSQL returns once past the guard (an empty map, no
// error) instead of on a nil p.db panic, which proves only that some code
// path was reached, not which one or with what outcome. cleat#2181 (b).
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

// TestAllInFlightWorkflowIDsMSSQL_RejectsACrossTenantContext is the
// known-positive for the guard in background.go: allInFlightWorkflowIDsMSSQL
// must refuse a ctx already marked by AcrossAllTenants rather than let
// ForTenant's silent no-op (see plugin.IsCrossTenant's doc) read every
// tenant's rows under the bypass instead. cleat#2141.
//
// Uses emptyTenantListDB rather than a nil p.db: if the guard were ever
// removed, this must fail as "returned no error, want one" -- an assertion
// pointing straight at the missing guard -- not panic on a nil pointer
// somewhere inside plugin.AllTenantIDs, which would still fail the test but
// say nothing about why.
func TestAllInFlightWorkflowIDsMSSQL_RejectsACrossTenantContext(t *testing.T) {
	p := &Plugin{dialect: plugin.DialectMSSQL, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), db: emptyTenantListDB{}}
	marked := plugin.AcrossAllTenants(context.Background(), "test: deliberately marked")
	if _, err := p.allInFlightWorkflowIDsMSSQL(marked); err == nil {
		t.Fatal("allInFlightWorkflowIDsMSSQL(marked ctx) returned no error, want one -- it " +
			"must refuse a cross-tenant-marked ctx instead of letting ForTenant no-op past it")
	}
}

// TestAllInFlightWorkflowIDsMSSQL_AcceptsAnUnmarkedContext is the negative
// control: the guard above must not fire on the ordinary case, or the
// known-positive would be meaningless. With emptyTenantListDB standing in
// for a database with no tenants, an unmarked context must reach all the way
// through and return an empty map with no error -- checked by that explicit
// result, not by recovering a nil-pointer panic (which used to be this
// test's only signal, and would have passed just as well if the function
// returned early for some unrelated wrong reason).
func TestAllInFlightWorkflowIDsMSSQL_AcceptsAnUnmarkedContext(t *testing.T) {
	p := &Plugin{dialect: plugin.DialectMSSQL, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), db: emptyTenantListDB{}}
	ids, err := p.allInFlightWorkflowIDsMSSQL(context.Background())
	if err != nil {
		t.Fatalf("allInFlightWorkflowIDsMSSQL(unmarked ctx) = %v, want nil -- the guard must not fire on an ordinary context", err)
	}
	if len(ids) != 0 {
		t.Fatalf("allInFlightWorkflowIDsMSSQL(unmarked ctx) returned %d ids from a database with no tenants, want 0", len(ids))
	}
}
