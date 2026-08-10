package engine

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

func TestReadOnlyDB_ExecDenied(t *testing.T) {
	db := newNoopDB(t)
	r := &ReadOnlyDB{Inner: db}

	_, err := r.Exec(context.Background(), "INSERT INTO t VALUES (1)")
	if err == nil {
		t.Fatal("expected error from Exec on read-only DB")
	}
	if err.Error() != "read-only: Exec denied" {
		t.Errorf("got %q, want 'read-only: Exec denied'", err.Error())
	}
}

func TestReadOnlyDB_QuerySuccess(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{"hello"}, {"world"}}},
	}, nil)
	r := &ReadOnlyDB{Inner: db}

	rows, err := r.Query(context.Background(), "SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	// Verify rows is a sqlRowsWrapper by checking it implements plugin.Rows
	var count int
	for rows.Next() {
		var val string
		if err := rows.Scan(&val); err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 2 {
		t.Errorf("got %d rows, want 2", count)
	}
}

func TestReadOnlyDB_QueryError(t *testing.T) {
	queryErr := errors.New("connection refused")
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", err: queryErr},
	}, nil)
	r := &ReadOnlyDB{Inner: db}

	_, err := r.Query(context.Background(), "SELECT * FROM t")
	if err == nil {
		t.Fatal("expected error from Query")
	}
}

func TestReadOnlyDB_QueryRow(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{int64(42)}}},
	}, nil)
	r := &ReadOnlyDB{Inner: db}

	scanner := r.QueryRow(context.Background(), "SELECT 42")
	var val int64
	if err := scanner.Scan(&val); err != nil {
		t.Fatal(err)
	}
	if val != 42 {
		t.Errorf("got %d, want 42", val)
	}
}

func TestReadOnlyDB_Ping(t *testing.T) {
	db := newNoopDB(t)
	r := &ReadOnlyDB{Inner: db}

	if err := r.Ping(context.Background()); err != nil {
		t.Errorf("unexpected ping error: %v", err)
	}
}

func TestReadOnlyDB_BeginSuccess(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	r := &ReadOnlyDB{Inner: db}

	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	if tx == nil {
		t.Fatal("expected non-nil transaction")
	}
	// readOnlyTx should satisfy plugin.PluginTx
	tx.Rollback()
}

func TestReadOnlyDB_BeginError(t *testing.T) {
	beginErr := errors.New("db down")
	db := newMockDBWithErrors(t, nil, nil, beginErr, nil)
	r := &ReadOnlyDB{Inner: db}

	_, err := r.Begin(context.Background())
	if err == nil {
		t.Fatal("expected error from Begin")
	}
}

func TestReadOnlyDB_Begin_SetTransactionError(t *testing.T) {
	setTxErr := errors.New("read-only mode not available")
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET TRANSACTION", err: setTxErr},
	})
	r := &ReadOnlyDB{Inner: db}

	_, err := r.Begin(context.Background())
	if err == nil {
		t.Fatal("expected error from SET TRANSACTION failure")
	}
	if !strings.Contains(err.Error(), "readOnlyDB set transaction read only") {
		t.Errorf("expected wrapped error, got: %v", err)
	}
}

func TestReadOnlyTx_ExecDenied(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	r := &ReadOnlyDB{Inner: db}

	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	_, err = tx.Exec(context.Background(), "INSERT INTO t VALUES (1)")
	if err == nil {
		t.Fatal("expected error from Exec on read-only tx")
	}
	if err.Error() != "read-only: Exec denied" {
		t.Errorf("got %q, want 'read-only: Exec denied'", err.Error())
	}
}

func TestReadOnlyTx_Commit(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	r := &ReadOnlyDB{Inner: db}

	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Errorf("Commit failed: %v", err)
	}
}

func TestReadOnlyTx_Rollback(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	r := &ReadOnlyDB{Inner: db}

	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Errorf("Rollback failed: %v", err)
	}
}

func TestReadOnlyTx_QuerySuccess(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{"x"}}},
	}, nil)
	r := &ReadOnlyDB{Inner: db}

	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	rows, err := tx.Query(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected a row")
	}
	var val string
	if err := rows.Scan(&val); err != nil {
		t.Fatal(err)
	}
	if val != "x" {
		t.Errorf("got %q, want 'x'", val)
	}
}

func TestReadOnlyTx_QueryRow(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{int64(99)}}},
	}, nil)
	r := &ReadOnlyDB{Inner: db}

	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	scanner := tx.QueryRow(context.Background(), "SELECT 99")
	var val int64
	if err := scanner.Scan(&val); err != nil {
		t.Fatal(err)
	}
	if val != 99 {
		t.Errorf("got %d, want 99", val)
	}
}

func TestReadOnlyTx_Query_Error(t *testing.T) {
	queryErr := errors.New("tx read error")
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", err: queryErr},
	}, nil)
	r := &ReadOnlyDB{Inner: db}

	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	_, err = tx.Query(context.Background(), "SELECT * FROM t")
	if err == nil {
		t.Fatal("expected error from tx.Query")
	}
}

func TestSQLRowsWrapper_Err_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{"hello"}, {"world"}}},
	}, nil)
	r := &ReadOnlyDB{Inner: db}

	rows, err := r.Query(context.Background(), "SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	// Iterate through all rows.
	for rows.Next() {
		var val string
		if err := rows.Scan(&val); err != nil {
			t.Fatal(err)
		}
	}
	// Err() should return nil after successful iteration.
	if err := rows.Err(); err != nil {
		t.Errorf("expected nil error from Err(), got %v", err)
	}
}

func TestSQLRowsWrapper_Err_EmptyResult(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{}},
	}, nil)
	r := &ReadOnlyDB{Inner: db}

	rows, err := r.Query(context.Background(), "SELECT * FROM t WHERE 1=0")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	// No rows should be available.
	if rows.Next() {
		t.Error("expected no rows")
	}
	// Err() should return nil even with empty result.
	if err := rows.Err(); err != nil {
		t.Errorf("expected nil error from Err() on empty result, got %v", err)
	}
}

func TestSQLRowsWrapper_Close(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{"x"}}},
	}, nil)
	r := &ReadOnlyDB{Inner: db}

	rows, err := r.Query(context.Background(), "SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}

	if err := rows.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}
}

func TestSQLRowsWrapper_RowsTypeCheck(t *testing.T) {
	// Verify that the returned rows implement plugin.Rows.
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{"v1"}}},
	}, nil)
	r := &ReadOnlyDB{Inner: db}

	rows, err := r.Query(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	// Verify the plugin.Rows interface.
	var want plugin.Rows = rows
	if want == nil {
		t.Error("expected non-nil plugin.Rows")
	}
}

func TestSQLRowsWrapper_ScanError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{"not-an-int"}}},
	}, nil)
	r := &ReadOnlyDB{Inner: db}

	rows, err := r.Query(context.Background(), "SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	if rows.Next() {
		var val int
		if err := rows.Scan(&val); err == nil {
			t.Error("expected scan error for type mismatch")
		}
	}
}
