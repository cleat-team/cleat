// Command sqlq runs one SQL batch against SQL Server or MySQL and prints each row's columns joined by "|",
// the way `psql -At` does, so scripts/upgrade_from_v040.py can read either database as it reads
// PostgreSQL.  cleat#3735 (D79).
//
// It exists because the owner login is NOT exempt from row-level security on SQL Server: a read of a
// tenant table through `sqlcmd` returns nothing unless the session sets `tenant_id` first, and that
// has to happen on the same connection as the query.  -tenant does it.  -pre runs a statement on that
// same connection first (MySQL's GROUP_CONCAT truncates at 1024 bytes unless the session raises
// group_concat_max_len).  Neither sqlcmd nor the mysql client is promised on a CI runner; the drivers
// are already in go.mod.
//
//	go run ./scripts/upgrade-from-v040/sqlq -driver sqlserver -dsn 'sqlserver://sa:pw@127.0.0.1:1433?database=cleat' \
//	    [-tenant ID] -sql 'SELECT status, count(*) FROM workflow_instances GROUP BY status'
//	go run ./scripts/upgrade-from-v040/sqlq -driver mysql -dsn 'root:pw@tcp(127.0.0.1:3306)/cleat?parseTime=true' \
//	    -pre 'SET SESSION group_concat_max_len = 16777216' -sql 'SELECT ...'
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

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"
)

func main() {
	driver := flag.String("driver", "sqlserver", "sqlserver or mysql")
	dsn := flag.String("dsn", "", "a sqlserver:// URL, or a MySQL user:pass@tcp(host:port)/db DSN")
	tenant := flag.String("tenant", "", "SQL Server only: set SESSION_CONTEXT tenant_id to this before the batch")
	pre := flag.String("pre", "", "a statement to run on the same connection before the batch")
	query := flag.String("sql", "", "the batch to run")
	flag.Parse()
	if *dsn == "" || *query == "" || (*driver != "sqlserver" && *driver != "mysql") || (*tenant != "" && *driver != "sqlserver") {
		fmt.Fprintln(os.Stderr, "sqlq: -dsn and -sql are required; -driver is sqlserver or mysql; -tenant is SQL Server only")
		os.Exit(1)
	}
	if err := run(*driver, *dsn, *tenant, *pre, *query); err != nil {
		fmt.Fprintln(os.Stderr, "sqlq:", err)
		os.Exit(1)
	}
}

func run(driver, dsn, tenant, pre, query string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := sql.Open(driver, dsn)
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
	if pre != "" {
		if _, err := conn.ExecContext(ctx, pre); err != nil {
			return fmt.Errorf("-pre: %w", err)
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
