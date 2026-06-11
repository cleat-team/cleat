package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero/api"
)

func (s *execSession) ChildWorkflow(ctx context.Context, m api.Module, name, inputJSON string, runIDPtr, runIDMaxLen uint32) int64 {
	return s.childWorkflowWithVersion(ctx, m, name, inputJSON, 0, 0, "", runIDPtr, runIDMaxLen)
}

func (s *execSession) ChildWorkflowWithOptions(ctx context.Context, m api.Module, name, inputJSON string, version int64, priority int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	return s.childWorkflowWithVersion(ctx, m, name, inputJSON, int(version), int(priority), parentClosePolicy, runIDPtr, runIDMaxLen)
}

// ChildWorkflowInSchema starts a child workflow in a target PostgreSQL schema.
// This enables cross-instance cooperation: a workflow in schema A can spawn a
// child in schema B, where B's worker pool claims and executes it.
//
// The target schema MUST be in the engine's configured peerSchemas (or be the
// engine's own schema).  An empty targetSchema falls back to the local schema.

func (s *execSession) ChildWorkflowInSchema(ctx context.Context, m api.Module, targetSchema, name, inputJSON string, version int64, priority int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	// Validate: target schema must be a peer or our own schema.
	if targetSchema != "" && targetSchema != s.engine.schema {
		allowed := false
		for _, p := range s.engine.peerSchemas {
			if p == targetSchema {
				allowed = true
				break
			}
		}
		if !allowed {
			errMsg := fmt.Sprintf("child workflow %q: target schema %q is not an allowed peer", name, targetSchema)
			errWritten, _ := s.writeResult(ctx, m, runIDPtr, errMsg, runIDMaxLen)
			return int64(uint64(errWritten)<<32 | 4) // errCode 4 = invalid
		}
	}

	return s.childWorkflowWithVersion(ctx, m, name, inputJSON, int(version), int(priority), parentClosePolicy, runIDPtr, runIDMaxLen, targetSchema)
}

// resolveChildVersion resolves the child workflow version by priority:
//  1. Explicit version from ChildWorkflowOptions (version > 0 from WASM ABI)
//  2. Runtime override (engine.childBindingOverride)
//  3. Binding policy from WASM metadata (engine.childBindingPolicy)
//  4. Fallback: 0 means DB resolves to MAX(version)
//
// Cross-schema children (targetSchema != "") skip policy resolution
// and return explicitVersion (if > 0) or 0 (DB fallback).
func (s *execSession) resolveChildVersion(ctx context.Context, name string, explicitVersion int, targetSchema string) int {
	if explicitVersion > 0 {
		return explicitVersion
	}

	// Cross-schema children should still use explicit version or fallback to MAX.
	if targetSchema != "" {
		return 0
	}

	// Check runtime override first (env var or worker flag for debugging).
	// An override completely bypasses the compiled-in policy.
	override := s.engine.childBindingOverride
	if override != "" {
		if override == "latest" {
			return 0 // DB resolves to MAX; skip metadata policy
		}
		if strings.HasPrefix(override, "tag:") {
			tag := strings.TrimPrefix(override, "tag:")
			if s.engine.childWfStore != nil {
				if v, err := s.engine.childWfStore.ResolveVersionByTag(ctx, name, tag); err == nil && v > 0 {
					s.engine.log().InfoContext(ctx, "child version resolved by runtime override",
						"name", name, "tag", tag, "version", v)
					return v
				}
			}
		}
	}

	// If no override resolved, apply metadata policy
	if s.engine.state != nil {
		policy := s.engine.childBindingPolicy // from metadata, set by worker
		if policy == "" {
			// Backwards compat: if pinned versions exist, use frozen; else latest
			if _, ok := s.engine.state.ChildVersion(name); ok {
				policy = "frozen"
			} else {
				policy = "latest"
			}
		}

		switch {
		case policy == "frozen":
			if pinnedVersion, ok := s.engine.state.ChildVersion(name); ok && pinnedVersion > 0 {
				s.engine.log().InfoContext(ctx, "child version resolved by frozen policy",
					"name", name, "version", pinnedVersion)
				return pinnedVersion
			}
			s.engine.log().InfoContext(ctx, "child version: frozen policy, no pinned version",
				"name", name)
		case policy == "stable":
			if s.engine.childWfStore != nil {
				if v, err := s.engine.childWfStore.ResolveVersionByTag(ctx, name, "stable"); err == nil && v > 0 {
					s.engine.log().InfoContext(ctx, "child version resolved by stable policy",
						"name", name, "version", v)
					return v
				}
			}
			s.engine.log().InfoContext(ctx, "child version: stable policy resolution failed",
				"name", name)
		case policy == "latest":
			s.engine.log().InfoContext(ctx, "child version: latest policy",
				"name", name)
			// Leave childVersion = 0, DB resolves to MAX
		case strings.HasPrefix(policy, "tag:"):
			tag := strings.TrimPrefix(policy, "tag:")
			if s.engine.childWfStore != nil {
				if v, err := s.engine.childWfStore.ResolveVersionByTag(ctx, name, tag); err == nil && v > 0 {
					s.engine.log().InfoContext(ctx, "child version resolved by tag policy",
						"name", name, "tag", tag, "version", v)
					return v
				}
			}
			s.engine.log().InfoContext(ctx, "child version: tag policy resolution failed",
				"name", name, "tag", tag)
		}
	}

	return 0
}

// childWorkflowWithVersion is the shared implementation for creating child workflows.
// If version <= 0, the parent's version is used as the default.
// If targetSchema is non-empty, the child is created in that PostgreSQL schema
// (cross-instance cooperation); otherwise the child is created locally.

func (s *execSession) childWorkflowWithVersion(ctx context.Context, m api.Module, name, inputJSON string, version int, priority int, parentClosePolicy string, runIDPtr, runIDMaxLen uint32, targetSchema ...string) int64 {
	ts := ""
	if len(targetSchema) > 0 {
		ts = targetSchema[0]
	}

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeChildWorkflow {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}

				written, _ := s.writeResult(ctx, m, runIDPtr, rec.RunID, runIDMaxLen)
				return int64(uint64(written)<<32 | 0)
			}
		}
		s.exitReplay()
	}

	// Resolve child version by priority:
	//   1. Explicit version from ChildWorkflowOptions (version > 0 from WASM ABI)
	//   2. Runtime override (engine.childBindingOverride)
	//   3. Binding policy from WASM metadata:
	//      - "frozen": use pinned ChildVersions from metadata
	//      - "stable": resolve against "stable" tag via store
	//      - "latest": resolve MAX(version) via store
	//      - "tag:X": resolve against tag X via store
	//      - "" (empty): use EffectivePolicy() logic
	//   4. Fallback: DB resolves version <= 0 to MAX(version) via CASE in INSERT
	childVersion := s.resolveChildVersion(ctx, name, version, ts)

	// Fresh execution: create child workflow atomically with event.
	var runID string
	parentID := s.workflowID
	if parentID == "" {
		parentID = fmt.Sprintf("unknown-%s-%d", name, s.stepCount)
	}

	s.engine.log().InfoContext(ctx, "childWorkflowWithVersion",
		"name", name, "version", version, "child_version", childVersion,
		"parent_id", parentID, "is_replay", s.isReplay, "step_count", s.stepCount,
		"child_wf_store_nil", s.engine.childWfStore == nil)

	if s.engine.childWfStore != nil {
		// Check child workflow quota before creating the child.
		if s.engine.maxQuotaChildren > 0 && s.engine.workflowStore != nil {
			count, err := s.engine.workflowStore.GetChildCount(context.Background(), s.workflowID)
			if err != nil {
				errMsg := fmt.Sprintf("workflow %s: failed to check child quota: %v", s.workflowID, err)
				s.engine.log().ErrorContext(ctx, errMsg, "workflow_id", s.workflowID, "tenant_id", s.tenantID)
				errWritten, _ := s.writeResult(ctx, m, runIDPtr, errMsg, runIDMaxLen)
				return int64(uint64(errWritten)<<32 | 4)
			}
			if count >= s.engine.maxQuotaChildren {
				errMsg := fmt.Sprintf("workflow %s: child workflow quota exceeded (current %d, max %d)", s.workflowID, count, s.engine.maxQuotaChildren)
				s.engine.log().ErrorContext(ctx, errMsg, "workflow_id", s.workflowID, "tenant_id", s.tenantID)
				errWritten, _ := s.writeResult(ctx, m, runIDPtr, errMsg, runIDMaxLen)
				return int64(uint64(errWritten)<<32 | 4)
			}
		}

		// Build the event record before the store call so the store can
		// INSERT it atomically with the child row.
		rec := EventRecord{
			Step:              s.stepCount,
			EventType:         EventTypeChildWorkflow,
			ChildName:         name,
			ChildInput:        inputJSON,
			ParentWorkflowID:  s.workflowID,
			ParentClosePolicy: parentClosePolicy,
			TimestampMs:       time.Now().UnixMilli(),
		}

		var err error
		if ts != "" {
			css, ok := s.engine.childWfStore.(CrossSchemaChildStore)
			if !ok {
				// Cross-schema requested but store doesn't support it.
				// Fail loudly rather than silently creating the child in the wrong schema.
				err := fmt.Errorf("child workflow %q: cross-schema requested (target=%q) but store does not implement CrossSchemaChildStore", name, ts)

				errWritten, _ := s.writeResult(ctx, m, runIDPtr, err.Error(), runIDMaxLen)
				return int64(uint64(errWritten)<<32 | 4) // error code 4 = invalid
			}
			runID, err = css.StartChildWorkflowInSchema(context.Background(), ts, parentID, name, inputJSON, childVersion, parentClosePolicy, priority)
		} else {
			s.engine.log().InfoContext(ctx, "calling StartChildWorkflowAtomic",
				"name", name, "parent_id", parentID, "child_version", childVersion)
			runID, err = s.engine.childWfStore.StartChildWorkflowAtomic(context.Background(), "", parentID, name, inputJSON, childVersion, parentClosePolicy, rec, priority)
		}
		if err != nil {
			s.engine.log().ErrorContext(ctx, "StartChildWorkflowAtomic failed",
				"error", err, "name", name, "parent_id", parentID, "child_version", childVersion)
			errMsg := fmt.Sprintf("child workflow %q: start failed: %v", name, err)
			s.engine.log().ErrorContext(ctx, errMsg, "workflow_id", s.workflowID, "tenant_id", s.tenantID)
			errWritten, _ := s.writeResult(ctx, m, runIDPtr, errMsg, runIDMaxLen)
			return int64(uint64(errWritten)<<32 | 3) // errCode 3 = not_found
		}
		// Append event to in-memory history for same-execution replay.
		// The store already wrote it to event_history atomically;
		// the later flush will skip it via ON CONFLICT DO NOTHING.
		rec.RunID = runID
		s.history = append(s.history, rec)
		s.nowMs = rec.TimestampMs
		s.stepCount++
	} else {
		runID = fmt.Sprintf("child-%s-%d", name, s.stepCount)
		rec := EventRecord{
			Step:              s.stepCount,
			EventType:         EventTypeChildWorkflow,
			ChildName:         name,
			ChildInput:        inputJSON,
			RunID:             runID,
			ParentWorkflowID:  s.workflowID,
			ParentClosePolicy: parentClosePolicy,
		}
		s.recordEvent(rec)
	}

	written, _ := s.writeResult(ctx, m, runIDPtr, runID, runIDMaxLen)
	return int64(uint64(written)<<32 | 0)
}

func (s *execSession) AwaitChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeAwaitChild {
				if rec.Response != "" || rec.Err != "" {
					// Cached result available — return it.
					if !s.advanceReplayStep(ctx, &rec) {
						return 0
					}
					if rec.Err != "" {
						written, _ := s.writeResult(ctx, m, resultPtr, rec.Err, resultMaxLen)
						return packAwaitChildResult(uint32(written), 1)
					}
					written, _ := s.writeResult(ctx, m, resultPtr, rec.Response, resultMaxLen)
					return packAwaitChildResult(uint32(written), 0)
				}
				s.engine.log().InfoContext(ctx, "await_child: no cached result, exitReplay to fresh", "workflow_id", s.workflowID, "runID", runID, "step", rec.Step)
				// No cached result yet — fall through to fresh to re-check.
				// Don't advance stepCount; the fresh execution will record
				// the result at this same step, overwriting the empty event.
				s.exitReplay()
			} else {
				// Event type mismatch — replay divergence.
				if s.engine.Metrics != nil {
					s.engine.Metrics.RecordReplayFailure(ctx)
				}
				errMsg := fmt.Sprintf("replay divergence at step %d: expected await_child, got %s.\n  run ID: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
					rec.Step, rec.EventType, runID)
				written, _ := s.writeResult(ctx, m, resultPtr, errMsg, resultMaxLen)
				return packAwaitChildResult(uint32(written), 1)
			}
		} else {
			s.exitReplay()
		}
	}

	// Fresh execution: check child result via store.
	if s.engine.childWfStore != nil {
		result, completed, err := s.engine.childWfStore.GetChildResult(context.Background(), runID)
		if completed && err == nil {
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeAwaitChild,
				RunID:     runID,
				Response:  result,
			}
			s.recordEvent(rec)

			written, _ := s.writeResult(ctx, m, resultPtr, result, resultMaxLen)
			return packAwaitChildResult(uint32(written), 0)
		}
		if err != nil {
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeAwaitChild,
				RunID:     runID,
				Err:       err.Error(),
			}
			s.recordEvent(rec)

			written, _ := s.writeResult(ctx, m, resultPtr, err.Error(), resultMaxLen)
			return packAwaitChildResult(uint32(written), 1)
		}
	}

	// Child not completed — record event and suspend.
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeAwaitChild,
		RunID:     runID,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("await_child(%s)", runID),
	}

	return packAwaitChildResultSuspend()
}

func (s *execSession) PollChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {
	// Non-blocking check of a child's status. Never suspends.
	// Returns: {"status":"running|completed|failed", "result":"...", "error":"..."}

	type pollResult struct {
		Status string `json:"status"`
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}

	var pr pollResult
	if s.engine.childWfStore != nil {
		result, completed, err := s.engine.childWfStore.GetChildResult(context.Background(), runID)
		if err != nil {
			pr = pollResult{Status: "failed", Error: err.Error()}
		} else if completed {
			if result != "" {
				pr = pollResult{Status: "completed", Result: result}
			} else {
				pr = pollResult{Status: "failed", Error: "child workflow failed (empty result)"}
			}
		} else {
			pr = pollResult{Status: "running"}
		}
	} else {
		pr = pollResult{Status: "failed", Error: "no child workflow store"}
	}

	out, _ := json.Marshal(pr)
	written, _ := s.writeResult(ctx, m, resultPtr, string(out), resultMaxLen)
	return int64(uint64(written)<<32 | 0)
}

func (s *execSession) AwaitAnyChild(ctx context.Context, m api.Module, runIDsJSON string, resultPtr, resultMaxLen uint32) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeAwaitAnyChild {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				if rec.Response != "" {
					written, _ := s.writeResult(ctx, m, resultPtr, rec.Response, resultMaxLen)
					return int64(uint64(written)<<32 | 0)
				}
				// Empty response: this was a suspend (no child was done yet).
				// Peek at the next event — if it is also an AwaitAnyChild with
				// a non-empty response, that is the re-execution result. Consume
				// both events and return the cached result. This avoids a
				// non-deterministic fall-through to fresh where multiple children
				// might show as completed on replay.
				if s.stepCount < len(s.history) {
					nextRec := s.history[s.stepCount]
					if nextRec.EventType == EventTypeAwaitAnyChild && nextRec.Response != "" {
						if !s.advanceReplayStep(ctx, &nextRec) {
							return 0
						}
						written, _ := s.writeResult(ctx, m, resultPtr, nextRec.Response, resultMaxLen)
						return int64(uint64(written)<<32 | 0)
					}
				}
				// No cached re-execution result — fall through to fresh.
				s.exitReplay()
			} else {
				// Event type mismatch — replay divergence.
				if s.engine.Metrics != nil {
					s.engine.Metrics.RecordReplayFailure(ctx)
				}
				errMsg := fmt.Sprintf("replay divergence at step %d: expected await_any_child, got %s", rec.Step, rec.EventType)
				written, _ := s.writeResult(ctx, m, resultPtr, errMsg, resultMaxLen)
				return int64(uint64(written)<<32 | 1)
			}
		} else {
			s.exitReplay()
		}
	}

	// Fresh execution: poll children in deterministic order and return the
	// first completed one. Sorted order guarantees that replay after a suspend
	// produces the same result as the original execution when multiple children
	// happen to be complete.
	var runIDs []string
	if err := json.Unmarshal([]byte(runIDsJSON), &runIDs); err != nil {
		written, _ := s.writeResult(ctx, m, resultPtr, fmt.Sprintf(`{"error":"invalid runIDs: %v"}`, err), resultMaxLen)
		return int64(uint64(written)<<32 | 1)
	}

	// Sort for deterministic polling order.
	sort.Strings(runIDs)

	type outcome struct {
		RunID  string `json:"run_id"`
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}

	if s.engine.childWfStore != nil {
		for _, rid := range runIDs {
			result, completed, err := s.engine.childWfStore.GetChildResult(context.Background(), rid)
			if err != nil || completed {
				var out outcome
				out.RunID = rid
				if err != nil {
					out.Error = err.Error()
				} else {
					out.Result = result
				}
				outJSON, _ := json.Marshal(out)
				rec := EventRecord{
					Step:      s.stepCount,
					EventType: EventTypeAwaitAnyChild,
					Request:   runIDsJSON,
					Response:  string(outJSON),
				}
				s.recordEvent(rec)
				written, _ := s.writeResult(ctx, m, resultPtr, string(outJSON), resultMaxLen)
				return int64(uint64(written)<<32 | 0)
			}
		}
	}

	// No child completed — suspend.
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeAwaitAnyChild,
		Request:   runIDsJSON,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("await_any_child(%s)", runIDsJSON),
	}

	return packAwaitChildResultSuspend()
}

func (s *execSession) AwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayAwaitAllChildren(ctx, m, runIDsJSON, resultsPtr, resultsMaxLen)
	}
	return s.freshAwaitAllChildren(ctx, m, runIDsJSON, resultsPtr, resultsMaxLen)
}

func (s *execSession) freshAwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64 {

	var runIDs []string
	if err := json.Unmarshal([]byte(runIDsJSON), &runIDs); err != nil {
		written, _ := s.writeResult(ctx, m, resultsPtr, fmt.Sprintf(`[{"error":"invalid runIDs: %v"}]`, err), resultsMaxLen)
		return packAwaitChildResult(uint32(written), 1)
	}

	// Concurrently await all children.
	type childOutcome struct {
		RunID  string `json:"run_id"`
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}

	outcomes := make([]childOutcome, len(runIDs))
	var wg sync.WaitGroup

	for i, runID := range runIDs {
		wg.Add(1)
		go func(idx int, rid string) {
			defer wg.Done()
			if s.engine.childWfStore != nil {
				result, completed, err := s.engine.childWfStore.GetChildResult(context.Background(), rid)
				if err != nil {
					outcomes[idx] = childOutcome{RunID: rid, Error: err.Error()}
				} else if completed {
					outcomes[idx] = childOutcome{RunID: rid, Result: result}
				} else {
					outcomes[idx] = childOutcome{RunID: rid, Error: "child not completed"}
				}
			} else {
				outcomes[idx] = childOutcome{RunID: rid, Error: "no child workflow store"}
			}
		}(i, runID)
	}
	wg.Wait()

	// Record event.
	outcomesJSON, _ := json.Marshal(outcomes)
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeAwaitAllChildren,
		Request:   runIDsJSON,
		Response:  string(outcomesJSON),
	}
	s.recordEvent(rec)

	written, _ := s.writeResult(ctx, m, resultsPtr, string(outcomesJSON), resultsMaxLen)
	return packAwaitChildResult(uint32(written), 0)
}

func (s *execSession) replayAwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64 {

	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) {
			return 0
		}

		if rec.EventType != EventTypeAwaitAllChildren {
			if s.engine.Metrics != nil {
				s.engine.Metrics.RecordReplayFailure(ctx)
			}
			errMsg := fmt.Sprintf("replay divergence at step %d: expected await_all_children, got %s.\n  actual run IDs: %s\n  expected run IDs: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, rec.EventType,
				truncateWithHash(runIDsJSON, maxPayloadLen),
				truncateWithHash(rec.Request, maxPayloadLen))
			written, _ := s.writeResult(ctx, m, resultsPtr, errMsg, resultsMaxLen)
			return packAwaitChildResult(uint32(written), 1)
		}

		if runIDsJSON != rec.Request {
			if s.engine.Metrics != nil {
				s.engine.Metrics.RecordReplayFailure(ctx)
			}
			errMsg := fmt.Sprintf("replay divergence at step %d: await_all_children run IDs mismatch.\n  actual run IDs: %s\n  expected run IDs: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step,
				truncateWithHash(runIDsJSON, maxPayloadLen),
				truncateWithHash(rec.Request, maxPayloadLen))
			written, _ := s.writeResult(ctx, m, resultsPtr, errMsg, resultsMaxLen)
			return packAwaitChildResult(uint32(written), 1)
		}

		written, _ := s.writeResult(ctx, m, resultsPtr, rec.Response, resultsMaxLen)
		return packAwaitChildResult(uint32(written), 0)
	}

	s.exitReplay()
	return s.freshAwaitAllChildren(ctx, m, runIDsJSON, resultsPtr, resultsMaxLen)
}

func (s *execSession) RunDetached(ctx context.Context, m api.Module, name, inputJSON string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType != EventTypeRunDetached || rec.DetachedName != name {
				return 1
			}
			return 0
		}
		s.exitReplay()
	}

	// Resolve child version using the same policy logic as childWorkflowWithVersion.
	childVersion := s.resolveChildVersion(ctx, name, 0, "")

	var runID string
	if s.engine.childWfStore != nil {
		rid, err := s.engine.childWfStore.StartChildWorkflow(ctx, s.workflowID, name, inputJSON, childVersion, "", 0)
		if err == nil {
			runID = rid
		}
	}
	if runID == "" {
		runID = fmt.Sprintf("detached-%s-%d", name, s.stepCount)
	}

	rec := EventRecord{
		Step:          s.stepCount,
		EventType:     EventTypeRunDetached,
		DetachedName:  name,
		DetachedInput: inputJSON,
		DetachedRunID: runID,
	}
	s.recordEvent(rec)
	return 0
}
