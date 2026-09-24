package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry. The plugin name is implicit -- each plugin gets its own
// scope, so function names need not be globally unique.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("notifications: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{Name: "send_webhook", Idempotent: false}, p.sendWebhook); err != nil {
		return err
	}
	return nil
}

// ---- Input/output types ----

type sendWebhookInput struct {
	WebhookID uuid.UUID       `json:"webhook_id"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
}

type sendWebhookOutput struct {
	DeliveryID uuid.UUID `json:"delivery_id"`
}

// ---- Host functions ----

// sendWebhook triggers a webhook delivery from within a workflow. It creates a
// delivery row in 'pending' status, which the background retry loop will pick
// up and deliver. Returns the delivery ID.
func (p *Plugin) sendWebhook(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return "", fmt.Errorf("notifications: no tenant context")
	}

	var input sendWebhookInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("notifications: invalid input: %w", err)
	}
	if input.WebhookID == uuid.Nil {
		return "", fmt.Errorf("notifications: webhook_id is required")
	}
	if input.EventType == "" {
		return "", fmt.Errorf("notifications: event_type is required")
	}
	if input.Payload == nil {
		input.Payload = json.RawMessage("{}")
	}

	// Verify the webhook belongs to the tenant. webhookExistsSQL already
	// filters deleted_at IS NULL, so a soft-deleted webhook reads as not
	// found here exactly as it does through the HTTP routes.
	var exists bool
	err := p.db.QueryRow(ctx, plugin.Rebind(webhookExistsSQL(p.dialect), p.dialect),
		input.WebhookID, cc.TenantID).Scan(&exists)
	if err != nil {
		return "", fmt.Errorf("notifications: verify webhook: %w", err)
	}
	if !exists {
		return "", fmt.Errorf("notifications: webhook not found: %s", input.WebhookID)
	}

	deliveryID := uuid.New()
	now := time.Now()

	// INSERT ... SELECT ... WHERE EXISTS, not a plain INSERT. cleat#2220/#2222,
	// the same race cleat-review found and fixed for webhookingest's
	// handleIngestWebhook (cleat#2199): the existence check above and this
	// INSERT are two separate statements, so a delete landing in between them
	// would otherwise still create a new pending delivery for a webhook the
	// tenant just removed -- one the cancellation in handleDeleteWebhook's own
	// transaction never sees, since it runs before this INSERT exists at all.
	//
	// FOR SHARE on the EXISTS subquery on PostgreSQL and MySQL, not on SQL
	// Server, closes the same READ COMMITTED gap #2221 fixed for
	// webhookingest: at PostgreSQL's default isolation, a plain read does not
	// see an UPDATE inside a still-open transaction, so an INSERT racing an
	// open handleDeleteWebhook transaction would otherwise read the
	// pre-delete row and succeed anyway. FOR SHARE makes the subquery block
	// until that transaction commits or rolls back, then re-reads and sees
	// the committed deleted_at. MySQL and SQL Server already block a plain
	// read against a row an open UPDATE holds, so FOR SHARE is added on MySQL
	// too (harmless there) and left off SQL Server, which has no such syntax.
	//
	// The residual: a send_webhook call that reads and commits entirely
	// before a delete's transaction begins is not a race at either isolation
	// level -- it is the ordinary "the delivery was queued before the
	// webhook was removed" case, and queryDueDeliveries' own independent
	// deleted_at guard (background.go) still stops it from ever being
	// attempted.
	existsGuard := "SELECT 1 FROM webhook_config WHERE id = $6 AND tenant_id = $7 AND deleted_at IS NULL"
	if p.dialect != plugin.DialectMSSQL {
		existsGuard += " FOR SHARE"
	}
	rowsInserted, err := p.db.Exec(ctx, plugin.Rebind(fmt.Sprintf(`
			INSERT INTO webhook_delivery (id, webhook_id, event_type, payload, status, attempt_count, next_attempt_at, created_at)
			SELECT $1, $2, $3, $4, 'pending', 0, %s, $5
			WHERE EXISTS (%s)
		`, nowSQLExpr(p.dialect), existsGuard), p.dialect),
		deliveryID, input.WebhookID, input.EventType, string(input.Payload), now, input.WebhookID, cc.TenantID)
	if err != nil {
		return "", fmt.Errorf("notifications: create delivery: %w", err)
	}
	if rowsInserted == 0 {
		return "", fmt.Errorf("notifications: webhook not found: %s", input.WebhookID)
	}

	p.logger.Info("notifications: delivery created",
		"delivery_id", deliveryID,
		"webhook_id", input.WebhookID,
		"event_type", input.EventType,
	)

	output := sendWebhookOutput{DeliveryID: deliveryID}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}
