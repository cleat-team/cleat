package main

import (
	"strings"
	"testing"
)

// Regression test for B6: a non-PostgreSQL DSN handed to a cmd/cleat
// subcommand must fail with an actionable message naming the dialect and the
// alternative (cmd/deploy-workflow), not whatever lib/pq happens to make of a
// string it was never built to parse.
//
// Messages measured against this binary before the openPostgresDB guard
// existed (2026-08-09):
//
//	MySQL DSN root:pass@tcp(127.0.0.1:3306)/cleat:
//	  `missing "=" after "root:pass@tcp(127.0.0.1:3306)/cleat" in connection info string`
//	SQL Server DSN sqlserver://sa:Password1@localhost:1433?database=cleat:
//	  `pq: SSL is not enabled on the server`
//
// Neither message says anything about dialect, and the second is actively
// misleading -- lib/pq fell through to a default host/port and reported an
// unrelated SSL problem. This test does not need a database: openPostgresDB
// rejects the wrong-dialect cases before ever calling sql.Open, and sql.Open
// itself never dials (database/sql connects lazily), so the accepted cases
// return a non-nil *sql.DB with no network access.
func TestOpenPostgresDBRejectsNonPostgresDialects(t *testing.T) {
	cases := []struct {
		name    string
		connStr string
		wantErr bool
		wantSub []string // required substrings of the error message
	}{
		{
			name:    "mysql URL scheme",
			connStr: "mysql://root:pass@localhost:3306/cleat",
			wantErr: true,
			wantSub: []string{"MySQL", "deploy-workflow --driver mysql"},
		},
		{
			name:    "mysql driver DSN form (user:pass@tcp(host:port)/db)",
			connStr: "root:pass@tcp(127.0.0.1:3306)/cleat",
			wantErr: true,
			wantSub: []string{"MySQL", "deploy-workflow --driver mysql"},
		},
		{
			name:    "sqlserver URL scheme",
			connStr: "sqlserver://sa:Password1@localhost:1433?database=cleat",
			wantErr: true,
			wantSub: []string{"SQL Server", "deploy-workflow --driver mssql"},
		},
		{
			name:    "mssql URL scheme",
			connStr: "mssql://sa:Password1@localhost:1433?database=cleat",
			wantErr: true,
			wantSub: []string{"SQL Server", "deploy-workflow --driver mssql"},
		},
		{
			name:    "jdbc sqlserver URL",
			connStr: "jdbc:sqlserver://localhost:1433;databaseName=cleat",
			wantErr: true,
			wantSub: []string{"SQL Server"},
		},
		{
			name:    "postgres URL scheme is accepted (not rejected here)",
			connStr: "postgres://user:pass@localhost:5432/cleat?sslmode=disable",
			wantErr: false,
		},
		{
			name:    "postgres key=value DSN is accepted (not rejected here)",
			connStr: "host=localhost user=cleat dbname=cleat sslmode=disable",
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := openPostgresDB(tc.connStr)
			if tc.wantErr {
				if err == nil {
					if db != nil {
						db.Close()
					}
					t.Fatalf("openPostgresDB(%q): expected an error, got none", tc.connStr)
				}
				for _, sub := range tc.wantSub {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("openPostgresDB(%q) error = %q, want it to contain %q", tc.connStr, err.Error(), sub)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("openPostgresDB(%q): unexpected error: %v", tc.connStr, err)
			}
			if db != nil {
				db.Close()
			}
		})
	}
}
