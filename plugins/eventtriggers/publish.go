package eventtriggers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// decodeJSONObject decodes a JSON object WITHOUT narrowing its numbers.
//
// encoding/json decodes a JSON number into float64 unless told otherwise, so
// the default decode of
//
//	{"n":123456789012345678901234567890}
//
// re-marshals as 1.2345678901234568e+29 -- a different value, with no error
// and no log. UseNumber keeps the literal as a json.Number, which is a string,
// and json.Marshal writes a json.Number back verbatim. So a value that makes
// the round trip through this function is the value that arrived.
//
// This exists because the map is needed for FILTERING and nothing else. The
// bytes that get stored and forwarded never pass through it. cleat#1641.
func decodeJSONObject(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// PublishEvent stores an event, dispatches it to matching subscriptions,
// and signals any workflows awaiting this event type. Returns the number
// of workflows started.
//
// This is the core publishing pipeline, exported so that other plugins
// (e.g., kafkaconnect, webhookingest) can publish events without going
// through the HTTP API.
//
// eventData is RAW JSON and is stored, forwarded to awaiters, and merged into
// a workflow input BYTE FOR BYTE. It was a map[string]any until cleat#1641,
// which meant this function re-encoded whatever it was handed -- and a
// map[string]any can only have been produced by a decode that already turned
// 123456789012345678901234567890 into 1.2345678901234568e+29. The column type
// could not fix that, because the value was wrong before any database saw it.
// Taking bytes is what removes the round trip; it is not a style preference.
func PublishEvent(
	ctx context.Context,
	db plugin.PluginDB,
	logger *slog.Logger,
	env *plugin.Environment,
	eventID uuid.UUID,
	tenantID uuid.UUID,
	eventType string,
	eventData json.RawMessage,
) (int, error) {
	// Scope every statement below to the tenant this event belongs to.
	//
	// The tenant arrives as an ARGUMENT and the context need not carry it --
	// which is not a quirk of one caller but the shape of the shared entry
	// point. PublishEvent is exported precisely so other plugins can publish
	// without going through the HTTP API, and two of its three callers reach it
	// with no tenant in context: kafkaconnect from a background poll loop, and
	// webhookingest from POST /ingest/{source_id}, which cmd/cleat-worker's
	// middleware list exempts from auth because the caller is an external
	// system holding no cleat credential.
	//
	// At that second caller the argument and the context can legitimately
	// DISAGREE even when the context has a tenant: the value comes from the
	// webhook_sources row the handler just looked up, not from the requester.
	// So the parameter is the only correct source, and scoping here rather than
	// at each call site is not a convenience -- a caller-side convention has to
	// be right at every site, this has to be right once. cleat#1538.
	//
	// It covers the fan-out too: triggerMatchingWorkflows reads
	// event_subscriptions, signalAwaiters reads event_awaiters and
	// unregisterAwaiter deletes from it, all three tenant-scoped, all three
	// taking this ctx.
	ctx = plugin.ForTenant(ctx, tenantID)

	// An absent body is an empty object, not SQL NULL and not the four bytes
	// "null" -- every reader here expects an object.
	if len(eventData) == 0 {
		eventData = json.RawMessage("{}")
	}

	// Insert with idempotency — ON CONFLICT DO NOTHING prevents duplicate
	// processing of the same event ID.
	rows, err := db.Exec(ctx, plugin.Rebind(insertEventIdempotent.For(currentDialect), currentDialect),
		eventID, tenantID, eventType, string(eventData))
	if err != nil {
		return 0, fmt.Errorf("store event: %w", err)
	}

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
		db.Exec(ctx, plugin.Rebind(`UPDATE ingested_events SET error_msg = $1 WHERE id = $2`, currentDialect),
			"failed to query subscriptions: "+err.Error(), eventID)
	}

	// ---- Signal awaiters ----

	signalAwaiters(ctx, db, logger, env, tenantID, eventType, string(eventData))

	return matched, nil
}

// triggerMatchingWorkflows queries subscriptions matching the event and starts
// a workflow for each one whose filter passes. Returns the number of workflows
// started and any error from the subscription query itself (individual dispatch
// errors are logged but do not halt processing).
func triggerMatchingWorkflows(
	ctx context.Context,
	db plugin.PluginDB,
	logger *slog.Logger,
	env *plugin.Environment,
	eventID uuid.UUID,
	tenantID uuid.UUID,
	eventType string,
	eventData json.RawMessage,
) (int, error) {
	// Decoded at most ONCE, and only for filtering. The filter language
	// compares values and never re-serialises them, so a json.Number here is
	// read through filter.go's toFloat64 exactly as a float64 was -- the
	// comparison semantics do not change. What must not happen is this map
	// becoming the source of the workflow input again; mergeInputAndTemplate
	// takes the bytes. cleat#1641.
	//
	// Lazy because the common subscription has no filter, and because event
	// data that is not a JSON object should fail only the subscriptions that
	// actually ask a question about it.
	var (
		decoded     map[string]any
		decodeErr   error
		decodeReady bool
	)
	filterData := func() (map[string]any, error) {
		if !decodeReady {
			decoded, decodeErr = decodeJSONObject(eventData)
			decodeReady = true
		}
		return decoded, decodeErr
	}

	rows, err := db.Query(ctx, plugin.Rebind(`
		SELECT id, tenant_id, event_type, def_name, entry_point, input_template, filter_expr, enabled, created_at, max_retries
		FROM event_subscriptions
		WHERE tenant_id = $1 AND event_type = $2 AND enabled = true
		`, currentDialect), tenantID, eventType)
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
		if err := plugin.ScanRow(rows, &sub.ID, &sub.TenantID, &sub.EventType, &sub.DefName,
			&sub.EntryPoint, &inputTemplateRaw, &sub.FilterExpr, &sub.Enabled, &sub.CreatedAt, &sub.MaxRetries); err != nil {
			logger.Error("event-triggers: scan subscription", "error", err)
			continue
		}
		sub.InputTemplate = json.RawMessage(inputTemplateRaw)

		// Evaluate filter expression.
		if sub.FilterExpr != "" && sub.FilterExpr != "true" {
			fd, err := filterData()
			if err != nil {
				logger.Error("event-triggers: event data is not a JSON object",
					"subscription_id", sub.ID,
					"event_id", eventID,
					"error", err,
				)
				continue
			}
			ok, err := EvaluateFilter(sub.FilterExpr, fd)
			if err != nil {
				// CodeQL go/clear-text-logging (alert #14) flags this: the
				// event data here can be built from inbound webhook HTTP
				// headers (plugins/webhookingest/routes.go), and fd -- the
				// decode of it -- is a parameter to EvaluateFilter, so the
				// tool conservatively treats err as tainted by header content. It never actually
				// is: every error path in filter.go's tokenizer, parser, and
				// evaluator (EvaluateFilter, evalPath, compareValues,
				// matchOperators, etc.) only formats the filter expression's
				// own tokens/paths/operators and Go type names (%T) into its
				// error strings — none of them interpolate an eventData
				// value. Verified by reading every fmt.Errorf in that file.
				// Dismissed as a false positive; see alert #14.
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
			// KEYED PER (EVENT, SUBSCRIPTION), which is the unit this loop
			// actually dispatches. Keying on the event alone would collapse a
			// fan-out to several subscriptions into one start; keying on the
			// subscription alone would deduplicate across unrelated events.
			//
			// This is also what a whole-event retry needs in order to become
			// safe: re-running this loop re-presents the same key for each
			// subscription that already succeeded, so those return the
			// existing run rather than creating a second. That is the
			// precondition for ever inverting the `matched > 0` gate, which is
			// separate work. cleat#1555.
			req := plugin.StartRequest{
				DefName:        sub.DefName,
				Input:          inputJSON,
				IdempotencyKey: fmt.Sprintf("eventtrigger:%s:%s", eventID, sub.ID),
				TenantID:       tenantID.String(),
				EntryPoint:     sub.EntryPoint,
			}
			runID, err := env.StartWorkflow(ctx, req)
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
	db plugin.PluginDB,
	logger *slog.Logger,
	env *plugin.Environment,
	tenantID uuid.UUID,
	eventType string,
	eventData string,
) {
	if env == nil || env.SignalWorkflow == nil {
		return
	}

	rows, err := db.Query(ctx, plugin.Rebind(`
		SELECT workflow_id
		FROM event_awaiters
		WHERE tenant_id = $1 AND event_type = $2
		`, currentDialect), tenantID, eventType)
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
			if errors.Is(err, plugin.ErrWorkflowNotFound) {
				// The awaiter's own workflow is gone -- purged, or the row
				// was already stale -- not a delivery failure.
				//
				// Before cleat#2218, SignalWorkflow returned a plain error
				// for this case (an FK violation, on the dialects that have
				// one), which this function had no branch for: it fell to
				// the generic failure path below, logged a WARN, and did
				// NOT unregister -- so the awaiter leaked forever, because
				// a permanently-gone workflow produces the identical error
				// on every future publish (cleat#2213). cleat#2218 changed
				// this same case to return nil instead, closing an
				// existence oracle -- and, as a side effect, that already
				// stopped the leak: nil fell to the SUCCESS path below,
				// which unregisters unconditionally. What #2218 did not fix
				// is that the success path also logs "signal delivered to
				// awaiter", which was false for this case -- the workflow
				// never received anything.
				//
				// cleat#2227 gives "not found" its own path instead of
				// relying on that accident: same outcome as before
				// (unregister), but honestly, with a log line that says
				// what actually happened rather than claiming a delivery
				// that did not occur.
				logger.Info("event-triggers: awaiter's workflow no longer exists, unregistering",
					"workflow_id", wfID,
					"signal", signalName,
				)
				unregisterAwaiter(ctx, db, logger, wfID, eventType)
				continue
			}
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
func unregisterAwaiter(ctx context.Context, db plugin.PluginDB, logger *slog.Logger, workflowID, eventType string) {
	if workflowID == "" {
		return
	}
	_, err := db.Exec(ctx, plugin.Rebind(`
		DELETE FROM event_awaiters
		WHERE workflow_id = $1 AND event_type = $2
		`, currentDialect), workflowID, eventType)
	if err != nil {
		logger.Warn("event-triggers: unregister awaiter", "error", err)
	}
}
