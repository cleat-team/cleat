package auth

// cleat#3678 (owner decisions D65 and D70). On SQL Server a scoped read carries the clear of the session
// tenant in its own batch, so the clear costs no round trip, and anything that is not shown to have cleared
// goes through releaseTenantConn: its own clear, or the connection discarded. This pins that logic against
// a recording driver, with no database, so it runs on every job. What the SQL Server driver does with a
// trailing statement is pinned against a real server in cmd/cleat-worker
// (TestATrailingStatementsErrorSurfacesThroughScan, TestAScopedReadsTenantDoesNotSurviveTheSQLServerConnectionPool).

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// recordingDriver logs every statement a connection sees, in order, and fails the ones the test marks.
// A query containing "ROWS" returns one row and one containing "FAIL" fails; any other returns none.
type recordingDriver struct {
	mu          sync.Mutex
	log         []string
	closes      int
	refuseClear bool
}

func (d *recordingDriver) note(s string) {
	d.mu.Lock()
	d.log = append(d.log, s)
	d.mu.Unlock()
}

func (d *recordingDriver) snapshot() ([]string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.log...), d.closes
}

func (d *recordingDriver) Connect(context.Context) (driver.Conn, error) {
	return &recordingConn{d: d}, nil
}
func (d *recordingDriver) Driver() driver.Driver { return nil }

type recordingConn struct{ d *recordingDriver }

func (c *recordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("recordingConn: no Prepare")
}
func (c *recordingConn) Close() error {
	c.d.mu.Lock()
	c.d.closes++
	c.d.mu.Unlock()
	return nil
}
func (c *recordingConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *recordingConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.d.note("begin")
	return recordingTx{c.d}, nil
}

type recordingTx struct{ d *recordingDriver }

func (t recordingTx) Commit() error   { t.d.note("commit"); return nil }
func (t recordingTx) Rollback() error { t.d.note("rollback"); return nil }

func (c *recordingConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.d.note("exec: " + q)
	if c.d.refuseClear && strings.Contains(q, "@value = NULL") {
		return nil, errors.New("clear refused (test)")
	}
	return driver.RowsAffected(0), nil
}

func (c *recordingConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.d.note("query: " + q)
	if strings.Contains(q, "FAIL") {
		return nil, errors.New("read failed (test)")
	}
	return &recordingRows{one: strings.Contains(q, "ROWS")}, nil
}

type recordingRows struct {
	one  bool
	done bool
}

func (r *recordingRows) Columns() []string { return []string{"x"} }
func (r *recordingRows) Close() error      { return nil }
func (r *recordingRows) Next(dest []driver.Value) error {
	if !r.one || r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(1)
	return nil
}

func scopedReadOn(t *testing.T, d *recordingDriver, fn func(q querier) error) error {
	t.Helper()
	db := sql.OpenDB(d)
	t.Cleanup(func() { _ = db.Close() })
	ts, err := NewTenantStoreForDialect(db, DialectMSSQL)
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	return ts.scopedRead(context.Background(), uuid.New(), fn)
}

// separateClears counts the clears that went out as a statement of their own.
func separateClears(log []string) int {
	n := 0
	for _, l := range log {
		if strings.HasPrefix(l, "exec: ") && strings.Contains(l, "@value = NULL") {
			n++
		}
	}
	return n
}

func TestAScopedReadOnSQLServerCarriesItsClearInItsOwnBatch(t *testing.T) {
	const marker = "SELECT 1 /* ROWS */"
	var one int
	scan := func(q querier) error { return q.QueryRowContext(context.Background(), marker).Scan(&one) }

	t.Run("a read that returns a row sends one batch ending in the clear and no clear of its own", func(t *testing.T) {
		d := &recordingDriver{}
		if err := scopedReadOn(t, d, scan); err != nil {
			t.Fatalf("scoped read: %v", err)
		}
		log, closes := d.snapshot()
		var queries []string
		for _, l := range log {
			if strings.HasPrefix(l, "query: ") {
				queries = append(queries, l)
			}
		}
		if len(queries) != 1 || !strings.HasPrefix(queries[0], "query: "+marker+";") || !strings.HasSuffix(queries[0], clearTenantStmt) {
			t.Errorf("want exactly one batch, the read followed by the clear; got %q", queries)
		}
		if n := separateClears(log); n != 0 {
			t.Errorf("a clear went out on its own round trip although the batch carried it: %q", log)
		}
		if closes != 0 {
			t.Errorf("the connection was discarded after a clean read (%d closes)", closes)
		}
	})

	t.Run("a read that finds no row returns sql.ErrNoRows and still counts as cleared", func(t *testing.T) {
		d := &recordingDriver{}
		err := scopedReadOn(t, d, func(q querier) error {
			return q.QueryRowContext(context.Background(), "SELECT 1 WHERE 1=0").Scan(&one)
		})
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("want sql.ErrNoRows, got %v", err)
		}
		log, closes := d.snapshot()
		if n := separateClears(log); n != 0 || closes != 0 {
			t.Errorf("a no-row read must be cleared by its own batch (separate clears %d, closes %d): %q", n, closes, log)
		}
	})

	t.Run("a read that fails is followed by a clear of its own", func(t *testing.T) {
		d := &recordingDriver{}
		err := scopedReadOn(t, d, func(q querier) error {
			return q.QueryRowContext(context.Background(), "SELECT FAIL").Scan(&one)
		})
		if err == nil || !strings.Contains(err.Error(), "read failed (test)") {
			t.Fatalf("want the read's error, got %v", err)
		}
		log, closes := d.snapshot()
		if n := separateClears(log); n != 1 || closes != 0 {
			t.Errorf("a failed read is not shown to have cleared, so it needs a clear of its own (separate clears %d, closes %d): %q", n, closes, log)
		}
	})

	t.Run("a read that failed and whose clear is refused discards the connection", func(t *testing.T) {
		d := &recordingDriver{refuseClear: true}
		_ = scopedReadOn(t, d, func(q querier) error {
			return q.QueryRowContext(context.Background(), "SELECT FAIL").Scan(&one)
		})
		if _, closes := d.snapshot(); closes != 1 {
			t.Errorf("the connection must be closed, not pooled, when its clear is refused (closes %d)", closes)
		}
	})

	t.Run("a read that runs no statement is not shown to have cleared", func(t *testing.T) {
		d := &recordingDriver{}
		if err := scopedReadOn(t, d, func(querier) error { return nil }); err != nil {
			t.Fatalf("scoped read: %v", err)
		}
		if log, _ := d.snapshot(); separateClears(log) != 1 {
			t.Errorf("nothing carried the clear, so it must go out on its own: %q", log)
		}
	})

	t.Run("a second statement is refused and never reaches the server", func(t *testing.T) {
		d := &recordingDriver{}
		var second error
		err := scopedReadOn(t, d, func(q querier) error {
			if err := q.QueryRowContext(context.Background(), marker).Scan(&one); err != nil {
				return err
			}
			second = q.QueryRowContext(context.Background(), marker).Scan(&one)
			return second
		})
		if second == nil || !strings.Contains(second.Error(), "one statement") {
			t.Fatalf("the second statement must be refused with a message naming the rule, got %v", second)
		}
		if err == nil {
			t.Error("the refusal must reach the caller")
		}
		log, _ := d.snapshot()
		n := 0
		for _, l := range log {
			if strings.HasPrefix(l, "query: ") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("want one batch on the wire, got %d: %q", n, log)
		}
	})
}
