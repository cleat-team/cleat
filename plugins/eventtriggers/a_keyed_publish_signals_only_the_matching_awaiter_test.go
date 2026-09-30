package eventtriggers

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#2625, P1 of docs/contributor/design/event-routing-design.md: two
// awaiters for the same tenant and event type, holding DIFFERENT correlation
// keys, must not wake each other. Before this, signalAwaiters and
// registerAwaiter matched on (tenant_id, event_type) alone, so a shared
// event type -- the exact shape order-lifecycle's webhook wait needs, where
// many order runs all await "payment.captured" -- had no way to tell them
// apart: publishing one order's event woke every run waiting on that type.
//
// THE CONTROL IS THE SECOND HALF OF THIS TEST, NOT A SEPARATE ONE. Asserting
// only "wf-A991 got signalled" would pass just as well against the OLD,
// unkeyed code, which signals every awaiter of the type -- wf-B2 would also
// receive it, silently, and nothing above would say so. Asserting wf-B2
// does NOT receive it and remains registered is what the key predicate is
// actually for; see the falsification note below.
func TestAKeyedPublishSignalsOnlyTheMatchingAwaiter(t *testing.T) {
	store := newETDBStore()
	db := sql.OpenDB(&etConnector{store: store})
	defer db.Close()

	adapter := &engine.SQLDBAdapter{DB: db}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tenantID := etTestTenantID

	p := &Plugin{db: adapter, logger: logger}

	// Two runs, same tenant, same event type, different orders.
	if err := registerAwaiterCore(context.Background(), p.db, p.dialect, p.logger, tenantID.String(), "wf-A991", "payment.captured", "A-991", "", ""); err != nil {
		t.Fatalf("registerAwaiter A-991: %v", err)
	}
	if err := registerAwaiterCore(context.Background(), p.db, p.dialect, p.logger, tenantID.String(), "wf-B2", "payment.captured", "B-2", "", ""); err != nil {
		t.Fatalf("registerAwaiter B-2: %v", err)
	}

	store.mu.RLock()
	if n := len(store.awaiters); n != 2 {
		store.mu.RUnlock()
		t.Fatalf("UNMEASURED: expected 2 registered awaiters before publishing, got %d", n)
	}
	store.mu.RUnlock()

	var signalled []string
	env := &plugin.Environment{
		SignalWorkflow: func(_ context.Context, workflowID, _, _ string) error {
			signalled = append(signalled, workflowID)
			return nil
		},
	}

	// A-991's payment arrives. key1 matches wf-A991's registration only.
	signalAwaiters(context.Background(), adapter, logger, env, tenantID,
		"payment.captured", `{"orderID":"A-991"}`, "A-991", "", "")

	// FLOOR: without this, a fake or a predicate that signals nobody would
	// make the isolation assertion below pass vacuously.
	if len(signalled) != 1 {
		t.Fatalf("UNMEASURED: expected exactly 1 signal, got %d: %v", len(signalled), signalled)
	}
	if signalled[0] != "wf-A991" {
		t.Errorf("signalled %v, want [wf-A991]", signalled)
	}

	// THE ASSERTION THIS TEST EXISTS FOR: wf-B2 was never signalled and is
	// still registered -- a keyed publish for one order does not wake, and
	// does not unregister, a run waiting on a different order's key.
	store.mu.RLock()
	var remaining []string
	for _, a := range store.awaiters {
		remaining = append(remaining, a.workflowID)
	}
	store.mu.RUnlock()
	if len(remaining) != 1 || remaining[0] != "wf-B2" {
		t.Errorf("remaining awaiters = %v, want exactly [wf-B2] -- wf-A991 should have been "+
			"unregistered on match, and wf-B2 must still be waiting since its key never matched",
			remaining)
	}
}

// A publish that carries no keys must still reach an awaiter that also
// asked for none -- the pre-existing, single-listener shape (e.g.
// webhookingest's current nil-keys callers) must keep working exactly as
// before. Regression coverage for the sentinel-empty-string rule in
// migrations.go's Version 6: an unkeyed event and an unkeyed awaiter both
// carry the empty-string sentinel in all three slots, so three-way equality
// still matches them.
func TestAnUnkeyedPublishStillReachesAnUnkeyedAwaiter(t *testing.T) {
	store := newETDBStore()
	db := sql.OpenDB(&etConnector{store: store})
	defer db.Close()

	adapter := &engine.SQLDBAdapter{DB: db}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tenantID := uuid.New()

	p := &Plugin{db: adapter, logger: logger}
	if err := registerAwaiterCore(context.Background(), p.db, p.dialect, p.logger, tenantID.String(), "wf-plain", "order.shipped", "", "", ""); err != nil {
		t.Fatalf("registerAwaiter: %v", err)
	}

	var signalled int
	env := &plugin.Environment{
		SignalWorkflow: func(context.Context, string, string, string) error {
			signalled++
			return nil
		},
	}
	signalAwaiters(context.Background(), adapter, logger, env, tenantID,
		"order.shipped", `{}`, "", "", "")

	if signalled != 1 {
		t.Errorf("signalled = %d, want 1 -- an unkeyed publish must still reach an unkeyed awaiter", signalled)
	}
}
