package eventtriggers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/durable/internal/auth"
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
}

type subscriptionJSON struct {
	ID            uuid.UUID       `json:"id"`
	TenantID      uuid.UUID       `json:"tenant_id"`
	EventType     string          `json:"event_type"`
	DefName       string          `json:"def_name"`
	EntryPoint    string          `json:"entry_point"`
	InputTemplate json.RawMessage `json:"input_template"`
	FilterExpr    string          `json:"filter_expr"`
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

	eventDataJSON, err := json.Marshal(req.Data)
	if err != nil {
		p.logger.Error("event-triggers: marshal event data", "error", err)
		p.writeError(w, 500, "failed to encode event data")
		return
	}

	// Insert event with idempotency — ON CONFLICT DO NOTHING prevents
	// duplicate processing of the same event ID.
	result, err := p.db.ExecContext(r.Context(), `
		INSERT INTO ingested_events (id, tenant_id, event_type, event_data, received_at, processed)
		VALUES ($1, $2, $3, $4, NOW(), false)
		ON CONFLICT (id) DO NOTHING
	`, eventID, tid, req.EventType, string(eventDataJSON))
	if err != nil {
		p.logger.Error("event-triggers: store event", "error", err)
		p.writeError(w, 500, "failed to store event")
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		// Event already ingested — idempotent return.
		p.writeJSON(w, 200, publishEventResponse{Status: "duplicate"})
		return
	}

	p.logger.Info("event-triggers: event stored",
		"event_id", eventID,
		"tenant", tid,
		"event_type", req.EventType,
	)

	// Query matching subscriptions.
	rows2, err := p.db.QueryContext(r.Context(), `
		SELECT id, tenant_id, event_type, def_name, entry_point, input_template, filter_expr, enabled, created_at
		FROM event_subscriptions
		WHERE tenant_id = $1 AND event_type = $2 AND enabled = true
	`, tid, req.EventType)
	if err != nil {
		p.logger.Error("event-triggers: query subscriptions", "error", err)
		// Event is stored but we failed to match — mark it for dead-letter.
		p.db.ExecContext(r.Context(), `UPDATE ingested_events SET error_msg = $1 WHERE id = $2`,
			"failed to query subscriptions: "+err.Error(), eventID)
		p.writeJSON(w, 200, publishEventResponse{Status: "published", Matched: 0})
		return
	}
	defer rows2.Close()

	matched := 0
	for rows2.Next() {
		var (
			sub              subscriptionJSON
			inputTemplateRaw []byte
		)
		if err := rows2.Scan(&sub.ID, &sub.TenantID, &sub.EventType, &sub.DefName,
			&sub.EntryPoint, &inputTemplateRaw, &sub.FilterExpr, &sub.Enabled, &sub.CreatedAt); err != nil {
			p.logger.Error("event-triggers: scan subscription", "error", err)
			continue
		}
		sub.InputTemplate = json.RawMessage(inputTemplateRaw)

		// Evaluate filter expression.
		if sub.FilterExpr != "" && sub.FilterExpr != "true" {
			ok, err := EvaluateFilter(sub.FilterExpr, req.Data)
			if err != nil {
				p.logger.Error("event-triggers: filter evaluation error",
					"subscription_id", sub.ID,
					"filter_expr", sub.FilterExpr,
					"error", err,
				)
				continue
			}
			if !ok {
				p.logger.Debug("event-triggers: filter did not match",
					"subscription_id", sub.ID,
					"event_id", eventID,
				)
				continue
			}
		}

		// Build workflow input from input_template merged with event data.
		inputJSON, err := mergeInputAndTemplate(sub.InputTemplate, req.Data)
		if err != nil {
			p.logger.Error("event-triggers: build workflow input", "error", err)
			continue
		}

		if p.env != nil && p.env.StartWorkflow != nil {
			runID, err := p.env.StartWorkflow(r.Context(), sub.DefName, inputJSON)
			if err != nil {
				p.logger.Error("event-triggers: start workflow failed",
					"def_name", sub.DefName,
					"event_id", eventID,
					"error", err,
				)
				continue
			}
			matched++
			p.logger.Info("event-triggers: workflow started",
				"def_name", sub.DefName,
				"run_id", runID,
				"event_id", eventID,
			)
		}
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
	err = p.db.QueryRowContext(r.Context(), `
		INSERT INTO event_subscriptions (tenant_id, event_type, def_name, entry_point, input_template, filter_expr, enabled, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, true, $7)
		RETURNING id
	`, tid, req.EventType, req.DefName, req.EntryPoint, inputTemplateStr, req.FilterExpr, now).Scan(&subID)
	if err != nil {
		p.logger.Error("event-triggers: create subscription", "error", err)
		p.writeError(w, 500, "failed to create subscription")
		return
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

	rows, err := p.db.QueryContext(r.Context(), `
		SELECT id, tenant_id, event_type, def_name, entry_point, input_template, filter_expr, enabled, created_at
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
			&s.EntryPoint, &inputTemplateRaw, &s.FilterExpr, &s.Enabled, &s.CreatedAt); err != nil {
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

	result, err := p.db.ExecContext(r.Context(), `
		DELETE FROM event_subscriptions
		WHERE id = $1 AND tenant_id = $2
	`, id, tid)
	if err != nil {
		p.logger.Error("event-triggers: delete subscription", "error", err)
		p.writeError(w, 500, "failed to delete subscription")
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		p.writeError(w, 404, "subscription not found")
		return
	}

	p.logger.Info("event-triggers: subscription deleted", "id", id, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}
