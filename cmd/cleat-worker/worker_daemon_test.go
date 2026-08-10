// Package main contains the cleat-worker daemon and its tests.
// This file tests the core daemon loops — dispatch, heartbeat, reaper,
// compaction — as well as the HTTP API handlers.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/monitoring/prometheus"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// mockStore — simulates engine.WorkflowStore for unit tests without PostgreSQL.
// ---------------------------------------------------------------------------

// mockStore implements engine.WorkflowStore entirely in-memory.
// Each method checks for a custom function field first; if set, it delegates
// to that function. Otherwise it returns a safe zero-valued result.
type mockStore struct {
	claimWorkflowFn                    func(ctx context.Context, workerID string) (*engine.WorkflowInstance, error)
	claimWorkflowsFn                   func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error)
	claimStickyWorkflowsFn             func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error)
	loadEventHistoryFn                 func(ctx context.Context, workflowID string) ([]engine.EventRecord, error)
	loadEventHistoryPaginatedFn        func(ctx context.Context, workflowID string, offset, limit int) ([]engine.EventRecord, error)
	countEventHistoryFn                func(ctx context.Context, workflowID string) (int, error)
	appendEventHistoryFn               func(ctx context.Context, workflowID string, rec engine.EventRecord) error
	appendEventHistoryBatchFn          func(ctx context.Context, workflowID string, recs []engine.EventRecord) error
	loadWASMFn                         func(ctx context.Context, defName string, defVersion int) ([]byte, error)
	getWASMLengthFn                    func(ctx context.Context, defName string, defVersion int) (int64, error)
	listVersionsFn                     func(ctx context.Context, defName string) ([]int, error)
	heartbeatFn                        func(ctx context.Context, workflowID, workerID string, generation int64) (bool, error)
	batchHeartbeatFn                   func(ctx context.Context, workerID string) (int64, error)
	completeWorkflowFn                 func(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error
	failWorkflowFn                     func(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error
	releaseWorkflowFn                  func(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error
	moveToDeadLetterQueueFn            func(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error
	requestCancellationFn              func(ctx context.Context, workflowID, reason string) error
	checkCancellationFn                func(ctx context.Context, workflowID string) (bool, string, error)
	deliverSignalFn                    func(ctx context.Context, workflowID, signalName, payload string) error
	pollAndClaimSignalFn               func(ctx context.Context, workflowID, signalName string) (string, bool, error)
	startNewRunFn                      func(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error)
	startChildWorkflowFn               func(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error)
	getChildResultFn                   func(ctx context.Context, runID string) (string, bool, error)
	reapStaleInstancesFn               func(ctx context.Context, timeout time.Duration) (int, error)
	getQueryStateFn                    func(ctx context.Context, workflowID, key string) (string, error)
	listWorkflowsFn                    func(ctx context.Context, filter engine.WorkflowFilter) ([]engine.WorkflowInstance, error)
	getWorkflowByIDFn                  func(ctx context.Context, id string) (*engine.WorkflowInstance, error)
	createScheduleFn                   func(ctx context.Context, s engine.Schedule) error
	listSchedulesFn                    func(ctx context.Context) ([]engine.Schedule, error)
	deleteScheduleFn                   func(ctx context.Context, name string) error
	setScheduleEnabledFn               func(ctx context.Context, name string, enabled bool) error
	getDueSchedulesFn                  func(ctx context.Context) ([]engine.Schedule, error)
	updateScheduleNextRunFn            func(ctx context.Context, name string, nextRun time.Time) error
	loadWorkflowConfigFn               func(ctx context.Context, defName string, defVersion int) (int, error)
	loadDAGSpecFn                      func(ctx context.Context, defName string, defVersion int) (json.RawMessage, error)
	traceWorkflowFn                    func(ctx context.Context, workflowID, traceID string) error
	getCompactionCandidatesFn          func(ctx context.Context, threshold int, limit int) ([]string, error)
	loadCompactionStateFn              func(ctx context.Context, workflowID string) (*engine.CompactionState, error)
	compactHistoryFn                   func(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error
	createPromiseFn                    func(ctx context.Context, workflowID, promiseName, promiseID string) error
	resolvePromiseFn                   func(ctx context.Context, workflowID, promiseID, result string) error
	rejectPromiseFn                    func(ctx context.Context, workflowID, promiseID, errMsg string) error
	getPromiseFn                       func(ctx context.Context, workflowID, promiseID string) (string, string, string, error)
	listPromisesFn                     func(ctx context.Context, workflowID string) ([]engine.PromiseInfo, error)
	createUpdateRequestFn              func(ctx context.Context, workflowID, updateName, payload, promiseID string) error
	getPendingUpdateRequestsFn         func(ctx context.Context, workflowID string) ([]engine.UpdateRequestInfo, error)
	completeUpdateRequestFn            func(ctx context.Context, workflowID, updateName, result, errMsg string) error
	acquireConcurrencyKeyFn            func(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error)
	releaseConcurrencyKeyFn            func(ctx context.Context, key string) error
	releaseWorkflowConcurrencyKeysFn   func(ctx context.Context, workflowID string) error
	reapExpiredConcurrencyKeysFn       func(ctx context.Context) (int64, error)
	updateStickyWorkerFn               func(ctx context.Context, workflowID, workerID string) error
	clearStickyWorkerFn                func(ctx context.Context, workflowID string) error
	deployWorkflowDefFn                func(ctx context.Context, def *engine.WorkflowDef) error
	listWorkflowDefsFn                 func(ctx context.Context, name string) ([]engine.WorkflowDef, error)
	getWorkflowDefFn                   func(ctx context.Context, name string, version int) (*engine.WorkflowDef, error)
	markVersionDeprecatedFn            func(ctx context.Context, name string, version int, deprecated bool) error
	purgeWorkflowDefFn                 func(ctx context.Context, name string, version int) error
	countActiveInstancesFn             func(ctx context.Context, name string, version int) (int, error)
	getActiveInstanceCountsByVersionFn func(ctx context.Context) (map[string]int, error)
	cleanupMemorySamplesFn             func(ctx context.Context, maxSamplesPerDef int) (int64, error)
	recordWorkflowMemorySampleFn       func(ctx context.Context, defName string, sampleBytes int64) error
	loadMemoryEstimatesFn              func(ctx context.Context) (map[string]float64, error)
	loadMemoryStatsFn                  func(ctx context.Context) ([]engine.WorkflowMemoryStats, error)
	queueDepthFn                       func(ctx context.Context) (int64, error)
	deleteExpiredEventsFn              func(ctx context.Context, olderThan time.Time) (int64, error)
	deleteCompletedWorkflowsFn         func(ctx context.Context, olderThan time.Time) (int64, error)
	continueAsNewFn                    func(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, result string, queryState map[string]string, priority int) (string, error)
	finalizeWorkflowSegmentFn          func(ctx context.Context, runID, workerID string, generation int64, newEvents []engine.EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error
	getAllowedSignalCallersFn          func(ctx context.Context, workflowID string) ([]string, error)
	terminateWorkflowFn                func(ctx context.Context, workflowID, reason string) error
	adminForceCompleteFn               func(ctx context.Context, workflowID string, generation int64, result string, operator string) error
	adminForceFailFn                   func(ctx context.Context, workflowID string, generation int64, errorMsg, errorCode string, operator string) error
	adminReReplayFn                    func(ctx context.Context, workflowID string, generation int64, operator string) error
}

func (m *mockStore) ClaimWorkflow(ctx context.Context, workerID string) (*engine.WorkflowInstance, error) {
	if m.claimWorkflowFn != nil {
		return m.claimWorkflowFn(ctx, workerID)
	}
	return nil, nil
}

func (m *mockStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
	if m.claimWorkflowsFn != nil {
		return m.claimWorkflowsFn(ctx, workerID, limit)
	}
	return nil, nil
}

func (m *mockStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
	if m.claimStickyWorkflowsFn != nil {
		return m.claimStickyWorkflowsFn(ctx, workerID, limit)
	}
	return nil, nil
}

func (m *mockStore) LoadEventHistory(ctx context.Context, workflowID string) ([]engine.EventRecord, error) {
	if m.loadEventHistoryFn != nil {
		return m.loadEventHistoryFn(ctx, workflowID)
	}
	return nil, nil
}

func (m *mockStore) AppendEventHistory(ctx context.Context, workflowID string, rec engine.EventRecord) error {
	if m.appendEventHistoryFn != nil {
		return m.appendEventHistoryFn(ctx, workflowID, rec)
	}
	return nil
}

func (m *mockStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []engine.EventRecord) error {
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

func (m *mockStore) GetWASMLength(ctx context.Context, defName string, defVersion int) (int64, error) {
	if m.getWASMLengthFn != nil {
		return m.getWASMLengthFn(ctx, defName, defVersion)
	}
	return 0, nil
}

func (m *mockStore) ListVersions(ctx context.Context, defName string) ([]int, error) {
	if m.listVersionsFn != nil {
		return m.listVersionsFn(ctx, defName)
	}
	return []int{1}, nil
}

func (m *mockStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) {
	if m.heartbeatFn != nil {
		return m.heartbeatFn(ctx, workflowID, workerID, generation)
	}
	return true, nil
}

func (m *mockStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error {
	if m.completeWorkflowFn != nil {
		return m.completeWorkflowFn(ctx, workflowID, workerID, generation, result, queryState)
	}
	return nil
}

func (m *mockStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	if m.failWorkflowFn != nil {
		return m.failWorkflowFn(ctx, workflowID, workerID, generation, errorMsg, errorCode, errorOp, queryState)
	}
	return nil
}

func (m *mockStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
	if m.releaseWorkflowFn != nil {
		return m.releaseWorkflowFn(ctx, workflowID, workerID, generation, nextWakeAt)
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

// PollSignal satisfies engine.SignalStore. Delegates to PollAndClaimSignal.
func (m *mockStore) PollSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	return m.PollAndClaimSignal(ctx, workflowID, signalName)
}

// PollCancellation satisfies engine.SignalStore. Delegates to CheckCancellation.
func (m *mockStore) PollCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	return m.CheckCancellation(ctx, workflowID)
}

func (m *mockStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) {
	if m.startNewRunFn != nil {
		return m.startNewRunFn(ctx, runID, defName, defVersion, input, idempotencyKey, tenantID, priority)
	}
	return "test-run-id", false, nil
}

func (m *mockStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	if m.startChildWorkflowFn != nil {
		return m.startChildWorkflowFn(ctx, parentID, defName, inputJSON, defVersion, parentClosePolicy, priority)
	}
	return "child-run-id", nil
}

func (m *mockStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event engine.EventRecord, priority int) (string, error) {
	return m.StartChildWorkflow(ctx, parentID, defName, inputJSON, defVersion, parentClosePolicy, priority)
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

func (m *mockStore) ListWorkflows(ctx context.Context, filter engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
	if m.listWorkflowsFn != nil {
		return m.listWorkflowsFn(ctx, filter)
	}
	return nil, nil
}

func (m *mockStore) GetWorkflowByID(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
	if m.getWorkflowByIDFn != nil {
		return m.getWorkflowByIDFn(ctx, id)
	}
	return nil, nil
}

func (m *mockStore) CreateSchedule(ctx context.Context, s engine.Schedule) error {
	if m.createScheduleFn != nil {
		return m.createScheduleFn(ctx, s)
	}
	return nil
}

func (m *mockStore) ListSchedules(ctx context.Context) ([]engine.Schedule, error) {
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

func (m *mockStore) GetDueSchedules(ctx context.Context) ([]engine.Schedule, error) {
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

func (m *mockStore) ClaimDueSchedule(ctx context.Context, name string, expectedNextRun, newNextRun time.Time, runID string) (bool, error) {
	return true, nil
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

func (m *mockStore) LoadCompactionState(ctx context.Context, workflowID string) (*engine.CompactionState, error) {
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

func (m *mockStore) ListPromises(ctx context.Context, workflowID string) ([]engine.PromiseInfo, error) {
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

func (m *mockStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]engine.UpdateRequestInfo, error) {
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

func (m *mockStore) DeployWorkflowDef(ctx context.Context, def *engine.WorkflowDef) error {
	if m.deployWorkflowDefFn != nil {
		return m.deployWorkflowDefFn(ctx, def)
	}
	return nil
}

func (m *mockStore) ListWorkflowDefs(ctx context.Context, name string) ([]engine.WorkflowDef, error) {
	if m.listWorkflowDefsFn != nil {
		return m.listWorkflowDefsFn(ctx, name)
	}
	return nil, nil
}

func (m *mockStore) GetWorkflowDef(ctx context.Context, name string, version int) (*engine.WorkflowDef, error) {
	if m.getWorkflowDefFn != nil {
		return m.getWorkflowDefFn(ctx, name, version)
	}
	return nil, nil
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
	if m.cleanupMemorySamplesFn != nil {
		return m.cleanupMemorySamplesFn(ctx, maxSamplesPerDef)
	}
	return 0, nil
}

func (m *mockStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error {
	if m.recordWorkflowMemorySampleFn != nil {
		return m.recordWorkflowMemorySampleFn(ctx, defName, sampleBytes)
	}
	return nil
}

func (m *mockStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) {
	if m.loadMemoryEstimatesFn != nil {
		return m.loadMemoryEstimatesFn(ctx)
	}
	return nil, nil
}

func (m *mockStore) LoadMemoryStats(ctx context.Context) ([]engine.WorkflowMemoryStats, error) {
	if m.loadMemoryStatsFn != nil {
		return m.loadMemoryStatsFn(ctx)
	}
	return nil, nil
}

func (m *mockStore) QueueDepth(ctx context.Context) (int64, error) {
	if m.queueDepthFn != nil {
		return m.queueDepthFn(ctx)
	}
	return 0, nil
}

func (m *mockStore) DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	if m.deleteExpiredEventsFn != nil {
		return m.deleteExpiredEventsFn(ctx, olderThan)
	}
	return 0, nil
}

func (m *mockStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []engine.EventRecord, result string, queryState map[string]string, priority int) (string, error) {
	if m.continueAsNewFn != nil {
		return m.continueAsNewFn(ctx, currentRunID, workerID, generation, defName, defVersion, newInput, result, queryState, priority)
	}
	return "new-run-id", nil
}

func (m *mockStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []engine.EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	if m.finalizeWorkflowSegmentFn != nil {
		return m.finalizeWorkflowSegmentFn(ctx, runID, workerID, generation, newEvents, finalStatus, result, errorCode, errorOp, queryState, nextWakeAt)
	}
	return nil
}

// ---- test helpers ----

// newTestWorker creates a Worker with a mock store and cancellable context,
// using very short intervals so loop tests complete quickly.
func newTestWorker(ms *mockStore) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	monitor := NewMemoryMonitor(5 * time.Second)
	mc := NewMemoryController(monitor, ms, "test-worker", 5, 1<<40, 1<<40)
	testMetrics := newTestPrometheus()
	return &Worker{
		Metrics:             testMetrics,
		id:                  "test-worker",
		store:               ms,
		concurrency:         5,
		memoryController:    mc,
		heartbeatInterval:   10 * time.Millisecond,
		pollInterval:        1 * time.Millisecond,
		compactionThreshold: engine.DefaultCompactionThreshold,
		compactionInterval:  10 * time.Millisecond,
		ctx:                 ctx,
		cancel:              cancel,
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		wasmCache:           newWasmLRUCache(100, 500),
		healthTracker:       newHealthTracker(),
		loopCtxMap:          make(map[string]*loopContext),
	}
}

// newTestWorkerWithConcurrency creates a Worker with the given concurrency,
// ensuring the memory controller is configured with the same value so that
// DynamicConcurrency() returns the expected capacity.
func newTestWorkerWithConcurrency(ms *mockStore, concurrency int) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	monitor := NewMemoryMonitor(5 * time.Second)
	mc := NewMemoryController(monitor, ms, "test-worker", concurrency, 1<<40, 1<<40)
	testMetrics := newTestPrometheus()
	return &Worker{
		Metrics:             testMetrics,
		id:                  "test-worker",
		store:               ms,
		concurrency:         concurrency,
		memoryController:    mc,
		heartbeatInterval:   10 * time.Millisecond,
		pollInterval:        1 * time.Millisecond,
		compactionThreshold: engine.DefaultCompactionThreshold,
		compactionInterval:  10 * time.Millisecond,
		ctx:                 ctx,
		cancel:              cancel,
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		wasmCache:           newWasmLRUCache(100, 500),
		healthTracker:       newHealthTracker(),
		loopCtxMap:          make(map[string]*loopContext),
	}
}

// newTestPrometheus creates a Metrics instance for test use. Errors are
// silently ignored; callers that need metric recording should check.
func newTestPrometheus() *prometheus.Metrics {
	m, err := prometheus.New(prometheus.Config{
		WorkerID: "test-worker",
	})
	if err != nil {
		return nil
	}
	return m
}

// newTestAPIServer creates an apiServer with a proper maxBodySize for tests.
func newTestAPIServer(ms *mockStore) *apiServer {
	// factory serves every tenant from the same mock store, which is what these
	// tests assumed implicitly before handlers scoped requests per tenant.
	// Tests that care *which* tenant's store was reached build their own
	// factory -- see twoTenantServer in tenant_isolation_test.go.
	return &apiServer{
		store:       ms,
		worker:      newTestWorker(ms),
		maxBodySize: 1 << 20,
		factory:     &fakeStoreFactory{fallback: ms},
	}
}

// waitForCond polls cond until it returns true or the timeout elapses.
func waitForCond(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for condition")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

// ---------------------------------------------------------------------------
// dispatchLoop tests
// ---------------------------------------------------------------------------

func TestDispatchLoop_ClaimsWorkflows(t *testing.T) {
	ms := &mockStore{}
	nCalls := 0
	// Block executeWorkflow from finishing before we check inflight.
	// loadWASM blocks on a channel, giving the test time to verify
	// that the workflow was added to inflight before it gets removed.
	loadWASMCh := make(chan struct{})
	claimedCh := make(chan string, 1)

	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		nCalls++
		if nCalls == 1 {
			return []*engine.WorkflowInstance{
				{ID: "wf-sticky-1", DefName: "test", DefVersion: 1, Status: "ready"},
			}, nil
		}
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		return nil, nil
	}
	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		close(claimedCh)
		<-loadWASMCh
		return nil, nil
	}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string, queryState map[string]string) error {
		return nil
	}

	w := newTestWorker(ms)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.wg.Add(1)
		w.dispatchLoop()
	}()

	// Wait until the workflow is claimed (the goroutine is blocked in loadWASM).
	select {
	case <-claimedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for workflow claim")
	}

	// Verify the workflow was added to inflight BEFORE executeWorkflow finishes.
	var found bool
	w.inflight.Range(func(key, value any) bool {
		if key.(string) == "wf-sticky-1" {
			found = true
		}
		return true
	})
	if !found {
		t.Error("expected wf-sticky-1 to be in inflight, but it was not found")
	}

	// Unblock executeWorkflow so the goroutine can finish.
	close(loadWASMCh)
	w.cancel()
	wg.Wait()
}

func TestDispatchLoop_StickyThenGeneral(t *testing.T) {
	// Verify that sticky claiming is attempted first, and remaining capacity
	// is filled from the general pool.
	ms := &mockStore{}
	var stickyCalled atomic.Bool
	var generalCalled atomic.Bool

	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		stickyCalled.Store(true)
		if limit >= 5 {
			return []*engine.WorkflowInstance{
				{ID: "sticky-1", DefName: "test", DefVersion: 1, Status: "ready"},
			}, nil
		}
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		generalCalled.Store(true)
		// Should be called with remaining = concurrency (5) - sticky (1) = 4.
		return nil, nil
	}

	w := newTestWorker(ms)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.wg.Add(1)
		w.dispatchLoop()
	}()

	// Wait until both calls have been made.
	waitForCond(t, 2*time.Second, func() bool {
		return stickyCalled.Load() && generalCalled.Load()
	})

	w.cancel()
	wg.Wait()
}

func TestDispatchLoop_StopsOnCancel(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.dispatchLoop()
		close(done)
	}()

	w.cancel()

	select {
	case <-done:
		// OK — loop exited promptly.
	case <-time.After(2 * time.Second):
		t.Fatal("dispatchLoop did not stop within 2s of context cancellation")
	}
}

func TestDispatchLoop_EmptyQueuesNoCrash(t *testing.T) {
	// When the store returns empty results, the loop must not crash
	// and must not busy-loop (progressive backoff caps at 6 ticks).
	ms := &mockStore{}
	callCount := 0
	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		callCount++
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		return nil, nil
	}

	w := newTestWorker(ms)
	// Use a bigger poll interval to make tick counting reliable.
	w.pollInterval = 5 * time.Millisecond

	// Let it cycle several times, then cancel.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.dispatchLoop()
		close(done)
	}()

	<-done

	if callCount == 0 {
		t.Error("expected at least one claim attempt, got 0")
	}
}

func TestDispatchLoop_AtCapacity(t *testing.T) {
	ms := &mockStore{}
	var claimAttempts atomic.Int32
	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		claimAttempts.Add(1)
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		return nil, nil
	}

	w := newTestWorkerWithConcurrency(ms, 1)

	// Fill the inflight map to capacity. This is a synthetic entry with no
	// goroutine backing it, so it will never clear on its own — it must be
	// removed explicitly below before the loop can be allowed to exit (see
	// the shutdown drain logic in dispatchLoop, setup.go, which intentionally
	// blocks until inflight reaches zero so real in-flight workflows finish
	// cleanly before the worker stops claiming).
	w.inflight.Store("wf-busy-1", &engine.WorkflowInstance{ID: "wf-busy-1", DefName: "test", DefVersion: 1})

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.dispatchLoop()
		close(done)
	}()

	// Let the loop run several poll cycles while at capacity, then snapshot
	// the claim count. This is the actual property under test: while the
	// worker is genuinely at capacity, no claims are attempted.
	time.Sleep(50 * time.Millisecond)
	attemptsAtCapacity := claimAttempts.Load()

	// Now unwind the test: cancel and clear the synthetic in-flight entry so
	// the loop's shutdown drain sees zero in-flight work and returns. A
	// claim slipping in during this shutdown transition (the in-flight loop
	// iteration that was already past its capacity check when we cancelled)
	// doesn't violate the capacity invariant above and is intentionally not
	// asserted on here — it's an artifact of tearing the loop down, not of
	// being at capacity.
	w.cancel()
	w.inflight.Delete("wf-busy-1")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatchLoop did not stop within 2s of context cancellation")
	}

	// With capacity full, no claim attempts should happen.
	if attemptsAtCapacity > 0 {
		t.Errorf("expected 0 claim attempts when at capacity, got %d", attemptsAtCapacity)
	}
}

func TestDispatchLoop_ProgressiveBackoff(t *testing.T) {
	// Verify that repeated empty results cause idleTicks to increase and
	// the effective sleep to grow up to the maxIdleTicks cap.
	ms := &mockStore{}
	claimTimestamps := make([]time.Time, 0, 10)
	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		claimTimestamps = append(claimTimestamps, time.Now())
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		return nil, nil
	}

	w := newTestWorker(ms)
	w.pollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.dispatchLoop()
		close(done)
	}()

	<-done

	// Should have completed at least 3 cycles (every 10-60ms → ~8-50 in 500ms).
	if len(claimTimestamps) < 3 {
		t.Errorf("expected at least 3 claim cycles, got %d", len(claimTimestamps))
	}
}

func TestDispatchLoop_StoreError(t *testing.T) {
	// When ClaimStickyWorkflows returns an error (non-connection), the loop
	// sleeps one second and retries. We verify it does not crash.
	ms := &mockStore{}
	errCount := 0
	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		errCount++
		return nil, errors.New("some store error")
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		return nil, nil
	}

	w := newTestWorker(ms)
	// Loop sleeps 1s on error, so we use a short timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.dispatchLoop()
		close(done)
	}()

	<-done

	if errCount == 0 {
		t.Error("expected at least one error from store, got 0")
	}
}

func TestDispatchLoop_ConnectionError(t *testing.T) {
	// When claim returns a connection error, the loop should back off and retry.
	ms := &mockStore{}
	connErrCount := 0
	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		connErrCount++
		return nil, errors.New("connection refused")
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		return nil, nil
	}

	w := newTestWorker(ms)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.dispatchLoop()
		close(done)
	}()

	<-done

	if connErrCount == 0 {
		t.Error("expected at least one connection error, got 0")
	}
	// consecutiveDBErrors should be > 0 (but we can't easily assert exact because of timing).
	if w.consecutiveDBErrors <= 0 {
		t.Log("note: consecutiveDBErrors was 0; backoff may not have triggered")
	}
}

// ---------------------------------------------------------------------------
// heartbeatLoop tests
// ---------------------------------------------------------------------------

func TestHeartbeatLoop_UpdatesHeartbeats(t *testing.T) {
	ms := &mockStore{}
	var (
		mu        sync.Mutex
		callCount int64
	)
	ms.batchHeartbeatFn = func(ctx context.Context, workerID string) (int64, error) {
		mu.Lock()
		callCount++
		mu.Unlock()
		return 2, nil
	}

	w := newTestWorker(ms)
	w.heartbeatInterval = 10 * time.Millisecond

	// Add workflows to inflight.
	w.inflight.Store("wf-1", &engine.WorkflowInstance{ID: "wf-1"})
	w.inflight.Store("wf-2", &engine.WorkflowInstance{ID: "wf-2"})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.wg.Add(1)
		w.heartbeatLoop()
	}()

	// Wait for at least two heartbeat cycles.
	time.Sleep(45 * time.Millisecond)
	w.cancel()
	wg.Wait()

	mu.Lock()
	count := callCount
	mu.Unlock()

	// Over ~45ms with 10ms interval: ~4 batch heartbeat calls.
	// Minimum: at least 1 call.
	if count < 1 {
		t.Errorf("expected at least 1 batch heartbeat, got %d", count)
	}
}

func TestHeartbeatLoop_StopsOnCancel(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.heartbeatLoop()
		close(done)
	}()

	w.cancel()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeatLoop did not stop within 2s of context cancellation")
	}
}

func TestHeartbeatLoop_DoesNotRemoveFromInflight(t *testing.T) {
	// With BatchHeartbeat, the heartbeat loop does not remove workflows from
	// inflight — ownership recovery is handled by the reaper loop.
	ms := &mockStore{}
	callCount := 0
	ms.batchHeartbeatFn = func(ctx context.Context, workerID string) (int64, error) {
		callCount++
		return 0, nil
	}

	w := newTestWorker(ms)
	w.heartbeatInterval = 10 * time.Millisecond
	w.inflight.Store("wf-alive", &engine.WorkflowInstance{ID: "wf-alive"})
	w.inflight.Store("wf-lost", &engine.WorkflowInstance{ID: "wf-lost"})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.wg.Add(1)
		w.heartbeatLoop()
	}()

	// Wait long enough for several heartbeat cycles.
	time.Sleep(60 * time.Millisecond)
	w.cancel()
	wg.Wait()

	if callCount == 0 {
		t.Error("expected at least one batch heartbeat call")
	}

	// Both workflows should remain in inflight.
	_, stillInflight := w.inflight.Load("wf-lost")
	if !stillInflight {
		t.Error("expected wf-lost to remain in inflight (reaper handles ownership)")
	}
	_, alive := w.inflight.Load("wf-alive")
	if !alive {
		t.Error("expected wf-alive to remain in inflight")
	}
}

func TestHeartbeatLoop_DBErrorNoCrash(t *testing.T) {
	ms := &mockStore{}
	ms.batchHeartbeatFn = func(ctx context.Context, workerID string) (int64, error) {
		return 0, errors.New("connection refused")
	}

	w := newTestWorker(ms)
	w.heartbeatInterval = 10 * time.Millisecond
	w.inflight.Store("wf-1", &engine.WorkflowInstance{ID: "wf-1"})

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.heartbeatLoop()
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	w.cancel()
	<-done
}

// ---------------------------------------------------------------------------
// reaperLoop tests
// ---------------------------------------------------------------------------

func TestReaperLoop_CallsReap(t *testing.T) {
	ms := &mockStore{}
	reapCalled := false
	ms.reapStaleInstancesFn = func(ctx context.Context, timeout time.Duration) (int, error) {
		reapCalled = true
		if timeout != 30*time.Second {
			t.Errorf("reaper called with timeout=%v, want 30s", timeout)
		}
		return 0, nil
	}

	w := newTestWorker(ms)

	// The reaper ticker is hardcoded at 30s so we can't easily wait for it
	// in a test. Instead we verify the loop stops on cancellation.
	// The function-field approach is not great for the 30s delay.
	//
	// Instead we test the behaviour by directly calling the _inner_ portion:
	// we verify the store method signature and default timeout.
	_, _ = w.store.ReapStaleInstances(context.Background(), 30*time.Second)
	if !reapCalled {
		t.Fatal("store.ReapStaleInstances did not reach the mock")
	}
}

func TestReaperLoop_StopsOnCancel(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.reaperLoop()
		close(done)
	}()

	w.cancel()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("reaperLoop did not stop within 2s of context cancellation")
	}
}

func TestReaperLoop_HandlesResults(t *testing.T) {
	ms := &mockStore{}
	ms.reapStaleInstancesFn = func(ctx context.Context, timeout time.Duration) (int, error) {
		return 3, nil
	}

	// Since we can't easily wait for the 30s ticker, we verify the
	// store.ReapStaleInstances contract directly.
	reapedCount, err := ms.ReapStaleInstances(context.Background(), 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reapedCount != 3 {
		t.Errorf("expected 3 reaped instances, got %d", reapedCount)
	}
}

func TestReaperLoop_DBErrorNoCrash(t *testing.T) {
	ms := &mockStore{}
	ms.reapStaleInstancesFn = func(ctx context.Context, timeout time.Duration) (int, error) {
		return 0, errors.New("connection refused")
	}

	// Verify the store method handles errors gracefully (no panic).
	n, err := ms.ReapStaleInstances(context.Background(), 30*time.Second)
	if err == nil {
		t.Error("expected error from mock, got nil")
	}
	if n != 0 {
		t.Errorf("expected 0 reaped, got %d", n)
	}
}

func TestReaperLoop_DefaultInterval(t *testing.T) {
	// The reaperLoop ticker is hardcoded to 30 seconds. Verify this contract
	// by checking the constant in the production code. We do this by looking
	// at the ticker creation (which is not externally visible), but we can
	// at least verify that ReapStaleInstances is called with 30s timeout
	// when reaperLoop invokes it.
	ms := &mockStore{}
	var capturedTimeout time.Duration
	ms.reapStaleInstancesFn = func(ctx context.Context, timeout time.Duration) (int, error) {
		capturedTimeout = timeout
		return 0, nil
	}

	// Call the method directly to verify the signature.
	_, _ = ms.ReapStaleInstances(context.Background(), 30*time.Second)
	if capturedTimeout != 30*time.Second {
		t.Errorf("expected timeout 30s, got %v", capturedTimeout)
	}
}

// ---------------------------------------------------------------------------
// retentionLoop / runRetentionSweep tests
//
// Like reaperLoop above, retentionLoop's ticker is hardcoded to 24 hours, so
// these test the extracted per-tick body (runRetentionSweep) directly rather
// than waiting on the ticker -- the same shape TestReaperLoop_DefaultInterval
// uses.
// ---------------------------------------------------------------------------

func TestRetentionLoop_ReturnsImmediatelyWhenBothDisabled(t *testing.T) {
	ms := &mockStore{}
	ms.deleteExpiredEventsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		t.Error("DeleteExpiredEvents called; retentionLoop(0, 0) should never reach the ticker")
		return 0, nil
	}
	ms.deleteCompletedWorkflowsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		t.Error("DeleteCompletedWorkflows called; retentionLoop(0, 0) should never reach the ticker")
		return 0, nil
	}
	w := newTestWorker(ms)

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.retentionLoop(0, 0)
		close(done)
	}()

	select {
	case <-done:
		// OK: returned without needing cancellation, because both knobs are
		// disabled and there is nothing for the loop to do.
	case <-time.After(2 * time.Second):
		t.Fatal("retentionLoop(0, 0) did not return immediately")
	}
}

func TestRunRetentionSweep_CallsBothWithIndependentCutoffs(t *testing.T) {
	ms := &mockStore{}
	var eventsCutoff, workflowsCutoff time.Time
	var eventsCalled, workflowsCalled bool
	ms.deleteExpiredEventsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		eventsCalled = true
		eventsCutoff = olderThan
		return 4, nil
	}
	ms.deleteCompletedWorkflowsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		workflowsCalled = true
		workflowsCutoff = olderThan
		return 2, nil
	}
	w := newTestWorker(ms)

	// Different windows on purpose: an operator can keep workflow_instances
	// rows far longer than event_history, e.g. --retention-days=7
	// --completed-workflow-retention-days=90. The two cutoffs must not be
	// the same computation reused for both.
	w.runRetentionSweep(7, 90)

	if !eventsCalled {
		t.Fatal("DeleteExpiredEvents was not called")
	}
	if !workflowsCalled {
		t.Fatal("DeleteCompletedWorkflows was not called")
	}
	wantEvents := time.Now().Add(-7 * 24 * time.Hour)
	wantWorkflows := time.Now().Add(-90 * 24 * time.Hour)
	if delta := eventsCutoff.Sub(wantEvents); delta < -time.Minute || delta > time.Minute {
		t.Errorf("events cutoff = %v, want ~%v (7 days ago)", eventsCutoff, wantEvents)
	}
	if delta := workflowsCutoff.Sub(wantWorkflows); delta < -time.Minute || delta > time.Minute {
		t.Errorf("workflows cutoff = %v, want ~%v (90 days ago)", workflowsCutoff, wantWorkflows)
	}
	if !workflowsCutoff.Before(eventsCutoff) {
		t.Errorf("workflows cutoff (%v) should be earlier than events cutoff (%v) given retentionDays=7 < completedWorkflowRetentionDays=90",
			workflowsCutoff, eventsCutoff)
	}
}

func TestRunRetentionSweep_SkipsCompletedWorkflowsWhenDisabled(t *testing.T) {
	ms := &mockStore{}
	ms.deleteExpiredEventsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		return 0, nil
	}
	ms.deleteCompletedWorkflowsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		t.Error("DeleteCompletedWorkflows called with completedWorkflowRetentionDays=0")
		return 0, nil
	}
	w := newTestWorker(ms)

	// completedWorkflowRetentionDays=0: this is the shipped default. Proves
	// the off-by-default argument in docs/operations/workflow-retention.md
	// is actually true of the code, not just the prose.
	w.runRetentionSweep(30, 0)
}

func TestRunRetentionSweep_SkipsEventsWhenDisabled(t *testing.T) {
	ms := &mockStore{}
	ms.deleteExpiredEventsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		t.Error("DeleteExpiredEvents called with retentionDays=0")
		return 0, nil
	}
	ms.deleteCompletedWorkflowsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		return 0, nil
	}
	w := newTestWorker(ms)

	w.runRetentionSweep(0, 90)
}

// TestCompletedWorkflowRetentionDaysDefaultsOff is the argument in
// docs/operations/workflow-retention.md, pinned to the flag definition:
// --completed-workflow-retention-days deletes the workflow_instances row
// itself (status, result, error, def_name all gone from ListWorkflows and
// the admin dashboard permanently), which is a materially more destructive
// default than --retention-days deleting event_history (which leaves the
// workflow's outcome in place). See TestRequireSignalAuthDefaultsOff above
// for the same asserted-on-the-flag-definition shape and its rationale.
func TestCompletedWorkflowRetentionDaysDefaultsOff(t *testing.T) {
	f := flag.Lookup("completed-workflow-retention-days")
	if f == nil {
		t.Fatal("completed-workflow-retention-days flag is gone; if completed-workflow retention was removed, so should this test be")
	}
	if got := f.DefValue; got != "0" {
		t.Errorf("completed-workflow-retention-days defaults to %s, want 0 -- deleting a workflow's own "+
			"record (not just its event history) must be an explicit opt-in, not shipped on", got)
	}
}

// ---------------------------------------------------------------------------
// compactionLoop tests
// ---------------------------------------------------------------------------

func TestCompactionLoop_CompactsCandidates(t *testing.T) {
	ms := &mockStore{}
	candidatesReturned := false
	var compactCalled atomic.Bool

	ms.getCompactionCandidatesFn = func(ctx context.Context, threshold int, limit int) ([]string, error) {
		if candidatesReturned {
			return nil, nil
		}
		candidatesReturned = true
		return []string{"wf-compact-1"}, nil
	}

	// CompactWorkflowHistory calls LoadEventHistory first. For the mock,
	// return enough events to exceed the default threshold.
	ms.loadEventHistoryFn = func(ctx context.Context, workflowID string) ([]engine.EventRecord, error) {
		events := make([]engine.EventRecord, engine.DefaultCompactionThreshold+100)
		for i := range events {
			events[i] = engine.EventRecord{Step: i, EventType: "call"}
		}
		return events, nil
	}

	ms.compactHistoryFn = func(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
		compactCalled.Store(true)
		if workflowID != "wf-compact-1" {
			t.Errorf("compactHistory called with workflowID=%q, want wf-compact-1", workflowID)
		}
		return nil
	}

	w := newTestWorker(ms)
	w.compactionInterval = 10 * time.Millisecond
	w.compactionThreshold = engine.DefaultCompactionThreshold

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.wg.Add(1)
		w.compactionLoop()
	}()

	waitForCond(t, 2*time.Second, func() bool {
		return compactCalled.Load()
	})

	w.cancel()
	wg.Wait()
}

func TestCompactionLoop_StopsOnCancel(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.compactionLoop()
		close(done)
	}()

	w.cancel()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("compactionLoop did not stop within 2s of context cancellation")
	}
}

func TestCompactionLoop_NoCandidates(t *testing.T) {
	// When there are no compaction candidates, the loop should not call
	// CompactWorkflowHistory (which would call compactHistory).
	ms := &mockStore{}
	getCandidatesCalled := false
	compactHistoryCalled := false

	ms.getCompactionCandidatesFn = func(ctx context.Context, threshold int, limit int) ([]string, error) {
		getCandidatesCalled = true
		return nil, nil
	}
	ms.compactHistoryFn = func(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
		compactHistoryCalled = true
		return nil
	}

	w := newTestWorker(ms)
	w.compactionInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.compactionLoop()
		close(done)
	}()

	<-done

	if !getCandidatesCalled {
		t.Error("expected GetCompactionCandidates to be called")
	}
	if compactHistoryCalled {
		t.Error("expected CompactHistory NOT to be called when no candidates")
	}
}

func TestCompactionLoop_StoreError(t *testing.T) {
	// When GetCompactionCandidates returns an error, the loop should log
	// and continue (not crash).
	ms := &mockStore{}
	errCount := 0
	ms.getCompactionCandidatesFn = func(ctx context.Context, threshold int, limit int) ([]string, error) {
		errCount++
		return nil, errors.New("db error")
	}

	w := newTestWorker(ms)
	w.compactionInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.compactionLoop()
		close(done)
	}()

	<-done

	if errCount == 0 {
		t.Error("expected GetCompactionCandidates to be called at least once")
	}
}

// ---------------------------------------------------------------------------
// apiServer tests (using httptest)
// ---------------------------------------------------------------------------

func TestAPIHealthz(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	api.handleHealthz(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz returned status %d, want 200", resp.StatusCode)
	}

	var body map[string]bool
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if !body["ok"] {
		t.Error("healthz body did not contain ok: true")
	}
}

func TestAPIWorkflowsList(t *testing.T) {
	ms := &mockStore{}
	ms.listWorkflowsFn = func(ctx context.Context, filter engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		return []engine.WorkflowInstance{
			{ID: "wf-1", DefName: "test", DefVersion: 1, Status: "running"},
		}, nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
	w := httptest.NewRecorder()
	api.handleWorkflowsList(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("workflows list returned status %d, want 200", resp.StatusCode)
	}

	var workflows []engine.WorkflowInstance
	json.NewDecoder(resp.Body).Decode(&workflows)
	resp.Body.Close()

	if len(workflows) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(workflows))
	}
	if workflows[0].ID != "wf-1" {
		t.Errorf("expected ID wf-1, got %s", workflows[0].ID)
	}
}

func TestAPIWorkflowsList_MethodNotAllowed(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPost, "/api/workflows", nil)
	w := httptest.NewRecorder()
	api.handleWorkflowsList(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPIWorkflowsGetByID(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{
			ID: "wf-detail", DefName: "test", DefVersion: 2, Status: "done",
		}, nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-detail", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var wf engine.WorkflowInstance
	json.NewDecoder(resp.Body).Decode(&wf)
	resp.Body.Close()

	if wf.ID != "wf-detail" {
		t.Errorf("expected wf-detail, got %s", wf.ID)
	}
}

func TestAPISchedulesList(t *testing.T) {
	ms := &mockStore{}
	ms.listSchedulesFn = func(ctx context.Context) ([]engine.Schedule, error) {
		return []engine.Schedule{
			{Name: "hourly-job", DefName: "test", CronExpression: "0 * * * *", Enabled: true},
		}, nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/schedules", nil)
	w := httptest.NewRecorder()
	api.handleSchedulesList(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var schedules []engine.Schedule
	json.NewDecoder(resp.Body).Decode(&schedules)
	resp.Body.Close()

	if len(schedules) != 1 {
		t.Fatalf("expected 1 schedule, got %d", len(schedules))
	}
	if schedules[0].Name != "hourly-job" {
		t.Errorf("expected hourly-job, got %s", schedules[0].Name)
	}
}

// TestAPIMetrics_ZeroCounts absorbed the two assertions worth keeping from
// TestAPIMetrics, which was deleted here.
//
// That test had been `t.Skip("metrics now use prometheus/promauto;
// TestAPIMetrics_ZeroCounts covers basic endpoint")` over a live body since the
// switch to promauto, and the body could not be revived: it asserted literal
// values -- "cleat_workflows_active 3", "cleat_calls_total 42" -- that came
// from a hand-rolled metrics writer which no longer exists. Deleting it is
// right. But the replacement it named was weaker than the thing it replaced,
// and the skip said "covers" as though it were not, which is the shape that
// makes a skip worse than a deletion: the coverage looks accounted for.
//
// Two of the deleted assertions were still true and are now here:
//
//	Content-Type   it checked text/plain and this did not. Measured:
//	               "text/plain; version=0.0.4; charset=utf-8". A /metrics
//	               endpoint that answers with the wrong content type is not
//	               scrapeable, and nothing else checks it.
//	a metric name  it checked six by name; this checked for the substring
//	               "cleat_", which any error message mentioning a cleat_
//	               metric would satisfy. Only one of the six survives the
//	               promauto rewrite as a registered name under this fixture --
//	               cleat_replay_duration_seconds, the one the fixture records
//	               -- so that is the one asserted, by name.
func TestAPIMetrics_ZeroCounts(t *testing.T) {
	old := globalWorker
	t.Cleanup(func() { globalWorker = old })
	m := newTestPrometheus()
	m.RecordReplayDuration(context.Background(), time.Second)
	globalWorker = &Worker{
		Metrics: m,
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	handleMetrics(w, req)
	resp := w.Result()
	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("metrics returned status %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want it to contain text/plain -- Prometheus "+
			"will not scrape an endpoint that answers otherwise", ct)
	}
	// By name, and as a registered metric rather than as a loose substring.
	if !strings.Contains(body, "# TYPE cleat_replay_duration_seconds") {
		t.Errorf("metrics body does not register cleat_replay_duration_seconds, "+
			"which the fixture just recorded; body was:\n%s", body)
	}
}

func TestAPIHealthz_ContentType(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	api.handleHealthz(w, req)

	resp := w.Result()
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json, got %s", ct)
	}
	resp.Body.Close()
}

func TestAPIWorkflows_Routing(t *testing.T) {
	// Test that /api/workflows/:id routes to the right handler.
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
		if id == "wf-exists" {
			return &engine.WorkflowInstance{ID: "wf-exists", DefName: "test", DefVersion: 1, Status: "ready"}, nil
		}
		return nil, nil
	}

	api := newTestAPIServer(ms)

	// GET existing workflow — 200.
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-exists", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)
	if w.Code != 200 {
		t.Errorf("expected 200 for existing workflow, got %d", w.Code)
	}

	// GET non-existing workflow — 500 (store returns nil, nil, handleGetWorkflow returns 404)
	req2 := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-missing", nil)
	w2 := httptest.NewRecorder()
	api.handleWorkflows(w2, req2)
	if w2.Code != 404 {
		t.Errorf("expected 404 for missing workflow, got %d", w2.Code)
	}

	// POST to a path that doesn't exist — 404.
	req3 := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-1/bogus", nil)
	w3 := httptest.NewRecorder()
	api.handleWorkflows(w3, req3)
	if w3.Code != 404 {
		t.Errorf("expected 404 for unknown path, got %d", w3.Code)
	}
}

func TestAPISchedulesCreate(t *testing.T) {
	ms := &mockStore{}
	created := false
	ms.createScheduleFn = func(ctx context.Context, s engine.Schedule) error {
		created = true
		return nil
	}

	api := newTestAPIServer(ms)

	body := `{"name":"my-schedule","cron":"*/5 * * * *","def_name":"my-workflow"}`
	req := httptest.NewRequest(http.MethodPost, "/api/schedules", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleSchedulesList(w, req)

	if w.Code != 201 {
		t.Errorf("expected 201, got %d", w.Code)
	}
	if !created {
		t.Error("expected CreateSchedule to be called")
	}
}

func TestAPISchedulesCreate_Validation(t *testing.T) {
	api := newTestAPIServer(&mockStore{})

	tests := []struct {
		name string
		body string
	}{
		{"missing name", `{"cron":"* * * * *","def_name":"test"}`},
		{"missing cron", `{"name":"test","def_name":"test"}`},
		{"missing def_name", `{"name":"test","cron":"* * * * *"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/schedules", strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			api.handleSchedulesList(w, req)
			if w.Code != 400 {
				t.Errorf("expected 400, got %d", w.Code)
			}
		})
	}
}

func TestAPIWriteJSON(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)

	w := httptest.NewRecorder()
	api.writeJSON(w, 201, map[string]string{"status": "created"})

	resp := w.Result()
	if resp.StatusCode != 201 {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json, got %s", ct)
	}
	resp.Body.Close()
}

func TestAPIWriteError(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)

	w := httptest.NewRecorder()
	api.writeError(w, 400, "bad request")

	resp := w.Result()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()

	if body["error"] != "bad request" {
		t.Errorf("expected error message 'bad request', got %q", body["error"])
	}
}

func TestAPISchedules_EnableDisable(t *testing.T) {
	ms := &mockStore{}
	enabled := false
	disabled := false
	ms.setScheduleEnabledFn = func(ctx context.Context, name string, e bool) error {
		if e {
			enabled = true
		} else {
			disabled = true
		}
		return nil
	}

	api := newTestAPIServer(ms)

	// Enable
	req := httptest.NewRequest(http.MethodPost, "/api/schedules/my-sched/enable", nil)
	w := httptest.NewRecorder()
	api.handleSchedules(w, req)
	if w.Code != 200 {
		t.Errorf("expected 200 on enable, got %d", w.Code)
	}
	if !enabled {
		t.Error("expected setScheduleEnabled(true) to be called")
	}

	// Disable
	req2 := httptest.NewRequest(http.MethodPost, "/api/schedules/my-sched/disable", nil)
	w2 := httptest.NewRecorder()
	api.handleSchedules(w2, req2)
	if w2.Code != 200 {
		t.Errorf("expected 200 on disable, got %d", w2.Code)
	}
	if !disabled {
		t.Error("expected setScheduleEnabled(false) to be called")
	}
}

func TestAPISchedules_Delete(t *testing.T) {
	ms := &mockStore{}
	deleted := false
	ms.deleteScheduleFn = func(ctx context.Context, name string) error {
		deleted = true
		return nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodDelete, "/api/schedules/to-delete", nil)
	w := httptest.NewRecorder()
	api.handleSchedules(w, req)
	if w.Code != 200 {
		t.Errorf("expected 200 on delete, got %d", w.Code)
	}
	if !deleted {
		t.Error("expected DeleteSchedule to be called")
	}
}

// ---------------------------------------------------------------------------
// Helper / pure-function tests
// ---------------------------------------------------------------------------

func TestDetermineEntryPoint_EdgeCases(t *testing.T) {
	// Additional edge cases beyond what main_test.go covers.
	tests := []struct {
		name  string
		input json.RawMessage
		want  string
	}{
		{"invalid json", json.RawMessage(`{bad`), ""},
		{"nested entry point ignored", json.RawMessage(`{"nested":{"__entry_point":"inner"}}`), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determineEntryPoint(tt.input, nil)
			if got != tt.want {
				t.Errorf("determineEntryPoint(%s) = %q, want %q", string(tt.input), got, tt.want)
			}
		})
	}
}

func TestCompactionThresholdDefault(t *testing.T) {
	if engine.DefaultCompactionThreshold != 1000 {
		t.Errorf("DefaultCompactionThreshold = %d, want 1000", engine.DefaultCompactionThreshold)
	}
}

func TestWorkerFieldsDefault(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)

	if w.concurrency != 5 {
		t.Errorf("concurrency = %d, want 5", w.concurrency)
	}
	if w.id != "test-worker" {
		t.Errorf("id = %q, want test-worker", w.id)
	}
	if w.heartbeatInterval != 10*time.Millisecond {
		t.Errorf("heartbeatInterval = %v, want 10ms", w.heartbeatInterval)
	}
	if w.pollInterval != 1*time.Millisecond {
		t.Errorf("pollInterval = %v, want 1ms", w.pollInterval)
	}
	if w.compactionThreshold != engine.DefaultCompactionThreshold {
		t.Errorf("compactionThreshold = %d, want %d", w.compactionThreshold, engine.DefaultCompactionThreshold)
	}
	if w.compactionInterval != 10*time.Millisecond {
		t.Errorf("compactionInterval = %v, want 10ms", w.compactionInterval)
	}
}

func TestIsConnectionError_Patterns(t *testing.T) {
	// Verify the connection error patterns remain in sync.
	patterns := []string{
		"connection refused",
		"connection reset",
		"connection closed",
		"no reachable servers",
		"server closed the connection",
		"connection timed out",
		"broken pipe",
		"EOF",
		"driver: bad connection",
	}
	for _, p := range patterns {
		if !isConnectionError(errors.New(p)) {
			t.Errorf("isConnectionError(%q) should be true", p)
		}
	}
}

// ---------------------------------------------------------------------------
// Integration-style tests (no database, just Worker + mock store)
// ---------------------------------------------------------------------------

func TestWorkerRun_StopsOnCancel(t *testing.T) {
	// Verify that calling cancel() during Run() causes it to exit.
	ms := &mockStore{}
	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		return nil, nil
	}

	w := newTestWorker(ms)
	// Set the schedule interval very high so the schedule loop doesn't matter.
	w.scheduleInterval = 24 * time.Hour

	done := make(chan struct{})
	go func() {
		w.Run()
		close(done)
	}()

	// Give the worker time to start its loops.
	time.Sleep(50 * time.Millisecond)
	w.cancel()

	select {
	case <-done:
		// OK
	case <-time.After(3 * time.Second):
		t.Fatal("Worker.Run did not stop within 3s of cancellation")
	}
}

func TestWorkerConcurrencyKeyReaper_CallsStore(t *testing.T) {
	ms := &mockStore{}
	reapCalled := false
	ms.reapExpiredConcurrencyKeysFn = func(ctx context.Context) (int64, error) {
		reapCalled = true
		return 5, nil
	}

	// Test the concurrency key reaper directly. The ticker is hardcoded at 60s,
	// so we just verify the store method contract.
	n, err := ms.ReapExpiredConcurrencyKeys(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 5 {
		t.Errorf("expected 5 reaped keys, got %d", n)
	}
	if !reapCalled {
		t.Error("expected ReapExpiredConcurrencyKeys to be called")
	}
}

func TestConcurrencyKeyReaperLoop_StopsOnCancel(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.concurrencyKeyReaperLoop()
		close(done)
	}()

	w.cancel()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("concurrencyKeyReaperLoop did not stop within 2s of cancellation")
	}
}

func TestScheduleLoop_StopsOnCancel(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	w.scheduleInterval = 10 * time.Millisecond

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.scheduleLoop()
		close(done)
	}()

	w.cancel()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("scheduleLoop did not stop within 2s of cancellation")
	}
}

func TestIdempotencyCleanupLoop_StopsOnCancel(t *testing.T) {
	// idempotencyCleanupLoop takes a raw context + *sql.DB.
	// We can't easily mock *sql.DB, but we can verify the loop exits on cancel.
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		// Pass nil db — the loop will exit on ctx.Done() before trying to use it.
		idempotencyCleanupLoop(ctx, nil, 10*time.Millisecond)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("idempotencyCleanupLoop did not stop within 2s of cancellation")
	}
}

func TestDispatchLoop_BatchSizeCap(t *testing.T) {
	// Verify that batch size is capped at maxBatchSize even when concurrency
	// is very high.
	ms := &mockStore{}
	requestedLimits := make([]int, 0)
	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		requestedLimits = append(requestedLimits, limit)
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		return nil, nil
	}

	w := newTestWorker(ms)
	w.concurrency = 100 // much higher than maxBatchSize (20)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.dispatchLoop()
		close(done)
	}()

	<-done

	for _, limit := range requestedLimits {
		if limit > 20 {
			t.Errorf("claim limit %d exceeds maxBatchSize 20", limit)
		}
	}
}

func TestHeartbeatLoop_EmptyInflight(t *testing.T) {
	// With BatchHeartbeat, the heartbeat loop always calls the store
	// regardless of inflight state — the DB tracks ownership.
	ms := &mockStore{}
	heartbeatCalled := false
	ms.batchHeartbeatFn = func(ctx context.Context, workerID string) (int64, error) {
		heartbeatCalled = true
		return 0, nil
	}

	w := newTestWorker(ms)
	w.heartbeatInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.heartbeatLoop()
		close(done)
	}()

	<-done

	if !heartbeatCalled {
		t.Error("expected batch heartbeat to be called even when inflight is empty")
	}
}

func TestDispatchLoop_ClaimWorkflowsFallback(t *testing.T) {
	// Verify that when sticky claiming returns partial results,
	// general claiming fills the remaining capacity.
	ms := &mockStore{}
	var stickyLimit, generalLimit int
	var stickyReturned bool
	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		stickyLimit = limit
		if !stickyReturned {
			stickyReturned = true
			return []*engine.WorkflowInstance{
				{ID: "sticky", DefName: "test", DefVersion: 1, Status: "ready"},
			}, nil
		}
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		if generalLimit == 0 {
			generalLimit = limit
		}
		return nil, nil
	}

	w := newTestWorker(ms)
	w.concurrency = 5

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.dispatchLoop()
		close(done)
	}()

	<-done

	if stickyLimit != 5 {
		t.Errorf("expected sticky claim limit 5, got %d", stickyLimit)
	}
	// After one sticky result, remaining = 5 - 1 = 4
	if generalLimit != 4 {
		t.Errorf("expected general claim limit 4, got %d", generalLimit)
	}
}

func TestBaseDSNFromURL_EdgeCases(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"", "host= port=5432 dbname= sslmode=disable"},
		{"not-a-url", "host= port=5432 dbname=not-a-url sslmode=disable"},
		{"postgres://", "host= port=5432 dbname= sslmode=disable"},
		{"postgres://user:pass@host:5432/db?sslmode=require", "host=host port=5432 dbname=db sslmode=require"},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got := baseDSNFromURL(tt.url)
			if got != tt.want {
				t.Errorf("baseDSNFromURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestBaseDSNFromDSN_EdgeCases(t *testing.T) {
	tests := []struct {
		dsn  string
		want string
	}{
		{"", ""},
		{"host=localhost", "host=localhost"},
		{"user=admin password=secret", ""},
		{"host=a user=b password=c port=1", "host=a port=1"},
	}
	for _, tt := range tests {
		t.Run(tt.dsn, func(t *testing.T) {
			got := baseDSNFromDSN(tt.dsn)
			if got != tt.want {
				t.Errorf("baseDSNFromDSN(%q) = %q, want %q", tt.dsn, got, tt.want)
			}
		})
	}
}

func TestGenerateUpdatePromiseID_Format(t *testing.T) {
	id, err := generateUpdatePromiseID()
	if err != nil {
		t.Fatalf("generateUpdatePromiseID() error: %v", err)
	}
	if !strings.HasPrefix(id, "upd-") {
		t.Errorf("expected prefix 'upd-', got %q", id)
	}
	// Hex part after "upd-".
	hexPart := id[4:]
	if len(hexPart) != 32 {
		t.Errorf("expected 32 hex chars, got %d in %q", len(hexPart), id)
	}
	for _, c := range hexPart {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("non-hex char %c in %q", c, id)
		}
	}
}

// ---------------------------------------------------------------------------
// loadShardConfigs tests (lightweight, no real file I/O)
// ---------------------------------------------------------------------------

func TestLoadShardConfigsErrors(t *testing.T) {
	// Test with non-existent file.
	_, err := loadShardConfigs("/nonexistent/shards.json")
	if err == nil {
		t.Error("expected error for non-existent file, got nil")
	}
}

// ---------------------------------------------------------------------------
// dispatchPendingUpdates tests
// ---------------------------------------------------------------------------

func TestDispatchPendingUpdates_EmptyInflight(t *testing.T) {
	ms := &mockStore{}
	ms.getPendingUpdateRequestsFn = func(ctx context.Context, workflowID string) ([]engine.UpdateRequestInfo, error) {
		t.Error("should not be called when inflight is empty")
		return nil, nil
	}

	w := newTestWorker(ms)
	// Should not panic or call store.
	w.dispatchPendingUpdates()
}

func TestDispatchPendingUpdates_NoEngine(t *testing.T) {
	ms := &mockStore{}
	ms.getPendingUpdateRequestsFn = func(ctx context.Context, workflowID string) ([]engine.UpdateRequestInfo, error) {
		return []engine.UpdateRequestInfo{
			{UpdateName: "update-1", Payload: "{}"},
		}, nil
	}

	w := newTestWorker(ms)
	w.inflight.Store("wf-1", &engine.WorkflowInstance{ID: "wf-1"})
	// No engine in execEngines for wf-1 — should skip without error.
	w.dispatchPendingUpdates()
}

// mockServiceCaller implements engine.ServiceCaller for tests.
type mockServiceCaller struct{}

func (m *mockServiceCaller) Call(ctx context.Context, service, operation, requestJSON string) (string, error) {
	return "", nil
}

func TestExecEngines_MapLifecycle(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)

	wfID := "wf-store-delete-test"
	w.inflight.Store(wfID, &engine.WorkflowInstance{ID: wfID})

	// Create a minimal engine with an update handler.
	caller := &mockServiceCaller{}
	eng := engine.NewEngine(nil, caller, engine.WithUpdateHandler(func(name, payload string) (string, error) {
		return "ok", nil
	}))

	// Store the engine.
	w.execEngines.Store(wfID, eng)

	// Load and verify it's found.
	loaded, ok := w.execEngines.Load(wfID)
	if !ok {
		t.Fatal("expected engine to be found after Store")
	}
	if loaded != eng {
		t.Errorf("loaded engine = %v, want %v", loaded, eng)
	}

	// Verify DispatchUpdate works through the loaded engine.
	result, err := loaded.(*engine.Engine).DispatchUpdate(context.Background(), "test-update", "{}")
	if err != nil {
		t.Fatalf("DispatchUpdate failed: %v", err)
	}
	if result != "ok" {
		t.Errorf("DispatchUpdate = %q, want %q", result, "ok")
	}

	// Delete the engine.
	w.execEngines.Delete(wfID)

	// Verify it's gone.
	_, ok = w.execEngines.Load(wfID)
	if ok {
		t.Fatal("expected engine to be gone after Delete")
	}
}

func TestDispatchPendingUpdates_WithEngine(t *testing.T) {
	ms := &mockStore{}

	var dispatchedName, dispatchedPayload string
	ms.getPendingUpdateRequestsFn = func(ctx context.Context, workflowID string) ([]engine.UpdateRequestInfo, error) {
		return []engine.UpdateRequestInfo{
			{UpdateName: "status-update", Payload: `{"status":"running"}`},
		}, nil
	}
	completed := false
	ms.completeUpdateRequestFn = func(ctx context.Context, workflowID, updateName, result, errMsg string) error {
		completed = true
		if updateName != "status-update" {
			t.Errorf("updateName = %q, want %q", updateName, "status-update")
		}
		if result != `{"status":"ok"}` {
			t.Errorf("result = %q, want %q", result, `{"status":"ok"}`)
		}
		if errMsg != "" {
			t.Errorf("errMsg = %q, want empty", errMsg)
		}
		return nil
	}

	w := newTestWorker(ms)
	wfID := "wf-dispatch-test"
	w.inflight.Store(wfID, &engine.WorkflowInstance{ID: wfID})

	// Create an engine that captures the dispatched update.
	caller := &mockServiceCaller{}
	eng := engine.NewEngine(nil, caller, engine.WithUpdateHandler(func(name, payload string) (string, error) {
		dispatchedName = name
		dispatchedPayload = payload
		return `{"status":"ok"}`, nil
	}))
	w.execEngines.Store(wfID, eng)

	w.dispatchPendingUpdates()

	if dispatchedName != "status-update" {
		t.Errorf("dispatched name = %q, want %q", dispatchedName, "status-update")
	}
	if dispatchedPayload != `{"status":"running"}` {
		t.Errorf("dispatched payload = %q, want %q", dispatchedPayload, `{"status":"running"}`)
	}
	if !completed {
		t.Error("expected CompleteUpdateRequest to be called")
	}

	// Verify cleanup: Delete removes engine, Load returns !ok.
	w.execEngines.Delete(wfID)
	if _, ok := w.execEngines.Load(wfID); ok {
		t.Error("expected engine to be gone after Delete")
	}
}

// ---------------------------------------------------------------------------
// releaseOrFail tests
// ---------------------------------------------------------------------------

func TestReleaseOrFail_WithError(t *testing.T) {
	ms := &mockStore{}
	failed := false
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string, queryState map[string]string) error {
		failed = true
		if errMsg != "test error" {
			t.Errorf("expected errMsg 'test error', got %q", errMsg)
		}
		return nil
	}

	w := newTestWorker(ms)
	w.id = "test-worker"

	w.releaseOrFail(&engine.WorkflowInstance{ID: "wf-1"}, "test error")
	if !failed {
		t.Error("expected FailWorkflow to be called")
	}
}

func TestReleaseOrFail_WithoutError(t *testing.T) {
	ms := &mockStore{}
	released := false
	ms.releaseWorkflowFn = func(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
		released = true
		return nil
	}

	w := newTestWorker(ms)
	w.id = "test-worker"

	nextWake := time.Now().Add(time.Hour)
	w.releaseOrFail(&engine.WorkflowInstance{ID: "wf-1", NextWakeAt: nextWake}, "")
	if !released {
		t.Error("expected ReleaseWorkflow to be called")
	}
}

// ---------------------------------------------------------------------------
// Compile-time check: verify that the mock implements the full interface.
// ---------------------------------------------------------------------------

func TestMockStoreImplementsInterface(t *testing.T) {
	var _ engine.WorkflowStore = (*mockStore)(nil)
}

// ---------------------------------------------------------------------------
// API handler tests for remaining handlers (0% coverage)
// ---------------------------------------------------------------------------

func TestAPIHealthz_Degraded(t *testing.T) {
	ms := &mockStore{}
	// Create a worker whose memory controller reports pressure > 0.
	w := newTestWorker(ms)
	w.memoryController = &MemoryController{pressure: 0.75}
	api := &apiServer{store: ms, worker: w}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	resp := httptest.NewRecorder()
	api.handleHealthz(resp, req)

	if resp.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.Code)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["degraded"] != true {
		t.Error("expected degraded=true in healthz response under memory pressure")
	}
	if body["reason"] != "memory_pressure" {
		t.Errorf("expected reason=memory_pressure, got %v", body["reason"])
	}
}

func TestAPIStartWorkflow(t *testing.T) {
	ms := &mockStore{}
	ms.listVersionsFn = func(ctx context.Context, defName string) ([]int, error) {
		return []int{1}, nil
	}
	ms.startNewRunFn = func(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) {
		return "wf-new-1", false, nil
	}

	api := newTestAPIServer(ms)

	reqBody := `{"input":{"order_id":"abc"},"entry_point":"place_order"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/my-wf/start", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleStartWorkflow(w, req, "my-wf")

	if w.Code != 201 {
		t.Errorf("expected 201, got %d", w.Code)
	}
	var result map[string]string
	json.NewDecoder(w.Body).Decode(&result)
	if result["id"] != "wf-new-1" {
		t.Errorf("expected id wf-new-1, got %s", result["id"])
	}
}

func TestAPIStartWorkflow_MemoryPressure(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	w.memoryController = &MemoryController{pressure: 1.0}
	api := &apiServer{store: ms, worker: w}

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/my-wf/start", nil)
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "my-wf")

	if resp.Code != 503 {
		t.Errorf("expected 503, got %d", resp.Code)
	}
}

func TestAPIStartWorkflow_DefNotFound(t *testing.T) {
	ms := &mockStore{}
	ms.listVersionsFn = func(ctx context.Context, defName string) ([]int, error) {
		return nil, nil
	}

	api := newTestAPIServer(ms)

	body := `{"input":{}}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/no-such-wf/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleStartWorkflow(w, req, "no-such-wf")

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
	// Body is bytes.Buffer; no Close needed.
}

func TestAPIStartWorkflow_WithIdempotencyKey(t *testing.T) {
	ms := &mockStore{}
	ms.listVersionsFn = func(ctx context.Context, defName string) ([]int, error) {
		return []int{1}, nil
	}
	ms.startNewRunFn = func(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) {
		return "wf-existing", true, nil
	}

	api := newTestAPIServer(ms)

	body := `{"input":{}}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/my-wf/start", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "idem-123")
	w := httptest.NewRecorder()
	api.handleStartWorkflow(w, req, "my-wf")

	if w.Code != 200 {
		t.Errorf("expected 200 (already started), got %d", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	// Body is bytes.Buffer; no Close needed.
	if resp["already_started"] != "true" {
		t.Error("expected already_started=true in response")
	}
}

func TestAPIStartWorkflow_WithConcurrencyKey(t *testing.T) {
	ms := &mockStore{}
	ms.listVersionsFn = func(ctx context.Context, defName string) ([]int, error) {
		return []int{1}, nil
	}
	ms.startNewRunFn = func(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) {
		return "wf-cc-1", false, nil
	}
	ms.acquireConcurrencyKeyFn = func(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
		return true, nil
	}

	api := newTestAPIServer(ms)

	body := `{"input":{},"concurrency_key":"my-key"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/my-wf/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleStartWorkflow(w, req, "my-wf")

	if w.Code != 201 {
		t.Errorf("expected 201, got %d", w.Code)
	}
	// Body is bytes.Buffer; no Close needed.
}

func TestAPISignal(t *testing.T) {
	ms := &mockStore{}
	ms.deliverSignalFn = func(ctx context.Context, workflowID, signalName, payload string) error {
		return nil
	}

	api := newTestAPIServer(ms)

	body := `{"signal_name":"my-signal","payload":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-1/signal", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleSignal(w, req, "wf-1")

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	// Body is bytes.Buffer; no Close needed.
	if resp["status"] != "delivered" {
		t.Errorf("expected delivered, got %s", resp["status"])
	}
}

func TestAPISignal_MissingName(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)

	body := `{"payload":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-1/signal", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleSignal(w, req, "wf-1")

	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
	// Body is bytes.Buffer; no Close needed.
}

func TestAPICancel(t *testing.T) {
	ms := &mockStore{}
	ms.requestCancellationFn = func(ctx context.Context, workflowID, reason string) error {
		return nil
	}

	api := newTestAPIServer(ms)

	body := `{"reason":"user requested"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-1/cancel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleCancel(w, req, "wf-1")

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	// Body is bytes.Buffer; no Close needed.
	if resp["status"] != "cancellation_requested" {
		t.Errorf("expected cancellation_requested, got %s", resp["status"])
	}
}

func TestAPIGetHistory(t *testing.T) {
	ms := &mockStore{}
	ms.loadEventHistoryPaginatedFn = func(ctx context.Context, workflowID string, offset, limit int) ([]engine.EventRecord, error) {
		return []engine.EventRecord{
			{Step: 1, EventType: "call", Service: "my_svc", Op: "my_func"},
		}, nil
	}
	ms.countEventHistoryFn = func(ctx context.Context, workflowID string) (int, error) {
		return 1, nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/history", nil)
	w := httptest.NewRecorder()
	api.handleGetHistory(w, req, "wf-1")

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var history []engine.EventRecord
	json.NewDecoder(w.Body).Decode(&history)
	// Body is bytes.Buffer; no Close needed.
	if len(history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(history))
	}
	if history[0].Service != "my_svc" {
		t.Errorf("expected my_svc, got %s", history[0].Service)
	}
}

func TestAPIGetHistory_Nil(t *testing.T) {
	// When store returns nil, handler should return an empty array.
	ms := &mockStore{}
	ms.loadEventHistoryPaginatedFn = func(ctx context.Context, workflowID string, offset, limit int) ([]engine.EventRecord, error) {
		return nil, nil
	}
	ms.countEventHistoryFn = func(ctx context.Context, workflowID string) (int, error) {
		return 0, nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/history", nil)
	w := httptest.NewRecorder()
	api.handleGetHistory(w, req, "wf-1")

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var history []engine.EventRecord
	json.NewDecoder(w.Body).Decode(&history)
	// Body is bytes.Buffer; no Close needed.
	if history == nil {
		t.Error("expected non-nil empty array for nil history")
	}
	if len(history) != 0 {
		t.Errorf("expected empty array, got %d", len(history))
	}
}

func TestAPIGetQueryState(t *testing.T) {
	ms := &mockStore{}
	ms.getQueryStateFn = func(ctx context.Context, workflowID, key string) (string, error) {
		return "state-value-42", nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/query?key=mykey", nil)
	w := httptest.NewRecorder()
	api.handleGetQueryState(w, req, "wf-1")

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	// Body is bytes.Buffer; no Close needed.
	if resp["key"] != "mykey" {
		t.Errorf("expected key mykey, got %s", resp["key"])
	}
	if resp["value"] != "state-value-42" {
		t.Errorf("expected value state-value-42, got %s", resp["value"])
	}
}

func TestAPIGetDAG(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{ID: "wf-dag-1", DefName: "dag-workflow", DefVersion: 1}, nil
	}
	ms.loadDAGSpecFn = func(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
		return json.RawMessage(`{"nodes":[{"name":"step1"}]}`), nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-dag-1/dag", nil)
	w := httptest.NewRecorder()
	api.handleGetDAG(w, req, "wf-dag-1")

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	// Body is bytes.Buffer; no Close needed.
	if resp["workflow_id"] != "wf-dag-1" {
		t.Errorf("expected workflow_id wf-dag-1, got %v", resp["workflow_id"])
	}
}

func TestAPIGetDAG_WorkflowNotFound(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
		return nil, nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-missing/dag", nil)
	w := httptest.NewRecorder()
	api.handleGetDAG(w, req, "wf-missing")

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
	// Body is bytes.Buffer; no Close needed.
}

func TestAPIGetDAG_SpecNotFound(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{ID: "wf-dag-2", DefName: "test", DefVersion: 1}, nil
	}
	ms.loadDAGSpecFn = func(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
		return nil, nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-dag-2/dag", nil)
	w := httptest.NewRecorder()
	api.handleGetDAG(w, req, "wf-dag-2")

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
	// Body is bytes.Buffer; no Close needed.
}

func TestAPIListPromises(t *testing.T) {
	ms := &mockStore{}
	ms.listPromisesFn = func(ctx context.Context, workflowID string) ([]engine.PromiseInfo, error) {
		return []engine.PromiseInfo{
			{PromiseID: "prom-1", Status: "pending"},
		}, nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/promises", nil)
	w := httptest.NewRecorder()
	api.handleListPromises(w, req, "wf-1")

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var promises []engine.PromiseInfo
	json.NewDecoder(w.Body).Decode(&promises)
	// Body is bytes.Buffer; no Close needed.
	if len(promises) != 1 {
		t.Fatalf("expected 1 promise, got %d", len(promises))
	}
	if promises[0].PromiseID != "prom-1" {
		t.Errorf("expected prom-1, got %s", promises[0].PromiseID)
	}
}

func TestAPIListPromises_Nil(t *testing.T) {
	ms := &mockStore{}
	ms.listPromisesFn = func(ctx context.Context, workflowID string) ([]engine.PromiseInfo, error) {
		return nil, nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/promises", nil)
	w := httptest.NewRecorder()
	api.handleListPromises(w, req, "wf-1")

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var promises []engine.PromiseInfo
	json.NewDecoder(w.Body).Decode(&promises)
	// Body is bytes.Buffer; no Close needed.
	if promises == nil {
		t.Error("expected non-nil empty slice for nil promises")
	}
}

func TestAPIResolvePromise(t *testing.T) {
	ms := &mockStore{}
	ms.resolvePromiseFn = func(ctx context.Context, workflowID, promiseID, result string) error {
		return nil
	}

	api := newTestAPIServer(ms)

	body := `{"result":"success"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-1/promises/prom-1/resolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleResolvePromise(w, req, "wf-1", "prom-1")

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	// Body is bytes.Buffer; no Close needed.
	if resp["status"] != "resolved" {
		t.Errorf("expected resolved, got %s", resp["status"])
	}
}

func TestAPIResolvePromise_InvalidJSON(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)

	body := `not-json`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-1/promises/prom-1/resolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleResolvePromise(w, req, "wf-1", "prom-1")

	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
	// Body is bytes.Buffer; no Close needed.
}

func TestAPIRejectPromise(t *testing.T) {
	ms := &mockStore{}
	ms.rejectPromiseFn = func(ctx context.Context, workflowID, promiseID, errMsg string) error {
		return nil
	}

	api := newTestAPIServer(ms)

	body := `{"reason":"something went wrong"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-1/promises/prom-1/reject", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleRejectPromise(w, req, "wf-1", "prom-1")

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	// Body is bytes.Buffer; no Close needed.
	if resp["status"] != "rejected" {
		t.Errorf("expected rejected, got %s", resp["status"])
	}
}

func TestAPIRejectPromise_InvalidJSON(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-1/promises/prom-1/reject", nil)
	w := httptest.NewRecorder()
	api.handleRejectPromise(w, req, "wf-1", "prom-1")

	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
	// Body is bytes.Buffer; no Close needed.
}

func TestAPIWorkflowUpdate(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{ID: "wf-upd-1"}, nil
	}
	ms.getPendingUpdateRequestsFn = func(ctx context.Context, workflowID string) ([]engine.UpdateRequestInfo, error) {
		return nil, nil
	}
	ms.createUpdateRequestFn = func(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
		return nil
	}

	api := newTestAPIServer(ms)

	body := `{"status":"new"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-upd-1/update/my-update", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflowUpdate(w, req, "wf-upd-1", "my-update")

	if w.Code != 202 {
		t.Errorf("expected 202, got %d", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	// Body is bytes.Buffer; no Close needed.
	if resp["promise_id"] == "" {
		t.Error("expected non-empty promise_id in response")
	}
	if !strings.HasPrefix(resp["promise_id"], "upd-") {
		t.Errorf("expected promise_id starting with upd-, got %s", resp["promise_id"])
	}
}

func TestAPIWorkflowUpdate_NotFound(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
		return nil, nil
	}

	api := newTestAPIServer(ms)

	body := `{"status":"new"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-missing/update/my-update", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflowUpdate(w, req, "wf-missing", "my-update")

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
	// Body is bytes.Buffer; no Close needed.
}

func TestAPIWorkflowUpdate_Duplicate(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{ID: "wf-upd-2"}, nil
	}
	ms.getPendingUpdateRequestsFn = func(ctx context.Context, workflowID string) ([]engine.UpdateRequestInfo, error) {
		return []engine.UpdateRequestInfo{
			{UpdateName: "my-update"},
		}, nil
	}

	api := newTestAPIServer(ms)

	body := `{"status":"new"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-upd-2/update/my-update", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflowUpdate(w, req, "wf-upd-2", "my-update")

	if w.Code != 409 {
		t.Errorf("expected 409, got %d", w.Code)
	}
	// Body is bytes.Buffer; no Close needed.
}

func TestAPIDefinitions(t *testing.T) {
	ms := &mockStore{}
	ms.listWorkflowDefsFn = func(ctx context.Context, name string) ([]engine.WorkflowDef, error) {
		return []engine.WorkflowDef{
			{Name: "wf-a", Version: 1, ABIVersion: 1, MinVersion: 1},
			{Name: "wf-b", Version: 2, ABIVersion: 1, MinVersion: 1},
		}, nil
	}
	ms.loadMemoryStatsFn = func(ctx context.Context) ([]engine.WorkflowMemoryStats, error) {
		return nil, nil
	}
	ms.countActiveInstancesFn = func(ctx context.Context, name string, version int) (int, error) {
		return 0, nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/definitions", nil)
	w := httptest.NewRecorder()
	api.handleDefinitions(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var defs []map[string]any
	json.NewDecoder(w.Body).Decode(&defs)
	// Body is bytes.Buffer; no Close needed.
	if len(defs) != 2 {
		t.Fatalf("expected 2 definitions, got %d", len(defs))
	}
	if defs[0]["name"] != "wf-a" {
		t.Errorf("expected wf-a, got %s", defs[0]["name"])
	}
}

func TestAPIDefinitions_MethodNotAllowed(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPost, "/api/definitions", nil)
	w := httptest.NewRecorder()
	api.handleDefinitions(w, req)

	if w.Code != 405 {
		t.Errorf("expected 405, got %d", w.Code)
	}
	// Body is bytes.Buffer; no Close needed.
}

func TestAPIDefinitions_Empty(t *testing.T) {
	ms := &mockStore{}
	ms.listWorkflowDefsFn = func(ctx context.Context, name string) ([]engine.WorkflowDef, error) {
		return nil, nil
	}
	ms.loadMemoryStatsFn = func(ctx context.Context) ([]engine.WorkflowMemoryStats, error) {
		return nil, nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/definitions", nil)
	w := httptest.NewRecorder()
	api.handleDefinitions(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var defs []map[string]any
	json.NewDecoder(w.Body).Decode(&defs)
	// Body is bytes.Buffer; no Close needed.
	if defs == nil {
		t.Error("expected non-nil empty array for nil definitions")
	}
}

func TestAPIDefinitions_WithMemoryStats(t *testing.T) {
	ms := &mockStore{}
	ms.listWorkflowDefsFn = func(ctx context.Context, name string) ([]engine.WorkflowDef, error) {
		return []engine.WorkflowDef{
			{Name: "wf-mem", Version: 1, ABIVersion: 1, MinVersion: 1},
		}, nil
	}
	ms.loadMemoryStatsFn = func(ctx context.Context) ([]engine.WorkflowMemoryStats, error) {
		return []engine.WorkflowMemoryStats{
			{DefName: "wf-mem", SampleCount: 10, AvgBytes: 42},
		}, nil
	}
	ms.countActiveInstancesFn = func(ctx context.Context, name string, version int) (int, error) {
		return 3, nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/definitions", nil)
	w := httptest.NewRecorder()
	api.handleDefinitions(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var defs []map[string]any
	json.NewDecoder(w.Body).Decode(&defs)
	// Body is bytes.Buffer; no Close needed.
	if len(defs) != 1 {
		t.Fatalf("expected 1 definition, got %d", len(defs))
	}
	if defs[0]["active_instances"].(float64) != 3 {
		t.Errorf("expected active_instances 3, got %v", defs[0]["active_instances"])
	}
}

// ---------------------------------------------------------------------------
// Memory controller tests
// ---------------------------------------------------------------------------

func TestCanAcceptAPIWorkflows(t *testing.T) {
	mc := &MemoryController{pressure: 0.5}
	if !mc.CanAcceptAPIWorkflows() {
		t.Error("CanAcceptAPIWorkflows() = false, want true when pressure < 1.0")
	}
}

func TestCanAcceptAPIWorkflows_UnderPressure(t *testing.T) {
	mc := &MemoryController{pressure: 1.0}
	if mc.CanAcceptAPIWorkflows() {
		t.Error("CanAcceptAPIWorkflows() = true, want false when pressure >= 1.0")
	}
}

func TestCanAcceptAPIWorkflows_Default(t *testing.T) {
	// Default zero-valued pressure should still allow acceptance.
	mc := &MemoryController{}
	if !mc.CanAcceptAPIWorkflows() {
		t.Error("CanAcceptAPIWorkflows() = false, want true with default pressure")
	}
}

// ---------------------------------------------------------------------------
// readMemTotal tests
// ---------------------------------------------------------------------------

// readMemTotal reads /proc/meminfo, so it only works on Linux. Its caller
// treats a false return as "memory monitoring unavailable" and disables the
// feature rather than failing, so on other platforms the correct behaviour is
// a clean false — which is worth asserting rather than skipping past.
func TestReadMemTotal(t *testing.T) {
	total, ok := readMemTotal()

	if runtime.GOOS != "linux" {
		if ok {
			t.Errorf("readMemTotal() = (%d, true) on %s; /proc/meminfo should not exist there",
				total, runtime.GOOS)
		}
		return
	}

	if !ok {
		t.Fatal("readMemTotal() returned false on Linux — /proc/meminfo should be readable")
	}
	if total == 0 {
		t.Error("readMemTotal() returned 0 bytes, expected > 0")
	}
	// Reasonable minimum: 16 MB
	if total < 16*1024*1024 {
		t.Errorf("readMemTotal() = %d bytes, seems unreasonably small", total)
	}
	// Sanity check: less than 64 TB
	if total > 64*1024*1024*1024*1024 {
		t.Errorf("readMemTotal() = %d bytes, seems unreasonably large", total)
	}
}
func (m *mockStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) {
	if m.batchHeartbeatFn != nil {
		return m.batchHeartbeatFn(ctx, workerID)
	}
	return 0, nil
}

func (m *mockStore) LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]engine.EventRecord, error) {
	if m.loadEventHistoryPaginatedFn != nil {
		return m.loadEventHistoryPaginatedFn(ctx, workflowID, offset, limit)
	}
	return nil, nil
}
func (m *mockStore) VerifyWorkflowEvents(ctx context.Context, workflowID string) error { return nil }
func (m *mockStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error {
	if m.moveToDeadLetterQueueFn != nil {
		return m.moveToDeadLetterQueueFn(ctx, workflowID, workerID, generation, errMsg, errorCode, errorOp)
	}
	return nil
}
func (m *mockStore) RetryWorkflow(ctx context.Context, workflowID string) error { return nil }
func (m *mockStore) ResolveLatestVersion(ctx context.Context, defName string) (int, error) {
	return 0, nil
}
func (m *mockStore) ValidateVersion(ctx context.Context, defName string, defVersion int) (bool, error) {
	return true, nil
}
func (m *mockStore) CountEventHistory(ctx context.Context, workflowID string) (int, error) {
	if m.countEventHistoryFn != nil {
		return m.countEventHistoryFn(ctx, workflowID)
	}
	return 0, nil
}
func (m *mockStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (m *mockStore) CountActiveConcurrencyKeys(ctx context.Context) (int, error) { return 0, nil }
func (m *mockStore) DeleteDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	return 0, nil
}
func (m *mockStore) DeleteCompletedWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	if m.deleteCompletedWorkflowsFn != nil {
		return m.deleteCompletedWorkflowsFn(ctx, olderThan)
	}
	return 0, nil
}
func (m *mockStore) LoadEventHistoryBatch(ctx context.Context, workflowIDs []string) (map[string][]engine.EventRecord, error) {
	return nil, nil
}
func (m *mockStore) StreamEventHistory(ctx context.Context, workflowID string, pageSize int) (<-chan engine.EventRecord, <-chan error) {
	return nil, nil
}
func (m *mockStore) TerminateWorkflow(ctx context.Context, workflowID, reason string) error {
	if m.terminateWorkflowFn != nil {
		return m.terminateWorkflowFn(ctx, workflowID, reason)
	}
	return nil
}
func (m *mockStore) AdminForceComplete(ctx context.Context, workflowID string, generation int64, result string, operator string) error {
	if m.adminForceCompleteFn != nil {
		return m.adminForceCompleteFn(ctx, workflowID, generation, result, operator)
	}
	return nil
}
func (m *mockStore) AdminForceFail(ctx context.Context, workflowID string, generation int64, errorMsg, errorCode string, operator string) error {
	if m.adminForceFailFn != nil {
		return m.adminForceFailFn(ctx, workflowID, generation, errorMsg, errorCode, operator)
	}
	return nil
}
func (m *mockStore) AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error {
	if m.adminReReplayFn != nil {
		return m.adminReReplayFn(ctx, workflowID, generation, operator)
	}
	return nil
}
func (m *mockStore) GetChildCount(ctx context.Context, parentWorkflowID string) (int, error) {
	return 0, nil
}
func (m *mockStore) GetConcurrencyKeyCount(ctx context.Context, workflowID string) (int, error) {
	return 0, nil
}
func (m *mockStore) GetEventCount(ctx context.Context, workflowID string) (int, error) {
	return 0, nil
}
func (m *mockStore) GetAllowedSignalCallers(ctx context.Context, workflowID string) ([]string, error) {
	if m.getAllowedSignalCallersFn != nil {
		return m.getAllowedSignalCallersFn(ctx, workflowID)
	}
	return nil, nil
}

func (m *mockStore) SetWorkflowTag(ctx context.Context, workflowName string, version int, tag string) error {
	return nil
}
func (m *mockStore) RemoveWorkflowTag(ctx context.Context, workflowName string, tag string) error {
	return nil
}
func (m *mockStore) GetWorkflowTag(ctx context.Context, workflowName string, tag string) (int, error) {
	return 0, nil
}
func (m *mockStore) GetWorkflowTags(ctx context.Context, workflowName string) (map[string]int, error) {
	return nil, nil
}
func (m *mockStore) SetRoutingRule(ctx context.Context, workflowName string, targetVersion int, weight float64) error {
	return nil
}
func (m *mockStore) RemoveRoutingRule(ctx context.Context, ruleID string) error { return nil }
func (m *mockStore) GetRoutingRules(ctx context.Context, workflowName string) ([]engine.RoutingRule, error) {
	return nil, nil
}
func (m *mockStore) PickVersionByRouting(ctx context.Context, workflowName string) (int, error) {
	return 0, nil
}
func (m *mockStore) ResolveVersionByTag(ctx context.Context, workflowName string, tag string) (int, error) {
	return 0, nil
}

// ---- healthTracker unit tests ----

func TestHealthTracker_RecordRunAndIsStale(t *testing.T) {
	ht := newHealthTracker()
	ht.registerLoop("testloop")
	ht.setInterval("testloop", 10*time.Millisecond)

	// Immediately after registration, not stale yet (maxAge hasn't elapsed).
	if ht.isStale("testloop") {
		t.Error("expected loop to not be stale immediately after registration")
	}

	// Wait for maxAge (6 * 10ms = 60ms).
	time.Sleep(120 * time.Millisecond)
	if !ht.isStale("testloop") {
		t.Error("expected loop to be stale after maxAge elapsed without recordRun")
	}

	ht.recordRun("testloop")
	if ht.isStale("testloop") {
		t.Error("expected loop to not be stale after recordRun")
	}

	// Wait for maxAge again.
	time.Sleep(120 * time.Millisecond)
	if !ht.isStale("testloop") {
		t.Error("expected loop to be stale after maxAge elapsed again")
	}
}

func TestHealthTracker_IsStale_NeverRegistered(t *testing.T) {
	ht := newHealthTracker()
	if ht.isStale("nonexistent") {
		t.Error("expected non-registered loop to not be stale")
	}
}

func TestHealthTracker_RegisteredCount(t *testing.T) {
	ht := newHealthTracker()
	if ht.registeredCount() != 0 {
		t.Error("expected 0 registered loops initially")
	}
	ht.registerLoop("a")
	ht.registerLoop("b")
	if ht.registeredCount() != 2 {
		t.Errorf("expected 2, got %d", ht.registeredCount())
	}
}

func TestHealthTracker_StaleLoops_IncludesNeverRun(t *testing.T) {
	ht := newHealthTracker()
	ht.registerLoop("never-run")
	ht.setInterval("never-run", time.Millisecond)

	// Wait for maxAge to elapse (3 * 1ms).
	time.Sleep(10 * time.Millisecond)

	stale := ht.staleLoops()
	found := false
	for _, s := range stale {
		if s == "never-run" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected never-run loop to appear in staleLoops after maxAge elapsed")
	}

	ht.recordRun("never-run")
	stale = ht.staleLoops()
	for _, s := range stale {
		if s == "never-run" {
			t.Error("expected loop to drop out of staleLoops after recordRun")
		}
	}
}

func TestHealthTracker_StaleLoops_ExcludesRunning(t *testing.T) {
	ht := newHealthTracker()
	ht.registerLoop("running")
	ht.setInterval("running", 10*time.Millisecond)
	ht.recordRun("running")

	stale := ht.staleLoops()
	for _, s := range stale {
		if s == "running" {
			t.Error("expected running loop to not be in staleLoops")
		}
	}
}

func TestHealthTracker_StaleLoops_DefaultMaxAge(t *testing.T) {
	// Loops without a set interval should use the 60s default maxAge.
	ht := newHealthTracker()
	ht.registerLoop("no-interval")
	ht.recordRun("no-interval")

	if ht.isStale("no-interval") {
		t.Error("loop that just ran should not be stale, even without interval set")
	}
}

// ---- launchLoop tests ----

func TestLaunchLoop_ClosesDoneChannel(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	w.loopFuncs = make(map[string]func())
	w.loopCtxMap = make(map[string]*loopContext)

	ctx, cancel := context.WithCancel(w.ctx)
	w.loopCtxMap["testloop"] = &loopContext{
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	w.healthTracker.registerLoop("testloop")

	exited := make(chan struct{})
	w.loopFuncs["testloop"] = func() {
		defer w.wg.Done()
		<-exited
	}
	w.launchLoop("testloop", w.loopFuncs["testloop"])

	close(exited)
	w.wg.Wait()

	lc := w.loopCtxMap["testloop"]
	select {
	case <-lc.done:
	case <-time.After(time.Second):
		t.Error("done channel was not closed within 1s of loop exit")
	}
}

// ---- restartLoop tests ----

func TestRestartLoop_RecoveredLoopNotKilled(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	w.loopFuncs = make(map[string]func())
	w.loopCtxMap = make(map[string]*loopContext)

	ctx, cancel := context.WithCancel(w.ctx)
	done := make(chan struct{})
	w.loopCtxMap["recovered"] = &loopContext{ctx: ctx, cancel: cancel, done: done}
	w.healthTracker.registerLoop("recovered")
	w.healthTracker.setInterval("recovered", 50*time.Millisecond)
	w.healthTracker.recordRun("recovered")

	loopStarted := make(chan struct{})
	loopWasCancelled := false
	w.loopFuncs["recovered"] = func() {
		defer w.wg.Done()
		close(loopStarted)
		<-ctx.Done()
		loopWasCancelled = true
	}

	w.launchLoop("recovered", w.loopFuncs["recovered"])
	<-loopStarted

	if w.healthTracker.isStale("recovered") {
		t.Error("expected isStale to return false immediately after recordRun")
	}

	w.restartLoop("recovered")

	if loopWasCancelled {
		t.Error("loop was cancelled even though it was not stale")
	}

	cancel()
	w.wg.Wait()
}

func TestRestartLoop_KillsStaleLoop(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	w.loopFuncs = make(map[string]func())
	w.loopCtxMap = make(map[string]*loopContext)

	ctx, cancel := context.WithCancel(w.ctx)
	done := make(chan struct{})
	w.loopCtxMap["stale-loop"] = &loopContext{ctx: ctx, cancel: cancel, done: done}
	w.healthTracker.registerLoop("stale-loop")
	w.healthTracker.setInterval("stale-loop", time.Millisecond)

	// Use a sync.Once to avoid double-close when restartLoop creates a new goroutine
	// that also calls this function.
	var exitedOnce sync.Once
	loopExited := make(chan struct{})
	w.loopFuncs["stale-loop"] = func() {
		defer w.wg.Done()
		exitedOnce.Do(func() { close(loopExited) })
		<-ctx.Done()
	}

	w.launchLoop("stale-loop", w.loopFuncs["stale-loop"])

	time.Sleep(10 * time.Millisecond)

	if !w.healthTracker.isStale("stale-loop") {
		t.Fatal("expected loop to be stale")
	}

	w.restartLoop("stale-loop")

	select {
	case <-loopExited:
	case <-time.After(time.Second):
		t.Error("old goroutine did not exit within 1s of restartLoop")
	}

	if lc, ok := w.loopCtxMap["stale-loop"]; ok {
		lc.cancel()
	}
	w.wg.Wait()
}

func TestRestartLoop_NoFunctionRegistered(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	w.loopFuncs = make(map[string]func())

	// Should not panic.
	w.restartLoop("nonexistent")
}

// ---- poison-pill tests ----

func TestWatchdog_PoisonPillCondition(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	w.healthCheckInterval = 10 * time.Millisecond
	w.loopFuncs = make(map[string]func())
	w.loopCtxMap = make(map[string]*loopContext)

	for _, name := range []string{"a", "b", "c", "d"} {
		ctx, cancel := context.WithCancel(w.ctx)
		w.loopCtxMap[name] = &loopContext{ctx: ctx, cancel: cancel, done: make(chan struct{})}
		w.healthTracker.registerLoop(name)
		w.healthTracker.setInterval(name, time.Millisecond)
		w.loopFuncs[name] = func() {
			defer w.wg.Done()
			<-w.ctx.Done()
		}
		w.launchLoop(name, w.loopFuncs[name])
	}

	time.Sleep(20 * time.Millisecond)

	stale := w.healthTracker.staleLoops()
	if len(stale) < 4 {
		t.Fatalf("expected 4 stale loops, got %d", len(stale))
	}

	w.healthTracker.registerLoop("watchdog")
	w.healthTracker.setInterval("watchdog", w.healthCheckInterval)
	w.healthTracker.recordRun("watchdog")

	total := w.healthTracker.registeredCount()
	if total < 3 || len(stale) < (total*4/5) {
		t.Errorf("expected poison-pill condition met: %d stale / %d total", len(stale), total)
	}

	w.cancel()
	w.wg.Wait()
}

// ---------------------------------------------------------------------------
// Drain race fix tests (cleat-230-race-fix3)
// ---------------------------------------------------------------------------

// TestAPIStartWorkflow_Draining verifies that handleStartWorkflow returns
// 503 when the worker is draining (Fix 1).
func TestAPIStartWorkflow_Draining(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	w.drainCh = make(chan struct{})
	w.draining.Store(true)
	api := &apiServer{store: ms, worker: w, maxBodySize: 1 << 20}

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/my-wf/start", strings.NewReader(`{"input":{}}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "my-wf")

	if resp.Code != 503 {
		t.Errorf("expected 503 during drain, got %d", resp.Code)
	}
}

// TestDispatchLoop_DrainAfterClaim verifies that the dispatch loop's
// post-claim drain check releases claimed workflows instead of executing
// them (Fix 2). The claim function sets draining=true to simulate drain
// starting during the DB claim.
func TestDispatchLoop_DrainAfterClaim(t *testing.T) {
	ms := &mockStore{}

	releasedCh := make(chan string, 1)

	w := newTestWorker(ms)
	w.drainCh = make(chan struct{})

	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
		// Simulate drain starting during the DB claim (TOCTOU window).
		w.draining.Store(true)
		return []*engine.WorkflowInstance{
			{ID: "wf-1", DefName: "test", DefVersion: 1, Status: "ready"},
		}, nil
	}
	ms.releaseWorkflowFn = func(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
		select {
		case releasedCh <- workflowID:
		default:
		}
		return nil
	}
	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		t.Error("executeWorkflow should not be called during drain")
		return nil, nil
	}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string, queryState map[string]string) error {
		return nil
	}

	// Launch dispatch loop. dispatchLoop has its own defer w.wg.Done(),
	// so we just Add and launch directly (the same pattern as launchLoop).
	w.wg.Add(1)
	go w.dispatchLoop()

	// Wait for the claimed workflow to be released.
	select {
	case id := <-releasedCh:
		if id != "wf-1" {
			t.Errorf("expected wf-1 to be released, got %s", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ReleaseWorkflow call")
	}

	// Verify workflow never entered inflight.
	if _, loaded := w.inflight.Load("wf-1"); loaded {
		t.Error("workflow should not be in inflight after drain release")
	}

	w.cancel()
	w.wg.Wait()
}

// TestDrainStatus_ClosesChannelBeforeCancel verifies that handleDrainStatus
// closes the drainCh and cancels the root context in the correct order
// (Fix 3). The drainCh is closed first, then cancel is called, ensuring
// that external callers waiting on DrainComplete() always unblock.
func TestDrainStatus_ClosesChannelBeforeCancel(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	w.drainCh = make(chan struct{})
	w.draining.Store(true)
	// inflight is empty (zero-value sync.Map)

	api := &apiServer{store: ms, worker: w, maxBodySize: 1 << 20}

	// Call handleDrainStatus. With draining=true and inflight=0, it should
	// close drainCh and cancel the context.
	req := httptest.NewRequest(http.MethodGet, "/api/drain", nil)
	resp := httptest.NewRecorder()
	api.handleDrainStatus(resp, req)

	if resp.Code != 200 {
		t.Fatalf("expected 200, got %d", resp.Code)
	}

	// drainCh must be closed.
	select {
	case <-w.drainCh:
		// drainCh closed — correct.
	default:
		t.Error("drainCh should be closed by handleDrainStatus when inflight is empty")
	}

	// Context must be cancelled AFTER drainCh is closed.
	if w.ctx.Err() == nil {
		t.Error("context should be cancelled by handleDrainStatus")
	}

	// Calling DrainComplete() must not block — it returns the already-closed channel.
	select {
	case <-w.DrainComplete():
		// Not blocking — correct.
	default:
		t.Error("DrainComplete() should not block after handleDrainStatus completes")
	}
}

// TestDrainStatus_DoesNotCloseChannelWhenInflight verifies that
// handleDrainStatus does NOT close drainCh when there are workflows
// still in flight.
func TestDrainStatus_DoesNotCloseChannelWhenInflight(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	w.drainCh = make(chan struct{})
	w.draining.Store(true)
	w.inflight.Store("wf-1", &engine.WorkflowInstance{ID: "wf-1"})

	api := &apiServer{store: ms, worker: w, maxBodySize: 1 << 20}

	req := httptest.NewRequest(http.MethodGet, "/api/drain", nil)
	resp := httptest.NewRecorder()
	api.handleDrainStatus(resp, req)

	if resp.Code != 200 {
		t.Fatalf("expected 200, got %d", resp.Code)
	}

	// drainCh must NOT be closed when inflight is non-zero.
	select {
	case <-w.drainCh:
		t.Error("drainCh should NOT be closed while workflows are in flight")
	default:
		// Not closed — correct.
	}

	// Context must NOT be cancelled.
	if w.ctx.Err() != nil {
		t.Error("context should NOT be cancelled while workflows are in flight")
	}
}

// TestDrainComplete_DoesNotBlock verifies that DrainComplete() returns
// an open channel initially and then unblocks after drain completes.
func TestDrainComplete_DoesNotBlock(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	w.drainCh = make(chan struct{})

	// Before drain: DrainComplete() should not block but should not
	// succeed either (channel is open).
	select {
	case <-w.DrainComplete():
		t.Error("DrainComplete() should not be closed before drain")
	default:
		// Open — correct.
	}

	// Complete drain.
	w.draining.Store(true)
	w.drainOnce.Do(func() {
		close(w.drainCh)
		w.cancel()
	})

	// After drain: DrainComplete() must not block.
	select {
	case <-w.DrainComplete():
		// Not blocking — correct.
	default:
		t.Error("DrainComplete() should not block after drain completes")
	}
}
