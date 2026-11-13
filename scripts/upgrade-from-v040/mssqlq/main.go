// Command mssqlq runs one SQL batch against SQL Server and prints each row's columns joined by "|",
// the way `psql -At` does, so scripts/upgrade_from_v040.py can read a SQL Server database as it reads
// PostgreSQL.  cleat#3735 (D79).
//
// It exists because the owner login is NOT exempt from row-level security on SQL Server: a read of a
// tenant table through `sqlcmd` returns nothing unless the session sets `tenant_id` first, and that
// has to happen on the same connection as the query.  -tenant does it.  sqlcmd would also have to be
// installed on the CI runner; the driver is already in go.mod.
//
//	go run ./scripts/upgrade-from-v040/mssqlq -dsn 'sqlserver://sa:pw@127.0.0.1:1433?database=cleat' \
//	    [-tenant ID] -sql 'SELECT status, count(*) FROM workflow_instances GROUP BY status'
//
// Exit status: 0 the batch ran, 1 it did not (the error is on stderr).  Rows come from the batch's
// FIRST result set; a statement that returns none (DDL, or DML without OUTPUT) prints nothing.  A later
// statement that fails in the batch is an error too.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

func main() {
	dsn := flag.String("dsn", "", "sqlserver:// URL")
	tenant := flag.String("tenant", "", "set SESSION_CONTEXT tenant_id to this before the batch")
	query := flag.String("sql", "", "the batch to run")
	flag.Parse()
	if *dsn == "" || *query == "" {
		fmt.Fprintln(os.Stderr, "mssqlq: -dsn and -sql are required")
		os.Exit(1)
	}
	if err := run(*dsn, *tenant, *query); err != nil {
		fmt.Fprintln(os.Stderr, "mssqlq:", err)
		os.Exit(1)
	}
}

func run(dsn, tenant, query string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	// One connection: SESSION_CONTEXT belongs to it, and a pool could run the batch on another.
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if tenant != "" {
		if _, err := conn.ExecContext(ctx, "EXEC sp_set_session_context @key = N'tenant_id', @value = @p1",
			sql.Named("p1", tenant)); err != nil {
			return fmt.Errorf("setting tenant_id: %w", err)
		}
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	// Only the first result set is printed, but every one is read: a batch that fails in a LATER statement
	// ("SELECT 1; SELECT 1/0") reports that error through NextResultSet or Err, and stopping after the first
	// result set would exit 0 on it.
	first := true
	for {
		for rows.Next() {
			if !first {
				continue
			}
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			parts := make([]string, len(cols))
			for i, v := range vals {
				switch x := v.(type) {
				case nil:
					parts[i] = ""
				case []byte:
					parts[i] = string(x)
				default:
					parts[i] = fmt.Sprint(x)
				}
			}
			fmt.Println(strings.Join(parts, "|"))
		}
		if err := rows.Err(); err != nil {
			return err
		}
		first = false
		if !rows.NextResultSet() {
			break
		}
		cols, err = rows.Columns()
		if err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}
