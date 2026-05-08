package eventtriggers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/plugin"
)

// PublishEvent stores an event, dispatches it to matching subscriptions,
// and signals any workflows awaiting this event type. Returns the number
// of workflows started.
//
// This is the core publishing pipeline, exported so that other plugins
// (e.g., kafkaconnect, webhookingest) can publish events without going
// through the HTTP API.
func PublishEvent(
	ctx context.Context,
db plugin.DB,
	logger *slog.Logger,
	env *plugin.Environment,
	eventID uuid.UUID,
	tenantID uuid.UUID,
	eventType string,
	eventData map[string]interface{},
) (int, error) {
	eventDataJSON, err := json.Marshal(eventData)
	if err != nil {
		return 0, fmt.Errorf("marshal event data: %w", err)
	}

	// Insert with idempotency — ON CONFLICT DO NOTHING prevents duplicate
	// processing of the same event ID.
	result, err := db.ExecContext(ctx, `
		INSERT INTO ingested_events (id, tenant_id, event_type, event_data, received_at, processed)
		VALUES ($1, $2, $3, $4, NOW(), false)
		ON CONFLICT (id) DO NOTHING
	`, eventID, tenantID, eventType, string(eventDataJSON))
	if err != nil {
		return 0, fmt.Errorf("store event: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		// Already ingested — idempotent return.
		logger.Info("event-triggers: duplicate event, skipping",
			"event_id", eventID,
			"event_type", eventType,
		)
		return 0, nil
	}

	logger.Info("event-triggers: event stored",
		"event_id", eventID,
		"tenant", tenantID,
		"event_type", eventType,
	)

	// ---- Subscription matching ----

	matched, err := triggerMatchingWorkflows(ctx, db, logger, env, eventID, tenantID, eventType, eventData)
	if err != nil {
		db.ExecContext(ctx, `UPDATE ingested_events SET error_msg = $1 WHERE id = $2`,
			"failed to query subscriptions: "+err.Error(), eventID)
	}

	// ---- Signal awaiters ----

	signalAwaiters(ctx, db, logger, env, tenantID, eventType, string(eventDataJSON))

	return matched, nil
}

// triggerMatchingWorkflows queries subscriptions matching the event and starts
// a workflow for each one whose filter passes. Returns the number of workflows
// started and any error from the subscription query itself (individual dispatch
// errors are logged but do not halt processing).
func triggerMatchingWorkflows(
	ctx context.Context,
db plugin.DB,
	logger *slog.Logger,
	env *plugin.Environment,
	eventID uuid.UUID,
	tenantID uuid.UUID,
	eventType string,
	eventData map[string]interface{},
) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, tenant_id, event_type, def_name, entry_point, input_template, filter_expr, enabled, created_at, max_retries
		FROM event_subscriptions
		WHERE tenant_id = $1 AND event_type = $2 AND enabled = true
	`, tenantID, eventType)
	if err != nil {
		return 0, fmt.Errorf("query subscriptions: %w", err)
	}
	defer rows.Close()

	matched := 0
	for rows.Next() {
		var (
			sub              subscriptionJSON
			inputTemplateRaw []byte
		)
		if err := rows.Scan(&sub.ID, &sub.TenantID, &sub.EventType, &sub.DefName,
			&sub.EntryPoint, &inputTemplateRaw, &sub.FilterExpr, &sub.Enabled, &sub.CreatedAt, &sub.MaxRetries); err != nil {
			logger.Error("event-triggers: scan subscription", "error", err)
			continue
		}
		sub.InputTemplate = json.RawMessage(inputTemplateRaw)

		// Evaluate filter expression.
		if sub.FilterExpr != "" && sub.FilterExpr != "true" {
			ok, err := EvaluateFilter(sub.FilterExpr, eventData)
			if err != nil {
				logger.Error("event-triggers: filter evaluation error",
					"subscription_id", sub.ID,
					"filter_expr", sub.FilterExpr,
					"error", err,
				)
				continue
			}
			if !ok {
				logger.Debug("event-triggers: filter did not match",
					"subscription_id", sub.ID,
					"event_id", eventID,
				)
				continue
			}
		}

		// Build workflow input from input_template merged with event data.
		inputJSON, err := mergeInputAndTemplate(sub.InputTemplate, eventData)
		if err != nil {
			logger.Error("event-triggers: build workflow input", "error", err)
			continue
		}

		if env != nil && env.StartWorkflow != nil {
			runID, err := env.StartWorkflow(ctx, sub.DefName, inputJSON)
			if err != nil {
				logger.Error("event-triggers: start workflow failed",
					"def_name", sub.DefName,
					"event_id", eventID,
					"error", err,
				)
				continue
			}
			matched++
			logger.Info("event-triggers: workflow started",
				"def_name", sub.DefName,
				"run_id", runID,
				"event_id", eventID,
			)
		}
	}
	if err := rows.Err(); err != nil {
		return matched, err
	}

	return matched, nil
}

// signalAwaiters delivers a signal to all workflows that are registered as
// waiting for the given event type. Called from the publish handler after
// an event has been successfully stored.
func signalAwaiters(
	ctx context.Context,
db plugin.DB,
	logger *slog.Logger,
	env *plugin.Environment,
	tenantID uuid.UUID,
	eventType string,
	eventData string,
) {
	if env == nil || env.SignalWorkflow == nil {
		return
	}

	rows, err := db.QueryContext(ctx, `
		SELECT workflow_id
		FROM event_awaiters
		WHERE tenant_id = $1 AND event_type = $2
	`, tenantID, eventType)
	if err != nil {
		logger.Error("event-triggers: query awaiters", "error", err)
		return
	}
	defer rows.Close()

	var workflowIDs []string
	for rows.Next() {
		var wfID string
		if err := rows.Scan(&wfID); err != nil {
			logger.Error("event-triggers: scan awaiter", "error", err)
			continue
		}
		workflowIDs = append(workflowIDs, wfID)
	}
	if err := rows.Err(); err != nil {
		logger.Error("event-triggers: awaiters rows error", "error", err)
		return
	}

	signalName := "__evt:" + eventType
	for _, wfID := range workflowIDs {
		if err := env.SignalWorkflow(ctx, wfID, signalName, eventData); err != nil {
			logger.Warn("event-triggers: signal awaiter failed",
				"workflow_id", wfID,
				"signal", signalName,
				"error", err,
			)
			continue
		}
		logger.Info("event-triggers: signal delivered to awaiter",
			"workflow_id", wfID,
			"signal", signalName,
		)
		unregisterAwaiter(ctx, db, logger, wfID, eventType)
	}
}

// unregisterAwaiter removes the awaiter record.
func unregisterAwaiter(ctx context.Context, db plugin.DB, logger *slog.Logger, workflowID, eventType string) {
	if workflowID == "" {
		return
	}
	_, err := db.ExecContext(ctx, `
		DELETE FROM event_awaiters
		WHERE workflow_id = $1 AND event_type = $2
	`, workflowID, eventType)
	if err != nil {
		logger.Warn("event-triggers: unregister awaiter", "error", err)
	}
}
