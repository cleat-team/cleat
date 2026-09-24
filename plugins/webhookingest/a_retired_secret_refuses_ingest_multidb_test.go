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

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestARetiredSecretRefusesIngest is cleat#1992's pin for webhookingest's part
// (a): a source that WAS signed, whose Secrets entry is then retired, must
// refuse ingest -- not silently fall back to unsigned. It runs on all three
// real backends, because secret_configured is a plain bool column and its
// scan (BIT on SQL Server, TINYINT on MySQL, BOOLEAN on PostgreSQL) is exactly
// the kind of thing that has broken per-dialect elsewhere in this repo
// (plugins/webhookingest/background.go's own comment on
// queryUnprocessedWebhookEvents is the same class of defect, on `NOT
// e.processed`).
//
// A REAL SecretStore, not plugintest.FakeSecrets: the marker (secret_configured)
// and the store are two different systems, and this test exists specifically
// to prove they stay in sync when the STORE side changes out from under the
// marker. A fake that does not model engine.ErrSecretNotFound's failure shape
// would not exercise the branch this test is for.
//
// WHY IT MUST GO RED AGAINST THE PRE-#1992 SHAPE. Before this migration, a
// source's own row carried its secret in a plain column, and
// handleIngestWebhook read `source.Secret.Reveal() != ""` to decide whether to
// require a signature -- a check against the ROW, which retiring a Secrets
// entry does not touch at all. Ported forward unchanged, that check would see
// secret_configured (still true) and never learn the store no longer holds
// anything, so ingest after retirement would need no signature and would
// succeed -- exactly the naive-check failure this test pins against. See the
// falsification note below for how that was verified rather than assumed.
func TestARetiredSecretRefusesIngest(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			// Core migrations first: tenant_secrets, which the real SecretStore
			// below needs, lives there rather than in this plugin's own
			// migrations.
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: quiet}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}

			// A REAL SecretStore, sealed under a fixed test key -- the same
			// shape tests/plugin-harness/harness.go wires for the harness,
			// reproduced here because that package imports this one (blank
			// import in wasm_plugin_test.go) and importing it back would cycle.
			key := make([]byte, 32)
			for i := range key {
				key[i] = 0x5a
			}
			ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: key})
			if err != nil {
				t.Fatalf("build key ring: %v", err)
			}
			secretStore := engine.NewSecretStoreWithRing(be.DB, string(dialect), ring)
			realSecrets := engine.NewPluginSecrets(secretStore)

			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.secrets = realSecrets
			p.env = &plugin.Environment{Dialect: dialect, Logger: quiet}

			// engine.DefaultTenantUUID, not a freshly generated one:
			// tenant_secrets carries a foreign key to admin.tenants, and
			// MySQL's schema is single-tenant by constraint -- the default
			// tenant is the one row every dialect's core migrations already
			// seed, so it is the only id that does not need its own INSERT
			// (and its own MySQL special case) here. This test is not about
			// cross-tenant isolation, which is covered elsewhere.
			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			tenantCtx := auth.WithTenantID(context.Background(), tenantID)

			// Create a signed source through the real route, so the secret
			// reaches the store the same way a caller's POST would -- not
			// seeded directly, which would prove nothing about handleCreateSource
			// itself.
			const rawSecret = "pin-secret-before-retirement"
			createBody := `{"name":"pinned-source","source_type":"github","secret":"` + rawSecret + `"}`
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
			if created["secret_configured"] != true {
				t.Fatalf("create response: secret_configured=%v, want true", created["secret_configured"])
			}
			sourceID := created["id"].(string)

			sign := func(secret string, payload []byte) string {
				mac := hmac.New(sha256.New, []byte(secret))
				mac.Write(payload)
				return "sha256=" + hex.EncodeToString(mac.Sum(nil))
			}

			// POSITIVE CONTROL: before retirement, a correctly-signed request
			// succeeds. Without this, a 401/503 after retirement could mean
			// the source was never reachable at all rather than that
			// retirement is what changed the outcome.
			payload := []byte(`{"before":"retirement"}`)
			preReq := httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader(payload))
			preReq.SetPathValue("source_id", sourceID)
			preReq.Header.Set("X-Hub-Signature-256", sign(rawSecret, payload))
			preRec := httptest.NewRecorder()
			p.handleIngestWebhook(preRec, preReq)
			if preRec.Code != http.StatusCreated {
				t.Fatalf("UNMEASURED: a correctly-signed request before retirement got %d, want 201: %s\n"+
					"  every assertion below would be comparing against a source that never worked.",
					preRec.Code, preRec.Body.String())
			}

			// Retire the secret directly against the store -- the source
			// row's secret_configured marker is untouched; only the store
			// entry goes away. This is exactly the scenario the coordinator
			// asked to be pinned: a signed source whose Secrets entry is
			// later retired.
			sourceUUID := uuid.MustParse(sourceID)
			if _, err := p.secrets.Retire(tenantCtx, WebhookIngestSecretName(sourceUUID)); err != nil {
				t.Fatalf("retire secret: %v", err)
			}

			// Ingest again, WITH a signature computed from the now-retired
			// secret. The marker (secret_configured) still reads true, so this
			// must be refused because the STORE lookup fails -- not accepted
			// because the row still says a secret was once configured.
			postReq := httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader(payload))
			postReq.SetPathValue("source_id", sourceID)
			postReq.Header.Set("X-Hub-Signature-256", sign(rawSecret, payload))
			postRec := httptest.NewRecorder()
			p.handleIngestWebhook(postRec, postReq)
			if postRec.Code == http.StatusCreated {
				t.Fatalf("a source whose Secrets entry was retired still accepted a webhook (got 201).\n"+
					"  secret_configured is true on the row, but the store no longer holds "+
					"anything under %q -- the handler must refuse rather than treat a lookup "+
					"failure as \"no secret configured\".", WebhookIngestSecretName(sourceUUID))
			}
			if postRec.Code != http.StatusServiceUnavailable {
				t.Errorf("retired-secret ingest: got %d, want 503: %s", postRec.Code, postRec.Body.String())
			}

			// And WITHOUT any signature at all -- the marker alone must be
			// enough to refuse, independent of what the caller sends.
			noSigReq := httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader(payload))
			noSigReq.SetPathValue("source_id", sourceID)
			noSigRec := httptest.NewRecorder()
			p.handleIngestWebhook(noSigRec, noSigReq)
			if noSigRec.Code == http.StatusCreated {
				t.Fatalf("a source whose Secrets entry was retired accepted an UNSIGNED webhook " +
					"(got 201) -- secret_configured being set must refuse regardless of what " +
					"signature header (if any) the request carries.")
			}
		})
	}
}

// TestTenantBCannotReadTenantAsWebhookIngestSecretThroughTheRoute is
// cleat#1992's cross-tenant known-positive for webhookingest, matching #2179
// (pagerdutyalert)'s TestTenantBCannotReadTenantAsRoutingKeyThroughTheRoute in
// shape -- and strengthened to run on all three real backends, since this
// plugin's admin routes are tenant-scoped SQL (`WHERE tenant_id = ...`) whose
// isolation is exactly the kind of thing a per-dialect placeholder or
// NULL-handling defect can silently break (see this file's sibling test's doc
// comment, and this segment's two real, pre-existing bugs found the same
// way).
//
// A REAL SecretStore and a REAL SECOND TENANT ROW, not FakeSecrets and not a
// made-up id: the isolation under test is tenant_secrets' own (tenant_id,
// name) scoping, in engine.SecretStore, which a fake cannot exercise, and
// Secrets.ForTenant("B").Get would fail with "no such tenant" rather than
// "wrong tenant" against an id with no row at all -- too weak a control to
// tell isolation from a broken id.
func TestTenantBCannotReadTenantAsWebhookIngestSecretThroughTheRoute(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
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
			realSecrets := engine.NewPluginSecrets(secretStore)

			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.secrets = realSecrets
			p.env = &plugin.Environment{Dialect: dialect, Logger: quiet}

			tenantA := uuid.MustParse(engine.DefaultTenantUUID)
			tenantACtx := auth.WithTenantID(context.Background(), tenantA)

			const realSecret = "tenant-a-ingest-secret"
			createBody := `{"name":"tenant-a-source","source_type":"github","secret":"` + realSecret + `"}`
			createReq := httptest.NewRequest("POST", "/ingest/sources",
				strings.NewReader(createBody)).WithContext(tenantACtx)
			createRec := httptest.NewRecorder()
			p.handleCreateSource(createRec, createReq)
			if createRec.Code != http.StatusCreated {
				t.Fatalf("create tenant A's source: want 201, got %d: %s", createRec.Code, createRec.Body.String())
			}
			var created map[string]any
			if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode create response: %v", err)
			}
			sourceID := created["id"].(string)

			// MySQL is single-tenant by constraint (engine/a_secret_rotation_test.go's
			// own comment on rotationEnv.tenants) -- it cannot hold a second tenant
			// row for admin.tenants' own sake, so there is no tenant B to seed here
			// and the cross-tenant assertions below do not apply to this dialect.
			// The subtest still runs everything above, which is the part that
			// exercises this dialect's own placeholder and scan behaviour.
			if be.Dialect == testutil.DialectMySQL {
				return
			}

			tenantB := uuid.New()
			insertTenant := map[testutil.Dialect]string{
				testutil.DialectPostgres: `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
				testutil.DialectMSSQL:    `INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`,
			}[be.Dialect]
			// admin.tenants has a UNIQUE constraint on name (tenants_name_key on
			// Postgres, uq_admin_tenants_name on MSSQL) separate from its
			// tenant_id primary key. tenantB.String() is fresh every run and
			// never collides on tenant_id -- but a fixed literal name would, on
			// a database that SuiteTestDB-style backends leave standing between
			// runs. Suffix the name with the same fresh uuid so both columns are
			// unique per run.
			tenantBName := "cleat-1992-tenant-b-" + tenantB.String()
			if _, err := be.DB.ExecContext(ctx, insertTenant, tenantB.String(), tenantBName); err != nil {
				t.Fatalf("seed tenant B: %v", err)
			}
			tenantBCtx := auth.WithTenantID(context.Background(), tenantB)

			// Tenant B's GET for tenant A's source: not found, and the real secret
			// never appears in the response.
			getReq := httptest.NewRequest("GET", "/ingest/sources/"+sourceID, nil).WithContext(tenantBCtx)
			getReq.SetPathValue("id", sourceID)
			getRec := httptest.NewRecorder()
			p.handleGetSource(getRec, getReq)
			if getRec.Code != http.StatusNotFound {
				t.Errorf("tenant B GET tenant A's source: want 404, got %d: %s", getRec.Code, getRec.Body.String())
			}
			if strings.Contains(getRec.Body.String(), realSecret) {
				t.Errorf("tenant B GET response leaked tenant A's real secret: %s", getRec.Body.String())
			}

			// Tenant B's LIST does not surface tenant A's source at all.
			listReq := httptest.NewRequest("GET", "/ingest/sources", nil).WithContext(tenantBCtx)
			listRec := httptest.NewRecorder()
			p.handleListSources(listRec, listReq)
			if listRec.Code != http.StatusOK {
				t.Fatalf("tenant B LIST: want 200, got %d: %s", listRec.Code, listRec.Body.String())
			}
			if strings.Contains(listRec.Body.String(), sourceID) || strings.Contains(listRec.Body.String(), realSecret) {
				t.Errorf("tenant B LIST response contained tenant A's source or secret: %s", listRec.Body.String())
			}

			// And through Secrets directly: tenant B's own scope cannot read tenant
			// A's secret under the name the route used, even knowing the source id.
			sourceUUID := uuid.MustParse(sourceID)
			if v, err := realSecrets.ForTenant(tenantB.String()).Get(ctx, WebhookIngestSecretName(sourceUUID)); err == nil {
				t.Errorf("tenant B's Secrets scope could read tenant A's ingest secret: %q", v)
			}
		})
	}
}
