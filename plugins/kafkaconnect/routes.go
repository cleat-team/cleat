package kafkaconnect

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/durable/internal/auth"
)

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("kafka-connect: nil mux")
	}
	mux.HandleFunc("POST /kafka/configs", p.handleCreateConfig)
	mux.HandleFunc("GET /kafka/configs", p.handleListConfigs)
	mux.HandleFunc("DELETE /kafka/configs/{id}", p.handleDeleteConfig)
	return nil
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

// ---- types ----

type kafkaConfigJSON struct {
	ID            uuid.UUID `json:"id"`
	TenantID      uuid.UUID `json:"tenant_id"`
	Name          string    `json:"name"`
	Brokers       string    `json:"brokers"`
	Topic         string    `json:"topic"`
	ConsumerGroup string    `json:"consumer_group"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type createConfigRequest struct {
	Name          string `json:"name"`
	Brokers       string `json:"brokers"`
	Topic         string `json:"topic"`
	ConsumerGroup string `json:"consumer_group,omitempty"`
}

// ---- POST /kafka/configs ----

func (p *Plugin) handleCreateConfig(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("kafka-connect: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req createConfigRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}
	if req.Name == "" {
		p.writeError(w, 400, "name is required")
		return
	}
	if req.Brokers == "" {
		p.writeError(w, 400, "brokers is required")
		return
	}
	if req.Topic == "" {
		p.writeError(w, 400, "topic is required")
		return
	}

	consumerGroup := req.ConsumerGroup
	if consumerGroup == "" {
		consumerGroup = "cleat-consumer"
	}

	id := uuid.New()
	now := time.Now()

	_, err = p.db.ExecContext(r.Context(), `
		INSERT INTO kafka_config (tenant_id, id, name, brokers, topic, consumer_group, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, true, $7, $7)
	`, tid, id, req.Name, req.Brokers, req.Topic, consumerGroup, now)
	if err != nil {
		p.logger.Error("kafka-connect: create config", "error", err)
		p.writeError(w, 500, "failed to create config")
		return
	}

	p.logger.Info("kafka-connect: config created", "id", id, "tenant", tid)

	p.writeJSON(w, 201, kafkaConfigJSON{
		ID:            id,
		TenantID:      tid,
		Name:          req.Name,
		Brokers:       req.Brokers,
		Topic:         req.Topic,
		ConsumerGroup: consumerGroup,
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
}

// ---- GET /kafka/configs ----

func (p *Plugin) handleListConfigs(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.QueryContext(r.Context(), `
		SELECT id, name, brokers, topic, consumer_group, enabled, created_at, updated_at
		FROM kafka_config
		WHERE tenant_id = $1
		ORDER BY created_at DESC
	`, tid)
	if err != nil {
		p.logger.Error("kafka-connect: list configs", "error", err)
		p.writeError(w, 500, "failed to list configs")
		return
	}
	defer rows.Close()

	var configs []kafkaConfigJSON
	for rows.Next() {
		var c kafkaConfigJSON
		if err := rows.Scan(&c.ID, &c.Name, &c.Brokers, &c.Topic, &c.ConsumerGroup, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
			p.logger.Error("kafka-connect: scan config", "error", err)
			continue
		}
		c.TenantID = tid
		configs = append(configs, c)
	}

	if configs == nil {
		configs = []kafkaConfigJSON{}
	}

	p.writeJSON(w, 200, configs)
}

// ---- DELETE /kafka/configs/{id} ----

func (p *Plugin) handleDeleteConfig(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid config id")
		return
	}

	result, err := p.db.ExecContext(r.Context(), `
		DELETE FROM kafka_config
		WHERE id = $1 AND tenant_id = $2
	`, id, tid)
	if err != nil {
		p.logger.Error("kafka-connect: delete config", "error", err)
		p.writeError(w, 500, "failed to delete config")
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		p.writeError(w, 404, "config not found")
		return
	}

	p.logger.Info("kafka-connect: config deleted", "id", id, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}
