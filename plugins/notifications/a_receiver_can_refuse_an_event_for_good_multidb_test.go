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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#3611: a webhook receiver can refuse an event for good, and only for the statuses that mean
// that. 400, 410, 413 and 422 make the delivery `failed` on the FIRST attempt; 401, 403, 404, 408,
// 425, 429 and every 5xx stay `retrying`, because nothing can resend a failed delivery yet
// (cleat#3612) and those are what an operator repairs from the outside.
//
// A real receiver, a real delivery created through the real host function, one real sweep, and the
// delivery read back through the real route, on all three dialects. Before this a 4xx was retried
// ten times over about 6h21m whatever it said, so the receiver had no way to say "never".
func TestAReceiverCanRefuseAnEventForGoodAndOnlyBySayingSo(t *testing.T) {
	cases := []struct {
		status    int
		permanent bool
	}{
		{400, true}, {410, true}, {413, true}, {422, true},
		{401, false}, {403, false}, {404, false}, {408, false}, {425, false}, {429, false},
		{500, false}, {502, false}, {503, false}, {504, false},
	}
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

			var status, hits atomic.Int32
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				io.Copy(io.Discard, r.Body)
				w.WriteHeader(int(status.Load()))
				w.Write([]byte("refused by the test receiver"))
			}))
			defer receiver.Close()

			createBody := fmt.Sprintf(`{"url":%q,"secret":"refusal-pin-secret","events":["test.event"]}`, receiver.URL)
			createRec := httptest.NewRecorder()
			p.handleCreateWebhook(createRec, httptest.NewRequest("POST", "/webhooks", strings.NewReader(createBody)).WithContext(tenantCtx))
			if createRec.Code != http.StatusCreated {
				t.Fatalf("create webhook: want 201, got %d: %s", createRec.Code, createRec.Body.String())
			}
			var created map[string]any
			if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode create response: %v", err)
			}
			webhookID := created["id"].(string)

			cc := &plugin.CallContext{TenantID: tenantID.String(), WorkflowID: "wf-refusal", DB: be.DB}
			sendCtx := plugin.WithCallContext(auth.WithTenantID(context.Background(), tenantID), cc)
			sweepCtx := plugin.AcrossAllTenants(ctx, "test: multidb refusal sweep")

			read := func(deliveryID string) deliveryJSON {
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

			for _, tc := range cases {
				tc := tc
				name := fmt.Sprintf("%d", tc.status)
				status.Store(int32(tc.status))
				hitsBefore := hits.Load()

				out, err := p.sendWebhook(sendCtx, fmt.Sprintf(`{"webhook_id":%q,"event_type":"test.event","payload":{"status":%d}}`, webhookID, tc.status))
				if err != nil {
					t.Fatalf("%s: sendWebhook: %v", name, err)
				}
				var sent struct {
					DeliveryID string `json:"delivery_id"`
				}
				if err := json.Unmarshal([]byte(out), &sent); err != nil || sent.DeliveryID == "" {
					t.Fatalf("%s: sendWebhook returned %q (%v), want a delivery_id", name, out, err)
				}

				if _, _, _, err := p.processDeliveries(sweepCtx, ctx); err != nil {
					t.Fatalf("%s: processDeliveries: %v", name, err)
				}
				if got := hits.Load() - hitsBefore; got != 1 {
					t.Fatalf("%s: the receiver was called %d times by one sweep, want 1", name, got)
				}
				d := read(sent.DeliveryID)
				wantReason := fmt.Sprintf("HTTP %d: refused by the test receiver", tc.status)
				if d.AttemptCount != 1 {
					t.Errorf("%s: attempt_count = %d, want 1", name, d.AttemptCount)
				}
				if d.ResponseBody == nil || *d.ResponseBody != wantReason {
					t.Errorf("%s: response_body = %v, want %q (the reason is the same either way)", name, d.ResponseBody, wantReason)
				}
				if tc.permanent {
					if d.Status != "failed" {
						t.Errorf("%s: status = %q, want failed on the first attempt: the receiver refused the event itself", name, d.Status)
					}
					// A failed delivery is never picked up again: a second sweep must not call the receiver.
					before := hits.Load()
					if _, _, _, err := p.processDeliveries(sweepCtx, ctx); err != nil {
						t.Fatalf("%s: second sweep: %v", name, err)
					}
					if again := hits.Load() - before; again != 0 {
						t.Errorf("%s: a failed delivery was attempted again (%d call(s))", name, again)
					}
				} else {
					if d.Status != "retrying" {
						t.Errorf("%s: status = %q, want retrying: this status is repairable from outside and nothing can resend a failed delivery", name, d.Status)
					}
					if d.NextAttemptAt == nil {
						t.Errorf("%s: a retrying delivery has no next_attempt_at", name)
					}
				}
			}
		})
	}
}
