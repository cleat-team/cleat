package webhookingest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/eventtriggers"
)

// POST /ingest/{source_id} is one of exactly two routes cmd/cleat-worker exempts
// from authentication, because the caller is the external system sending the
// webhook and holds no cleat credential. So its request context carries NO
// tenant -- and once webhook_sources and webhook_events carry policies whose
// predicate raises on an unset tenant, every statement in the handler fails.
// cleat#1538.
//
// WHAT MAKES THIS TEST ABLE TO FAIL, which is the whole difficulty with an RLS
// test. Four conditions have to hold together or it is green and empty:
//
//   - Connect as PostgresRLSTestRole. The owner and any superuser BYPASS row
//     level security, so against a superuser connection this handler works
//     whether the policy is right, wrong, or absent.
//   - Assert the policy is actually engaged, in this database, on this
//     connection, before asserting anything about the handler. A handler that
//     returns 201 is exactly what an UNSCOPED table produces too, so without
//     this arm the test cannot distinguish "the fix works" from "the migration
//     never installed a policy".
//   - Send the request with NO tenant in its context -- the thing the route
//     actually does. A test that helpfully sets one is testing the management
//     endpoints, not this one.
//   - Seed a SECOND tenant's source. A USING clause is a row-level predicate
//     and is never evaluated against zero rows, so a lookup in a table holding
//     only the row under test passes against a policy that does nothing.
//
// POSTGRESQL ONLY: row-level security is the mechanism, and
// plugin.applyTenantScoping emits nothing on MySQL, which has no row-level
// security. It DOES install a policy on SQL Server as of cleat#1552; this
// test still runs on PostgreSQL only because that is where its fixture is.
func TestAnAuthExemptRouteCannotAssumeATenant(t *testing.T) {
	su := testutil.SuiteTestDB(t, "webhookingest")
	t.Cleanup(func() { su.Close() })
	testutil.SetupFullSchema(t, su, testutil.DialectPostgres)

	ctx := context.Background()
	dialect := plugin.DialectPostgres
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	p := &Plugin{dialect: dialect, logger: quiet}

	// eventtriggers is migrated and initialised too, because the handler
	// publishes through it and its three tables are tenant-scoped in the same
	// batch. Init is what sets its package-level dialect; without it Rebind
	// produces the wrong placeholders and the failure looks like a syntax error
	// rather than a missing setup step.
	et := &eventtriggers.Plugin{}
	if err := et.Init(ctx, &plugin.Environment{Dialect: dialect, Logger: quiet}); err != nil {
		t.Fatalf("eventtriggers Init: %v", err)
	}

	// ONE VALUE FOR "THE SCHEMA". plugin.RunMigrations defaults to "public" and
	// does not consult search_path; deriving it a second way from
	// current_schema() is how an earlier version of this pattern passed locally
	// and failed in the Tier 2 gate, which connects as a user whose
	// current_schema() is `cleat`.
	const schema = "public"
	if err := plugin.RunMigrations(ctx, su, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}, {Plugin: et, Healthy: true}},
		plugin.WithSchema(schema)); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	rls := testutil.OpenPostgresRLSTestDB(t, su)
	t.Cleanup(func() { rls.Close() })
	grants := []string{`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + testutil.PostgresRLSTestRole}
	for _, tbl := range []string{
		"webhook_sources", "webhook_events",
		"ingested_events", "event_subscriptions", "event_awaiters",
	} {
		grants = append(grants, `GRANT SELECT, INSERT, UPDATE, DELETE ON `+
			schema+`.`+tbl+` TO `+testutil.PostgresRLSTestRole)
	}
	for _, g := range grants {
		if _, err := su.ExecContext(ctx, g); err != nil {
			t.Fatalf("granting the RLS role access: %v\n  %s", err, g)
		}
	}
	p.db = &engine.SQLDBAdapter{DB: rls, Dialect: dialect}
	p.env = &plugin.Environment{Dialect: dialect, Logger: quiet}

	// A REAL SecretStore: cleat#1992/#2172, owner decision (b), made a
	// signing secret mandatory, so this route's own signature check -- not
	// just its tenant scoping -- is on the path this test exercises. Sealed
	// under the OWNER connection (su), not rls: the RLS connection is the
	// thing under test for webhook_sources/webhook_events, and tenant_secrets
	// is not part of that -- routing it through su keeps this test's own
	// setup from depending on a second policy it is not about.
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x5a
	}
	ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: key})
	if err != nil {
		t.Fatalf("build key ring: %v", err)
	}
	secretStore := engine.NewSecretStoreWithRing(su, string(dialect), ring)
	p.secrets = engine.NewPluginSecrets(secretStore)

	// THE TWO CONNECTIONS MUST BE ON THE SAME DATABASE. If this is ever
	// violated every seed below succeeds, the rows land somewhere real, and
	// every read finds nothing -- which reads as a broken statement rather than
	// as two databases.
	var suDB, rlsDB string
	if err := su.QueryRowContext(ctx, `SELECT current_database()`).Scan(&suDB); err != nil {
		t.Fatalf("current_database on the superuser connection: %v", err)
	}
	if err := rls.QueryRowContext(ctx, `SELECT current_database()`).Scan(&rlsDB); err != nil {
		t.Fatalf("current_database on the RLS connection: %v", err)
	}
	if suDB != rlsDB {
		t.Fatalf("UNMEASURED: the seeding connection is on %q and the reading connection "+
			"on %q, so no assertion below compares a write against a read of it.", suDB, rlsDB)
	}

	// START FROM EMPTY TABLES. SuiteTestDB hands back a database that PERSISTS
	// between runs, so a second invocation would otherwise see the first one's
	// rows and the counts below would climb by one per run.
	for _, tbl := range []string{"webhook_events", "webhook_sources", "ingested_events"} {
		if _, err := su.ExecContext(ctx, `DELETE FROM `+schema+`.`+tbl); err != nil {
			t.Fatalf("clearing %s before seeding: %v", tbl, err)
		}
	}

	mine := uuid.MustParse("11112222-0000-4000-8000-0000000015aa")
	theirs := uuid.MustParse("33334444-0000-4000-8000-0000000015bb")
	sourceID := uuid.MustParse("55556666-0000-4000-8000-0000000015cc")
	otherSourceID := uuid.MustParse("77778888-0000-4000-8000-0000000015dd")

	// The other tenant's source is seeded FIRST, and it is the reason the
	// lookup's predicate is evaluated at all.
	//
	// signal_workflow_id is written explicitly as the empty string here. The
	// column is nullable with no default, and handleCreateSource DOES leave
	// it NULL for a source created with no signal_workflow_id -- routes.go's
	// three SELECTs now COALESCE it to '' before scanning (cleat#1992 found
	// this: an uncoalesced NULL there fails with "converting NULL to string
	// is unsupported", the same 500 this test exists to catch, from an
	// unrelated cause). Seeding '' rather than NULL here just avoids
	// depending on that COALESCE to reach the assertions below.
	//
	// secret_configured = true for both: cleat#1992/#2172, owner decision
	// (b), made a signing secret mandatory, so a real row can no longer read
	// false here. Only "mine"'s secret is actually seeded into the store
	// below -- "theirs" never needs to be readable, since nothing in this
	// test ingests against it.
	// tenant_secrets carries a foreign key to admin.tenants (cleat#1992's own
	// finding, this same PR): neither "mine" nor "theirs" is the seeded
	// default tenant, so each needs its own row here before Put below can
	// succeed. ON CONFLICT DO NOTHING because mine/theirs are FIXED uuids
	// (unlike sourceID etc. above) and SuiteTestDB's database PERSISTS
	// between runs -- a second run of this test hits a duplicate key on a
	// plain INSERT.
	if _, err := su.ExecContext(ctx,
		`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2), ($3, $4)
		 ON CONFLICT (tenant_id) DO NOTHING`,
		mine, "cleat-1992-mine", theirs, "cleat-1992-theirs"); err != nil {
		t.Fatalf("seed admin.tenants for mine and theirs: %v", err)
	}

	const mineSecret = "mine-tenant-secret"
	if _, err := su.ExecContext(ctx,
		`INSERT INTO `+schema+`.webhook_sources
		     (id, tenant_id, name, source_type, secret_configured, enabled, signal_workflow_id, signal_name)
		 VALUES ($1, $2, 'theirs', 'generic', true, true, '', 'webhook_received'),
		        ($3, $4, 'mine',   'generic', true, true, '', 'webhook_received')`,
		otherSourceID, theirs, sourceID, mine); err != nil {
		t.Fatalf("seeding both tenants' sources: %v", err)
	}
	if err := p.secrets.ForTenant(mine.String()).Put(ctx, WebhookIngestSecretName(sourceID), mineSecret); err != nil {
		t.Fatalf("seed mine's ingest secret: %v", err)
	}

	// THE POSITIVE CONTROL, and it runs before anything about the handler.
	//
	// Everything below asserts that a tenantless request SUCCEEDS. That is also
	// what an unscoped table produces, so this arm establishes that the policy
	// is installed and fail-closed on this connection: a tenantless read must
	// RAISE, not return rows and not return zero rows.
	var n int
	err = rls.QueryRowContext(ctx, `SELECT count(*) FROM `+schema+`.webhook_sources`).Scan(&n)
	if err == nil {
		t.Fatalf("UNMEASURED: a read of webhook_sources with no tenant set returned %d rows "+
			"instead of raising. The policy is absent, or this connection bypasses it, and "+
			"every assertion below would pass against an unscoped table.", n)
	}
	if !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("UNMEASURED: the tenantless read failed, but not for the reason this test "+
			"needs it to fail. Expected the policy's tenant assertion, got: %v", err)
	}

	// The request as the route actually receives it: no tenant in the context,
	// because cmd/cleat-worker exempts this path from auth. Signed with
	// mine's secret -- required unconditionally since cleat#2172 (b).
	const ingestPayload = `{"hello":"world"}`
	mac := hmac.New(sha256.New, []byte(mineSecret))
	mac.Write([]byte(ingestPayload))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, "/ingest/"+sourceID.String(),
		strings.NewReader(ingestPayload))
	req.SetPathValue("source_id", sourceID.String())
	req.Header.Set("X-Event-Type", "push")
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()

	p.handleIngestWebhook(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("an inbound webhook on an auth-exempt route answered %d, want 201.\n"+
			"  body: %s\n\n"+
			"The handler's first statement reads webhook_sources, which carries a policy "+
			"whose predicate raises when no tenant is set -- and this route has none by "+
			"design. The lookup has to name itself cross-tenant (the source id is HOW the "+
			"tenant is learned) and everything after it has to run under "+
			"plugin.ForTenant(r.Context(), source.TenantID).",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	// The event landed, under the source's tenant and not the other one.
	var gotTenant uuid.UUID
	var gotType string
	if err := su.QueryRowContext(ctx,
		`SELECT tenant_id, event_type FROM `+schema+`.webhook_events WHERE source_id = $1`,
		sourceID).Scan(&gotTenant, &gotType); err != nil {
		t.Fatalf("reading back the stored webhook event: %v", err)
	}
	if gotTenant != mine {
		t.Errorf("the stored event carries tenant %s, want %s -- the tenant must come from "+
			"the webhook_sources row, not from the request.", gotTenant, mine)
	}
	if gotType != "push" {
		t.Errorf("stored event_type %q, want \"push\"", gotType)
	}

	// The publish reached eventtriggers' own tenant-scoped table. This is the
	// second break on this path and the one a peer found from the kafkaconnect
	// side: PublishEvent takes the tenant as an ARGUMENT while executing on the
	// caller's context.
	var ingested int
	if err := su.QueryRowContext(ctx,
		`SELECT count(*) FROM `+schema+`.ingested_events WHERE tenant_id = $1`, mine).Scan(&ingested); err != nil {
		t.Fatalf("counting ingested_events: %v", err)
	}
	if ingested != 1 {
		t.Errorf("ingested_events holds %d rows for this tenant, want 1 -- PublishEvent ran "+
			"on a context with no tenant, so its insert was refused by the same policy.", ingested)
	}

	// The other tenant's source was never touched.
	var strays int
	if err := su.QueryRowContext(ctx,
		`SELECT count(*) FROM `+schema+`.webhook_events WHERE tenant_id = $1`, theirs).Scan(&strays); err != nil {
		t.Fatalf("counting the other tenant's events: %v", err)
	}
	if strays != 0 {
		t.Errorf("the other tenant gained %d webhook_events rows from a request that named "+
			"neither it nor its source", strays)
	}
}
