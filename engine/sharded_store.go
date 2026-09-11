package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	// "database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// ShardConfig is a single database shard configuration loaded from JSON.
type ShardConfig struct {
	Name    string   `json:"name"`
	ConnStr string   `json:"conn_str"`
	Schema  string   `json:"schema,omitempty"` // PostgreSQL schema name; "public" if empty
	Tenants []string `json:"tenants,omitempty"`
}

var errNoRows = fmt.Errorf("no rows")

// Shard wraps a WorkflowStore for a single database shard.
type Shard struct {
	Config ShardConfig
	Store  WorkflowStore
	Close  func() error
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

	// claimCursor rotates the shard a claim starts from. Claims walk shards in
	// order and stop once the budget is spent, so a fixed starting point would
	// drain shard 0 first and starve the tail under sustained load.
	claimCursor atomic.Uint64
}

// NewShardedStore creates a ShardedStore from pre-constructed WorkflowStore
// instances (one per shard). Each store is already initialized and migrated.
// The stores slice must be non-empty and have the same length as configs.
// closers are called when the ShardedStore is closed; one per store.
func NewShardedStore(configs []ShardConfig, stores []WorkflowStore, closers []func() error) (*ShardedStore, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("sharded store: at least one shard config is required")
	}
	if len(configs) != len(stores) {
		return nil, fmt.Errorf("sharded store: %d configs but %d stores", len(configs), len(stores))
	}
	if len(stores) != len(closers) {
		return nil, fmt.Errorf("sharded store: %d stores but %d closers", len(stores), len(closers))
	}

	shards := make([]*Shard, len(configs))
	for i, cfg := range configs {
		shards[i] = &Shard{
			Config: cfg,
			Store:  stores[i],
			Close:  closers[i],
		}
	}

	return &ShardedStore{shards: shards}, nil
}

// Close closes all shard stores.
func (s *ShardedStore) Close() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, shard := range s.shards {
		if shard.Close != nil {
			shard.Close()
		}
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

// stripChildSuffix returns the root ancestor UUID portion of a workflow ID.
// Child IDs have the form "rootUUID.c{step}" — stripping from ".c" onward
// yields the root ancestor. Regular UUIDs without ".c" are returned unchanged.
func stripChildSuffix(key string) string {
	if idx := strings.Index(key, ".c"); idx >= 0 {
		return key[:idx]
	}
	return key
}

// getShard returns the shard responsible for the given workflow key using
// consistent hashing. Child workflow IDs are stripped to their root ancestor
// so the entire workflow family lands on the same shard.
func (s *ShardedStore) getShard(key string) *Shard {
	key = stripChildSuffix(key)
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
func (s *ShardedStore) tryEachShard(fn func(WorkflowStore) (done bool, err error)) error {
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
	return errNoRows
}

// forEachShard calls fn on every shard and accumulates errors.
func (s *ShardedStore) forEachShard(fn func(WorkflowStore) error) error {
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
func (s *ShardedStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) {
	wfs, err := s.ClaimWorkflows(ctx, workerID, 1)
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
// CountRunnableWorkflows sums every shard, because ClaimWorkflows walks every
// shard. A count from one shard would answer a different question than the
// claim asks and would under-report whenever the runnable work is elsewhere.
//
// A shard that errors is skipped rather than failing the whole count: this
// feeds a diagnostic log line, and refusing to report anything because one
// shard is unreachable is worse than reporting what the others say. The caller
// treats the number as a floor.
func (s *ShardedStore) CountRunnableWorkflows(ctx context.Context) (int, error) {
	total := 0
	for _, sh := range s.shards {
		n, err := sh.Store.CountRunnableWorkflows(ctx)
		if err != nil {
			continue
		}
		total += n
	}
	return total, nil
}

func (s *ShardedStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	return s.claimAcrossShards(ctx, workerID, limit, "claim_workflows",
		func(sh *Shard, budget int) ([]*WorkflowInstance, error) {
			return sh.Store.ClaimWorkflows(ctx, workerID, budget)
		})
}

// claimAcrossShards walks shards in order, spending a decreasing budget, and
// stops as soon as the budget is exhausted.
//
// It used to fan out to every shard concurrently with the *full* limit and then
// truncate the merged slice. The return value respected the limit, which is why
// nothing noticed -- but the rows beyond it had already been updated to
// status='running' with assigned_to set to this worker, in their own shards, in
// committed transactions. Truncating a slice does not release them: they were
// claimed by a worker that would never execute them and stayed that way until
// the lease or heartbeat reaper took them back. With S shards and limit L, one
// poll could strand (S-1)*L workflows. See IMPROVEMENT-PLAN 2.17.
//
// Sequential is not the cost it looks like. When work is available the first
// shard usually fills the budget and the loop stops after one round-trip --
// fewer than the fan-out made. The serial walk only happens when shards are
// empty, which is exactly when claim latency does not matter.
func (s *ShardedStore) claimAcrossShards(ctx context.Context, workerID string, limit int, op string,
	claim func(sh *Shard, budget int) ([]*WorkflowInstance, error)) ([]*WorkflowInstance, error) {

	if limit <= 0 {
		return nil, nil
	}

	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	if len(shards) == 0 {
		return nil, nil
	}

	start := int(s.claimCursor.Add(1)-1) % len(shards)

	var all []*WorkflowInstance
	for i := 0; i < len(shards); i++ {
		budget := limit - len(all)
		if budget <= 0 {
			break
		}
		sh := shards[(start+i)%len(shards)]
		wfs, err := claim(sh, budget)
		if err != nil {
			return nil, fmt.Errorf("shard %q: %w", sh.Config.Name, err)
		}
		// A shard that returns more than it was asked for has already
		// committed those rows, so this cannot be silently truncated: it is a
		// bug in that store, and the whole point of 2.17 was that the excess
		// is invisible from the return value.
		if len(wfs) > budget {
			return nil, fmt.Errorf("%s: shard %q returned %d workflows for a budget of %d; "+
				"the excess is already claimed in that shard and will be stranded",
				op, sh.Config.Name, len(wfs), budget)
		}
		all = append(all, wfs...)
	}

	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

// ClaimStickyWorkflows claims up to limit sticky workflow instances across all shards.
// Sticky workflows use idx_instances_sticky for low-contention claiming.
// Iterates through shards collecting workflows until limit is reached or shards exhausted.
func (s *ShardedStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	return s.claimAcrossShards(ctx, workerID, limit, "claim_sticky_workflows",
		func(sh *Shard, budget int) ([]*WorkflowInstance, error) {
			return sh.Store.ClaimStickyWorkflows(ctx, workerID, budget)
		})
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

// GetWASMLength returns the byte length of the stored WASM binary.
func (s *ShardedStore) GetWASMLength(ctx context.Context, defName string, defVersion int) (int64, error) {
	var lastErr error
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		length, err := shard.Store.GetWASMLength(ctx, defName, defVersion)
		if err == nil {
			return length, nil
		}
		lastErr = err
	}
	return 0, lastErr
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
func (s *ShardedStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return false, fmt.Errorf("heartbeat: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.Heartbeat(ctx, workflowID, workerID, generation)
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

// CountEventHistory routes by workflow ID.
func (s *ShardedStore) CountEventHistory(ctx context.Context, workflowID string) (int, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return 0, fmt.Errorf("no shard available for workflow %s", workflowID)
	}
	return shard.Store.CountEventHistory(ctx, workflowID)
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
func (s *ShardedStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("no shard available for workflow %s", workflowID)
	}
	return shard.Store.MoveToDeadLetterQueue(ctx, workflowID, workerID, generation, errMsg, errorCode, errorOp)
}

// RetryWorkflow routes by workflow ID.
func (s *ShardedStore) RetryWorkflow(ctx context.Context, workflowID string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("no shard available for workflow %s", workflowID)
	}
	return shard.Store.RetryWorkflow(ctx, workflowID)
}

// CompleteWorkflow routes by workflow ID.
func (s *ShardedStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("complete_workflow: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.CompleteWorkflow(ctx, workflowID, workerID, generation, result, queryState)
}

// FailWorkflow routes by workflow ID.
func (s *ShardedStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("fail_workflow: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.FailWorkflow(ctx, workflowID, workerID, generation, errorMsg, errorCode, errorOp, queryState)
}

// ReleaseWorkflow routes by workflow ID.
func (s *ShardedStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("release_workflow: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.ReleaseWorkflow(ctx, workflowID, workerID, generation, nextWakeAt)
}

// ContinueAsNew routes by current run ID so that both the new-run insert and
// the old-run completion land on the same shard.
func (s *ShardedStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, result string, queryState map[string]string, priority int) (string, error) {
	shard := s.getShard(currentRunID)
	if shard == nil {
		return "", fmt.Errorf("continue_as_new: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.ContinueAsNew(ctx, currentRunID, workerID, generation, defName, defVersion, newInput, newEvents, result, queryState, priority)
}

// FinalizeWorkflowSegment routes by workflow ID.
func (s *ShardedStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	shard := s.getShard(runID)
	if shard == nil {
		return fmt.Errorf("finalize_workflow_segment: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.FinalizeWorkflowSegment(ctx, runID, workerID, generation, newEvents, finalStatus, result, errorCode, errorOp, queryState, nextWakeAt)
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

// ConsumeSignal routes by workflow ID.
//
// This is the reason ConsumeSignal takes a workflowID it does not strictly
// need to identify the row: an id alone cannot be routed to a shard.
func (s *ShardedStore) ConsumeSignal(ctx context.Context, workflowID string, id int64) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("consume_signal: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.ConsumeSignal(ctx, workflowID, id)
}

// StartNewRun generates a UUID and routes by it, so the workflow lands on a
// shard determined by its own ID (not its definition name). This ensures all
// subsequent operations (LoadEventHistory, child workflows, signals, etc.)
// route to the same shard.
func (s *ShardedStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int) (string, bool, error) {
	if runID == "" {
		runID = uuid.New().String()
	}
	if tenantID == "" {
		tenantID = DefaultTenantUUID
	}
	shard := s.getShard(runID)
	if shard == nil {
		return "", false, fmt.Errorf("start_new_run: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.StartNewRun(ctx, runID, defName, defVersion, input, idempotencyKey, tenantID, priority)
}

// StartChildWorkflow places the child on the same shard as the parent.
// defVersion is passed through to the underlying store for version resolution.
func (s *ShardedStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	shard := s.getShard(parentID)
	if shard == nil {
		return "", fmt.Errorf("start_child_workflow: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.StartChildWorkflow(ctx, parentID, defName, inputJSON, defVersion, parentClosePolicy, priority)
}

// StartChildWorkflowAtomic routes by the root ancestor UUID (stripping .c suffix)
// so the child lands on the same shard as its family. Generates a shard-aligned
// ".c{step}.{rand}" child ID if childID is empty. The random suffix guarantees
// uniqueness across generations (a parent and its child can both be at step 5).
func (s *ShardedStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (string, error) {
	if childID == "" {
		rootID := stripChildSuffix(parentID)
		// 12 hex chars from the UUID (48 bits) guarantees uniqueness within
		// the max 1000 children per parent (birthday bound P < 10^-9).
		randSuffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:12]
		childID = fmt.Sprintf("%s.c%d.%s", rootID, event.Step, randSuffix)
	}
	shard := s.getShard(childID)
	if shard == nil {
		return "", fmt.Errorf("start_child_workflow_atomic: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.StartChildWorkflowAtomic(ctx, childID, parentID, defName, inputJSON, defVersion, parentClosePolicy, event, priority)
}

// GetChildResult routes by child run ID.
func (s *ShardedStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	// Resolve the chain ACROSS shards before routing, not after. The concrete
	// stores resolve it too (cleat#955), but only within themselves -- and a
	// continue-as-new chain crosses shards routinely, because every
	// continuation gets a fresh id and getShard hashes the id. Routing on the
	// id the parent holds sends the question to the shard with run 1, whose
	// local walk sees no successor and answers with run 1's empty result: the
	// original defect, reappearing on exactly the deployment the per-store fix
	// looks like it covers.
	terminal, err := terminalRunID(ctx, runID, s.successorAcrossShards)
	if err != nil {
		return "", false, err
	}
	shard := s.getShard(terminal)
	if shard == nil {
		return "", false, fmt.Errorf("get_child_result: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.GetChildResult(ctx, terminal)
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

// GetTerminalRun follows a ContinueAsNew chain forward from id. See
// WorkflowStore.
//
// Delegated per hop rather than to one shard, because a chain is NOT
// shard-local: every continuation gets a fresh id, and getShard hashes the id,
// so successive runs of one chain routinely land on different shards. Walking
// inside a single shard's GetTerminalRun would stop at the first hop that
// moved, and report an intermediate run as terminal -- a wrong answer rather
// than an error, which is the failure mode worth avoiding here.
//
// So the walk stays at this level and each successor lookup is a fan-out, the
// same way GetWorkflowByID already scans shards for one id.
//
// The fan-out asks runSuccessorFinder, NOT GetTerminalRun. That is the fix for
// the bug this design was already meant to avoid and did not: a per-shard
// GetTerminalRun reads its head first and returns nil when the shard does not
// hold the id, so the shard holding the SUCCESSOR -- which by definition does
// not hold its predecessor -- returned nil before ever consulting
// continued_from. Every cross-shard hop was invisible, and the walk stopped at
// the first one, reporting an intermediate run as terminal. Exactly the wrong
// answer the comment above says the design exists to prevent, which is why
// TestShardedGetTerminalRunCrossesAShardBoundary is written against mocks that
// answer only for ids they hold.
func (s *ShardedStore) GetTerminalRun(ctx context.Context, id string) (*WorkflowInstance, error) {
	return walkToTerminalRun(ctx, id, s.successorAcrossShards, s.GetWorkflowByID)
}

// successorAcrossShards asks every shard which run continued from cur.
//
// A chain is not shard-local: every continuation gets a fresh id and getShard
// hashes the id, so run N and run N+1 routinely live on different shards. Only
// one shard can hold the successor, and which one is not predictable from cur,
// so this asks all of them.
func (s *ShardedStore) successorAcrossShards(ctx context.Context, cur string) (string, error) {
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, sh := range shards {
		finder, ok := sh.Store.(runSuccessorFinder)
		if !ok {
			// Loudly, not silently. A shard that cannot answer the successor
			// question makes every chain crossing into it invisible, and the
			// symptom is a plausible id rather than an error -- the failure
			// mode this whole mechanism is about.
			return "", fmt.Errorf("sharded chain walk: shard %q (%T) cannot look up "+
				"continue-as-new successors, so a chain crossing it would silently "+
				"appear to end", sh.Config.Name, sh.Store)
		}
		next, err := finder.successorOfRun(ctx, cur)
		if err != nil {
			return "", err
		}
		if next != "" {
			return next, nil
		}
	}
	return "", nil
}

// CreateSchedule registers a schedule on every shard.
func (s *ShardedStore) CreateSchedule(ctx context.Context, sch Schedule) error {
	return s.forEachShard(func(store WorkflowStore) error {
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
	return s.forEachShard(func(store WorkflowStore) error {
		return store.DeleteSchedule(ctx, name)
	})
}

// SetScheduleEnabled updates a schedule on every shard.
func (s *ShardedStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error {
	return s.forEachShard(func(store WorkflowStore) error {
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
func (s *ShardedStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("trace_workflow: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
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
func (s *ShardedStore) PollSignal(ctx context.Context, workflowID, signalName string) (SignalDelivery, bool, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return SignalDelivery{}, false, fmt.Errorf("poll_signal: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
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

// GetAllowedSignalCallers routes by workflow ID.
func (s *ShardedStore) GetAllowedSignalCallers(ctx context.Context, workflowID string) ([]string, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return nil, fmt.Errorf("get_allowed_signal_callers: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.GetAllowedSignalCallers(ctx, workflowID)
}

// PickVersionByRouting checks A/B routing rules for the given workflow name.
// Delegates to the shard determined by the workflow name.
func (s *ShardedStore) PickVersionByRouting(ctx context.Context, workflowName string) (int, error) {
	shard := s.getShard(workflowName)
	if shard == nil {
		return 0, nil
	}
	return shard.Store.PickVersionByRouting(ctx, workflowName)
}

// ResolveVersionByTag resolves a tag to a workflow definition version.
// Delegates to the shard determined by the workflow name.
func (s *ShardedStore) ResolveVersionByTag(ctx context.Context, workflowName string, tag string) (int, error) {
	shard := s.getShard(workflowName)
	if shard == nil {
		return 0, nil
	}
	return shard.Store.ResolveVersionByTag(ctx, workflowName, tag)
}

// SetWorkflowTag assigns a tag to a specific version.
// Delegates to the shard determined by the workflow name.
func (s *ShardedStore) SetWorkflowTag(ctx context.Context, workflowName string, version int, tag string) error {
	shard := s.getShard(workflowName)
	if shard == nil {
		return fmt.Errorf("set_workflow_tag: no shard available")
	}
	return shard.Store.SetWorkflowTag(ctx, workflowName, version, tag)
}

// RemoveWorkflowTag deletes a tag assignment.
// Delegates to the shard determined by the workflow name.
func (s *ShardedStore) RemoveWorkflowTag(ctx context.Context, workflowName string, tag string) error {
	shard := s.getShard(workflowName)
	if shard == nil {
		return fmt.Errorf("remove_workflow_tag: no shard available")
	}
	return shard.Store.RemoveWorkflowTag(ctx, workflowName, tag)
}

// GetWorkflowTag returns the version for a given tag.
// Delegates to the shard determined by the workflow name.
func (s *ShardedStore) GetWorkflowTag(ctx context.Context, workflowName string, tag string) (int, error) {
	shard := s.getShard(workflowName)
	if shard == nil {
		return 0, fmt.Errorf("get_workflow_tag: no shard available")
	}
	return shard.Store.GetWorkflowTag(ctx, workflowName, tag)
}

// GetWorkflowTags returns all tag -> version mappings for a workflow.
// Delegates to the shard determined by the workflow name.
func (s *ShardedStore) GetWorkflowTags(ctx context.Context, workflowName string) (map[string]int, error) {
	shard := s.getShard(workflowName)
	if shard == nil {
		return nil, fmt.Errorf("get_workflow_tags: no shard available")
	}
	return shard.Store.GetWorkflowTags(ctx, workflowName)
}

// SetRoutingRule creates a routing rule for a workflow version.
// Delegates to the shard determined by the workflow name.
func (s *ShardedStore) SetRoutingRule(ctx context.Context, workflowName string, targetVersion int, weight float64) error {
	shard := s.getShard(workflowName)
	if shard == nil {
		return fmt.Errorf("set_routing_rule: no shard available")
	}
	return shard.Store.SetRoutingRule(ctx, workflowName, targetVersion, weight)
}

// RemoveRoutingRule deletes a routing rule from every shard, the same fan-out
// DeleteSchedule uses, and for the same reason: the caller has an id but not
// the key the row was placed under.
//
// It used to delegate to getShard(ruleID), which is the wrong shard almost
// every time (cleat#946). Routing rules are written and read by workflow NAME
// -- SetRoutingRule and GetRoutingRules both use getShard(workflowName) -- and
// getShard is sha256(key) % len(shards), so a rule id says nothing about where
// its row lives. The id cannot help, because it is assigned by the database
// (`id UUID PRIMARY KEY DEFAULT gen_random_uuid()`) and has no relationship to
// the name.
//
// The two keys agree only by coincidence, at a rate of 1/len(shards):
//
//	shards   removals that reached a shard never holding the rule
//	2        50%
//	4        75%
//	8        87.5%
//
// and the failure is silent in the worst way. No dialect checks rows-affected,
// so a DELETE matching nothing returns nil and the API answers
// 200 {"status":"removed"} for a rule that is still present -- and still
// shifting live traffic, since PickVersionByRouting runs on every start.
//
// Fanning out is correct rather than merely safe here: rule ids are UUIDs and
// so globally unique, so at most one shard can hold the row and deleting by id
// on the others matches nothing. That is what makes this preferable to
// tryEachShard, which would need each store to distinguish "deleted" from "no
// such rule" -- an error where none exists today, changing what an unsharded
// store does about a rule id that is simply gone.
//
// Cost is len(shards) statements instead of one, on an operator action that
// happens when a canary is torn down.
// The no-shards refusal is kept deliberately. forEachShard iterates zero
// shards and returns nil, which would report success for a removal that could
// not have happened -- the same silent success this change exists to remove,
// arrived at from the other direction. TestRemoveRoutingRule_NoShard caught it.
func (s *ShardedStore) RemoveRoutingRule(ctx context.Context, ruleID string) error {
	s.mu.RLock()
	n := len(s.shards)
	s.mu.RUnlock()
	if n == 0 {
		return fmt.Errorf("remove_routing_rule: no shard available")
	}
	// Every shard is asked, as #948 established -- the rule ID is not the shard
	// key, so there is no way to know in advance which shard holds the row.
	//
	// What changed with cleat#946's second half: a store now returns
	// ErrRoutingRuleNotFound when its DELETE matches nothing, and with n shards
	// exactly n-1 of them legitimately do not hold the rule. forEachShard stops
	// at the FIRST error, so passing that sentinel through would abort the walk
	// at shard 0 and never reach the shard that has it -- reintroducing #948's
	// defect by way of fixing the reporting. It is swallowed per shard and
	// re-raised only if no shard claimed the row.
	found := false
	if err := s.forEachShard(func(store WorkflowStore) error {
		rErr := store.RemoveRoutingRule(ctx, ruleID)
		if errors.Is(rErr, ErrRoutingRuleNotFound) {
			return nil // not this shard; keep going
		}
		if rErr == nil {
			found = true
		}
		return rErr
	}); err != nil {
		return err
	}
	if !found {
		// No shard held it. Reported rather than swallowed: this is the case
		// the API answered 200 {"status":"removed"} for.
		return ErrRoutingRuleNotFound
	}
	return nil
}

// GetRoutingRules returns all routing rules for a workflow.
// Delegates to the shard determined by the workflow name.
func (s *ShardedStore) GetRoutingRules(ctx context.Context, workflowName string) ([]RoutingRule, error) {
	shard := s.getShard(workflowName)
	if shard == nil {
		return nil, fmt.Errorf("get_routing_rules: no shard available")
	}
	return shard.Store.GetRoutingRules(ctx, workflowName)
}

// CreatePromise routes by workflow ID.
func (s *ShardedStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("create_promise: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.CreatePromise(ctx, workflowID, promiseName, promiseID)
}

// ResolvePromise fans out: a settler has no workflow ID to route by.
//
// Every other promise call routes by workflow ID, but settling is done by
// something that holds only the promise ID -- that is the whole point of a
// promise -- so there is nothing to hash. The shard holding the row is found
// by asking each in turn, as ReapStaleInstances does.
//
// ErrPromiseNotFound from a shard means "not here", so it continues; any other
// error is real and stops. If no shard has it, the ErrPromiseNotFound is the
// honest answer and is returned.
func (s *ShardedStore) ResolvePromise(ctx context.Context, promiseID, result string) error {
	return s.settleAcrossShards(ctx, "resolve_promise", func(st WorkflowStore) error {
		return st.ResolvePromise(ctx, promiseID, result)
	})
}

// RejectPromise fans out, as ResolvePromise does and for the same reason.
func (s *ShardedStore) RejectPromise(ctx context.Context, promiseID, errMsg string) error {
	return s.settleAcrossShards(ctx, "reject_promise", func(st WorkflowStore) error {
		return st.RejectPromise(ctx, promiseID, errMsg)
	})
}

// settleAcrossShards applies settle to each shard until one reports something
// other than ErrPromiseNotFound.
func (s *ShardedStore) settleAcrossShards(ctx context.Context, op string, settle func(WorkflowStore) error) error {
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	if len(shards) == 0 {
		return fmt.Errorf("%s: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG", op)
	}
	for _, shard := range shards {
		err := settle(shard.Store)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrPromiseNotFound) {
			return fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
	}
	return ErrPromiseNotFound
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
// Quota methods
// ---------------------------------------------------------------------------

// GetChildCount routes by parent workflow ID.
func (s *ShardedStore) GetChildCount(ctx context.Context, parentWorkflowID string) (int, error) {
	shard := s.getShard(parentWorkflowID)
	if shard == nil {
		return 0, fmt.Errorf("get_child_count: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.GetChildCount(ctx, parentWorkflowID)
}

// GetConcurrencyKeyCount routes by workflow ID.
func (s *ShardedStore) GetConcurrencyKeyCount(ctx context.Context, workflowID string) (int, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return 0, fmt.Errorf("get_concurrency_key_count: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.GetConcurrencyKeyCount(ctx, workflowID)
}

// GetEventCount returns the event_count for a workflow instance.
func (s *ShardedStore) GetEventCount(ctx context.Context, workflowID string) (int, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return 0, fmt.Errorf("get_event_count: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.GetEventCount(ctx, workflowID)
}

// AcquireConcurrencyKey routes by key text hash for consistent sharding.
func (s *ShardedStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	shard := s.getShard(key)
	if shard == nil {
		return false, fmt.Errorf("acquire_concurrency_key: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.AcquireConcurrencyKey(ctx, key, workflowID, ttl)
}

// ReleaseConcurrencyKey routes by key text hash.
func (s *ShardedStore) ReleaseConcurrencyKey(ctx context.Context, key, workflowID string) error {
	shard := s.getShard(key)
	if shard == nil {
		return fmt.Errorf("release_concurrency_key: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.ReleaseConcurrencyKey(ctx, key, workflowID)
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

// SetAllowedSignalCallers routes by workflow ID.
func (s *ShardedStore) SetAllowedSignalCallers(ctx context.Context, workflowID string, callers []string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("set_allowed_signal_callers: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.SetAllowedSignalCallers(ctx, workflowID, callers)
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
	err := s.forEachShard(func(store WorkflowStore) error {
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
	err := s.forEachShard(func(store WorkflowStore) error {
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

func (s *ShardedStore) ClearExpiredCompactionState(ctx context.Context, olderThan time.Time) (int64, error) {
	var total int64
	var errs []string
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		n, err := shard.Store.ClearExpiredCompactionState(ctx, olderThan)
		if err != nil {
			errs = append(errs, fmt.Sprintf("shard %q: %v", shard.Config.Name, err))
			continue
		}
		total += n
	}
	if len(errs) > 0 {
		return total, fmt.Errorf("ClearExpiredCompactionState errors: %s", strings.Join(errs, "; "))
	}
	return total, nil
}

// TerminateWorkflow routes by workflow ID.
func (s *ShardedStore) TerminateWorkflow(ctx context.Context, workflowID, reason string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("terminate_workflow: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.TerminateWorkflow(ctx, workflowID, reason)
}

// LoadEventHistoryBatch returns event histories for multiple workflow IDs
// by dispatching per-ID to the appropriate shard.
func (s *ShardedStore) LoadEventHistoryBatch(ctx context.Context, workflowIDs []string) (map[string][]EventRecord, error) {
	result := make(map[string][]EventRecord, len(workflowIDs))
	for _, id := range workflowIDs {
		shard := s.getShard(id)
		if shard == nil {
			continue
		}
		events, err := shard.Store.LoadEventHistory(ctx, id)
		if err != nil {
			return nil, err
		}
		result[id] = events
	}
	return result, nil
}

// DeleteDeadLetteredWorkflows fans out to all shards and sums the deleted counts.
func (s *ShardedStore) DeleteDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	var total int64
	var errs []string
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		n, err := shard.Store.DeleteDeadLetteredWorkflows(ctx, olderThan)
		if err != nil {
			errs = append(errs, fmt.Sprintf("shard %q: %v", shard.Config.Name, err))
			continue
		}
		total += n
	}
	if len(errs) > 0 {
		return total, fmt.Errorf("DeleteDeadLetteredWorkflows errors: %s", strings.Join(errs, "; "))
	}
	return total, nil
}

// DeleteCompletedWorkflows fans out to all shards and sums the deleted counts.
func (s *ShardedStore) DeleteCompletedWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	var total int64
	var errs []string
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		n, err := shard.Store.DeleteCompletedWorkflows(ctx, olderThan)
		if err != nil {
			errs = append(errs, fmt.Sprintf("shard %q: %v", shard.Config.Name, err))
			continue
		}
		total += n
	}
	if len(errs) > 0 {
		return total, fmt.Errorf("DeleteCompletedWorkflows errors: %s", strings.Join(errs, "; "))
	}
	return total, nil
}

// metricsStore is a local interface for type-asserting whether a shard's
// underlying store supports metrics collection methods. This mirrors the
// MetricsStore interface in cmd/cleat-worker/metrics_store.go but lives
// in the host package to avoid import cycles.
type metricsStore interface {
	CountStalledWorkflows(ctx context.Context, threshold time.Duration) (int, error)
	CountEventHistoryTotal(ctx context.Context) (int, error)
	EstimateEventHistorySize(ctx context.Context) (int64, error)
	CountActiveConcurrencyKeys(ctx context.Context) (int, error)
}

// ---------------------------------------------------------------------------
// MetricsStore implementation (fans out to all shards and aggregates)
// ---------------------------------------------------------------------------

// CountStalledWorkflows returns the maximum stalled count across all shards.
// Using max rather than sum because stalled workflows on different shards are
// independent — the max captures the worst-case shard, which is the most
// actionable signal for operator attention.
func (s *ShardedStore) CountStalledWorkflows(ctx context.Context, threshold time.Duration) (int, error) {
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	var maxCount int
	for _, shard := range shards {
		ms, ok := shard.Store.(metricsStore)
		if !ok {
			continue
		}
		n, err := ms.CountStalledWorkflows(ctx, threshold)
		if err != nil {
			return 0, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		if n > maxCount {
			maxCount = n
		}
	}
	return maxCount, nil
}

// CountEventHistoryTotal returns the total row count across all shards.
func (s *ShardedStore) CountEventHistoryTotal(ctx context.Context) (int, error) {
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	var total int
	for _, shard := range shards {
		ms, ok := shard.Store.(metricsStore)
		if !ok {
			continue
		}
		n, err := ms.CountEventHistoryTotal(ctx)
		if err != nil {
			return 0, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		total += n
	}
	return total, nil
}

// EstimateEventHistorySize returns the estimated total size across all shards.
func (s *ShardedStore) EstimateEventHistorySize(ctx context.Context) (int64, error) {
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	var total int64
	for _, shard := range shards {
		ms, ok := shard.Store.(metricsStore)
		if !ok {
			continue
		}
		n, err := ms.EstimateEventHistorySize(ctx)
		if err != nil {
			return 0, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		total += n
	}
	return total, nil
}

// CountActiveConcurrencyKeys returns the total active concurrency keys across all shards.
func (s *ShardedStore) CountActiveConcurrencyKeys(ctx context.Context) (int, error) {
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	var total int
	for _, shard := range shards {
		ms, ok := shard.Store.(metricsStore)
		if !ok {
			continue
		}
		n, err := ms.CountActiveConcurrencyKeys(ctx)
		if err != nil {
			return 0, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		total += n
	}
	return total, nil
}

// StreamEventHistory routes by workflow ID.
func (s *ShardedStore) StreamEventHistory(ctx context.Context, workflowID string, pageSize int) (<-chan EventRecord, <-chan error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		errCh := make(chan error, 1)
		errCh <- fmt.Errorf("stream_event_history: no shard available for workflow %s", workflowID)
		ch := make(chan EventRecord)
		close(ch)
		return ch, errCh
	}
	return shard.Store.StreamEventHistory(ctx, workflowID, pageSize)
}

// ResolveTenantFromAPIKey looks up a tenant UUID by API key hash across all shards.
func (s *ShardedStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) {
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		tid, err := shard.Store.ResolveTenantFromAPIKey(ctx, keyHash)
		if err == nil {
			return tid, nil
		}
	}
	return uuid.Nil, fmt.Errorf("tenant not found for API key")
}

// AdminForceComplete marks a workflow as done, bypassing worker ownership.
func (s *ShardedStore) AdminForceComplete(ctx context.Context, workflowID string, generation int64, result string, operator string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("admin force-complete: no shard for workflow %s", workflowID)
	}
	return shard.Store.AdminForceComplete(ctx, workflowID, generation, result, operator)
}

// AdminForceFail marks a workflow as failed, bypassing worker ownership.
func (s *ShardedStore) AdminForceFail(ctx context.Context, workflowID string, generation int64, errorMsg, errorCode string, operator string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("admin force-fail: no shard for workflow %s", workflowID)
	}
	return shard.Store.AdminForceFail(ctx, workflowID, generation, errorMsg, errorCode, operator)
}

// AdminReReplay replays a workflow's event history for debugging.
func (s *ShardedStore) AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error {
	shard := s.getShard(workflowID)
	if shard == nil {
		return fmt.Errorf("admin re-replay: no shard for workflow %s", workflowID)
	}
	return shard.Store.AdminReReplay(ctx, workflowID, generation, operator)
}

// ClaimDueSchedule claims on the shard that holds the schedule.
//
// This deliberately does NOT fan out to every shard.
//
// The comparison here used to be with UpdateScheduleNextRun, which fanned out
// and has since been deleted -- unfenced, superseded by this call, and reached
// by nothing that ships. The CAS is what decides who owns a firing instant, and a fan-out
// would report "claimed" if any shard's row matched -- turning a
// single-winner election into a poll. Schedules are replicated across shards,
// so the first shard whose row still holds expectedNextRun is the winner and
// the rest are already-advanced copies.
func (s *ShardedStore) ClaimDueSchedule(ctx context.Context, name string, expectedNextRun, newNextRun time.Time, runID string) (bool, error) {
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()

	claimed := false
	var lastErr error
	for _, shard := range shards {
		ok, err := shard.Store.ClaimDueSchedule(ctx, name, expectedNextRun, newNextRun, runID)
		if err != nil {
			lastErr = err
			continue
		}
		if ok {
			claimed = true
		}
	}
	if !claimed && lastErr != nil {
		return false, lastErr
	}
	return claimed, nil
}

// ---- DeferPhaseStore ----
//
// IMPROVEMENT-PLAN 3.75 step 2. ShardedStore has to implement this pair or a
// sharded deployment's terminate would mark a defer phase it could never
// finalize: TerminateWorkflow routes to the shard and marks the row, and with
// no FinalizeDeferPhase the worker's segment would have nowhere to apply the
// recorded outcome. The pair travels with TerminateWorkflow, not with the
// store type.

// FinalizeDeferPhase routes by workflow ID, like every other per-workflow
// write. The fence lives on the shard's own row, so nothing here needs to know
// about it.
func (s *ShardedStore) FinalizeDeferPhase(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord) error {
	shard := s.getShard(runID)
	if shard == nil {
		return fmt.Errorf("finalize_defer_phase: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	dps, ok := shard.Store.(DeferPhaseStore)
	if !ok {
		return fmt.Errorf("finalize_defer_phase: shard %q's store cannot finalize a defer phase, "+
			"so a terminate that marked one on it can never complete", shard.Config.Name)
	}
	return dps.FinalizeDeferPhase(ctx, runID, workerID, generation, newEvents)
}

// ExpireDeferPhases fans out, like ReapStaleInstances: a deadline is a property
// of a row rather than of a workflow this call knows the id of.
//
// A shard whose store cannot expire is skipped rather than fatal, because the
// same store could not have marked a phase either -- so it has nothing to
// expire, and failing the whole sweep over it would stop the shards that do.
func (s *ShardedStore) ExpireDeferPhases(ctx context.Context) (int, error) {
	total := 0
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		dps, ok := shard.Store.(DeferPhaseStore)
		if !ok {
			continue
		}
		n, err := dps.ExpireDeferPhases(ctx)
		if err != nil {
			return total, fmt.Errorf("shard %q: %w", shard.Config.Name, err)
		}
		total += n
	}
	return total, nil
}

// GetChildCompletedAtMs routes to the shard owning the child, matching
// GetChildResult.
func (s *ShardedStore) GetChildCompletedAtMs(ctx context.Context, runID string) (int64, bool, error) {
	shard := s.getShard(runID)
	if shard == nil {
		return 0, false, fmt.Errorf("get_child_completed_at: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	return shard.Store.GetChildCompletedAtMs(ctx, runID)
}
