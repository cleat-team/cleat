package engine

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
)

func TestSQLDBAdapter_Exec(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT", affected: 3},
	})
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	affected, err := a.Exec(context.Background(), "INSERT INTO t VALUES (1)")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if affected != 3 {
		t.Errorf("expected affected=3, got %d", affected)
	}
}

func TestSQLDBAdapter_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT", err: errors.New("exec failed")},
	})
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	_, err := a.Exec(context.Background(), "INSERT INTO t VALUES (1)")
	if err == nil {
		t.Fatal("expected error from Exec")
	}
}

func TestSQLDBAdapter_QuerySuccess(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{"val1"}, {"val2"}}},
	}, nil)
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	rows, err := a.Query(context.Background(), "SELECT v FROM t")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected at least one row")
	}
	var v string
	if err := rows.Scan(&v); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if v != "val1" {
		t.Errorf("expected 'val1', got %q", v)
	}
}

func TestSQLDBAdapter_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	_, err := a.Query(context.Background(), "SELECT * FROM t")
	if err == nil {
		t.Fatal("expected error from Query")
	}
}

func TestSQLDBAdapter_QueryRow(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{int64(42)}}},
	}, nil)
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	scanner := a.QueryRow(context.Background(), "SELECT 42")
	if scanner == nil {
		t.Fatal("expected non-nil scanner")
	}
	var val int64
	if err := scanner.Scan(&val); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if val != 42 {
		t.Errorf("expected 42, got %d", val)
	}
}

func TestSQLDBAdapter_Ping(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	if err := a.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestSQLDBAdapter_BeginSuccess(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if tx == nil {
		t.Fatal("expected non-nil tx")
	}
	_ = tx.Rollback()
}

func TestSQLDBAdapter_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	_, err := a.Begin(context.Background())
	if err == nil {
		t.Fatal("expected error from Begin")
	}
}

func TestSqlTxAdapter_Exec(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE", affected: 5},
	})
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	affected, err := tx.Exec(context.Background(), "UPDATE t SET x=1")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if affected != 5 {
		t.Errorf("expected affected=5, got %d", affected)
	}
}

func TestSqlTxAdapter_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE", err: errors.New("exec failed")},
	})
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	_, err = tx.Exec(context.Background(), "UPDATE t SET x=1")
	if err == nil {
		t.Fatal("expected error from Exec")
	}
}

func TestSqlTxAdapter_Commit(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Errorf("Commit: %v", err)
	}
}

func TestSqlTxAdapter_Rollback(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Errorf("Rollback: %v", err)
	}
}

func TestSqlTxAdapter_Query_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{"from-tx"}}},
	}, nil)
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(context.Background(), "SELECT v FROM t")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected at least one row")
	}
	var v string
	if err := rows.Scan(&v); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if v != "from-tx" {
		t.Errorf("expected 'from-tx', got %q", v)
	}
}

func TestSqlTxAdapter_Query_Error(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", err: errors.New("tx query failed")},
	}, nil)
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	_, err = tx.Query(context.Background(), "SELECT * FROM t")
	if err == nil {
		t.Fatal("expected error from sqlTxAdapter.Query")
	}
}

func TestSqlTxAdapter_QueryRow_Scan(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{int64(77)}}},
	}, nil)
	defer db.Close()

	a := &SQLDBAdapter{DB: db}
	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	scanner := tx.QueryRow(context.Background(), "SELECT 77")
	if scanner == nil {
		t.Fatal("expected non-nil scanner")
	}
	var val int64
	if err := scanner.Scan(&val); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if val != 77 {
		t.Errorf("expected 77, got %d", val)
	}
}
