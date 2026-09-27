package notifications

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// TestARetryIsStampedWithTheDatabaseClock is cleat#1992/#2172's real-database
// pin for markRetrying, across all three dialects. TestMarkRetrying and
// TestMarkRetrying_ExecError (notifications_behavioral_test.go) only exercise
// markRetrying against a fake driver that records the query text and never
// executes it -- so nothing before this test had confirmed nowSQLExpr's and
// nowPlusSecondsSQLExpr's generated SQL (background.go) is actually valid on
// a real MySQL or SQL Server server, only that it was built from strings this
// package's author believed were valid.
//
// The clock-consistency GAP cleat-review found on #2198 was in sendWebhook's
// INSERT (fixed, and pinned by the same-tick assertion in
// a_delivery_signs_with_the_configured_secret_multidb_test.go); markRetrying
// sets the same column on every retry, so it was changed the same way for
// the same reason -- this test is that fix's own falsifiable pin, since
// nothing else exercises it against a real database.
func TestARetryIsStampedWithTheDatabaseClock(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: discardLogger()}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)

			// Seed a webhook_config row and a webhook_delivery row through
			// p.db under a tenant-scoped context, not be.DB.ExecContext
			// directly: both tables carry a row-level policy since #1992, and
			// a raw connection with no session context set is BLOCKED on SQL
			// Server (its BLOCK predicate refuses the write outright, unlike
			// PostgreSQL's policy which simply filters -- see
			// plugin/a_plugin_table_has_a_policy_on_sql_server_test.go for the
			// same shape). This test is about markRetrying's SQL, not about
			// sendWebhook or the HTTP path, both already covered by this
			// package's sibling multidb test -- so the seed goes in directly
			// rather than through those handlers, but still through the
			// tenant-scoped adapter every real write in this plugin uses.
			seedCtx := plugin.ForTenant(ctx, tenantID)
			webhookID := uuid.New()
			deliveryID := uuid.New()
			if _, err := p.db.Exec(seedCtx, `
				INSERT INTO webhook_config (tenant_id, id, url, secret_configured, events, enabled, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			`, tenantID, webhookID, "http://127.0.0.1:1/unreachable", false, "[]", true, time.Now(), time.Now()); err != nil {
				t.Fatalf("seed webhook_config: %v", err)
			}
			if _, err := p.db.Exec(seedCtx, `
				INSERT INTO webhook_delivery (id, webhook_id, event_type, payload, status, attempt_count, next_attempt_at, created_at)
				VALUES ($1, $2, $3, $4, 'pending', 0, $5, $6)
			`, deliveryID, webhookID, "test.event", `{}`, time.Now(), time.Now()); err != nil {
				t.Fatalf("seed webhook_delivery: %v", err)
			}

			// The call under test.
			if err := p.markRetrying(ctx, deliveryID, 1, "connection refused"); err != nil {
				t.Fatalf("markRetrying: %v", err)
			}

			// Read back through the same adapter, not a second raw query --
			// the DATABASE's own comparison of the two columns it just wrote
			// is the thing under test, so let the database do the comparing.
			var status string
			var attemptCount int
			var secondsUntilDue float64
			nowExpr := map[plugin.Dialect]string{
				plugin.DialectMySQL: "NOW(6)",
				plugin.DialectMSSQL: "SYSUTCDATETIME()",
			}[dialect]
			if nowExpr == "" {
				nowExpr = "now()"
			}
			diffExpr := map[plugin.Dialect]string{
				plugin.DialectMySQL: "TIMESTAMPDIFF(MICROSECOND, " + nowExpr + ", next_attempt_at) / 1000000.0",
				plugin.DialectMSSQL: "DATEDIFF(MILLISECOND, " + nowExpr + ", next_attempt_at) / 1000.0",
			}[dialect]
			if diffExpr == "" {
				diffExpr = "EXTRACT(EPOCH FROM (next_attempt_at - " + nowExpr + "))"
			}
			row := plugintest.QueryRowRebound(t, ctx, be.DB, dialect, `
				SELECT status, attempt_count, `+diffExpr+`
				FROM webhook_delivery
				WHERE id = $1
			`, deliveryID)
			if err := row.Scan(&status, &attemptCount, &secondsUntilDue); err != nil {
				t.Fatalf("read back delivery: %v", err)
			}

			if status != "retrying" {
				t.Errorf("status: got %q, want %q", status, "retrying")
			}
			if attemptCount != 1 {
				t.Errorf("attempt_count: got %d, want 1", attemptCount)
			}
			// nextBackoff(1) -- checked against the DATABASE's own clock, not
			// the test process's: the whole point of this fix is that the two
			// are no longer compared against each other at all. A wide
			// tolerance (the backoff schedule is not this test's subject);
			// what matters is the sign and the order of magnitude -- a
			// next_attempt_at stamped with the pre-fix app clock and no
			// backoff at all would read close to 0s here, not tens of
			// seconds.
			wantBackoff := nextBackoff(1).Seconds()
			if secondsUntilDue < wantBackoff-5 || secondsUntilDue > wantBackoff+30 {
				t.Errorf("next_attempt_at is %.1fs from the database's own now() "+
					"(want close to nextBackoff(1) = %.1fs) -- markRetrying may be "+
					"stamping with the wrong clock again", secondsUntilDue, wantBackoff)
			}
		})
	}
}
