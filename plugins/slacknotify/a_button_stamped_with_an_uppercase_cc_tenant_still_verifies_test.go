package slacknotify

import (
	"context"
	"database/sql"
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

// TestSendMessageCanonicalizesAnUppercaseCallContextTenant is cleat-review's
// gap on cleat#2230, found reviewing 920168e8: every test up to that point
// stamped through stampBlocksWithRoutes directly with an already-canonical
// (lowercase) tenant string, so nothing actually exercised the canonicalizing
// line host_functions.go's sendMessage adds ahead of it --
//
//	tenantID := cc.TenantID
//	if parsed, parseErr := uuid.Parse(tenantID); parseErr == nil {
//		tenantID = parsed.String()
//	}
//
// -- and cleat-review confirmed by deletion that removing it leaves every
// existing test green. It is not dead: MSSQL's claim path is where a real
// engine.CallContext.TenantID can arrive UPPERCASE (measured on real MSSQL,
// cleat-review + coordinator), and a route stamped under that raw case would
// carry a tenant field that never matches what resolveSlackTenant resolves
// at click time (always canonical lowercase, per its own canonicalization
// added in the same commit) -- 404 on every click.
//
// This drives the real host function, sendMessage, with a CallContext whose
// TenantID is deliberately uppercased -- not stampBlocksWithRoutes directly,
// which would just repeat the gap -- against a real httptest webhook, then
// clicks the button exactly as it was actually sent (mirroring
// TestSignedRouteResolvesAndVerifiesOnEveryDialect's click-fidelity
// discipline) through the full HTTP handler. A single real backend is
// sufficient: the property under test is sendMessage's OWN canonicalization
// of whatever case its caller handed it, which is dialect-independent Go
// string handling, not a dialect-specific CAST -- that half is already
// covered by TestResolveSlackTenant's dialect-cast subtest and this file's
// sibling.
func TestSendMessageCanonicalizesAnUppercaseCallContextTenant(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			// Core migrations: admin.tenants, slack_workspace.
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			// This plugin's OWN migrations: slack_config, which sendMessage
			// reads and stampBlocksWithRoutes-only tests never needed.
			p := &Plugin{dialect: dialect, logger: quiet}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("plugin migrations: %v", err)
			}
			// RunMigrations creates slack_config (and, on MSSQL, its
			// tenant_isolation security policy) directly in be.DB, and
			// be.Cleanup only closes the pool -- it never runs this plugin's
			// Down SQL. Left behind, the next `go test ./...` against this
			// same shared test database fails
			// engine.TestEveryTenantOwnedTableIsEmptiedByDropTenant on
			// public.slack_config, exactly the shape cleat-review found in
			// #2262's vestigial RunMigrations call and #2239's
			// eventtriggers acceptance test. Registered after RunMigrations
			// but before be.Cleanup so it always runs first (defers unwind
			// LIFO), whether this subtest reaches t.Skip, t.Fatalf, or the
			// end.
			defer cleanupSlackConfigSchema(t, be.DB, be.Dialect)

			// MySQL is single-tenant by constraint (see the sibling
			// real-dialect test's own comment), which only ever leaves
			// engine.DefaultTenantUUID available -- all zeros, so its
			// uppercase form is identical to canonical and this test's
			// whole premise (an UPPERCASE cc.TenantID that differs from
			// canonical) cannot be constructed on this dialect. Skipped
			// rather than run vacuously or hard-failed on a "fixture bug"
			// that is really just "not applicable here": MySQL was never
			// the dialect this case-mismatch lived on in the first place.
			if be.Dialect == testutil.DialectMySQL {
				t.Skip("MySQL is single-tenant by constraint (engine.DefaultTenantUUID, all zeros) -- an uppercase tenant id cannot be constructed here, and this dialect was never where the case bug lived")
			}

			// A tenant id with real hex letters, so its uppercase form
			// actually differs from canonical.
			tenantID := uuid.New()
			insertTenant := map[testutil.Dialect]string{
				testutil.DialectPostgres: `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
				testutil.DialectMSSQL:    `INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`,
			}[be.Dialect]
			name := "cleat-2230-uppercase-cc-" + tenantID.String()
			if _, err := be.DB.ExecContext(ctx, insertTenant, tenantID.String(), name); err != nil {
				t.Fatalf("seed tenant: %v", err)
			}

			// slack_workspace, keyed canonical lowercase -- the same form
			// resolveSlackTenant's own canonicalization now always produces,
			// regardless of what case CAST(tenant_id AS CHAR(36)) rendered.
			teamID := "T" + strings.ToUpper(strings.ReplaceAll(uuid.New().String(), "-", ""))[:20]
			if _, err := plugintest.ExecRebound(t, ctx, be.DB, dialect,
				`INSERT INTO slack_workspace (team_id, tenant_id) VALUES ($1, $2)`,
				teamID, tenantID.String()); err != nil {
				t.Fatalf("seed slack_workspace: %v", err)
			}

			// slack_config: the row sendMessage's own query reads. The
			// webhook points at an httptest server so the click-fidelity
			// discipline below can read back exactly what was sent, the same
			// way TestSN_SendMessage_WithChannelOverride captures it.
			var capturedPayload map[string]any
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				json.Unmarshal(b, &capturedPayload)
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"ok":true,"ts":"ts1"}`))
			}))
			defer ts.Close()

			// slack_config is TenantScoped RLS (migrations.go, v2) -- on SQL
			// Server that is a security policy with a BLOCK predicate keyed
			// on SESSION_CONTEXT('tenant_id'), enforced on every writer and
			// reader regardless of login, unlike postgres/mysql where this
			// test's connection is the table owner and bypasses RLS by
			// default. A bare *sql.DB write fails "has a block predicate
			// that conflicts with this operation" (33504), and pinning one
			// physical connection plus a single manual
			// sp_set_session_context is NOT enough to fix it either:
			// database/sql calls ResetSession on essentially every
			// pool-checkout cycle, go-mssqldb answers with
			// sp_reset_connection, and THAT clears SESSION_CONTEXT before the
			// next statement runs (engine/mssql_store.go's tenantSessionConn
			// doc comment; measured there).
			//
			// The correct fixture is the one production already has:
			// engine.SQLDBAdapter opens a tenant-scoped transaction and sets
			// SESSION_CONTEXT inside it (engine/plugindb_tenant.go,
			// beginTenantTx) whenever ctx carries a tenant via
			// internal/tenantctx -- the exact mechanism
			// engine.pluginCallContext uses to bridge a workflow's tenant
			// into a real plugin invocation. A transaction never returns its
			// connection to the pool mid-statement, so no reset can strike
			// between setting the context and using it.
			cfgID := uuid.New()
			insertConfig := `INSERT INTO slack_config (tenant_id, id, name, webhook_url, enabled) VALUES ($1, $2, $3, $4, ` + trueLiteral(dialect) + `)`
			dbAdapter := &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			if _, err := dbAdapter.Exec(tenantctx.With(ctx, tenantID), insertConfig, tenantID.String(), cfgID.String(), "uppercase-cc-test", ts.URL); err != nil {
				t.Fatalf("seed slack_config: %v", err)
			}

			pl := &Plugin{
				db:         dbAdapter,
				dialect:    dialect,
				logger:     quiet,
				httpClient: &http.Client{Timeout: 5 * time.Second},
				deploymentSecrets: &fakeInteractiveDeploymentSecrets{
					secret:   testSigningSecret,
					routeKey: testRouteSigningKey,
				},
			}

			// The load-bearing input: cc.TenantID handed to sendMessage in
			// UPPERCASE -- exactly the shape MSSQL's claim path can produce,
			// never the canonical form any other test in this package uses.
			uppercaseCC := strings.ToUpper(tenantID.String())
			if uppercaseCC == tenantID.String() {
				t.Fatalf("[dialect=%s] test fixture bug: tenant %q has no letters, uppercasing changed nothing -- this run proves nothing", be.Name, tenantID.String())
			}
			cc := &plugin.CallContext{TenantID: uppercaseCC, WorkflowID: "wf-uppercase-cc"}
			// engine.pluginCallContext bridges the workflow's own (always
			// canonical -- it is a uuid.UUID, not a string) tenant into
			// internal/tenantctx before a real plugin invocation ever
			// reaches sendMessage; without that bridge here too, sendMessage's
			// own reads through pl.db (an engine.SQLDBAdapter) run unscoped on
			// SQL Server and the RLS filter predicate silently returns no
			// slack_config row. tenantID, not uppercaseCC: this is the
			// engine's side of the boundary, which never sees the
			// case-corrupted string cc.TenantID carries.
			sendCtx := plugin.WithCallContext(tenantctx.With(ctx, tenantID), cc)

			input := map[string]any{
				"config_id": cfgID.String(),
				"text":      "uppercase cc tenant test",
				"blocks": []map[string]any{
					{"type": "actions", "elements": []map[string]any{
						{"type": "button", "action_id": "wf:wf-uppercase-cc:sig:approve", "value": "yes"},
					}},
				},
			}
			inputJSON, _ := json.Marshal(input)

			if _, err := pl.sendMessage(sendCtx, string(inputJSON)); err != nil {
				t.Fatalf("[dialect=%s] sendMessage: %v", be.Name, err)
			}

			if capturedPayload == nil {
				t.Fatalf("[dialect=%s] webhook never received a payload", be.Name)
			}
			blocks, _ := capturedPayload["blocks"].([]any)
			if len(blocks) == 0 {
				t.Fatalf("[dialect=%s] no blocks in the sent payload", be.Name)
			}
			block := blocks[0].(map[string]any)
			blockID, _ := block["block_id"].(string)
			elements, _ := block["elements"].([]any)
			if blockID == "" || len(elements) == 0 {
				t.Fatalf("[dialect=%s] expected an injected block_id and one element, got block_id=%q elements=%d", be.Name, blockID, len(elements))
			}
			elem := elements[0].(map[string]any)
			signedActionID, _ := elem["action_id"].(string)
			value, _ := elem["value"].(string)

			// The click: echo back EXACTLY what sendMessage actually sent,
			// through the real HTTP handler -- resolving the tenant fresh
			// from slack_workspace, never from the uppercase cc used above.
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
			pl.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
				gotWF, gotSig = workflowID, signalName
				gotTenant, gotTenantOK = tenantctx.From(ctx)
				return nil
			}

			mux := http.NewServeMux()
			if err := pl.RegisterRoutes(mux); err != nil {
				t.Fatalf("RegisterRoutes: %v", err)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, signedInteractiveRequest(body))
			if rec.Code != http.StatusOK {
				t.Fatalf("[dialect=%s] click on a button stamped with an UPPERCASE cc.TenantID refused: want 200, got %d: %s -- this is exactly the MSSQL-claim-path 404 if it recurs", be.Name, rec.Code, rec.Body.String())
			}
			if gotWF != "wf-uppercase-cc" || gotSig != "approve" {
				t.Errorf("[dialect=%s] delivered workflow/signal = %q/%q, want wf-uppercase-cc/approve", be.Name, gotWF, gotSig)
			}
			if !gotTenantOK || gotTenant != tenantID {
				t.Errorf("[dialect=%s] delivered tenant = %v (ok=%v), want %v (canonical lowercase, not the uppercase form sendMessage was given)", be.Name, gotTenant, gotTenantOK, tenantID)
			}
		})
	}
}

// trueLiteral returns this dialect's boolean-true literal for a raw INSERT --
// MSSQL has no `true` keyword, only BIT 1/0.
func trueLiteral(d plugin.Dialect) string {
	if d == plugin.Dialect(testutil.DialectMSSQL) {
		return "1"
	}
	return "true"
}

// cleanupSlackConfigSchema undoes slacknotify's Migrations() against this
// test's shared, persistent test database: RunMigrations has no matching
// teardown call anywhere in this file, and plugin.RunDownMigrations would not
// help here even if called -- v2 (TenantScoped, cleat#1512) applies its MSSQL
// security policy through plugin.RunMigrations' own runtime side effect
// (applyTenantScoping), not through any Up/Down SQL the migration declares,
// so a Down pass never drops the policy and DROP TABLE would fail on SQL
// Server while it still references slack_config. Same shape, same fix, as
// cmd/cleat-worker/a_purged_awaiter_unregisters_across_dialects_test.go's
// cleanupEventTriggersSchema (cleat#2239): drop the policy by the
// "<table>_tenant_isolation" name plugin/migration.go's
// applyTenantScopingMSSQL constructs, then the table, then the
// plugin_migrations row so a re-run of this test applies the migration fresh
// rather than finding it already recorded against a table that is gone.
func cleanupSlackConfigSchema(t *testing.T, conn *sql.DB, dialect testutil.Dialect) {
	t.Helper()
	ctx := context.Background()
	const pluginName = "slack-notify"

	exec := func(query string) {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			t.Errorf("cleanupSlackConfigSchema: %s: %v", query, err)
		}
	}
	exists := func(query string, args ...any) bool {
		var n int
		if err := conn.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Errorf("cleanupSlackConfigSchema: existence check %s: %v", query, err)
			return false
		}
		return n > 0
	}

	if dialect == testutil.DialectMSSQL {
		const policy = "slack_config_tenant_isolation"
		if exists(`SELECT COUNT(*) FROM sys.security_policies WHERE name = @p1`, policy) {
			exec(`DROP SECURITY POLICY dbo.` + policy)
		}
		if exists(`SELECT COUNT(*) FROM sys.tables WHERE name = @p1`, "slack_config") {
			exec(`DROP TABLE slack_config`)
		}
		exec(`DELETE FROM plugin_migrations WHERE plugin_name = '` + pluginName + `'`)
		return
	}

	exec(`DROP TABLE IF EXISTS slack_config`)
	exec(`DELETE FROM plugin_migrations WHERE plugin_name = '` + pluginName + `'`)
	if dialect == testutil.DialectPostgres {
		exec(`DELETE FROM admin.plugin_tables WHERE plugin_name = '` + pluginName + `'`)
	}
}
