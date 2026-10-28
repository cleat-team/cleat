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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#3526. Every worker runs this plugin's delivery loop, and the sweep read
// due rows with a plain SELECT and sent the POST before any status change, so two
// workers whose sweeps overlapped could both read one row as due and both POST
// it. send_webhook is registered Idempotent:false.
//
// THE OVERLAP IS MADE DETERMINISTIC rather than hoped for: the receiver holds the
// first POST open, a second sweep then runs to completion while it is held, and
// only then is the first released. A second sweep that is allowed to send will
// have sent by the time it returns.
//
// KNOWN-POSITIVE FIRST. Against the loop as it was before the claim, this test
// sees two POSTs on every dialect; that is the measurement the fix is judged
// against, and it is why the assertion below is on the count of requests the
// receiver saw rather than on the sweep's own return values.
func TestTwoOverlappingSweepsSendADeliveryOnce(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)

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

			// The receiver: hold the first POST, answer any other at once.
			var mu sync.Mutex
			posts := 0
			firstSeen := make(chan struct{})
			release := make(chan struct{})
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				posts++
				n := posts
				mu.Unlock()
				if n == 1 {
					close(firstSeen)
					<-release
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer receiver.Close()
			// Never leave the first handler parked if the test fails early.
			var releaseOnce sync.Once
			doRelease := func() { releaseOnce.Do(func() { close(release) }) }
			defer doRelease()

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			tenantCtx := auth.WithTenantID(context.Background(), tenantID)
			createBody := fmt.Sprintf(`{"url":%q,"secret":"claim-test-secret","events":["test.event"]}`, receiver.URL)
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

			sendCtx := plugin.WithCallContext(tenantCtx, &plugin.CallContext{TenantID: tenantID.String(), WorkflowID: "wf-claim", DB: be.DB})
			input := fmt.Sprintf(`{"webhook_id":%q,"event_type":"test.event","payload":{"n":1}}`, webhookID)
			if _, err := p.sendWebhook(sendCtx, input); err != nil {
				t.Fatalf("sendWebhook: %v", err)
			}

			sweepCtx := plugin.AcrossAllTenants(ctx, "test: overlapping delivery sweeps")
			defer func() {
				if _, err := p.db.Exec(sweepCtx, `DELETE FROM webhook_delivery WHERE webhook_id = $1`, webhookID); err != nil {
					t.Logf("could not remove this test's deliveries: %v", err)
				}
			}()

			// Sweep A takes the row and parks inside the receiver.
			type sweep struct {
				attempted, succeeded, failed int
				err                          error
			}
			aDone := make(chan sweep, 1)
			go func() {
				a, s, f, err := p.processDeliveries(sweepCtx, ctx)
				aDone <- sweep{a, s, f, err}
			}()
			select {
			case <-firstSeen:
			case <-time.After(15 * time.Second):
				t.Fatal("UNMEASURED: sweep A never reached the receiver, so there is no overlap to test")
			}

			// Sweep B runs to completion while A's POST is still in flight.
			bAttempted, _, _, bErr := p.processDeliveries(sweepCtx, ctx)
			if bErr != nil {
				t.Fatalf("sweep B: %v", bErr)
			}

			doRelease()
			select {
			case a := <-aDone:
				if a.err != nil {
					t.Fatalf("sweep A: %v", a.err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("sweep A did not finish after the receiver released it")
			}

			mu.Lock()
			got := posts
			mu.Unlock()
			if got != 1 {
				t.Errorf("the receiver saw %d POSTs of one delivery, want 1: a second sweep that overlapped the first "+
					"sent it again (sweep B attempted %d)", got, bAttempted)
			}

			// And it was recorded once, as delivered.
			var status string
			var attempts int
			rows, err := p.db.Query(sweepCtx, `SELECT status, attempt_count FROM webhook_delivery WHERE webhook_id = $1`, webhookID)
			if err != nil {
				t.Fatalf("read the delivery back: %v", err)
			}
			if !rows.Next() {
				rows.Close()
				t.Fatal("read the delivery back: no row")
			}
			if err := rows.Scan(&status, &attempts); err != nil {
				rows.Close()
				t.Fatalf("read the delivery back: %v", err)
			}
			rows.Close()
			if status != "delivered" || attempts != 1 {
				t.Errorf("delivery recorded as status=%q attempt_count=%d, want delivered after 1 attempt", status, attempts)
			}
		})
	}
}
