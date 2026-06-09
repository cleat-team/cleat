package engine

import (
	"context"
	"errors"
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
		{"duplicate keyword", fmt.Errorf("duplicate row detected"), true},
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
// isMSSQLTimeout
// ---------------------------------------------------------------------------

func TestIsMSSQLTimeout(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"context deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped deadline exceeded", fmt.Errorf("wrap: %w", context.DeadlineExceeded), true},
		{"error 258 timeout", fmt.Errorf("timeout expired error 258"), true},
		{"timeout keyword", fmt.Errorf("query timeout"), true},
		{"connection refused", fmt.Errorf("connection refused"), true},
		{"unrelated error", fmt.Errorf("syntax error"), false},
		{"empty error", fmt.Errorf(""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMSSQLTimeout(tt.err)
			if got != tt.want {
				t.Errorf("isMSSQLTimeout(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// isMSSQLConnectionError
// ---------------------------------------------------------------------------

func TestIsMSSQLConnectionError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"connection keyword", fmt.Errorf("connection reset by peer"), true},
		{"TLS error", fmt.Errorf("TLS handshake failed"), true},
		{"transport error", fmt.Errorf("transport endpoint is not connected"), true},
		{"broken pipe", fmt.Errorf("write: broken pipe"), true},
		{"unreachable", fmt.Errorf("host unreachable"), true},
		{"unrelated error", fmt.Errorf("syntax error near SELECT"), false},
		{"empty error", fmt.Errorf(""), false},
		{"deadlock (not connection)", fmt.Errorf("deadlock victim"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMSSQLConnectionError(tt.err)
			if got != tt.want {
				t.Errorf("isMSSQLConnectionError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// isMSSQLRetryable
// ---------------------------------------------------------------------------

func TestIsMSSQLRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"deadlock", fmt.Errorf("transaction was deadlocked"), true},
		{"snapshot isolation", fmt.Errorf("snapshot isolation error 3960"), true},
		{"timeout 258", fmt.Errorf("timeout expired error 258"), true},
		{"connection error", fmt.Errorf("connection reset by peer"), true},
		{"context deadline exceeded", context.DeadlineExceeded, true},
		{"transport error", fmt.Errorf("transport failure"), true},
		{"duplicate key (not retryable)", fmt.Errorf("violation of UNIQUE KEY constraint error 2627"), false},
		{"unrelated permanent error", fmt.Errorf("syntax error near SELECT"), false},
		{"empty error", fmt.Errorf(""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMSSQLRetryable(tt.err)
			if got != tt.want {
				t.Errorf("isMSSQLRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// mapMSSQLError
// ---------------------------------------------------------------------------

func TestMapMSSQLError(t *testing.T) {
	tests := []struct {
		name          string
		op            string
		workflowID    string
		err           error
		wantNil       bool
		wantTransient bool
		wantCancelled bool
		wantPermanent bool
	}{
		{
			name:    "nil error returns nil",
			op:      "ClaimWorkflow",
			err:     nil,
			wantNil: true,
		},
		{
			name:          "deadlock maps to transient",
			op:            "ClaimWorkflow",
			workflowID:    "wf-1",
			err:           fmt.Errorf("deadlock victim in resource"),
			wantTransient: true,
		},
		{
			name:          "snapshot conflict maps to transient",
			op:            "CompleteWorkflow",
			workflowID:    "wf-2",
			err:           fmt.Errorf("snapshot isolation update conflict error 3960"),
			wantTransient: true,
		},
		{
			name:          "timeout maps to transient",
			op:            "FailWorkflow",
			workflowID:    "wf-3",
			err:           fmt.Errorf("timeout expired error 258"),
			wantTransient: true,
		},
		{
			name:          "connection error maps to transient",
			op:            "StartNewRun",
			workflowID:    "wf-4",
			err:           fmt.Errorf("connection reset by peer"),
			wantTransient: true,
		},
		{
			name:          "duplicate key maps to cancelled",
			op:            "DeployWorkflowDef",
			workflowID:    "wf-5",
			err:           fmt.Errorf("violation of PRIMARY KEY constraint error 2627"),
			wantCancelled: true,
		},
		{
			name:          "other error maps to permanent",
			op:            "LoadWASM",
			workflowID:    "wf-6",
			err:           fmt.Errorf("syntax error"),
			wantPermanent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapMSSQLError(tt.op, tt.workflowID, tt.err)
			if tt.wantNil {
				if got != nil {
					t.Errorf("mapMSSQLError() = %v, want nil", got)
				}
				return
			}

			var ce *CleatError
			if !errors.As(got, &ce) {
				t.Fatalf("mapMSSQLError() returned %T, want *CleatError", got)
			}

			if ce.Op != tt.op {
				t.Errorf("CleatError.Op = %q, want %q", ce.Op, tt.op)
			}
			if ce.WorkflowID != tt.workflowID {
				t.Errorf("CleatError.WorkflowID = %q, want %q", ce.WorkflowID, tt.workflowID)
			}

			if tt.wantTransient && ce.Code != ErrTransient {
				t.Errorf("CleatError.Code = %v, want ErrTransient", ce.Code)
			}
			if tt.wantCancelled && ce.Code != ErrCancelled {
				t.Errorf("CleatError.Code = %v, want ErrCancelled", ce.Code)
			}
			if tt.wantPermanent && ce.Code != ErrPermanent {
				t.Errorf("CleatError.Code = %v, want ErrPermanent", ce.Code)
			}

			// Verify the original error is wrapped.
			if !errors.Is(got, tt.err) {
				t.Errorf("mapMSSQLError() does not wrap original error: %v", got)
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
