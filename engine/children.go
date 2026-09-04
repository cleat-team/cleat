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

// resolveChildVersion resolves the child workflow version by priority:
//  1. Explicit version from ChildWorkflowOptions (version > 0 from WASM ABI)
//  2. Runtime override (engine.childBindingOverride)
//  3. Binding policy from WASM metadata (engine.childBindingPolicy)
//  4. Fallback: 0 means DB resolves to MAX(version)
func (s *execSession) resolveChildVersion(ctx context.Context, name string, explicitVersion int) int {
	if explicitVersion > 0 {
		return explicitVersion
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
func (s *execSession) childWorkflowWithVersion(ctx context.Context, m api.Module, name, inputJSON string, version int, priority int, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeChildWorkflow {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}

				written, _ := s.writeResult(ctx, m, runIDPtr, rec.RunID, runIDMaxLen)
				return packSimpleResult(0, written)
			}
		}
		s.exitReplay()
	}

	// Past the frontier in a defer segment: starting a child workflow is new
	// work, and unlike a durable call it leaves a row behind that outlives the
	// segment. See IMPROVEMENT-PLAN 3.84.
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
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
	childVersion := s.resolveChildVersion(ctx, name, version)

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
		s.engine.log().InfoContext(ctx, "calling StartChildWorkflowAtomic",
			"name", name, "parent_id", parentID, "child_version", childVersion)
		runID, err = s.engine.childWfStore.StartChildWorkflowAtomic(context.Background(), "", parentID, name, inputJSON, childVersion, parentClosePolicy, rec, priority)
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
	return packSimpleResult(0, written)
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
						return packAwaitChildResult(written, 1)
					}
					written, _ := s.writeResult(ctx, m, resultPtr, rec.Response, resultMaxLen)
					return packAwaitChildResult(written, 0)
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
				return packAwaitChildResult(written, 1)
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
			return packAwaitChildResult(written, 0)
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
			return packAwaitChildResult(written, 1)
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
	return packSimpleResult(0, written)
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
					return packSimpleResult(0, written)
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
						return packSimpleResult(0, written)
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
				return packSimpleResult(0, written)
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
		return packAwaitChildResult(written, 1)
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
				// context.Background() rather than ctx, deliberately, and NOT
				// for the usual reason.
				//
				// The usual reason is lifetime -- a goroutine that outlives its
				// caller cannot borrow the caller's context. That is why
				// adaptive_flush.go:474 and scheduledbackup/routes.go:534 use
				// Background(), and it does NOT apply here: wg.Wait() below
				// joins every one of these, so they cannot outlive this call.
				// gosec's G118 flags this site for exactly that mismatch, and
				// on lifetime grounds it would be right.
				//
				// The reason is durability. Whatever these goroutines produce
				// is marshalled into the EventRecord recorded a few lines down,
				// and replayAwaitAllChildren hands `rec.Response` back to the
				// guest verbatim on every future replay. So cancelling these
				// queries does not abandon work -- it writes
				// "context canceled" into the workflow's permanent history and
				// replays it forever. A transient shutdown would become a
				// durable wrong answer, which is a strictly worse failure than
				// the one cancellation avoids.
				//
				// The cost is real and is accepted: because wg.Wait() joins,
				// an unreachable database makes this call block for as long as
				// the driver takes to give up, and a cancelled ctx will not cut
				// that short. A bounded context would cap the wait but bakes
				// the same wrong history on timeout, just later -- it moves the
				// defect rather than fixing it. Compare
				// cmd/cleat-worker/memory_controller.go:189, which DOES wrap
				// Background() in a timeout: nothing there is replayed, so
				// giving up early loses only a stats row.
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
	return packAwaitChildResult(written, 0)
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
			return packAwaitChildResult(written, 1)
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
			return packAwaitChildResult(written, 1)
		}

		written, _ := s.writeResult(ctx, m, resultsPtr, rec.Response, resultsMaxLen)
		return packAwaitChildResult(written, 0)
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

	// A detached run is a child workflow by another name: the line below calls
	// the SAME StartChildWorkflow that childWorkflowWithVersion calls, and
	// leaves the same claimable workflow_instances row behind. That one is
	// refused in a defer segment (3.84) and this one was not, so a terminated
	// workflow's cleanup pass could still create live work -- through the same
	// store method, two functions apart in this file. IMPROVEMENT-PLAN 3.111.
	//
	// After the replay return, because a refusal records no event and a replay
	// that reached this would find nothing where an event should be.
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}

	// Resolve child version using the same policy logic as childWorkflowWithVersion.
	childVersion := s.resolveChildVersion(ctx, name, 0)

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
