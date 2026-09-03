package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (s *PostgresStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("load wasm: begin: %w", err)
	}
	defer tx.Rollback()

	var wasmBytes []byte
	err = tx.QueryRowContext(ctx, `
		SELECT wasm_bytes FROM workflow_defs WHERE name = $1 AND version = $2
	`, defName, defVersion).Scan(&wasmBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("wasm not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("load wasm: %w", err)
	}
	return wasmBytes, tx.Commit()
}

// GetWASMLength returns the byte length of the stored WASM binary.

func (s *PostgresStore) GetWASMLength(ctx context.Context, defName string, defVersion int) (int64, error) {
	// In an RLS transaction, like LoadWASM above, rather than on a bare
	// connection.
	//
	// This ran on s.db with no cleat.tenant_id set, so on the role the engine
	// is meant to run as (migrations/postgres/005_app_role.sql, 1.10) the
	// policy on workflow_defs could not be evaluated and the call failed with
	//
	//	pq: invalid input syntax for type uuid: "" (22P02)
	//
	// every time. That is not a leak but it is not harmless either: the one
	// caller is Worker.loadWASM, which uses the length as a cache-freshness
	// check on every cache hit, and treats an error as "keep serving the
	// cache". So on PostgreSQL a redeployed definition was never picked up by
	// a worker with a warm cache -- the staleness check could not fire.
	// IMPROVEMENT-PLAN 3.11.
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("get wasm length: begin: %w", err)
	}
	defer tx.Rollback()

	var length int64
	if err := tx.QueryRowContext(ctx,
		`SELECT length(wasm_bytes) FROM workflow_defs WHERE name = $1 AND version = $2`,
		defName, defVersion).Scan(&length); err != nil {
		return 0, err
	}
	return length, tx.Commit()
}

// TraceWorkflow sets the W3C trace_id on a workflow instance.

func (s *PostgresStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("trace workflow: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET trace_id = $2 WHERE id = $1
	`, workflowID, traceID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveTenantFromAPIKey looks up a tenant UUID by API key hash.

func (s *PostgresStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) {
	var tenantID uuid.UUID
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id FROM admin.tenant_api_keys
		 WHERE key_hash = $1 AND revoked_at IS NULL`, keyHash).Scan(&tenantID)
	if err != nil {
		return uuid.Nil, err
	}
	return tenantID, nil
}

// LoadWorkflowConfig returns configuration for a workflow definition.

func (s *PostgresStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("load workflow config: begin: %w", err)
	}
	defer tx.Rollback()

	var maxHistoryLength int
	err = tx.QueryRowContext(ctx, `
		SELECT max_history_length FROM workflow_defs WHERE name = $1 AND version = $2
	`, defName, defVersion).Scan(&maxHistoryLength)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("workflow def not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return 0, fmt.Errorf("load workflow config: %w", err)
	}
	return maxHistoryLength, tx.Commit()
}

// LoadDAGSpec returns the dag_spec JSON for a workflow definition, or nil if none.

func (s *PostgresStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("load dag_spec: begin: %w", err)
	}
	defer tx.Rollback()

	var spec json.RawMessage
	err = tx.QueryRowContext(ctx, `
		SELECT dag_spec FROM workflow_defs WHERE name = $1 AND version = $2
	`, defName, defVersion).Scan(&spec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("workflow def not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("load dag_spec: %w", err)
	}
	return spec, tx.Commit()
}

// ListVersions returns all deployed versions of a workflow.

func (s *PostgresStore) ListVersions(ctx context.Context, defName string) ([]int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("list versions: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT version FROM workflow_defs WHERE name = $1 ORDER BY version DESC
	`, defName)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()

	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return versions, tx.Commit()
}

// DeployWorkflowDef inserts or updates a workflow definition.

func (s *PostgresStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("deploy workflow def: begin: %w", err)
	}
	defer tx.Rollback()

	// json.Marshal of a nil map returns the four bytes "null", not nil, so the
	// guard this replaced -- `if pluginDepsJSON == nil` -- could never fire and
	// every workflow that declares no plugin dependencies stored the literal
	// `null`. PostgreSQL JSONB and MySQL JSON both accept a bare JSON scalar, so
	// nothing noticed; SQL Server's ISJSON does not (`ISJSON('null')` = 0),
	// which is how the CHECK constraint in migrations/mssql/036 found it.
	//
	// An error is folded in for the same reason the default exists: the column
	// is NOT NULL DEFAULT '{}' on all three dialects, so "no dependencies" has
	// one spelling and it is not `null`.
	pluginDepsJSON, err := json.Marshal(def.PluginDeps)
	if err != nil || len(pluginDepsJSON) == 0 || string(pluginDepsJSON) == "null" {
		pluginDepsJSON = []byte("{}")
	}

	// The definition records the tenant that deployed it.
	//
	// This line used to be a literal `tenantID :=
	// "00000000-0000-0000-0000-000000000000"`, ignoring s.tenantID, so every
	// definition every tenant deployed was written as the default tenant's --
	// and this table's RLS policy admits the default tenant by design
	// (`tenant_id = cleat.assert_tenant_set() OR tenant_id = '000…'`, for
	// shared definitions), so every definition was a shared definition.
	// IMPROVEMENT-PLAN 3.12.
	tenantID := s.tenantID

	// No ownership check: under (tenant_id, name, version) another tenant's
	// definition of the same name is a different row, so there is nothing to
	// adjudicate. The ON CONFLICT below can now only fire for this tenant's own
	// redeploy of the same version, which is an ordinary upsert.
	// IMPROVEMENT-PLAN 3.77.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, plugin_deps, deprecated, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, name, version) DO UPDATE SET
			wasm_bytes = EXCLUDED.wasm_bytes,
			abi_version = EXCLUDED.abi_version,
			min_version = EXCLUDED.min_version,
			plugin_deps = EXCLUDED.plugin_deps,
			deprecated = EXCLUDED.deprecated
	`, def.Name, def.Version, def.WASMBytes, def.ABIVersion, def.MinVersion, pluginDepsJSON, def.Deprecated, tenantID)
	if err != nil {
		return fmt.Errorf("deploy workflow def: %w", err)
	}
	return tx.Commit()
}

// ListWorkflowDefs returns all versions of a workflow, ordered by version DESC.
// If name is empty, returns all workflow definitions across all workflows.

func (s *PostgresStore) ListWorkflowDefs(ctx context.Context, name string) ([]WorkflowDef, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workflow defs: begin: %w", err)
	}
	defer tx.Rollback()

	var rows *sql.Rows
	if name == "" {
		rows, err = tx.QueryContext(ctx, `
			SELECT name, version, abi_version, min_version, plugin_deps, created_at, deprecated
			FROM workflow_defs ORDER BY name, version DESC
		`)
	} else {
		rows, err = tx.QueryContext(ctx, `
			SELECT name, version, abi_version, min_version, plugin_deps, created_at, deprecated
			FROM workflow_defs WHERE name = $1 ORDER BY version DESC
		`, name)
	}
	if err != nil {
		return nil, fmt.Errorf("list workflow defs: %w", err)
	}
	defer rows.Close()

	var defs []WorkflowDef
	for rows.Next() {
		var def WorkflowDef
		var pluginDepsRaw []byte
		var createdAt time.Time
		if err := rows.Scan(&def.Name, &def.Version, &def.ABIVersion, &def.MinVersion,
			&pluginDepsRaw, &createdAt, &def.Deprecated); err != nil {
			return nil, fmt.Errorf("scan workflow def: %w", err)
		}
		def.CreatedAt = createdAt
		if len(pluginDepsRaw) > 0 {
			def.PluginDeps = decodePluginDeps(s.log(), pluginDepsRaw, def.Name, def.Version)
		}
		if def.PluginDeps == nil {
			def.PluginDeps = make(map[string]string)
		}
		defs = append(defs, def)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return defs, tx.Commit()
}

// GetWorkflowDef returns a single workflow definition by name and version.

func (s *PostgresStore) GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get workflow def: begin: %w", err)
	}
	defer tx.Rollback()

	var def WorkflowDef
	var pluginDepsRaw []byte
	var wasmBytes []byte
	var createdAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT name, version, wasm_bytes, abi_version, min_version, plugin_deps, created_at, deprecated
		FROM workflow_defs WHERE name = $1 AND version = $2
	`, name, version).Scan(&def.Name, &def.Version, &wasmBytes, &def.ABIVersion,
		&def.MinVersion, &pluginDepsRaw, &createdAt, &def.Deprecated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow def: %w", err)
	}
	def.WASMBytes = wasmBytes
	def.CreatedAt = createdAt
	if len(pluginDepsRaw) > 0 {
		def.PluginDeps = decodePluginDeps(s.log(), pluginDepsRaw, name, version)
	}
	if def.PluginDeps == nil {
		def.PluginDeps = make(map[string]string)
	}
	return &def, tx.Commit()
}

// MarkVersionDeprecated sets the deprecated flag on a workflow version.

func (s *PostgresStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("mark version deprecated: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_defs SET deprecated = $3 WHERE name = $1 AND version = $2
	`, name, version, deprecated)
	if err != nil {
		return fmt.Errorf("mark version deprecated: %w", err)
	}
	return tx.Commit()
}

// PurgeWorkflowDef permanently deletes a workflow definition.

func (s *PostgresStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("purge workflow def: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		DELETE FROM workflow_defs WHERE name = $1 AND version = $2
	`, name, version)
	if err != nil {
		return fmt.Errorf("purge workflow def: %w", err)
	}
	return tx.Commit()
}

// CountActiveInstances returns the number of ready or running instances for a version.

func (s *PostgresStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("count active instances: begin: %w", err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE def_name = $1 AND def_version = $2
		  AND status IN ('ready', 'running')
	`, name, version).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count active instances: %w", err)
	}
	return count, tx.Commit()
}

// GetActiveInstanceCountsByVersion returns a map of "name:version" -> count.

func (s *PostgresStore) GetActiveInstanceCountsByVersion(ctx context.Context) (map[string]int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get active instance counts: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT def_name, def_version, COUNT(*) as cnt
		FROM workflow_instances
		WHERE status IN ('ready', 'running')
		GROUP BY def_name, def_version
	`)
	if err != nil {
		return nil, fmt.Errorf("get active instance counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var name string
		var version, count int
		if err := rows.Scan(&name, &version, &count); err != nil {
			return nil, fmt.Errorf("scan active instance count: %w", err)
		}
		key := name + ":" + fmt.Sprintf("%d", version)
		counts[key] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration: %w", err)
	}
	return counts, tx.Commit()
}

// Heartbeat updates the heartbeat timestamp.

func (s *PostgresStore) ResolveLatestVersion(ctx context.Context, defName string) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve latest version: begin: %w", err)
	}
	defer tx.Rollback()

	var version int
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) FROM workflow_defs
		WHERE name = $1 AND NOT deprecated AND tenant_id = $2
	`, defName, s.tenantID).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("resolve latest version: %w", err)
	}
	return version, tx.Commit()
}

// ValidateVersion checks whether a specific workflow definition version
// exists and is not deprecated. Returns true if the version can be used.
//
//	SQL: SELECT EXISTS(SELECT 1 FROM workflow_defs
//	     WHERE name = $1 AND version = $2 AND NOT deprecated)

func (s *PostgresStore) ValidateVersion(ctx context.Context, defName string, defVersion int) (bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return false, fmt.Errorf("validate version: begin: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	err = tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM workflow_defs
			WHERE name = $1 AND version = $2 AND NOT deprecated
		)
	`, defName, defVersion).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("validate version: %w", err)
	}
	return exists, tx.Commit()
}

// ---- Tag methods (deployment channels) ----

// SetWorkflowTag assigns a tag to a specific version.
// Uses INSERT ... ON CONFLICT DO UPDATE so reassigning a tag updates in place.
