package notifications

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// TestNextAttemptAtIsStampedOnlyByTheDatabaseClock is cleat#2219's deterministic
// pin for the regression cleat-review found on #2198: sendWebhook and
// markRetrying used to stamp next_attempt_at with the APP clock (Go's
// time.Now()), which silently drifted from the DATABASE clock
// queryDueDeliveries compares it against (background.go's own doc comment on
// nowSQLExpr explains the mechanism and the ~35ms MySQL skew that caused it).
//
// #2198's fix, and the multidb tests that pin it
// (a_delivery_signs_with_the_configured_secret_multidb_test.go's same-tick
// assertion, a_retry_is_stamped_with_the_database_clock_multidb_test.go's
// window), both depend on a REAL clock skew between the test runner and a
// real database server to go red on a regression. Measured in cleat#2219: in
// CI, where the runner and the database share a host, that skew is ~0, so the
// valid mutant described there (sendWebhook stamping Go's time.Now() again,
// as a separate bound argument) passed 6/6 on MySQL and PostgreSQL and failed
// only 1/6 on MSSQL -- the regression test cannot reliably catch the
// regression it exists for.
//
// THIS TEST DOES NOT DEPEND ON ANY CLOCK AT ALL. It captures the exact SQL
// sendWebhook and markRetrying send to the database (via the same fake-driver
// pattern TestMarkRetrying already uses, extended here with an ownership
// check that reports "exists") and asserts, as a literal substring of that
// real query text, that next_attempt_at (and markRetrying's last_attempt_at)
// is the database-clock expression nowSQLExpr/nowPlusSecondsSQLExpr produces
// -- never a bound Go value. A regression that reintroduces app-clock
// stamping changes that substring on every single run, on every dialect,
// with no database, no Docker, and no timing dependency: falsified by
// temporarily reverting sendWebhook's %s to a sixth bound parameter (the
// issue's own named mutant) and confirming this test goes red while
// TestSendWebhookDeliversWithTheConfiguredSecret stays green 6/6 in the same
// CI environment where that regression is invisible.
//
// This is the issue's own named ALTERNATIVE ("a static guard that every
// next_attempt_at write goes through the DB-clock helpers"), chosen over its
// primary suggestion (a p.now clock hook): in the current, correct
// implementation, next_attempt_at never flows through a Go clock value at
// all -- nowSQLExpr/nowPlusSecondsSQLExpr are SQL text embedded directly in
// the query, not a bound parameter -- so a p.now hook would have nothing to
// intercept until some future regression happened to call it specifically,
// and a regression that called time.Now() directly (exactly how the original
// bug was written) would silently evade it. A structural assertion over the
// real generated SQL has no such blind spot: it does not care which Go clock
// source, if any, a future regression reaches for.
func TestNextAttemptAtIsStampedOnlyByTheDatabaseClock(t *testing.T) {
	dialects := []struct {
		name    string
		dialect plugin.Dialect
		// rebound is the placeholder spelling $2 takes after plugin.Rebind /
		// plugin.RebindArgs, inside nowPlusSecondsSQLExpr's own embedded
		// text -- see plugin/query.go: identity for PostgreSQL, "?" for
		// MySQL (RebindArgs rewrites every $N occurrence, including ones
		// embedded inside a helper's returned SQL text, to a bare "?"), and
		// "@p2" for MSSQL (Rebind's dollarRE).
		rebound string
	}{
		{"postgres", plugin.DialectPostgres, "$2"},
		{"mysql", plugin.DialectMySQL, "?"},
		{"mssql", plugin.DialectMSSQL, "@p2"},
	}

	for _, d := range dialects {
		d := d
		t.Run(d.name, func(t *testing.T) {
			t.Run("sendWebhook", func(t *testing.T) {
				conn := &existsThenRecordConnector{}
				db := sql.OpenDB(conn)
				defer db.Close()

				p := &Plugin{
					db:      &engine.SQLDBAdapter{DB: db, Dialect: d.dialect},
					dialect: d.dialect,
					logger:  discardLogger(),
				}
				cc := &plugin.CallContext{TenantID: uuid.New().String()}
				ctx := plugin.WithCallContext(context.Background(), cc)
				input := `{"webhook_id":"` + uuid.New().String() + `","event_type":"test.event","payload":{}}`
				if _, err := p.sendWebhook(ctx, input); err != nil {
					t.Fatalf("sendWebhook: %v", err)
				}

				conn.mu.Lock()
				records := conn.records
				conn.mu.Unlock()
				if len(records) != 1 {
					t.Fatalf("expected 1 exec record (the INSERT), got %d", len(records))
				}
				query := records[0].query

				// next_attempt_at is the 7th value in the column list
				// (id, webhook_id, event_type, payload, status, attempt_count,
				// next_attempt_at, created_at) -- "'pending', 0, " immediately
				// precedes it, and it must be EXACTLY nowSQLExpr's output, not
				// a bound placeholder.
				want := "'pending', 0, " + nowSQLExpr(d.dialect) + ","
				if !strings.Contains(query, want) {
					t.Errorf("sendWebhook's INSERT does not stamp next_attempt_at with the "+
						"database clock on %s -- want %q as a literal substring, got query: %s",
						d.name, want, query)
				}
			})

			t.Run("markRetrying", func(t *testing.T) {
				conn := &recordingConnector{}
				db := sql.OpenDB(conn)
				defer db.Close()

				p := &Plugin{
					db:      &engine.SQLDBAdapter{DB: db, Dialect: d.dialect},
					dialect: d.dialect,
					logger:  discardLogger(),
				}
				if err := p.markRetrying(context.Background(), uuid.New(), 1, "request failed"); err != nil {
					t.Fatalf("markRetrying: %v", err)
				}

				conn.mu.Lock()
				records := conn.records
				conn.mu.Unlock()
				if len(records) != 1 {
					t.Fatalf("expected 1 exec record (the UPDATE), got %d", len(records))
				}
				query := records[0].query

				wantLast := "last_attempt_at = " + nowSQLExpr(d.dialect) + ","
				if !strings.Contains(query, wantLast) {
					t.Errorf("markRetrying's UPDATE does not stamp last_attempt_at with the "+
						"database clock on %s -- want %q as a literal substring, got query: %s",
						d.name, wantLast, query)
				}
				wantNext := "next_attempt_at = " + nowPlusSecondsSQLExpr(d.dialect, d.rebound) + ","
				if !strings.Contains(query, wantNext) {
					t.Errorf("markRetrying's UPDATE does not stamp next_attempt_at with the "+
						"database clock on %s -- want %q as a literal substring, got query: %s",
						d.name, wantNext, query)
				}
			})
		})
	}
}

// existsThenRecordConnector is recordingConnector (notifications_behavioral_test.go)
// plus an ownership check that always reports "exists": sendWebhook's path to
// the INSERT this test needs to capture runs a QueryRow ownership check
// first, and recordingConn's own QueryContext (tailored for a due-deliveries
// scan elsewhere in this package) returns no rows, which reads as "not
// found" and sendWebhook never reaches the INSERT at all. A dedicated
// connector, rather than widening the shared one, keeps every other test
// that already depends on recordingConn's current QueryContext behaviour
// unaffected.
type existsThenRecordConnector struct {
	mu      sync.Mutex
	records []execRecord
}

func (c *existsThenRecordConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &existsThenRecordConn{parent: c}, nil
}

func (c *existsThenRecordConnector) Driver() driver.Driver { return &recordingDrv{} }

type existsThenRecordConn struct {
	parent *existsThenRecordConnector
}

func (*existsThenRecordConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, io.ErrClosedPipe
}

func (*existsThenRecordConn) Close() error { return nil }

func (*existsThenRecordConn) Begin() (driver.Tx, error) { return &recordingTx{}, nil }

func (c *existsThenRecordConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.parent.mu.Lock()
	c.parent.records = append(c.parent.records, execRecord{query: query, args: args})
	c.parent.mu.Unlock()
	return &recordingResult{}, nil
}

func (c *existsThenRecordConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	return &existsRows{}, nil
}

// existsRows reports a single row whose one column ("exists") is true --
// enough for webhookExistsSQL's `Scan(&exists)` to read the webhook as
// present, on every dialect: database/sql's driver.Bool converter accepts an
// int64 source for a *bool destination.
type existsRows struct {
	done bool
}

func (*existsRows) Columns() []string { return []string{"exists"} }
func (*existsRows) Close() error      { return nil }

func (r *existsRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(1)
	return nil
}
