package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/internal/host"
)

// ---------------------------------------------------------------------------
// runVersions subcommand dispatch
// ---------------------------------------------------------------------------

func TestRunVersions_DispatchList(t *testing.T) {
	var called bool
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			called = true
			return nil, nil
		},
	}
	_ = captureStdout(t, func() {
		runVersions(context.Background(), store, []string{"list", "mywf"})
	})
	if !called {
		t.Error("expected listVersions to be called via runVersions dispatch")
	}
}

func TestRunVersions_DispatchDeprecate(t *testing.T) {
	var capturedVersion int
	store := &mockStore{
		markVersionDeprecatedFn: func(_ context.Context, name string, version int, deprecated bool) error {
			capturedVersion = version
			return nil
		},
	}
	_ = captureStdout(t, func() {
		runVersions(context.Background(), store, []string{"deprecate", "mywf", "3"})
	})
	if capturedVersion != 3 {
		t.Errorf("expected deprecate to be dispatched for version 3, got %d", capturedVersion)
	}
}

func TestRunVersions_DispatchRestore(t *testing.T) {
	var capturedDeprecated bool
	store := &mockStore{
		markVersionDeprecatedFn: func(_ context.Context, name string, version int, deprecated bool) error {
			capturedDeprecated = deprecated
			return nil
		},
	}
	_ = captureStdout(t, func() {
		runVersions(context.Background(), store, []string{"restore", "mywf", "2"})
	})
	if capturedDeprecated != false {
		t.Error("expected restore to dispatch with deprecated=false")
	}
}

func TestRunVersions_DispatchPurge(t *testing.T) {
	var capturedVersion int
	store := &mockStore{
		purgeWorkflowDefFn: func(_ context.Context, name string, version int) error {
			capturedVersion = version
			return nil
		},
	}
	withStdin(t, "y\n", func() {
		_, _ = captureOutputs(t, func() {
			runVersions(context.Background(), store, []string{"purge", "mywf", "4"})
		})
	})
	if capturedVersion != 4 {
		t.Errorf("expected purge to be dispatched for version 4, got %d", capturedVersion)
	}
}

func TestRunVersions_DispatchActive(t *testing.T) {
	store := &mockStore{
		getActiveInstanceCountsByVersionFn: func(_ context.Context) (map[string]int, error) {
			return map[string]int{}, nil
		},
	}
	stdout := captureStdout(t, func() {
		runVersions(context.Background(), store, []string{"active"})
	})
	if !strings.Contains(stdout, "No active instances") {
		t.Errorf("expected 'No active instances' via active dispatch, got: %s", stdout)
	}
}

func TestRunVersions_DispatchGC(t *testing.T) {
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return []host.WorkflowDef{
				{Name: "wf", Version: 1, Deprecated: true, CreatedAt: time.Now().Add(-90 * 24 * time.Hour)},
			}, nil
		},
		getActiveInstanceCountsByVersionFn: func(_ context.Context) (map[string]int, error) {
			return map[string]int{}, nil
		},
		purgeWorkflowDefFn: func(_ context.Context, name string, version int) error {
			return errors.New("purge error")
		},
	}
	stdout := captureStdout(t, func() {
		runVersions(context.Background(), store, []string{"gc"})
	})
	if !strings.Contains(stdout, "GC complete") {
		t.Errorf("expected GC output via dispatch, got: %s", stdout)
	}
}

// ---------------------------------------------------------------------------
// runDeploy subcommand dispatch
// ---------------------------------------------------------------------------

func TestRunDeploy_DispatchWorkflow(t *testing.T) {
	dir := t.TempDir()
	path := writeWASM(t, dir, []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00})

	var capturedDef *host.WorkflowDef
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return nil, nil
		},
		deployWorkflowDefFn: func(_ context.Context, def *host.WorkflowDef) error {
			capturedDef = def
			return nil
		},
	}
	_ = captureStdout(t, func() {
		runDeploy(context.Background(), store, nil, []string{"workflow", "dispatch-wf", path})
	})
	if capturedDef == nil || capturedDef.Version != 1 {
		t.Errorf("expected deploy workflow to be dispatched, got %+v", capturedDef)
	}
}

func TestRunDeploy_DispatchPlugin(t *testing.T) {
	dir := t.TempDir()
	path := writeWASM(t, dir, []byte{0x00, 0x61, 0x73, 0x6d})
	connector := &mockPluginConnector{existing: false, fail: false}
	db := sql.OpenDB(connector)
	defer db.Close()

	stdout, stderr := captureOutputs(t, func() {
		runDeploy(context.Background(), &mockStore{}, db, []string{"plugin", "dispatch-plugin", path})
	})
	if !strings.Contains(stdout, "Deployed plugin dispatch-plugin") {
		t.Errorf("expected deploy plugin via dispatch, got stdout: %s, stderr: %s", stdout, stderr)
	}
}

// ---------------------------------------------------------------------------
// purgeVersion: invalid version
// ---------------------------------------------------------------------------

func TestPurgeVersion_InvalidVersion(t *testing.T) {
	stderr := withExitPanic(t, func() {
		purgeVersion(context.Background(), &mockStore{}, []string{"wf", "abc"})
	})
	if !strings.Contains(stderr, "invalid version") {
		t.Errorf("expected 'invalid version' error, got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// purgeVersion: uppercase Y confirmation
// ---------------------------------------------------------------------------

func TestPurgeVersion_UpperCaseY(t *testing.T) {
	var capturedName string
	store := &mockStore{
		purgeWorkflowDefFn: func(_ context.Context, name string, version int) error {
			capturedName = name
			return nil
		},
	}
	stdout, stderr := captureOutputs(t, func() {
		withStdin(t, "Y\n", func() {
			purgeVersion(context.Background(), store, []string{"cap-wf", "7"})
		})
	})
	if capturedName != "cap-wf" {
		t.Errorf("expected purge with uppercase Y, got name=%q", capturedName)
	}
	if !strings.Contains(stdout, "cap-wf v7 purged") {
		t.Errorf("expected purge success, got: %s", stdout)
	}
	if stderr != "" {
		t.Errorf("unexpected stderr: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// activeInstances: ListWorkflowDefs error with name
// ---------------------------------------------------------------------------

func TestActiveInstances_WithNameListError(t *testing.T) {
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return nil, errors.New("list failed")
		},
	}
	stderr := withExitPanic(t, func() {
		activeInstances(context.Background(), store, []string{"err-wf"})
	})
	if !strings.Contains(stderr, "list failed") {
		t.Errorf("expected 'list failed' in stderr, got: %s", stderr)
	}
}
