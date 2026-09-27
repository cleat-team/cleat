package webhookingest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// TestAnIngestRacingAnOpenDeleteTransactionIsBlocked pins cleat-review's
// second finding on #2221's own race fix: the EXISTS-guarded INSERT
// (routes.go) narrowed the window between handleDeleteSource's soft-delete
// and handleIngestWebhook's write, but did not close it on every dialect.
//
// FOR SHARE turns the subquery into a locking read: against a row an open
// UPDATE already holds, it BLOCKS until that transaction ends, then re-reads
// under READ COMMITTED's per-statement snapshot rule and sees the
// now-committed deleted_at. This test proves the BLOCK, not merely the
// eventual answer -- it holds the delete open, starts the ingest
// concurrently, waits long enough that an unblocked ingest would already
// have finished and signalled, and only then commits the delete. A version
// of this test that just ran both sequentially would pass whether or not the
// ingest actually waited for anything.
//
// PostgreSQL, MySQL (at both its default isolation and at READ COMMITTED)
// and SQL Server. PostgreSQL: FOR SHARE closes the gap at its default READ
// COMMITTED, where a plain read does not see an UPDATE inside a still-open
// transaction.
//
// MySQL gets TWO subtests, and neither is on the strength of the routes.go
// comment's original claim that its default isolation already blocks a
// plain read against a row an open UPDATE holds -- that claim shipped in
// #2221 with no live-interleaving test behind it, which is exactly the
// unmeasured-claim shape CLAUDE.md warns about, and it was also imprecise: a
// plain InnoDB SELECT never blocks. What actually closes the gap is
// narrower -- this is an INSERT ... SELECT, and InnoDB takes shared
// next-key locks on the rows the SELECT half reads, but only at REPEATABLE
// READ (MySQL's default). "mysql" measures that: real MySQL, its ordinary
// connection, same runIngestRaceTest, same 700ms proof of blocking.
// "mysql_read_committed" measures the case docs/reference/database-backends.md
// actually recommends for this dialect -- READ COMMITTED, where the same
// INSERT ... SELECT does NOT take those locks and the gap reopens unless
// FOR SHARE forces it, which is why FOR SHARE stays unconditional in
// routes.go rather than being gated on MySQL's isolation level. cleat#2242,
// cleat-review.
//
// SQL Server: with READ_COMMITTED_SNAPSHOT (RCSI) ON -- the configuration
// docs/reference/database-backends.md recommends -- a plain read sees a
// row-versioned snapshot instead of blocking on the open UPDATE's lock, so
// it has the SAME gap PostgreSQL has and needs the SAME kind of fix: WITH
// (REPEATABLEREAD) on the existsGuard SELECT (routes.go) -- not
// READCOMMITTEDLOCK, which closes the race but opens a deadlock window
// instead (see routes.go's comment for the measurements). This test runs
// SQL Server's leg against a PRIVATE database with RCSI forced ON, because
// RCSI ON is the case the fix exists for and the shared CI database's own
// RCSI setting is not this test's to depend on. cleat#2237, the same gap
// cleat#2233 closed for notifications' sendWebhook -- this test is that
// PR's TestASendWebhookRacingAnOpenDeleteTransactionIsBlocked, adapted to
// this package's HTTP handler.
//
// Every subtest whose backend is optional SKIPS explicitly rather than
// silently doing nothing: a loop over testutil.NewPluginTestBackends that
// filters by dialect and falls through with no t.Run body when the dialect
// is absent reports a trivial PASS, not a skip -- measured directly on the
// "mysql" subtest with CLEAT_TEST_MYSQL unset (0.06s, no work done, no skip
// event in `go test -json`). cleat-review's nit.
func TestAnIngestRacingAnOpenDeleteTransactionIsBlocked(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectPostgres)
		runIngestRaceTest(t, plugin.DialectPostgres, db)
	})

	t.Run("mysql", func(t *testing.T) {
		if os.Getenv("CLEAT_TEST_MYSQL") == "" {
			t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
		}
		db := testutil.MySQLTestDB(t)
		runIngestRaceTest(t, plugin.DialectMySQL, db)
	})

	t.Run("mysql_read_committed", func(t *testing.T) {
		if os.Getenv("CLEAT_TEST_MYSQL") == "" {
			t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
		}
		db := mysqlReadCommittedTestDB(t)
		runIngestRaceTest(t, plugin.DialectMySQL, db)
	})

	t.Run("mssql_rcsi_on", func(t *testing.T) {
		if os.Getenv("CLEAT_TEST_MSSQL") == "" {
			t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
		}
		db := webhookingestPrivateRCSIDatabase(t)
		runIngestRaceTest(t, plugin.DialectMSSQL, db)
	})
}

// mysqlReadCommittedTestDB opens CLEAT_TEST_MYSQL with
// transaction_isolation forced to READ-COMMITTED, the isolation level
// docs/reference/database-backends.md actually recommends for this dialect
// -- MySQL's default (REPEATABLE READ) gives INSERT ... SELECT an implicit
// shared-lock read that masks the race FOR SHARE exists to close.
//
// The go-sql-driver/mysql DSN param, not a SET SESSION after connect: the
// pool may hand runIngestRaceTest's two concurrent statements (the seeded
// create and the racing ingest) different pooled connections, and a SET
// SESSION issued on only one of them would leave the other at the default
// isolation with no visible error -- silently testing the wrong thing. The
// DSN param applies to every connection the pool opens.
func mysqlReadCommittedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("CLEAT_TEST_MYSQL")
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	dsn := base + sep + "transaction_isolation=%27READ-COMMITTED%27"
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open MySQL at READ COMMITTED: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping MySQL at READ COMMITTED: %v", err)
	}

	var iso string
	if err := db.QueryRow("SELECT @@transaction_isolation").Scan(&iso); err != nil {
		t.Fatalf("read @@transaction_isolation: %v", err)
	}
	if iso != "READ-COMMITTED" {
		t.Fatalf("precondition: @@transaction_isolation reads %q, want READ-COMMITTED -- "+
			"mysqlReadCommittedTestDB's own setup is wrong, not the tree", iso)
	}
	return db
}

// runIngestRaceTest holds handleDeleteSource's soft-delete UPDATE open in its
// own uncommitted transaction, starts a concurrent ingest against the same
// source, and asserts it blocks until the delete commits and then refuses --
// not a signalled, stored event.
func runIngestRaceTest(t *testing.T, dialect plugin.Dialect, db *sql.DB) {
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

	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x5a
	}
	ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: key})
	if err != nil {
		t.Fatalf("build key ring: %v", err)
	}
	secretStore := engine.NewSecretStoreWithRing(db, string(dialect), ring)
	p.db = &engine.SQLDBAdapter{DB: db, Dialect: dialect}
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

	const rawSecret = "interleaving-secret"
	createBody := `{"name":"interleaving","source_type":"github",` +
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

	// Open handleDeleteSource's own soft-delete UPDATE directly, in its own
	// transaction, and hold it open -- reproduced verbatim from routes.go's
	// handleDeleteSource rather than called through the route, because the
	// route commits before returning and this test needs the transaction to
	// stay open across the ingest. Through p.db.Begin(seedCtx), not a bare
	// db.BeginTx: webhook_sources carries a row-level policy (migrations.go,
	// TenantScoped), and a write with no tenant in SESSION_CONTEXT is BLOCKED
	// outright on SQL Server (migration 103's BLOCK predicates) -- the
	// original PostgreSQL-only version of this test used db.BeginTx directly
	// because PostgreSQL's superuser connection bypasses RLS unconditionally,
	// which does not hold on SQL Server.
	seedCtx := plugin.ForTenant(ctx, tenantID)
	deleteTx, err := p.db.Begin(seedCtx)
	if err != nil {
		t.Fatalf("begin delete tx: %v", err)
	}
	if _, err := deleteTx.Exec(seedCtx, `
			UPDATE webhook_sources
			SET enabled = false, deleted_at = now()
			WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
		`, sourceID, tenantID); err != nil {
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

	// Long enough that an UNBLOCKED ingest would already have finished and
	// signalled -- proving the block, not sampling a lucky ordering.
	select {
	case res := <-ingestDone:
		deleteTx.Rollback()
		t.Fatalf("ingest returned BEFORE the delete committed (code=%d, body=%s) -- "+
			"the existence guard did not block it", res.code, res.body)
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

	var eventCount int
	if err := plugintest.QueryRowRebound(t, ctx, db, dialect,
		`SELECT count(*) FROM webhook_events WHERE source_id = $1`,
		sourceID).Scan(&eventCount); err != nil {
		t.Fatalf("count webhook_events for source: %v", err)
	}
	if eventCount != 0 {
		t.Errorf("webhook_events rows for the raced source: got %d, want 0", eventCount)
	}
}

// webhookingestPrivateRCSIDatabase creates a database named
// cleat_test_2237_rcsi on the same server CLEAT_TEST_MSSQL points at, sets
// READ_COMMITTED_SNAPSHOT ON before anything else connects to it, and
// registers a t.Cleanup that drops it.
//
// A private database, not the shared CLEAT_TEST_MSSQL one, for the same
// reason engine's mssqlPrivateRCSIDatabase
// (mssql_retention_2060_lock_escalation_test.go) and notifications'
// notificationsPrivateRCSIDatabase (cleat#2233) use one: ALTER DATABASE SET
// READ_COMMITTED_SNAPSHOT affects every session connected to that database,
// and flipping the shared CI database's setting mid-suite (or leaving it
// flipped after) would change what every other test measures. Copied rather
// than shared across packages -- coordinator's #2237 call, to dedupe later
// rather than stack this branch on #2233's for an unexported test helper.
func webhookingestPrivateRCSIDatabase(t *testing.T) *sql.DB {
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

	const dbName = "cleat_test_2237_rcsi"
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
			"webhookingestPrivateRCSIDatabase's own setup is wrong, not the tree", dbName)
	}

	testutil.SetupMSSQLFullSchema(t, db)
	return db
}
