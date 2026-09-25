package pinnedtx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// A driver that does nothing and takes no session-reset hooks, which is what
// makes database/sql discard the connection when a transaction's context ends
// (Tx.keepConnOnRollback is false): the same shape as the pinned PostgreSQL
// connection in the migration runners.
type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{}, nil }

type fakeConn struct{}

func (*fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (*fakeConn) Close() error                        { return nil }
func (*fakeConn) Begin() (driver.Tx, error)           { return fakeTx{}, nil }
func (*fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return fakeTx{}, nil
}
func (*fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.ResultNoRows, nil
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

var driverSeq atomic.Int64

func openFake(t *testing.T) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("pinnedtx-fake-%d", driverSeq.Add(1))
	sql.Register(name, fakeDriver{})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// settle gives Tx.awaitDone, which runs on its own goroutine, ample time to act
// on a cancelled context. It is a negative check, so a longer wait only makes
// it stricter; the CONTROL below is what shows the wait is long enough for the
// goroutine to have done its work.
const settle = 50 * time.Millisecond

func usable(tx *sql.Tx) bool {
	_, err := tx.ExecContext(context.Background(), "SELECT 1")
	return err == nil
}

// The property the fix is for: on a pinned connection the run's context does
// not end the transaction behind the caller's back. The control arm begins the
// same transaction the old way and shows it IS ended -- without it, "still
// usable after the wait" would be true of a wait that was too short to matter.
func TestTheRunContextDoesNotEndATransactionOnAPinnedConnection(t *testing.T) {
	db := openFake(t)
	ctx := context.Background()

	t.Run("control: BeginTx(ctx) is ended by ctx", func(t *testing.T) {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		runCtx, cancel := context.WithCancel(ctx)
		tx, err := conn.BeginTx(runCtx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !usable(tx) {
			t.Fatal("PRECONDITION FAILED: a fresh transaction is not usable")
		}
		cancel()
		time.Sleep(settle)
		if usable(tx) {
			t.Fatal("the control is not a control: a transaction begun on a cancelled context is still usable, " +
				"so the assertion below could pass without the fix")
		}
	})

	t.Run("Begin on a pinned connection survives ctx", func(t *testing.T) {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		runCtx, cancel := context.WithCancel(ctx)
		tx, err := Begin(runCtx, conn, nil)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		time.Sleep(settle)
		if !usable(tx) {
			t.Fatal("the run's context ended the transaction asynchronously: database/sql would now be " +
				"closing the pinned connection from its own goroutine (cleat#2215)")
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("the caller's own Rollback must end it cleanly: %v", err)
		}
	})
}

// A context that is already done still refuses, as it did before.
func TestBeginRefusesAnAlreadyCancelledContext(t *testing.T) {
	db := openFake(t)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if tx, err := Begin(ctx, conn, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Begin on a cancelled context: tx=%v err=%v, want context.Canceled", tx, err)
	}
}

// Anything that is not a pinned *sql.Conn keeps its behaviour: ctx still ends
// the transaction, because there is no Conn to close underneath a caller.
func TestBeginLeavesAPooledSessionBoundToItsContext(t *testing.T) {
	db := openFake(t)
	runCtx, cancel := context.WithCancel(context.Background())
	tx, err := Begin(runCtx, db, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(settle)
	if usable(tx) {
		t.Fatal("a transaction on the pool outlived its cancelled context")
	}
}
