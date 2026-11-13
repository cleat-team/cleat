package main

import (
	"context"
	"database/sql"

	"github.com/cleat-team/cleat/engine"
)

// tenantCreationGuard answers whether creating one more tenant is refused on this worker's connection. A
// nil guard refuses nothing: every dialect but PostgreSQL, where the row-level security check does not
// run. It returns the refusal, or an error when it could not tell.
type tenantCreationGuard func(ctx context.Context) (*engine.RLSMultiTenantRefusal, error)

// multiTenantRefusalFor applies cleat#3772's rule to a connection whose RLS check has already run: it
// returns the refusal when reasons is non-empty (the connection is not subject to row-level security) and
// the database holds, or with creating would hold, more than one tenant.
//
// It counts only when there is something to refuse. A connection that is subject to the policies never
// reads admin.tenants here, so a worker on cleat_app pays nothing for it.
func multiTenantRefusalFor(ctx context.Context, db *sql.DB, reasons []engine.RLSBypassReason, creating bool) (*engine.RLSMultiTenantRefusal, error) {
	if len(reasons) == 0 {
		return nil, nil
	}
	role, err := engine.CurrentRole(ctx, db)
	if err != nil {
		return nil, err
	}
	n, err := engine.CountTenants(ctx, db)
	if err != nil {
		return nil, err
	}
	if creating {
		n++
	}
	return engine.DecideRLSMultiTenant(role, reasons, n, creating), nil
}

// newTenantCreationGuard builds the guard for a PostgreSQL connection. It interrogates the connection
// again on every call rather than trusting what startup saw, so it is right for a role changed under a
// running worker, and so that it applies under -rls-check=off, which skips the startup warning but not
// this.
func newTenantCreationGuard(db *sql.DB) tenantCreationGuard {
	return func(ctx context.Context) (*engine.RLSMultiTenantRefusal, error) {
		reasons, err := engine.CheckRLSEnforced(ctx, db)
		if err != nil {
			return nil, err
		}
		return multiTenantRefusalFor(ctx, db, reasons, true)
	}
}
