package slacknotify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// TestSignedRouteResolvesAndVerifiesOnEveryDialect is coordinator's explicit
// ask after cleat-review's real-dialect pass on cleat#2230: unit tests with
// fakes could not have caught either of the two real bugs found there --
//
//  1. block_id-in-the-MAC breaking any button whose block has no explicit
//     block_id, because SLACK, not this code, decides what gets echoed back
//     on click -- a fake db can't exercise "what did SQL actually return",
//     but a real dialect's own CAST/collation behavior CAN, and did.
//  2. the tenant-id case mismatch, which is entirely about what
//     CAST(tenant_id AS CHAR(36)) returns on a REAL SQL Server, upper where
//     postgres/mysql return whatever was written.
//
// This runs resolveSlackTenant, the full HTTP handler
// (handleInteractiveCallback), and stampBlocksWithRoutes against a REAL
// database on every configured dialect -- exactly the shape review's own
// probe used: a real slack_workspace row, a real admin.tenants row with
// actual hex letters in its id (not engine.DefaultTenantUUID, which is all
// zeros and could never expose a case bug), and a Blocks JSON payload whose
// block carries no author-set block_id, exercising the injection fix.
//
// "Slack echoing the click back" is simulated the only way that is
// faithful to Slack's OWN behavior: read back exactly what
// stampBlocksWithRoutes produced (action_id, block_id, value) and send
// THAT in the click, unchanged -- never a value this test invents. Slack
// auto-generates a block_id only when the field is absent from what it was
// sent; since stamping leaves it always present, there is nothing left for
// Slack to invent, and a probe that invented its own block_id here would
// not be testing the fix.
func TestSignedRouteResolvesAndVerifiesOnEveryDialect(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			// Core migrations only: slack_workspace and admin.tenants live
			// in migrations/*/10{3,4}_a_slack_workspace_maps_to_one_tenant.sql
			// and the earlier core schema, not in this plugin's own
			// Migrations() (which only ever created slack_config).
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			// A tenant id with real hex letters -- uuid.New(), not
			// engine.DefaultTenantUUID (all zeros, upper==lower trivially,
			// so it could never expose the MSSQL uppercase-CAST bug).
			// MySQL is single-tenant by constraint (see
			// plugins/webhookingest/a_retired_secret_refuses_ingest_multidb_test.go's
			// own comment on admin.tenants there) -- and MySQL was never
			// the dialect with the case bug anyway, so it gets the one
			// tenant every dialect's core migrations already seed.
			var tenantID uuid.UUID
			if be.Dialect == testutil.DialectMySQL {
				tenantID = uuid.MustParse(engine.DefaultTenantUUID)
			} else {
				tenantID = uuid.New()
				insertTenant := map[testutil.Dialect]string{
					testutil.DialectPostgres: `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
					testutil.DialectMSSQL:    `INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`,
				}[be.Dialect]
				name := "cleat-2230-realdialect-" + tenantID.String()
				if _, err := be.DB.ExecContext(ctx, insertTenant, tenantID.String(), name); err != nil {
					t.Fatalf("seed tenant: %v", err)
				}
			}

			// Map a Slack team to that tenant -- the same row cleatctl's
			// slack-workspace mapping command writes. team_id must satisfy
			// ck_slack_workspace_team_id_shape (^[TE][A-Z0-9]+$) on every
			// dialect, so it cannot be a test placeholder like "T-realdialect"
			// -- and it must be FRESH per run: team_id is slack_workspace's
			// PRIMARY KEY, and SuiteTestDB-style backends persist data
			// between invocations, so a fixed literal collided on a second
			// run against the same container (measured directly: "duplicate
			// key value violates unique constraint slack_workspace_pkey").
			// Written CANONICAL lowercase, the same form sendMessage's own
			// canonicalization now produces -- exercising the real
			// production path rather than a value hand-picked to dodge the
			// bug this is here to catch.
			teamID := "T" + strings.ToUpper(strings.ReplaceAll(uuid.New().String(), "-", ""))[:20]
			if _, err := plugintest.ExecRebound(t, ctx, be.DB, dialect,
				`INSERT INTO slack_workspace (team_id, tenant_id) VALUES ($1, $2)`,
				teamID, tenantID.String()); err != nil {
				t.Fatalf("seed slack_workspace: %v", err)
			}

			p := &Plugin{
				db:      &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect},
				dialect: dialect,
				logger:  quiet,
				deploymentSecrets: &fakeInteractiveDeploymentSecrets{
					secret:   testSigningSecret,
					routeKey: testRouteSigningKey,
				},
			}

			// resolveSlackTenant against the REAL row just inserted --
			// this is what actually exercises dialect-specific CAST/
			// collation behavior; the HTTP round trip below exercises the
			// same call through the full handler.
			var payload slackInteractivePayload
			if err := json.Unmarshal([]byte(fmt.Sprintf(`{"team":{"id":%q}}`, teamID)), &payload); err != nil {
				t.Fatalf("test fixture: %v", err)
			}
			resolvedTenant, ok := p.resolveSlackTenant(ctx, payload)
			if !ok {
				t.Fatalf("[dialect=%s] resolveSlackTenant refused a real, freshly-seeded mapping", be.Name)
			}
			if resolvedTenant != tenantID.String() {
				t.Errorf("[dialect=%s] resolveSlackTenant = %q, want %q (canonical lowercase) -- this is exactly the MSSQL CAST-uppercases-it bug if it recurs", be.Name, resolvedTenant, tenantID.String())
			}

			// STAMP side: exactly what sendMessage does, including this
			// PR's canonicalization -- and a block with NO block_id, so
			// the injection fix is exercised, not sidestepped.
			routeKey, _, err := p.routeSigningKey(ctx)
			if err != nil {
				t.Fatalf("routeSigningKey: %v", err)
			}
			raw := json.RawMessage(`{"blocks":[{"type":"actions","elements":[
				{"type":"button","action_id":"wf:wf-realdialect:sig:approve","value":"yes"}
			]}]}`)
			stamped, err := stampBlocksWithRoutes(raw, routeKey, tenantID.String(), time.Now())
			if err != nil {
				t.Fatalf("stampBlocksWithRoutes: %v", err)
			}

			var tree map[string]any
			if err := json.Unmarshal(stamped, &tree); err != nil {
				t.Fatalf("stamped output invalid JSON: %v", err)
			}
			block := tree["blocks"].([]any)[0].(map[string]any)
			blockID, _ := block["block_id"].(string)
			if blockID == "" {
				t.Fatalf("[dialect=%s] expected an injected block_id, block still has none", be.Name)
			}
			elem := block["elements"].([]any)[0].(map[string]any)
			signedActionID := elem["action_id"].(string)
			value := elem["value"].(string)

			// The click: echo back EXACTLY what was just stamped, the
			// only faithful simulation of Slack's own behavior (see the
			// doc comment above).
			actionsJSON, err := json.Marshal([]map[string]any{{
				"action_id": signedActionID,
				"block_id":  blockID,
				"value":     value,
			}})
			if err != nil {
				t.Fatal(err)
			}
			rawBody := fmt.Sprintf(`{"type":"block_actions","team":{"id":%q},"actions":%s}`, teamID, actionsJSON)
			body := "payload=" + url.QueryEscape(rawBody)

			var gotWF, gotSig string
			var gotTenant uuid.UUID
			var gotTenantOK bool
			p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
				gotWF, gotSig = workflowID, signalName
				gotTenant, gotTenantOK = tenantctx.From(ctx)
				return nil
			}

			mux := http.NewServeMux()
			if err := p.RegisterRoutes(mux); err != nil {
				t.Fatalf("RegisterRoutes: %v", err)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, signedInteractiveRequest(body))
			if rec.Code != http.StatusOK {
				t.Fatalf("[dialect=%s] resolve+verify+deliver refused a real, freshly-seeded, correctly-echoed click: want 200, got %d: %s", be.Name, rec.Code, rec.Body.String())
			}
			if gotWF != "wf-realdialect" || gotSig != "approve" {
				t.Errorf("[dialect=%s] delivered workflow/signal = %q/%q, want wf-realdialect/approve", be.Name, gotWF, gotSig)
			}
			if !gotTenantOK || gotTenant != tenantID {
				t.Errorf("[dialect=%s] delivered tenant = %v (ok=%v), want %v", be.Name, gotTenant, gotTenantOK, tenantID)
			}
		})
	}
}
