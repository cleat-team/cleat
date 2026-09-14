package eventtriggers

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// PublishEvent takes the tenant as an ARGUMENT and must scope its own
// statements to it, because two of its three callers reach it with no tenant in
// context: kafkaconnect from a background poll loop, and webhookingest from an
// auth-exempt route. cleat#1538.
//
// WHY THIS TEST EXISTS SEPARATELY FROM THE WEBHOOK ONE, which is the whole
// point and was found by trying to break it rather than by design. The
// webhookingest regression test passes a context that already carries the
// tenant -- so removing the ForTenant inside PublishEvent leaves that test
// GREEN. It covers the handler's scoping and cannot see this function's. The
// caller whose context is genuinely empty is the background one, and this is
// that caller's shape: a tenant known as a value, a context that knows nothing.
//
// context.Background() is used deliberately and must not be "helpfully"
// replaced with a tenant-carrying context -- that would restore exactly the
// blind spot this test was written to cover.
//
// POSTGRESQL ONLY: row-level security is the mechanism.
func TestPublishEventCarriesItsOwnTenant(t *testing.T) {
	su := testutil.SuiteTestDB(t, "eventtriggers")
	t.Cleanup(func() { su.Close() })
	testutil.SetupFullSchema(t, su, testutil.DialectPostgres)

	ctx := context.Background()
	dialect := plugin.DialectPostgres
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	p := &Plugin{}
	if err := p.Init(ctx, &plugin.Environment{Dialect: dialect, Logger: quiet}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const schema = "public"
	if err := plugin.RunMigrations(ctx, su, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}},
		plugin.WithSchema(schema)); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	rls := testutil.OpenPostgresRLSTestDB(t, su)
	t.Cleanup(func() { rls.Close() })
	grants := []string{`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + testutil.PostgresRLSTestRole}
	for _, tbl := range []string{"ingested_events", "event_subscriptions", "event_awaiters"} {
		grants = append(grants, `GRANT SELECT, INSERT, UPDATE, DELETE ON `+
			schema+`.`+tbl+` TO `+testutil.PostgresRLSTestRole)
	}
	for _, g := range grants {
		if _, err := su.ExecContext(ctx, g); err != nil {
			t.Fatalf("granting the RLS role access: %v\n  %s", err, g)
		}
	}
	db := &engine.SQLDBAdapter{DB: rls, Dialect: dialect}

	var suDB, rlsDB string
	if err := su.QueryRowContext(ctx, `SELECT current_database()`).Scan(&suDB); err != nil {
		t.Fatalf("current_database on the superuser connection: %v", err)
	}
	if err := rls.QueryRowContext(ctx, `SELECT current_database()`).Scan(&rlsDB); err != nil {
		t.Fatalf("current_database on the RLS connection: %v", err)
	}
	if suDB != rlsDB {
		t.Fatalf("UNMEASURED: seeding on %q, reading on %q", suDB, rlsDB)
	}

	for _, tbl := range []string{"ingested_events", "event_subscriptions", "event_awaiters"} {
		if _, err := su.ExecContext(ctx, `DELETE FROM `+schema+`.`+tbl); err != nil {
			t.Fatalf("clearing %s: %v", tbl, err)
		}
	}

	mine := uuid.MustParse("11112222-0000-4000-8000-0000000015ee")
	theirs := uuid.MustParse("33334444-0000-4000-8000-0000000015ff")

	// Another tenant's event, so the policy's USING clause is evaluated against
	// a row it must exclude rather than against an empty table.
	if _, err := su.ExecContext(ctx,
		`INSERT INTO `+schema+`.ingested_events (id, tenant_id, event_type, event_data)
		 VALUES (gen_random_uuid(), $1, 'theirs.event', '{}')`, theirs); err != nil {
		t.Fatalf("seeding the other tenant's event: %v", err)
	}

	// THE POSITIVE CONTROL. Everything below asserts that a tenantless call
	// SUCCEEDS -- which is also what an unscoped table produces. This
	// establishes that the policy is installed and fail-closed on this
	// connection before any of that is believed.
	var n int
	err := rls.QueryRowContext(ctx, `SELECT count(*) FROM `+schema+`.ingested_events`).Scan(&n)
	if err == nil {
		t.Fatalf("UNMEASURED: a read of ingested_events with no tenant set returned %d rows "+
			"instead of raising, so every assertion below would pass against an unscoped "+
			"table.", n)
	}
	if !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("UNMEASURED: the tenantless read failed for the wrong reason: %v", err)
	}

	// The call as a background loop makes it: the tenant is a value, and the
	// context carries nothing.
	eventID := uuid.New()
	matched, err := PublishEvent(context.Background(), db, quiet,
		&plugin.Environment{Dialect: dialect, Logger: quiet},
		eventID, mine, "kafka.message", map[string]any{"topic": "orders"})
	if err != nil {
		t.Fatalf("PublishEvent from a context with no tenant: %v\n\n"+
			"The tenant is in hand as an argument and ingested_events carries a policy "+
			"whose predicate raises when cleat.tenant_id is unset. PublishEvent has to "+
			"scope its own statements -- plugin.ForTenant(ctx, tenantID) at the top -- "+
			"because the callers that cannot do it for it are exactly the ones that reach "+
			"it with an empty context.", err)
	}
	if matched != 0 {
		t.Errorf("matched %d subscriptions, want 0 -- none was seeded", matched)
	}

	// The row landed, under the tenant passed as an argument.
	var gotTenant uuid.UUID
	if err := su.QueryRowContext(ctx,
		`SELECT tenant_id FROM `+schema+`.ingested_events WHERE id = $1`, eventID).Scan(&gotTenant); err != nil {
		t.Fatalf("reading back the published event: %v", err)
	}
	if gotTenant != mine {
		t.Errorf("the published event carries tenant %s, want %s", gotTenant, mine)
	}

	// And the other tenant's row is untouched -- a bypass would also make the
	// insert succeed, so this is what separates "scoped correctly" from
	// "scoped not at all".
	var strays int
	if err := su.QueryRowContext(ctx,
		`SELECT count(*) FROM `+schema+`.ingested_events WHERE tenant_id = $1`, theirs).Scan(&strays); err != nil {
		t.Fatalf("counting the other tenant's events: %v", err)
	}
	if strays != 1 {
		t.Errorf("the other tenant has %d events, want the 1 that was seeded", strays)
	}
}
