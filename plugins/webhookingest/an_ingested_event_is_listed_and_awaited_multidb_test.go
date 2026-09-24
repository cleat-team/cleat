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

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestAnIngestedEventIsListedAndAwaited is cleat#1992/#2172's end-to-end pin,
// across all three real dialects, for the two webhookingest reads
// cleat-review's re-check on #2198 found broken on SQL Server -- both hit the
// same bug class as notifications/routes.go's handleListDeliveries, fixed
// the same way:
//
//   - GET /ingest/events (handleListEvents): built its row limit as a literal
//     "LIMIT $N", which is not valid T-SQL -- 500 on every call.
//   - await_webhook (awaitWebhook, the host function a workflow calls):
//     the same literal LIMIT, so a workflow could never see an ingested
//     event on SQL Server at all -- not a degraded read, a hard error on
//     the one path guest code actually uses.
//
// Both are fixed with plugin.LimitClause, the same helper #2191 used for
// /audit/events and this PR's sibling fix in notifications/routes.go.
func TestAnIngestedEventIsListedAndAwaited(t *testing.T) {
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

			// A REAL SecretStore, same fixed test key as this package's sibling
			// multidb tests (a_retired_secret_refuses_ingest_multidb_test.go) --
			// reproduced here rather than shared, for the same reason given
			// there: nothing in this package can import a helper from the
			// harness without cycling.
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

			// Create a signed source through the real route.
			const rawSecret = "ingest-list-await-secret"
			createBody := `{"name":"listed-source","source_type":"github","secret":"` + rawSecret + `"}`
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

			// Ingest one event through the real route -- not seeded directly
			// into webhook_events, which would prove nothing about
			// handleIngestWebhook's own signature check or insert.
			payload := []byte(`{"listed":"event"}`)
			mac := hmac.New(sha256.New, []byte(rawSecret))
			mac.Write(payload)
			sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

			ingestReq := httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader(payload))
			ingestReq.SetPathValue("source_id", sourceID)
			ingestReq.Header.Set("X-Hub-Signature-256", sig)
			ingestReq.Header.Set("X-Event-Type", "listed.event")
			ingestRec := httptest.NewRecorder()
			p.handleIngestWebhook(ingestRec, ingestReq)
			if ingestRec.Code != http.StatusCreated {
				t.Fatalf("ingest webhook: want 201, got %d: %s", ingestRec.Code, ingestRec.Body.String())
			}

			// GET /ingest/events, through the real route. cleat-review found
			// this 500ing on MSSQL with "Incorrect syntax near 'LIMIT'".
			//
			// Filtered by source_id: webhook_events has no per-test isolation
			// (NewPluginTestBackends' Cleanup only closes the connection, and
			// this table is not in engine/testutil's cleanup lists, which cover
			// only core engine tables) -- unfiltered, this would see every
			// event any run against this database has ever ingested for the
			// default tenant, not just this test's own.
			listReq := httptest.NewRequest("GET", "/ingest/events?source_id="+sourceID, nil).WithContext(tenantCtx)
			listRec := httptest.NewRecorder()
			p.handleListEvents(listRec, listReq)
			if listRec.Code != http.StatusOK {
				t.Fatalf("list events: want 200, got %d: %s", listRec.Code, listRec.Body.String())
			}
			var events []map[string]any
			if err := json.Unmarshal(listRec.Body.Bytes(), &events); err != nil {
				t.Fatalf("decode events list: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("events list: got %d entries, want 1: %s", len(events), listRec.Body.String())
			}
			if events[0]["event_type"] != "listed.event" {
				t.Errorf("event_type in list: got %v, want %q", events[0]["event_type"], "listed.event")
			}
			if events[0]["processed"] != false {
				t.Errorf("processed in list before await_webhook: got %v, want false", events[0]["processed"])
			}

			// await_webhook, the host function a workflow calls -- through the
			// real host function, exactly as a running workflow would.
			// cleat-review found this erroring outright on SQL Server: a
			// workflow could never see an ingested event there at all.
			cc := &plugin.CallContext{
				TenantID:   tenantID.String(),
				WorkflowID: "wf-test",
				DB:         be.DB,
			}
			// auth.WithTenantID + plugin.WithCallContext, the same pairing this
			// PR's sibling notifications test uses, for the same reason: RLS on
			// SQL Server has no superuser exemption, so the row is genuinely
			// invisible without the session context set.
			awaitCtx := plugin.WithCallContext(auth.WithTenantID(context.Background(), tenantID), cc)
			awaitInput := `{"source_id":"` + sourceID + `","event_type":"listed.event"}`
			outJSON, err := p.awaitWebhook(awaitCtx, awaitInput)
			if err != nil {
				t.Fatalf("await_webhook: %v", err)
			}
			var out awaitWebhookOutput
			if err := json.Unmarshal([]byte(outJSON), &out); err != nil {
				t.Fatalf("decode await_webhook output: %v (%q)", err, outJSON)
			}
			if !out.Found {
				t.Fatalf("await_webhook: found=false, want true (output: %s)", outJSON)
			}
			if out.EventType != "listed.event" {
				t.Errorf("await_webhook event_type: got %q, want %q", out.EventType, "listed.event")
			}
			var gotPayload, wantPayload map[string]any
			if err := json.Unmarshal(out.Payload, &gotPayload); err != nil {
				t.Fatalf("await_webhook payload is not valid JSON: %v (%q)", err, out.Payload)
			}
			if err := json.Unmarshal(payload, &wantPayload); err != nil {
				t.Fatalf("test payload is not valid JSON: %v", err)
			}
			if gotPayload["listed"] != wantPayload["listed"] {
				t.Errorf("await_webhook payload: got %v, want %v", gotPayload, wantPayload)
			}

			// A second await_webhook call finds nothing: the first call marked
			// the event processed, so this is a control that await_webhook's
			// own LIMIT/ORDER BY actually selects the right (and only) row
			// rather than something that happens to satisfy `found: true` once
			// by accident.
			outJSON2, err := p.awaitWebhook(awaitCtx, awaitInput)
			if err != nil {
				t.Fatalf("await_webhook (second call): %v", err)
			}
			var out2 awaitWebhookOutput
			if err := json.Unmarshal([]byte(outJSON2), &out2); err != nil {
				t.Fatalf("decode await_webhook output (second call): %v (%q)", err, outJSON2)
			}
			if out2.Found {
				t.Errorf("await_webhook (second call): found=true, want false -- the event was "+
					"already consumed and marked processed by the first call (output: %s)", outJSON2)
			}
		})
	}
}
