package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/internal/host"
)

// ---------------------------------------------------------------------------
// mockStore — simulates host.WorkflowStore for cleatctl tests.
// ---------------------------------------------------------------------------

type mockStore struct {
	listWorkflowDefsFn                func(ctx context.Context, name string) ([]host.WorkflowDef, error)
	getActiveInstanceCountsByVersionFn func(ctx context.Context) (map[string]int, error)
	countActiveInstancesFn            func(ctx context.Context, name string, version int) (int, error)
	markVersionDeprecatedFn           func(ctx context.Context, name string, version int, deprecated bool) error
	purgeWorkflowDefFn                func(ctx context.Context, name string, version int) error
	deployWorkflowDefFn               func(ctx context.Context, def *host.WorkflowDef) error
	claimWorkflowFn                   func(ctx context.Context, workerID string) (*host.WorkflowInstance, error)
	claimWorkflowsFn                  func(ctx context.Context, workerID string, limit int) ([]*host.WorkflowInstance, error)
	claimStickyWorkflowsFn            func(ctx context.Context, workerID string, limit int) ([]*host.WorkflowInstance, error)
	loadEventHistoryFn                func(ctx context.Context, workflowID string) ([]host.EventRecord, error)
	appendEventHistoryFn              func(ctx context.Context, workflowID string, rec host.EventRecord) error
	appendEventHistoryBatchFn         func(ctx context.Context, workflowID string, recs []host.EventRecord) error
	loadWASMFn                        func(ctx context.Context, defName string, defVersion int) ([]byte, error)
	listVersionsFn                    func(ctx context.Context, defName string) ([]int, error)
	heartbeatFn                       func(ctx context.Context, workflowID, workerID string) (bool, error)
	batchHeartbeatFn                 func(ctx context.Context, workerID string) (int64, error)
	completeWorkflowFn                func(ctx context.Context, workflowID, workerID, result string, queryState map[string]string) error
	failWorkflowFn                    func(ctx context.Context, workflowID, workerID, errMsg, errorCode, errorOp string, queryState map[string]string) error
	releaseWorkflowFn                 func(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error
	requestCancellationFn             func(ctx context.Context, workflowID, reason string) error
	checkCancellationFn               func(ctx context.Context, workflowID string) (bool, string, error)
	deliverSignalFn                   func(ctx context.Context, workflowID, signalName, payload string) error
	pollAndClaimSignalFn              func(ctx context.Context, workflowID, signalName string) (string, bool, error)
	startNewRunFn                     func(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string) (string, bool, error)
	startChildWorkflowFn              func(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string) (string, error)
	getChildResultFn                  func(ctx context.Context, runID string) (string, bool, error)
	reapStaleInstancesFn              func(ctx context.Context, timeout time.Duration) (int, error)
	getQueryStateFn                   func(ctx context.Context, workflowID, key string) (string, error)
	listWorkflowsFn                   func(ctx context.Context, filter host.WorkflowFilter) ([]host.WorkflowInstance, error)
	getWorkflowByIDFn                 func(ctx context.Context, id string) (*host.WorkflowInstance, error)
	createScheduleFn                  func(ctx context.Context, s host.Schedule) error
	listSchedulesFn                   func(ctx context.Context) ([]host.Schedule, error)
	deleteScheduleFn                  func(ctx context.Context, name string) error
	setScheduleEnabledFn              func(ctx context.Context, name string, enabled bool) error
	getDueSchedulesFn                 func(ctx context.Context) ([]host.Schedule, error)
	updateScheduleNextRunFn           func(ctx context.Context, name string, nextRun time.Time) error
	loadWorkflowConfigFn              func(ctx context.Context, defName string, defVersion int) (int, error)
	loadDAGSpecFn                     func(ctx context.Context, defName string, defVersion int) (json.RawMessage, error)
	traceWorkflowFn                   func(ctx context.Context, workflowID, traceID string) error
	getCompactionCandidatesFn         func(ctx context.Context, threshold int, limit int) ([]string, error)
	loadCompactionStateFn             func(ctx context.Context, workflowID string) (*host.CompactionState, error)
	compactHistoryFn                  func(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error
	createPromiseFn                   func(ctx context.Context, workflowID, promiseName, promiseID string) error
	resolvePromiseFn                  func(ctx context.Context, workflowID, promiseID, result string) error
	rejectPromiseFn                   func(ctx context.Context, workflowID, promiseID, errMsg string) error
	getPromiseFn                      func(ctx context.Context, workflowID, promiseID string) (string, string, string, error)
	listPromisesFn                    func(ctx context.Context, workflowID string) ([]host.PromiseInfo, error)
	createUpdateRequestFn             func(ctx context.Context, workflowID, updateName, payload, promiseID string) error
	getPendingUpdateRequestsFn        func(ctx context.Context, workflowID string) ([]host.UpdateRequestInfo, error)
	completeUpdateRequestFn           func(ctx context.Context, workflowID, updateName, result, errMsg string) error
	acquireConcurrencyKeyFn           func(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error)
	releaseConcurrencyKeyFn           func(ctx context.Context, key string) error
	releaseWorkflowConcurrencyKeysFn  func(ctx context.Context, workflowID string) error
	reapExpiredConcurrencyKeysFn      func(ctx context.Context) (int64, error)
	updateStickyWorkerFn              func(ctx context.Context, workflowID, workerID string) error
	clearStickyWorkerFn               func(ctx context.Context, workflowID string) error
	getWorkflowDefFn                  func(ctx context.Context, name string, version int) (*host.WorkflowDef, error)
	deleteExpiredEventsFn             func(ctx context.Context, olderThan time.Time) (int64, error)
	continueAsNewFn                   func(ctx context.Context, currentRunID, workerID string, defName string, defVersion int, newInput json.RawMessage, result string, queryState map[string]string) (string, error)
	finalizeWorkflowSegmentFn         func(ctx context.Context, runID, workerID string, newEvents []host.EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error
}

// ---- mockStore interface methods ----

func (m *mockStore) ClaimWorkflow(ctx context.Context, workerID string) (*host.WorkflowInstance, error) {
	if m.claimWorkflowFn != nil {
		return m.claimWorkflowFn(ctx, workerID)
	}
	return nil, nil
}

func (m *mockStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*host.WorkflowInstance, error) {
	if m.claimWorkflowsFn != nil {
		return m.claimWorkflowsFn(ctx, workerID, limit)
	}
	return nil, nil
}

func (m *mockStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*host.WorkflowInstance, error) {
	if m.claimStickyWorkflowsFn != nil {
		return m.claimStickyWorkflowsFn(ctx, workerID, limit)
	}
	return nil, nil
}

func (m *mockStore) LoadEventHistory(ctx context.Context, workflowID string) ([]host.EventRecord, error) {
	if m.loadEventHistoryFn != nil {
		return m.loadEventHistoryFn(ctx, workflowID)
	}
	return nil, nil
}

func (m *mockStore) AppendEventHistory(ctx context.Context, workflowID string, rec host.EventRecord) error {
	if m.appendEventHistoryFn != nil {
		return m.appendEventHistoryFn(ctx, workflowID, rec)
	}
	return nil
}

func (m *mockStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []host.EventRecord) error {
	if m.appendEventHistoryBatchFn != nil {
		return m.appendEventHistoryBatchFn(ctx, workflowID, recs)
	}
	return nil
}

func (m *mockStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) {
	if m.loadWASMFn != nil {
		return m.loadWASMFn(ctx, defName, defVersion)
	}
	return nil, nil
}

func (m *mockStore) ListVersions(ctx context.Context, defName string) ([]int, error) {
	if m.listVersionsFn != nil {
		return m.listVersionsFn(ctx, defName)
	}
	return []int{1}, nil
}

func (m *mockStore) Heartbeat(ctx context.Context, workflowID, workerID string) (bool, error) {
	if m.heartbeatFn != nil {
		return m.heartbeatFn(ctx, workflowID, workerID)
	}
	return true, nil
}

func (m *mockStore) CompleteWorkflow(ctx context.Context, workflowID, workerID, result string, queryState map[string]string) error {
	if m.completeWorkflowFn != nil {
		return m.completeWorkflowFn(ctx, workflowID, workerID, result, queryState)
	}
	return nil
}

func (m *mockStore) FailWorkflow(ctx context.Context, workflowID, workerID, errMsg, errorCode, errorOp string, queryState map[string]string) error {
	if m.failWorkflowFn != nil {
		return m.failWorkflowFn(ctx, workflowID, workerID, errMsg, errorCode, errorOp, queryState)
	}
	return nil
}

func (m *mockStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error {
	if m.releaseWorkflowFn != nil {
		return m.releaseWorkflowFn(ctx, workflowID, workerID, nextWakeAt)
	}
	return nil
}

func (m *mockStore) RequestCancellation(ctx context.Context, workflowID, reason string) error {
	if m.requestCancellationFn != nil {
		return m.requestCancellationFn(ctx, workflowID, reason)
	}
	return nil
}

func (m *mockStore) CheckCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	if m.checkCancellationFn != nil {
		return m.checkCancellationFn(ctx, workflowID)
	}
	return false, "", nil
}

func (m *mockStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error {
	if m.deliverSignalFn != nil {
		return m.deliverSignalFn(ctx, workflowID, signalName, payload)
	}
	return nil
}

func (m *mockStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	if m.pollAndClaimSignalFn != nil {
		return m.pollAndClaimSignalFn(ctx, workflowID, signalName)
	}
	return "", false, nil
}

func (m *mockStore) PollSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	return m.PollAndClaimSignal(ctx, workflowID, signalName)
}

func (m *mockStore) PollCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	return m.CheckCancellation(ctx, workflowID)
}

func (m *mockStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string) (string, bool, error) {
	if m.startNewRunFn != nil {
		return m.startNewRunFn(ctx, runID, defName, defVersion, input, idempotencyKey)
	}
	return "test-run-id", false, nil
}

func (m *mockStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string) (string, error) {
	if m.startChildWorkflowFn != nil {
		return m.startChildWorkflowFn(ctx, parentID, defName, inputJSON, defVersion, parentClosePolicy)
	}
	return "child-run-id", nil
}

func (m *mockStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event host.EventRecord) (string, error) {
	return m.StartChildWorkflow(ctx, parentID, defName, inputJSON, defVersion, parentClosePolicy)
}

func (m *mockStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	if m.getChildResultFn != nil {
		return m.getChildResultFn(ctx, runID)
	}
	return "", false, nil
}

func (m *mockStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) {
	if m.reapStaleInstancesFn != nil {
		return m.reapStaleInstancesFn(ctx, timeout)
	}
	return 0, nil
}

func (m *mockStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) {
	if m.getQueryStateFn != nil {
		return m.getQueryStateFn(ctx, workflowID, key)
	}
	return "", nil
}

func (m *mockStore) ListWorkflows(ctx context.Context, filter host.WorkflowFilter) ([]host.WorkflowInstance, error) {
	if m.listWorkflowsFn != nil {
		return m.listWorkflowsFn(ctx, filter)
	}
	return nil, nil
}

func (m *mockStore) GetWorkflowByID(ctx context.Context, id string) (*host.WorkflowInstance, error) {
	if m.getWorkflowByIDFn != nil {
		return m.getWorkflowByIDFn(ctx, id)
	}
	return nil, nil
}

func (m *mockStore) CreateSchedule(ctx context.Context, s host.Schedule) error {
	if m.createScheduleFn != nil {
		return m.createScheduleFn(ctx, s)
	}
	return nil
}

func (m *mockStore) ListSchedules(ctx context.Context) ([]host.Schedule, error) {
	if m.listSchedulesFn != nil {
		return m.listSchedulesFn(ctx)
	}
	return nil, nil
}

func (m *mockStore) DeleteSchedule(ctx context.Context, name string) error {
	if m.deleteScheduleFn != nil {
		return m.deleteScheduleFn(ctx, name)
	}
	return nil
}

func (m *mockStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error {
	if m.setScheduleEnabledFn != nil {
		return m.setScheduleEnabledFn(ctx, name, enabled)
	}
	return nil
}

func (m *mockStore) GetDueSchedules(ctx context.Context) ([]host.Schedule, error) {
	if m.getDueSchedulesFn != nil {
		return m.getDueSchedulesFn(ctx)
	}
	return nil, nil
}

func (m *mockStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error {
	if m.updateScheduleNextRunFn != nil {
		return m.updateScheduleNextRunFn(ctx, name, nextRun)
	}
	return nil
}

func (m *mockStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (int, error) {
	if m.loadWorkflowConfigFn != nil {
		return m.loadWorkflowConfigFn(ctx, defName, defVersion)
	}
	return 0, nil
}

func (m *mockStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
	if m.loadDAGSpecFn != nil {
		return m.loadDAGSpecFn(ctx, defName, defVersion)
	}
	return nil, nil
}

func (m *mockStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) error {
	if m.traceWorkflowFn != nil {
		return m.traceWorkflowFn(ctx, workflowID, traceID)
	}
	return nil
}

func (m *mockStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) {
	if m.getCompactionCandidatesFn != nil {
		return m.getCompactionCandidatesFn(ctx, threshold, limit)
	}
	return nil, nil
}

func (m *mockStore) LoadCompactionState(ctx context.Context, workflowID string) (*host.CompactionState, error) {
	if m.loadCompactionStateFn != nil {
		return m.loadCompactionStateFn(ctx, workflowID)
	}
	return nil, nil
}

func (m *mockStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
	if m.compactHistoryFn != nil {
		return m.compactHistoryFn(ctx, workflowID, compactionState, compactionStep, keepStep)
	}
	return nil
}

func (m *mockStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error {
	if m.createPromiseFn != nil {
		return m.createPromiseFn(ctx, workflowID, promiseName, promiseID)
	}
	return nil
}

func (m *mockStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error {
	if m.resolvePromiseFn != nil {
		return m.resolvePromiseFn(ctx, workflowID, promiseID, result)
	}
	return nil
}

func (m *mockStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error {
	if m.rejectPromiseFn != nil {
		return m.rejectPromiseFn(ctx, workflowID, promiseID, errMsg)
	}
	return nil
}

func (m *mockStore) GetPromise(ctx context.Context, workflowID, promiseID string) (string, string, string, error) {
	if m.getPromiseFn != nil {
		return m.getPromiseFn(ctx, workflowID, promiseID)
	}
	return "pending", "", "", nil
}

func (m *mockStore) ListPromises(ctx context.Context, workflowID string) ([]host.PromiseInfo, error) {
	if m.listPromisesFn != nil {
		return m.listPromisesFn(ctx, workflowID)
	}
	return nil, nil
}

func (m *mockStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
	if m.createUpdateRequestFn != nil {
		return m.createUpdateRequestFn(ctx, workflowID, updateName, payload, promiseID)
	}
	return nil
}

func (m *mockStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]host.UpdateRequestInfo, error) {
	if m.getPendingUpdateRequestsFn != nil {
		return m.getPendingUpdateRequestsFn(ctx, workflowID)
	}
	return nil, nil
}

func (m *mockStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error {
	if m.completeUpdateRequestFn != nil {
		return m.completeUpdateRequestFn(ctx, workflowID, updateName, result, errMsg)
	}
	return nil
}

func (m *mockStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	if m.acquireConcurrencyKeyFn != nil {
		return m.acquireConcurrencyKeyFn(ctx, key, workflowID, ttl)
	}
	return true, nil
}

func (m *mockStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	if m.releaseConcurrencyKeyFn != nil {
		return m.releaseConcurrencyKeyFn(ctx, key)
	}
	return nil
}

func (m *mockStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error {
	if m.releaseWorkflowConcurrencyKeysFn != nil {
		return m.releaseWorkflowConcurrencyKeysFn(ctx, workflowID)
	}
	return nil
}

func (m *mockStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) {
	if m.reapExpiredConcurrencyKeysFn != nil {
		return m.reapExpiredConcurrencyKeysFn(ctx)
	}
	return 0, nil
}

func (m *mockStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error {
	if m.updateStickyWorkerFn != nil {
		return m.updateStickyWorkerFn(ctx, workflowID, workerID)
	}
	return nil
}

func (m *mockStore) ClearStickyWorker(ctx context.Context, workflowID string) error {
	if m.clearStickyWorkerFn != nil {
		return m.clearStickyWorkerFn(ctx, workflowID)
	}
	return nil
}

func (m *mockStore) ListWorkflowDefs(ctx context.Context, name string) ([]host.WorkflowDef, error) {
	if m.listWorkflowDefsFn != nil {
		return m.listWorkflowDefsFn(ctx, name)
	}
	return nil, nil
}

func (m *mockStore) GetWorkflowDef(ctx context.Context, name string, version int) (*host.WorkflowDef, error) {
	if m.getWorkflowDefFn != nil {
		return m.getWorkflowDefFn(ctx, name, version)
	}
	return nil, nil
}

func (m *mockStore) DeployWorkflowDef(ctx context.Context, def *host.WorkflowDef) error {
	if m.deployWorkflowDefFn != nil {
		return m.deployWorkflowDefFn(ctx, def)
	}
	return nil
}

func (m *mockStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error {
	if m.markVersionDeprecatedFn != nil {
		return m.markVersionDeprecatedFn(ctx, name, version, deprecated)
	}
	return nil
}

func (m *mockStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error {
	if m.purgeWorkflowDefFn != nil {
		return m.purgeWorkflowDefFn(ctx, name, version)
	}
	return nil
}

func (m *mockStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) {
	if m.countActiveInstancesFn != nil {
		return m.countActiveInstancesFn(ctx, name, version)
	}
	return 0, nil
}

func (m *mockStore) GetActiveInstanceCountsByVersion(ctx context.Context) (map[string]int, error) {
	if m.getActiveInstanceCountsByVersionFn != nil {
		return m.getActiveInstanceCountsByVersionFn(ctx)
	}
	return nil, nil
}

func (m *mockStore) CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error) {
	return 0, nil
}

func (m *mockStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error {
	return nil
}

func (m *mockStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) {
	return nil, nil
}

func (m *mockStore) LoadMemoryStats(ctx context.Context) ([]host.WorkflowMemoryStats, error) {
	return nil, nil
}

func (m *mockStore) QueueDepth(ctx context.Context) (int64, error) {
	return 0, nil
}

func (m *mockStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, defName string, defVersion int, newInput json.RawMessage, newEvents []host.EventRecord, result string, queryState map[string]string) (string, error) {
	if m.continueAsNewFn != nil {
		return m.continueAsNewFn(ctx, currentRunID, workerID, defName, defVersion, newInput, result, queryState)
	}
	return "", nil
}
func (m *mockStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, newEvents []host.EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	if m.finalizeWorkflowSegmentFn != nil {
		return m.finalizeWorkflowSegmentFn(ctx, runID, workerID, newEvents, finalStatus, result, errorCode, errorOp, queryState, nextWakeAt)
	}
	return nil
}
func (m *mockStore) DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	if m.deleteExpiredEventsFn != nil {
		return m.deleteExpiredEventsFn(ctx, olderThan)
	}
	return 0, nil
}

// ---------------------------------------------------------------------------
// mock driver for deploy plugin tests
// ---------------------------------------------------------------------------

// mockPluginConnector implements driver.Connector for testing deployPlugin.
type mockPluginConnector struct {
	existing bool
	fail     bool
}

type mockPluginDriver struct{}

func (d *mockPluginDriver) Open(name string) (driver.Conn, error) {
	return nil, errors.New("unused")
}

func (c *mockPluginConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &mockPluginConn{existing: c.existing, fail: c.fail}, nil
}

func (c *mockPluginConnector) Driver() driver.Driver {
	return &mockPluginDriver{}
}

type mockPluginConn struct {
	existing bool
	fail     bool
}

func (c *mockPluginConn) Prepare(query string) (driver.Stmt, error) {
	if c.fail {
		return nil, errors.New("mock db error")
	}
	return &mockPluginStmt{existing: c.existing, fail: c.fail}, nil
}

func (c *mockPluginConn) Close() error { return nil }

func (c *mockPluginConn) Begin() (driver.Tx, error) {
	return nil, errors.New("no tx")
}

type mockPluginStmt struct {
	existing bool
	fail     bool
}

func (s *mockPluginStmt) Close() error { return nil }
func (s *mockPluginStmt) NumInput() int { return -1 }

func (s *mockPluginStmt) Exec(_ []driver.Value) (driver.Result, error) {
	if s.fail {
		return nil, errors.New("mock exec error")
	}
	return &mockResult{}, nil
}

func (s *mockPluginStmt) Query(_ []driver.Value) (driver.Rows, error) {
	if s.fail {
		return nil, errors.New("mock query error")
	}
	if s.existing {
		return &mockSingleRow{}, nil
	}
	return &mockNoRows{}, nil
}

type mockResult struct{}

func (r *mockResult) LastInsertId() (int64, error) { return 0, nil }
func (r *mockResult) RowsAffected() (int64, error) { return 1, nil }

type mockNoRows struct{}

func (r *mockNoRows) Columns() []string          { return []string{"id"} }
func (r *mockNoRows) Close() error               { return nil }
func (r *mockNoRows) Next(_ []driver.Value) error { return io.EOF }

type mockSingleRow struct {
	called bool
}

func (r *mockSingleRow) Columns() []string { return []string{"id"} }
func (r *mockSingleRow) Close() error      { return nil }
func (r *mockSingleRow) Next(dest []driver.Value) error {
	if !r.called {
		r.called = true
		dest[0] = "existing-plugin-id"
		return nil
	}
	return io.EOF
}

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

// captureStdout runs fn and returns everything written to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	out := make(chan string)
	go func() {
		var buf bytes.Buffer
		if _, cerr := io.Copy(&buf, r); cerr != nil {
			t.Logf("copy error: %v", cerr)
		}
		out <- buf.String()
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-out
}

// captureOutputs captures both stdout and stderr while running fn.
// This is safe for functions that DO NOT call os.Exit.
func captureOutputs(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	oldOut := os.Stdout
	oldErr := os.Stderr
	os.Stdout = wOut
	os.Stderr = wErr

	outCh := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, rOut)
		outCh <- buf.String()
	}()
	errCh := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, rErr)
		errCh <- buf.String()
	}()

	fn()

	wOut.Close()
	wErr.Close()
	os.Stdout = oldOut
	os.Stderr = oldErr
	return <-outCh, <-errCh
}

// withExitPanic replaces osExit to panic (so we can recover from os.Exit(1)
// calls). It returns the captured stderr from the function.
func withExitPanic(t *testing.T, fn func()) (stderr string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	oldErr := os.Stderr
	os.Stderr = w

	origExit := osExit
	osExit = func(code int) { panic(fmt.Sprintf("EXIT:%d", code)) }

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, r)
		close(done)
	}()

	func() {
		defer func() { recover() }()
		fn()
	}()

	osExit = origExit
	os.Stderr = oldErr
	w.Close()
	<-done
	return buf.String()
}

// withExitPanicOutput captures both stdout and stderr with osExit panicking.
func withExitPanicOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	oldOut := os.Stdout
	oldErr := os.Stderr
	os.Stdout = wOut
	os.Stderr = wErr

	origExit := osExit
	osExit = func(code int) { panic(fmt.Sprintf("EXIT:%d", code)) }

	outCh := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, rOut)
		outCh <- buf.String()
	}()
	errCh := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, rErr)
		errCh <- buf.String()
	}()

	func() {
		defer func() { recover() }()
		fn()
	}()

	osExit = origExit
	os.Stdout = oldOut
	os.Stderr = oldErr
	wOut.Close()
	wErr.Close()
	return <-outCh, <-errCh
}

// withStdin pipes the given input to os.Stdin for the duration of fn.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	oldStdin := os.Stdin
	os.Stdin = r
	if _, err := w.Write([]byte(input)); err != nil {
		t.Fatalf("stdin write: %v", err)
	}
	w.Close()
	defer func() { os.Stdin = oldStdin }()
	fn()
}

// writeWASM creates a temporary WASM file for testing.
func writeWASM(t *testing.T, dir string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, "test.wasm")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write wasm: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------------
// Versions: runVersions / listVersions
// ---------------------------------------------------------------------------

func TestRunVersions_NoSubcommand(t *testing.T) {
	_, stderr := withExitPanicOutput(t, func() {
		runVersions(context.Background(), &mockStore{}, nil)
	})
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("expected usage in stderr, got: %s", stderr)
	}
}

func TestRunVersions_UnknownSubcommand(t *testing.T) {
	_, stderr := withExitPanicOutput(t, func() {
		runVersions(context.Background(), &mockStore{}, []string{"nosuch"})
	})
	if !strings.Contains(stderr, "unknown versions subcommand") {
		t.Errorf("expected unknown subcommand error, got: %s", stderr)
	}
}

func TestListVersions_All(t *testing.T) {
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			if name != "" {
				return nil, nil
			}
			return []host.WorkflowDef{
				{Name: "wf-a", Version: 2, ABIVersion: 1, MinVersion: 1, Deprecated: false, CreatedAt: time.Now().Add(-24 * time.Hour)},
				{Name: "wf-a", Version: 1, ABIVersion: 1, MinVersion: 0, Deprecated: true, CreatedAt: time.Now().Add(-48 * time.Hour)},
			}, nil
		},
		getActiveInstanceCountsByVersionFn: func(_ context.Context) (map[string]int, error) {
			return map[string]int{"wf-a:2": 3, "wf-a:1": 1}, nil
		},
		countActiveInstancesFn: func(_ context.Context, name string, version int) (int, error) {
			return 0, nil
		},
	}
	stdout := captureStdout(t, func() {
		listVersions(context.Background(), store, []string{})
	})
	if !strings.Contains(stdout, "Total versions: 2") {
		t.Errorf("expected 'Total versions: 2', got: %s", stdout)
	}
	if !strings.Contains(stdout, "Active: 1") {
		t.Errorf("expected 'Active: 1', got: %s", stdout)
	}
	if !strings.Contains(stdout, "Deprecated: 1") {
		t.Errorf("expected 'Deprecated: 1', got: %s", stdout)
	}
	if !strings.Contains(stdout, "wf-a") {
		t.Errorf("expected 'wf-a' in output, got: %s", stdout)
	}
}

func TestListVersions_WithName(t *testing.T) {
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return []host.WorkflowDef{
				{Name: name, Version: 2, ABIVersion: 1, MinVersion: 1, Deprecated: false, CreatedAt: time.Now().Add(-24 * time.Hour)},
				{Name: name, Version: 1, ABIVersion: 1, MinVersion: 0, Deprecated: true, CreatedAt: time.Now().Add(-48 * time.Hour)},
			}, nil
		},
		countActiveInstancesFn: func(_ context.Context, name string, version int) (int, error) {
			return 5, nil
		},
	}
	stdout := captureStdout(t, func() {
		listVersions(context.Background(), store, []string{"myworkflow"})
	})
	if !strings.Contains(stdout, "Version") || !strings.Contains(stdout, "ABI") {
		t.Errorf("expected version table header, got: %s", stdout)
	}
	if !strings.Contains(stdout, "2") || !strings.Contains(stdout, "1") {
		t.Errorf("expected versions 2 and 1 in output, got: %s", stdout)
	}
}

func TestListVersions_NoResults(t *testing.T) {
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return nil, nil
		},
	}
	stdout := captureStdout(t, func() {
		listVersions(context.Background(), store, []string{"nonexistent"})
	})
	if !strings.Contains(stdout, "No versions found") {
		t.Errorf("expected 'No versions found', got: %s", stdout)
	}
}

func TestListVersions_StoreError(t *testing.T) {
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return nil, errors.New("connection refused")
		},
	}
	stderr := withExitPanic(t, func() {
		listVersions(context.Background(), store, []string{})
	})
	if !strings.Contains(stderr, "connection refused") {
		t.Errorf("expected 'connection refused' error in stderr, got: %s", stderr)
	}
}

func TestListVersions_CollectMetricsError(t *testing.T) {
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return []host.WorkflowDef{
				{Name: "wf", Version: 1, CreatedAt: time.Now()},
			}, nil
		},
		getActiveInstanceCountsByVersionFn: func(_ context.Context) (map[string]int, error) {
			return nil, errors.New("metrics error")
		},
	}
	stderr := withExitPanic(t, func() {
		listVersions(context.Background(), store, []string{})
	})
	if !strings.Contains(stderr, "metrics error") {
		t.Errorf("expected 'metrics error' in stderr, got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Versions: deprecate / restore
// ---------------------------------------------------------------------------

func TestDeprecateVersion_MissingArgs(t *testing.T) {
	stderr := withExitPanic(t, func() {
		deprecateVersion(context.Background(), &mockStore{}, []string{}, true)
	})
	if !strings.Contains(stderr, "usage") || !strings.Contains(stderr, "deprecate") {
		t.Errorf("expected usage in stderr, got: %s", stderr)
	}
}

func TestDeprecateVersion_InvalidVersion(t *testing.T) {
	stderr := withExitPanic(t, func() {
		deprecateVersion(context.Background(), &mockStore{}, []string{"wf", "abc"}, true)
	})
	if !strings.Contains(stderr, "invalid version") {
		t.Errorf("expected invalid version error, got: %s", stderr)
	}
}

func TestDeprecateVersion_Success(t *testing.T) {
	var capturedName string
	var capturedVersion int
	var capturedDeprecated bool

	store := &mockStore{
		markVersionDeprecatedFn: func(_ context.Context, name string, version int, deprecated bool) error {
			capturedName = name
			capturedVersion = version
			capturedDeprecated = deprecated
			return nil
		},
	}
	stdout := captureStdout(t, func() {
		deprecateVersion(context.Background(), store, []string{"mywf", "3"}, true)
	})
	if capturedName != "mywf" || capturedVersion != 3 || capturedDeprecated != true {
		t.Errorf("deprecate called with (%q, %d, %t), want (%q, %d, %t)",
			capturedName, capturedVersion, capturedDeprecated, "mywf", 3, true)
	}
	if !strings.Contains(stdout, "mywf v3 deprecated") {
		t.Errorf("expected 'mywf v3 deprecated', got: %s", stdout)
	}
}

func TestRestoreVersion_Success(t *testing.T) {
	var capturedName string
	var capturedVersion int
	var capturedDeprecated bool

	store := &mockStore{
		markVersionDeprecatedFn: func(_ context.Context, name string, version int, deprecated bool) error {
			capturedName = name
			capturedVersion = version
			capturedDeprecated = deprecated
			return nil
		},
	}
	stdout := captureStdout(t, func() {
		deprecateVersion(context.Background(), store, []string{"mywf", "2"}, false)
	})
	if capturedName != "mywf" || capturedVersion != 2 || capturedDeprecated != false {
		t.Errorf("restore called with (%q, %d, %t), want (%q, %d, %t)",
			capturedName, capturedVersion, capturedDeprecated, "mywf", 2, false)
	}
	if !strings.Contains(stdout, "mywf v2 restored") {
		t.Errorf("expected 'mywf v2 restored', got: %s", stdout)
	}
}

func TestDeprecateVersion_StoreError(t *testing.T) {
	store := &mockStore{
		markVersionDeprecatedFn: func(_ context.Context, name string, version int, deprecated bool) error {
			return errors.New("db unavailable")
		},
	}
	stderr := withExitPanic(t, func() {
		deprecateVersion(context.Background(), store, []string{"wf", "1"}, true)
	})
	if !strings.Contains(stderr, "db unavailable") {
		t.Errorf("expected 'db unavailable' in stderr, got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Versions: purge
// ---------------------------------------------------------------------------

func TestPurgeVersion_MissingArgs(t *testing.T) {
	stderr := withExitPanic(t, func() {
		purgeVersion(context.Background(), &mockStore{}, []string{})
	})
	if !strings.Contains(stderr, "usage") {
		t.Errorf("expected usage in stderr, got: %s", stderr)
	}
}

func TestPurgeVersion_Success(t *testing.T) {
	var capturedName string
	var capturedVersion int

	store := &mockStore{
		purgeWorkflowDefFn: func(_ context.Context, name string, version int) error {
			capturedName = name
			capturedVersion = version
			return nil
		},
	}
	stdout, stderr := captureOutputs(t, func() {
		withStdin(t, "y\n", func() {
			purgeVersion(context.Background(), store, []string{"purge-wf", "5"})
		})
	})
	if capturedName != "purge-wf" || capturedVersion != 5 {
		t.Errorf("purge called with (%q, %d), want (%q, %d)",
			capturedName, capturedVersion, "purge-wf", 5)
	}
	if !strings.Contains(stdout, "purge-wf v5 purged") {
		t.Errorf("expected 'purge-wf v5 purged', got: %s", stdout)
	}
	if stderr != "" {
		t.Errorf("unexpected stderr: %s", stderr)
	}
}

func TestPurgeVersion_Cancelled(t *testing.T) {
	store := &mockStore{
		purgeWorkflowDefFn: func(_ context.Context, name string, version int) error {
			t.Error("PurgeWorkflowDef should not be called on cancel")
			return nil
		},
	}
	stdout, stderr := captureOutputs(t, func() {
		withStdin(t, "n\n", func() {
			purgeVersion(context.Background(), store, []string{"purge-wf", "5"})
		})
	})
	if !strings.Contains(stdout, "cancelled") {
		t.Errorf("expected 'cancelled', got: %s", stdout)
	}
	if stderr != "" {
		t.Errorf("unexpected stderr: %s", stderr)
	}
}

func TestPurgeVersion_StoreError(t *testing.T) {
	store := &mockStore{
		purgeWorkflowDefFn: func(_ context.Context, name string, version int) error {
			return errors.New("permission denied")
		},
	}
	stderr := withExitPanic(t, func() {
		withStdin(t, "y\n", func() {
			purgeVersion(context.Background(), store, []string{"purge-wf", "5"})
		})
	})
	if !strings.Contains(stderr, "permission denied") {
		t.Errorf("expected 'permission denied' in stderr, got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Versions: active instances
// ---------------------------------------------------------------------------

func TestActiveInstances_All(t *testing.T) {
	store := &mockStore{
		getActiveInstanceCountsByVersionFn: func(_ context.Context) (map[string]int, error) {
			return map[string]int{"wf:1": 3, "wf:2": 7}, nil
		},
	}
	stdout := captureStdout(t, func() {
		activeInstances(context.Background(), store, []string{})
	})
	if !strings.Contains(stdout, "Grand total active instances: 10") {
		t.Errorf("expected 'Grand total active instances: 10', got: %s", stdout)
	}
	if !strings.Contains(stdout, "Workflow") || !strings.Contains(stdout, "Version") {
		t.Errorf("expected table header, got: %s", stdout)
	}
}

func TestActiveInstances_WithName(t *testing.T) {
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return []host.WorkflowDef{
				{Name: name, Version: 2, Deprecated: false, CreatedAt: time.Now()},
				{Name: name, Version: 1, Deprecated: true, CreatedAt: time.Now()},
			}, nil
		},
		countActiveInstancesFn: func(_ context.Context, name string, version int) (int, error) {
			if version == 2 {
				return 5, nil
			}
			return 2, nil
		},
	}
	stdout := captureStdout(t, func() {
		activeInstances(context.Background(), store, []string{"myapp"})
	})
	if !strings.Contains(stdout, "Total active instances for myapp: 7") {
		t.Errorf("expected 'Total active instances for myapp: 7', got: %s", stdout)
	}
}

func TestActiveInstances_NoInstances(t *testing.T) {
	store := &mockStore{
		getActiveInstanceCountsByVersionFn: func(_ context.Context) (map[string]int, error) {
			return map[string]int{}, nil
		},
	}
	stdout := captureStdout(t, func() {
		activeInstances(context.Background(), store, []string{})
	})
	if !strings.Contains(stdout, "No active instances") {
		t.Errorf("expected 'No active instances', got: %s", stdout)
	}
}

func TestActiveInstances_StoreError(t *testing.T) {
	store := &mockStore{
		getActiveInstanceCountsByVersionFn: func(_ context.Context) (map[string]int, error) {
			return nil, errors.New("query timeout")
		},
	}
	stderr := withExitPanic(t, func() {
		activeInstances(context.Background(), store, []string{})
	})
	if !strings.Contains(stderr, "query timeout") {
		t.Errorf("expected 'query timeout' in stderr, got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Versions: gc
// ---------------------------------------------------------------------------

func TestGCVersions_Success(t *testing.T) {
	oldCreated := time.Now().Add(-60 * 24 * time.Hour)

	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return []host.WorkflowDef{
				{Name: "wf", Version: 4, Deprecated: false, CreatedAt: time.Now()},
				{Name: "wf", Version: 3, Deprecated: true, CreatedAt: oldCreated},
				{Name: "wf", Version: 2, Deprecated: true, CreatedAt: oldCreated},
				{Name: "wf", Version: 1, Deprecated: true, CreatedAt: oldCreated},
			}, nil
		},
		getActiveInstanceCountsByVersionFn: func(_ context.Context) (map[string]int, error) {
			return map[string]int{}, nil
		},
		purgeWorkflowDefFn: func(_ context.Context, name string, version int) error {
			return nil
		},
	}

	stdout := captureStdout(t, func() {
		gcVersions(context.Background(), store, []string{})
	})
	if !strings.Contains(stdout, "GC complete") {
		t.Errorf("expected 'GC complete' in stdout, got: %s", stdout)
	}
	if !strings.Contains(stdout, "Versions removed:") {
		t.Errorf("expected 'Versions removed:' in stdout, got: %s", stdout)
	}
}

func TestGCVersions_DryRun(t *testing.T) {
	oldCreated := time.Now().Add(-60 * 24 * time.Hour)

	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return []host.WorkflowDef{
				{Name: "wf", Version: 4, Deprecated: false, CreatedAt: time.Now()},
				{Name: "wf", Version: 3, Deprecated: true, CreatedAt: oldCreated},
				{Name: "wf", Version: 2, Deprecated: true, CreatedAt: oldCreated},
				{Name: "wf", Version: 1, Deprecated: true, CreatedAt: oldCreated},
			}, nil
		},
		getActiveInstanceCountsByVersionFn: func(_ context.Context) (map[string]int, error) {
			return map[string]int{}, nil
		},
		purgeWorkflowDefFn: func(_ context.Context, name string, version int) error {
			t.Error("PurgeWorkflowDef should not be called during dry run")
			return nil
		},
	}

	stdout := captureStdout(t, func() {
		gcVersions(context.Background(), store, []string{"--dry-run"})
	})
	if !strings.Contains(stdout, "dry run") {
		t.Errorf("expected 'dry run' in stdout, got: %s", stdout)
	}
	if !strings.Contains(stdout, "Versions removed:") {
		t.Errorf("expected 'Versions removed:' in stdout, got: %s", stdout)
	}
}

func TestGCVersions_StoreError(t *testing.T) {
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return nil, errors.New("no db connection")
		},
	}
	stderr := withExitPanic(t, func() {
		gcVersions(context.Background(), store, []string{})
	})
	if !strings.Contains(stderr, "no db connection") {
		t.Errorf("expected 'no db connection' in stderr, got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Deploy: runDeploy
// ---------------------------------------------------------------------------

func TestRunDeploy_NoSubcommand(t *testing.T) {
	_, stderr := withExitPanicOutput(t, func() {
		runDeploy(context.Background(), &mockStore{}, nil, nil)
	})
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("expected usage in stderr, got: %s", stderr)
	}
}

func TestRunDeploy_UnknownSubcommand(t *testing.T) {
	_, stderr := withExitPanicOutput(t, func() {
		runDeploy(context.Background(), &mockStore{}, nil, []string{"nosuch"})
	})
	if !strings.Contains(stderr, "unknown deploy subcommand") {
		t.Errorf("expected unknown subcommand error, got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Deploy: workflow
// ---------------------------------------------------------------------------

func TestDeployWorkflow_MissingArgs(t *testing.T) {
	stderr := withExitPanic(t, func() {
		deployWorkflow(context.Background(), &mockStore{}, nil, []string{})
	})
	if !strings.Contains(stderr, "usage") {
		t.Errorf("expected usage in stderr, got: %s", stderr)
	}
}

func TestDeployWorkflow_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := writeWASM(t, dir, []byte{})

	stderr := withExitPanic(t, func() {
		deployWorkflow(context.Background(), &mockStore{}, nil, []string{"test-wf", path})
	})
	if !strings.Contains(stderr, "empty WASM") {
		t.Errorf("expected 'empty WASM' error, got: %s", stderr)
	}
}

func TestDeployWorkflow_Success(t *testing.T) {
	dir := t.TempDir()
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	path := writeWASM(t, dir, wasmBytes)

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

	stdout, stderr := captureOutputs(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"new-wf", path})
	})

	if capturedDef == nil {
		t.Fatal("expected DeployWorkflowDef to be called")
	}
	if capturedDef.Name != "new-wf" || capturedDef.Version != 1 {
		t.Errorf("expected name=new-wf version=1, got name=%s version=%d", capturedDef.Name, capturedDef.Version)
	}
	if !strings.Contains(stdout, "Deployed new-wf v1") {
		t.Errorf("expected 'Deployed new-wf v1' in stdout, got: %s", stdout)
	}
	if stderr != "" {
		t.Errorf("unexpected stderr: %s", stderr)
	}
}

func TestDeployWorkflow_SameHash(t *testing.T) {
	dir := t.TempDir()
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	path := writeWASM(t, dir, wasmBytes)

	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return []host.WorkflowDef{
				{
					Name:      name,
					Version:   1,
					WASMBytes: wasmBytes,
					CreatedAt: time.Now().Add(-24 * time.Hour),
				},
			}, nil
		},
		deployWorkflowDefFn: func(_ context.Context, def *host.WorkflowDef) error {
			t.Error("DeployWorkflowDef should not be called for unchanged WASM")
			return nil
		},
	}

	stdout, stderr := captureOutputs(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"existing-wf", path})
	})

	if !strings.Contains(stdout, "WASM unchanged") {
		t.Errorf("expected 'WASM unchanged' in stdout, got: %s", stdout)
	}
	if stderr != "" {
		t.Errorf("unexpected stderr: %s", stderr)
	}
}

func TestDeployWorkflow_SameHashSecondIteration(t *testing.T) {
	dir := t.TempDir()
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	path := writeWASM(t, dir, wasmBytes)

	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return []host.WorkflowDef{
				{Name: name, Version: 2, WASMBytes: []byte{0, 1, 2}, CreatedAt: time.Now()},
				{Name: name, Version: 1, WASMBytes: wasmBytes, CreatedAt: time.Now().Add(-24 * time.Hour)},
			}, nil
		},
		deployWorkflowDefFn: func(_ context.Context, def *host.WorkflowDef) error {
			t.Error("DeployWorkflowDef should not be called for unchanged WASM")
			return nil
		},
	}

	stdout := captureStdout(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"existing-wf", path})
	})
	if !strings.Contains(stdout, "WASM unchanged") {
		t.Errorf("expected 'WASM unchanged' in stdout, got: %s", stdout)
	}
}

func TestDeployWorkflow_WithExistingVersions(t *testing.T) {
	dir := t.TempDir()
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	path := writeWASM(t, dir, wasmBytes)

	var capturedDef *host.WorkflowDef
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return []host.WorkflowDef{
				{
					Name:       name,
					Version:    2,
					ABIVersion: 1,
					WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
					PluginDeps: map[string]string{"p1": "v1"},
					CreatedAt:  time.Now(),
				},
			}, nil
		},
		deployWorkflowDefFn: func(_ context.Context, def *host.WorkflowDef) error {
			capturedDef = def
			return nil
		},
	}

	stdout := captureStdout(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"existing-wf", path})
	})

	if capturedDef == nil {
		t.Fatal("expected DeployWorkflowDef to be called")
	}
	if capturedDef.Version != 3 {
		t.Errorf("expected version=3, got %d", capturedDef.Version)
	}
	if capturedDef.MinVersion != 2 {
		t.Errorf("expected minVersion=2, got %d", capturedDef.MinVersion)
	}
	if capturedDef.ABIVersion != 1 {
		t.Errorf("expected abiVersion=1, got %d", capturedDef.ABIVersion)
	}
	if capturedDef.PluginDeps["p1"] != "v1" {
		t.Errorf("expected plugin deps to be copied")
	}
	if !strings.Contains(stdout, "Deployed existing-wf v3") {
		t.Errorf("expected 'Deployed existing-wf v3' in stdout, got: %s", stdout)
	}
}

func TestDeployWorkflow_StoreError(t *testing.T) {
	dir := t.TempDir()
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	path := writeWASM(t, dir, wasmBytes)

	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return nil, nil
		},
		deployWorkflowDefFn: func(_ context.Context, def *host.WorkflowDef) error {
			return errors.New("deploy rejected")
		},
	}
	stderr := withExitPanic(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"fail-wf", path})
	})
	if !strings.Contains(stderr, "deploy rejected") {
		t.Errorf("expected 'deploy rejected' in stderr, got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Deploy: plugin
// ---------------------------------------------------------------------------

func TestDeployPlugin_MissingArgs(t *testing.T) {
	stderr := withExitPanic(t, func() {
		deployPlugin(context.Background(), nil, []string{})
	})
	if !strings.Contains(stderr, "usage") {
		t.Errorf("expected usage in stderr, got: %s", stderr)
	}
}

func TestDeployPlugin_DBError(t *testing.T) {
	dir := t.TempDir()
	path := writeWASM(t, dir, []byte{0x00, 0x61, 0x73, 0x6d})

	connector := &mockPluginConnector{fail: true}
	db := sql.OpenDB(connector)
	defer db.Close()

	stderr := withExitPanic(t, func() {
		deployPlugin(context.Background(), db, []string{"test-plugin", path})
	})
	if !strings.Contains(stderr, "error") {
		t.Errorf("expected error in stderr, got: %s", stderr)
	}
}

func TestDeployPlugin_InsertPath(t *testing.T) {
	dir := t.TempDir()
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	path := writeWASM(t, dir, wasmBytes)

	connector := &mockPluginConnector{existing: false, fail: false}
	db := sql.OpenDB(connector)
	defer db.Close()

	stdout, stderr := captureOutputs(t, func() {
		deployPlugin(context.Background(), db, []string{"new-plugin", path})
	})
	if !strings.Contains(stdout, "Deployed plugin new-plugin") {
		t.Errorf("expected 'Deployed plugin new-plugin' in stdout, got: %s", stdout)
	}
	if !strings.Contains(stdout, "SHA256") {
		t.Errorf("expected SHA256 in stdout, got: %s", stdout)
	}
	if stderr != "" {
		t.Errorf("unexpected stderr: %s", stderr)
	}
}

func TestDeployPlugin_UpdatePath(t *testing.T) {
	dir := t.TempDir()
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	path := writeWASM(t, dir, wasmBytes)

	connector := &mockPluginConnector{existing: true, fail: false}
	db := sql.OpenDB(connector)
	defer db.Close()

	stdout, stderr := captureOutputs(t, func() {
		deployPlugin(context.Background(), db, []string{"existing-plugin", path})
	})
	if !strings.Contains(stdout, "Updated plugin existing-plugin") {
		t.Errorf("expected 'Updated plugin existing-plugin' in stdout, got: %s", stdout)
	}
	if !strings.Contains(stdout, "SHA256") {
		t.Errorf("expected SHA256 in stdout, got: %s", stdout)
	}
	if stderr != "" {
		t.Errorf("unexpected stderr: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Deploy: error paths — file not found
// ---------------------------------------------------------------------------

func TestDeployWorkflow_FileNotFound(t *testing.T) {
	stderr := withExitPanic(t, func() {
		deployWorkflow(context.Background(), &mockStore{}, nil, []string{"wf", "/nonexistent.wasm"})
	})
	if !strings.Contains(stderr, "error reading") {
		t.Errorf("expected 'error reading' in stderr, got: %s", stderr)
	}
}

func TestDeployPlugin_FileNotFound(t *testing.T) {
	db := sql.OpenDB(&mockPluginConnector{})
	defer db.Close()

	stderr := withExitPanic(t, func() {
		deployPlugin(context.Background(), db, []string{"plugin", "/nonexistent.wasm"})
	})
	if !strings.Contains(stderr, "error reading") {
		t.Errorf("expected 'error reading' in stderr, got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Deploy workflow — ListWorkflowDefs error (fallback to nextVersion=1)
// ---------------------------------------------------------------------------

func TestDeployWorkflow_ListError(t *testing.T) {
	dir := t.TempDir()
	path := writeWASM(t, dir, []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00})

	var capturedDef *host.WorkflowDef
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return nil, errors.New("list failed")
		},
		deployWorkflowDefFn: func(_ context.Context, def *host.WorkflowDef) error {
			capturedDef = def
			return nil
		},
	}

	stdout := captureStdout(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"recover-wf", path})
	})
	if capturedDef == nil || capturedDef.Version != 1 {
		t.Errorf("expected version=1 after list failure, got %+v", capturedDef)
	}
	if !strings.Contains(stdout, "Deployed recover-wf v1") {
		t.Errorf("expected 'Deployed recover-wf v1' in stdout, got: %s", stdout)
	}
}

// ---------------------------------------------------------------------------
// Edge case: single-workflow active with CountActiveInstances error (continues)
// ---------------------------------------------------------------------------

func TestActiveInstances_WithNameCountError(t *testing.T) {
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return []host.WorkflowDef{
				{Name: name, Version: 1, Deprecated: false, CreatedAt: time.Now()},
			}, nil
		},
		countActiveInstancesFn: func(_ context.Context, name string, version int) (int, error) {
			return 0, errors.New("count error")
		},
	}
	stdout := captureStdout(t, func() {
		activeInstances(context.Background(), store, []string{"err-wf"})
	})
	if !strings.Contains(stdout, "Total active instances for err-wf: 0") {
		t.Errorf("expected total=0 after count error, got: %s", stdout)
	}
}

// ---------------------------------------------------------------------------
// Test that `versions gc` with errors in GC result prints them
// ---------------------------------------------------------------------------

func TestGCVersions_WithErrors(t *testing.T) {
	oldCreated := time.Now().Add(-60 * 24 * time.Hour)

	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return []host.WorkflowDef{
				{Name: "wf", Version: 4, Deprecated: false, CreatedAt: time.Now()},
				{Name: "wf", Version: 3, Deprecated: true, CreatedAt: oldCreated},
				{Name: "wf", Version: 2, Deprecated: true, CreatedAt: oldCreated},
				{Name: "wf", Version: 1, Deprecated: true, CreatedAt: oldCreated},
			}, nil
		},
		getActiveInstanceCountsByVersionFn: func(_ context.Context) (map[string]int, error) {
			return map[string]int{}, nil
		},
		purgeWorkflowDefFn: func(_ context.Context, name string, version int) error {
			return errors.New("purge failure")
		},
	}

	stdout := captureStdout(t, func() {
		gcVersions(context.Background(), store, []string{})
	})
	if !strings.Contains(stdout, "Errors") || !strings.Contains(stdout, "purge failure") {
		t.Errorf("expected error output in stdout, got: %s", stdout)
	}
}

// ---------------------------------------------------------------------------
// listVersions with name — CountActiveInstances error (uses -1)
// ---------------------------------------------------------------------------

func TestListVersions_WithNameCountError(t *testing.T) {
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return []host.WorkflowDef{
				{Name: name, Version: 1, CreatedAt: time.Now()},
			}, nil
		},
		countActiveInstancesFn: func(_ context.Context, name string, version int) (int, error) {
			return 0, errors.New("bad count")
		},
	}
	stdout := captureStdout(t, func() {
		listVersions(context.Background(), store, []string{"err-wf"})
	})
	if !strings.Contains(stdout, "-1") {
		t.Errorf("expected -1 count in output, got: %s", stdout)
	}
}

// ---------------------------------------------------------------------------
// Test captureStderr helper from existing test file also works with our tests
// ---------------------------------------------------------------------------

func TestCaptureStderrWrapper(t *testing.T) {
	out := captureStderr(t, func() {
		os.Stderr.WriteString("stderr test message\n")
	})
	if !strings.Contains(out, "stderr test message") {
		t.Errorf("expected 'stderr test message', got: %s", out)
	}
}

// ---------------------------------------------------------------------------
// Test that non-dry-run args do not trigger dry-run mode
// ---------------------------------------------------------------------------

func TestGCVersions_ArgsNotDryRun(t *testing.T) {
	oldCreated := time.Now().Add(-60 * 24 * time.Hour)

	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]host.WorkflowDef, error) {
			return []host.WorkflowDef{
				{Name: "wf", Version: 4, Deprecated: false, CreatedAt: time.Now()},
				{Name: "wf", Version: 3, Deprecated: true, CreatedAt: oldCreated},
				{Name: "wf", Version: 2, Deprecated: true, CreatedAt: oldCreated},
				{Name: "wf", Version: 1, Deprecated: true, CreatedAt: oldCreated},
			}, nil
		},
		getActiveInstanceCountsByVersionFn: func(_ context.Context) (map[string]int, error) {
			return map[string]int{}, nil
		},
		purgeWorkflowDefFn: func(_ context.Context, name string, version int) error {
			return nil
		},
	}

	stdout := captureStdout(t, func() {
		gcVersions(context.Background(), store, []string{"--some-other-flag"})
	})
	if strings.Contains(stdout, "dry run") {
		t.Errorf("should not contain 'dry run': %s", stdout)
	}
}
func (m *mockStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) { if m.batchHeartbeatFn != nil { return m.batchHeartbeatFn(ctx, workerID) }; return 0, nil }

func (m *mockStore) LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]host.EventRecord, error) { return nil, nil }
func (m *mockStore) VerifyWorkflowEvents(ctx context.Context, workflowID string) error { return nil }
func (m *mockStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID, errMsg, errorCode, errorOp string) error { return nil }
func (m *mockStore) RetryWorkflow(ctx context.Context, workflowID string) error { return nil }
func (m *mockStore) ResolveLatestVersion(ctx context.Context, defName string) (int, error) { return 0, nil }
func (m *mockStore) ValidateVersion(ctx context.Context, defName string, defVersion int) (bool, error) { return true, nil }
func (m *mockStore) CountEventHistory(ctx context.Context, workflowID string) (int, error) { return 0, nil }
func (m *mockStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) { return uuid.Nil, nil }
func (m *mockStore) CountActiveConcurrencyKeys(ctx context.Context) (int, error) { return 0, nil }
func (m *mockStore) DeleteDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) { return 0, nil }
func (m *mockStore) LoadEventHistoryBatch(ctx context.Context, workflowIDs []string) (map[string][]host.EventRecord, error) {
	return nil, nil
}
func (m *mockStore) StreamEventHistory(ctx context.Context, workflowID string, pageSize int) (<-chan host.EventRecord, <-chan error) {
	return nil, nil
}
func (m *mockStore) TerminateWorkflow(ctx context.Context, workflowID, reason string) error { return nil }
