package notifications

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("notifications: nil mux")
	}
	mux.HandleFunc("POST /webhooks", p.handleCreateWebhook)
	mux.HandleFunc("GET /webhooks", p.handleListWebhooks)
	mux.HandleFunc("GET /webhooks/{id}", p.handleGetWebhook)
	mux.HandleFunc("PUT /webhooks/{id}", p.handleUpdateWebhook)
	mux.HandleFunc("DELETE /webhooks/{id}", p.handleDeleteWebhook)
	mux.HandleFunc("GET /webhooks/{id}/deliveries", p.handleListDeliveries)
	return nil
}

// ---- helpers ----

func (p *Plugin) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (p *Plugin) writeError(w http.ResponseWriter, status int, msg string) {
	p.writeJSON(w, status, map[string]string{"error": msg})
}

// WebhookSecretName is the tenant-secret name a webhook_config row's signing
// secret is stored under. cleat#1992.
//
// PER-WEBHOOK, NOT PER-TENANT: a tenant can register more than one webhook
// (own id, url, event filter), so a single fixed name would collide across a
// tenant's own webhooks -- tenant secrets are keyed (tenant_id, name), a
// singleton per name. Keying by webhook id preserves that with no product
// change. Mirrors plugins/datadogexport/routes.go's DatadogAPIKeySecretName.
func WebhookSecretName(id uuid.UUID) string {
	return "notifications.webhook_secret." + id.String()
}

// webhookExistsSQL returns dialect-specific SQL that checks whether a
// webhook_config row exists for a given (id, tenant_id) pair, scanned into a
// Go bool. `SELECT EXISTS(...)` as a top-level select list expression is
// valid PostgreSQL and MySQL but not T-SQL -- SQL Server has no boolean
// column type, so it needs `CASE WHEN EXISTS(...) THEN 1 ELSE 0 END`. Same
// shape, same reason, as plugin/migration.go's checkPluginMigrationSQL.
// Found by cleat-review running sendWebhook and handleListDeliveries against
// real SQL Server for the first time -- every call failed outright, since
// SELECT EXISTS(...) is not valid syntax there at all.
//
// deleted_at IS NULL: a soft-deleted webhook reads as gone (404/not found),
// the same as one that never existed, rather than as merely disabled --
// cleat#2220, matching the treatment cleat#2199 gave webhookingest's sources.
// Both of this function's callers (handleListDeliveries, sendWebhook) rely on
// this to refuse a deleted webhook's own sub-resources and new deliveries,
// not just the config row itself.
func webhookExistsSQL(d plugin.Dialect) string {
	if d == plugin.DialectMSSQL {
		return `SELECT CASE WHEN EXISTS(SELECT 1 FROM webhook_config WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL) THEN 1 ELSE 0 END`
	}
	return `SELECT EXISTS(SELECT 1 FROM webhook_config WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL)`
}

// ---- types ----

type webhookConfigJSON struct {
	ID               uuid.UUID `json:"id"`
	TenantID         uuid.UUID `json:"tenant_id"`
	URL              string    `json:"url"`
	SecretConfigured bool      `json:"secret_configured"`
	Events           []string  `json:"events"`
	Enabled          bool      `json:"enabled"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type createWebhookRequest struct {
	URL    string        `json:"url"`
	Secret plugin.Secret `json:"secret,omitempty"`
	Events []string      `json:"events"`
}

type updateWebhookRequest struct {
	URL     *string        `json:"url,omitempty"`
	Secret  *plugin.Secret `json:"secret,omitempty"`
	Events  *[]string      `json:"events,omitempty"`
	Enabled *bool          `json:"enabled,omitempty"`
}

type deliveryJSON struct {
	ID            uuid.UUID       `json:"id"`
	WebhookID     uuid.UUID       `json:"webhook_id"`
	EventType     string          `json:"event_type"`
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	AttemptCount  int             `json:"attempt_count"`
	LastAttemptAt *time.Time      `json:"last_attempt_at,omitempty"`
	NextAttemptAt *time.Time      `json:"next_attempt_at,omitempty"`
	DeliveredAt   *time.Time      `json:"delivered_at,omitempty"`
	ResponseCode  *int            `json:"response_code,omitempty"`
	ResponseBody  *string         `json:"response_body,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// ---- POST /webhooks ----

func (p *Plugin) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("notifications: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req createWebhookRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}
	if req.URL == "" {
		p.writeError(w, 400, "url is required")
		return
	}
	if req.Events == nil {
		req.Events = []string{}
	}
	// A signing secret is REQUIRED, not optional. cleat#1992/#2172, owner
	// decision (b): every webhook must sign the deliveries it sends, so the
	// receiving end can verify them. An unsigned webhook can no longer be
	// created.
	if req.Secret.Reveal() == "" {
		p.writeError(w, 400, "secret is required")
		return
	}

	eventsJSON, err := json.Marshal(req.Events)
	if err != nil {
		p.logger.Error("notifications: marshal events", "error", err)
		p.writeError(w, 500, "failed to encode events")
		return
	}

	id := uuid.New()
	now := time.Now()
	const secretConfigured = true

	// The row is written FIRST, the secret AFTER, with a compensating delete
	// if the secret write fails -- the opposite order from before cleat-review
	// on #2198. Secret-first meant every failed INSERT orphaned a secret; on
	// MySQL the INSERT below failed on EVERY call (the $6, $6 bug this same
	// change fixes), so it was not a rare edge case, it was every create.
	// Row-first still risks a row with secret_configured=true and no secret
	// if the Put fails, which the compensating delete below closes: nothing
	// is left with secretConfigured=true unless Put actually succeeded.
	//
	// Every placeholder numbered once, strictly increasing: $6 named twice
	// (for created_at and updated_at) would rebind to two SEPARATE "?" on
	// MySQL, in TEXTUAL order -- plugin.Rebind replaces every $N occurrence
	// positionally, not by its number (see CLAUDE.md's "MySQL binds `?` by
	// APPEARANCE") -- while PostgreSQL and SQL Server bind by the number
	// itself. The only ordering that satisfies both is one placeholder per
	// argument, numbered in the same order the arguments are passed, so `now`
	// is passed twice ($6 and $7) rather than reused. Same defect, same fix,
	// as plugins/webhookingest/routes.go's handleCreateSource -- found there
	// first; this one was missed in the same PR and caught by cleat-review
	// running POST /webhooks against real MySQL.
	_, err = p.db.Exec(r.Context(), plugin.Rebind(`
			INSERT INTO webhook_config (tenant_id, id, url, secret_configured, events, enabled, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, true, $6, $7)
		`, p.dialect), tid, id, req.URL, secretConfigured, string(eventsJSON), now, now)
	if err != nil {
		p.logger.Error("notifications: create webhook", "error", err)
		p.writeError(w, 500, "failed to create webhook")
		return
	}

	if err := p.secrets.Put(r.Context(), WebhookSecretName(id), req.Secret.Reveal()); err != nil {
		p.logger.Error("notifications: store webhook secret", "error", err)
		if _, delErr := p.db.Exec(r.Context(), plugin.Rebind(
			`DELETE FROM webhook_config WHERE id = $1`, p.dialect), id); delErr != nil {
			p.logger.Error("notifications: compensating delete after failed secret store",
				"id", id, "error", delErr)
		}
		p.writeError(w, 500, "failed to store secret")
		return
	}

	p.logger.Info("notifications: webhook created", "id", id, "tenant", tid)

	p.writeJSON(w, 201, webhookConfigJSON{
		ID:               id,
		TenantID:         tid,
		URL:              req.URL,
		SecretConfigured: secretConfigured,
		Events:           req.Events,
		Enabled:          true,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
}

// ---- GET /webhooks ----

func (p *Plugin) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.Query(r.Context(), plugin.Rebind(`
			SELECT id, url, secret_configured, events, enabled, created_at, updated_at
			FROM webhook_config
			WHERE tenant_id = $1 AND deleted_at IS NULL
			ORDER BY created_at DESC
		`, p.dialect), tid)
	if err != nil {
		p.logger.Error("notifications: list webhooks", "error", err)
		p.writeError(w, 500, "failed to list webhooks")
		return
	}
	defer rows.Close()

	var configs []webhookConfigJSON
	for rows.Next() {
		var (
			c         webhookConfigJSON
			eventsRaw []byte
		)
		if err := plugin.ScanRow(rows, &c.ID, &c.URL, &c.SecretConfigured, &eventsRaw, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
			p.logger.Error("notifications: scan webhook", "error", err)
			continue
		}
		c.TenantID = tid
		json.Unmarshal(eventsRaw, &c.Events)
		configs = append(configs, c)
	}

	if configs == nil {
		configs = []webhookConfigJSON{}
	}

	p.writeJSON(w, 200, configs)
}

// ---- GET /webhooks/{id} ----

func (p *Plugin) handleGetWebhook(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid webhook id")
		return
	}

	var (
		c         webhookConfigJSON
		eventsRaw []byte
	)
	err = plugin.ScanRow(p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, url, secret_configured, events, enabled, created_at, updated_at
			FROM webhook_config
			WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
		`, p.dialect), id, tid), &c.ID, &c.URL, &c.SecretConfigured, &eventsRaw, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		p.writeError(w, 404, "webhook not found")
		return
	}
	if err != nil {
		p.logger.Error("notifications: get webhook", "error", err)
		p.writeError(w, 500, "failed to get webhook")
		return
	}

	c.TenantID = tid
	json.Unmarshal(eventsRaw, &c.Events)

	p.writeJSON(w, 200, c)
}

// ---- PUT /webhooks/{id} ----

func (p *Plugin) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid webhook id")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("notifications: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req updateWebhookRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}

	// Build dynamic UPDATE query for the fields that are present.
	setClauses := []string{}
	args := []any{}
	argIdx := 1

	if req.URL != nil {
		setClauses = append(setClauses, fmt.Sprintf("url = $%d", argIdx))
		args = append(args, *req.URL)
		argIdx++
	}
	// A signing secret is REQUIRED, not optional (cleat#1992/#2172, owner
	// decision (b)): PUT can ROTATE it but can no longer clear it back to
	// unsigned, so secret_configured has nothing left to set to false and no
	// longer needs its own SET clause -- true from creation onward, always.
	if req.Secret != nil && req.Secret.Reveal() == "" {
		p.writeError(w, 400, "secret cannot be cleared")
		return
	}
	if req.Events != nil {
		eventsJSON, err := json.Marshal(*req.Events)
		if err != nil {
			p.logger.Error("notifications: marshal events", "error", err)
			p.writeError(w, 500, "failed to encode events")
			return
		}
		setClauses = append(setClauses, fmt.Sprintf("events = $%d", argIdx))
		args = append(args, string(eventsJSON))
		argIdx++
	}
	if req.Enabled != nil {
		setClauses = append(setClauses, fmt.Sprintf("enabled = $%d", argIdx))
		args = append(args, *req.Enabled)
		argIdx++
	}

	// A secret rotation carries no SET clause of its own any more -- it goes
	// through p.secrets, not this UPDATE -- so it no longer counts toward
	// setClauses, and the "nothing to update" check has to ask about it
	// separately or a PUT that rotates only the secret would be rejected.
	if len(setClauses) == 0 && req.Secret == nil {
		p.writeError(w, 400, "no fields to update")
		return
	}

	setClauses = append(setClauses, "updated_at = now()")
	args = append(args, id, tid)

	query := fmt.Sprintf(`
			UPDATE webhook_config
			SET %s
			WHERE id = $%d AND tenant_id = $%d AND deleted_at IS NULL
		`, joinSetClauses(setClauses), argIdx, argIdx+1)

	rows, err := p.db.Exec(r.Context(), plugin.Rebind(query, p.dialect), args...)
	if err != nil {
		p.logger.Error("notifications: update webhook", "error", err)
		p.writeError(w, 500, "failed to update webhook")
		return
	}
	if rows == 0 {
		p.writeError(w, 404, "webhook not found")
		return
	}

	// The secret write happens AFTER the row update confirms id belongs to
	// tid -- so a request naming another tenant's (or no) webhook id never
	// reaches p.secrets at all, rather than rotating a secret under the
	// caller's own tenant for an id that is not theirs. req.Secret == nil
	// means the field was omitted (no rotation); req.Secret.Reveal() == ""
	// was already rejected above, so every reachable call here is a rotation.
	if req.Secret != nil {
		if err := p.secrets.Put(r.Context(), WebhookSecretName(id), req.Secret.Reveal()); err != nil {
			p.logger.Error("notifications: rotate webhook secret", "error", err, "id", id)
			p.writeError(w, 500, "failed to store secret")
			return
		}
	}

	// Return the updated webhook config.
	var (
		c         webhookConfigJSON
		eventsRaw []byte
	)
	err = plugin.ScanRow(p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, url, secret_configured, events, enabled, created_at, updated_at
			FROM webhook_config
			WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
		`, p.dialect), id, tid), &c.ID, &c.URL, &c.SecretConfigured, &eventsRaw, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		p.logger.Error("notifications: re-fetch webhook", "error", err)
		p.writeError(w, 500, "failed to retrieve updated webhook")
		return
	}

	c.TenantID = tid
	json.Unmarshal(eventsRaw, &c.Events)

	p.writeJSON(w, 200, c)
}

// ---- DELETE /webhooks/{id} ----

func (p *Plugin) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid webhook id")
		return
	}

	// A SOFT delete, not a hard one. cleat#2220, matching cleat#2199's shape
	// for webhookingest's sources: a hard DELETE FROM webhook_config hit a
	// foreign key violation on PostgreSQL and SQL Server for any webhook that
	// had ever received a delivery (webhook_delivery.webhook_id REFERENCES
	// webhook_config(id), no ON DELETE action before migrations.go v7) and
	// silently orphaned the delivery rows on MySQL instead. Marking the row
	// deleted rather than removing it keeps it out of every read path
	// (webhookExistsSQL and the SELECTs above all filter deleted_at IS NULL)
	// while leaving its delivery history intact for GET .../deliveries and
	// for anything auditing what was sent before the webhook was removed.
	//
	// Cancelling the webhook's own pending/retrying deliveries in the SAME
	// transaction as the soft-delete, not as a separate step, mirrors
	// cleat#2199's handleDeleteSource exactly: a tenant deleting a webhook
	// is asking cleat to stop sending to it, and without this a delivery
	// already queued (or awaiting its next backoff retry) would still go out
	// after the delete -- background.go's own doc comments describe the
	// existing config-lookup and secret-lookup failure paths that already
	// existed if this were left to happen by the config row simply becoming
	// unreadable, none of which mark the delivery as anything other than
	// "still pending, retried forever". Setting status='cancelled' here
	// stops it at the source rather than relying on deliver() to fail its
	// way to the same place. queryDueDeliveries (background.go) carries an
	// independent deleted_at IS NULL guard on top of this, the same
	// defense-in-depth belt-and-suspenders shape #2199 used for
	// processBatch/awaitWebhook.
	tx, err := p.db.Begin(r.Context())
	if err != nil {
		p.logger.Error("notifications: begin delete transaction", "error", err)
		p.writeError(w, 500, "failed to delete webhook")
		return
	}

	rows, err := tx.Exec(r.Context(), plugin.Rebind(`
			UPDATE webhook_config
			SET enabled = false, deleted_at = now()
			WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
		`, p.dialect), id, tid)
	if err != nil {
		tx.Rollback()
		p.logger.Error("notifications: delete webhook", "error", err)
		p.writeError(w, 500, "failed to delete webhook")
		return
	}
	if rows == 0 {
		tx.Rollback()
		p.writeError(w, 404, "webhook not found")
		return
	}

	if _, err := tx.Exec(r.Context(), plugin.Rebind(`
			UPDATE webhook_delivery
			SET status = 'cancelled'
			WHERE webhook_id = $1 AND status IN ('pending', 'retrying')
		`, p.dialect), id); err != nil {
		tx.Rollback()
		p.logger.Error("notifications: cancel pending deliveries after delete", "error", err, "id", id)
		p.writeError(w, 500, "failed to delete webhook")
		return
	}

	if err := tx.Commit(); err != nil {
		p.logger.Error("notifications: commit delete webhook", "error", err)
		p.writeError(w, 500, "failed to delete webhook")
		return
	}

	// Best-effort, outside the transaction: the config row is already
	// soft-deleted, which is the operation the caller asked for and got. A
	// failure here leaves a retired-but-not-yet-retired secret pointing at a
	// webhook id no route or sweep will read again (every lookup filters
	// deleted_at IS NULL) -- inert rather than reachable -- so it is logged
	// rather than turned into a 500 for an otherwise-successful delete.
	if _, err := p.secrets.Retire(r.Context(), WebhookSecretName(id)); err != nil {
		p.logger.Error("notifications: retire webhook secret after delete", "error", err, "id", id)
	}

	p.logger.Info("notifications: webhook deleted", "id", id, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}

// ---- GET /webhooks/{id}/deliveries ----

func (p *Plugin) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	webhookID, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid webhook id")
		return
	}

	// Verify the webhook belongs to the tenant.
	var exists bool
	err = p.db.QueryRow(r.Context(), plugin.Rebind(webhookExistsSQL(p.dialect), p.dialect),
		webhookID, tid).Scan(&exists)
	if err != nil {
		p.logger.Error("notifications: verify webhook", "error", err)
		p.writeError(w, 500, "failed to verify webhook")
		return
	}
	if !exists {
		p.writeError(w, 404, "webhook not found")
		return
	}

	query := `
			SELECT id, webhook_id, event_type, payload, status, attempt_count,
			       last_attempt_at, next_attempt_at, delivered_at,
			       response_code, response_body, created_at
			FROM webhook_delivery
			WHERE webhook_id = $1
		`
	args := []any{webhookID}
	argIdx := 2

	if statusFilter := r.URL.Query().Get("status"); statusFilter != "" {
		query += fmt.Sprintf(" AND status = $%d", argIdx)
		args = append(args, statusFilter)
		argIdx++
	}

	query += " ORDER BY created_at DESC"
	// plugin.LimitClause, not a literal "LIMIT $N": SQL Server has no LIMIT,
	// only OFFSET/FETCH after an ORDER BY (which this query already has).
	// cleat-review's re-check on #2198 found this endpoint 500ing on MSSQL
	// with "Incorrect syntax near 'LIMIT'" -- the same bug #2191 already
	// fixed the same way for /audit/events.
	query += " " + plugin.LimitClause(fmt.Sprintf("$%d", argIdx), p.dialect)
	args = append(args, 100)

	rows, err := p.db.Query(r.Context(), plugin.Rebind(query, p.dialect), args...)
	if err != nil {
		p.logger.Error("notifications: list deliveries", "error", err)
		p.writeError(w, 500, "failed to list deliveries")
		return
	}
	defer rows.Close()

	var deliveries []deliveryJSON
	for rows.Next() {
		var (
			d             deliveryJSON
			payloadRaw    []byte
			lastAttemptAt sql.NullTime
			nextAttemptAt sql.NullTime
			deliveredAt   sql.NullTime
			responseCode  sql.NullInt64
			responseBody  sql.NullString
		)
		if err := plugin.ScanRow(rows,
			&d.ID, &d.WebhookID, &d.EventType, &payloadRaw,
			&d.Status, &d.AttemptCount,
			&lastAttemptAt, &nextAttemptAt, &deliveredAt,
			&responseCode, &responseBody, &d.CreatedAt,
		); err != nil {
			p.logger.Error("notifications: scan delivery", "error", err)
			continue
		}
		d.Payload = json.RawMessage(payloadRaw)
		if lastAttemptAt.Valid {
			d.LastAttemptAt = &lastAttemptAt.Time
		}
		if nextAttemptAt.Valid {
			d.NextAttemptAt = &nextAttemptAt.Time
		}
		if deliveredAt.Valid {
			d.DeliveredAt = &deliveredAt.Time
		}
		if responseCode.Valid {
			v := int(responseCode.Int64)
			d.ResponseCode = &v
		}
		if responseBody.Valid {
			d.ResponseBody = &responseBody.String
		}
		deliveries = append(deliveries, d)
	}

	if deliveries == nil {
		deliveries = []deliveryJSON{}
	}

	p.writeJSON(w, 200, deliveries)
}

// joinSetClauses joins SET clause fragments with ", ".
func joinSetClauses(clauses []string) string {
	result := ""
	for i, c := range clauses {
		if i > 0 {
			result += ", "
		}
		result += c
	}
	return result
}
