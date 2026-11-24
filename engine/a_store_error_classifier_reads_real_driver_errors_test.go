package engine

// cleat#3878. StoreErrorIsTransient decides whether the HTTP layer answers a failed store call 503 (retry) or 500. A
// classifier written against the error values its author could construct says nothing about what the three
// drivers actually return, so this provokes the real thing on each dialect: a lock wait that times out (PostgreSQL
// 55P03, MySQL 1205, SQL Server 1222) must read as transient, and a unique-key violation on the same table must
// not. The second half is the negative control: a classifier that answered "transient" for everything would pass
// the first.

import (
	"context"
	"testing"
	"time"
)

func TestTheStoreErrorClassifierReadsTheErrorsRealDriversReturn(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			db := rawDBOf(t, store)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			var setTimeout string
			switch backend.Name() {
			case "postgres":
				setTimeout = `SET lock_timeout = '300ms'`
			case "mysql":
				setTimeout = `SET SESSION innodb_lock_wait_timeout = 1`
			case "mssql":
				setTimeout = `SET LOCK_TIMEOUT 300`
			default:
				t.Fatalf("no lock-timeout statement for backend %q", backend.Name())
			}

			const table = "cleat3878_classifier_probe"
			if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
				t.Fatalf("drop: %v", err)
			}
			if _, err := db.ExecContext(ctx, `CREATE TABLE `+table+` (id INT PRIMARY KEY, v INT NOT NULL)`); err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+table)
			if _, err := db.ExecContext(ctx, `INSERT INTO `+table+` (id, v) VALUES (1, 0)`); err != nil {
				t.Fatalf("seed: %v", err)
			}

			// The negative control runs first: on MySQL and SQL Server a duplicate insert against a row another
			// transaction holds waits for that lock, and would then measure the wait and not the violation.
			_, dupErr := db.ExecContext(ctx, `INSERT INTO `+table+` (id, v) VALUES (1, 0)`)
			if dupErr == nil {
				t.Fatal("a duplicate key was accepted; the negative control measured nothing")
			}
			if StoreErrorIsTransient(dupErr) {
				t.Errorf("a unique-key violation on %s was classified transient: %T %v", backend.Name(), dupErr, dupErr)
			}

			holder, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer holder.Rollback()
			if _, err := holder.ExecContext(ctx, `UPDATE `+table+` SET v = v + 1 WHERE id = 1`); err != nil {
				t.Fatalf("hold the row: %v", err)
			}

			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("conn: %v", err)
			}
			defer conn.Close()
			if _, err := conn.ExecContext(ctx, setTimeout); err != nil {
				t.Fatalf("%s: %v", setTimeout, err)
			}
			_, lockErr := conn.ExecContext(ctx, `UPDATE `+table+` SET v = v + 1 WHERE id = 1`)
			if lockErr == nil {
				t.Fatal("the second writer was not blocked by the first; the probe measured nothing")
			}
			if !StoreErrorIsTransient(lockErr) {
				t.Errorf("a lock wait that timed out on %s was not classified transient: %T %v", backend.Name(), lockErr, lockErr)
			}

		})
	}
}
