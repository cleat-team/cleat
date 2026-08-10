package engine

// Unit-level (no real database) coverage for the CLEAT-1.2 fix: the
// generation-fenced workflow lifecycle methods (CompleteWorkflow,
// FailWorkflow, MoveToDeadLetterQueue, ContinueAsNew) must inspect
// RowsAffected() on their fencing UPDATE and skip all post-commit cleanup
// (ClearStickyWorker, ReleaseWorkflowConcurrencyKeys,
// enforceParentClosePolicy) plus the in-transaction idempotency-key write
// when the fence did not hold (RowsAffected == 0), returning ErrFenceLost
// instead.
//
// This is exercised here against a minimal fake database/sql/driver rather
// than a real Postgres instance so it runs unconditionally in `go test
// ./engine/...` (no CLEAT_TEST_DB required) and pins down the Go-side
// behavior in isolation from the SQL stored procedure fix (see
// fence_lost_integration_test.go for the real-database reproduction of the
// full zombie-writer scenario, which does require a database).

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// Fake driver: records every statement executed against it and returns a
// caller-controlled RowsAffected for whichever statement looks like the
// primary generation-fenced UPDATE (identified by referencing both
// "assigned_to =" and "generation =" on workflow_instances -- the one
// pattern shared by CompleteWorkflow / FailWorkflow /
// MoveToDeadLetterQueue / ContinueAsNew's fencing UPDATE, and by no other
// statement those methods issue). All other statements succeed as a 1-row
// no-op so the surrounding transaction machinery behaves normally.
// ---------------------------------------------------------------------

type fenceFakeState struct {
	mu                 sync.Mutex
	calls              []string
	fencedRowsAffected int64
	committed          int
	rolledBack         int
}

func (s *fenceFakeState) record(query string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, query)
}

func (s *fenceFakeState) callLog() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.calls, "\n---\n")
}

func (s *fenceFakeState) counts() (committed, rolledBack int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.committed, s.rolledBack
}

func isFencedWorkflowUpdate(query string) bool {
	return strings.Contains(query, "workflow_instances") &&
		strings.Contains(query, "assigned_to =") &&
		strings.Contains(query, "generation =")
}

type fenceFakeResult struct{ rowsAffected int64 }

func (r fenceFakeResult) LastInsertId() (int64, error) { return 0, nil }
func (r fenceFakeResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

type fenceFakeTx struct{ state *fenceFakeState }

func (t *fenceFakeTx) Commit() error {
	t.state.mu.Lock()
	t.state.committed++
	t.state.mu.Unlock()
	return nil
}

func (t *fenceFakeTx) Rollback() error {
	t.state.mu.Lock()
	t.state.rolledBack++
	t.state.mu.Unlock()
	return nil
}

type fenceFakeConn struct{ state *fenceFakeState }

func (c *fenceFakeConn) Prepare(query string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fenceFakeConn: Prepare not supported by this fake driver (query=%q)", query)
}

func (c *fenceFakeConn) Close() error { return nil }

func (c *fenceFakeConn) Begin() (driver.Tx, error) {
	return &fenceFakeTx{state: c.state}, nil
}

// ExecContext handles every Exec call directly (bypassing Prepare/Stmt),
// which is what database/sql does automatically when the Conn implements
// driver.ExecerContext.
func (c *fenceFakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.state.record(query)
	rows := int64(1)
	if isFencedWorkflowUpdate(query) {
		rows = c.state.fencedRowsAffected
	}
	return fenceFakeResult{rowsAffected: rows}, nil
}

// QueryContext handles the one QueryRowContext call these four methods (and
// their nested calls) issue: ContinueAsNew's "INSERT ... RETURNING id" for
// the new run. database/sql uses this directly (bypassing Prepare/Stmt)
// because fenceFakeConn implements driver.QueryerContext.
func (c *fenceFakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.state.record(query)
	return &fenceFakeRows{cols: []string{"id"}, val: "fake-new-run-id"}, nil
}

// fenceFakeRows is a single-row, single-column driver.Rows.
type fenceFakeRows struct {
	cols []string
	val  driver.Value
	done bool
}

func (r *fenceFakeRows) Columns() []string { return r.cols }
func (r *fenceFakeRows) Close() error      { return nil }
func (r *fenceFakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.val
	return nil
}

type fenceFakeDriver struct{}

var (
	fenceFakeRegistryMu sync.Mutex
	fenceFakeRegistry   = map[string]*fenceFakeState{}
	fenceFakeDriverOnce sync.Once
)

func (fenceFakeDriver) Open(name string) (driver.Conn, error) {
	fenceFakeRegistryMu.Lock()
	st, ok := fenceFakeRegistry[name]
	fenceFakeRegistryMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fenceFakeDriver: no state registered for %q", name)
	}
	return &fenceFakeConn{state: st}, nil
}

// newFenceFakeDB opens a *sql.DB backed by the fake driver above, with the
// fencing UPDATE configured to report fencedRowsAffected rows affected.
func newFenceFakeDB(t *testing.T, fencedRowsAffected int64) (*sql.DB, *fenceFakeState) {
	t.Helper()
	fenceFakeDriverOnce.Do(func() {
		sql.Register("cleat-fence-fake", fenceFakeDriver{})
	})

	name := fmt.Sprintf("fence-%d-%s", time.Now().UnixNano(), t.Name())
	st := &fenceFakeState{fencedRowsAffected: fencedRowsAffected}
	fenceFakeRegistryMu.Lock()
	fenceFakeRegistry[name] = st
	fenceFakeRegistryMu.Unlock()

	db, err := sql.Open("cleat-fence-fake", name)
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		fenceFakeRegistryMu.Lock()
		delete(fenceFakeRegistry, name)
		fenceFakeRegistryMu.Unlock()
	})
	return db, st
}

// cleanupRan reports whether any of the post-commit best-effort cleanup
// statements (ClearStickyWorker, ReleaseWorkflowConcurrencyKeys,
// enforceParentClosePolicy) or the in-transaction idempotency-key write
// appear in the call log.
func cleanupRan(log string) bool {
	markers := []string{
		"sticky_worker_id = NULL",       // ClearStickyWorker
		"DELETE FROM concurrency_keys",  // ReleaseWorkflowConcurrencyKeys
		"parent workflow terminated",    // enforceParentClosePolicy (TERMINATE)
		"cancellation_requested = true", // enforceParentClosePolicy (REQUEST_CANCEL)
		"idempotency_keys",              // in-tx idempotency write
	}
	for _, m := range markers {
		if strings.Contains(log, m) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------

func TestFenceLost_CompleteWorkflow(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rowsAffected int64
	}{
		{"fence held", 1},
		{"fence lost", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, st := newFenceFakeDB(t, tc.rowsAffected)
			store := NewPostgresStore(db)

			err := store.CompleteWorkflow(context.Background(), "wf-1", "worker-1", 5, `{"ok":true}`, nil)

			assertFenceOutcome(t, tc.rowsAffected, err, st)
		})
	}
}

func TestFenceLost_FailWorkflow(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rowsAffected int64
	}{
		{"fence held", 1},
		{"fence lost", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, st := newFenceFakeDB(t, tc.rowsAffected)
			store := NewPostgresStore(db)

			err := store.FailWorkflow(context.Background(), "wf-1", "worker-1", 5, "boom", "unknown", "op", nil)

			assertFenceOutcome(t, tc.rowsAffected, err, st)
		})
	}
}

func TestFenceLost_MoveToDeadLetterQueue(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rowsAffected int64
	}{
		{"fence held", 1},
		{"fence lost", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, st := newFenceFakeDB(t, tc.rowsAffected)
			store := NewPostgresStore(db)

			err := store.MoveToDeadLetterQueue(context.Background(), "wf-1", "worker-1", 5, "boom", "unknown", "op")

			assertFenceOutcome(t, tc.rowsAffected, err, st)
		})
	}
}

func TestFenceLost_ContinueAsNew(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rowsAffected int64
	}{
		{"fence held", 1},
		{"fence lost", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, st := newFenceFakeDB(t, tc.rowsAffected)
			store := NewPostgresStore(db)

			newRunID, err := store.ContinueAsNew(context.Background(), "wf-1", "worker-1", 5,
				"test-workflow", 1, json.RawMessage(`{}`), nil, `{"ok":true}`, nil, 0)

			assertFenceOutcome(t, tc.rowsAffected, err, st)
			if tc.rowsAffected == 0 && newRunID != "" {
				t.Errorf("ContinueAsNew returned a new run ID %q despite a lost fence", newRunID)
			}
		})
	}
}

// assertFenceOutcome centralizes the shared assertions across the four
// fenced methods: on a held fence, the method must succeed, commit, and run
// its post-commit cleanup; on a lost fence, it must return ErrFenceLost,
// never commit, and never touch any cleanup/idempotency statement.
func assertFenceOutcome(t *testing.T, rowsAffected int64, err error, st *fenceFakeState) {
	t.Helper()
	log := st.callLog()
	committed, rolledBack := st.counts()

	if rowsAffected == 0 {
		if !errors.Is(err, ErrFenceLost) {
			t.Fatalf("error = %v, want ErrFenceLost", err)
		}
		if cleanupRan(log) {
			t.Errorf("cleanup/idempotency statement ran despite a lost fence; call log:\n%s", log)
		}
		if committed != 0 {
			t.Errorf("transaction was committed despite a lost fence (committed=%d, rolledBack=%d)", committed, rolledBack)
		}
		return
	}

	if err != nil {
		t.Fatalf("unexpected error with a held fence: %v", err)
	}
	if !cleanupRan(log) {
		t.Errorf("cleanup did not run despite a held fence; call log:\n%s", log)
	}
	if committed == 0 {
		t.Errorf("transaction was not committed despite a held fence")
	}
}
