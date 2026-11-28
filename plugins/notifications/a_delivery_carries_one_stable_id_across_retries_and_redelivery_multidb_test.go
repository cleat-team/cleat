package notifications

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#3621: every request for one delivery carries the same X-Webhook-Delivery header, and the
// value is the delivery's own id.
//
// Before it, a request carried X-Webhook-Event and X-Webhook-Signature and nothing that named the
// delivery, so a receiver could not tell a retry, or a redelivery after cleat#3612, from a new
// event except by reading the payload. A receiver whose payload has no id of its own had nothing
// to dedupe on.
//
// The case is a real receiver, the real routes and real sweeps, on all three dialects: one
// delivery is refused with a 500 (retried), refused again, then accepted, and after it succeeded
// a second delivery is failed for good, redelivered and sent again. The header must be the same
// value on every one of those requests, must equal the id the list route returns, and must be
// different for a different delivery. The signature must still be the HMAC of the body alone: the
// header is not signed, and signing it would break every verifier.
func TestADeliveryCarriesOneStableIdAcrossRetriesAndRedelivery(t *testing.T) {
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
				&plugin.CallContext{TenantID: tenantID.String(), WorkflowID: "wf-delivery-id", DB: be.DB})
			sweepCtx := plugin.AcrossAllTenants(ctx, "test: multidb delivery id sweep")

			var mu sync.Mutex
			var status = http.StatusInternalServerError
			var ids, sigs, bodies []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				b, _ := io.ReadAll(req.Body)
				mu.Lock()
				ids = append(ids, req.Header.Get("X-Webhook-Delivery"))
				sigs = append(sigs, req.Header.Get("X-Webhook-Signature"))
				bodies = append(bodies, string(b))
				code := status
				mu.Unlock()
				w.WriteHeader(code)
			}))
			defer srv.Close()
			setStatus := func(s int) { mu.Lock(); status = s; mu.Unlock() }
			seen := func() (i, s, b []string) {
				mu.Lock()
				defer mu.Unlock()
				return append([]string(nil), ids...), append([]string(nil), sigs...), append([]string(nil), bodies...)
			}

			const secret = "delivery-id-pin-secret"
			createRec := httptest.NewRecorder()
			p.handleCreateWebhook(createRec, httptest.NewRequest("POST", "/webhooks",
				strings.NewReader(fmt.Sprintf(`{"url":%q,"secret":%q,"events":["test.event"]}`, srv.URL, secret))).WithContext(tenantCtx))
			if createRec.Code != http.StatusCreated {
				t.Fatalf("create webhook: want 201, got %d: %s", createRec.Code, createRec.Body.String())
			}
			var created map[string]any
			if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode create response: %v", err)
			}
			webhookID := created["id"].(string)
			defer func() {
				req := httptest.NewRequest("DELETE", "/webhooks/"+webhookID, nil).WithContext(tenantCtx)
				req.SetPathValue("id", webhookID)
				p.handleDeleteWebhook(httptest.NewRecorder(), req)
			}()

			send := func(n int) uuid.UUID {
				t.Helper()
				out, err := p.sendWebhook(sendCtx, fmt.Sprintf(`{"webhook_id":%q,"event_type":"test.event","payload":{"n":%d}}`, webhookID, n))
				if err != nil {
					t.Fatalf("sendWebhook: %v", err)
				}
				var sent struct {
					DeliveryID uuid.UUID `json:"delivery_id"`
				}
				if err := json.Unmarshal([]byte(out), &sent); err != nil || sent.DeliveryID == uuid.Nil {
					t.Fatalf("sendWebhook returned %q (%v), want a delivery_id", out, err)
				}
				return sent.DeliveryID
			}
			sweep := func() {
				t.Helper()
				if _, _, _, err := p.processDeliveries(sweepCtx, ctx); err != nil {
					t.Fatalf("processDeliveries: %v", err)
				}
			}
			listed := func(id uuid.UUID) deliveryJSON {
				t.Helper()
				req := httptest.NewRequest("GET", "/webhooks/"+webhookID+"/deliveries", nil).WithContext(tenantCtx)
				req.SetPathValue("id", webhookID)
				rec := httptest.NewRecorder()
				p.handleListDeliveries(rec, req)
				var all []deliveryJSON
				if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
					t.Fatalf("decode deliveries: %v", err)
				}
				for _, d := range all {
					if d.ID == id {
						return d
					}
				}
				t.Fatalf("delivery %s is not in the list of %d", id, len(all))
				return deliveryJSON{}
			}

			// Delivery A: refused (500) twice, so it is sent three times, then accepted.
			a := send(1)
			sweep()
			if d := listed(a); d.Status != "retrying" {
				t.Fatalf("UNMEASURED: delivery A is %q after a 500, not retrying, so the retry below would test nothing", d.Status)
			}
			setDue(t, p, sweepCtx, a, -5)
			sweep()
			setStatus(http.StatusOK)
			setDue(t, p, sweepCtx, a, -5)
			sweep()
			if d := listed(a); d.Status != "delivered" {
				t.Fatalf("UNMEASURED: delivery A is %q, not delivered, so the sends were not what this case counts", d.Status)
			}
			gotIDs, _, _ := seen()
			if len(gotIDs) != 3 {
				t.Fatalf("UNMEASURED: the receiver saw %d requests for delivery A, want 3 (two refused, one accepted)", len(gotIDs))
			}
			for i, id := range gotIDs {
				if id != a.String() {
					t.Errorf("attempt %d of delivery A carried X-Webhook-Delivery=%q, want the delivery's id %q", i+1, id, a)
				}
			}

			// Delivery B: failed for good (410), repaired, redelivered, sent again.
			setStatus(http.StatusGone)
			b := send(2)
			sweep()
			if d := listed(b); d.Status != "failed" {
				t.Fatalf("UNMEASURED: delivery B is %q after a 410, not failed, so the redelivery below would test nothing", d.Status)
			}
			setStatus(http.StatusOK)
			req := httptest.NewRequest("POST", "/webhooks/"+webhookID+"/deliveries/"+b.String()+"/redeliver", nil).WithContext(operatorCtx)
			req.SetPathValue("id", webhookID)
			req.SetPathValue("delivery_id", b.String())
			rec := httptest.NewRecorder()
			p.handleRedeliver(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("redeliver: want 200, got %d: %s", rec.Code, rec.Body.String())
			}
			sweep()
			allIDs, allSigs, allBodies := seen()
			if len(allIDs) != 5 {
				t.Fatalf("UNMEASURED: the receiver saw %d requests in all, want 5 (3 for A, 2 for B)", len(allIDs))
			}
			for i, id := range allIDs[3:] {
				if id != b.String() {
					t.Errorf("request %d of delivery B (first send, then the redelivery) carried X-Webhook-Delivery=%q, want %q", i+1, id, b)
				}
			}
			if a == b {
				t.Fatalf("UNMEASURED: the two deliveries share an id")
			}

			// The header is not signed: every signature is still the HMAC of that request's body
			// alone.
			for i := range allIDs {
				mac := hmac.New(sha256.New, []byte(secret))
				mac.Write([]byte(allBodies[i]))
				if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); allSigs[i] != want {
					t.Errorf("request %d: X-Webhook-Signature=%q, want the HMAC of the body alone %q", i+1, allSigs[i], want)
				}
			}
		})
	}
}
