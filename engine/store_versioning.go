package engine

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
)

func (s *PostgresStore) SetWorkflowTag(ctx context.Context, workflowName string, version int, tag string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("set workflow tag: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_tags (workflow_name, version, tag, tenant_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (workflow_name, tag) DO UPDATE SET version = EXCLUDED.version, created_at = now()
	`, workflowName, version, tag, s.tenantID)
	if err != nil {
		return fmt.Errorf("set workflow tag: %w", err)
	}
	return tx.Commit()
}

// RemoveWorkflowTag deletes a tag assignment.

func (s *PostgresStore) RemoveWorkflowTag(ctx context.Context, workflowName string, tag string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("remove workflow tag: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		DELETE FROM workflow_tags WHERE workflow_name = $1 AND tag = $2
	`, workflowName, tag)
	if err != nil {
		return fmt.Errorf("remove workflow tag: %w", err)
	}
	return tx.Commit()
}

// GetWorkflowTag returns the version for a given tag.

func (s *PostgresStore) GetWorkflowTag(ctx context.Context, workflowName string, tag string) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("get workflow tag: begin: %w", err)
	}
	defer tx.Rollback()

	var version int
	err = tx.QueryRowContext(ctx, `
		SELECT version FROM workflow_tags WHERE workflow_name = $1 AND tag = $2
	`, workflowName, tag).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("get workflow tag: tag %q not found for workflow %s", tag, workflowName)
	}
	if err != nil {
		return 0, fmt.Errorf("get workflow tag: %w", err)
	}
	return version, tx.Commit()
}

// GetWorkflowTags returns all tag -> version mappings for a workflow.

func (s *PostgresStore) GetWorkflowTags(ctx context.Context, workflowName string) (map[string]int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get workflow tags: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT tag, version FROM workflow_tags WHERE workflow_name = $1
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get workflow tags: rows: %w", err)
	}
	return tags, tx.Commit()
}

// ---- Routing methods (A/B traffic splitting) ----

// SetRoutingRule creates a routing rule for a workflow version.

func (s *PostgresStore) SetRoutingRule(ctx context.Context, workflowName string, targetVersion int, weight float64) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("set routing rule: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_routing (workflow_name, target_version, weight, tenant_id)
		VALUES ($1, $2, $3, $4)
	`, workflowName, targetVersion, weight, s.tenantID)
	if err != nil {
		return fmt.Errorf("set routing rule: %w", err)
	}
	return tx.Commit()
}

// RemoveRoutingRule deletes a routing rule by ID.

func (s *PostgresStore) RemoveRoutingRule(ctx context.Context, ruleID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("remove routing rule: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		DELETE FROM workflow_routing WHERE id = $1
	`, ruleID)
	if err != nil {
		return fmt.Errorf("remove routing rule: %w", err)
	}
	return tx.Commit()
}

// GetRoutingRules returns all routing rules for a workflow.

func (s *PostgresStore) GetRoutingRules(ctx context.Context, workflowName string) ([]RoutingRule, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get routing rules: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, workflow_name, target_version, weight
		FROM workflow_routing WHERE workflow_name = $1
	`, workflowName)
	if err != nil {
		return nil, fmt.Errorf("get routing rules: %w", err)
	}
	defer rows.Close()

	var rules []RoutingRule
	for rows.Next() {
		var r RoutingRule
		var id uuid.UUID
		if err := rows.Scan(&id, &r.WorkflowName, &r.TargetVersion, &r.Weight); err != nil {
			return nil, fmt.Errorf("get routing rules: scan: %w", err)
		}
		r.ID = id.String()
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get routing rules: rows: %w", err)
	}
	return rules, tx.Commit()
}

// PickVersionByRouting performs weighted random version selection.
// Returns 0 if no routing rules exist.

func (s *PostgresStore) PickVersionByRouting(ctx context.Context, workflowName string) (int, error) {
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

// ---- Version Resolution ----

// ResolveVersionByTag resolves a tag to a version number.
// If tag is "latest", returns the highest non-deprecated version.

func (s *PostgresStore) ResolveVersionByTag(ctx context.Context, workflowName string, tag string) (int, error) {
	if tag == "latest" {
		return s.ResolveLatestVersion(ctx, workflowName)
	}
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve version by tag: begin: %w", err)
	}
	defer tx.Rollback()

	var version int
	err = tx.QueryRowContext(ctx, `
		SELECT version FROM workflow_tags WHERE workflow_name = $1 AND tag = $2
	`, workflowName, tag).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("resolve version by tag: tag %q not found for workflow %s", tag, workflowName)
	}
	if err != nil {
		return 0, fmt.Errorf("resolve version by tag: %w", err)
	}
	return version, tx.Commit()
}

// Heartbeat updates the heartbeat timestamp.
