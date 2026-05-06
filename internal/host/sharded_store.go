package host

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
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
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		wf, err := shard.Store.ClaimWorkflow(ctx, workerID, namespace)
		if err != nil {
			return nil, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		if wf != nil {
			return wf, nil
		}
	}
	return nil, nil
}

// LoadEventHistory routes by workflow ID.
func (s *ShardedStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return nil, fmt.Errorf("no shard available")
	}
	return shard.Store.LoadEventHistory(ctx, workflowID)
}

// AppendEventHistory routes by workflow ID.
func (s *ShardedStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("no shard available")
	}
	return shard.Store.AppendEventHistory(ctx, workflowID, rec)
}

// AppendEventHistoryBatch routes by workflow ID.
func (s *ShardedStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("no shard available")
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
		return false, fmt.Errorf("no shard available")
	}
	return shard.Store.Heartbeat(ctx, workflowID, workerID)
}

// CompleteWorkflow routes by workflow ID.
func (s *ShardedStore) CompleteWorkflow(ctx context.Context, workflowID, workerID, result string, queryState map[string]string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("no shard available")
	}
	return shard.Store.CompleteWorkflow(ctx, workflowID, workerID, result, queryState)
}

// FailWorkflow routes by workflow ID.
func (s *ShardedStore) FailWorkflow(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("no shard available")
	}
	return shard.Store.FailWorkflow(ctx, workflowID, workerID, errMsg, queryState)
}

// ReleaseWorkflow routes by workflow ID.
func (s *ShardedStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("no shard available")
	}
	return shard.Store.ReleaseWorkflow(ctx, workflowID, workerID, nextWakeAt)
}

// RequestCancellation routes by workflow ID.
func (s *ShardedStore) RequestCancellation(ctx context.Context, workflowID, reason string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("no shard available")
	}
	return shard.Store.RequestCancellation(ctx, workflowID, reason)
}

// CheckCancellation routes by workflow ID.
func (s *ShardedStore) CheckCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return false, "", fmt.Errorf("no shard available")
	}
	return shard.Store.CheckCancellation(ctx, workflowID)
}

// DeliverSignal routes by workflow ID.
func (s *ShardedStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("no shard available")
	}
	return shard.Store.DeliverSignal(ctx, workflowID, signalName, payload)
}

// PollAndClaimSignal routes by workflow ID.
func (s *ShardedStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return "", false, fmt.Errorf("no shard available")
	}
	return shard.Store.PollAndClaimSignal(ctx, workflowID, signalName)
}

// StartNewRun picks a shard by hashing the definition name so all runs of the
// same workflow type land on the same shard. The idempotencyKey is forwarded
// to the underlying store for exactly-once semantics.
func (s *ShardedStore) StartNewRun(ctx context.Context, defName string, defVersion int, input json.RawMessage, idempotencyKey string) (string, bool, error) {
	shard := s.getShard(defName)
	if shard == nil {
		return "", false, fmt.Errorf("no shard available")
	}
	return shard.Store.StartNewRun(ctx, defName, defVersion, input, idempotencyKey)
}

// StartChildWorkflow places the child on the same shard as the parent.
func (s *ShardedStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string) (string, error) {
	shard := s.getShard(parentID)
	if shard == nil {
		return "", fmt.Errorf("no shard available")
	}
	return shard.Store.StartChildWorkflow(ctx, parentID, defName, inputJSON)
}

// GetChildResult routes by child run ID.
func (s *ShardedStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	shard := s.getShard(runID)
	if shard == nil {
		return "", false, fmt.Errorf("no shard available")
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
		return "", fmt.Errorf("no shard available")
	}
	return shard.Store.GetQueryState(ctx, workflowID, key)
}

// ListWorkflows merges results from all shards.
func (s *ShardedStore) ListWorkflows(ctx context.Context, status string, limit int) ([]WorkflowInstance, error) {
	var all []WorkflowInstance
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		workflows, err := shard.Store.ListWorkflows(ctx, status, limit)
		if err != nil {
			return nil, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		all = append(all, workflows...)
	}
	if len(all) > limit && limit > 0 {
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

// TraceWorkflow routes by workflow ID.
func (s *ShardedStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) (sql.Result, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return nil, fmt.Errorf("no shard available")
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
		return nil, fmt.Errorf("no shard available")
	}
	return shard.Store.LoadCompactionState(ctx, workflowID)
}

// CompactHistory routes by workflow ID.
func (s *ShardedStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("no shard available")
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
		return "", false, fmt.Errorf("no shard available")
	}
	return shard.Store.PollSignal(ctx, workflowID, signalName)
}

// PollCancellation satisfies the SignalStore interface.  It routes by workflow ID.
func (s *ShardedStore) PollCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return false, "", fmt.Errorf("no shard available")
	}
	return shard.Store.PollCancellation(ctx, workflowID)
}
