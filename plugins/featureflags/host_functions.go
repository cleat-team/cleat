package featureflags

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/plugin"
)

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry. The plugin name is implicit -- each plugin gets its own
// scope, so function names need not be globally unique.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("feature-flags: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{Name: "evaluate_flag", Idempotent: true}, p.evaluateFlag); err != nil {
		return err
	}
	return nil
}

// ---- Input/output types ----

type evaluateFlagInput struct {
	Key     string            `json:"key"`
	Context EvaluationContext `json:"context"`
}

type evaluateFlagOutput struct {
	Enabled    bool              `json:"enabled"`
	Key        string            `json:"key"`
	Evaluation *EvaluationDetail `json:"evaluation,omitempty"`
}

// ---- Host functions ----

// evaluateFlag evaluates a feature flag from a workflow. It looks up the flag
// by tenant_id + key, evaluates rules, checks rollout percentage, and returns
// the result. This function is idempotent and safe to re-invoke during replay.
func (p *Plugin) evaluateFlag(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == uuid.Nil {
		return "", fmt.Errorf("feature-flags: no tenant context")
	}

	var input evaluateFlagInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("feature-flags: invalid input: %w", err)
	}
	if input.Key == "" {
		return "", fmt.Errorf("feature-flags: key is required")
	}

	// Look up the flag by tenant_id and key.
	var (
		id                uuid.UUID
		tenantID          uuid.UUID
		key               string
		name              sql.NullString
		description       sql.NullString
		enabled           bool
		rulesJSON         []byte
		rolloutPercentage int
	)

	err := p.db.QueryRow(ctx, plugin.Rebind(`
			SELECT id, tenant_id, key, name, description, enabled, rules, rollout_percentage
			FROM feature_flags
			WHERE tenant_id = $1 AND key = $2
		`, p.dialect), cc.TenantID, input.Key).Scan(
		&id, &tenantID, &key, &name, &description,
		&enabled, &rulesJSON, &rolloutPercentage,
	)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("feature-flags: flag not found: %s", input.Key)
	}
	if err != nil {
		return "", fmt.Errorf("feature-flags: lookup: %w", err)
	}

	flake := &Flag{
		ID:                id.String(),
		TenantID:          tenantID.String(),
		Key:               key,
		Name:              name.String,
		Description:       description.String,
		Enabled:           enabled,
		Rules:             rulesJSON,
		RolloutPercentage: rolloutPercentage,
	}

	result := EvaluateFlag(flake, input.Context)

	output := evaluateFlagOutput{
		Enabled:    result.Enabled,
		Key:        result.Key,
		Evaluation: result.Evaluation,
	}
	outJSON, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("feature-flags: marshal output: %w", err)
	}
	return string(outJSON), nil
}
