package oauthprovider

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
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// TestSweepExpiredSessionsOnlyDeletesAbandonedLogins is cleat#2340's
// background-sweep MUST (cleat-review's original review of #2339/cleat#2319:
// oauth_sessions grows without bound because nothing ever cleans up a login
// that was never completed).
//
// The sweep's predicate is deliberately NOT "expires_at < now()" alone.
// handleLogin (routes.go) inserts a row with token_hash NULL; handleCallback
// sets token_hash on success and, in the same UPDATE, rewrites expires_at to
// the session's own (separately-set, longer) expiry -- so a COMPLETED
// session's row also reads expires_at < now() once that later expiry
// elapses, and extractSession (middleware.go) already refuses it at read
// time by checking expires_at there. Deleting it here too would be silently
// correct today and silently wrong the moment anything needs a completed row
// after expiry (an audit trail, a "your last session was..." UI) --
// background.go's doc comment says this; this test is what makes it false to
// say without also being caught.
//
// Runs against real Postgres, MySQL and SQL Server, not a fake: oauth_config
// and oauth_sessions are TenantScoped (migrations.go:172), so a naive INSERT
// against the raw pool is refused by SQL Server's RLS policy the same way
// TestARealLoginStoresNoTokensOnAnyDialect's fixture already had to route
// around -- be.CrossTenantConn + plugintest.ExecRebound is reused here for
// exactly that reason, not for the sweep itself, which drives
// plugin.AcrossAllTenants like production does (see background.go).
func TestSweepExpiredSessionsOnlyDeletesAbandonedLogins(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			seedPlugin := &Plugin{dialect: dialect, logger: quiet}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: seedPlugin, Healthy: true}}); err != nil {
				t.Fatalf("oauthprovider migrations on %s: %v", be.Name, err)
			}
			defer cleanupOauthproviderSchema(t, be.DB, be.Dialect)

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			fixtureDB := be.CrossTenantConn(t, ctx,
				"sweep fixture: seeds oauth_sessions rows directly, bypassing the /login "+
					"insert path this test is not exercising")

			var (
				abandonedID      = uuid.New() // token_hash NULL, PKCE window long past -- MUST be swept
				stillPendingID   = uuid.New() // token_hash NULL, PKCE window not yet elapsed -- must survive
				completedExpired = uuid.New() // token_hash set, session expiry past -- must survive
			)

			past := time.Now().Add(-1 * time.Hour)
			future := time.Now().Add(1 * time.Hour)

			insert := func(id uuid.UUID, tokenHash any, expiresAt time.Time) {
				t.Helper()
				if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect, `
					INSERT INTO oauth_sessions (id, tenant_id, provider, state, token_hash, expires_at)
					VALUES ($1, $2, $3, $4, $5, $6)
				`, id, tenantID, "google", "some-state", tokenHash, expiresAt); err != nil {
					t.Fatalf("seed oauth_sessions row %s: %v", id, err)
				}
			}
			insert(abandonedID, nil, past)
			insert(stillPendingID, nil, future)
			insert(completedExpired, "a-real-token-hash", past)

			// A plain defer, not t.Cleanup: t.Cleanup callbacks run only
			// after every defer in this function has already unwound
			// (they're a separate, later mechanism), so registering the row
			// delete that way would run it AFTER the defer above already
			// dropped the table -- exactly the ordering
			// TestARealLoginStoresNoTokensOnAnyDialect's own `defer cleanup()`
			// (this file's neighbour) avoids by being a defer too, placed
			// after cleanupOauthproviderSchema's so LIFO runs it first.
			defer func() {
				for _, id := range []uuid.UUID{abandonedID, stillPendingID, completedExpired} {
					if _, err := plugintest.ExecRebound(t, context.Background(), fixtureDB, dialect,
						`DELETE FROM oauth_sessions WHERE id = $1`, id); err != nil {
						t.Errorf("cleanup oauth_sessions %s on %s: %v", id, be.Name, err)
					}
				}
			}()

			p := &Plugin{dialect: dialect, logger: quiet}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}

			p.sweepExpiredSessions(ctx)

			exists := func(id uuid.UUID) bool {
				t.Helper()
				var n int
				if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
					`SELECT COUNT(*) FROM oauth_sessions WHERE id = $1`, id).Scan(&n); err != nil {
					t.Fatalf("checking existence of %s: %v", id, err)
				}
				return n > 0
			}

			if exists(abandonedID) {
				t.Errorf("on %s: the abandoned login (token_hash NULL, expired) survived the sweep -- "+
					"this is the unbounded-growth case cleat#2340's sweep exists to fix", be.Name)
			}
			if !exists(stillPendingID) {
				t.Errorf("on %s: the still-pending login (token_hash NULL, not yet expired) was swept -- "+
					"the sweep deleted a login that is still within its PKCE window", be.Name)
			}
			if !exists(completedExpired) {
				t.Errorf("on %s: a COMPLETED session (token_hash set) whose expiry has passed was swept -- "+
					"session expiry is enforced by extractSession at read time, not by deleting the row; "+
					"the sweep must only ever touch token_hash IS NULL rows", be.Name)
			}
		})
	}
}
