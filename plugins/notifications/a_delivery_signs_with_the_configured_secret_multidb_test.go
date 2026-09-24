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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestSendWebhookDeliversWithTheConfiguredSecret is cleat#1992/#2172's
// end-to-end pin across all three real dialects: create a webhook through the
// real route, trigger a delivery through the real host function, sweep the
// real delivery loop, and check the signature a real HTTP receiver saw.
// cleat-review on #2198 found this exact path broken on two of the three
// dialects, and this test found a third and fourth on its own -- none caught
// by the two packages' in-memory fake-driver suites (which bind by ordinal
// and accept any SQL string that matches their own pattern, regardless of
// what a real database does with it):
//
//   - MySQL: POST /webhooks' INSERT reused $6 for both created_at and
//     updated_at. plugin.Rebind rebinds MySQL's `?` positionally, so two
//     placeholder OCCURRENCES need two arguments -- reusing one argument for
//     both left the statement expecting 7 values and receiving 6, and every
//     create failed outright ("expected 7 arguments, got 6").
//   - SQL Server: sendWebhook's ownership check used `SELECT EXISTS(SELECT 1
//     FROM ...)` as a top-level select expression, which is valid PostgreSQL
//     and MySQL but not T-SQL -- no delivery could ever be queued on MSSQL.
//   - MySQL: queryDueDeliveries compared a microsecond-precision column
//     against a bare `now()`, which MySQL truncates to whole seconds -- see
//     the NOW(6) comment on queryDueDeliveries in background.go.
//   - SQL Server: processDeliveries scanned the payload column straight into
//     deliveryRow.Payload (a json.RawMessage). go-mssqldb returns NVARCHAR as
//     a Go string, and database/sql has no fast-path conversion from a string
//     driver.Value into a named []byte type -- every due delivery's scan
//     failed, was logged, and was skipped, so attempted stayed 0 forever. See
//     plugin.JSONColumn's doc comment; processDeliveries now scans through it.
//
// THE SWEEP IS POLLED, NOT CALLED ONCE. A single processDeliveries call
// proved flaky in exactly the way this comment warns against elsewhere in
// this codebase: next_attempt_at is set from the TEST PROCESS's clock (Go's
// time.Now(), on the host), and the due-delivery WHERE clause compares
// against the DATABASE SERVER's own clock (SQL now()/NOW(6)/SYSUTCDATETIME(),
// inside a Docker container). The two are not the same clock, and a
// container's clock reading even a few milliseconds behind the host's would
// make a freshly-created delivery read as "not yet due" for a single
// synchronous sweep. Production never notices this: Run ticks every
// deliveryInterval (30s by default), which drowns out any millisecond-scale
// skew. A single-shot assertion here does not have that margin, so it polls
// instead -- the same shape webhook_config_rows_are_scoped_by_a_policy_test.go
// already uses for the same reason (its own comment: polled rather than
// slept, because a fixed sleep is still a wall-clock assertion).
func TestSendWebhookDeliversWithTheConfiguredSecret(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{
				dialect:    dialect,
				logger:     quiet,
				httpClient: &http.Client{Timeout: 5 * time.Second},
			}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}

			// A REAL SecretStore, sealed under a fixed test key -- same shape
			// as plugins/webhookingest's retired-secret and cross-tenant
			// multi-dialect tests, reproduced here for the same reason
			// (importing that package back would cycle).
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

			// A real HTTP receiver, so the signature checked below is the one
			// deliver() actually sent over the wire, not one recomputed
			// against internal state that could agree with a bug in the same
			// way it agrees with itself.
			var receivedBody []byte
			var receivedSig string
			receiverDone := make(chan struct{}, 1)
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedBody, _ = io.ReadAll(r.Body)
				receivedSig = r.Header.Get("X-Webhook-Signature")
				w.WriteHeader(http.StatusOK)
				receiverDone <- struct{}{}
			}))
			defer receiver.Close()

			const rawSecret = "delivery-pin-secret"
			createBody := fmt.Sprintf(`{"url":%q,"secret":%q,"events":["test.event"]}`, receiver.URL, rawSecret)
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
			webhookID := created["id"].(string)

			// Trigger the delivery through the real host function, exactly as
			// a running workflow would -- not seeded directly into
			// webhook_delivery, which would prove nothing about sendWebhook's
			// own ownership check (the SQL Server bug above).
			cc := &plugin.CallContext{
				TenantID:   tenantID.String(),
				WorkflowID: "wf-test",
				DB:         be.DB,
			}
			// auth.WithTenantID, not a bare context.Background(): in
			// production, engine.pluginCallContext bridges a workflow's
			// tenant into BOTH plugin.CallContext (which sendWebhook itself
			// reads) and the internal tenantctx SQLDBAdapter's tenantTx uses
			// to scope the connection for row-level security -- see
			// engine/plugindb_tenant.go's doc comment on beginTenantTx. A
			// bare context.Background() only supplies the first, which is
			// enough on PostgreSQL (be.DB here is a superuser connection,
			// exempt from RLS regardless) but not on SQL Server, which has no
			// such exemption: the row is genuinely invisible without the
			// session context set, indistinguishable from "not found".
			sendCtx := plugin.WithCallContext(auth.WithTenantID(context.Background(), tenantID), cc)
			const payload = `{"hello":"world"}`
			input := fmt.Sprintf(`{"webhook_id":%q,"event_type":"test.event","payload":%s}`, webhookID, payload)
			if _, err := p.sendWebhook(sendCtx, input); err != nil {
				t.Fatalf("sendWebhook: %v", err)
			}

			// sweepCtx carries the same AcrossAllTenants marking Run()
			// applies (background.go's own comment on why): deliver()'s
			// webhook_config lookup has no tenant predicate of its own --
			// the delivery row does not know its tenant until that lookup
			// runs -- and on PostgreSQL/SQL Server that means it needs the
			// cross-tenant bypass, not a plain unmarked context, to get past
			// row-level security at all. baseCtx (unmarked ctx) is still
			// what deliver() builds its per-tenant Secrets.ForTenant call
			// from, same as Run().
			sweepCtx := plugin.AcrossAllTenants(ctx, "test: multidb delivery sweep")

			// Polled, not called once -- see the clock-skew comment above the
			// test. Every tick is a REAL processDeliveries call through the
			// production code path; this is not a sleep-then-assert.
			deadline := time.Now().Add(10 * time.Second)
			var lastAttempted, lastSucceeded, lastFailed int
			delivered := false
			for time.Now().Before(deadline) {
				attempted, succeeded, failed, err := p.processDeliveries(sweepCtx, ctx)
				if err != nil {
					t.Fatalf("processDeliveries: %v", err)
				}
				lastAttempted, lastSucceeded, lastFailed = attempted, succeeded, failed
				if succeeded > 0 {
					delivered = true
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if !delivered {
				t.Fatalf("delivery was never swept as due within 10s "+
					"(last sweep: attempted=%d succeeded=%d failed=%d)",
					lastAttempted, lastSucceeded, lastFailed)
			}
			if lastFailed != 0 {
				t.Fatalf("delivery attempt failed instead of succeeding: attempted=%d succeeded=%d failed=%d",
					lastAttempted, lastSucceeded, lastFailed)
			}

			select {
			case <-receiverDone:
			case <-time.After(5 * time.Second):
				t.Fatal("receiver was never called")
			}

			// Compare decoded JSON, not raw bytes: Postgres stores payload as
			// jsonb and re-serialises it on the way back out (a space after
			// each colon), so a byte-for-byte comparison fails on a value
			// that is semantically identical to what was sent. MySQL and SQL
			// Server round-trip the text unchanged, which is why this only
			// showed up on one of three dialects.
			var gotPayload, wantPayload map[string]any
			if err := json.Unmarshal(receivedBody, &gotPayload); err != nil {
				t.Fatalf("receiver body is not valid JSON: %v (%q)", err, receivedBody)
			}
			if err := json.Unmarshal([]byte(payload), &wantPayload); err != nil {
				t.Fatalf("test payload is not valid JSON: %v", err)
			}
			if !reflect.DeepEqual(gotPayload, wantPayload) {
				t.Fatalf("receiver body: got %q, want (decoded) %v", receivedBody, wantPayload)
			}

			mac := hmac.New(sha256.New, []byte(rawSecret))
			mac.Write(receivedBody)
			wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
			if receivedSig != wantSig {
				t.Errorf("signature %q does not match the secret set via the admin route (want %q)",
					receivedSig, wantSig)
			}
		})
	}
}
