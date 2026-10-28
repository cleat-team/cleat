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
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// TestADisabledWebhookDoesNotDeliver is cleat#3539, across all three dialects.
// webhook_config.enabled was stored, listed and written back by PUT, and read by
// nothing that sends: PUT {"enabled": false} returned 200 and deliveries went on
// being queued and POSTed.
//
// THE RULES IT PINS, and why each is the way it is:
//
//   - send_webhook on a disabled row is REFUSED with its own message, not "not
//     found" (the row exists) and not accepted (that queues a backlog nobody sees
//     until re-enable, then fires it all).
//   - A delivery already queued is HELD: not POSTed, and not changed at all
//     (status, attempt_count and last_attempt_at as seeded), so that re-enable
//     resumes it. Cancel and dead-letter are terminal and could not be resumed.
//   - Held is decided in queryDueDeliveries' JOIN, not by skipping in deliver().
//     The due query is ORDER BY next_attempt_at LIMIT 100 across every webhook, so
//     held rows that stayed in its result would take places in it ahead of every
//     enabled webhook's delivery. The case below seeds more than 100 held rows
//     older than an enabled webhook's delivery and requires that delivery to be
//     attempted by the FIRST sweep; a skip at deliver() time fails it. (A claim's
//     60 s lease takes the skipped rows out of the next sweep, so the delay is a
//     sweep or two for 105 rows and does not end for 200 or more: the lease is two
//     sweep intervals. Asserting after several sweeps, as this test first did,
//     passes under the skip, which was measured.)
//   - GET .../deliveries still lists the held rows (webhookExistsSQL is shared with
//     it and must not learn about enabled).
//
// KNOWN-POSITIVE FIRST: the enabled control at the top delivers, so a silent
// receiver below means the delivery was withheld, not that the harness cannot
// deliver. On the code before this change, the disabled phase fails because the
// held rows are POSTed and send_webhook succeeds.
func TestADisabledWebhookDoesNotDeliver(t *testing.T) {
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
			sweepCtx := plugin.AcrossAllTenants(ctx, "cleat#3539 test: sweeping due deliveries across tenants")
			readConn := be.CrossTenantConn(t, ctx, "cleat#3539: webhook_delivery rows around a disable")

			// One receiver per webhook, so a count says which webhook was POSTed.
			newReceiver := func() (*httptest.Server, *atomic.Int64) {
				var n atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n.Add(1)
					w.WriteHeader(http.StatusOK)
				}))
				return srv, &n
			}
			disabledSrv, disabledHits := newReceiver()
			defer disabledSrv.Close()
			enabledSrv, enabledHits := newReceiver()
			defer enabledSrv.Close()

			createWebhook := func(url string) uuid.UUID {
				t.Helper()
				body := fmt.Sprintf(`{"url":%q,"secret":"cleat-3539-secret","events":["test.event"]}`, url)
				rec := httptest.NewRecorder()
				p.handleCreateWebhook(rec, httptest.NewRequest("POST", "/webhooks", strings.NewReader(body)).WithContext(tenantCtx))
				if rec.Code != http.StatusCreated {
					t.Fatalf("create webhook: want 201, got %d: %s", rec.Code, rec.Body.String())
				}
				var created map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
					t.Fatalf("decode create response: %v", err)
				}
				return uuid.MustParse(created["id"].(string))
			}
			setEnabled := func(id uuid.UUID, enabled bool) {
				t.Helper()
				req := httptest.NewRequest("PUT", "/webhooks/"+id.String(),
					strings.NewReader(fmt.Sprintf(`{"enabled":%v}`, enabled))).WithContext(tenantCtx)
				req.SetPathValue("id", id.String())
				rec := httptest.NewRecorder()
				p.handleUpdateWebhook(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("PUT enabled=%v: want 200, got %d: %s", enabled, rec.Code, rec.Body.String())
				}
			}
			send := func(id uuid.UUID, n int) error {
				cc := &plugin.CallContext{TenantID: tenantID.String(), WorkflowID: "wf-3539", DB: be.DB}
				sendCtx := plugin.WithCallContext(auth.WithTenantID(context.Background(), tenantID), cc)
				_, err := p.sendWebhook(sendCtx, fmt.Sprintf(`{"webhook_id":%q,"event_type":"test.event","payload":{"n":%d}}`, id, n))
				return err
			}
			sweep := func() {
				t.Helper()
				if _, _, _, err := p.processDeliveries(sweepCtx, ctx); err != nil {
					t.Fatalf("processDeliveries: %v", err)
				}
			}
			type rowState struct {
				status   string
				attempts int
				lastAt   any
			}
			read := func(id uuid.UUID) rowState {
				t.Helper()
				var s rowState
				if err := plugintest.QueryRowRebound(t, ctx, readConn, dialect,
					`SELECT status, attempt_count, last_attempt_at FROM webhook_delivery WHERE id = $1`, id).
					Scan(&s.status, &s.attempts, &s.lastAt); err != nil {
					t.Fatalf("read delivery %s: %v", id, err)
				}
				return s
			}
			countRows := func(webhook uuid.UUID) int {
				t.Helper()
				var n int
				if err := plugintest.QueryRowRebound(t, ctx, readConn, dialect,
					`SELECT count(*) FROM webhook_delivery WHERE webhook_id = $1`, webhook).Scan(&n); err != nil {
					t.Fatalf("count deliveries for %s: %v", webhook, err)
				}
				return n
			}

			disabledID := createWebhook(disabledSrv.URL)
			enabledID := createWebhook(enabledSrv.URL)
			defer func() {
				for _, id := range []uuid.UUID{disabledID, enabledID} {
					if _, err := p.db.Exec(sweepCtx, `DELETE FROM webhook_delivery WHERE webhook_id = $1`, id); err != nil {
						t.Logf("could not remove deliveries of %s: %v", id, err)
					}
				}
			}()

			// ---- CONTROL: an enabled webhook delivers through this harness ----
			if err := send(enabledID, 0); err != nil {
				t.Fatalf("CONTROL: send_webhook on an enabled webhook: %v", err)
			}
			sweep()
			if got := enabledHits.Load(); got != 1 {
				t.Fatalf("CONTROL FAILED: an enabled webhook received %d POST(s) after one send and one sweep, want 1; "+
					"the silence asserted below would mean nothing", got)
			}

			// ---- drain whatever else is due, so the sweeps below see only this test's rows ----
			// Other tests in this package leave due deliveries in a shared database. The
			// loop ends when a sweep attempts nothing; it runs BEFORE the held rows exist,
			// so it cannot spend their leases.
			for i := 0; i < 10; i++ {
				attempted, _, _, err := p.processDeliveries(sweepCtx, ctx)
				if err != nil {
					t.Fatalf("drain sweep: %v", err)
				}
				if attempted == 0 {
					break
				}
			}

			// ---- seed the queue, then disable ----
			// Direct INSERTs for deterministic statuses, as the delete test does:
			// one pending, one retrying (attempt_count 1), both long due. Then more
			// than the due query's LIMIT 100, all older than anything the enabled
			// webhook will queue, for the starvation case.
			past := time.Now().Add(-2 * time.Hour)
			seed := func(status string, attempts int, due time.Time) uuid.UUID {
				id := uuid.New()
				if _, err := p.db.Exec(tenantCtx, `
					INSERT INTO webhook_delivery (id, webhook_id, event_type, payload, status, attempt_count, next_attempt_at, created_at)
					VALUES ($1, $2, 'test.event', '{}', $3, $4, $5, $6)
				`, id, disabledID, status, attempts, due, due); err != nil {
					t.Fatalf("seed %s delivery: %v", status, err)
				}
				return id
			}
			pendingID := seed("pending", 0, past)
			retryingID := seed("retrying", 1, past)
			const crowd = 105
			crowdIDs := make([]uuid.UUID, 0, crowd)
			for i := 0; i < crowd; i++ {
				crowdIDs = append(crowdIDs, seed("pending", 0, past.Add(-time.Hour)))
			}
			held := append([]uuid.UUID{pendingID, retryingID}, crowdIDs...)
			rowsBefore := countRows(disabledID)

			before := map[uuid.UUID]rowState{}
			for _, id := range held {
				before[id] = read(id)
			}
			if before[pendingID].status != "pending" || before[retryingID].status != "retrying" || before[retryingID].attempts != 1 {
				t.Fatalf("PRECONDITION FAILED: seeded states %+v / %+v", before[pendingID], before[retryingID])
			}

			setEnabled(disabledID, false)

			// ---- deliver() holds a disabled webhook's row by itself ----
			// Called directly, before any sweep: the due query's join keeps held rows out of a
			// sweep, so a sweep never reaches deliver()'s own check, which exists for a disable
			// landing between a sweep's read and its lookup. Calling deliver() skips the sweep
			// and so the race, and leaves that check as the only thing deciding. It must come
			// BEFORE the sweeps below: a sweep's claim moves next_attempt_at, and the
			// "a hold must not touch the row" assertion would then compare a row this call
			// had already touched (cleat-review on cleat#3544).
			outcome, derr := p.deliver(sweepCtx, ctx, deliveryRow{
				ID: pendingID, WebhookID: disabledID, EventType: "test.event", Payload: json.RawMessage(`{}`),
			})
			if derr != nil || outcome != "held" {
				t.Errorf("deliver() on a disabled webhook's due row: got outcome %q err %v, want \"held\" and no error", outcome, derr)
			}
			if got := disabledHits.Load(); got != 0 {
				t.Errorf("deliver() POSTed %d time(s) to a disabled webhook, want 0", got)
			}
			if after := read(pendingID); after != before[pendingID] {
				t.Errorf("deliver() changed a held row: before %+v, after %+v", before[pendingID], after)
			}

			// ---- (a) send_webhook on a disabled row is refused, with its own message ----
			err = send(disabledID, 1)
			switch {
			case err == nil:
				t.Errorf("send_webhook on a disabled webhook: want an error, got nil")
			case !strings.Contains(err.Error(), "webhook disabled"):
				t.Errorf("send_webhook on a disabled webhook: got %q, want it to say \"webhook disabled\" "+
					"(not \"webhook not found\": the row exists)", err)
			}
			if got := countRows(disabledID); got != rowsBefore {
				t.Errorf("a refused send_webhook left %d delivery row(s), want the %d seeded", got, rowsBefore)
			}

			// ---- an enabled neighbour is attempted by the very next sweep ----
			// 107 held rows, every one due and older than this delivery. The due query
			// takes 100 rows by age: left in its result and skipped later, they would
			// fill it, and this delivery would wait for a later sweep.
			if err := send(enabledID, 1); err != nil {
				t.Fatalf("send_webhook on the enabled webhook: %v", err)
			}
			hitsBefore := enabledHits.Load()
			sweep()
			if got := enabledHits.Load(); got != hitsBefore+1 {
				t.Errorf("the enabled webhook received %d new POST(s) from one sweep while %d held rows were due, want 1: "+
					"held rows must be left out of the due query, not skipped after it (head-of-line blocking)",
					got-hitsBefore, len(held))
			}

			// ---- (b) held: nothing was POSTed, and nothing about the rows changed ----
			if got := disabledHits.Load(); got != 0 {
				t.Errorf("the disabled webhook's receiver got %d POST(s), want 0", got)
			}
			for _, id := range held {
				if after := read(id); after != before[id] {
					t.Errorf("held delivery %s changed: before %+v, after %+v; a hold must not touch the row", id, before[id], after)
					break
				}
			}

			// ---- the held rows are still listed ----
			listReq := httptest.NewRequest("GET", "/webhooks/"+disabledID.String()+"/deliveries", nil).WithContext(tenantCtx)
			listReq.SetPathValue("id", disabledID.String())
			listRec := httptest.NewRecorder()
			p.handleListDeliveries(listRec, listReq)
			if listRec.Code != http.StatusOK {
				t.Errorf("GET .../deliveries on a disabled webhook: want 200 (the held rows stay visible), got %d: %s",
					listRec.Code, listRec.Body.String())
			}

			// ---- re-enable: every held delivery goes out, once ----
			setEnabled(disabledID, true)
			for i := 0; i < 8 && disabledHits.Load() < int64(len(held)); i++ {
				sweep()
			}
			if got := disabledHits.Load(); got != int64(len(held)) {
				t.Errorf("after re-enable the receiver got %d POST(s), want exactly the %d held deliveries", got, len(held))
			}
			for _, id := range held {
				if s := read(id); s.status != "delivered" {
					t.Errorf("held delivery %s after re-enable: status %q, want delivered", id, s.status)
					break
				}
			}
		})
	}
}
