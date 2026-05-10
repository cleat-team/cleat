package eventtriggers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
	"github.com/rcownie/cleat/internal/plugin"
)

// RegisterRoutes registers HTTP handlers for the event-triggers plugin.
func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("event-triggers: nil mux")
	}
	mux.HandleFunc("POST /api/events/publish", p.handlePublishEvent)
	mux.HandleFunc("POST /api/events/subscriptions", p.handleCreateSubscription)
	mux.HandleFunc("GET /api/events/subscriptions", p.handleListSubscriptions)
	mux.HandleFunc("DELETE /api/events/subscriptions/{id}", p.handleDeleteSubscription)
	mux.HandleFunc("POST /api/events/{event_id}/retry", p.handleRetryEvent)
	return nil
}

// ---- types ----

type publishEventRequest struct {
	ID        string                 `json:"id"`
	EventType string                 `json:"event_type"`
	Data      map[string]interface{} `json:"data"`
}

type publishEventResponse struct {
	Status  string `json:"status"`
	Matched int    `json:"matched,omitempty"`
}

type createSubscriptionRequest struct {
	EventType     string          `json:"event_type"`
	DefName       string          `json:"def_name"`
	EntryPoint    string          `json:"entry_point"`
	InputTemplate json.RawMessage `json:"input_template"`
	FilterExpr    string          `json:"filter_expr"`
	MaxRetries    *int            `json:"max_retries,omitempty"`
}

type subscriptionJSON struct {
	ID            uuid.UUID       `json:"id"`
	TenantID      uuid.UUID       `json:"tenant_id"`
	EventType     string          `json:"event_type"`
	DefName       string          `json:"def_name"`
	EntryPoint    string          `json:"entry_point"`
	InputTemplate json.RawMessage `json:"input_template"`
	FilterExpr    string          `json:"filter_expr"`
	MaxRetries    int             `json:"max_retries"`
	Enabled       bool            `json:"enabled"`
	CreatedAt     time.Time       `json:"created_at"`
}

// ---- helpers ----

func (p *Plugin) writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (p *Plugin) writeError(w http.ResponseWriter, status int, msg string) {
	p.writeJSON(w, status, map[string]string{"error": msg})
}

// tenantID extracts the tenant UUID from the request context. Returns the
// zero UUID if no tenant is set.
func (p *Plugin) tenantID(r *http.Request) uuid.UUID {
	tid, _ := auth.TenantIDFromContext(r.Context())
	return tid
}

// ---- POST /api/events/publish ----

func (p *Plugin) handlePublishEvent(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("event-triggers: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req publishEventRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}

	if req.ID == "" {
		p.writeError(w, 400, "id is required")
		return
	}
	if req.EventType == "" {
		p.writeError(w, 400, "event_type is required")
		return
	}

	eventID, err := uuid.Parse(req.ID)
	if err != nil {
		p.writeError(w, 400, "invalid event id")
		return
	}

	if req.Data == nil {
		req.Data = make(map[string]interface{})
	}

	// Dispatch through the core publish pipeline — stores the event,
	// matches subscriptions, starts workflows, and signals awaiters.
	matched, err := PublishEvent(r.Context(), p.db, p.logger, p.env, eventID, tid, req.EventType, req.Data)
	if err != nil {
		p.logger.Error("event-triggers: publish event", "error", err)
	}

	p.writeJSON(w, 200, publishEventResponse{
		Status:  "published",
		Matched: matched,
	})
}

// mergeInputAndTemplate builds the workflow input JSON by starting with the
// subscription's input_template and overlaying the published event data.
func mergeInputAndTemplate(tmpl json.RawMessage, eventData map[string]interface{}) (json.RawMessage, error) {
	// Start with input_template as base.
	base := make(map[string]interface{})
	if len(tmpl) > 0 {
		if err := json.Unmarshal(tmpl, &base); err != nil {
			// If template is not a JSON object, ignore it.
			base = make(map[string]interface{})
		}
	}

	// Overlay event data (event data takes precedence for duplicate keys).
	for k, v := range eventData {
		base[k] = v
	}

	return json.Marshal(base)
}

// ---- POST /api/events/subscriptions ----

func (p *Plugin) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("event-triggers: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req createSubscriptionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}

	if req.EventType == "" {
		p.writeError(w, 400, "event_type is required")
		return
	}
	if req.DefName == "" {
		p.writeError(w, 400, "def_name is required")
		return
	}

	if req.InputTemplate == nil {
		req.InputTemplate = json.RawMessage("{}")
	}
	inputTemplateStr := string(req.InputTemplate)

	now := time.Now()

	var subID uuid.UUID
	if p.dialect == plugin.DialectMySQL {
		// MySQL: generate UUID on Go side, insert without RETURNING
		subID = uuid.New()
		_, execErr := p.db.Exec(r.Context(), plugin.Rebind(insertSubscriptionReturning.For(p.dialect), p.dialect),
			subID, tid, req.EventType, req.DefName, req.EntryPoint, inputTemplateStr, req.FilterExpr, req.MaxRetries, now)
		if execErr != nil {
			p.logger.Error("event-triggers: create subscription", "error", execErr)
			p.writeError(w, 500, "failed to create subscription")
			return
		}
	} else {
		err = p.db.QueryRow(r.Context(), plugin.Rebind(insertSubscriptionReturning.For(p.dialect), p.dialect),
			tid, req.EventType, req.DefName, req.EntryPoint, inputTemplateStr, req.FilterExpr, req.MaxRetries, now).Scan(&subID)
	}
	if err != nil {
		p.logger.Error("event-triggers: create subscription", "error", err)
		p.writeError(w, 500, "failed to create subscription")
		return
	}

	maxRetries := 3
	if req.MaxRetries != nil {
		maxRetries = *req.MaxRetries
	}

	p.logger.Info("event-triggers: subscription created",
		"id", subID,
		"tenant", tid,
		"event_type", req.EventType,
		"def_name", req.DefName,
	)

	p.writeJSON(w, 201, subscriptionJSON{
		ID:            subID,
		TenantID:      tid,
		EventType:     req.EventType,
		DefName:       req.DefName,
		EntryPoint:    req.EntryPoint,
		InputTemplate: req.InputTemplate,
		FilterExpr:    req.FilterExpr,
		MaxRetries:    maxRetries,
		Enabled:       true,
		CreatedAt:     now,
	})
}

// ---- GET /api/events/subscriptions ----

func (p *Plugin) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.Query(r.Context(), `
		SELECT id, tenant_id, event_type, def_name, entry_point, input_template, filter_expr, max_retries, enabled, created_at
		FROM event_subscriptions
		WHERE tenant_id = $1
		ORDER BY created_at DESC
	`, tid)
	if err != nil {
		p.logger.Error("event-triggers: list subscriptions", "error", err)
		p.writeError(w, 500, "failed to list subscriptions")
		return
	}
	defer rows.Close()

	var subscriptions []subscriptionJSON
	for rows.Next() {
		var (
			s                subscriptionJSON
			inputTemplateRaw []byte
		)
		if err := rows.Scan(&s.ID, &s.TenantID, &s.EventType, &s.DefName,
			&s.EntryPoint, &inputTemplateRaw, &s.FilterExpr, &s.MaxRetries, &s.Enabled, &s.CreatedAt); err != nil {
			p.logger.Error("event-triggers: scan subscription", "error", err)
			continue
		}
		s.InputTemplate = json.RawMessage(inputTemplateRaw)
		subscriptions = append(subscriptions, s)
	}

	if subscriptions == nil {
		subscriptions = []subscriptionJSON{}
	}

	p.writeJSON(w, 200, subscriptions)
}

// ---- DELETE /api/events/subscriptions/{id} ----

func (p *Plugin) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid subscription id")
		return
	}

	rows, err := p.db.Exec(r.Context(), `
		DELETE FROM event_subscriptions
		WHERE id = $1 AND tenant_id = $2
	`, id, tid)
	if err != nil {
		p.logger.Error("event-triggers: delete subscription", "error", err)
		p.writeError(w, 500, "failed to delete subscription")
		return
	}
	if rows == 0 {
		p.writeError(w, 404, "subscription not found")
		return
	}

	p.logger.Info("event-triggers: subscription deleted", "id", id, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}

// ---- POST /api/events/{event_id}/retry ----

type retryEventResponse struct {
	Status      string `json:"status"`
	EventID     string `json:"event_id"`
	WorkflowsStarted int `json:"workflows_started"`
}

// handleRetryEvent replays a dead-lettered or failed event by resetting its
// processing state and immediately re-attempting dispatch to subscriptions.
func (p *Plugin) handleRetryEvent(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	eventIDStr := r.PathValue("event_id")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		p.writeError(w, 400, "invalid event id")
		return
	}

	// Look up the event to verify it exists and is eligible for retry.
	var (
		currentStatus string
		eventType     string
		eventDataRaw  []byte
	)
	err = p.db.QueryRow(r.Context(), `
		SELECT COALESCE(status, 'pending'), event_type, event_data
		FROM ingested_events
		WHERE id = $1 AND tenant_id = $2
	`, eventID, tid).Scan(&currentStatus, &eventType, &eventDataRaw)
	if err == sql.ErrNoRows {
		p.writeError(w, 404, "event not found")
		return
	}
	if err != nil {
		p.logger.Error("event-triggers: query event for retry", "error", err)
		p.writeError(w, 500, "failed to query event")
		return
	}

	// Only retry if the event is dead-lettered or has an error.
	if currentStatus != "dead_letter" && currentStatus != "error" {
		p.writeError(w, 400, fmt.Sprintf("event status is %q, must be %q or %q", currentStatus, "dead_letter", "error"))
		return
	}

	// Reset processing state.
	_, err = p.db.Exec(r.Context(), `
		UPDATE ingested_events
		SET processed = false, status = 'pending', retry_count = 0, error_msg = NULL, last_retry_at = NULL
		WHERE id = $1 AND tenant_id = $2
	`, eventID, tid)
	if err != nil {
		p.logger.Error("event-triggers: reset event for retry", "error", err)
		p.writeError(w, 500, "failed to reset event")
		return
	}

	// Parse event data back into map for dispatch.
	var eventData map[string]interface{}
	if err := json.Unmarshal(eventDataRaw, &eventData); err != nil {
		eventData = make(map[string]interface{})
	}

	// Re-dispatch to matching subscriptions.
	matched, err := triggerMatchingWorkflows(r.Context(), p.db, p.logger, p.env, eventID, tid, eventType, eventData)
	if err != nil {
		p.logger.Error("event-triggers: retry dispatch", "error", err)
		p.db.Exec(r.Context(), `
			UPDATE ingested_events SET error_msg = $1 WHERE id = $2
		`, "retry dispatch failed: "+err.Error(), eventID)
		p.writeError(w, 500, "retry dispatch failed")
		return
	}

	// Also signal any awaiting workflows so they wake up promptly.
	if len(eventDataRaw) > 0 {
		signalAwaiters(r.Context(), p.db, p.logger, p.env, tid, eventType, string(eventDataRaw))
	}

	if matched > 0 {
		// Mark as completed since at least one workflow was started.
		p.db.Exec(r.Context(), `
			UPDATE ingested_events
			SET processed = true, status = 'completed', error_msg = NULL
			WHERE id = $1
		`, eventID)
	}

	p.logger.Info("event-triggers: event retried",
		"event_id", eventID,
		"tenant", tid,
		"event_type", eventType,
		"workflows_started", matched,
	)

	p.writeJSON(w, 200, retryEventResponse{
		Status:           "retried",
		EventID:          eventID.String(),
		WorkflowsStarted: matched,
	})
}
