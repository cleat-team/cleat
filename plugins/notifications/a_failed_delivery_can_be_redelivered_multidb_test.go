package notifications

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// apiKeyResolver is the one thing auth.MiddlewareWithMux needs from a store: the tenant a key hash
// belongs to. It knows a single key.
type apiKeyResolver struct {
	hash   []byte
	tenant uuid.UUID
}

func (r apiKeyResolver) ResolveTenantFromAPIKey(_ context.Context, keyHash []byte) (uuid.UUID, error) {
	if bytes.Equal(keyHash, r.hash) {
		return r.tenant, nil
	}
	return uuid.Nil, sql.ErrNoRows
}

// cleat#3612: POST /webhooks/{id}/deliveries/{delivery_id}/redeliver puts one FAILED delivery
// back to pending so the sweep sends it again, and refuses everything else.
//
// A delivery that ended failed was lost to the operator: the payload sat in the row and no
// route sent it. That is what kept 401, 403 and 404 (and, after review of cleat#3616, 400) from
// being classed permanent. Every case here goes through the real routes, a real receiver, one
// real sweep, and the delivery read back through the real list route, on all three dialects.
//
// The first case is the point of the route: a receiver refuses, the cause is repaired, the
// delivery is redelivered and ARRIVES, with the same body and the same signature. The rest are
// what it must not do: resend a delivery that is not failed (it would send twice), reach another
// webhook's delivery or another tenant's, or reset one behind a disabled webhook, where it would
// sit pending and look like a send that did not happen.
func TestAFailedDeliveryCanBeRedeliveredAndOnlyAFailedOne(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: quiet, httpClient: &http.Client{Timeout: 5 * time.Second}}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}
			key := make([]byte, 32)
			for i := range key {
				key[i] = 0x5a
			}
			ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: key})
			if err != nil {
				t.Fatalf("build key ring: %v", err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.secrets = engine.NewPluginSecrets(engine.NewSecretStoreWithRing(be.DB, string(dialect), ring))

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			tenantCtx := auth.WithTenantID(context.Background(), tenantID)
			operatorCtx := auth.WithSubject(tenantCtx, "operator@example.com")
			sendCtx := plugin.WithCallContext(auth.WithTenantID(context.Background(), tenantID),
				&plugin.CallContext{TenantID: tenantID.String(), WorkflowID: "wf-redeliver", DB: be.DB})
			sweepCtx := plugin.AcrossAllTenants(ctx, "test: multidb redeliver sweep")

			// A receiver per webhook, so a delivery left pending in one case cannot be swept into
			// another. It records every request body and signature it was sent.
			type receiver struct {
				srv    *httptest.Server
				status atomic.Int32
				mu     sync.Mutex
				bodies []string
				sigs   []string
			}
			hits := func(r *receiver) int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.bodies) }
			newWebhook := func(t *testing.T, status int) (string, *receiver) {
				t.Helper()
				r := &receiver{}
				r.status.Store(int32(status))
				r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					b, _ := io.ReadAll(req.Body)
					r.mu.Lock()
					r.bodies = append(r.bodies, string(b))
					r.sigs = append(r.sigs, req.Header.Get("X-Webhook-Signature"))
					r.mu.Unlock()
					w.WriteHeader(int(r.status.Load()))
					w.Write([]byte("test receiver"))
				}))
				t.Cleanup(r.srv.Close)
				body := fmt.Sprintf(`{"url":%q,"secret":"redeliver-pin-secret","events":["test.event"]}`, r.srv.URL)
				rec := httptest.NewRecorder()
				p.handleCreateWebhook(rec, httptest.NewRequest("POST", "/webhooks", strings.NewReader(body)).WithContext(tenantCtx))
				if rec.Code != http.StatusCreated {
					t.Fatalf("create webhook: want 201, got %d: %s", rec.Code, rec.Body.String())
				}
				var created map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
					t.Fatalf("decode create response: %v", err)
				}
				id := created["id"].(string)
				// Whatever a case leaves pending is cancelled with its webhook, so a later sweep
				// cannot send it.
				t.Cleanup(func() {
					req := httptest.NewRequest("DELETE", "/webhooks/"+id, nil).WithContext(tenantCtx)
					req.SetPathValue("id", id)
					p.handleDeleteWebhook(httptest.NewRecorder(), req)
				})
				return id, r
			}
			send := func(t *testing.T, webhookID string, n int) string {
				t.Helper()
				out, err := p.sendWebhook(sendCtx, fmt.Sprintf(`{"webhook_id":%q,"event_type":"test.event","payload":{"n":%d}}`, webhookID, n))
				if err != nil {
					t.Fatalf("sendWebhook: %v", err)
				}
				var sent struct {
					DeliveryID string `json:"delivery_id"`
				}
				if err := json.Unmarshal([]byte(out), &sent); err != nil || sent.DeliveryID == "" {
					t.Fatalf("sendWebhook returned %q (%v), want a delivery_id", out, err)
				}
				return sent.DeliveryID
			}
			sweep := func(t *testing.T) {
				t.Helper()
				if _, _, _, err := p.processDeliveries(sweepCtx, ctx); err != nil {
					t.Fatalf("processDeliveries: %v", err)
				}
			}
			read := func(t *testing.T, webhookID, deliveryID string) deliveryJSON {
				t.Helper()
				req := httptest.NewRequest("GET", "/webhooks/"+webhookID+"/deliveries", nil).WithContext(tenantCtx)
				req.SetPathValue("id", webhookID)
				rec := httptest.NewRecorder()
				p.handleListDeliveries(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("list deliveries: want 200, got %d: %s", rec.Code, rec.Body.String())
				}
				var all []deliveryJSON
				if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
					t.Fatalf("decode deliveries: %v", err)
				}
				for _, d := range all {
					if d.ID.String() == deliveryID {
						return d
					}
				}
				t.Fatalf("delivery %s is not in the list of %d", deliveryID, len(all))
				return deliveryJSON{}
			}
			redeliver := func(c context.Context, webhookID, deliveryID string) (int, string) {
				req := httptest.NewRequest("POST", "/webhooks/"+webhookID+"/deliveries/"+deliveryID+"/redeliver", nil).WithContext(c)
				req.SetPathValue("id", webhookID)
				req.SetPathValue("delivery_id", deliveryID)
				rec := httptest.NewRecorder()
				p.handleRedeliver(rec, req)
				return rec.Code, rec.Body.String()
			}
			// failedDelivery drives a delivery to failed through the real path: a receiver that
			// refuses it for good (410, in the permanent set of cleat#3611).
			failedDelivery := func(t *testing.T, webhookID string, r *receiver, n int) string {
				t.Helper()
				r.status.Store(410)
				id := send(t, webhookID, n)
				sweep(t)
				if d := read(t, webhookID, id); d.Status != "failed" {
					t.Fatalf("UNMEASURED: the delivery is %q, not failed, so a redelivery below would test nothing", d.Status)
				}
				return id
			}

			t.Run("the point: repaired, redelivered, and it arrives with the same body and signature", func(t *testing.T) {
				wh, r := newWebhook(t, 410)
				id := failedDelivery(t, wh, r, 1)
				if hits(r) != 1 {
					t.Fatalf("UNMEASURED: the receiver was called %d times before the redelivery, want 1", hits(r))
				}
				before := read(t, wh, id)

				r.status.Store(200) // the cause is repaired
				code, body := redeliver(operatorCtx, wh, id)
				if code != http.StatusOK {
					t.Fatalf("redeliver a failed delivery: want 200, got %d: %s", code, body)
				}
				var got deliveryJSON
				if err := json.Unmarshal([]byte(body), &got); err != nil {
					t.Fatalf("decode the redelivered row: %v", err)
				}
				if got.Status != "pending" || got.AttemptCount != 0 || got.NextAttemptAt == nil {
					t.Errorf("after redeliver: status=%q attempt_count=%d next_attempt_at=%v, want pending, 0 and a time", got.Status, got.AttemptCount, got.NextAttemptAt)
				}
				if got.RedeliveryCount != 1 || got.LastRedeliveredAt == nil || got.LastRedeliveredBy == nil || *got.LastRedeliveredBy != "operator@example.com" {
					t.Errorf("the resend is not recorded: count=%d at=%v by=%v, want 1, a time and the operator", got.RedeliveryCount, got.LastRedeliveredAt, got.LastRedeliveredBy)
				}
				if string(got.Payload) != string(before.Payload) || got.EventType != before.EventType {
					t.Errorf("the redelivered row changed the event: %s %q, was %s %q", got.Payload, got.EventType, before.Payload, before.EventType)
				}

				sweep(t)
				if hits(r) != 2 {
					t.Fatalf("the receiver was called %d times after redelivery and a sweep, want 2: the delivery did not go out", hits(r))
				}
				r.mu.Lock()
				sameBody, sameSig := r.bodies[0] == r.bodies[1], r.sigs[0] == r.sigs[1] && r.sigs[0] != ""
				r.mu.Unlock()
				if !sameBody || !sameSig {
					t.Errorf("the resend is not the same request: same body=%v, same non-empty signature=%v", sameBody, sameSig)
				}
				if d := read(t, wh, id); d.Status != "delivered" {
					t.Errorf("after the resend arrived the delivery is %q, want delivered", d.Status)
				}
				// A delivered delivery would be sent twice.
				if code, body := redeliver(operatorCtx, wh, id); code != http.StatusConflict || !strings.Contains(body, "delivered") {
					t.Errorf("redeliver a delivered delivery: got %d %s, want 409 naming delivered", code, body)
				}
				sweep(t)
				if hits(r) != 2 {
					t.Errorf("a refused redeliver still sent: the receiver was called %d times, want 2", hits(r))
				}
			})

			t.Run("a delivery that is not failed is refused, and left as it was", func(t *testing.T) {
				wh, r := newWebhook(t, 200)
				pending := send(t, wh, 2) // created, not swept: already going out
				if code, body := redeliver(operatorCtx, wh, pending); code != http.StatusConflict || !strings.Contains(body, "pending") {
					t.Errorf("redeliver a pending delivery: got %d %s, want 409 naming pending", code, body)
				}
				if d := read(t, wh, pending); d.Status != "pending" || d.RedeliveryCount != 0 {
					t.Errorf("a refused redeliver changed the delivery: status=%q redelivery_count=%d", d.Status, d.RedeliveryCount)
				}

				wh2, r2 := newWebhook(t, 503)
				retrying := send(t, wh2, 3)
				sweep(t)
				if d := read(t, wh2, retrying); d.Status != "retrying" {
					t.Fatalf("UNMEASURED: the delivery is %q, not retrying", d.Status)
				}
				if code, body := redeliver(operatorCtx, wh2, retrying); code != http.StatusConflict || !strings.Contains(body, "retrying") {
					t.Errorf("redeliver a retrying delivery: got %d %s, want 409 naming retrying", code, body)
				}
				if d := read(t, wh2, retrying); d.Status != "retrying" || d.RedeliveryCount != 0 || d.AttemptCount != 1 {
					t.Errorf("a refused redeliver changed the delivery: status=%q attempts=%d redeliveries=%d", d.Status, d.AttemptCount, d.RedeliveryCount)
				}
				_, _ = r, r2
			})

			t.Run("who and how often: no identity is recorded as none, and the count survives a second resend", func(t *testing.T) {
				wh, r := newWebhook(t, 410)
				id := failedDelivery(t, wh, r, 4)

				code, body := redeliver(tenantCtx, wh, id) // no subject in this context
				if code != http.StatusOK {
					t.Fatalf("redeliver: want 200, got %d: %s", code, body)
				}
				var first deliveryJSON
				if err := json.Unmarshal([]byte(body), &first); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if first.RedeliveryCount != 1 || first.LastRedeliveredAt == nil || first.LastRedeliveredBy != nil {
					t.Errorf("an unauthenticated-subject resend: count=%d at=%v by=%v, want 1, a time and no identity (nothing resolved one)",
						first.RedeliveryCount, first.LastRedeliveredAt, first.LastRedeliveredBy)
				}

				sweep(t) // the receiver still refuses it: failed again, on its first attempt after the reset
				d := read(t, wh, id)
				if d.Status != "failed" || d.AttemptCount != 1 {
					t.Fatalf("UNMEASURED: after the second refusal the delivery is %q with %d attempt(s), want failed and 1", d.Status, d.AttemptCount)
				}
				code, body = redeliver(operatorCtx, wh, id)
				if code != http.StatusOK {
					t.Fatalf("second redeliver: want 200, got %d: %s", code, body)
				}
				var second deliveryJSON
				if err := json.Unmarshal([]byte(body), &second); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if second.RedeliveryCount != 2 || second.LastRedeliveredBy == nil || *second.LastRedeliveredBy != "operator@example.com" {
					t.Errorf("after two resends: count=%d by=%v, want 2 and the most recent asker", second.RedeliveryCount, second.LastRedeliveredBy)
				}
			})

			t.Run("it reaches only this webhook's delivery, in this tenant", func(t *testing.T) {
				whA, rA := newWebhook(t, 410)
				whB, rB := newWebhook(t, 410)
				idA := failedDelivery(t, whA, rA, 5)
				idB := failedDelivery(t, whB, rB, 6)

				unchanged := func(t *testing.T, wh, id, why string) {
					t.Helper()
					if d := read(t, wh, id); d.Status != "failed" || d.RedeliveryCount != 0 {
						t.Errorf("%s: the delivery was changed (status=%q redelivery_count=%d)", why, d.Status, d.RedeliveryCount)
					}
				}
				if code, body := redeliver(operatorCtx, whA, uuid.NewString()); code != http.StatusNotFound {
					t.Errorf("an unknown delivery id: got %d %s, want 404", code, body)
				}
				// webhook B's delivery through webhook A's route: not A's delivery.
				if code, body := redeliver(operatorCtx, whA, idB); code != http.StatusNotFound {
					t.Errorf("another webhook's delivery: got %d %s, want 404", code, body)
				}
				unchanged(t, whB, idB, "another webhook's delivery was reachable")
				// the same webhook under another tenant: not that tenant's webhook.
				otherTenant := auth.WithSubject(auth.WithTenantID(context.Background(), uuid.New()), "someone@elsewhere.example")
				if code, body := redeliver(otherTenant, whA, idA); code != http.StatusNotFound {
					t.Errorf("another tenant's webhook: got %d %s, want 404", code, body)
				}
				unchanged(t, whA, idA, "another tenant could reset this tenant's delivery")
				if code, _ := redeliver(operatorCtx, "not-a-uuid", idA); code != http.StatusBadRequest {
					t.Errorf("an invalid webhook id: got %d, want 400", code)
				}
				if code, _ := redeliver(operatorCtx, whA, "not-a-uuid"); code != http.StatusBadRequest {
					t.Errorf("an invalid delivery id: got %d, want 400", code)
				}
				if code, body := redeliver(operatorCtx, whA, idA); code != http.StatusOK {
					t.Errorf("CONTROL: the right webhook and tenant: got %d %s, want 200, or the refusals above prove nothing", code, body)
				}
			})

			t.Run("a disabled webhook refuses, and a deleted one is not found", func(t *testing.T) {
				wh, r := newWebhook(t, 410)
				id := failedDelivery(t, wh, r, 7)
				setEnabled := func(enabled bool) {
					t.Helper()
					req := httptest.NewRequest("PUT", "/webhooks/"+wh, strings.NewReader(fmt.Sprintf(`{"enabled":%v}`, enabled))).WithContext(tenantCtx)
					req.SetPathValue("id", wh)
					rec := httptest.NewRecorder()
					p.handleUpdateWebhook(rec, req)
					if rec.Code != http.StatusOK {
						t.Fatalf("PUT enabled=%v: want 200, got %d: %s", enabled, rec.Code, rec.Body.String())
					}
				}
				setEnabled(false)
				if code, body := redeliver(operatorCtx, wh, id); code != http.StatusConflict || !strings.Contains(body, "disabled") {
					t.Errorf("redeliver behind a disabled webhook: got %d %s, want 409 naming disabled", code, body)
				}
				if d := read(t, wh, id); d.Status != "failed" || d.RedeliveryCount != 0 {
					t.Errorf("a refused redeliver changed the delivery: status=%q redelivery_count=%d", d.Status, d.RedeliveryCount)
				}
				setEnabled(true)
				if code, body := redeliver(operatorCtx, wh, id); code != http.StatusOK {
					t.Errorf("CONTROL: enabled again: got %d %s, want 200", code, body)
				}

				delReq := httptest.NewRequest("DELETE", "/webhooks/"+wh, nil).WithContext(tenantCtx)
				delReq.SetPathValue("id", wh)
				delRec := httptest.NewRecorder()
				p.handleDeleteWebhook(delRec, delReq)
				if delRec.Code != http.StatusNoContent && delRec.Code != http.StatusOK {
					t.Fatalf("UNMEASURED: delete webhook: got %d: %s", delRec.Code, delRec.Body.String())
				}
				if code, body := redeliver(operatorCtx, wh, id); code != http.StatusNotFound {
					t.Errorf("redeliver under a deleted webhook: got %d %s, want 404", code, body)
				}
			})

			// The normal caller: a tenant API key, through the real auth middleware and the plugin's
			// real route table, not a handler called with a hand-built context. It pins two things.
			// The route is reachable that way (path values and all), and the identity it records is
			// NONE: MiddlewareWithMux resolves only the tenant from a key and sets nothing else, so
			// no subject exists for the handler to read (cleat#3623). When that issue lands this
			// assertion is the one that changes, to the key's id, never to the key or its hash.
			t.Run("an API-key caller through the real auth middleware: it goes through and records no identity", func(t *testing.T) {
				wh, r := newWebhook(t, 410)
				id := failedDelivery(t, wh, r, 8)

				mux := http.NewServeMux()
				if err := p.RegisterRoutes(mux); err != nil {
					t.Fatalf("RegisterRoutes: %v", err)
				}
				const apiKey = "cleat_test_redeliver_key"
				sum := sha256.Sum256([]byte(apiKey))
				handler := auth.MiddlewareWithMux(apiKeyResolver{hash: sum[:], tenant: tenantID}, true, mux)(mux)
				do := func(bearer string) *httptest.ResponseRecorder {
					req := httptest.NewRequest("POST", "/webhooks/"+wh+"/deliveries/"+id+"/redeliver", nil)
					if bearer != "" {
						req.Header.Set("Authorization", "Bearer "+bearer)
					}
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, req)
					return rec
				}

				// Control first: no key, and a key nobody issued, reset nothing.
				if rec := do(""); rec.Code != http.StatusUnauthorized {
					t.Fatalf("no key: got %d %s, want 401", rec.Code, rec.Body.String())
				}
				if rec := do("not-the-key"); rec.Code != http.StatusUnauthorized {
					t.Fatalf("a key nobody issued: got %d %s, want 401", rec.Code, rec.Body.String())
				}
				if d := read(t, wh, id); d.Status != "failed" || d.RedeliveryCount != 0 {
					t.Fatalf("a refused request changed the delivery: status=%q redelivery_count=%d", d.Status, d.RedeliveryCount)
				}

				rec := do(apiKey)
				if rec.Code != http.StatusOK {
					t.Fatalf("redeliver with a valid API key: got %d %s, want 200", rec.Code, rec.Body.String())
				}
				var got deliveryJSON
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if got.Status != "pending" || got.RedeliveryCount != 1 || got.LastRedeliveredAt == nil {
					t.Errorf("the resend did not take: status=%q count=%d at=%v", got.Status, got.RedeliveryCount, got.LastRedeliveredAt)
				}
				if got.LastRedeliveredBy != nil {
					t.Errorf("last_redelivered_by = %q for an API-key caller; nothing resolves an identity for one today (cleat#3623), so this must be absent, and the key itself must never be recorded",
						*got.LastRedeliveredBy)
				}
			})
		})
	}
}
