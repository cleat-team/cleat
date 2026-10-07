package engine

import (
	"context"
	"fmt"
	"testing"
)

// ---------------------------------------------------------------------------
// isMSSQLDeadlock
// ---------------------------------------------------------------------------

func TestIsMSSQLDeadlock(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"deadlock keyword", fmt.Errorf("transaction was deadlocked"), true},
		{"deadlock victim phrase", fmt.Errorf("transaction was chosen as the deadlock victim"), true},
		{"error 1205 deadlock", fmt.Errorf("Transaction was deadlocked on lock resources with another process and has been chosen as the deadlock victim. Error 1205"), true},
		{"unrelated error", fmt.Errorf("connection refused"), false},
		{"empty error", fmt.Errorf(""), false},
		{"timeout error", fmt.Errorf("timeout expired"), false},
		{"wrapped deadline exceeded", fmt.Errorf("operation failed: %w", context.DeadlineExceeded), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMSSQLDeadlock(tt.err)
			if got != tt.want {
				t.Errorf("isMSSQLDeadlock(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// isMSSQLDuplicateKey
// ---------------------------------------------------------------------------

func TestIsMSSQLDuplicateKey(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"error 2627", fmt.Errorf("violation of UNIQUE KEY constraint error 2627"), true},
		{"error 2601", fmt.Errorf("cannot insert duplicate key error 2601"), true},
		{"duplicate key row phrase", fmt.Errorf("Cannot insert duplicate key row in object 'dbo.wf'"), true},
		// Bare "duplicate" is not a SQL Server duplicate-key signal: it appears
		// in unrelated messages and in business data. Structured detection is
		// via mssql.Error.Number (2627/2601).
		{"bare duplicate word is not enough", fmt.Errorf("duplicate row detected"), false},
		{"primary key constraint", fmt.Errorf("violation of PRIMARY KEY constraint"), true},
		{"cannot insert duplicate key phrase", fmt.Errorf("Cannot insert duplicate key in object"), true},
		{"unique key constraint phrase", fmt.Errorf("UNIQUE KEY constraint violated"), true},
		{"unrelated error", fmt.Errorf("connection timeout"), false},
		{"empty error", fmt.Errorf(""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMSSQLDuplicateKey(tt.err)
			if got != tt.want {
				t.Errorf("isMSSQLDuplicateKey(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// isMSSQLSnapshotError
// ---------------------------------------------------------------------------

func TestIsMSSQLSnapshotError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"error 3960", fmt.Errorf("snapshot isolation error 3960"), true},
		{"snapshot isolation phrase", fmt.Errorf("snapshot isolation transaction aborted"), true},
		{"update conflict phrase", fmt.Errorf("update conflict with snapshot"), true},
		{"unrelated error", fmt.Errorf("connection refused"), false},
		{"empty error", fmt.Errorf(""), false},
		{"deadlock (different error)", fmt.Errorf("deadlock victim"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMSSQLSnapshotError(tt.err)
			if got != tt.want {
				t.Errorf("isMSSQLSnapshotError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// isMSSQLLockTimeout
// ---------------------------------------------------------------------------

func TestIsMSSQLLockTimeout(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"error 1222 lock timeout", fmt.Errorf("Lock request time out period exceeded. Error 1222"), true},
		{"lock timeout phrase, mixed case", fmt.Errorf("LOCK REQUEST TIME OUT PERIOD EXCEEDED"), true},
		// 258 is a different error (client/network wait), not this one.
		{"error 258 is a different timeout", fmt.Errorf("timeout expired error 258"), false},
		{"deadlock is a different error", fmt.Errorf("transaction was deadlocked"), false},
		{"unrelated error", fmt.Errorf("connection refused"), false},
		{"empty error", fmt.Errorf(""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMSSQLLockTimeout(tt.err)
			if got != tt.want {
				t.Errorf("isMSSQLLockTimeout(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MSSQLConnectionString
// ---------------------------------------------------------------------------

func TestMSSQLConnectionString(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		port     int
		user     string
		password string
		database string
		want     string
	}{
		{
			name:     "standard connection",
			host:     "localhost",
			port:     1433,
			user:     "sa",
			password: "Passw0rd!",
			database: "cleat",
			want:     "sqlserver://sa:Passw0rd!@localhost:1433?database=cleat&connection+timeout=30&encrypt=false",
		},
		{
			name:     "remote server",
			host:     "sqlserver.internal",
			port:     1433,
			user:     "admin",
			password: "s3cret!",
			database: "prod",
			want:     "sqlserver://admin:s3cret!@sqlserver.internal:1433?database=prod&connection+timeout=30&encrypt=false",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MSSQLConnectionString(tt.host, tt.port, tt.user, tt.password, tt.database)
			if got != tt.want {
				t.Errorf("MSSQLConnectionString() = %q, want %q", got, tt.want)
			}
		})
	}
}
