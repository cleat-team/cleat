package notifications

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/plugin"
)

// Run starts the delivery retry loop. It runs every 30 seconds, finding
// undelivered webhook deliveries whose next_attempt_at <= now() and
// attempting HTTP POST delivery. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("notifications: no database, delivery loop disabled")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	p.logger.Info("notifications: delivery retry loop started, interval=30s")

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("notifications: delivery retry loop stopped")
			return nil

		case <-ticker.C:
			start := time.Now()
			attempted, succeeded, failed, err := p.processDeliveries(ctx)
			if err != nil {
				p.logger.Error("notifications: delivery processing failed",
					"plugin", p.Info().Name,
					"error", err,
				)
				continue
			}
			p.logger.Info("notifications: work cycle completed",
				"plugin", p.Info().Name,
				"duration_ms", time.Since(start).Milliseconds(),
				"deliveries_attempted", attempted,
				"deliveries_succeeded", succeeded,
				"deliveries_failed", failed,
			)
		}
	}
}

// deliveryRow represents a pending or retrying delivery fetched from the database.
type deliveryRow struct {
	ID           uuid.UUID
	WebhookID    uuid.UUID
	EventType    string
	Payload      json.RawMessage
	AttemptCount int
}

// webhookConfigRow represents the webhook configuration needed for delivery.
type webhookConfigRow struct {
	URL    string
	Secret string
}

// processDeliveries queries for pending and retrying deliveries whose retry
// time has elapsed, and attempts HTTP POST delivery for each.
// Returns (attempted, succeeded, failed, error).
func (p *Plugin) processDeliveries(ctx context.Context) (int, int, int, error) {
	rows, err := p.db.Query(ctx, `
			SELECT d.id, d.webhook_id, d.event_type, d.payload, d.attempt_count
			FROM webhook_delivery d
			WHERE d.status IN ('pending', 'retrying')
			  AND d.next_attempt_at <= now()
			ORDER BY d.next_attempt_at ASC
			LIMIT 100
		`)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("query deliveries: %w", err)
	}
	defer rows.Close()

	var attempted, succeeded, failed int

	for rows.Next() {
		var d deliveryRow
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.EventType, &d.Payload, &d.AttemptCount); err != nil {
			p.logger.Error("notifications: scan delivery row", "error", err)
			continue
		}

		attempted++
		outcome, err := p.deliver(ctx, d)
		if err != nil {
			p.logger.Error("notifications: deliver", "delivery_id", d.ID, "error", err)
			continue
		}
		switch outcome {
		case "delivered":
			succeeded++
		case "failed":
			failed++
		}
	}

	return attempted, succeeded, failed, rows.Err()
}

// deliver attempts a single webhook delivery. It reads the webhook config,
// builds and sends an HTTP POST with HMAC-SHA256 signing, and updates the
// delivery status accordingly. Returns the outcome ("delivered", "retrying", "failed").
func (p *Plugin) deliver(ctx context.Context, d deliveryRow) (string, error) {
	// Look up the webhook config.
	var cfg webhookConfigRow
	err := p.db.QueryRow(ctx, plugin.Rebind(`
			SELECT url, secret FROM webhook_config WHERE id = $1
		`, p.dialect), d.WebhookID).Scan(&cfg.URL, &cfg.Secret)
	if err != nil {
		return "", fmt.Errorf("lookup webhook config: %w", err)
	}

	// Build the request body.
	payloadBytes := []byte(d.Payload)
	mac := hmac.New(sha256.New, []byte(cfg.Secret))
	mac.Write(payloadBytes)
	signature := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.URL, bytes.NewReader(payloadBytes))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Event", d.EventType)
	req.Header.Set("X-Webhook-Signature", "sha256="+signature)

	// Execute the HTTP request.
	resp, err := p.httpClient.Do(req)
	if err != nil {
		// Network or timeout error — record and retry.
		newCount := d.AttemptCount + 1
		if newCount >= 10 {
			return "failed", p.markFailed(ctx, d.ID, newCount, fmt.Sprintf("request failed: %v", err))
		}
		return "retrying", p.markRetrying(ctx, d.ID, newCount, fmt.Sprintf("request failed: %v", err))
	}
	defer resp.Body.Close()

	respBodyBytes, _ := io.ReadAll(resp.Body)
	respBody := string(respBodyBytes)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "delivered", p.markDelivered(ctx, d.ID, d.AttemptCount+1, resp.StatusCode, respBody)
	}

	// Non-2xx response — retry.
	newCount := d.AttemptCount + 1
	if newCount >= 10 {
		return "failed", p.markFailed(ctx, d.ID, newCount, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, respBody))
	}
	return "retrying", p.markRetrying(ctx, d.ID, newCount, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, respBody))
}

// markDelivered updates the delivery as successfully delivered.
func (p *Plugin) markDelivered(ctx context.Context, id uuid.UUID, attemptCount, statusCode int, responseBody string) error {
	_, err := p.db.Exec(ctx, plugin.Rebind(`
			UPDATE webhook_delivery
			SET status = 'delivered',
			    attempt_count = $1,
			    last_attempt_at = now(),
			    delivered_at = now(),
			    response_code = $2,
			    response_body = $3
			WHERE id = $4
		`, p.dialect), attemptCount, statusCode, responseBody, id)
	if err != nil {
		return fmt.Errorf("mark delivered: %w", err)
	}
	p.logger.Info("notifications: delivery delivered", "id", id, "attempts", attemptCount)
	return nil
}

// markRetrying updates the delivery for retry with exponential backoff.
func (p *Plugin) markRetrying(ctx context.Context, id uuid.UUID, attemptCount int, reason string) error {
	nextAt := time.Now().Add(nextBackoff(attemptCount))
	_, err := p.db.Exec(ctx, plugin.Rebind(`
			UPDATE webhook_delivery
			SET status = 'retrying',
			    attempt_count = $1,
			    last_attempt_at = now(),
			    next_attempt_at = $2,
			    response_body = $3
			WHERE id = $4
		`, p.dialect), attemptCount, nextAt, reason, id)
	if err != nil {
		return fmt.Errorf("mark retrying: %w", err)
	}
	p.logger.Info("notifications: delivery retrying",
		"id", id, "attempt", attemptCount, "next_attempt", nextAt, "reason", reason)
	return nil
}

// markFailed updates the delivery as permanently failed.
func (p *Plugin) markFailed(ctx context.Context, id uuid.UUID, attemptCount int, reason string) error {
	_, err := p.db.Exec(ctx, plugin.Rebind(`
			UPDATE webhook_delivery
			SET status = 'failed',
			    attempt_count = $1,
			    last_attempt_at = now(),
			    response_body = $2
			WHERE id = $3
		`, p.dialect), attemptCount, reason, id)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	p.logger.Warn("notifications: delivery failed", "id", id, "attempts", attemptCount, "reason", reason)
	return nil
}

// nextBackoff returns the delay before the next retry based on the attempt
// count. The backoff schedule is: 1m, 5m, 15m, then 1h for subsequent retries.
func nextBackoff(attemptCount int) time.Duration {
	switch attemptCount {
	case 1:
		return 1 * time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 15 * time.Minute
	default:
		return 1 * time.Hour
	}
}
