package notifications

import (
	"context"
	"encoding/json"
	"fmt"
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
)

// TestADeletedWebhooksPendingDeliveriesAreCancelledNotSent is cleat#2220's own
// test, across all three real dialects: a tenant deleting a webhook that has
// pending/retrying deliveries must not have them go out afterward, the
// webhook itself must read as gone everywhere (GET/PUT/DELETE/ListDeliveries,
// sendWebhook), and repeating the delete must be a 404, not a silent no-op --
// the same shape cleat#2199 proved for webhookingest's sources, applied here
// from the owner's explicit "matching #2199" decision on this issue.
//
// It pins three independent layers, matching handleDeleteWebhook's own
// design note (routes.go): the proactive cancellation in the delete's own
// transaction, queryDueDeliveries' defense-in-depth join (background.go), and
// the guarded INSERT in sendWebhook (host_functions.go) that stops a new
// delivery from being created for an already-deleted webhook at all.
func TestADeletedWebhooksPendingDeliveriesAreCancelledNotSent(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: quiet, httpClient: &http.Client{Timeout: 5 * time.Second}}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}

			// A real SecretStore, the same fixed test key as this package's other
			// multidb tests and webhookingest's sibling: nothing in this package
			// can import a helper from the harness without cycling.
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

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			tenantCtx := auth.WithTenantID(context.Background(), tenantID)

			// The mock HTTP server: if the background sweep ever attempts either
			// of this webhook's cancelled deliveries, this test must fail rather
			// than pass by luck of timing, so it fails the delivery loudly
			// instead of just counting it.
			var delivered int
			mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				delivered++
				w.WriteHeader(http.StatusOK)
			}))
			defer mockSrv.Close()

			// ---- create the webhook ----
			const rawSecret = "delete-cancels-pending-deliveries-secret"
			createBody := fmt.Sprintf(`{"url":%q,"secret":%q,"events":["test.event"]}`, mockSrv.URL, rawSecret)
			createReq := httptest.NewRequest("POST", "/webhooks", strings.NewReader(createBody)).WithContext(tenantCtx)
			createRec := httptest.NewRecorder()
			p.handleCreateWebhook(createRec, createReq)
			if createRec.Code != http.StatusCreated {
				t.Fatalf("create webhook: want 201, got %d: %s", createRec.Code, createRec.Body.String())
			}
			var created map[string]any
			if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode create response: %v", err)
			}
			webhookID := uuid.MustParse(created["id"].(string))

			// ---- seed a pending and a retrying delivery directly ----
			// Direct INSERTs, not sendWebhook: this test wants deterministic
			// control over both statuses queryDueDeliveries selects (pending AND
			// retrying), rather than depending on a prior delivery attempt
			// having failed once to reach 'retrying'.
			pendingID := uuid.New()
			retryingID := uuid.New()
			past := time.Now().Add(-1 * time.Hour)
			if _, err := p.db.Exec(tenantCtx, plugin.Rebind(`
				INSERT INTO webhook_delivery (id, webhook_id, event_type, payload, status, attempt_count, next_attempt_at, created_at)
				VALUES ($1, $2, 'test.event', '{}', 'pending', 0, $3, $4)
			`, dialect), pendingID, webhookID, past, past); err != nil {
				t.Fatalf("seed pending delivery: %v", err)
			}
			if _, err := p.db.Exec(tenantCtx, plugin.Rebind(`
				INSERT INTO webhook_delivery (id, webhook_id, event_type, payload, status, attempt_count, next_attempt_at, created_at)
				VALUES ($1, $2, 'test.event', '{}', 'retrying', 1, $3, $4)
			`, dialect), retryingID, webhookID, past, past); err != nil {
				t.Fatalf("seed retrying delivery: %v", err)
			}

			// CrossTenantConn, not be.DB directly: webhook_config is TenantScoped
			// and SQL Server's policy applies even to sysadmin/dbo, unlike
			// PostgreSQL where a superuser bypasses RLS unconditionally -- a bare
			// be.DB read would find 0 rows whether the row is genuinely absent or
			// merely hidden.
			readConn := be.CrossTenantConn(t, ctx, "cleat#2220: webhook_config/webhook_delivery rows around a delete")

			// PRECONDITIONS: both deliveries really are pending/retrying, and the
			// webhook really is live, before the delete -- otherwise every
			// assertion below would pass for the wrong reason.
			var statusBefore [2]string
			for i, id := range []uuid.UUID{pendingID, retryingID} {
				if err := readConn.QueryRowContext(ctx, plugin.Rebind(
					`SELECT status FROM webhook_delivery WHERE id = $1`, dialect), id).Scan(&statusBefore[i]); err != nil {
					t.Fatalf("PRECONDITION: read delivery %s before delete: %v", id, err)
				}
			}
			if statusBefore[0] != "pending" || statusBefore[1] != "retrying" {
				t.Fatalf("PRECONDITION FAILED: delivery statuses before delete: %v, want [pending retrying]", statusBefore)
			}
			var enabledBefore bool
			var deletedAtBefore any
			if err := readConn.QueryRowContext(ctx, plugin.Rebind(
				`SELECT enabled, deleted_at FROM webhook_config WHERE id = $1`, dialect),
				webhookID).Scan(&enabledBefore, &deletedAtBefore); err != nil {
				t.Fatalf("PRECONDITION: read webhook_config before delete: %v", err)
			}
			if !enabledBefore || deletedAtBefore != nil {
				t.Fatalf("PRECONDITION FAILED: webhook_config before delete: enabled=%v deleted_at=%v, want true/nil",
					enabledBefore, deletedAtBefore)
			}

			// ---- the delete itself ----
			deleteReq := httptest.NewRequest("DELETE", "/webhooks/"+webhookID.String(), nil).WithContext(tenantCtx)
			deleteReq.SetPathValue("id", webhookID.String())
			deleteRec := httptest.NewRecorder()
			p.handleDeleteWebhook(deleteRec, deleteReq)
			if deleteRec.Code != http.StatusNoContent {
				t.Fatalf("delete webhook: want 204, got %d: %s", deleteRec.Code, deleteRec.Body.String())
			}

			// ---- the soft-delete itself, checked directly ----
			var enabledAfter bool
			var deletedAtAfter any
			if err := readConn.QueryRowContext(ctx, plugin.Rebind(
				`SELECT enabled, deleted_at FROM webhook_config WHERE id = $1`, dialect),
				webhookID).Scan(&enabledAfter, &deletedAtAfter); err != nil {
				t.Fatalf("read webhook_config after delete: %v", err)
			}
			if enabledAfter {
				t.Errorf("webhook_config.enabled after delete: got true, want false")
			}
			if deletedAtAfter == nil {
				t.Errorf("webhook_config.deleted_at after delete: got nil, want set")
			}

			// ---- both deliveries cancelled, in the SAME transaction ----
			for i, id := range []uuid.UUID{pendingID, retryingID} {
				var status string
				if err := readConn.QueryRowContext(ctx, plugin.Rebind(
					`SELECT status FROM webhook_delivery WHERE id = $1`, dialect), id).Scan(&status); err != nil {
					t.Fatalf("read delivery %s after delete: %v", id, err)
				}
				if status != "cancelled" {
					t.Errorf("delivery %d (%s) status after delete: got %q, want %q", i, id, status, "cancelled")
				}
			}

			// ---- every read path treats the webhook as gone ----
			getReq := httptest.NewRequest("GET", "/webhooks/"+webhookID.String(), nil).WithContext(tenantCtx)
			getReq.SetPathValue("id", webhookID.String())
			getRec := httptest.NewRecorder()
			p.handleGetWebhook(getRec, getReq)
			if getRec.Code != http.StatusNotFound {
				t.Errorf("GET deleted webhook: want 404, got %d: %s", getRec.Code, getRec.Body.String())
			}

			putReq := httptest.NewRequest("PUT", "/webhooks/"+webhookID.String(),
				strings.NewReader(`{"url":"https://new.example.com"}`)).WithContext(tenantCtx)
			putReq.SetPathValue("id", webhookID.String())
			putRec := httptest.NewRecorder()
			p.handleUpdateWebhook(putRec, putReq)
			if putRec.Code != http.StatusNotFound {
				t.Errorf("PUT deleted webhook: want 404, got %d: %s", putRec.Code, putRec.Body.String())
			}

			deliveriesReq := httptest.NewRequest("GET", "/webhooks/"+webhookID.String()+"/deliveries", nil).WithContext(tenantCtx)
			deliveriesReq.SetPathValue("id", webhookID.String())
			deliveriesRec := httptest.NewRecorder()
			p.handleListDeliveries(deliveriesRec, deliveriesReq)
			if deliveriesRec.Code != http.StatusNotFound {
				t.Errorf("LIST deliveries for deleted webhook: want 404, got %d: %s", deliveriesRec.Code, deliveriesRec.Body.String())
			}

			// handleListWebhooks was the one read path GET/PUT/DELETE/
			// ListDeliveries above did not cover: coordinator's #2233 review
			// found it unasserted -- its own query carries the same
			// "AND deleted_at IS NULL" as every other read here, but nothing
			// proved a soft-deleted webhook is actually absent from the list
			// rather than merely 404ing when addressed directly.
			listReq := httptest.NewRequest("GET", "/webhooks", nil).WithContext(tenantCtx)
			listRec := httptest.NewRecorder()
			p.handleListWebhooks(listRec, listReq)
			if listRec.Code != http.StatusOK {
				t.Fatalf("LIST webhooks: want 200, got %d: %s", listRec.Code, listRec.Body.String())
			}
			var listed []webhookConfigJSON
			if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
				t.Fatalf("decode list response: %v", err)
			}
			for _, c := range listed {
				if c.ID == webhookID {
					t.Errorf("LIST webhooks after delete: the deleted webhook %s is still present", webhookID)
				}
			}

			// Repeating the delete is a 404, not a silent no-op success: the
			// production WHERE clause's own "deleted_at IS NULL" makes a second
			// soft-delete affect 0 rows.
			redeleteReq := httptest.NewRequest("DELETE", "/webhooks/"+webhookID.String(), nil).WithContext(tenantCtx)
			redeleteReq.SetPathValue("id", webhookID.String())
			redeleteRec := httptest.NewRecorder()
			p.handleDeleteWebhook(redeleteRec, redeleteReq)
			if redeleteRec.Code != http.StatusNotFound {
				t.Errorf("re-DELETE already-deleted webhook: want 404, got %d: %s", redeleteRec.Code, redeleteRec.Body.String())
			}

			// ---- defense-in-depth: a pending delivery for a soft-deleted
			// webhook is still never attempted, even if it somehow exists ----
			// (the cancellation above already stopped these two, so this seeds a
			// THIRD delivery, written directly and bypassing the app layer
			// entirely, to exercise queryDueDeliveries' own independent
			// deleted_at guard rather than merely re-observing the cancellation.)
			orphanID := uuid.New()
			if _, err := p.db.Exec(tenantCtx, plugin.Rebind(`
				INSERT INTO webhook_delivery (id, webhook_id, event_type, payload, status, attempt_count, next_attempt_at, created_at)
				VALUES ($1, $2, 'test.event', '{}', 'pending', 0, $3, $4)
			`, dialect), orphanID, webhookID, past, past); err != nil {
				t.Fatalf("seed post-delete pending delivery: %v", err)
			}
			// AcrossAllTenants, the same marking Run() applies before calling
			// processDeliveries (background.go's own doc comment on Run
			// explains why): without it, on SQL Server, webhook_config's own
			// FILTER predicate hides every row from a session with no tenant in
			// SESSION_CONTEXT -- INCLUDING the ones this assertion means to
			// prove are correctly excluded -- so the query would read 0 rows
			// regardless of whether queryDueDeliveries' own deleted_at guard is
			// present or not, and this assertion would pass for the wrong
			// reason on that dialect. Caught by falsifying the guard itself
			// (removing "AND wc.deleted_at IS NULL" from the JOIN) and finding
			// the mssql subtest stayed green -- it was RLS hiding everything,
			// not the guard being exercised.
			//
			// The sweep's own attempted/succeeded/failed RETURN VALUES are NOT
			// asserted on here, deliberately: they count every due delivery in
			// the whole database, tenant-scoped test data from a PRIOR run
			// included, since nothing truncates this shared, persistent
			// database between runs. Coordinator's #2233 review found exactly
			// that -- attempted!=0 on a second run against PG/MSSQL, from
			// leftover rows the first run's cleanup never reached. What this
			// test owns is orphanID's own row, so that is what gets checked,
			// by identity rather than by a count something else can inflate.
			sweepCtx := plugin.AcrossAllTenants(ctx, "cleat#2220 test: sweeping due deliveries across tenants")
			if _, _, _, err := p.processDeliveries(sweepCtx, ctx); err != nil {
				t.Fatalf("processDeliveries: %v", err)
			}
			var orphanStatus string
			var orphanAttempts int
			var orphanLastAttempt any
			if err := readConn.QueryRowContext(ctx, plugin.Rebind(
				`SELECT status, attempt_count, last_attempt_at FROM webhook_delivery WHERE id = $1`, dialect),
				orphanID).Scan(&orphanStatus, &orphanAttempts, &orphanLastAttempt); err != nil {
				t.Fatalf("read orphan delivery after sweep: %v", err)
			}
			if orphanStatus != "pending" || orphanAttempts != 0 || orphanLastAttempt != nil {
				t.Errorf("orphan delivery %s after sweep: status=%q attempt_count=%d last_attempt_at=%v, "+
					"want pending/0/nil (queryDueDeliveries' join must exclude it)",
					orphanID, orphanStatus, orphanAttempts, orphanLastAttempt)
			}
			if delivered != 0 {
				t.Errorf("mock HTTP server received %d request(s) for a deleted webhook, want 0", delivered)
			}

			// ---- sendWebhook (host function) refuses the deleted webhook ----
			cc := &plugin.CallContext{TenantID: tenantID.String(), WorkflowID: "wf-2220", DB: be.DB}
			sendCtx := plugin.WithCallContext(auth.WithTenantID(context.Background(), tenantID), cc)
			sendInput := fmt.Sprintf(`{"webhook_id":"%s","event_type":"test.event"}`, webhookID)
			if _, err := p.sendWebhook(sendCtx, sendInput); err == nil {
				t.Errorf("sendWebhook against a deleted webhook: want an error, got nil")
			} else if !strings.Contains(err.Error(), "webhook not found") {
				t.Errorf("sendWebhook against a deleted webhook: got %q, want it to mention \"webhook not found\"", err)
			}

			// The race window itself -- a sendWebhook call whose existence
			// check ran a moment before the delete lands, racing the delete's
			// own open transaction -- used to be covered here by a retyped
			// copy of host_functions.go's guarded INSERT text run
			// deterministically against an already-deleted webhook.
			// cleat-review measured that a retyped copy passes even with the
			// real guard mutated to `1=1 OR EXISTS` or with FOR SHARE
			// dropped entirely, because it is not the code under test. It is
			// replaced by
			// TestASendWebhookRacingAnOpenDeleteTransactionIsBlocked
			// (a_send_webhook_racing_an_open_delete_is_blocked_test.go),
			// which calls the real p.sendWebhook concurrently against a
			// delete held open in an uncommitted transaction and proves the
			// BLOCK, not merely the eventual answer -- PostgreSQL and SQL
			// Server (with READ_COMMITTED_SNAPSHOT forced ON, the
			// configuration the gap exists under), matching #2221's own
			// live-interleaving template for webhookingest.
		})
	}
}
