# Multi-Tenancy in Cleat

## Tenant Resolution Modes

Cleat supports three tenant resolution modes via the `--tenant-resolver` flag:

1. **`single-tenant`** (default): All requests use the zero UUID. No per-tenant isolation.

2. **`header:<name>`**: The tenant UUID is extracted from the named HTTP header.
   Example: `--tenant-resolver=header:X-Tenant-ID`

3. **`api-key`**: The tenant is derived from the API key via `Authorization: Bearer <key>`.
   The key is SHA-256 hashed and looked up in `tenant_api_keys`.

## Tenant Propagation

1. HTTP request arrives → auth middleware resolves tenant → stored in request context
2. Workflow started → tenant stored in `workflow_instances.tenant_id`
3. Workflow executes → engine injects tenant into `plugin.CallContext`
4. Plugin host functions → extract tenant via `plugin.CallContextFromContext(ctx)`

## Row-Level Security

PostgreSQL deployments use RLS policies scoped by `cleat.tenant_id` session variable.
MySQL and SQL Server use per-tenant databases or application-level filtering.
