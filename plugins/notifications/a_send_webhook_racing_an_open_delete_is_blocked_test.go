package notifications

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestASendWebhookRacingAnOpenDeleteTransactionIsBlocked pins
// host_functions.go's EXISTS-guarded INSERT (send_webhook, cleat#2220/#2222)
// against a LIVE interleaving, matching #2221's own template for
// webhookingest
// (a_an_ingest_racing_an_open_delete_on_postgres_is_blocked_test.go):
// cleat-review found that
// TestADeletedWebhooksPendingDeliveriesAreCancelledNotSent's race coverage
// was a retyped copy of the guarded INSERT's SQL, run deterministically
// against an already-committed delete -- and that copy still passed with the
// real guard mutated to `1=1 OR EXISTS` or with FOR SHARE dropped entirely,
// because it is not the code under test. This test calls the REAL
// p.sendWebhook, concurrently, against a delete held open in an uncommitted
// transaction, and proves the BLOCK itself (it waits long enough that an
// unblocked call would already have finished and returned), not merely the
// eventual answer.
//
// PostgreSQL and SQL Server, not MySQL. PostgreSQL: FOR SHARE closes the gap
// at its default READ COMMITTED, where a plain read does not see an UPDATE
// inside a still-open transaction. SQL Server: with READ_COMMITTED_SNAPSHOT
// (RCSI) ON -- the configuration docs/reference/database-backends.md
// recommends -- a plain read sees a row-versioned snapshot instead of
// blocking on the open UPDATE's lock, so it has the SAME gap PostgreSQL has
// and needed the SAME kind of fix: WITH (READCOMMITTEDLOCK) on the
// existsGuard SELECT. This test runs SQL Server's leg against a PRIVATE
// database with RCSI forced ON, because RCSI ON is the case the fix exists
// for and the shared CI database's own RCSI setting is not this test's to
// depend on. MySQL is excluded: its default isolation already blocks a
// plain read against a row an open UPDATE holds, with no dialect-specific
// tuning needed, so there is no analogous race to close or test -- the same
// exemption #2221's template records for webhookingest's version of this
// test.
func TestASendWebhookRacingAnOpenDeleteTransactionIsBlocked(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		for _, be := range testutil.NewPluginTestBackends(t) {
			if be.Dialect != testutil.DialectPostgres {
				continue
			}
			defer be.Cleanup()
			runSendWebhookRaceTest(t, plugin.DialectPostgres, be.DB)
			return
		}
	})

	t.Run("mssql_rcsi_on", func(t *testing.T) {
		if os.Getenv("CLEAT_TEST_MSSQL") == "" {
			t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
		}
		db := notificationsPrivateRCSIDatabase(t)
		runSendWebhookRaceTest(t, plugin.DialectMSSQL, db)
	})
}

// runSendWebhookRaceTest holds handleDeleteWebhook's soft-delete UPDATE open
// in its own uncommitted transaction, starts a concurrent send_webhook call
// against the same webhook, and asserts it blocks until the delete commits
// and then refuses -- not a new pending delivery.
func runSendWebhookRaceTest(t *testing.T, dialect plugin.Dialect, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	testDialect := testutil.Dialect(dialect)
	testutil.SetupFullSchema(t, db, testDialect)

	p := &Plugin{dialect: dialect, logger: quiet}
	if err := plugin.RunMigrations(ctx, db, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	p.db = &engine.SQLDBAdapter{DB: db, Dialect: dialect}

	tenantID := uuid.MustParse(engine.DefaultTenantUUID)
	webhookID := uuid.New()
	now := time.Now()
	// Through plugin.ForTenant, not a bare ctx: webhook_config carries a
	// row-level policy since migrations.go v2, and a write with no tenant in
	// SESSION_CONTEXT is BLOCKED outright on SQL Server (migration 103's
	// BLOCK predicates).
	seedCtx := plugin.ForTenant(ctx, tenantID)
	if _, err := p.db.Exec(seedCtx, plugin.Rebind(`
			INSERT INTO webhook_config (tenant_id, id, url, secret_configured, events, enabled, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, true, $6, $7)
		`, dialect), tenantID, webhookID, "https://example.com/race", true, `["test.event"]`, now, now); err != nil {
		t.Fatalf("seed webhook_config: %v", err)
	}

	// Open handleDeleteWebhook's own soft-delete UPDATE directly, in its own
	// transaction, and hold it open -- reproduced verbatim from routes.go's
	// handleDeleteWebhook rather than called through the route, because the
	// route commits before returning and this test needs the transaction to
	// stay open across the race. Through p.db.Begin(seedCtx), exactly as
	// handleDeleteWebhook itself does with p.db.Begin(r.Context()): a bare
	// *sql.Tx from db.BeginTx carries no tenant in SESSION_CONTEXT, and a
	// write with none is BLOCKED outright on SQL Server (migration 103's
	// BLOCK predicates).
	deleteTx, err := p.db.Begin(seedCtx)
	if err != nil {
		t.Fatalf("begin delete tx: %v", err)
	}
	if _, err := deleteTx.Exec(seedCtx, plugin.Rebind(`
			UPDATE webhook_config
			SET enabled = false, deleted_at = now()
			WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
		`, dialect), webhookID, tenantID); err != nil {
		t.Fatalf("apply soft-delete inside open tx: %v", err)
	}

	// Start the racing send_webhook call concurrently.
	cc := &plugin.CallContext{TenantID: tenantID.String(), WorkflowID: "wf-race", DB: db}
	sendCtx := plugin.WithCallContext(auth.WithTenantID(context.Background(), tenantID), cc)
	sendInput := fmt.Sprintf(`{"webhook_id":"%s","event_type":"raced.event"}`, webhookID)

	type sendResult struct {
		output string
		err    error
	}
	sendDone := make(chan sendResult, 1)
	go func() {
		output, err := p.sendWebhook(sendCtx, sendInput)
		sendDone <- sendResult{output: output, err: err}
	}()

	// Long enough that an UNBLOCKED call would already have finished --
	// proving the block, not sampling a lucky ordering.
	select {
	case res := <-sendDone:
		deleteTx.Rollback()
		t.Fatalf("sendWebhook returned BEFORE the delete committed (output=%q err=%v) -- "+
			"the existence guard did not block it", res.output, res.err)
	case <-time.After(700 * time.Millisecond):
	}

	if err := deleteTx.Commit(); err != nil {
		t.Fatalf("commit delete tx: %v", err)
	}

	var res sendResult
	select {
	case res = <-sendDone:
	case <-time.After(5 * time.Second):
		t.Fatal("sendWebhook did not return within 5s of the delete committing -- deadlock?")
	}

	if res.err == nil {
		t.Errorf("sendWebhook racing an open delete: want an error, got output %q", res.output)
	} else if got := res.err.Error(); !strings.Contains(got, "webhook not found") {
		t.Errorf("sendWebhook racing an open delete: got %q, want it to mention \"webhook not found\"", got)
	}

	var deliveryCount int
	if err := db.QueryRowContext(ctx, plugin.Rebind(
		`SELECT count(*) FROM webhook_delivery WHERE webhook_id = $1`, dialect),
		webhookID).Scan(&deliveryCount); err != nil {
		t.Fatalf("count webhook_delivery: %v", err)
	}
	if deliveryCount != 0 {
		t.Errorf("webhook_delivery rows for the raced webhook: got %d, want 0", deliveryCount)
	}
}

// notificationsPrivateRCSIDatabase creates a database named
// cleat_test_2233_rcsi on the same server CLEAT_TEST_MSSQL points at, sets
// READ_COMMITTED_SNAPSHOT ON before anything else connects to it, and
// registers a t.Cleanup that drops it.
//
// A private database, not the shared CLEAT_TEST_MSSQL one, for the same
// reason engine's mssqlPrivateRCSIDatabase (mssql_retention_2060_lock_escalation_test.go)
// uses one: ALTER DATABASE SET READ_COMMITTED_SNAPSHOT affects every session
// connected to that database, and flipping the shared CI database's setting
// mid-suite (or leaving it flipped after) would change what every other
// test measures.
func notificationsPrivateRCSIDatabase(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()

	base := os.Getenv("CLEAT_TEST_MSSQL")
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse CLEAT_TEST_MSSQL: %v", err)
	}
	adminDB, err := sql.Open("sqlserver", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer adminDB.Close()

	const dbName = "cleat_test_2233_rcsi"
	drop := fmt.Sprintf(
		"IF DB_ID('%s') IS NOT NULL BEGIN ALTER DATABASE [%s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [%s]; END",
		dbName, dbName, dbName)
	if _, err := adminDB.ExecContext(ctx, drop); err != nil {
		t.Fatalf("drop any %s left by a prior interrupted run: %v", dbName, err)
	}
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE ["+dbName+"]"); err != nil {
		t.Fatalf("create %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		cleanupDB, err := sql.Open("sqlserver", base)
		if err != nil {
			t.Logf("cleanup: reopen admin connection: %v", err)
			return
		}
		defer cleanupDB.Close()
		if _, err := cleanupDB.ExecContext(context.Background(), drop); err != nil {
			t.Logf("cleanup: drop %s: %v", dbName, err)
		}
	})

	// Set before anything else connects to dbName, so there is nothing
	// else's session to roll back.
	if _, err := adminDB.ExecContext(ctx,
		fmt.Sprintf("ALTER DATABASE [%s] SET READ_COMMITTED_SNAPSHOT ON WITH ROLLBACK IMMEDIATE", dbName)); err != nil {
		t.Fatalf("set READ_COMMITTED_SNAPSHOT on %s: %v", dbName, err)
	}

	q := u.Query()
	q.Set("database", dbName)
	u.RawQuery = q.Encode()
	dsn := u.String()

	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dbName, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping %s: %v", dbName, err)
	}

	var rcsi bool
	if err := db.QueryRowContext(ctx,
		`SELECT is_read_committed_snapshot_on FROM sys.databases WHERE name = DB_NAME()`,
	).Scan(&rcsi); err != nil {
		t.Fatalf("read is_read_committed_snapshot_on: %v", err)
	}
	if !rcsi {
		t.Fatalf("precondition: %s reads READ_COMMITTED_SNAPSHOT=false, want true -- "+
			"notificationsPrivateRCSIDatabase's own setup is wrong, not the tree", dbName)
	}

	testutil.SetupMSSQLFullSchema(t, db)
	return db
}
