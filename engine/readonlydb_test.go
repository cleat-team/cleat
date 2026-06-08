package engine

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

func TestReadOnlyDB_ExecDenied(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	_, err := r.Exec(context.Background(), "INSERT INTO t VALUES (1)")
	if err == nil {
		t.Fatal("expected error from Exec")
	}
	if !strings.Contains(err.Error(), "Exec denied") {
		t.Errorf("expected 'Exec denied' in error, got: %v", err)
	}
}

func TestReadOnlyDB_Ping(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	err := r.Ping(context.Background())
	if err != nil {
		t.Errorf("Ping should succeed with noop driver: %v", err)
	}
}

func TestReadOnlyDB_BeginSuccess(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if tx == nil {
		t.Fatal("expected non-nil tx")
	}
	_ = tx.Rollback()
}

func TestReadOnlyDB_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	_, err := r.Begin(context.Background())
	if err == nil {
		t.Fatal("expected error from BeginTx")
	}
	if !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("expected 'begin tx' in error, got: %v", err)
	}
}

func TestReadOnlyDB_QuerySuccess(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{"hello", int64(42)}}},
	}, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	rows, err := r.Query(context.Background(), "SELECT msg, count FROM t")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected at least one row")
	}
	var msg string
	var count int64
	if err := rows.Scan(&msg, &count); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if msg != "hello" || count != 42 {
		t.Errorf("unexpected row: msg=%q, count=%d", msg, count)
	}
	if rows.Next() {
		t.Error("expected only one row")
	}
}

func TestReadOnlyDB_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	_, err := r.Query(context.Background(), "SELECT * FROM t")
	if err == nil {
		t.Fatal("expected error from Query")
	}
}

func TestReadOnlyDB_QueryRow(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{int64(99)}}},
	}, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	scanner := r.QueryRow(context.Background(), "SELECT COUNT(*) FROM t")
	if scanner == nil {
		t.Fatal("expected non-nil scanner")
	}
	var count int64
	if err := scanner.Scan(&count); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if count != 99 {
		t.Errorf("expected 99, got %d", count)
	}
}

func TestReadOnlyTx_ExecDenied(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	_, err = tx.Exec(context.Background(), "INSERT INTO t VALUES (1)")
	if err == nil {
		t.Fatal("expected error from readOnlyTx.Exec")
	}
	if !strings.Contains(err.Error(), "Exec denied") {
		t.Errorf("expected 'Exec denied' in error, got: %v", err)
	}
}

func TestReadOnlyTx_Commit(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Errorf("Commit: %v", err)
	}
}

func TestReadOnlyTx_Rollback(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Errorf("Rollback: %v", err)
	}
}

func TestSqlRowsWrapper_Delegation(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{"x"}, {"y"}}},
	}, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	rows, err := r.Query(context.Background(), "SELECT v FROM t")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()

	// Next, Scan, Err delegation
	if !rows.Next() {
		t.Fatal("expected first row")
	}
	var val string
	if err := rows.Scan(&val); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if val != "x" {
		t.Errorf("expected 'x', got %q", val)
	}

	if !rows.Next() {
		t.Fatal("expected second row")
	}
	if err := rows.Scan(&val); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if val != "y" {
		t.Errorf("expected 'y', got %q", val)
	}

	if rows.Next() {
		t.Error("expected no more rows")
	}
	if err := rows.Err(); err != nil {
		t.Errorf("Err: %v", err)
	}
}

func TestReadOnlyDB_Begin_SetTransactionError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET TRANSACTION", err: errors.New("set transaction failed")},
	})
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	_, err := r.Begin(context.Background())
	if err == nil {
		t.Fatal("expected error from SET TRANSACTION failure")
	}
	if !strings.Contains(err.Error(), "readOnlyDB set transaction read only") {
		t.Errorf("expected 'readOnlyDB set transaction read only' in error, got: %v", err)
	}
}

func TestReadOnlyTx_Query_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{int64(42)}}},
	}, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(context.Background(), "SELECT 42")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected at least one row")
	}
	var val int64
	if err := rows.Scan(&val); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if val != 42 {
		t.Errorf("expected 42, got %d", val)
	}
}

func TestReadOnlyTx_Query_Error(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", err: errors.New("tx query failed")},
	}, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	_, err = tx.Query(context.Background(), "SELECT * FROM t")
	if err == nil {
		t.Fatal("expected error from readOnlyTx.Query")
	}
}

func TestReadOnlyTx_QueryRow_Scan(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{int64(7)}}},
	}, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	tx, err := r.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	scanner := tx.QueryRow(context.Background(), "SELECT 7")
	if scanner == nil {
		t.Fatal("expected non-nil scanner")
	}
	var val int64
	if err := scanner.Scan(&val); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if val != 7 {
		t.Errorf("expected 7, got %d", val)
	}
}

func TestRowScanner_Scan(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{int64(7)}}},
	}, nil)
	defer db.Close()

	r := &ReadOnlyDB{Inner: db}
	scanner := r.QueryRow(context.Background(), "SELECT 7")
	var val int64
	if err := scanner.Scan(&val); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if val != 7 {
		t.Errorf("expected 7, got %d", val)
	}
}
