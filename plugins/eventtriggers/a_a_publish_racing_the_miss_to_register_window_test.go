package eventtriggers

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestAPublishRacingTheMissToRegisterWindowIsNotALostWakeup pins
// cleat-review's finding on cleat#2695: PublishEvent's INSERT commits, then
// it calls signalAwaiters in a SEPARATE step (publish.go) -- so a publish
// can land ENTIRELY inside the window between ClaimOrRegisterAwaiter's
// first claim attempt missing and its registerAwaiterCore write.
// signalAwaiters' own query runs before that awaiter row exists, finds
// nothing, and never looks again for that same event -- so without a second
// claim attempt after registering, the published row sits in
// ingested_events unprocessed forever and the awaiter is never signalled.
//
// This was always possible and always harmless before cleat#2649: every
// caller was a poll, so a caller that re-invoked this whole function on a
// timer would have its NEXT first-attempt claim find the row directly.
// cleat#2649 gave webhookingest's await_webhook a caller
// (examples/order-lifecycle) that suspends on the registered awaiter's
// signal instead of polling -- exactly the shape that turns an always-latent
// race into an actual, user-visible timeout.
//
// afterFirstMissBeforeRegister (claim.go) is a test-only hook, called
// between the first miss and the registration -- exactly the window this
// test needs to publish into, deterministically, with no goroutines or
// timing to get flaky.
func TestAPublishRacingTheMissToRegisterWindowIsNotALostWakeup(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	dialect := plugin.DialectPostgres
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	p := &Plugin{}
	if err := plugin.RunMigrations(context.Background(), db, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pdb := &engine.SQLDBAdapter{DB: db, Dialect: dialect}
	if err := p.Init(context.Background(), &plugin.Environment{
		DB: pdb, Dialect: dialect, Logger: quiet,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	tenantID := uuid.New()
	seedCtx := plugin.ForTenant(context.Background(), tenantID)

	const eventType = "order.paid"
	keys := []string{"src-races-the-window", "order-42"}

	// The hook: publish the exact event ClaimOrRegisterAwaiter is about to
	// register an awaiter for, in the window where a real concurrent
	// publisher could land -- between the first miss and the registration.
	// env is nil: signalAwaiters checks env == nil and returns immediately
	// (publish.go), so this does not need a fake SignalWorkflow -- the whole
	// point of this test is that the SIGNAL is not what has to save it.
	var publishErr error
	afterFirstMissBeforeRegister = func() {
		_, publishErr = PublishEvent(seedCtx, pdb, quiet, nil,
			uuid.New(), tenantID, eventType, []byte(`{"hello":"world"}`), keys)
	}
	t.Cleanup(func() { afterFirstMissBeforeRegister = nil })

	claimed, err := ClaimOrRegisterAwaiter(seedCtx, pdb, dialect, quiet,
		tenantID.String(), "wf-races-the-window", eventType, keys, nil)
	if err != nil {
		t.Fatalf("ClaimOrRegisterAwaiter: %v", err)
	}
	if publishErr != nil {
		t.Fatalf("UNMEASURED: the hook's PublishEvent call itself failed (%v) -- "+
			"this run says nothing about the race", publishErr)
	}

	// THE FINDING: without the re-check, this is nil -- the publish above
	// landed after the first miss (which is why it wasn't found then) and
	// before signalAwaiters could have found the awaiter (which does not
	// exist until registerAwaiterCore, called after this hook), so nothing
	// would ever have delivered it. With the fix, the second claim attempt
	// (after registration) finds exactly the row the hook just published.
	if claimed == nil {
		t.Fatalf("lost wakeup: a publish landing exactly between the first miss and the " +
			"awaiter registration was never claimed and would never be signalled either")
	}
	if claimed.EventType != eventType {
		t.Errorf("claimed event_type: got %q, want %q", claimed.EventType, eventType)
	}

	// Control: the awaiter registered in the race window must not be left
	// behind once this call delivers the event directly -- a stale awaiter
	// row is exactly the spurious-wake hazard cleat-review's GAP 2 warns a
	// caller has to tolerate rather than rely on being absent.
	var awaiterCount int
	row := pdb.QueryRow(seedCtx,
		`SELECT COUNT(*) FROM event_awaiters WHERE workflow_id = $1 AND event_type = $2`,
		"wf-races-the-window", eventType)
	if err := plugin.ScanRow(row, &awaiterCount); err != nil {
		t.Fatalf("count awaiters: %v", err)
	}
	if awaiterCount != 0 {
		t.Errorf("awaiter rows left behind after direct delivery: got %d, want 0", awaiterCount)
	}
}
