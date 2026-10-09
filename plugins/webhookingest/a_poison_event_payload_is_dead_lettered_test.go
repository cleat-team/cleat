package webhookingest

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// mustInsertPoisonIngestedEvent seeds an ingested_events row directly --
// bypassing handleIngestWebhook's own validation (part 1 of cleat#2666,
// PublishEvent's json.Valid check) the same way
// eventtriggers.mustInsertIngestedEventWithData does, for the same reason:
// this reproduces a row that already exists, not one a current write path
// could still create. key1 is set to sourceID, matching what
// handleIngestWebhook publishes and what awaitWebhook's beforeCommit closure
// (ClaimOrRegisterAwaiter's keys argument) correlates on -- see
// plugins/webhookingest/host_functions.go's "key1 = this source's id,
// ALWAYS first" comment.
func mustInsertPoisonIngestedEvent(t *testing.T, ctx context.Context, p *Plugin, id, tenantID, sourceID uuid.UUID, eventType, eventData string, receivedAt time.Time) {
	t.Helper()
	if _, err := p.db.Exec(ctx, `
		INSERT INTO ingested_events (id, tenant_id, event_type, event_data, key1, received_at, processed, status)
		VALUES ($1, $2, $3, $4, $5, $6, false, 'pending')
	`, id, tenantID, eventType, eventData, sourceID.String(), receivedAt); err != nil {
		t.Fatalf("insert ingested_events %s: %v", id, err)
	}
}

// TestAwaitWebhookPoisonPayloadDeadLettersTheEventInstead is cleat#2666's
// second call site: await_webhook's beforeCommit closure (host_functions.go)
// unmarshals the stored envelope the same way eventtriggers.awaitEvent
// marshals its output, and fails for the identical reason -- c.EventData
// (here, the whole {"source_id":...,"payload":...} envelope) being corrupt.
// Both callers share ClaimOrRegisterAwaiter/tryClaim (claim.go), so this is
// not re-testing that mechanism -- TestAwaitEventMarshalFailureDeadLettersTheEventInstead
// and TestAwaitEventSkipsAPoisonRowAndDeliversTheNextRealEventInTheSameCall
// (eventtriggers package) already do that thoroughly. This proves
// webhookingest's OWN closure actually wraps its failure with PoisonEvent,
// which is the one line specific to this package and the one a future edit
// here could silently drop.
//
// No webhook_sources row is seeded deliberately: awaitWebhook's own doc
// comment and TestADeletedSourcesPendingEventIsCancelledNotDelivered already
// establish that a source lookup miss (sql.ErrNoRows) is not an error and
// does not block the claim -- ClaimOrRegisterAwaiter runs regardless. Adding
// a source row here would test a second thing this test is not about.
//
// MSSQL-only, same reason as its eventtriggers counterpart: Postgres/MySQL
// refuse the malformed literal at INSERT (event_data is JSONB/JSON there),
// so there is no poison row to seed on those two dialects.
func TestAwaitWebhookPoisonPayloadDeadLettersTheEventInstead(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectMSSQL)
	dialect := plugin.DialectMSSQL
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	p := &Plugin{dialect: dialect, logger: quiet}
	if err := plugin.RunMigrations(context.Background(), db, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	p.db = &engine.SQLDBAdapter{DB: db, Dialect: dialect}

	tenantID := uuid.New()
	sourceID := uuid.New()
	eventID := uuid.New()
	seedCtx := plugin.ForTenant(context.Background(), tenantID)

	// Not valid JSON at all, so json.Unmarshal into the envelope struct
	// fails outright -- verified directly: json.Unmarshal([]byte("not valid
	// json{"), &struct{...}{}) returns a non-nil error.
	mustInsertPoisonIngestedEvent(t, seedCtx, p, eventID, tenantID, sourceID,
		defaultWebhookEventType, "not valid json{", time.Now().Add(-time.Hour))

	ctx := plugin.WithCallContext(seedCtx, &plugin.CallContext{
		TenantID:   tenantID.String(),
		WorkflowID: "wf-poison-webhook-mssql-" + tenantID.String(),
	})
	out, err := p.awaitWebhook(ctx, AwaitWebhookInput{SourceID: sourceID.String()})
	if err != nil {
		t.Fatalf("awaitWebhook: expected no error (the poison row should be handled "+
			"internally), got: %v", err)
	}
	if out.Found {
		t.Fatalf("awaitWebhook reported Found:true -- the poison row should never be "+
			"delivered to a caller, got %+v", out)
	}

	var processed bool
	var status, errMsg sql.NullString
	row := p.db.QueryRow(seedCtx,
		`SELECT processed, status, error_msg FROM ingested_events WHERE id = $1`, eventID)
	if err := plugin.ScanRow(row, &processed, &status, &errMsg); err != nil {
		t.Fatalf("query event row: %v", err)
	}
	if !processed {
		t.Fatal("event was NOT marked processed -- it is still the oldest unprocessed row " +
			"for its (tenant, event type) and will block every future awaitWebhook for that " +
			"type, the exact stall cleat#2666 describes")
	}
	if status.String != "dead_letter" {
		t.Fatalf("status = %q, want \"dead_letter\"", status.String)
	}
	if !strings.Contains(errMsg.String, "unwrap event payload") {
		t.Fatalf("error_msg = %q, want it to name the unwrap failure", errMsg.String)
	}
}
