package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// mockRestoreDB — a minimal SQL driver mock that always succeeds on Exec.
// ---------------------------------------------------------------------------

type mockRestoreDB struct{}

func (c *mockRestoreDB) Connect(_ context.Context) (driver.Conn, error) {
	return &mockRestoreConn{}, nil
}

func (c *mockRestoreDB) Driver() driver.Driver {
	return &mockRestoreDriver{}
}

type mockRestoreDriver struct{}

func (d *mockRestoreDriver) Open(_ string) (driver.Conn, error) {
	return nil, errors.New("unused")
}

type mockRestoreConn struct{}

func (c *mockRestoreConn) Prepare(_ string) (driver.Stmt, error) {
	return &mockRestoreStmt{}, nil
}

func (c *mockRestoreConn) Close() error { return nil }

func (c *mockRestoreConn) Begin() (driver.Tx, error) {
	return nil, errors.New("no tx")
}

type mockRestoreStmt struct{}

func (s *mockRestoreStmt) Close() error { return nil }

func (s *mockRestoreStmt) NumInput() int { return -1 }

func (s *mockRestoreStmt) Exec(_ []driver.Value) (driver.Result, error) {
	return &mockRestoreResult{}, nil
}

func (s *mockRestoreStmt) Query(_ []driver.Value) (driver.Rows, error) {
	return nil, errors.New("read-only")
}

type mockRestoreResult struct{}

func (r *mockRestoreResult) LastInsertId() (int64, error) { return 0, nil }
func (r *mockRestoreResult) RowsAffected() (int64, error) { return 1, nil }

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestNullIfEmpty(t *testing.T) {
	// Empty string should return nil.
	if got := nullIfEmpty(""); got != nil {
		t.Errorf("nullIfEmpty('') = %v, want nil", got)
	}

	// Non-empty string should return the string itself.
	if got := nullIfEmpty("hello"); got != "hello" {
		t.Errorf("nullIfEmpty('hello') = %v, want 'hello'", got)
	}
}

func TestPrintRestoreUsage(t *testing.T) {
	stderr := captureStderr(t, func() {
		printRestoreUsage()
	})
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("expected 'Usage:' in output, got: %s", stderr)
	}
	if !strings.Contains(stderr, "restore-workflow") {
		t.Errorf("expected 'restore-workflow' in output, got: %s", stderr)
	}
	if !strings.Contains(stderr, "NDJSON") {
		t.Errorf("expected 'NDJSON' in output, got: %s", stderr)
	}
}

func TestRunRestoreWorkflow_InvalidArgs(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runRestoreWorkflow(context.Background(), nil, nil, []string{"only-one-arg"})
	})
	if !strings.Contains(stderr, "usage:") {
		t.Errorf("expected usage message in stderr, got: %s", stderr)
	}
}

func TestRunRestoreWorkflow_NoArgs(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runRestoreWorkflow(context.Background(), nil, nil, nil)
	})
	if !strings.Contains(stderr, "usage:") {
		t.Errorf("expected usage message in stderr, got: %s", stderr)
	}
}

func TestRunRestoreWorkflow_FileNotFound(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runRestoreWorkflow(context.Background(), nil, nil, []string{"wf-1", "/nonexistent/path.ndjson"})
	})
	if !strings.Contains(stderr, "error opening") {
		t.Errorf("expected 'error opening' in stderr, got: %s", stderr)
	}
}

func TestRunRestoreWorkflow_ValidBackup(t *testing.T) {
	dir := t.TempDir()
	backupPath := filepath.Join(dir, "backup.ndjson")

	// Create an NDJSON backup file with a workflow instance row and an event
	// history row, both referencing the same workflow ID.
	content := []byte(`{"table":"workflow_instances","row":{"id":"restore-wf-1","def_name":"test-def","status":"completed","input":"{}","created_at":"2025-01-01T00:00:00Z"}}
{"table":"event_history","row":{"workflow_id":"restore-wf-1","step":0,"event_type":"timer_started","service":"","operation":"","request":"","response":"","error":""}}
{"table":"event_history","row":{"workflow_id":"restore-wf-1","step":1,"event_type":"timer_fired","service":"","operation":"","request":"","response":"ok","error":""}}
`)
	if err := os.WriteFile(backupPath, content, 0644); err != nil {
		t.Fatalf("write backup file: %v", err)
	}

	connector := &mockRestoreDB{}
	db := sql.OpenDB(connector)
	defer db.Close()

	// Run the restore; it should complete without calling osExit.
	out := captureStdout(t, func() {
		runRestoreWorkflow(context.Background(), nil, db, []string{"restore-wf-1", backupPath})
	})

	if !strings.Contains(out, "Restoring workflow restore-wf-1") {
		t.Errorf("expected 'Restoring workflow restore-wf-1' in output, got: %s", out)
	}
	if !strings.Contains(out, "1 workflow instance rows") {
		t.Errorf("expected '1 workflow instance row' in output, got: %s", out)
	}
	if !strings.Contains(out, "2 event_history rows") {
		t.Errorf("expected '2 event_history rows' in output, got: %s", out)
	}
	if !strings.Contains(out, "Restore complete") {
		t.Errorf("expected 'Restore complete' in output, got: %s", out)
	}
}

func TestRunRestoreWorkflow_BackupWithSignalsAndPromises(t *testing.T) {
	dir := t.TempDir()
	backupPath := filepath.Join(dir, "backup_full.ndjson")

	content := []byte(`{"table":"workflow_instances","row":{"id":"wf-full","def_name":"test","status":"running","input":"{}"}}
{"table":"workflow_signals","row":{"workflow_id":"wf-full","signal_name":"greeting","payload":"hello","correlation_id":"corr-1"}}
{"table":"workflow_promises","row":{"workflow_id":"wf-full","promise_id":"prom-1","name":"payment","status":"pending","result":""}}
{"table":"child_workflows","row":{"workflow_id":"wf-full","child_id":"child-1","def_name":"child-wf","input":"{}"}}
`)
	if err := os.WriteFile(backupPath, content, 0644); err != nil {
		t.Fatalf("write backup file: %v", err)
	}

	connector := &mockRestoreDB{}
	db := sql.OpenDB(connector)
	defer db.Close()

	out := captureStdout(t, func() {
		runRestoreWorkflow(context.Background(), nil, db, []string{"wf-full", backupPath})
	})

	if !strings.Contains(out, "1 workflow_signals rows") {
		t.Errorf("expected signal rows in output, got: %s", out)
	}
	if !strings.Contains(out, "1 workflow_promises rows") {
		t.Errorf("expected promise rows in output, got: %s", out)
	}
	if !strings.Contains(out, "1 child workflow rows") {
		t.Errorf("expected child workflow rows in output, got: %s", out)
	}
	if !strings.Contains(out, "Restore complete") {
		t.Errorf("expected 'Restore complete' in output, got: %s", out)
	}
}

func TestRunRestoreWorkflow_WorkflowNotFoundInBackup(t *testing.T) {
	dir := t.TempDir()
	backupPath := filepath.Join(dir, "empty.ndjson")

	// Create backup with a different workflow ID.
	content := []byte(`{"table":"workflow_instances","row":{"id":"other-wf","def_name":"test","status":"completed","input":"{}"}}
`)
	if err := os.WriteFile(backupPath, content, 0644); err != nil {
		t.Fatalf("write backup file: %v", err)
	}

	connector := &mockRestoreDB{}
	db := sql.OpenDB(connector)
	defer db.Close()

	stderr := withExitPanic(t, func() {
		runRestoreWorkflow(context.Background(), nil, db, []string{"missing-wf", backupPath})
	})
	if !strings.Contains(stderr, "not found in backup") {
		t.Errorf("expected 'not found in backup' in stderr, got: %s", stderr)
	}
}

func TestRunRestoreWorkflow_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	backupPath := filepath.Join(dir, "invalid.ndjson")

	content := []byte(`this is not json
`)
	if err := os.WriteFile(backupPath, content, 0644); err != nil {
		t.Fatalf("write backup file: %v", err)
	}

	stderr := withExitPanic(t, func() {
		runRestoreWorkflow(context.Background(), nil, nil, []string{"wf-1", backupPath})
	})
	if !strings.Contains(stderr, "error parsing") {
		t.Errorf("expected 'error parsing' in stderr, got: %s", stderr)
	}
}

func TestRunRestoreWorkflow_UnknownTable(t *testing.T) {
	dir := t.TempDir()
	backupPath := filepath.Join(dir, "unknown_table.ndjson")

	content := []byte(`{"table":"workflow_instances","row":{"id":"wf-1","def_name":"test","status":"running","input":"{}"}}
{"table":"unknown_table","row":{"workflow_id":"wf-1","data":"test"}}
`)
	if err := os.WriteFile(backupPath, content, 0644); err != nil {
		t.Fatalf("write backup file: %v", err)
	}

	connector := &mockRestoreDB{}
	db := sql.OpenDB(connector)
	defer db.Close()

	out := captureStdout(t, func() {
		runRestoreWorkflow(context.Background(), nil, db, []string{"wf-1", backupPath})
	})

	// Unknown table should be warned about but not cause an error.
	if !strings.Contains(out, "Restore complete") {
		t.Errorf("expected 'Restore complete' in output, got: %s", out)
	}
}

func TestRunRestoreWorkflow_WithChildInstance(t *testing.T) {
	dir := t.TempDir()
	backupPath := filepath.Join(dir, "child.ndjson")

	// A child workflow instance that has parent_workflow_id matching the
	// target workflow ID should also be restored.
	content := []byte(`{"table":"workflow_instances","row":{"id":"parent-wf","def_name":"parent","status":"running","input":"{}"}}
{"table":"workflow_instances","row":{"id":"child-wf","def_name":"child","status":"running","input":"{}","parent_workflow_id":"parent-wf"}}
`)
	if err := os.WriteFile(backupPath, content, 0644); err != nil {
		t.Fatalf("write backup file: %v", err)
	}

	connector := &mockRestoreDB{}
	db := sql.OpenDB(connector)
	defer db.Close()

	out := captureStdout(t, func() {
		runRestoreWorkflow(context.Background(), nil, db, []string{"parent-wf", backupPath})
	})

	// Both the parent workflow (matched by id) and the child workflow
	// (matched by parent_workflow_id) should be restored.
	
	if !strings.Contains(out, "2 workflow instance rows") {
		t.Errorf("expected 2 workflow instance rows, got: %s", out)
	}
	if !strings.Contains(out, "Restore complete") {
		t.Errorf("expected 'Restore complete' in output, got: %s", out)
	}
}

func TestRunRestoreWorkflow_CommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	backupPath := filepath.Join(dir, "comments.ndjson")

	// Backup file with comments and blank lines should be handled.
	content := []byte(`# This is a comment
// This is also a comment

{"table":"workflow_instances","row":{"id":"wf-comment","def_name":"test","status":"running","input":"{}"}}
`)
	if err := os.WriteFile(backupPath, content, 0644); err != nil {
		t.Fatalf("write backup file: %v", err)
	}

	connector := &mockRestoreDB{}
	db := sql.OpenDB(connector)
	defer db.Close()

	out := captureStdout(t, func() {
		runRestoreWorkflow(context.Background(), nil, db, []string{"wf-comment", backupPath})
	})

	if !strings.Contains(out, "Restore complete") {
		t.Errorf("expected 'Restore complete' in output, got: %s", out)
	}
}
