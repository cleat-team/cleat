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

// cleat#3525. send_webhook only records a row; the POST was made on the delivery
// loop's next tick, so a receiver saw it 0 to 30 seconds late. send_webhook now
// wakes the loop of the same plugin instance.
//
// THE INTERVAL IS LEFT AT ITS 30s DEFAULT, which is the point of the test: Run
// builds its ticker at start, the delivery is recorded immediately after, and
// the first tick is therefore ~30s away. A POST that arrives within a few
// seconds can only have been woken, not ticked. Shortening the interval here
// would make a tick explain the result and the test could not tell the two apart.
//
// KNOWN-POSITIVE FIRST. With the nudge call removed from sendWebhook the POST
// does not arrive inside the window on any dialect; that is the measurement the
// nudge is judged against.
func TestASentWebhookIsAttemptedAtOnceNotAtTheNextTick(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)

			if deliveryInterval < 20*time.Second {
				t.Fatalf("UNMEASURED: deliveryInterval is %v; the test needs the production default so a tick cannot explain an early POST", deliveryInterval)
			}

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: quiet, httpClient: &http.Client{Timeout: 20 * time.Second}}
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

			got := make(chan struct{}, 4)
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- struct{}{}
				w.WriteHeader(http.StatusOK)
			}))
			defer receiver.Close()

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			tenantCtx := auth.WithTenantID(context.Background(), tenantID)
			createBody := fmt.Sprintf(`{"url":%q,"secret":"nudge-test-secret","events":["test.event"]}`, receiver.URL)
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

			sweepCtx := plugin.AcrossAllTenants(ctx, "test: nudged delivery loop")
			defer func() {
				if _, err := p.db.Exec(sweepCtx, `DELETE FROM webhook_delivery WHERE webhook_id = $1`, webhookID); err != nil {
					t.Logf("could not remove this test's deliveries: %v", err)
				}
			}()

			// The loop first, so its ticker starts now and its first tick is ~30s out.
			runCtx, stop := context.WithCancel(ctx)
			runDone := make(chan error, 1)
			go func() { runDone <- p.Run(runCtx) }()
			defer func() {
				stop()
				select {
				case <-runDone:
				case <-time.After(30 * time.Second):
					t.Errorf("Run did not return after its context was cancelled")
				}
			}()

			sendCtx := plugin.WithCallContext(tenantCtx, &plugin.CallContext{TenantID: tenantID.String(), WorkflowID: "wf-nudge", DB: be.DB})
			input := fmt.Sprintf(`{"webhook_id":%q,"event_type":"test.event","payload":{"n":1}}`, webhookID)
			sent := time.Now()
			if _, err := p.sendWebhook(sendCtx, input); err != nil {
				t.Fatalf("sendWebhook: %v", err)
			}

			select {
			case <-got:
				t.Logf("first POST arrived %v after send_webhook returned (tick interval %v)", time.Since(sent).Round(time.Millisecond), deliveryInterval)
			case <-time.After(10 * time.Second):
				t.Fatalf("the receiver saw no POST within 10s of send_webhook; the delivery waits for the %v tick", deliveryInterval)
			}

			var status string
			var attempts int
			deadline := time.Now().Add(10 * time.Second)
			for {
				if err := p.db.QueryRow(sweepCtx, `SELECT status, attempt_count FROM webhook_delivery WHERE webhook_id = $1`, webhookID).Scan(&status, &attempts); err != nil {
					t.Fatalf("read delivery back: %v", err)
				}
				if status == "delivered" || time.Now().After(deadline) {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if status != "delivered" || attempts != 1 {
				t.Fatalf("want delivered after 1 attempt, got %q after %d", status, attempts)
			}
		})
	}
}

// A nudge must never block send_webhook or fail it, whether or not a loop is
// running to take it, and a burst of them while one is already pending must
// collapse rather than queue.
func TestNudgingTheDeliveryLoopNeverBlocks(t *testing.T) {
	p := &Plugin{} // no Init, no Run: the zero value must be usable
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			p.nudgeDeliveryLoop()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("nudgeDeliveryLoop blocked with no loop running")
	}
	if n := len(p.nudgeCh()); n != 1 {
		t.Fatalf("1000 nudges with no consumer should leave exactly 1 pending, got %d", n)
	}
}
