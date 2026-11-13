package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3766: replay and debug read a run's history and its definition through a
// store scoped to ONE tenant, and the run they were asked about may belong to
// another.
//
// main opens a single store, for defaultTenantID, before it dispatches. Neither
// command takes a tenant, but the run's own row names it, and
// loadWorkflowInstance reads that row by id without a tenant scope. Since #3740
// every PostgreSQL read of a definition, and since before that of an event
// history, carries the store's tenant, so a run of any other tenant was read
// through the wrong one: its definition came back as the default tenant's of the
// same name and version, or "not found", and its history as empty.
//
// The tenant comes from the run, not from a flag. A flag would add a way to name
// a pair that does not match; the instance id already determines the tenant.

// tenantStoreOpener opens a store scoped to a tenant other than the default.
// main sets it from the store factory it already built; it is nil in tests that
// never meet a run of another tenant.
var tenantStoreOpener func(ctx context.Context, tenantID string) (engine.WorkflowStore, io.Closer, error)

// storeForInstance returns the store to read inst's history and definition
// through, and a function to release it. For a run of the default tenant, or one
// whose tenant the row did not carry, that is the store main opened and releasing
// it is a no-op.
func storeForInstance(ctx context.Context, def engine.WorkflowStore, d dialect, inst *engine.WorkflowInstance) (engine.WorkflowStore, func(), error) {
	noop := func() {}
	tenant := strings.ToLower(strings.TrimSpace(inst.TenantID))
	if tenant == "" || tenant == defaultTenantID {
		return def, noop, nil
	}
	if _, err := uuid.Parse(tenant); err != nil {
		return nil, noop, fmt.Errorf("workflow %q names tenant %q, which is not a UUID", inst.ID, inst.TenantID)
	}
	// MySQL keeps each tenant in a database of its own, and OpenStore for a tenant
	// whose database is missing CREATES it (MySQLStoreFactory.CreateTenantDatabase).
	// A command that only reads must not do that, and cannot tell "missing" from
	// "present" first, so it does not open one at all.
	if d.name == "mysql" {
		return nil, noop, fmt.Errorf("workflow %q belongs to tenant %s. MySQL keeps each tenant's data in a database of its own, "+
			"and cleatctl reads through the default tenant's only, so it cannot read this run (opening another tenant's store "+
			"on MySQL would create its database)", inst.ID, tenant)
	}
	if tenantStoreOpener == nil {
		return nil, noop, fmt.Errorf("workflow %q belongs to tenant %s, and no store can be opened for it", inst.ID, tenant)
	}
	st, closer, err := tenantStoreOpener(ctx, tenant)
	if err != nil {
		return nil, noop, fmt.Errorf("open the store of tenant %s, which owns workflow %q: %w", tenant, inst.ID, err)
	}
	return st, func() { _ = closer.Close() }, nil
}

// instanceTenantNote is the header line replay and debug print when the run is
// not the default tenant's, so the operator can see which tenant's definition
// and history are on screen. Empty for the default tenant: the output for the
// common case does not change.
func instanceTenantNote(inst *engine.WorkflowInstance) string {
	tenant := strings.ToLower(strings.TrimSpace(inst.TenantID))
	if tenant == "" || tenant == defaultTenantID {
		return ""
	}
	return fmt.Sprintf("  Tenant: %s\n", tenant)
}

// describeInstanceLoadError is what replay and debug say when the run cannot be
// loaded. The query is not scoped to a tenant, so what a connection can read
// depends on the connection:
//
//   - one that bypasses row-level security (a superuser, BYPASSRLS) sees every
//     tenant's runs, and "no rows" means there is no such run in THIS database;
//   - one the policy applies to has no tenant context here, and the policy
//     refuses the read outright (cleat.assert_tenant_set raises P0001, "tenant_id
//     is not set") rather than returning no rows.
//
// Neither says "not found" in the sense an operator means, so the message says
// which it was. And cleatctl reads one database: on a sharded deployment the run
// may be on another shard, which this command cannot reach.
func describeInstanceLoadError(id string, err error) string {
	msg := fmt.Sprintf("error loading workflow instance %q: %v", id, err)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		msg += fmt.Sprintf("\n  workflow %q was not found, or is not visible to this connection's tenant. A run of another "+
			"tenant is visible only to a connection that bypasses row-level security (a superuser or a role with "+
			"BYPASSRLS). cleatctl reads the one --db database and has no sharded store, so a run on another shard "+
			"is not found here either", id)
	case strings.Contains(err.Error(), "tenant_id is not set"):
		msg += "\n  this connection is subject to row-level security and has no tenant context, so it cannot read a run " +
			"by id: the run may exist but is not visible to this connection's tenant. Use a --db connection that " +
			"bypasses row-level security (a superuser or a role with BYPASSRLS). cleatctl has no sharded store, so a " +
			"run on another shard is not visible here either"
	}
	return msg
}

func noteTenantOnStdout(inst *engine.WorkflowInstance) {
	if n := instanceTenantNote(inst); n != "" {
		fmt.Fprint(os.Stdout, n)
	}
}
