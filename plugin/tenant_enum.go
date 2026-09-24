package plugin

import (
	"context"
	"fmt"
)

// AllTenantIDs lists every tenant in admin.tenants, suspended ones included.
// cleat#2125.
//
// EVERY TENANT, NOT JUST THE ONES ACCEPTING NEW WORK. A suspended tenant can
// still hold rows a background sweep must account for -- workflow_instances,
// task_queue, workflow_blob_refs -- so this must not narrow to tenants new
// work is routed to. engine/tenant_secrets.go's CountSecrets enumerates the
// same way for the same reason (secrets under a suspended tenant are still
// sealed ciphertext that needs accounting for); this is the version a PLUGIN
// can reach, since that one is unexported and reads a raw *sql.DB a
// SecretStore holds and no plugin does.
//
// admin.tenants carries no row-level security on any dialect (see
// CountSecrets' allTenantIDs, which this mirrors), so ctx needs no tenant or
// cross-tenant marker for this call -- pass the plugin's ordinary background
// context.
func AllTenantIDs(ctx context.Context, db PluginDB, dialect Dialect) ([]string, error) {
	var query string
	switch dialect {
	case DialectMySQL:
		query = `SELECT tenant_id FROM tenants`
	case DialectMSSQL:
		query = `SELECT CONVERT(varchar(36), tenant_id) FROM admin.tenants`
	default:
		query = `SELECT tenant_id FROM admin.tenants`
	}
	rows, err := db.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list tenants: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	return ids, nil
}
