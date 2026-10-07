package webhookingest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/eventtriggers"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// TestADeletedSourcesPendingEventIsCancelledNotDelivered is cleat-review's
// finding on #2221, across all three real dialects.
//
// An event ingested BEFORE a source is deleted, still pending, must never
// reach a workflow once the source is gone. Measured before the original
// fix: deleting the source left the event row untouched, and a subsequent
// background retry sweep delivered it anyway -- one new signal call, on all
// three dialects, for a webhook accepted during exactly the compromise
// window an operator's delete is meant to shut off.
//
// That sweep (background.go's processBatch/retryEvent, and the
// signal_workflow_id/signal_name static binding it served) was retired in
// cleat#2689 -- the correlated await_webhook path below is the only
// delivery mechanism left, so this test no longer has a PUSH half to pin.
// What remains, still pinned here: handleDeleteSource (routes.go) cancels
// the source's own pending events in the same transaction as the
// soft-delete (status='cancelled', processed=true) -- now purely an audit
// signal, since nothing reads webhook_events.processed for delivery
// anymore -- and, owner decision on cleat#2199, awaitWebhook
// (host_functions.go) refuses a cancelled event too, so the PULL path (a
// workflow's own await_webhook call) cannot hand it out either.
func TestADeletedSourcesPendingEventIsCancelledNotDelivered(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: quiet}

			// eventtriggers migrated and initialised too, cleat#2649:
			// handleDeleteSource now cancels pending ingested_events rows in
			// the same transaction as the soft-delete (routes.go), and
			// awaitWebhook claims from it directly -- both error outright if
			// that table does not exist. See
			// an_auth_exempt_route_cannot_assume_a_tenant_test.go's identical
			// pairing.
			et := &eventtriggers.Plugin{}
			if err := et.Init(ctx, &plugin.Environment{Dialect: dialect, Logger: quiet}); err != nil {
				t.Fatalf("eventtriggers Init: %v", err)
			}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}, {Plugin: et, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}

			// A real SecretStore, same fixed test key as this package's sibling
			// multidb tests -- reproduced here rather than shared, for the same
			// reason given there: nothing in this package can import a helper
			// from the harness without cycling.
			key := make([]byte, 32)
			for i := range key {
				key[i] = 0x5a
			}
			ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: key})
			if err != nil {
				t.Fatalf("build key ring: %v", err)
			}
			secretStore := engine.NewSecretStoreWithRing(be.DB, string(dialect), ring)
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.secrets = engine.NewPluginSecrets(secretStore)
			p.env = &plugin.Environment{Dialect: dialect, Logger: quiet}

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			tenantCtx := auth.WithTenantID(context.Background(), tenantID)

			const rawSecret = "delete-cancels-pending-event-secret"
			createBody := `{"name":"delete-cancels-pending","source_type":"github",` +
				`"secret":"` + rawSecret + `"}`
			createReq := httptest.NewRequest("POST", "/ingest/sources",
				strings.NewReader(createBody)).WithContext(tenantCtx)
			createRec := httptest.NewRecorder()
			p.handleCreateSource(createRec, createReq)
			if createRec.Code != http.StatusCreated {
				t.Fatalf("create source: want 201, got %d: %s", createRec.Code, createRec.Body.String())
			}
			var created map[string]any
			if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode create response: %v", err)
			}
			sourceID := created["id"].(string)

			payload := []byte(`{"pending":"at-delete-time"}`)
			mac := hmac.New(sha256.New, []byte(rawSecret))
			mac.Write(payload)
			sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

			ingestReq := httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader(payload))
			ingestReq.SetPathValue("source_id", sourceID)
			ingestReq.Header.Set("X-Hub-Signature-256", sig)
			ingestReq.Header.Set("X-Event-Type", "still.pending")
			ingestRec := httptest.NewRecorder()
			p.handleIngestWebhook(ingestRec, ingestReq)
			if ingestRec.Code != http.StatusCreated {
				t.Fatalf("ingest webhook: want 201, got %d: %s", ingestRec.Code, ingestRec.Body.String())
			}
			var ingested map[string]any
			if err := json.Unmarshal(ingestRec.Body.Bytes(), &ingested); err != nil {
				t.Fatalf("decode ingest response: %v", err)
			}
			eventID := ingested["id"].(string)

			// CrossTenantConn, not be.DB directly: webhook_events is
			// TenantScoped and SQL Server's policy applies even to sysadmin/dbo
			// (unlike PostgreSQL, where a superuser bypasses RLS
			// unconditionally) -- a bare be.DB read would find 0 rows whether
			// the event exists and is merely hidden or genuinely absent.
			readConn := be.CrossTenantConn(t, ctx,
				"cleat-review on #2221: confirming the event is genuinely still pending before the delete")

			// PRECONDITION: the event really is unprocessed, with no signal
			// having gone out -- otherwise every assertion below would pass
			// for the wrong reason (there being nothing left to cancel).
			var processedBefore bool
			var statusBefore string
			if err := plugintest.QueryRowRebound(t, ctx, readConn, dialect,
				`SELECT processed, COALESCE(status, '') FROM webhook_events WHERE id = $1`,
				eventID).Scan(&processedBefore, &statusBefore); err != nil {
				t.Fatalf("PRECONDITION: read event before delete: %v", err)
			}
			if processedBefore {
				t.Fatalf("PRECONDITION FAILED: event already processed before the delete")
			}
			if statusBefore != "pending" && statusBefore != "" {
				t.Fatalf("PRECONDITION FAILED: event status before delete: got %q, want pending or empty", statusBefore)
			}

			// The delete itself.
			deleteReq := httptest.NewRequest("DELETE", "/ingest/sources/"+sourceID, nil).WithContext(tenantCtx)
			deleteReq.SetPathValue("id", sourceID)
			deleteRec := httptest.NewRecorder()
			p.handleDeleteSource(deleteRec, deleteReq)
			if deleteRec.Code != http.StatusNoContent {
				t.Fatalf("delete source: want 204, got %d: %s", deleteRec.Code, deleteRec.Body.String())
			}

			// The cancellation itself, checked directly: status='cancelled',
			// processed=true, applied in the same transaction as the
			// soft-delete.
			var processedAfter bool
			var statusAfter string
			if err := plugintest.QueryRowRebound(t, ctx, readConn, dialect,
				`SELECT processed, status FROM webhook_events WHERE id = $1`,
				eventID).Scan(&processedAfter, &statusAfter); err != nil {
				t.Fatalf("read event after delete: %v", err)
			}
			if statusAfter != "cancelled" {
				t.Errorf("event status after delete: got %q, want %q", statusAfter, "cancelled")
			}
			if !processedAfter {
				t.Errorf("event processed after delete: got false, want true")
			}

			// PULL path: await_webhook must not hand it out -- owner
			// decision on cleat#2199.
			cc := &plugin.CallContext{TenantID: tenantID.String(), WorkflowID: "wf-review-2221", DB: be.DB}
			awaitCtx := plugin.WithCallContext(auth.WithTenantID(context.Background(), tenantID), cc)
			out, err := p.awaitWebhook(awaitCtx, AwaitWebhookInput{SourceID: sourceID})
			if err != nil {
				t.Fatalf("await_webhook: %v", err)
			}
			if out.Found {
				t.Errorf("await_webhook returned a deleted source's cancelled event (id=%s), want found=false", out.ID)
			}

			// The race guard, in isolation: handleIngestWebhook's own SELECT
			// (deleted_at IS NULL) has already refused every ingest against
			// this source since the delete -- that is what the pre-existing
			// "ingest after delete: want 404" assertion in this package's
			// sibling test covers. What it CANNOT exercise is the window
			// between that SELECT succeeding and the INSERT running: an
			// ingest whose lookup happened a moment before the delete lands.
			// Running the production INSERT text directly, against a source
			// this test has already deleted, is the deterministic
			// equivalent of losing that race every time, with no goroutines
			// or timing needed.
			raceEventID := uuid.New()
			rowsInserted, err := p.db.Exec(tenantCtx, `
				INSERT INTO webhook_events (id, source_id, tenant_id, event_type, headers, payload, received_at, processed)
				SELECT $1, $2, $3, $4, $5, $6, $7, false
				WHERE EXISTS (SELECT 1 FROM webhook_sources WHERE id = $8 AND deleted_at IS NULL)
			`, raceEventID, sourceID, tenantID, "raced.after.delete", "{}", "{}", time.Now(), sourceID)
			if err != nil {
				t.Fatalf("race-guarded INSERT against a deleted source: %v", err)
			}
			if rowsInserted != 0 {
				t.Errorf("race-guarded INSERT against a deleted source: inserted %d row(s), want 0", rowsInserted)
			}
		})
	}
}
