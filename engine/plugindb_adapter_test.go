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
	a := &SQLDBAdapter{DB: db}

	n, err := a.Exec(context.Background(), "INSERT INTO t VALUES (1)")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("RowsAffected = %d, want 3", n)
	}
}

func TestSQLDBAdapter_ExecError(t *testing.T) {
	execErr := errors.New("disk full")
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT", err: execErr},
	})
	a := &SQLDBAdapter{DB: db}

	_, err := a.Exec(context.Background(), "INSERT INTO t VALUES (1)")
	if err == nil {
		t.Fatal("expected error from Exec")
	}
}

func TestSQLDBAdapter_QuerySuccess(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{"a"}, {"b"}, {"c"}}},
	}, nil)
	a := &SQLDBAdapter{DB: db}

	rows, err := a.Query(context.Background(), "SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var val string
		if err := rows.Scan(&val); err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 3 {
		t.Errorf("got %d rows, want 3", count)
	}
}

func TestSQLDBAdapter_QueryError(t *testing.T) {
	queryErr := errors.New("timeout")
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", err: queryErr},
	}, nil)
	a := &SQLDBAdapter{DB: db}

	_, err := a.Query(context.Background(), "SELECT * FROM t")
	if err == nil {
		t.Fatal("expected error from Query")
	}
}

func TestSQLDBAdapter_QueryRow(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{int64(7)}}},
	}, nil)
	a := &SQLDBAdapter{DB: db}

	scanner := a.QueryRow(context.Background(), "SELECT 7")
	var val int64
	if err := scanner.Scan(&val); err != nil {
		t.Fatal(err)
	}
	if val != 7 {
		t.Errorf("got %d, want 7", val)
	}
}

func TestSQLDBAdapter_Ping(t *testing.T) {
	db := newNoopDB(t)
	a := &SQLDBAdapter{DB: db}

	if err := a.Ping(context.Background()); err != nil {
		t.Errorf("unexpected ping error: %v", err)
	}
}

func TestSQLDBAdapter_BeginSuccess(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	a := &SQLDBAdapter{DB: db}

	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	if tx == nil {
		t.Fatal("expected non-nil transaction")
	}
	tx.Rollback()
}

func TestSQLDBAdapter_BeginError(t *testing.T) {
	beginErr := errors.New("cannot connect")
	db := newMockDBWithErrors(t, nil, nil, beginErr, nil)
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
	a := &SQLDBAdapter{DB: db}

	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	n, err := tx.Exec(context.Background(), "UPDATE t SET x=1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("RowsAffected = %d, want 5", n)
	}
}

func TestSqlTxAdapter_ExecError(t *testing.T) {
	execErr := errors.New("constraint violation")
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE", err: execErr},
	})
	a := &SQLDBAdapter{DB: db}

	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	_, err = tx.Exec(context.Background(), "UPDATE t SET x=1")
	if err == nil {
		t.Fatal("expected error from Exec")
	}
}

func TestSqlTxAdapter_Commit(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	a := &SQLDBAdapter{DB: db}

	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Errorf("Commit failed: %v", err)
	}
}

func TestSqlTxAdapter_Rollback(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	a := &SQLDBAdapter{DB: db}

	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Errorf("Rollback failed: %v", err)
	}
}

func TestSqlTxAdapter_QuerySuccess(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{"tx-row"}}},
	}, nil)
	a := &SQLDBAdapter{DB: db}

	tx, err := a.Begin(context.Background())
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
	if val != "tx-row" {
		t.Errorf("got %q, want 'tx-row'", val)
	}
}

func TestSqlTxAdapter_QueryRow(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", data: [][]driver.Value{{int64(77)}}},
	}, nil)
	a := &SQLDBAdapter{DB: db}

	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	scanner := tx.QueryRow(context.Background(), "SELECT 77")
	var val int64
	if err := scanner.Scan(&val); err != nil {
		t.Fatal(err)
	}
	if val != 77 {
		t.Errorf("got %d, want 77", val)
	}
}

func TestSqlTxAdapter_Query_Error(t *testing.T) {
	queryErr := errors.New("tx query failed")
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT", err: queryErr},
	}, nil)
	a := &SQLDBAdapter{DB: db}

	tx, err := a.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	_, err = tx.Query(context.Background(), "SELECT * FROM t")
	if err == nil {
		t.Fatal("expected error from tx.Query")
	}
}
