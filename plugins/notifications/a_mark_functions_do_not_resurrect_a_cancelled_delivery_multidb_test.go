package notifications

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestMarkFunctionsDoNotResurrectACancelledDelivery is cleat#2233's own
// regression test for coordinator MUST-FIX item 6: markDelivered,
// markRetrying and markFailed (background.go) all update a delivery row by
// id alone before this fix, with no status check -- so a delivery attempt
// already in flight when handleDeleteWebhook's own cancellation (routes.go)
// commits would still land afterward and overwrite the row's 'cancelled'
// status back to 'delivered', 'retrying' or 'failed'. The same race
// host_functions.go's guarded INSERT closes for a NEW delivery
// (TestASendWebhookRacingAnOpenDeleteTransactionIsBlocked), one step later
// in a delivery's life: an EXISTING one, already in flight.
//
// This is deterministic rather than a live interleaving, unlike that sibling
// test: the fix is "AND status IN ('pending', 'retrying')" on a single
// UPDATE, not a read/write race across two statements, so there is no
// window to hold open -- calling each mark* function directly against a
// row already 'cancelled' either updates it (bug) or affects zero rows
// (fixed), with nothing timing-dependent about which.
func TestMarkFunctionsDoNotResurrectACancelledDelivery(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: quiet}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			seedCtx := plugin.ForTenant(ctx, tenantID)
			now := time.Now()

			webhookID := uuid.New()
			if _, err := p.db.Exec(seedCtx, plugin.Rebind(`
					INSERT INTO webhook_config (tenant_id, id, url, secret_configured, events, enabled, created_at, updated_at)
					VALUES ($1, $2, $3, $4, $5, true, $6, $7)
				`, dialect), tenantID, webhookID, "https://example.com/mark", true, `["test.event"]`, now, now); err != nil {
				t.Fatalf("seed webhook_config: %v", err)
			}

			readConn := be.CrossTenantConn(t, ctx, "cleat#2233 item 6: confirming a cancelled delivery is not resurrected")

			// One cancelled delivery per function under test, each with a
			// distinguishing attempt_count and response_body so a spurious
			// update is visible even if status alone were somehow
			// unaffected by an incomplete fix.
			seedCancelled := func(name string, attemptCount int) uuid.UUID {
				t.Helper()
				id := uuid.New()
				// Every placeholder numbered once and passed in that same
				// order -- next_attempt_at and created_at both get `now`,
				// but as TWO distinct args ($4, $6), never one number
				// reused: MySQL's Rebind binds `?` by appearance in the
				// text, not by placeholder number, so a repeated $N would
				// need a repeated arg at that textual position too, and
				// this passes each arg once. See CLAUDE.md's "MySQL binds
				// `?` by APPEARANCE".
				if _, err := p.db.Exec(ctx, plugin.Rebind(`
						INSERT INTO webhook_delivery (id, webhook_id, event_type, payload, status, attempt_count, next_attempt_at, response_body, created_at)
						VALUES ($1, $2, 'test.event', '{}', 'cancelled', $3, $4, $5, $6)
					`, dialect), id, webhookID, attemptCount, now, "cancelled-before-"+name, now); err != nil {
					t.Fatalf("seed cancelled delivery for %s: %v", name, err)
				}
				return id
			}

			assertStillCancelled := func(name string, id uuid.UUID, wantAttemptCount int) {
				t.Helper()
				var status, responseBody string
				var attemptCount int
				if err := readConn.QueryRowContext(ctx, plugin.Rebind(
					`SELECT status, attempt_count, response_body FROM webhook_delivery WHERE id = $1`, dialect),
					id).Scan(&status, &attemptCount, &responseBody); err != nil {
					t.Fatalf("read delivery after %s: %v", name, err)
				}
				if status != "cancelled" {
					t.Errorf("%s against a cancelled delivery: status=%q, want %q (resurrected)", name, status, "cancelled")
				}
				if attemptCount != wantAttemptCount {
					t.Errorf("%s against a cancelled delivery: attempt_count=%d, want %d unchanged (the UPDATE ran despite the guard)",
						name, attemptCount, wantAttemptCount)
				}
				if responseBody != "cancelled-before-"+name {
					t.Errorf("%s against a cancelled delivery: response_body=%q, want unchanged (the UPDATE ran despite the guard)",
						name, responseBody)
				}
			}

			deliveredID := seedCancelled("markDelivered", 1)
			if err := p.markDelivered(ctx, deliveredID, 5, 200, "should-not-land"); err != nil {
				t.Fatalf("markDelivered: %v", err)
			}
			assertStillCancelled("markDelivered", deliveredID, 1)

			retryingID := seedCancelled("markRetrying", 2)
			if err := p.markRetrying(ctx, retryingID, 5, "should-not-land"); err != nil {
				t.Fatalf("markRetrying: %v", err)
			}
			assertStillCancelled("markRetrying", retryingID, 2)

			failedID := seedCancelled("markFailed", 9)
			if err := p.markFailed(ctx, failedID, 10, "should-not-land"); err != nil {
				t.Fatalf("markFailed: %v", err)
			}
			assertStillCancelled("markFailed", failedID, 9)
		})
	}
}
