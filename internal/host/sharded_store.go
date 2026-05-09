package host

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ShardConfig is a single database shard configuration loaded from JSON.
type ShardConfig struct {
	Name    string   `json:"name"`
	ConnStr string   `json:"conn_str"`
	Tenants []string `json:"tenants,omitempty"`
}

// Shard wraps a database connection and PostgresStore for a single database shard.
type Shard struct {
	Config ShardConfig
	DB     *sql.DB
	Store  *PostgresStore
}

// ShardedStore implements WorkflowStore across multiple PostgreSQL shards.
//
// Each shard hosts a full copy of the schema but owns a subset of the total
// workflow data.  ClaimWorkflow polls every shard (to discover runnable work
// across the fleet).  Most other operations use a consistent hash of the
// workflow ID to route to the owning shard.  Global operations (schedules,
// reaping, listing) fan out to every shard and merge results.
type ShardedStore struct {
	shards []*Shard
	mu     sync.RWMutex
}

// NewShardedStore creates a ShardedStore from the given shard configs.
// Each config's ConnStr is used to open a database connection and the
// optional taskQueues are forwarded to each child PostgresStore.
func NewShardedStore(ctx context.Context, configs []ShardConfig, taskQueues ...string) (*ShardedStore, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("sharded store: at least one shard config is required")
	}

	shards := make([]*Shard, len(configs))
	for i, cfg := range configs {
		db, err := sql.Open("postgres", cfg.ConnStr)
		if err != nil {
			return nil, fmt.Errorf("sharded store: shard %q open: %w", cfg.Name, err)
		}
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			return nil, fmt.Errorf("sharded store: shard %q ping: %w", cfg.Name, err)
		}
		db.SetMaxOpenConns(15)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(5 * time.Minute)

		shards[i] = &Shard{
			Config: cfg,
			DB:     db,
			Store:  NewPostgresStore(db, taskQueues...),
		}
	}

	return &ShardedStore{shards: shards}, nil
}

// Close closes all database connections.
func (s *ShardedStore) Close() {
	for _, shard := range s.shards {
		shard.DB.Close()
	}
}

// Shards returns the underlying shard list (for inspection / metrics).
func (s *ShardedStore) Shards() []*Shard {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Shard, len(s.shards))
	copy(out, s.shards)
	return out
}

// getShard returns the shard responsible for the given workflow key using
// consistent hashing.
func (s *ShardedStore) getShard(key string) *Shard {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.shards) == 0 {
		return nil
	}
	h := sha256.Sum256([]byte(key))
	idx := binary.BigEndian.Uint64(h[:8]) % uint64(len(s.shards))
	return s.shards[idx]
}

// tryEachShard calls fn on every shard in order.  It returns as soon as fn
// returns done=true (carrying fn's error back).  If no shard claims the
// workflow and the last error is non-nil, it is returned.
func (s *ShardedStore) tryEachShard(fn func(*PostgresStore) (done bool, err error)) error {
	var lastErr error
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		done, err := fn(shard.Store)
		if done {
			return err
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return sql.ErrNoRows
}

// forEachShard calls fn on every shard and accumulates errors.
func (s *ShardedStore) forEachShard(fn func(*PostgresStore) error) error {
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		if err := fn(shard.Store); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// WorkflowStore implementation
// ---------------------------------------------------------------------------

// ClaimWorkflow polls every shard and returns the first runnable workflow
// found.  This is the primary dispatch path so we fan-out across all shards.
func (s *ShardedStore) ClaimWorkflow(ctx context.Context, workerID, namespace string) (*WorkflowInstance, error) {
	wfs, err := s.ClaimWorkflows(ctx, workerID, namespace, 1)
	if err != nil {
		return nil, err
	}
	if len(wfs) == 0 {
		return nil, nil
	}
	return wfs[0], nil
}

// ClaimWorkflows claims up to limit runnable workflows across all shards.
// Iterates through shards collecting workflows until limit is reached or shards exhausted.
func (s *ShardedStore) ClaimWorkflows(ctx context.Context, workerID, namespace string, limit int) ([]*WorkflowInstance, error) {
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()

	type shardResult struct {
		wfs  []*WorkflowInstance
		err  error
		name string
	}

	resultCh := make(chan shardResult, len(shards))
	var wg sync.WaitGroup

	for _, shard := range shards {
		wg.Add(1)
		go func(sh *Shard) {
			defer wg.Done()
			wfs, err := sh.Store.ClaimWorkflows(ctx, workerID, namespace, limit)
			resultCh <- shardResult{wfs: wfs, err: err, name: sh.Config.Name}
		}(shard)
	}

	wg.Wait()
	close(resultCh)

	var all []*WorkflowInstance
	for res := range resultCh {
		if res.err != nil {
			return nil, fmt.Errorf("shard %q: %w", res.name, res.err)
		}
		all = append(all, res.wfs...)
	}

	if len(all) == 0 {
		return nil, nil
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// ClaimStickyWorkflows claims up to limit sticky workflow instances across all shards.
// Sticky workflows use idx_instances_sticky for low-contention claiming.
// Iterates through shards collecting workflows until limit is reached or shards exhausted.
func (s *ShardedStore) ClaimStickyWorkflows(ctx context.Context, workerID, namespace string, limit int) ([]*WorkflowInstance, error) {
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()

	type shardResult struct {
		wfs  []*WorkflowInstance
		err  error
		name string
	}

	resultCh := make(chan shardResult, len(shards))
	var wg sync.WaitGroup

	for _, shard := range shards {
		wg.Add(1)
		go func(sh *Shard) {
			defer wg.Done()
			wfs, err := sh.Store.ClaimStickyWorkflows(ctx, workerID, namespace, limit)
			resultCh <- shardResult{wfs: wfs, err: err, name: sh.Config.Name}
		}(shard)
	}

	wg.Wait()
	close(resultCh)

	var all []*WorkflowInstance
	for res := range resultCh {
		if res.err != nil {
			return nil, fmt.Errorf("shard %q: %w", res.name, res.err)
		}
		all = append(all, res.wfs...)
	}

	if len(all) == 0 {
		return nil, nil
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// LoadEventHistory routes by workflow ID.
func (s *ShardedStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return nil, fmt.Errorf("load_event_history: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.LoadEventHistory(ctx, workflowID)
}

// AppendEventHistory routes by workflow ID.
func (s *ShardedStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("append_event_history: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.AppendEventHistory(ctx, workflowID, rec)
}

// AppendEventHistoryBatch routes by workflow ID.
func (s *ShardedStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("append_event_history_batch: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.AppendEventHistoryBatch(ctx, workflowID, recs)
}

// LoadWASM tries each shard (WASM definitions are replicated across all shards).
func (s *ShardedStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) {
	var lastErr error
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		wasm, err := shard.Store.LoadWASM(ctx, defName, defVersion)
		if err == nil {
			return wasm, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// ListVersions tries each shard (definitions are replicated across shards).
func (s *ShardedStore) ListVersions(ctx context.Context, defName string) ([]int, error) {
	// Merge version lists from all shards (deduped).
	seen := make(map[int]bool)
	var allVersions []int
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		versions, err := shard.Store.ListVersions(ctx, defName)
		if err != nil {
			return nil, err
		}
		for _, v := range versions {
			if !seen[v] {
				seen[v] = true
				allVersions = append(allVersions, v)
			}
		}
	}
	if len(allVersions) == 0 {
		return nil, fmt.Errorf("workflow def %s not found on any shard", defName)
	}
	return allVersions, nil
}

// Heartbeat routes by workflow ID.
func (s *ShardedStore) Heartbeat(ctx context.Context, workflowID, workerID string) (bool, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return false, fmt.Errorf("heartbeat: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.Heartbeat(ctx, workflowID, workerID)
}

// BatchHeartbeat fans out to all shards, aggregating the total count.
func (s *ShardedStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) {
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()

	var total int64
	for _, shard := range shards {
		n, err := shard.Store.BatchHeartbeat(ctx, workerID)
		if err != nil {
			return total, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		total += n
	}
	return total, nil
}

// LoadEventHistoryPaginated routes by workflow ID.
func (s *ShardedStore) LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]EventRecord, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return nil, fmt.Errorf("no shard available for workflow %s", workflowID)
	}
	return shard.Store.LoadEventHistoryPaginated(ctx, workflowID, offset, limit)
}

// VerifyWorkflowEvents routes by workflow ID.
func (s *ShardedStore) VerifyWorkflowEvents(ctx context.Context, workflowID string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("no shard available for workflow %s", workflowID)
	}
	return shard.Store.VerifyWorkflowEvents(ctx, workflowID)
}

// MoveToDeadLetterQueue routes by workflow ID.
func (s *ShardedStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID, errMsg string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("no shard available for workflow %s", workflowID)
	}
	return shard.Store.MoveToDeadLetterQueue(ctx, workflowID, workerID, errMsg)
}

// CompleteWorkflow routes by workflow ID.
func (s *ShardedStore) CompleteWorkflow(ctx context.Context, workflowID, workerID, result string, queryState map[string]string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("complete_workflow: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.CompleteWorkflow(ctx, workflowID, workerID, result, queryState)
}

// FailWorkflow routes by workflow ID.
func (s *ShardedStore) FailWorkflow(ctx context.Context, workflowID, workerID, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("fail_workflow: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.FailWorkflow(ctx, workflowID, workerID, errorMsg, errorCode, errorOp, queryState)
}

// ReleaseWorkflow routes by workflow ID.
func (s *ShardedStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("release_workflow: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.ReleaseWorkflow(ctx, workflowID, workerID, nextWakeAt)
}

// ContinueAsNew routes by current run ID so that both the new-run insert and
// the old-run completion land on the same shard.
func (s *ShardedStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, defName string, defVersion int, newInput json.RawMessage, result string, queryState map[string]string) (string, error) {
	shard := s.getShard(currentRunID)
	if shard == nil {
		return "", fmt.Errorf("continue_as_new: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.ContinueAsNew(ctx, currentRunID, workerID, defName, defVersion, newInput, result, queryState)
}

// FinalizeWorkflowSegment routes by workflow ID.
func (s *ShardedStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	shard := s.getShard(runID)
	if shard == nil {
		return fmt.Errorf("finalize_workflow_segment: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.FinalizeWorkflowSegment(ctx, runID, workerID, newEvents, finalStatus, result, errorCode, errorOp, queryState, nextWakeAt)
}

// RequestCancellation routes by workflow ID.
func (s *ShardedStore) RequestCancellation(ctx context.Context, workflowID, reason string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("request_cancellation: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.RequestCancellation(ctx, workflowID, reason)
}

// CheckCancellation routes by workflow ID.
func (s *ShardedStore) CheckCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return false, "", fmt.Errorf("check_cancellation: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.CheckCancellation(ctx, workflowID)
}

// DeliverSignal routes by workflow ID.
func (s *ShardedStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("deliver_signal: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.DeliverSignal(ctx, workflowID, signalName, payload)
}

// PollAndClaimSignal routes by workflow ID.
func (s *ShardedStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return "", false, fmt.Errorf("poll_and_claim_signal: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.PollAndClaimSignal(ctx, workflowID, signalName)
}

// StartNewRun picks a shard by hashing the definition name so all runs of the
// same workflow type land on the same shard. The idempotencyKey is forwarded
// to the underlying store for exactly-once semantics.
func (s *ShardedStore) StartNewRun(ctx context.Context, defName string, defVersion int, input json.RawMessage, idempotencyKey string) (string, bool, error) {
	shard := s.getShard(defName)
	if shard == nil {
		return "", false, fmt.Errorf("start_new_run: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.StartNewRun(ctx, defName, defVersion, input, idempotencyKey)
}

// StartChildWorkflow places the child on the same shard as the parent.
// defVersion is passed through to the underlying store for version resolution.
func (s *ShardedStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string) (string, error) {
	shard := s.getShard(parentID)
	if shard == nil {
		return "", fmt.Errorf("start_child_workflow: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.StartChildWorkflow(ctx, parentID, defName, inputJSON, defVersion, parentClosePolicy)
}

// GetChildResult routes by child run ID.
func (s *ShardedStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	shard := s.getShard(runID)
	if shard == nil {
		return "", false, fmt.Errorf("get_child_result: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.GetChildResult(ctx, runID)
}

// ReapStaleInstances runs on every shard and returns the total reclaimed count.
func (s *ShardedStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) {
	total := 0
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		n, err := shard.Store.ReapStaleInstances(ctx, timeout)
		if err != nil {
			return total, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		total += n
	}
	return total, nil
}

// GetQueryState routes by workflow ID.
func (s *ShardedStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return "", fmt.Errorf("get_query_state: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.GetQueryState(ctx, workflowID, key)
}

// ListWorkflows merges results from all shards.
func (s *ShardedStore) ListWorkflows(ctx context.Context, filter WorkflowFilter) ([]WorkflowInstance, error) {
	var all []WorkflowInstance
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		workflows, err := shard.Store.ListWorkflows(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		all = append(all, workflows...)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	} else if limit > 1000 {
		limit = 1000
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// GetWorkflowByID tries each shard (workflow could be on any shard).
func (s *ShardedStore) GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error) {
	// Fast path: try the hashed shard first.
	shard := s.getShard(id)
	if shard != nil {
		wf, err := shard.Store.GetWorkflowByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if wf != nil {
			return wf, nil
		}
	}
	// Fallback: scan remaining shards.
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, other := range shards {
		if other == shard {
			continue
		}
		wf, err := other.Store.GetWorkflowByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if wf != nil {
			return wf, nil
		}
	}
	return nil, nil
}

// CreateSchedule registers a schedule on every shard.
func (s *ShardedStore) CreateSchedule(ctx context.Context, sch Schedule) error {
	return s.forEachShard(func(store *PostgresStore) error {
		return store.CreateSchedule(ctx, sch)
	})
}

// ListSchedules merges schedules from all shards (deduped by name).
func (s *ShardedStore) ListSchedules(ctx context.Context) ([]Schedule, error) {
	seen := make(map[string]bool)
	var all []Schedule
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		schedules, err := shard.Store.ListSchedules(ctx)
		if err != nil {
			return nil, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		for _, sch := range schedules {
			if !seen[sch.Name] {
				seen[sch.Name] = true
				all = append(all, sch)
			}
		}
	}
	return all, nil
}

// DeleteSchedule removes a schedule from every shard.
func (s *ShardedStore) DeleteSchedule(ctx context.Context, name string) error {
	return s.forEachShard(func(store *PostgresStore) error {
		return store.DeleteSchedule(ctx, name)
	})
}

// SetScheduleEnabled updates a schedule on every shard.
func (s *ShardedStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error {
	return s.forEachShard(func(store *PostgresStore) error {
		return store.SetScheduleEnabled(ctx, name, enabled)
	})
}

// GetDueSchedules collects due schedules from every shard (deduped by name).
func (s *ShardedStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) {
	seen := make(map[string]bool)
	var all []Schedule
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		schedules, err := shard.Store.GetDueSchedules(ctx)
		if err != nil {
			return nil, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		for _, sch := range schedules {
			if !seen[sch.Name] {
				seen[sch.Name] = true
				all = append(all, sch)
			}
		}
	}
	return all, nil
}

// UpdateScheduleNextRun updates a schedule on every shard.
func (s *ShardedStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error {
	return s.forEachShard(func(store *PostgresStore) error {
		return store.UpdateScheduleNextRun(ctx, name, nextRun)
	})
}

// LoadWorkflowConfig tries each shard (defs are replicated across shards).
func (s *ShardedStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (int, error) {
	var lastErr error
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		maxHistory, err := shard.Store.LoadWorkflowConfig(ctx, defName, defVersion)
		if err == nil {
			return maxHistory, nil
		}
		lastErr = err
	}
	return 0, lastErr
}

// LoadDAGSpec tries each shard (defs are replicated across shards).
func (s *ShardedStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
	var lastErr error
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		spec, err := shard.Store.LoadDAGSpec(ctx, defName, defVersion)
		if err == nil {
			return spec, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// TraceWorkflow routes by workflow ID.
func (s *ShardedStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) (sql.Result, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return nil, fmt.Errorf("trace_workflow: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.TraceWorkflow(ctx, workflowID, traceID)
}

// GetCompactionCandidates runs on every shard and merges results.
func (s *ShardedStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) {
	seen := make(map[string]bool)
	var all []string
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		ids, err := shard.Store.GetCompactionCandidates(ctx, threshold, limit)
		if err != nil {
			return nil, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				all = append(all, id)
			}
		}
	}
	if len(all) > limit && limit > 0 {
		all = all[:limit]
	}
	return all, nil
}

// LoadCompactionState routes by workflow ID.
func (s *ShardedStore) LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return nil, fmt.Errorf("load_compaction_state: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.LoadCompactionState(ctx, workflowID)
}

// CompactHistory routes by workflow ID.
func (s *ShardedStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("compact_history: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.CompactHistory(ctx, workflowID, compactionState, compactionStep, keepStep)
}

// ---------------------------------------------------------------------------
// SignalStore compatibility
// ---------------------------------------------------------------------------

// PollSignal satisfies the SignalStore interface.  It routes by workflow ID.
func (s *ShardedStore) PollSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return "", false, fmt.Errorf("poll_signal: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.PollSignal(ctx, workflowID, signalName)
}

// PollCancellation satisfies the SignalStore interface.  It routes by workflow ID.
func (s *ShardedStore) PollCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return false, "", fmt.Errorf("poll_cancellation: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.PollCancellation(ctx, workflowID)
}

// CreatePromise routes by workflow ID.
func (s *ShardedStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("create_promise: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.CreatePromise(ctx, workflowID, promiseName, promiseID)
}

// ResolvePromise routes by workflow ID.
func (s *ShardedStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("resolve_promise: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.ResolvePromise(ctx, workflowID, promiseID, result)
}

// RejectPromise routes by workflow ID.
func (s *ShardedStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("reject_promise: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.RejectPromise(ctx, workflowID, promiseID, errMsg)
}

// GetPromise routes by workflow ID.
func (s *ShardedStore) GetPromise(ctx context.Context, workflowID, promiseID string) (string, string, string, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return "", "", "", fmt.Errorf("get_promise: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.GetPromise(ctx, workflowID, promiseID)
}

// ListPromises routes by workflow ID.
func (s *ShardedStore) ListPromises(ctx context.Context, workflowID string) ([]PromiseInfo, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return nil, fmt.Errorf("list_promises: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.ListPromises(ctx, workflowID)
}

// ---------------------------------------------------------------------------
// Concurrency Key methods (Feature 5)
// ---------------------------------------------------------------------------

// AcquireConcurrencyKey routes by key text hash for consistent sharding.
func (s *ShardedStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	shard := s.getShard(key)
	if shard == nil {
		return false, fmt.Errorf("acquire_concurrency_key: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.AcquireConcurrencyKey(ctx, key, workflowID, ttl)
}

// ReleaseConcurrencyKey routes by key text hash.
func (s *ShardedStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	shard := s.getShard(key)
	if shard == nil {
		return fmt.Errorf("release_concurrency_key: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.ReleaseConcurrencyKey(ctx, key)
}

// ReleaseWorkflowConcurrencyKeys routes by workflow ID.
func (s *ShardedStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("release_workflow_concurrency_keys: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.ReleaseWorkflowConcurrencyKeys(ctx, workflowID)
}

// ReapExpiredConcurrencyKeys runs on every shard and returns the total count.
func (s *ShardedStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) {
	var total int64
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		n, err := shard.Store.ReapExpiredConcurrencyKeys(ctx)
		if err != nil {
			return total, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		total += n
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// Sticky Session methods (Feature 10)
// ---------------------------------------------------------------------------

// UpdateStickyWorker routes by workflow ID.
func (s *ShardedStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("update_sticky_worker: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.UpdateStickyWorker(ctx, workflowID, workerID)
}

// ClearStickyWorker routes by workflow ID.
func (s *ShardedStore) ClearStickyWorker(ctx context.Context, workflowID string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("clear_sticky_worker: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.ClearStickyWorker(ctx, workflowID)
}

// ---------------------------------------------------------------------------
// Update Request methods (Feature 3: Update Handler)
// ---------------------------------------------------------------------------

// CreateUpdateRequest routes by workflow ID.
func (s *ShardedStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("create_update_request: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.CreateUpdateRequest(ctx, workflowID, updateName, payload, promiseID)
}

// GetPendingUpdateRequests routes by workflow ID.
func (s *ShardedStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]UpdateRequestInfo, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return nil, fmt.Errorf("get_pending_update_requests: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.GetPendingUpdateRequests(ctx, workflowID)
}

// CompleteUpdateRequest routes by workflow ID.
func (s *ShardedStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("complete_update_request: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.CompleteUpdateRequest(ctx, workflowID, updateName, result, errMsg)
}

// ---- Version management methods ----

// DeployWorkflowDef delegates to the shard determined by the workflow name.
func (s *ShardedStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error {
	shard := s.getShard(def.Name)
	if shard == nil {
		return fmt.Errorf("deploy_workflow_def: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.DeployWorkflowDef(ctx, def)
}

// ListWorkflowDefs queries each shard and aggregates results.
func (s *ShardedStore) ListWorkflowDefs(ctx context.Context, name string) ([]WorkflowDef, error) {
	var all []WorkflowDef
	err := s.forEachShard(func(store *PostgresStore) error {
		defs, err := store.ListWorkflowDefs(ctx, name)
		if err != nil {
			return err
		}
		all = append(all, defs...)
		return nil
	})
	return all, err
}

// GetWorkflowDef delegates to the shard determined by the workflow name.
func (s *ShardedStore) GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error) {
	shard := s.getShard(name)
	if shard == nil {
		return nil, fmt.Errorf("get_workflow_def: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.GetWorkflowDef(ctx, name, version)
}

// MarkVersionDeprecated delegates to the shard determined by the workflow name.
func (s *ShardedStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error {
	shard := s.getShard(name)
	if shard == nil {
		return fmt.Errorf("mark_version_deprecated: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.MarkVersionDeprecated(ctx, name, version, deprecated)
}

// PurgeWorkflowDef delegates to the shard determined by the workflow name.
func (s *ShardedStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error {
	shard := s.getShard(name)
	if shard == nil {
		return fmt.Errorf("purge_workflow_def: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.PurgeWorkflowDef(ctx, name, version)
}

// CountActiveInstances delegates to the shard determined by the workflow name.
func (s *ShardedStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) {
	shard := s.getShard(name)
	if shard == nil {
		return 0, fmt.Errorf("count_active_instances: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.CountActiveInstances(ctx, name, version)
}

// GetActiveInstanceCountsByVersion queries each shard and aggregates results.
func (s *ShardedStore) GetActiveInstanceCountsByVersion(ctx context.Context) (map[string]int, error) {
	result := make(map[string]int)
	err := s.forEachShard(func(store *PostgresStore) error {
		counts, err := store.GetActiveInstanceCountsByVersion(ctx)
		if err != nil {
			return err
		}
		for k, v := range counts {
			result[k] += v
		}
		return nil
	})
	return result, err
}

// ResolveLatestVersion delegates to the shard determined by the workflow name.
func (s *ShardedStore) ResolveLatestVersion(ctx context.Context, defName string) (int, error) {
	shard := s.getShard(defName)
	if shard == nil {
		return 0, fmt.Errorf("resolve_latest_version: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.ResolveLatestVersion(ctx, defName)
}

// ValidateVersion delegates to the shard determined by the workflow name.
func (s *ShardedStore) ValidateVersion(ctx context.Context, defName string, defVersion int) (bool, error) {
	shard := s.getShard(defName)
	if shard == nil {
		return false, fmt.Errorf("validate_version: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.ValidateVersion(ctx, defName, defVersion)
}

// RecordWorkflowMemorySample routes by defName hash for consistent shard affinity.
func (s *ShardedStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error {
	shard := s.getShard(defName)
	if shard == nil {
		return fmt.Errorf("record_memory_sample: no shard available")
	}
	return shard.Store.RecordWorkflowMemorySample(ctx, defName, sampleBytes)
}

// LoadMemoryEstimates fans out to all shards and merges results.
func (s *ShardedStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) {
	result := make(map[string]float64)
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		estimates, err := shard.Store.LoadMemoryEstimates(ctx)
		if err != nil {
			return nil, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		for k, v := range estimates {
			result[k] = v
		}
	}
	return result, nil
}

// LoadMemoryStats fans out to all shards and appends results.
func (s *ShardedStore) LoadMemoryStats(ctx context.Context) ([]WorkflowMemoryStats, error) {
	var all []WorkflowMemoryStats
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		stats, err := shard.Store.LoadMemoryStats(ctx)
		if err != nil {
			return nil, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		all = append(all, stats...)
	}
	return all, nil
}

// QueueDepth fans out to all shards and sums the counts.
func (s *ShardedStore) QueueDepth(ctx context.Context) (int64, error) {
	var total int64
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		n, err := shard.Store.QueueDepth(ctx)
		if err != nil {
			return total, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		total += n
	}
	return total, nil
}

// CleanupMemorySamples fans out to all shards and sums deleted counts.
func (s *ShardedStore) CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error) {
	var total int64
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		n, err := shard.Store.CleanupMemorySamples(ctx, maxSamplesPerDef)
		if err != nil {
			return total, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		total += n
	}
	return total, nil
}

// DeleteExpiredEvents fans out to all shards and sums the deleted counts.
// Errors from individual shards are collected and returned as a single
// multi-error; remaining shards are still processed.
func (s *ShardedStore) DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	var total int64
	var errs []string
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		n, err := shard.Store.DeleteExpiredEvents(ctx, olderThan)
		if err != nil {
			errs = append(errs, fmt.Sprintf("shard %q: %v", shard.Config.Name, err))
			continue
		}
		total += n
	}
	if len(errs) > 0 {
		return total, fmt.Errorf("DeleteExpiredEvents errors: %s", strings.Join(errs, "; "))
	}
	return total, nil
}
