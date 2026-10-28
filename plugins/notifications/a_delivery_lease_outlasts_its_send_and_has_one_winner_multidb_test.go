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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#3526. The claim is a compare-and-swap on the row's due time. These pin
// what makes it sound: the lease is longer than the send it protects, exactly
// one of N concurrent claims wins, a lease that runs out frees the row, and a
// cancelled row cannot be claimed.

// recordingDB captures what claimDelivery sends to the database.
type recordingDB struct {
	plugin.PluginDB
	queries []string
	args    [][]any
}

func (r *recordingDB) Exec(_ context.Context, q string, args ...any) (int64, error) {
	r.queries = append(r.queries, q)
	r.args = append(r.args, args)
	return 1, nil
}

// TestTheLeaseOutlastsTheSlowestSend: if a send could outlast its lease, another
// sweeper would reclaim the row and send again -- the defect, back through a
// different path. The lease is derived from the HTTP timeout, and the client in
// Init is built from the same constant, so changing one cannot leave the other.
func TestTheLeaseOutlastsTheSlowestSend(t *testing.T) {
	if deliveryLeaseMargin <= 0 {
		t.Errorf("the lease margin is %v; a lease with no margin ends the instant the HTTP timeout does, "+
			"before the secret fetch and the closing write that surround the send", deliveryLeaseMargin)
	}
	if deliveryLease <= deliveryHTTPTimeout {
		t.Errorf("the lease (%v) does not outlast the HTTP timeout (%v)", deliveryLease, deliveryHTTPTimeout)
	}
	if deliveryLease < deliveryHTTPTimeout+deliveryLeaseMargin {
		t.Errorf("the lease (%v) is shorter than timeout + margin (%v)", deliveryLease, deliveryHTTPTimeout+deliveryLeaseMargin)
	}

	p := &Plugin{}
	if err := p.Init(context.Background(), &plugin.Environment{Dialect: plugin.DialectPostgres}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := p.httpClient.Timeout; got != deliveryHTTPTimeout {
		t.Errorf("the delivery client's timeout is %v, but the lease is derived from %v: the lease no longer "+
			"covers the slowest send", got, deliveryHTTPTimeout)
	}
}

// TestALeaseUnderASecondIsRefused: a lease of 0 would claim nothing, and on
// MySQL (rows CHANGED, not matched) would make a good claim look like zero rows.
func TestALeaseUnderASecondIsRefused(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second, 999 * time.Millisecond} {
		if _, err := leaseSeconds(d); err == nil {
			t.Errorf("leaseSeconds(%v) was accepted; a lease under one second must be refused", d)
		}
	}
	if got, err := leaseSeconds(deliveryLease); err != nil || got != int(deliveryLease/time.Second) {
		t.Errorf("leaseSeconds(deliveryLease) = %d, %v", got, err)
	}
}

// TestTheClaimUsesTheDatabaseClockOnBothSides: no Go time.Time reaches the
// statement, and both the SET and the WHERE name the database's own now.
func TestTheClaimUsesTheDatabaseClockOnBothSides(t *testing.T) {
	for _, d := range []plugin.Dialect{plugin.DialectPostgres, plugin.DialectMySQL, plugin.DialectMSSQL} {
		rec := &recordingDB{}
		p := &Plugin{dialect: d, db: rec}
		if ok, err := p.claimDelivery(context.Background(), uuid.New()); err != nil || !ok {
			t.Fatalf("%s: claimDelivery = %v, %v", d, ok, err)
		}
		if len(rec.queries) != 1 {
			t.Fatalf("%s: want one statement, got %d", d, len(rec.queries))
		}
		for _, a := range rec.args[0] {
			if _, isTime := a.(time.Time); isTime {
				t.Errorf("%s: a Go time.Time is bound to the claim (%v): the app clock decides whether a lease has run out", d, a)
			}
		}
		q := rec.queries[0]
		if !strings.Contains(q, nowSQLExpr(d)) || !strings.Contains(q, nowPlusSecondsSQLExpr(d, "$1")) {
			t.Errorf("%s: the claim does not use the database clock on both sides:\n%s", d, q)
		}
	}
}

// claimFixture is the plugin, schema, receiver and one webhook for a test below.
func claimFixture(t *testing.T, be testutil.PluginTestBackend, h http.HandlerFunc) (p *Plugin, sweepCtx context.Context, webhookID string, tenantCtx context.Context, cleanup func()) {
	t.Helper()
	ctx := context.Background()
	dialect := plugin.Dialect(string(be.Dialect))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	testutil.SetupFullSchema(t, be.DB, be.Dialect)

	p = &Plugin{dialect: dialect, logger: quiet, httpClient: &http.Client{Timeout: 20 * time.Second}}
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

	receiver := httptest.NewServer(h)
	t.Cleanup(receiver.Close)

	tenantID := uuid.MustParse(engine.DefaultTenantUUID)
	tenantCtx = auth.WithTenantID(context.Background(), tenantID)
	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"url":%q,"secret":"claim-test-secret","events":["test.event"]}`, receiver.URL)
	p.handleCreateWebhook(rec, httptest.NewRequest("POST", "/webhooks", strings.NewReader(body)).WithContext(tenantCtx))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create webhook: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	sweepCtx = plugin.AcrossAllTenants(ctx, "test: delivery claim")
	webhookID = created["id"].(string)
	// The database is shared with the rest of the package, and a due row left behind
	// is swept by whichever test runs next. Each test removes the deliveries it made,
	// and defers this AFTER be.Cleanup so it runs while the connection is open.
	cleanup = func() {
		if _, err := p.db.Exec(sweepCtx, `DELETE FROM webhook_delivery WHERE webhook_id = $1`, webhookID); err != nil {
			t.Logf("could not remove this test's deliveries: %v", err)
		}
	}
	return p, sweepCtx, webhookID, tenantCtx, cleanup
}

// queueDelivery sends one webhook through the real host function and returns the
// delivery's id.
func queueDelivery(t *testing.T, p *Plugin, be testutil.PluginTestBackend, tenantCtx context.Context, webhookID string) uuid.UUID {
	t.Helper()
	cc := &plugin.CallContext{TenantID: engine.DefaultTenantUUID, WorkflowID: "wf-claim", DB: be.DB}
	out, err := p.sendWebhook(plugin.WithCallContext(tenantCtx, cc),
		fmt.Sprintf(`{"webhook_id":%q,"event_type":"test.event","payload":{"n":1}}`, webhookID))
	if err != nil {
		t.Fatalf("sendWebhook: %v", err)
	}
	var o struct {
		DeliveryID uuid.UUID `json:"delivery_id"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.DeliveryID == uuid.Nil {
		t.Fatalf("decode sendWebhook output %q: %v", out, err)
	}
	return o.DeliveryID
}

// setDue moves a delivery's due time by seconds relative to the database's now.
func setDue(t *testing.T, p *Plugin, ctx context.Context, id uuid.UUID, seconds int) {
	t.Helper()
	if _, err := p.db.Exec(ctx, fmt.Sprintf(`UPDATE webhook_delivery SET next_attempt_at = %s WHERE id = $2`,
		nowPlusSecondsSQLExpr(p.dialect, "$1")), seconds, id); err != nil {
		t.Fatalf("set due time: %v", err)
	}
}

// claimRounds runs the concurrent claim race several times against one delivery.
func claimRounds(t *testing.T, p *Plugin, sweepCtx context.Context, id uuid.UUID) {
	t.Helper()
	const rounds, claimers = 12, 8
	for round := 0; round < rounds; round++ {
		setDue(t, p, sweepCtx, id, -5) // due again: a fresh race each round
		var wins, errs atomic.Int32
		var firstErr atomic.Value
		var ready, start, done sync.WaitGroup
		ready.Add(claimers)
		start.Add(1)
		for i := 0; i < claimers; i++ {
			done.Add(1)
			go func() {
				defer done.Done()
				ready.Done()
				start.Wait()
				ok, err := p.claimDelivery(sweepCtx, id)
				if err != nil {
					errs.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
					return
				}
				if ok {
					wins.Add(1)
				}
			}()
		}
		ready.Wait()
		start.Done()
		done.Wait()
		if errs.Load() != 0 {
			t.Fatalf("round %d: %d claims returned an error; the first: %v", round, errs.Load(), firstErr.Load())
		}
		if wins.Load() != 1 {
			t.Fatalf("round %d: %d of %d concurrent claims won, want exactly 1", round, wins.Load(), claimers)
		}
	}
}

// TestConcurrentClaimsOfOneDeliveryHaveOneWinner is the compare-and-swap itself:
// N sweepers that all read the row as due and all try to claim it. Several rounds,
// because a single round of a millisecond-wide race detects far less than it looks
// (CLAUDE.md: 40% in one round of eight, 10 of 10 in six). On SQL Server this is
// also where it is settled whether the UPDATE re-checks its predicate after waiting
// on a row lock under RCSI.
func TestConcurrentClaimsOfOneDeliveryHaveOneWinner(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)
			p, sweepCtx, webhookID, tenantCtx, cleanup := claimFixture(t, be, func(w http.ResponseWriter, r *http.Request) {})
			defer cleanup()
			id := queueDelivery(t, p, be, tenantCtx, webhookID)

			// SQL Server's answer depends on READ_COMMITTED_SNAPSHOT: whether an
			// UPDATE waiting on a row lock re-checks its predicate afterwards.
			// cleat does not turn RCSI on itself, but docs/reference/database-
			// backends.md recommends it and Azure SQL Database, the preferred Azure
			// target, has it on, so both regimes are run and the setting is stated
			// rather than assumed.
			rcsiState := func() int {
				var v int
				if err := be.DB.QueryRowContext(context.Background(),
					`SELECT CAST(is_read_committed_snapshot_on AS int) FROM sys.databases WHERE name = DB_NAME()`).Scan(&v); err != nil {
					t.Fatalf("read the SQL Server isolation setting: %v", err)
				}
				return v
			}
			claimRounds(t, p, sweepCtx, id)
			if plugin.Dialect(string(be.Dialect)) == plugin.DialectMSSQL {
				t.Logf("SQL Server READ_COMMITTED_SNAPSHOT = %d for the first pass", rcsiState())
				var name string
				if err := be.DB.QueryRowContext(context.Background(), `SELECT DB_NAME()`).Scan(&name); err != nil {
					t.Fatalf("read the database name: %v", err)
				}
				if _, err := be.DB.ExecContext(context.Background(),
					fmt.Sprintf("ALTER DATABASE [%s] SET READ_COMMITTED_SNAPSHOT ON WITH ROLLBACK IMMEDIATE", name)); err != nil {
					t.Fatalf("UNMEASURED: could not turn READ_COMMITTED_SNAPSHOT on: %v", err)
				}
				// WITH ROLLBACK IMMEDIATE disconnects the pool's idle sessions, and the
				// first claim to reuse one fails with "Read: EOF" (measured: exactly one
				// error, in round 0, on every off-to-on transition). That is the test
				// setup killing a connection, not the claim; drop the idle connections
				// so the race below runs on fresh ones.
				be.DB.SetMaxIdleConns(0)
				be.DB.SetMaxIdleConns(10)
				if got := rcsiState(); got != 1 {
					t.Fatalf("UNMEASURED: READ_COMMITTED_SNAPSHOT is %d after turning it on", got)
				}
				t.Logf("SQL Server READ_COMMITTED_SNAPSHOT = 1 for the second pass")
				claimRounds(t, p, sweepCtx, id)
			}
		})
	}
}

// TestAClaimedDeliveryIsFreedWhenItsLeaseRunsOut: a sweep that claims and dies
// must not strand the row. The lease is not waited out (60s); the due time is
// moved into the past, which is what its expiry is.
func TestAClaimedDeliveryIsFreedWhenItsLeaseRunsOut(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)
			var posts atomic.Int32
			p, sweepCtx, webhookID, tenantCtx, cleanup := claimFixture(t, be, func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
			})
			defer cleanup()
			id := queueDelivery(t, p, be, tenantCtx, webhookID)

			// A claim that is never followed by a send: the sweep died.
			if ok, err := p.claimDelivery(sweepCtx, id); err != nil || !ok {
				t.Fatalf("claim: %v, %v", ok, err)
			}
			// About THIS delivery and THIS receiver, not the sweep's totals: the
			// database is shared, and another test's due row would be attempted too.
			if _, _, _, err := p.processDeliveries(sweepCtx, context.Background()); err != nil {
				t.Fatal(err)
			}
			if posts.Load() != 0 {
				t.Fatalf("a leased delivery was sent (%d POSTs) while its lease was running", posts.Load())
			}

			setDue(t, p, sweepCtx, id, -1) // the lease has run out
			if _, _, _, err := p.processDeliveries(sweepCtx, context.Background()); err != nil {
				t.Fatal(err)
			}
			if posts.Load() != 1 {
				t.Errorf("after the lease ran out the delivery was sent %d times, want exactly once", posts.Load())
			}
		})
	}
}

// TestACancelledDeliveryCannotBeClaimed: handleDeleteWebhook cancels pending
// deliveries; a sweep that read the row before that commit must not send it after.
func TestACancelledDeliveryCannotBeClaimed(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)
			p, sweepCtx, webhookID, tenantCtx, cleanup := claimFixture(t, be, func(w http.ResponseWriter, r *http.Request) {})
			defer cleanup()
			id := queueDelivery(t, p, be, tenantCtx, webhookID)

			if _, err := p.db.Exec(sweepCtx, `UPDATE webhook_delivery SET status = 'cancelled' WHERE id = $1`, id); err != nil {
				t.Fatalf("cancel: %v", err)
			}
			ok, err := p.claimDelivery(sweepCtx, id)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Error("a cancelled delivery was claimed, so a sweep that read it earlier would still send it")
			}
		})
	}
}
