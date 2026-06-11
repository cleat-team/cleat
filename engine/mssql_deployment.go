package engine

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
)

// LoadWASM returns the compiled WASM bytes for a workflow definition.
func (s *MSSQLStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) {
	var wasmBytes []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT wasm_bytes FROM workflow_defs WHERE name = @p1 AND version = @p2
	`, defName, defVersion).Scan(&wasmBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("wasm not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("load wasm: %w", err)
	}
	return wasmBytes, nil
}

// GetWASMLength returns the byte length of the stored WASM binary.
func (s *MSSQLStore) GetWASMLength(ctx context.Context, defName string, defVersion int) (int64, error) {
	var length int64
	err := s.db.QueryRowContext(ctx, `SELECT len(wasm_bytes) FROM workflow_defs WHERE name = @p1 AND version = @p2`, defName, defVersion).Scan(&length)
	return length, err
}

// ListVersions returns all deployed versions of a workflow.
func (s *MSSQLStore) ListVersions(ctx context.Context, defName string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT version FROM workflow_defs WHERE name = @p1 ORDER BY version DESC
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
	return versions, rows.Err()
}

// LoadWorkflowConfig returns the max_history_length for a workflow definition.
func (s *MSSQLStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (int, error) {
	var maxHistoryLength int
	err := s.db.QueryRowContext(ctx, `
		SELECT max_history_length FROM workflow_defs WHERE name = @p1 AND version = @p2
	`, defName, defVersion).Scan(&maxHistoryLength)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("workflow def not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return 0, fmt.Errorf("load workflow config: %w", err)
	}
	return maxHistoryLength, nil
}

// LoadDAGSpec returns the dag_spec JSON for a workflow definition, or nil if none.
func (s *MSSQLStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
	var raw *[]byte
	err := s.db.QueryRowContext(ctx, `
		SELECT dag_spec FROM workflow_defs WHERE name = @p1 AND version = @p2
	`, defName, defVersion).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("workflow def not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("load dag_spec: %w", err)
	}
	if raw == nil {
		return nil, nil
	}
	return json.RawMessage(*raw), nil
}

// TraceWorkflow sets the W3C trace_id on a workflow instance.
func (s *MSSQLStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET trace_id = @p2 WHERE id = @p1
	`, workflowID, traceID)
	return err
}

// ResolveTenantFromAPIKey looks up a tenant UUID by API key hash.
// Uses CONVERT(NVARCHAR(36), tenant_id) to avoid byte-swapping issues with
// MSSQL UNIQUEIDENTIFIER mixed-endian storage.
func (s *MSSQLStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) {
	var tenantIDStr string
	err := s.db.QueryRowContext(ctx,
		`SELECT CONVERT(NVARCHAR(36), tenant_id) FROM tenant_api_keys
		 WHERE key_hash = @p1 AND revoked_at IS NULL`, keyHash).Scan(&tenantIDStr)
	if err != nil {
		return uuid.Nil, err
	}
	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve tenant: parse uuid: %w", err)
	}
	return tenantID, nil
}

// ListWorkflows returns workflow instances filtered by the given filter parameters.
// Supported filters: Status, InputContains, ErrorContains, Search.
// Supports pagination via Offset and Limit (default 100, max 1000).
func (s *MSSQLStore) ListWorkflows(ctx context.Context, filter WorkflowFilter) ([]WorkflowInstance, error) {
	d := s.dialect
	qb := NewQueryBuilder(d,
		"SELECT "+d.workflowInstanceColumns()+" FROM workflow_instances WHERE tenant_id = @p1",
	)
	qb.AddArgs(s.tenantID)

	if filter.Status != "" {
		qb.AddCondition("status = %s", filter.Status)
	}
	if filter.InputContains != "" {
		qb.AddLikeCondition(d.castExpr("input"), "%"+filter.InputContains+"%", true)
	}
	if filter.ErrorContains != "" {
		qb.AddLikeCondition("error_msg", "%"+filter.ErrorContains+"%", true)
	}
	if filter.Search != "" {
		pattern := "%" + filter.Search + "%"
		icol := d.castExpr("input")
		rcol := d.castExpr("result")
		n := qb.NextPos()
		qb.AddRaw(fmt.Sprintf("AND (%s OR %s OR %s OR %s)",
			d.likeExpr(icol, n, true),
			d.likeExpr(rcol, n+1, true),
			d.likeExpr("error_msg", n+2, true),
			d.likeExpr("def_name", n+3, true)))
		qb.AddArgs(pattern, pattern, pattern, pattern)
	}

	qb.AddRaw("ORDER BY created_at DESC")

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	} else if limit > 1000 {
		limit = 1000
	}

	if filter.Offset > 0 {
		qb.AddRaw(d.limitOffset(qb.NextPos(), qb.NextPos()+1, true))
		qb.AddArgs(limit, filter.Offset)
	} else {
		qb.AddRaw(d.limitOffset(qb.NextPos(), 0, false))
		qb.AddArgs(limit)
	}

	query, args := qb.SQL()
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer rows.Close()

	var workflows []WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt, createdAt sql.NullTime
		var inputStr string
		var assignedTo, errorCode, errorOp, errorMsg sql.NullString
		var traceID sql.NullString
		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &inputStr,
			&assignedTo, &nextWakeAt, &errorCode, &errorOp, &errorMsg, &createdAt, &wf.Generation, &wf.Priority, &traceID); err != nil {
			return nil, fmt.Errorf("scan workflow: %w", err)
		}
		wf.TraceID = traceID.String
		wf.Input = json.RawMessage(inputStr)
		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if createdAt.Valid {
			wf.CreatedAt = createdAt.Time
		}
		wf.AssignedTo = assignedTo.String
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wf.Error = errorMsg.String
		workflows = append(workflows, wf)
	}
	return workflows, rows.Err()
}

// GetWorkflowByID returns a single workflow instance by ID.
func (s *MSSQLStore) GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error) {
	var wf WorkflowInstance
	var nextWakeAt, heartbeatAt, completedAt sql.NullTime
	var assignedTo, errorMsg sql.NullString
	var result sql.NullString
	var inputRaw string
	var errorCode, errorOp sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, def_name, def_version, status, input,
		       assigned_to, heartbeat_at, next_wake_at, completed_at, CAST(result AS NVARCHAR(MAX)), error_msg, error_code, error_op,
		       generation, COALESCE(priority, 0) AS priority,
		       COALESCE(trace_id, '')
		FROM workflow_instances WHERE id = @p1 AND tenant_id = @p2
	`, id, s.tenantID).Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &inputRaw,
		&assignedTo, &heartbeatAt, &nextWakeAt, &completedAt, &result, &errorMsg, &errorCode, &errorOp,
		&wf.Generation, &wf.Priority,
		&wf.TraceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow: %w", err)
	}
	wf.Input = json.RawMessage(inputRaw)
	wf.AssignedTo = assignedTo.String
	wf.Result = result.String
	wf.Error = errorMsg.String
	wf.ErrorCode = errorCode.String
	wf.ErrorOp = errorOp.String
	if nextWakeAt.Valid {
		wf.NextWakeAt = nextWakeAt.Time
	}
	return &wf, nil
}

// ---- Version Management methods ----

// DeployWorkflowDef inserts or updates a workflow definition.
func (s *MSSQLStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error {
	pluginDepsJSON, _ := json.Marshal(def.PluginDeps)
	if pluginDepsJSON == nil {
		pluginDepsJSON = []byte("{}")
	}
	_, err := s.db.ExecContext(ctx, `
		MERGE workflow_defs AS target
		USING (SELECT @p1 AS name, @p2 AS version) AS source
		ON target.name = source.name AND target.version = source.version
		WHEN MATCHED THEN UPDATE SET
			wasm_bytes = @p3,
			abi_version = @p4,
			min_version = @p5,
			plugin_deps = @p6,
			deprecated = @p7
		WHEN NOT MATCHED THEN INSERT (name, version, wasm_bytes, abi_version, min_version, plugin_deps, deprecated)
		     VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7);
	`, def.Name, def.Version, def.WASMBytes, def.ABIVersion, def.MinVersion, pluginDepsJSON, def.Deprecated)
	if err != nil {
		return fmt.Errorf("deploy workflow def: %w", err)
	}
	return nil
}

// ListWorkflowDefs returns all versions of a workflow, ordered by version DESC.
func (s *MSSQLStore) ListWorkflowDefs(ctx context.Context, name string) ([]WorkflowDef, error) {
	var rows *sql.Rows
	var err error
	if name == "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT name, version, abi_version, min_version, plugin_deps, created_at, deprecated
			FROM workflow_defs ORDER BY name, version DESC
		`)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT name, version, abi_version, min_version, plugin_deps, created_at, deprecated
			FROM workflow_defs WHERE name = @p1 ORDER BY version DESC
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
			json.Unmarshal(pluginDepsRaw, &def.PluginDeps)
		}
		if def.PluginDeps == nil {
			def.PluginDeps = make(map[string]string)
		}
		defs = append(defs, def)
	}
	return defs, rows.Err()
}

// GetWorkflowDef returns a single workflow definition by name and version.
func (s *MSSQLStore) GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error) {
	var def WorkflowDef
	var pluginDepsRaw []byte
	var wasmBytes []byte
	var createdAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT name, version, wasm_bytes, abi_version, min_version, plugin_deps, created_at, deprecated
		FROM workflow_defs WHERE name = @p1 AND version = @p2
	`, name, version).Scan(&def.Name, &def.Version, &wasmBytes, &def.ABIVersion,
		&def.MinVersion, &pluginDepsRaw, &createdAt, &def.Deprecated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow def: %w", err)
	}
	def.WASMBytes = wasmBytes
	def.CreatedAt = createdAt
	if len(pluginDepsRaw) > 0 {
		json.Unmarshal(pluginDepsRaw, &def.PluginDeps)
	}
	if def.PluginDeps == nil {
		def.PluginDeps = make(map[string]string)
	}
	return &def, nil
}

// MarkVersionDeprecated sets the deprecated flag on a workflow version.
func (s *MSSQLStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_defs SET deprecated = @p3 WHERE name = @p1 AND version = @p2
	`, name, version, deprecated)
	if err != nil {
		return fmt.Errorf("mark version deprecated: %w", err)
	}
	return nil
}

// PurgeWorkflowDef permanently deletes a workflow definition.
func (s *MSSQLStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM workflow_defs WHERE name = @p1 AND version = @p2
	`, name, version)
	if err != nil {
		return fmt.Errorf("purge workflow def: %w", err)
	}
	return nil
}

// CountActiveInstances returns the number of ready or running instances for a version.
func (s *MSSQLStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE def_name = @p1 AND def_version = @p2
		  AND status IN ('ready', 'running')
	`, name, version).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count active instances: %w", err)
	}
	return count, nil
}

// ResolveLatestVersion resolves the latest version for a named definition.
func (s *MSSQLStore) ResolveLatestVersion(ctx context.Context, defName string) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx, `
		SELECT ISNULL(MAX(version), 0) FROM workflow_defs
		WHERE name = @p1 AND deprecated = 0
	`, defName).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("resolve latest version: %w", err)
	}
	if version == 0 {
		return 0, fmt.Errorf("resolve latest version: no non-deprecated version found for %s", defName)
	}
	return version, nil
}

// ValidateVersion checks whether the given version is valid.
func (s *MSSQLStore) ValidateVersion(ctx context.Context, defName string, defVersion int) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT CASE WHEN EXISTS (
			SELECT 1 FROM workflow_defs
			WHERE name = @p1 AND version = @p2 AND deprecated = 0
		) THEN 1 ELSE 0 END
	`, defName, defVersion).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("validate version: %w", err)
	}
	return exists, nil
}

// GetActiveInstanceCountsByVersion returns a map of "name:version" -> count.
func (s *MSSQLStore) GetActiveInstanceCountsByVersion(ctx context.Context) (map[string]int, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("get active instance counts: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT def_name, def_version, COUNT(*) as cnt
		FROM workflow_instances
		WHERE status IN ('ready', 'running')
		  AND (tenant_id = @p1 OR tenant_id IS NULL)
		GROUP BY def_name, def_version
	`, s.tenantID)
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

// ---------------------------------------------------------------------------
// Tag methods (deployment channels)
// ---------------------------------------------------------------------------

// SetWorkflowTag assigns a tag to a specific version.
// Uses MERGE so reassigning a tag updates in place.
func (s *MSSQLStore) SetWorkflowTag(ctx context.Context, workflowName string, version int, tag string) error {
	_, err := s.db.ExecContext(ctx, `
		MERGE workflow_tags AS target
		USING (SELECT @p1 AS workflow_name, @p2 AS tag) AS source
		ON target.workflow_name = source.workflow_name AND target.tag = source.tag
		WHEN MATCHED THEN UPDATE SET
			version = @p3,
			created_at = SYSUTCDATETIME()
		WHEN NOT MATCHED THEN INSERT (workflow_name, version, tag, tenant_id)
			VALUES (@p1, @p3, @p2, @p4);
	`, workflowName, tag, version, s.tenantID)
	if err != nil {
		return fmt.Errorf("set workflow tag: %w", err)
	}
	return nil
}

// RemoveWorkflowTag deletes a tag assignment.
func (s *MSSQLStore) RemoveWorkflowTag(ctx context.Context, workflowName string, tag string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM workflow_tags WHERE workflow_name = @p1 AND tag = @p2
	`, workflowName, tag)
	if err != nil {
		return fmt.Errorf("remove workflow tag: %w", err)
	}
	return nil
}

// GetWorkflowTag returns the version for a given tag.
func (s *MSSQLStore) GetWorkflowTag(ctx context.Context, workflowName string, tag string) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx, `
		SELECT version FROM workflow_tags WHERE workflow_name = @p1 AND tag = @p2
	`, workflowName, tag).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("get workflow tag: tag %q not found for workflow %s", tag, workflowName)
	}
	if err != nil {
		return 0, fmt.Errorf("get workflow tag: %w", err)
	}
	return version, nil
}

// GetWorkflowTags returns all tag -> version mappings for a workflow.
func (s *MSSQLStore) GetWorkflowTags(ctx context.Context, workflowName string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tag, version FROM workflow_tags WHERE workflow_name = @p1
	`, workflowName)
	if err != nil {
		return nil, fmt.Errorf("get workflow tags: %w", err)
	}
	defer rows.Close()

	tags := make(map[string]int)
	for rows.Next() {
		var tag string
		var version int
		if err := rows.Scan(&tag, &version); err != nil {
			return nil, fmt.Errorf("get workflow tags: scan: %w", err)
		}
		tags[tag] = version
	}
	return tags, rows.Err()
}

// ---------------------------------------------------------------------------
// Routing methods (A/B traffic splitting)
// ---------------------------------------------------------------------------

// SetRoutingRule creates a routing rule for a workflow version.
func (s *MSSQLStore) SetRoutingRule(ctx context.Context, workflowName string, targetVersion int, weight float64) error {
	id := uuid.New()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_routing (id, workflow_name, target_version, weight, tenant_id)
		VALUES (@p1, @p2, @p3, @p4, @p5)
	`, id, workflowName, targetVersion, weight, s.tenantID)
	if err != nil {
		return fmt.Errorf("set routing rule: %w", err)
	}
	return nil
}

// RemoveRoutingRule deletes a routing rule by ID.
func (s *MSSQLStore) RemoveRoutingRule(ctx context.Context, ruleID string) error {
	id, err := uuid.Parse(ruleID)
	if err != nil {
		return fmt.Errorf("remove routing rule: invalid rule id %q: %w", ruleID, err)
	}
	_, err = s.db.ExecContext(ctx, `
		DELETE FROM workflow_routing WHERE id = @p1
	`, id)
	if err != nil {
		return fmt.Errorf("remove routing rule: %w", err)
	}
	return nil
}

// GetRoutingRules returns all routing rules for a workflow.
func (s *MSSQLStore) GetRoutingRules(ctx context.Context, workflowName string) ([]RoutingRule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT CONVERT(NVARCHAR(36), id), workflow_name, target_version, weight
		FROM workflow_routing WHERE workflow_name = @p1
	`, workflowName)
	if err != nil {
		return nil, fmt.Errorf("get routing rules: %w", err)
	}
	defer rows.Close()

	var rules []RoutingRule
	for rows.Next() {
		var r RoutingRule
		if err := rows.Scan(&r.ID, &r.WorkflowName, &r.TargetVersion, &r.Weight); err != nil {
			return nil, fmt.Errorf("get routing rules: scan: %w", err)
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// PickVersionByRouting performs weighted random version selection.
// Returns 0 if no routing rules exist.
func (s *MSSQLStore) PickVersionByRouting(ctx context.Context, workflowName string) (int, error) {
	rules, err := s.GetRoutingRules(ctx, workflowName)
	if err != nil {
		return 0, err
	}
	if len(rules) == 0 {
		return 0, nil
	}

	total := 0.0
	for _, r := range rules {
		total += r.Weight
	}
	if total <= 0 {
		return 0, nil
	}

	// Use crypto/rand for weighted selection.
	scale := int64(1_000_000_000)
	scaledTotal := int64(total * float64(scale))
	if scaledTotal <= 0 {
		return 0, nil
	}

	n, err := rand.Int(rand.Reader, big.NewInt(scaledTotal))
	if err != nil {
		return 0, fmt.Errorf("pick version by routing: random: %w", err)
	}
	pick := n.Int64()

	cumulative := int64(0)
	for _, r := range rules {
		cumulative += int64(r.Weight * float64(scale))
		if pick < cumulative {
			return r.TargetVersion, nil
		}
	}
	return rules[len(rules)-1].TargetVersion, nil
}

// ---------------------------------------------------------------------------
// Version Resolution
// ---------------------------------------------------------------------------

// ResolveVersionByTag resolves a tag to a version number.
// If tag is "latest", returns the highest non-deprecated version.
func (s *MSSQLStore) ResolveVersionByTag(ctx context.Context, workflowName string, tag string) (int, error) {
	if tag == "latest" {
		return s.ResolveLatestVersion(ctx, workflowName)
	}
	var version int
	err := s.db.QueryRowContext(ctx, `
		SELECT version FROM workflow_tags WHERE workflow_name = @p1 AND tag = @p2
	`, workflowName, tag).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("resolve version by tag: tag %q not found for workflow %s", tag, workflowName)
	}
	if err != nil {
		return 0, fmt.Errorf("resolve version by tag: %w", err)
	}
	return version, nil
}
