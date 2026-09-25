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

// TestTheWorkerGetsAPluginThatCanRunTheSweep runs the worker's own plugin
// discovery and asserts the type assertion the worker will make against it.
//
// cmd/cleat-worker/main.go:1457 calls plugin.Discover(), and main.go:1784
// then asks `lp.Plugin.(plugin.HasBackground)` of each entry to decide which
// plugins get a background loop. This test makes the same two calls in the
// same order, so it is a reproduction of the production path rather than a
// resemblance to it.
//
// The failure it guards is silent by construction: nothing logs, no error
// surfaces, and TestSweepExpiredSessionsOnlyDeletesAbandonedLogins cannot
// report it -- that test calls p.sweepExpiredSessions(ctx) directly, which
// proves the predicate is right and says nothing about whether anything ever
// calls it. This is CLAUDE.md's "a mechanism that exists and is wired to
// nothing reads as done": a unit test proves it classifies, a caller proves
// it runs.
//
// Honest about which half decides, because the two assertions here are not
// equals. background.go's
//
//	var _ plugin.HasBackground = (*Plugin)(nil)
//
// already fails the BUILD if *Plugin stops satisfying the interface, so the
// second assertion below cannot fire while that line stands -- measured, not
// reasoned: the falsification (a decorated constructor returning a type that
// satisfies plugin.Plugin by embedding it but promotes no Run) went red on
// the FIRST assertion, `ours == nil`, as it must, since a non-*Plugin never
// reaches the interface check.
//
// So the load-bearing assertion is the first one: does the registry hand
// over an *Plugin at all. That is the thing the compile-time line is
// structurally unable to see, and it is not academic -- a refactor that
// renamed Plugin, or wrapped it in a decorator, keeps the package compiling
// and keeps every other test in this package green while the worker never
// starts the loop. The interface assertion is kept because it costs a line
// and becomes the only check on this path if that compile-time line is ever
// removed; it is not a second, independent failure mode today.
func TestTheWorkerGetsAPluginThatCanRunTheSweep(t *testing.T) {
	loaded, err := plugin.Discover()
	if err != nil {
		t.Fatalf("plugin.Discover() failed, so a worker would not get as far as starting "+
			"any background loop: %v", err)
	}

	var ours plugin.Plugin
	for _, lp := range loaded {
		if _, ok := lp.Plugin.(*Plugin); ok {
			ours = lp.Plugin
			break
		}
	}
	if ours == nil {
		t.Fatalf("plugin.Discover() returned %d plugin(s) and none is an *oauthprovider.Plugin. "+
			"Discover is what instantiates the registered constructor -- the same call "+
			"cmd/cleat-worker/main.go:1457 makes -- so the worker's HasBackground assertion would "+
			"never be made against this plugin and the abandoned-login sweep would never run, "+
			"with every other test in this package still green.", len(loaded))
	}

	if _, ok := ours.(plugin.HasBackground); !ok {
		t.Fatalf("the plugin the registry produces (%T) does not satisfy plugin.HasBackground. "+
			"cmd/cleat-worker/main.go:1784 asserts this interface to decide which plugins get a "+
			"background loop, so the assertion fails, Run is never called, and oauth_sessions "+
			"grows unbounded again. TestSweepExpiredSessionsOnlyDeletesAbandonedLogins cannot "+
			"report that: it calls sweepExpiredSessions directly.", ours)
	}
}

// TestRunSweepsOnStartupAndReturnsOnCancel drives Run itself, which is the
// entry point the worker actually calls and which no test in this package
// exercised before: TestSweepExpiredSessionsOnlyDeletesAbandonedLogins calls
// sweepExpiredSessions directly, so Run's own body -- the immediate startup
// sweep, the ticker loop, and the return-on-cancel that plugin.HasBackground
// implies -- was reachable only from production.
//
// background.go runs one sweep immediately on startup so an abandoned row
// left over from before a restart does not have to wait a full
// sweepInterval (5 minutes) to be swept. Deleting that one call would leave
// the sweep running only every 5 minutes with every existing test green;
// this test observes the startup sweep, so it is what makes that call
// load-bearing.
//
// The two assertions on rows that must SURVIVE are here as well as in the
// sweep test deliberately: they are the same invariant reached through a
// different entry point, which is the reason this test exists rather than
// being folded into that one.
func TestRunSweepsOnStartupAndReturnsOnCancel(t *testing.T) {
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
				"Run startup-sweep test: seeds oauth_sessions rows directly, bypassing the "+
					"/login insert path this test is not exercising")

			var (
				abandonedID      = uuid.New() // token_hash NULL, PKCE window long past -- MUST be swept
				stillPendingID   = uuid.New() // token_hash NULL, window not yet elapsed -- must survive
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

			// A plain defer, registered after cleanupOauthproviderSchema's so
			// LIFO runs these row deletes while the table still exists -- the
			// same ordering the neighbouring sweep test needed, and for the
			// same reason (t.Cleanup callbacks run only after every defer in
			// this function has unwound).
			defer func() {
				for _, id := range []uuid.UUID{abandonedID, stillPendingID, completedExpired} {
					if _, err := plugintest.ExecRebound(t, context.Background(), fixtureDB, dialect,
						`DELETE FROM oauth_sessions WHERE id = $1`, id); err != nil {
						t.Errorf("cleanup oauth_sessions %s on %s: %v", id, be.Name, err)
					}
				}
			}()

			exists := func(id uuid.UUID) bool {
				t.Helper()
				var n int
				if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
					`SELECT COUNT(*) FROM oauth_sessions WHERE id = $1`, id).Scan(&n); err != nil {
					t.Fatalf("checking existence of %s: %v", id, err)
				}
				return n > 0
			}

			p := &Plugin{dialect: dialect, logger: quiet}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}

			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- p.Run(runCtx) }()

			// The startup sweep runs before Run enters its ticker loop, so
			// this observes it without waiting sweepInterval (5 minutes).
			// The bound is a liveness guard, not a timing assertion: the
			// real latency is milliseconds, and a generous deadline here
			// cannot make a correct Run look wrong, only make a broken one
			// report instead of hanging the suite.
			deadline := time.Now().Add(15 * time.Second)
			for exists(abandonedID) {
				if time.Now().After(deadline) {
					t.Fatalf("on %s: Run did not delete the abandoned login within 15s. Run's "+
						"immediate startup sweep (background.go) is what the worker relies on so an "+
						"abandoned row left over from before a restart does not wait a full "+
						"sweepInterval -- if it is gone, the loop still ticks but nothing sweeps "+
						"until 5 minutes in.", be.Name)
				}
				time.Sleep(20 * time.Millisecond)
			}

			if !exists(stillPendingID) {
				t.Errorf("on %s: Run's startup sweep deleted a still-pending login (token_hash NULL, "+
					"not yet expired) -- the sweep must only touch rows past their PKCE window", be.Name)
			}
			if !exists(completedExpired) {
				t.Errorf("on %s: Run's startup sweep deleted a COMPLETED session (token_hash set) "+
					"whose expiry has passed -- session expiry is enforced by extractSession at read "+
					"time, not by deleting the row; the sweep must only ever touch token_hash IS NULL "+
					"rows", be.Name)
			}

			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("on %s: Run returned %v when its context was cancelled; "+
						"plugin.HasBackground's contract is to return promptly and cleanly, and a "+
						"non-nil error here would be logged as a failed background plugin at "+
						"shutdown", be.Name, err)
				}
			case <-time.After(15 * time.Second):
				t.Fatalf("on %s: Run did not return within 15s of context cancellation -- a "+
					"background loop that ignores cancellation hangs worker shutdown", be.Name)
			}
		})
	}
}
