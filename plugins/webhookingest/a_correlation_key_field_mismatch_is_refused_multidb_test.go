package webhookingest

import (
	"context"
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
	"github.com/cleat-team/cleat/plugins/eventtriggers"
)

// TestACorrelationKeyFieldMismatchIsRefused is cleat-review's finding on
// #2697: Keys (AwaitWebhookInput) and a source's own correlation_key_field
// must agree on whether key2 exists at all, or the mismatch is silent in
// BOTH directions -- key slots are strict equality and "" is a sentinel, not
// a wildcard (keys.go), so a mismatched await never matches a delivered
// event and never errors either; it just reads as "not found yet", forever.
//
// Direction 1 is exactly the bug cleat#2697's own order-lifecycle scenario
// hit: scripts/run-order-lifecycle-scenario.sh created its webhook source
// with no correlation_key_field while order.go's awaitPaymentConfirmation
// always passes Keys: []string{orderID}. Direction 2 is the one nobody had
// hit yet when cleat-review found it by reasoning about the two call sites
// together: setting correlation_key_field on an EXISTING source silently
// breaks every key-less await_webhook call already using it.
func TestACorrelationKeyFieldMismatchIsRefused(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: quiet}

			// eventtriggers migrated and Init'd too -- see
			// an_ingested_event_is_listed_and_awaited_multidb_test.go's
			// identical pairing and its comment on why Init (not just
			// Migrations) is required.
			et := &eventtriggers.Plugin{}
			if err := et.Init(ctx, &plugin.Environment{Dialect: dialect, Logger: quiet}); err != nil {
				t.Fatalf("eventtriggers Init: %v", err)
			}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}, {Plugin: et, Healthy: true}}); err != nil {
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
			secretStore := engine.NewSecretStoreWithRing(be.DB, string(dialect), ring)
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.secrets = engine.NewPluginSecrets(secretStore)
			p.env = &plugin.Environment{Dialect: dialect, Logger: quiet}

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			tenantCtx := auth.WithTenantID(context.Background(), tenantID)

			createSource := func(t *testing.T, name, correlationKeyField string) string {
				t.Helper()
				body := `{"name":"` + name + `","source_type":"payment","secret":"s3cret-` + name + `"`
				if correlationKeyField != "" {
					body += `,"correlation_key_field":"` + correlationKeyField + `"`
				}
				body += `}`
				req := httptest.NewRequest("POST", "/ingest/sources",
					strings.NewReader(body)).WithContext(tenantCtx)
				rec := httptest.NewRecorder()
				p.handleCreateSource(rec, req)
				if rec.Code != http.StatusCreated {
					t.Fatalf("create source %q: want 201, got %d: %s", name, rec.Code, rec.Body.String())
				}
				var created struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
					t.Fatalf("decode create response: %v", err)
				}
				return created.ID
			}

			awaitCtx := plugin.WithCallContext(tenantCtx, &plugin.CallContext{
				TenantID: tenantID.String(), WorkflowID: "wf-2697-guard", DB: be.DB,
			})

			t.Run("Keys passed against a source with no correlation_key_field", func(t *testing.T) {
				sourceID := createSource(t, "no-field", "")
				_, err := p.awaitWebhook(awaitCtx, AwaitWebhookInput{
					SourceID: sourceID,
					Keys:     []string{"ol-1"},
				})
				if err == nil {
					t.Fatal("await_webhook: expected an error, got nil")
				}
				const want = "no correlation_key_field configured"
				if !strings.Contains(err.Error(), want) {
					t.Errorf("await_webhook error: got %q, want it to contain %q", err.Error(), want)
				}
			})

			t.Run("no Keys against a source with correlation_key_field set", func(t *testing.T) {
				sourceID := createSource(t, "with-field", "order_id")
				_, err := p.awaitWebhook(awaitCtx, AwaitWebhookInput{
					SourceID: sourceID,
				})
				if err == nil {
					t.Fatal("await_webhook: expected an error, got nil")
				}
				const want = "no Keys were passed"
				if !strings.Contains(err.Error(), want) {
					t.Errorf("await_webhook error: got %q, want it to contain %q", err.Error(), want)
				}
			})

			// Controls: the two AGREEING shapes must not be refused -- both
			// read as an ordinary "not found yet" (no event was ever
			// delivered), never an error, or this guard would be refusing
			// the very calls order.go and TestAnIngestedEventIsListedAndAwaited
			// already rely on.
			t.Run("control: Keys and correlation_key_field both set", func(t *testing.T) {
				sourceID := createSource(t, "agree-both-set", "order_id")
				out, err := p.awaitWebhook(awaitCtx, AwaitWebhookInput{
					SourceID: sourceID,
					Keys:     []string{"ol-agree"},
				})
				if err != nil {
					t.Fatalf("await_webhook: %v", err)
				}
				if out.Found {
					t.Errorf("await_webhook: found=true, want false (no event was ever delivered)")
				}
			})

			t.Run("control: Keys and correlation_key_field both unset", func(t *testing.T) {
				sourceID := createSource(t, "agree-both-unset", "")
				out, err := p.awaitWebhook(awaitCtx, AwaitWebhookInput{
					SourceID: sourceID,
				})
				if err != nil {
					t.Fatalf("await_webhook: %v", err)
				}
				if out.Found {
					t.Errorf("await_webhook: found=true, want false (no event was ever delivered)")
				}
			})
		})
	}
}
