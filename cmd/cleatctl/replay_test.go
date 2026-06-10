package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// mockReplayDB — a minimal SQL driver mock for loadWorkflowInstance tests.
// ---------------------------------------------------------------------------

// mockReplayDB implements driver.Connector, returning a single row when
// notFound is false, or no rows when notFound is true.
type mockReplayDB struct {
	notFound bool
}

type mockReplayDriver struct{}

func (d *mockReplayDriver) Open(_ string) (driver.Conn, error) {
	return nil, errors.New("unused")
}

func (c *mockReplayDB) Connect(_ context.Context) (driver.Conn, error) {
	return &mockReplayConn{notFound: c.notFound}, nil
}

func (c *mockReplayDB) Driver() driver.Driver {
	return &mockReplayDriver{}
}

type mockReplayConn struct {
	notFound bool
}

func (c *mockReplayConn) Prepare(_ string) (driver.Stmt, error) {
	return &mockReplayStmt{notFound: c.notFound}, nil
}

func (c *mockReplayConn) Close() error { return nil }

func (c *mockReplayConn) Begin() (driver.Tx, error) {
	return nil, errors.New("no tx")
}

type mockReplayStmt struct {
	notFound bool
}

func (s *mockReplayStmt) Close() error { return nil }

func (s *mockReplayStmt) NumInput() int { return -1 }

func (s *mockReplayStmt) Exec(_ []driver.Value) (driver.Result, error) {
	return nil, errors.New("read-only")
}

func (s *mockReplayStmt) Query(_ []driver.Value) (driver.Rows, error) {
	if s.notFound {
		return &mockReplayNoRows{}, nil
	}
	return &mockReplayRows{}, nil
}

type mockReplayNoRows struct{}

func (r *mockReplayNoRows) Columns() []string {
	return []string{"id", "def_name", "def_version", "min_version", "status", "input",
		"result", "error", "error_code", "error_op", "assigned_to",
		"next_wake_at", "tenant_id", "created_at", "generation"}
}

func (r *mockReplayNoRows) Close() error { return nil }

func (r *mockReplayNoRows) Next(_ []driver.Value) error { return io.EOF }

type mockReplayRows struct {
	called bool
}

func (r *mockReplayRows) Columns() []string {
	return []string{"id", "def_name", "def_version", "min_version", "status", "input",
		"result", "error", "error_code", "error_op", "assigned_to",
		"next_wake_at", "tenant_id", "created_at", "generation"}
}

func (r *mockReplayRows) Close() error { return nil }

func (r *mockReplayRows) Next(dest []driver.Value) error {
	if r.called {
		return io.EOF
	}
	r.called = true
	now := time.Now()
	dest[0] = "wf-test-123"
	dest[1] = "my-workflow"
	dest[2] = int64(3)
	dest[3] = int64(1)
	dest[4] = "running"
	dest[5] = []byte(`{"customer":"acme"}`)
	dest[6] = ""
	dest[7] = ""
	dest[8] = ""
	dest[9] = ""
	dest[10] = "worker-1"
	dest[11] = now
	dest[12] = "tenant-default"
	dest[13] = now
	dest[14] = int64(5)
	return nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestParseReplayFlags_Success(t *testing.T) {
	flags := parseReplayFlags([]string{"wf-123", "--entry-point", "HandleLead"})
	if flags == nil {
		t.Fatal("expected non-nil flags")
	}
	if flags.workflowID != "wf-123" {
		t.Errorf("workflowID = %q, want %q", flags.workflowID, "wf-123")
	}
	if flags.entryPoint != "HandleLead" {
		t.Errorf("entryPoint = %q, want %q", flags.entryPoint, "HandleLead")
	}
	if flags.verbose {
		t.Error("expected verbose=false")
	}
}

func TestParseReplayFlags_MissingEntryPoint(t *testing.T) {
	stderr := captureStderr(t, func() {
		flags := parseReplayFlags([]string{"wf-123"})
		if flags != nil {
			t.Error("expected nil flags when --entry-point is missing")
		}
	})
	if !strings.Contains(stderr, "--entry-point") {
		t.Errorf("expected stderr to mention --entry-point, got: %s", stderr)
	}
}

func TestParseReplayFlags_NoArgs(t *testing.T) {
	stderr := captureStderr(t, func() {
		flags := parseReplayFlags(nil)
		if flags != nil {
			t.Error("expected nil flags with no args")
		}
	})
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("expected usage in stderr, got: %s", stderr)
	}
}

func TestParseReplayFlags_WithVerbose(t *testing.T) {
	flags := parseReplayFlags([]string{"wf-99", "--entry-point", "Run", "--verbose"})
	if flags == nil {
		t.Fatal("expected non-nil flags")
	}
	if !flags.verbose {
		t.Error("expected verbose=true")
	}

	// Also test -v short form.
	flags2 := parseReplayFlags([]string{"wf-99", "--entry-point", "Run", "-v"})
	if flags2 == nil {
		t.Fatal("expected non-nil flags")
	}
	if !flags2.verbose {
		t.Error("expected verbose=true with -v")
	}
}

func TestParseReplayFlags_MissingEntryPointValue(t *testing.T) {
	stderr := captureStderr(t, func() {
		flags := parseReplayFlags([]string{"wf-123", "--entry-point"})
		if flags != nil {
			t.Error("expected nil flags when --entry-point has no value")
		}
	})
	if !strings.Contains(stderr, "--entry-point requires a value") {
		t.Errorf("expected error about --entry-point requiring value, got: %s", stderr)
	}
}

func TestPrintReplayUsage(t *testing.T) {
	stderr := captureStderr(t, func() {
		printReplayUsage()
	})
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("expected 'Usage:' in output, got: %s", stderr)
	}
	if !strings.Contains(stderr, "replay") {
		t.Errorf("expected 'replay' in output, got: %s", stderr)
	}
	if !strings.Contains(stderr, "--entry-point") {
		t.Errorf("expected '--entry-point' in output, got: %s", stderr)
	}
}

func TestLoadWorkflowInstance_Success(t *testing.T) {
	connector := &mockReplayDB{notFound: false}
	db := sql.OpenDB(connector)
	defer db.Close()

	ctx := context.Background()
	inst, err := loadWorkflowInstance(ctx, db, "wf-test-123")
	if err != nil {
		t.Fatalf("loadWorkflowInstance returned error: %v", err)
	}
	if inst == nil {
		t.Fatal("expected non-nil instance")
	}
	if inst.ID != "wf-test-123" {
		t.Errorf("ID = %q, want %q", inst.ID, "wf-test-123")
	}
	if inst.DefName != "my-workflow" {
		t.Errorf("DefName = %q, want %q", inst.DefName, "my-workflow")
	}
	if inst.DefVersion != 3 {
		t.Errorf("DefVersion = %d, want 3", inst.DefVersion)
	}
	if inst.MinVersion != 1 {
		t.Errorf("MinVersion = %d, want 1", inst.MinVersion)
	}
	if inst.Status != "running" {
		t.Errorf("Status = %q, want %q", inst.Status, "running")
	}
	if string(inst.Input) != `{"customer":"acme"}` {
		t.Errorf("Input = %q, want %q", string(inst.Input), `{"customer":"acme"}`)
	}
	if inst.AssignedTo != "worker-1" {
		t.Errorf("AssignedTo = %q, want %q", inst.AssignedTo, "worker-1")
	}
	if inst.TenantID != "tenant-default" {
		t.Errorf("TenantID = %q, want %q", inst.TenantID, "tenant-default")
	}
	if inst.Generation != 5 {
		t.Errorf("Generation = %d, want 5", inst.Generation)
	}
}

func TestLoadWorkflowInstance_NotFound(t *testing.T) {
	connector := &mockReplayDB{notFound: true}
	db := sql.OpenDB(connector)
	defer db.Close()

	ctx := context.Background()
	inst, err := loadWorkflowInstance(ctx, db, "nonexistent")
	if err == nil {
		t.Fatal("expected error for not-found workflow")
	}
	if inst != nil {
		t.Errorf("expected nil instance, got %v", inst)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short string", "hello", 10, "hello"},
		{"exact fit", "hello", 5, "hello"},
		{"long string", "hello world this is long", 10, "hello worl..."},
		{"empty string", "", 10, ""},
		{"maxLen zero", "hello", 0, "..."},
		{"unicode", "hello world", 5, "hello..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}
