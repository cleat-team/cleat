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
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestAnIngestRacingAnOpenDeleteTransactionOnPostgresIsBlocked pins
// cleat-review's second finding on #2221's own race fix: the EXISTS-guarded
// INSERT (routes.go) narrowed the window between handleDeleteSource's
// soft-delete and handleIngestWebhook's write, but did not close it on
// PostgreSQL. At READ COMMITTED (Postgres's default), a plain read sees only
// committed data, so an ingest whose EXISTS subquery ran while the delete's
// UPDATE was applied but not yet committed read the PRE-delete row -- 201,
// inserted, signalled inline -- and only afterward did the delete commit.
// Measured before this fix: exactly that, every time.
//
// FOR SHARE turns the subquery into a locking read: against a row an open
// UPDATE already holds, it BLOCKS until that transaction ends, then re-reads
// under READ COMMITTED's per-statement snapshot rule and sees the
// now-committed deleted_at. This test proves the BLOCK, not merely the
// eventual answer -- it holds the delete open, starts the ingest
// concurrently, waits long enough that an unblocked ingest would already
// have finished and signalled, and only then commits the delete. A version
// of this test that just ran both sequentially would pass whether or not
// the ingest actually waited for anything.
//
// PostgreSQL only: MySQL and SQL Server were never affected by this gap --
// both already block a plain read against a row an open UPDATE holds -- so
// there is no analogous race to close or test on either.
func TestAnIngestRacingAnOpenDeleteTransactionOnPostgresIsBlocked(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		if be.Dialect != testutil.DialectPostgres {
			continue
		}
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

			signalled := make(chan string, 1)
			p.env = &plugin.Environment{
				Dialect: dialect,
				Logger:  quiet,
				SignalWorkflow: func(_ context.Context, _, _, payload string) error {
					signalled <- payload
					return nil
				},
			}

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			tenantCtx := auth.WithTenantID(context.Background(), tenantID)

			const rawSecret = "pg-interleaving-secret"
			createBody := `{"name":"pg-interleaving","source_type":"github",` +
				`"secret":"` + rawSecret + `","signal_workflow_id":"wf-interleaving"}`
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

			// Open the delete's transaction directly and apply its UPDATE,
			// then hold it open -- be.DB is a superuser connection (RLS
			// does not apply to it), so no tenant session context is needed
			// for this raw write. The statement text is handleDeleteSource's
			// own, reproduced here rather than called through the route,
			// because the route commits before returning and this test
			// needs the transaction to stay open across the ingest.
			deleteTx, err := be.DB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin delete tx: %v", err)
			}
			if _, err := deleteTx.ExecContext(ctx, plugin.Rebind(`
				UPDATE webhook_sources
				SET enabled = false, deleted_at = $1
				WHERE id = $2 AND tenant_id = $3 AND deleted_at IS NULL
			`, dialect), time.Now(), sourceID, tenantID.String()); err != nil {
				t.Fatalf("apply soft-delete inside open tx: %v", err)
			}

			// Start the racing ingest concurrently.
			payload := []byte(`{"raced":"against-open-delete"}`)
			mac := hmac.New(sha256.New, []byte(rawSecret))
			mac.Write(payload)
			sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

			type ingestResult struct {
				code int
				body string
			}
			ingestDone := make(chan ingestResult, 1)
			go func() {
				ingestReq := httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader(payload))
				ingestReq.SetPathValue("source_id", sourceID)
				ingestReq.Header.Set("X-Hub-Signature-256", sig)
				ingestReq.Header.Set("X-Event-Type", "raced.event")
				rec := httptest.NewRecorder()
				p.handleIngestWebhook(rec, ingestReq)
				ingestDone <- ingestResult{code: rec.Code, body: rec.Body.String()}
			}()

			// Long enough that an UNBLOCKED ingest would already have
			// finished and signalled -- proving the block, not sampling a
			// lucky ordering.
			select {
			case res := <-ingestDone:
				deleteTx.Rollback()
				t.Fatalf("ingest returned BEFORE the delete committed (code=%d, body=%s) -- "+
					"FOR SHARE did not block it", res.code, res.body)
			case <-time.After(700 * time.Millisecond):
			}

			if err := deleteTx.Commit(); err != nil {
				t.Fatalf("commit delete tx: %v", err)
			}

			var res ingestResult
			select {
			case res = <-ingestDone:
			case <-time.After(5 * time.Second):
				t.Fatal("ingest did not return within 5s of the delete committing -- deadlock?")
			}

			if res.code != http.StatusNotFound {
				t.Errorf("ingest racing an open delete: want 404, got %d: %s", res.code, res.body)
			}

			select {
			case payload := <-signalled:
				t.Errorf("a signal was delivered for an event ingested against a source mid-delete: %s", payload)
			default:
			}

			readConn := be.CrossTenantConn(t, ctx,
				"cleat-review: confirming no event row survived the raced ingest")
			var eventCount int
			if err := readConn.QueryRowContext(ctx, plugin.Rebind(
				`SELECT count(*) FROM webhook_events WHERE source_id = $1`, dialect),
				sourceID).Scan(&eventCount); err != nil {
				t.Fatalf("count webhook_events for source: %v", err)
			}
			if eventCount != 0 {
				t.Errorf("webhook_events rows for the raced source: got %d, want 0", eventCount)
			}
		})
	}
}
