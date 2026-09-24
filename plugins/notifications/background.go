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

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// deliveryInterval is how often Run processes due deliveries.
//
// A var rather than a literal so that the test which proves Run marks its own
// loop can drive a real tick. At 30s a test would have to sleep for half a
// minute to see one, so the AcrossAllTenants call below would be covered by no
// test at all -- and it is the one line whose absence breaks the worker
// outright rather than degrading it. cleat#1512.
var deliveryInterval = 30 * time.Second

// Run starts the delivery retry loop. It runs every 30 seconds, finding
// undelivered webhook deliveries whose next_attempt_at <= now() and
// attempting HTTP POST delivery. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("notifications: no database, delivery loop disabled")
		<-ctx.Done()
		return nil
	}

	// THE DELIVERY LOOP NAMES ITSELF CROSS-TENANT. cleat#1512. webhook_config
	// carries a row-level policy from migration v2, and the policy calls
	// cleat.assert_tenant_set(), which RAISEs rather than filtering when no
	// tenant is in scope. Without this the loop does not degrade -- it fails
	// outright on its first statement after the migration lands.
	//
	// WHICH STATEMENT, precisely, because only one of them needs this and it is
	// not the obvious one. queryDueDeliveries reads webhook_delivery, which has
	// no tenant_id and therefore no policy, so it would run unmarked. It is
	// deliver() that then reads `SELECT url, secret FROM webhook_config WHERE
	// id = $1` -- by delivery, with no tenant predicate of its own, because the
	// delivery row does not know its tenant. That read is what the bypass is
	// for.
	//
	// AcrossAllTenants rather than ForTenant, and the reason is the same fact:
	// there is no tenant to narrow to. The loop cannot know a delivery's tenant
	// until it has read the config, and reading the config is the statement in
	// question. A ForTenant afterwards would buy nothing -- every remaining
	// statement in deliver() writes webhook_delivery, which has no policy.
	//
	// Marked ONCE here rather than at those call sites. The handlers in
	// routes.go are deliberately NOT marked: they run on r.Context(), which
	// carries the request's tenant, and so is the host call in
	// host_functions.go since cleat#1492 bridged the workflow's tenant at the
	// PluginCall boundary. Marking any of them would widen a per-tenant read to
	// every tenant.
	//
	// baseCtx keeps the PRE-bypass context alive. cleat#1992 moved the webhook
	// secret into tenant Secrets, fetched once deliver() has read the config row
	// and therefore knows the tenant -- but engine/plugin_secrets.go refuses
	// every Secrets call, ForTenant's returned methods included, on a ctx that
	// already carries the AcrossAllTenants marker (plugin/crosstenant.go: "a
	// bypass already in scope wins, and this is silent"). The marked ctx below
	// stays in use for webhook_config/webhook_delivery reads and writes, which
	// need it -- webhook_delivery's write grant is scoped to the cleat_sweep
	// role that marking switches to (migration v3). The Secrets.ForTenant call
	// in deliver() must instead build its own per-tenant ctx from baseCtx.
	baseCtx := ctx
	ctx = plugin.AcrossAllTenants(ctx,
		"notifications delivery loop: a due delivery does not know its tenant until its webhook_config row is read")

	ticker := time.NewTicker(deliveryInterval)
	defer ticker.Stop()

	p.logger.Info("notifications: delivery retry loop started", "interval", deliveryInterval)

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("notifications: delivery retry loop stopped")
			return nil

		case <-ticker.C:
			start := time.Now()
			attempted, succeeded, failed, err := p.processDeliveries(ctx, baseCtx)
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
// It carries no secret -- cleat#1992 moved that into tenant secrets, keyed
// per-webhook by WebhookSecretName(ID) (routes.go); deliver fetches it
// separately, once it knows this row's tenant.
type webhookConfigRow struct {
	URL              string
	TenantID         uuid.UUID
	SecretConfigured bool
}

// processDeliveries queries for pending and retrying deliveries whose retry
// time has elapsed, and attempts HTTP POST delivery for each.
// Returns (attempted, succeeded, failed, error).
//
// baseCtx is ctx without the AcrossAllTenants marker Run applied -- see the
// comment there. It is passed through unchanged to deliver, which is the
// only place that needs it.
func (p *Plugin) processDeliveries(ctx, baseCtx context.Context) (int, int, int, error) {
	rows, err := p.db.Query(ctx, queryDueDeliveries.For(p.dialect))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("query deliveries: %w", err)
	}
	defer rows.Close()

	var attempted, succeeded, failed int

	for rows.Next() {
		var d deliveryRow
		// plugin.JSONColumn, not &d.Payload directly: SQL Server returns
		// NVARCHAR as a Go string, and database/sql has no fast-path
		// conversion from a string driver.Value into a *json.RawMessage
		// (json.RawMessage is a named []byte type, not the literal []byte
		// database/sql's fast path matches on) -- see JSONColumn's own doc
		// comment for the exact failure signature. lib/pq and
		// go-sql-driver/mysql both return jsonb/json columns as []byte, which
		// convertAssignRows DOES special-case (a []byte value is assignable
		// to any named-[]byte-underlying type), so this scanned successfully
		// on Postgres and MySQL and failed silently -- logged, then
		// `continue`d past -- on every SQL Server row. Found running the real
		// delivery loop against real SQL Server for the first time
		// (cleat-review's requested multi-dialect test on #2198): every due
		// delivery was skipped, attempted stayed 0, and nothing else in this
		// loop's return values said why.
		var payload plugin.JSONColumn
		if err := plugin.ScanRow(rows, &d.ID, &d.WebhookID, &d.EventType, &payload, &d.AttemptCount); err != nil {
			p.logger.Error("notifications: scan delivery row", "error", err)
			continue
		}
		d.Payload = payload.Raw

		attempted++
		// ONE TRACE PER DELIVERY, originated here because a webhook delivery has
		// no caller: this sweep runs on a timer with no inbound request, so there
		// is nothing to continue. cleat#1611.
		//
		// Per DELIVERY rather than per sweep tick, and the loop above is why that
		// is affordable: it iterates deliveries that are DUE, so a quiet period
		// originates nothing at all. Per item rather than per batch because the
		// item is what anyone asks about -- "why did this webhook fail" is a
		// question for a trace; "how long did the sweep take" is one for a metric.
		dctx := plugin.WithNewTrace(ctx)
		outcome, err := p.deliver(dctx, baseCtx, d)
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
//
// baseCtx is the pre-AcrossAllTenants context threaded from Run/processDeliveries
// -- see Run's comment. It is used only to build the per-tenant ctx for the
// Secrets.ForTenant call below; everything else in deliver keeps using ctx,
// which carries the marking that webhook_config/webhook_delivery need.
func (p *Plugin) deliver(ctx, baseCtx context.Context, d deliveryRow) (string, error) {
	// Look up the webhook config.
	//
	// plugin.ScanRow, not a bare .Scan: SQL Server returns UNIQUEIDENTIFIER
	// (tenant_id) in mixed-endian byte order, which uuid.UUID's own Scan
	// takes without error and turns into a DIFFERENT uuid -- see its doc
	// comment. ScanRow substitutes plugin.GUID for any *uuid.UUID
	// destination and swaps it back, the same correction routes.go's own
	// scans in this package already get.
	var cfg webhookConfigRow
	err := plugin.ScanRow(p.db.QueryRow(ctx, plugin.Rebind(`
			SELECT url, tenant_id, secret_configured FROM webhook_config WHERE id = $1
		`, p.dialect), d.WebhookID), &cfg.URL, &cfg.TenantID, &cfg.SecretConfigured)
	if err != nil {
		return "", fmt.Errorf("lookup webhook config: %w", err)
	}

	// A signing secret is now REQUIRED on every webhook (cleat#1992/#2172,
	// owner decision (b)): handleCreateWebhook refuses to create one without
	// it and handleUpdateWebhook refuses to clear it, so secret_configured
	// unset is refused here rather than read as "sign with an empty key" --
	// there is no path today that produces such a row, but this is what
	// makes that a failed delivery instead of an unsigned one if it is ever
	// reached. The secret MUST be readable: Secrets.ForTenant, not the
	// request-path Get, since this sweep has no request to inherit a tenant
	// from and cfg.TenantID is what it just read above -- declared in
	// plugin/a_secrets_for_tenant_is_declared_test.go's secretsForTenantLedger.
	// ANY lookup failure here fails the delivery attempt outright rather than
	// falling back to an empty-key signature nobody configured -- the same
	// "a lookup failure must not read as unsigned" reasoning cleat#2172 needed
	// for webhookingest's inbound verification, applied to this plugin's
	// outbound one.
	//
	// Both failure paths below go through retryOrFail rather than a bare
	// error return. Before cleat-review on #2198, deliver() returned an error
	// here and processDeliveries just `continue`d: the delivery row was never
	// touched, so it stayed pending forever, was retried every tick with
	// nothing but a log line to show for it, got no attempt_count, and
	// GET .../deliveries had no way to say why. A retired or undecryptable
	// secret is not a transient condition that will clear on the next tick
	// the way a network blip might, but it is still recorded through the
	// same retry-then-fail machinery as every other delivery error, so it
	// surfaces in the same place an operator already looks.
	if !cfg.SecretConfigured {
		return p.retryOrFail(ctx, d, fmt.Sprintf("webhook %s has no secret configured", d.WebhookID))
	}
	tenantCtx := plugin.ForTenant(baseCtx, cfg.TenantID)
	secret, err := p.secrets.ForTenant(cfg.TenantID.String()).Get(tenantCtx, WebhookSecretName(d.WebhookID))
	if err != nil {
		return p.retryOrFail(ctx, d, fmt.Sprintf("get webhook secret: %v", err))
	}

	// Build the request body.
	payloadBytes := []byte(d.Payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payloadBytes)
	signature := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.URL, bytes.NewReader(payloadBytes))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	plugin.SetTraceparentFromContext(ctx, req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Event", d.EventType)
	req.Header.Set("X-Webhook-Signature", "sha256="+signature)

	// Execute the HTTP request.
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return p.retryOrFail(ctx, d, fmt.Sprintf("request failed: %v", err))
	}
	defer resp.Body.Close()

	respBodyBytes, _ := io.ReadAll(resp.Body)
	respBody := string(respBodyBytes)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "delivered", p.markDelivered(ctx, d.ID, d.AttemptCount+1, resp.StatusCode, respBody)
	}

	return p.retryOrFail(ctx, d, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, respBody))
}

// retryOrFail marks a delivery for another attempt, or permanently failed
// once it has reached the retry ceiling, and returns the outcome in the same
// (string, error) shape markRetrying/markFailed's callers already expect.
// Shared by every failure path in deliver -- a network error, a non-2xx
// response, and (cleat#1992/#2172, cleat-review on #2198) a secret that could
// not be configured or read -- so each is recorded the same way instead of
// some going through markRetrying/markFailed and others returning a bare
// error that processDeliveries only logs and drops.
func (p *Plugin) retryOrFail(ctx context.Context, d deliveryRow, reason string) (string, error) {
	newCount := d.AttemptCount + 1
	if newCount >= 10 {
		return "failed", p.markFailed(ctx, d.ID, newCount, reason)
	}
	return "retrying", p.markRetrying(ctx, d.ID, newCount, reason)
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
//
// The backoff is added IN SQL, against the DATABASE's clock -- see
// nowSQLExpr/nowPlusSecondsSQLExpr's doc comment below. Go computes only the
// backoff DURATION (nextBackoff); the resulting instant is the database
// server's own "now" plus that many seconds, never the app's.
func (p *Plugin) markRetrying(ctx context.Context, id uuid.UUID, attemptCount int, reason string) error {
	backoffSeconds := int(nextBackoff(attemptCount).Seconds())
	// Every placeholder numbered once, strictly increasing in the order it
	// appears in the text -- $2 (backoffSeconds) is used inside the
	// next_attempt_at expression, ahead of $3/$4 textually, so the args
	// below are ordered to match rather than to match the column order in
	// the SET list. See CLAUDE.md's "MySQL binds `?` by APPEARANCE".
	query := fmt.Sprintf(`
			UPDATE webhook_delivery
			SET status = 'retrying',
			    attempt_count = $1,
			    last_attempt_at = %s,
			    next_attempt_at = %s,
			    response_body = $3
			WHERE id = $4
		`, nowSQLExpr(p.dialect), nowPlusSecondsSQLExpr(p.dialect, "$2"))
	_, err := p.db.Exec(ctx, plugin.Rebind(query, p.dialect), attemptCount, backoffSeconds, reason, id)
	if err != nil {
		return fmt.Errorf("mark retrying: %w", err)
	}
	p.logger.Info("notifications: delivery retrying",
		"id", id, "attempt", attemptCount, "next_attempt_seconds", backoffSeconds, "reason", reason)
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

// Deliveries that are due: pending or retrying, with their next attempt in the
// past.
//
// `LIMIT 100` has no T-SQL spelling (cleat#1133). `now()` does not either, but
// that one the adapter handles -- plugin.Rebind rewrites it to
// SYSUTCDATETIME(), so only the row limit needs an arm. The split is the same
// one the adapter draws everywhere: a token that maps one-to-one is rewritten
// centrally; a construct that moves to a different clause is written out.
//
// THE MYSQL ARM USES `NOW(6)`, NOT A BARE `now()`. next_attempt_at is
// TIMESTAMP(6) (migrations.go), storing microseconds -- at the time this was
// found, the value sendWebhook inserted was Go's time.Now(), full precision.
// (sendWebhook and markRetrying now stamp next_attempt_at with the
// database's own clock too -- see nowSQLExpr below, added for a related but
// distinct bug -- so this paragraph's "the value sendWebhook inserts" is
// history rather than current behaviour; the precision mismatch it explains
// would have applied to an app-clock value just the same.) MySQL's `now()`
// with no argument returns SECOND precision, truncating any fractional part
// to zero.
// So `next_attempt_at <= now()` compares a microsecond-precise value against
// one truncated DOWN to the start of the current second: a delivery whose
// next_attempt_at falls anywhere after that second's :00 -- which is nearly
// always, since it is set to "now" at creation -- reads as still in the
// future until the wall clock ticks over to the NEXT second. Found running
// this against real MySQL with no delay between creating a delivery and
// sweeping for it (cleat-review on #2198's requested test): attempted=0 on
// every run, despite the row existing with status='pending' and
// next_attempt_at a few milliseconds in the past. NOW(6) matches the
// column's own precision, the same fix engine/query_builder.go's nowExpr()
// already uses for MySQL. In production, where Run ticks every
// deliveryInterval (30s) rather than immediately, this cost at most one
// missed sweep before the next one caught it -- silent by dilution, not by
// impossibility, which is exactly the class of bug a lower-frequency
// production system does not surface for itself.
//
// nowSQLExpr and nowPlusSecondsSQLExpr build next_attempt_at (in sendWebhook
// and markRetrying) out of the SAME clock this query compares it against:
// the database server's, not the Go process's.
//
// A SECOND, DISTINCT gap from the one above, found in cleat-review's re-check
// of #2198: sendWebhook and markRetrying used to stamp next_attempt_at with
// Go's time.Now() (the app/host clock), literal precision aside. Measured:
// MySQL runs about 35ms behind the Go host clock in cleat-review's
// environment, so a freshly-created delivery's next_attempt_at (host clock,
// "now") read as still in the future against the database's own, slightly
// earlier "now" -- it missed its first sweep every time, not only when the
// clocks happened to straddle a second boundary the way the precision bug
// above needed. Any app/DB clock skew, in EITHER direction, delays every
// attempt -- creation and every retry -- by the same amount, silently.
// Stamping with the database's own clock, exactly as this query's own read
// side already does, removes the skew rather than bounding it.
func nowSQLExpr(d plugin.Dialect) string {
	switch d {
	case plugin.DialectMySQL:
		return "NOW(6)"
	case plugin.DialectMSSQL:
		return "SYSUTCDATETIME()"
	default:
		return "now()"
	}
}

// nowPlusSecondsSQLExpr returns a dialect-correct SQL expression for "the
// database's own now, advanced by the number of seconds bound at ph" -- the
// same shape as engine's internal Dialect.intervalExpr
// (engine/query_builder.go), reproduced here because a plugin cannot import
// engine (see plugin.Secrets' own doc comment for why), using the exact
// per-dialect interval syntax already proven in this repo's
// queryUnprocessedWebhookEvents (plugins/webhookingest/background.go):
// PostgreSQL's interval literal multiplied by a bound count, MySQL's
// INTERVAL clause with the count unquoted and singular, and SQL Server's
// DATEADD in place of an interval type it does not have.
func nowPlusSecondsSQLExpr(d plugin.Dialect, ph string) string {
	switch d {
	case plugin.DialectMySQL:
		return fmt.Sprintf("NOW(6) + INTERVAL %s SECOND", ph)
	case plugin.DialectMSSQL:
		return fmt.Sprintf("DATEADD(SECOND, %s, SYSUTCDATETIME())", ph)
	default:
		return fmt.Sprintf("now() + interval '1 second' * %s", ph)
	}
}

var queryDueDeliveries = plugin.Query{
	Default: `SELECT d.id, d.webhook_id, d.event_type, d.payload, d.attempt_count
FROM webhook_delivery d
WHERE d.status IN ('pending', 'retrying')
  AND d.next_attempt_at <= now()
ORDER BY d.next_attempt_at ASC
LIMIT 100`,
	MySQL: `SELECT d.id, d.webhook_id, d.event_type, d.payload, d.attempt_count
FROM webhook_delivery d
WHERE d.status IN ('pending', 'retrying')
  AND d.next_attempt_at <= NOW(6)
ORDER BY d.next_attempt_at ASC
LIMIT 100`,
	MSSQL: `SELECT TOP 100 d.id, d.webhook_id, d.event_type, d.payload, d.attempt_count
FROM webhook_delivery d
WHERE d.status IN ('pending', 'retrying')
  AND d.next_attempt_at <= now()
ORDER BY d.next_attempt_at ASC`,
}
