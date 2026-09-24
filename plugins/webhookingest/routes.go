package webhookingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/eventtriggers"
	"github.com/google/uuid"
)

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("webhook-ingest: nil mux")
	}
	// Inbound webhook endpoint -- no tenant auth required.
	// Source ID in the URL identifies the tenant.
	mux.HandleFunc("POST /ingest/{source_id}", p.handleIngestWebhook)

	// Management endpoints -- require tenant auth.
	mux.HandleFunc("GET /ingest/sources", p.handleListSources)
	mux.HandleFunc("POST /ingest/sources", p.handleCreateSource)
	mux.HandleFunc("GET /ingest/sources/{id}", p.handleGetSource)
	mux.HandleFunc("DELETE /ingest/sources/{id}", p.handleDeleteSource)
	mux.HandleFunc("GET /ingest/events", p.handleListEvents)
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

// WebhookIngestSecretName is the tenant-secret name a webhook_sources row's
// signing secret is stored under. cleat#1992.
//
// PER-SOURCE, NOT PER-TENANT: a tenant can register more than one source (own
// id, name, source_type), so a single fixed name would collide across a
// tenant's own sources -- tenant secrets are keyed (tenant_id, name), a
// singleton per name. Keying by source id preserves that with no product
// change. Mirrors plugins/notifications/routes.go's WebhookSecretName.
//
// EXPORTED, not package-private: tests/plugin-harness may need the exact same
// name a test seeds under to be readable back by handleIngestWebhook.
func WebhookIngestSecretName(id uuid.UUID) string {
	return "webhook-ingest.source_secret." + id.String()
}

// ---- types ----

type webhookSourceJSON struct {
	ID               uuid.UUID `json:"id"`
	TenantID         uuid.UUID `json:"tenant_id"`
	Name             string    `json:"name"`
	SourceType       string    `json:"source_type"`
	SecretConfigured bool      `json:"secret_configured"`
	Enabled          bool      `json:"enabled"`
	SignalWorkflowID string    `json:"signal_workflow_id,omitempty"`
	SignalName       string    `json:"signal_name,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type createSourceRequest struct {
	Name             string        `json:"name"`
	SourceType       string        `json:"source_type"`
	Secret           plugin.Secret `json:"secret,omitempty"`
	SignalWorkflowID string        `json:"signal_workflow_id,omitempty"`
	SignalName       string        `json:"signal_name,omitempty"`
}

type webhookEventJSON struct {
	ID         uuid.UUID       `json:"id"`
	SourceID   uuid.UUID       `json:"source_id"`
	TenantID   uuid.UUID       `json:"tenant_id"`
	EventType  string          `json:"event_type"`
	Headers    json.RawMessage `json:"headers"`
	Payload    json.RawMessage `json:"payload"`
	ReceivedAt time.Time       `json:"received_at"`
	Processed  bool            `json:"processed"`
}

// ---- POST /ingest/{source_id} ----

func (p *Plugin) handleIngestWebhook(w http.ResponseWriter, r *http.Request) {
	sourceIDStr := r.PathValue("source_id")
	sourceID, err := uuid.Parse(sourceIDStr)
	if err != nil {
		p.writeError(w, 400, "invalid source id")
		return
	}

	// Look up the webhook source.
	//
	// A NAMED cross-tenant read, bound to a SEPARATE variable. cleat#1538.
	//
	// POST /ingest/{source_id} is one of exactly two routes cmd/cleat-worker
	// exempts from auth (main.go, the auth.Middleware call), because the caller
	// is the external system sending the webhook and holds no cleat credential.
	// So r.Context() carries no tenant, and this SELECT -- which has no tenant
	// predicate because the row is HOW the handler learns the tenant -- cannot
	// be scoped to one. webhook_sources carries a policy whose predicate RAISES
	// on an unset tenant, so without this the endpoint answers 500 to every
	// inbound webhook before reaching anything else.
	//
	// `discoverCtx :=`, never `ctx =` or a reassignment of r's context.
	// beginTenantTx tests the cross-tenant marker BEFORE the tenant one, so a
	// ForTenant inside this scope is silently ignored -- carrying the bypass
	// forward would run the event insert, the publish and the signal unscoped.
	// The same rule kafkaconnect's pollConfigs documents for its own discovery
	// query.
	discoverCtx := plugin.AcrossAllTenants(r.Context(),
		"webhook ingest: the source id identifies the tenant, so there is none to scope by")

	// COALESCE(signal_workflow_id, ''): the column is nullable with no
	// default (migrations.go v3) and handleCreateSource writes NULL for a
	// source created with no signal_workflow_id, but this scans into a plain
	// Go string -- an uncoalesced NULL fails every ingest on such a source
	// with "converting NULL to string is unsupported". background.go's
	// queryUnprocessedWebhookEvents already coalesced this column; these
	// three SELECTs (here, handleGetSource, handleListSources) had not.
	// Found running handleCreateSource+handleIngestWebhook against a real
	// database for the first time (cleat#1992's dialect coverage) -- the
	// in-memory fake driver has no NULL to fail to scan.
	var source webhookSourceJSON
	err = plugin.ScanRow(p.db.QueryRow(discoverCtx, plugin.Rebind(`
		SELECT id, tenant_id, name, source_type, secret_configured, enabled, COALESCE(signal_workflow_id, ''), signal_name, created_at, updated_at
		FROM webhook_sources
		WHERE id = $1
	`, p.dialect), sourceID), &source.ID, &source.TenantID, &source.Name, &source.SourceType,
		&source.SecretConfigured, &source.Enabled, &source.SignalWorkflowID, &source.SignalName,
		&source.CreatedAt, &source.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		p.writeError(w, 404, "source not found")
		return
	}
	if err != nil {
		p.logger.Error("webhook-ingest: lookup source", "source_id", sourceID, "error", err)
		p.writeError(w, 500, "failed to look up source")
		return
	}

	if !source.Enabled {
		p.writeError(w, 403, "source is disabled")
		return
	}

	// Everything from here runs as the tenant the source belongs to. Derived
	// from r.Context() rather than from discoverCtx: narrowing a bypassed
	// context is a no-op, so a ForTenant built on discoverCtx would leave every
	// statement below cross-tenant while reading as though it were scoped.
	tenantCtx := plugin.ForTenant(r.Context(), source.TenantID)

	// Read the request body.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("webhook-ingest: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	// Verify HMAC-SHA256 signature. cleat#1992/#2172, owner decision (b): a
	// signing secret is now REQUIRED on every source -- handleCreateSource
	// refuses to create one without it -- so an unsigned request is never
	// accepted, on any source, unconditionally.
	//
	// secret_configured is checked first rather than assumed true: it is the
	// row's own record of what handleCreateSource actually enforced, and
	// refusing here rather than proceeding to a lookup keeps this handler
	// correct even against a row that somehow lacks one (there is no such
	// path today, but the check is what makes that a refusal instead of a
	// silent unsigned accept if one is ever introduced).
	//
	// The secret itself no longer lives on the row, so this cannot be read
	// off source.Secret. It MUST be readable through the tenant-secrets
	// store: a not-found, an empty value, or any other error all refuse the
	// request rather than falling back to unsigned. A naive `Reveal() != ""`
	// check on a value that failed to load reads as empty and would silently
	// accept the payload unsigned -- exactly the gap this branch exists to
	// close, and what pins it (see the regression test create-then-retire a
	// secret and confirm ingest refuses).
	if !source.SecretConfigured {
		p.logger.Error("webhook-ingest: source has no secret configured",
			"source_id", sourceID)
		p.writeError(w, 503, "signing secret not configured")
		return
	}
	secret, err := p.secrets.ForTenant(source.TenantID.String()).Get(tenantCtx, WebhookIngestSecretName(source.ID))
	if err != nil || secret == "" {
		p.logger.Error("webhook-ingest: signing secret unavailable",
			"source_id", sourceID, "error", err)
		p.writeError(w, 503, "signing secret unavailable")
		return
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		p.writeError(w, 401, "missing signature")
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		p.writeError(w, 401, "invalid signature")
		return
	}

	// Store request headers as JSON.
	headersMap := make(map[string]string)
	for k, v := range r.Header {
		headersMap[k] = strings.Join(v, ", ")
	}
	headersJSON, err := json.Marshal(headersMap)
	if err != nil {
		p.logger.Error("webhook-ingest: marshal headers", "error", err)
		p.writeError(w, 500, "failed to encode headers")
		return
	}

	// Store the payload. Try to parse as JSON first; if not valid JSON,
	// wrap it as a JSON string.
	var payloadJSON []byte
	if json.Valid(body) {
		payloadJSON = body
	} else {
		payloadJSON, _ = json.Marshal(string(body))
	}

	eventType := r.Header.Get("X-Github-Event")
	if eventType == "" {
		eventType = r.Header.Get("X-Event-Type")
	}
	if eventType == "" {
		eventType = "webhook"
	}

	eventID := uuid.New()
	now := time.Now()

	_, err = p.db.Exec(tenantCtx, plugin.Rebind(`
		INSERT INTO webhook_events (id, source_id, tenant_id, event_type, headers, payload, received_at, processed)
		VALUES ($1, $2, $3, $4, $5, $6, $7, false)
	`, p.dialect), eventID, sourceID, source.TenantID, eventType, string(headersJSON), string(payloadJSON), now)
	if err != nil {
		p.logger.Error("webhook-ingest: store event", "error", err)
		p.writeError(w, 500, "failed to store event")
		return
	}

	p.logger.Info("webhook-ingest: event received",
		"event_id", eventID,
		"source_id", sourceID,
		"tenant", source.TenantID,
		"event_type", eventType,
	)

	// ---- Publish as an event through the event-triggers system ----

	// Build event data that includes both headers and payload.
	payloadData := webhookPayload(body)

	eventData := map[string]any{
		"source_id":   sourceID.String(),
		"source_name": source.Name,
		"webhook_id":  eventID.String(),
		"event_type":  eventType,
		"headers":     headersMap,
		"payload":     payloadData,
	}

	eventDataJSON, err := json.Marshal(eventData)
	if err != nil {
		p.logger.Error("webhook-ingest: marshal event data", "error", err)
		p.writeError(w, 500, "failed to build event")
		return
	}

	matched, pubErr := eventtriggers.PublishEvent(
		tenantCtx, p.db, p.logger, p.env,
		eventID, source.TenantID, eventType, eventDataJSON,
	)
	if pubErr != nil {
		p.logger.Error("webhook-ingest: publish event failed", "error", pubErr)
	} else if matched > 0 {
		p.logger.Info("webhook-ingest: event triggered workflows",
			"event_id", eventID,
			"workflows_started", matched,
		)
	}

	// Also deliver a signal if this source is bound to a workflow (legacy path).
	if source.SignalWorkflowID != "" {
		signalPayload := map[string]any{
			"source_id":   sourceID.String(),
			"event_id":    eventID.String(),
			"event_type":  eventType,
			"received_at": now.Format(time.RFC3339),
		}
		if json.Valid(body) {
			signalPayload["payload"] = json.RawMessage(body)
		} else {
			signalPayload["payload"] = string(body)
		}
		payloadBytes, _ := json.Marshal(signalPayload)
		signalName := source.SignalName
		if signalName == "" {
			signalName = "webhook_received"
		}
		if p.env != nil && p.env.SignalWorkflow != nil {
			if serr := p.env.SignalWorkflow(tenantCtx, source.SignalWorkflowID, signalName, string(payloadBytes)); serr != nil {
				p.logger.Error("webhook-ingest: signal delivery failed",
					"workflow_id", source.SignalWorkflowID,
					"error", serr,
				)
			} else {
				p.db.Exec(tenantCtx, plugin.Rebind(`
					UPDATE webhook_events SET processed = true, status = 'completed' WHERE id = $1
				`, p.dialect), eventID)
				p.logger.Info("webhook-ingest: signal delivered",
					"workflow_id", source.SignalWorkflowID,
					"event_id", eventID,
				)
			}
		}
	}

	p.writeJSON(w, 201, map[string]any{
		"id":         eventID,
		"event_type": eventType,
		"received":   true,
	})
}

// ---- GET /ingest/sources ----

func (p *Plugin) handleListSources(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.Query(r.Context(), plugin.Rebind(`
		SELECT id, tenant_id, name, source_type, secret_configured, enabled, COALESCE(signal_workflow_id, ''), signal_name, created_at, updated_at
		FROM webhook_sources
		WHERE tenant_id = $1
		ORDER BY created_at DESC
	`, p.dialect), tid)
	if err != nil {
		p.logger.Error("webhook-ingest: list sources", "error", err)
		p.writeError(w, 500, "failed to list sources")
		return
	}
	defer rows.Close()

	var sources []webhookSourceJSON
	for rows.Next() {
		var s webhookSourceJSON
		if err := plugin.ScanRow(rows, &s.ID, &s.TenantID, &s.Name, &s.SourceType,
			&s.SecretConfigured, &s.Enabled, &s.SignalWorkflowID, &s.SignalName,
			&s.CreatedAt, &s.UpdatedAt); err != nil {
			p.logger.Error("webhook-ingest: scan source", "error", err)
			continue
		}
		sources = append(sources, s)
	}

	if sources == nil {
		sources = []webhookSourceJSON{}
	}

	p.writeJSON(w, 200, sources)
}

// ---- POST /ingest/sources ----

func (p *Plugin) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("webhook-ingest: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req createSourceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}
	if req.Name == "" {
		p.writeError(w, 400, "name is required")
		return
	}
	if req.SourceType == "" {
		req.SourceType = "generic"
	}
	// A signing secret is REQUIRED, not optional. cleat#1992/#2172, owner
	// decision (b): every source must verify its inbound requests: an
	// unsigned source can no longer be created, closing off the class of
	// bug this PR's part (a) refuses at ingest time -- there, the row already
	// existed unsigned; here, it is never allowed to.
	if req.Secret.Reveal() == "" {
		p.writeError(w, 400, "secret is required")
		return
	}

	id := uuid.New()
	now := time.Now()
	const secretConfigured = true

	var signalWorkflowID any
	if req.SignalWorkflowID != "" {
		signalWorkflowID = req.SignalWorkflowID
	}
	signalName := req.SignalName
	if signalName == "" {
		signalName = "webhook_received"
	}

	// The secret is written FIRST. If it fails, nothing else has happened --
	// no orphaned source row. If the INSERT below fails after this succeeds,
	// the secret is orphaned under a name no source row references; harmless
	// (unreachable, never resolved by anything) but logged so it is not a
	// silent leak of key material nobody can account for. cleat#1992, same
	// shape as plugins/notifications/routes.go's handleCreateWebhook.
	if err := p.secrets.Put(r.Context(), WebhookIngestSecretName(id), req.Secret.Reveal()); err != nil {
		p.logger.Error("webhook-ingest: store source secret", "error", err)
		p.writeError(w, 500, "failed to store secret")
		return
	}

	// Every placeholder numbered once, strictly increasing: $6 named twice
	// (for created_at and updated_at) would rebind to two SEPARATE "?" on
	// MySQL, in TEXTUAL order -- plugin.Rebind replaces every $N occurrence
	// positionally, not by its number (see CLAUDE.md's "MySQL binds `?` by
	// APPEARANCE") -- while PostgreSQL and SQL Server bind by the number
	// itself. The only ordering that satisfies both is one placeholder per
	// argument, numbered in the same order the arguments are passed, so `now`
	// is passed twice ($6 and $7) rather than reused. Found running this
	// INSERT against real MySQL for the first time (cleat#1992's dialect
	// coverage) -- the in-memory fake driver binds by Ordinal and cannot see
	// this class of defect.
	_, err = p.db.Exec(r.Context(), plugin.Rebind(`
		INSERT INTO webhook_sources (tenant_id, id, name, source_type, secret_configured, enabled, created_at, updated_at, signal_workflow_id, signal_name)
		VALUES ($1, $2, $3, $4, $5, true, $6, $7, $8, $9)
	`, p.dialect), tid, id, req.Name, req.SourceType, secretConfigured, now, now, signalWorkflowID, signalName)
	if err != nil {
		p.logger.Error("webhook-ingest: create source",
			"error", err, "orphaned_secret_configured", secretConfigured)
		p.writeError(w, 500, "failed to create source")
		return
	}

	// Build the endpoint URL for this source.
	endpointURL := fmt.Sprintf("/ingest/%s", id)

	p.logger.Info("webhook-ingest: source created", "id", id, "tenant", tid)

	p.writeJSON(w, 201, map[string]any{
		"id":                 id,
		"tenant_id":          tid,
		"name":               req.Name,
		"source_type":        req.SourceType,
		"secret_configured":  secretConfigured,
		"signal_workflow_id": req.SignalWorkflowID,
		"signal_name":        signalName,
		"enabled":            true,
		"endpoint_url":       endpointURL,
		"created_at":         now,
		"updated_at":         now,
	})
}

// ---- GET /ingest/sources/{id} ----

func (p *Plugin) handleGetSource(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid source id")
		return
	}

	var s webhookSourceJSON
	err = plugin.ScanRow(p.db.QueryRow(r.Context(), plugin.Rebind(`
		SELECT id, tenant_id, name, source_type, secret_configured, enabled, COALESCE(signal_workflow_id, ''), signal_name, created_at, updated_at
		FROM webhook_sources
		WHERE id = $1 AND tenant_id = $2
	`, p.dialect), id, tid), &s.ID, &s.TenantID, &s.Name, &s.SourceType,
		&s.SecretConfigured, &s.Enabled, &s.SignalWorkflowID, &s.SignalName,
		&s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		p.writeError(w, 404, "source not found")
		return
	}
	if err != nil {
		p.logger.Error("webhook-ingest: get source", "error", err)
		p.writeError(w, 500, "failed to get source")
		return
	}

	p.writeJSON(w, 200, s)
}

// ---- DELETE /ingest/sources/{id} ----

func (p *Plugin) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid source id")
		return
	}

	rows, err := p.db.Exec(r.Context(), plugin.Rebind(`
		DELETE FROM webhook_sources
		WHERE id = $1 AND tenant_id = $2
	`, p.dialect), id, tid)
	if err != nil {
		p.logger.Error("webhook-ingest: delete source", "error", err)
		p.writeError(w, 500, "failed to delete source")
		return
	}
	if rows == 0 {
		p.writeError(w, 404, "source not found")
		return
	}

	// Best-effort: the source row is already gone, which is the operation
	// the caller asked for and got. A failure here leaves a retired-but-not-
	// yet-retired secret with no source row pointing at it -- inert, since
	// nothing looks it up by an id that no longer exists -- so it is logged
	// rather than turned into a 500 for an otherwise-successful delete.
	// cleat#1992, same shape as plugins/notifications/routes.go's
	// handleDeleteWebhook.
	if _, err := p.secrets.Retire(r.Context(), WebhookIngestSecretName(id)); err != nil {
		p.logger.Error("webhook-ingest: retire source secret after delete", "error", err, "id", id)
	}

	p.logger.Info("webhook-ingest: source deleted", "id", id, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}

// ---- GET /ingest/events ----

func (p *Plugin) handleListEvents(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	query := `
		SELECT id, source_id, tenant_id, event_type, headers, payload, received_at, processed
		FROM webhook_events
		WHERE tenant_id = $1
	`
	args := []any{tid}
	argIdx := 2

	if sourceIDStr := r.URL.Query().Get("source_id"); sourceIDStr != "" {
		sourceID, err := uuid.Parse(sourceIDStr)
		if err == nil {
			query += fmt.Sprintf(" AND source_id = $%d", argIdx)
			args = append(args, sourceID)
			argIdx++
		}
	}

	if eventType := r.URL.Query().Get("event_type"); eventType != "" {
		query += fmt.Sprintf(" AND event_type = $%d", argIdx)
		args = append(args, eventType)
		argIdx++
	}

	if processedStr := r.URL.Query().Get("processed"); processedStr != "" {
		processed := processedStr == "true"
		query += fmt.Sprintf(" AND processed = $%d", argIdx)
		args = append(args, processed)
		argIdx++
	}

	query += " ORDER BY received_at DESC"
	// plugin.LimitClause, not a literal "LIMIT $N": SQL Server has no LIMIT,
	// only OFFSET/FETCH after an ORDER BY (which this query already has).
	// cleat-review's re-check on #2198 found this endpoint 500ing on MSSQL --
	// the same bug as notifications/routes.go's handleListDeliveries, fixed
	// the same way #2191 already fixed it for /audit/events.
	query += " " + plugin.LimitClause(fmt.Sprintf("$%d", argIdx), p.dialect)
	args = append(args, 100)

	rows, err := p.db.Query(r.Context(), plugin.Rebind(query, p.dialect), args...)
	if err != nil {
		p.logger.Error("webhook-ingest: list events", "error", err)
		p.writeError(w, 500, "failed to list events")
		return
	}
	defer rows.Close()

	var events []webhookEventJSON
	for rows.Next() {
		var (
			e          webhookEventJSON
			headersRaw []byte
			payloadRaw []byte
		)
		if err := plugin.ScanRow(rows,
			&e.ID, &e.SourceID, &e.TenantID, &e.EventType,
			&headersRaw, &payloadRaw, &e.ReceivedAt, &e.Processed,
		); err != nil {
			p.logger.Error("webhook-ingest: scan event", "error", err)
			continue
		}
		e.Headers = json.RawMessage(headersRaw)
		e.Payload = json.RawMessage(payloadRaw)
		events = append(events, e)
	}

	if events == nil {
		events = []webhookEventJSON{}
	}

	p.writeJSON(w, 200, events)
}

// webhookPayload turns an inbound request body into the value that goes into
// the published event's "payload" key.
//
// It returns RAW BYTES for JSON, not a decoded value. Decoding into an `any`
// and letting the caller's json.Marshal re-encode it rewrote any number the
// webhook sent that float64 cannot hold exactly -- silently, and before the
// event reached any database, so converting the column could not have fixed
// it. A json.RawMessage inside a map[string]any is written back verbatim by
// encoding/json, so the payload a caller POSTed is the payload a subscriber
// sees. cleat#1641.
//
// It is a named function rather than four inline lines so that the property
// can be asserted without a database: the handler around it needs one, this
// does not, and the defect was never in the parts that do.
func webhookPayload(body []byte) any {
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	// Not JSON: carried as a string, which json.Marshal will quote and escape.
	return string(body)
}
