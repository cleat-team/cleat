package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/cleat-team/cleat/monitoring/prometheus"
)

// DefaultCompactionThreshold is the default number of events before history
// compaction triggers. A workflow with more than this many events is eligible.
const DefaultCompactionThreshold = 1000

// DefaultMaxCompactedEvents caps the number of compacted events stored in a
// single compaction state JSONB. Beyond this limit, the oldest events are
// truncated into a summary to keep the compaction state size bounded.
const DefaultMaxCompactedEvents = 10000

// Event type codes for compact JSONB storage. Short int codes minimize
// storage size when a workflow has thousands of compacted events.
const (
	EventCodeCall                  = 0
	EventCodeSleep                 = 1
	EventCodeAwaitSignals          = 2
	EventCodeSignalReceived        = 3
	EventCodeDefer                 = 4
	EventCodeChildWorkflow         = 5
	EventCodeAwaitChild            = 6
	EventCodeContinueAsNew         = 7
	EventCodeHeartbeat             = 8
	EventCodeAwaitAllChildren      = 9
	EventCodePluginCall            = 10
	EventCodeCreatePromise         = 11
	EventCodeAwaitPromise          = 12
	EventCodePromiseResolved       = 13
	EventCodePromiseRejected       = 14
	EventCodeUpdateHandler         = 15
	EventCodeStateMutation         = 16
	EventCodeRunDetached           = 17
	EventCodeAcquireLock           = 18
	EventCodeReleaseLock           = 19
	EventCodePluginCallStreamChunk = 20
	EventCodeSideEffect            = 21
	EventCodeScopeAcquired         = 22
	EventCodeFetch                 = 23
	EventCodeDurableLog            = 24
	EventCodeDurableSend           = 25
	EventCodeDurableScheduleInvoke = 26
)

// EventCodeSleep (1) is defined above but has no corresponding EventType
// — sleep events are handled locally in the engine and are never recorded
// to event history, so the code is reserved/unused.

var eventTypeToCode = map[EventType]int{
	EventTypeCall:                  EventCodeCall,
	EventTypeAwaitSignals:          EventCodeAwaitSignals,
	EventTypeSignalReceived:        EventCodeSignalReceived,
	EventTypeDefer:                 EventCodeDefer,
	EventTypeChildWorkflow:         EventCodeChildWorkflow,
	EventTypeAwaitChild:            EventCodeAwaitChild,
	EventTypeContinueAsNew:         EventCodeContinueAsNew,
	EventTypeHeartbeat:             EventCodeHeartbeat,
	EventTypeAwaitAllChildren:      EventCodeAwaitAllChildren,
	EventTypePluginCall:            EventCodePluginCall,
	EventTypeCreatePromise:         EventCodeCreatePromise,
	EventTypeAwaitPromise:          EventCodeAwaitPromise,
	EventTypePromiseResolved:       EventCodePromiseResolved,
	EventTypePromiseRejected:       EventCodePromiseRejected,
	EventTypeUpdateHandler:         EventCodeUpdateHandler,
	EventTypeStateMutation:         EventCodeStateMutation,
	EventTypeRunDetached:           EventCodeRunDetached,
	EventTypeAcquireLock:           EventCodeAcquireLock,
	EventTypeReleaseLock:           EventCodeReleaseLock,
	EventTypePluginCallStreamChunk: EventCodePluginCallStreamChunk,
	EventTypeSideEffect:            EventCodeSideEffect,
	EventTypeScopeAcquired:         EventCodeScopeAcquired,
	EventTypeFetch:                 EventCodeFetch,
	EventTypeDurableLog:            EventCodeDurableLog,
	EventTypeDurableSend:           EventCodeDurableSend,
	EventTypeDurableScheduleInvoke: EventCodeDurableScheduleInvoke,
}

var codeToEventType = map[int]EventType{
	EventCodeCall:                  EventTypeCall,
	EventCodeAwaitSignals:          EventTypeAwaitSignals,
	EventCodeSignalReceived:        EventTypeSignalReceived,
	EventCodeDefer:                 EventTypeDefer,
	EventCodeChildWorkflow:         EventTypeChildWorkflow,
	EventCodeAwaitChild:            EventTypeAwaitChild,
	EventCodeContinueAsNew:         EventTypeContinueAsNew,
	EventCodeHeartbeat:             EventTypeHeartbeat,
	EventCodeAwaitAllChildren:      EventTypeAwaitAllChildren,
	EventCodePluginCall:            EventTypePluginCall,
	EventCodeCreatePromise:         EventTypeCreatePromise,
	EventCodeAwaitPromise:          EventTypeAwaitPromise,
	EventCodePromiseResolved:       EventTypePromiseResolved,
	EventCodePromiseRejected:       EventTypePromiseRejected,
	EventCodeUpdateHandler:         EventTypeUpdateHandler,
	EventCodeStateMutation:         EventTypeStateMutation,
	EventCodeRunDetached:           EventTypeRunDetached,
	EventCodeAcquireLock:           EventTypeAcquireLock,
	EventCodeReleaseLock:           EventTypeReleaseLock,
	EventCodePluginCallStreamChunk: EventTypePluginCallStreamChunk,
	EventCodeSideEffect:            EventTypeSideEffect,
	EventCodeScopeAcquired:         EventTypeScopeAcquired,
	EventCodeFetch:                 EventTypeFetch,
	EventCodeDurableLog:            EventTypeDurableLog,
	EventCodeDurableSend:           EventTypeDurableSend,
	EventCodeDurableScheduleInvoke: EventTypeDurableScheduleInvoke,
}

// CompactionState holds the minimal state needed to reconstruct the compacted
// portion of a workflow's event history for deterministic replay.
type CompactionState struct {
	Version       int               `json:"version"`
	CompactedStep int               `json:"compacted_step"`
	Events        []CompactedEvent  `json:"events"`
	PendingDefers []CompactedDefer  `json:"pending_defers,omitempty"`
	OpenChildren  []CompactedChild  `json:"open_children,omitempty"`
	QueryState    map[string]string `json:"query_state,omitempty"`

	// Summary is populated when Events exceeds DefaultMaxCompactedEvents and is
	// truncated. It records the truncation count for observability.
	Summary *TruncationSummary `json:"summary,omitempty"`
}

// TruncationSummary records how many compacted events were truncated to keep
// the compaction state JSONB size bounded.
type TruncationSummary struct {
	TruncatedCount int `json:"truncated_count"`
}

// CompactedEvent is a sparse representation of a single event in the compacted
// portion of a workflow's history. Only fields relevant to the event type are
// populated; omitempty keeps JSONB storage compact. Short JSON tags minimize
// storage size for workflows with thousands of compacted events.
type CompactedEvent struct {
	Type          int    `json:"t"`
	Service       string `json:"svc,omitempty"`
	Op            string `json:"op,omitempty"`
	Request       string `json:"req,omitempty"`
	Response      string `json:"resp,omitempty"`
	Error         string `json:"err,omitempty"`
	DurationMs    int64  `json:"dur,omitempty"`
	SignalNames   string `json:"sigs,omitempty"`
	TimeoutMs     int64  `json:"to,omitempty"`
	SignalName    string `json:"sn,omitempty"`
	SignalPayload string `json:"sp,omitempty"`
	DeferID       string `json:"did,omitempty"`
	DeferDesc     string `json:"dd,omitempty"`
	ChildName     string `json:"cn,omitempty"`
	ChildInput    string `json:"ci,omitempty"`
	RunID         string `json:"rid,omitempty"`
	NewInput      string `json:"ni,omitempty"`
	PromiseName   string `json:"prom_name,omitempty"`
	PromiseID     string `json:"prom_id,omitempty"`
	PromiseResult string `json:"prom_res,omitempty"`
	PromiseError  string `json:"prom_err,omitempty"`

	// Plugin call fields.
	PluginName   string `json:"pn,omitempty"`
	PluginFunc   string `json:"pf,omitempty"`
	PluginInput  string `json:"pi,omitempty"`
	PluginOutput string `json:"po,omitempty"`
	PluginError  string `json:"pe,omitempty"`
}

// CompactedDefer represents a deferred cleanup callback registered in the
// compacted portion of history that has not yet been executed.
type CompactedDefer struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// CompactedChild represents a child workflow started in the compacted portion
// that is still running (not yet completed).
type CompactedChild struct {
	RunID string `json:"run_id"`
	Name  string `json:"name"`
	Input string `json:"input"`
}

// CompactWorkflowHistory compacts the event history for a workflow.
// It loads all events, extracts a compaction checkpoint from events before
// the compaction point, deletes those events from the database, and stores
// the compaction state on the workflow_instances row.
func CompactWorkflowHistory(ctx context.Context, store WorkflowStore, workflowID string, threshold int, metrics *prometheus.Metrics) error {
	events, err := store.LoadEventHistory(ctx, workflowID)
	if err != nil {
		return fmt.Errorf("compact: load events: %w", err)
	}
	if len(events) <= threshold {
		return nil // Not enough events to compact.
	}

	// Determine the compaction point: keep the most recent threshold/2 events
	// as the tail, compact everything before that.
	keepStep := len(events) - threshold/2
	if keepStep < 0 {
		keepStep = 0
	}
	compactedStep := keepStep

	// Extract compaction state from the events being compacted.
	compactedEvents := events[:keepStep]
	cs := extractCompactionState(compactedEvents)

	csJSON, err := json.Marshal(cs)
	if err != nil {
		return fmt.Errorf("compact: marshal state: %w", err)
	}

	// Retry compact history on deadlock errors (up to 3 attempts, exponential backoff).
	var compactErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			delay := time.Duration(100*(1<<(attempt-1))) * time.Millisecond
			select {
			case <-ctx.Done():
				return fmt.Errorf("compact: context cancelled during retry: %w", ctx.Err())
			case <-time.After(delay):
			}
		}
		compactErr = store.CompactHistory(ctx, workflowID, csJSON, compactedStep, keepStep)
		if compactErr == nil {
			break
		}
		if !isCompactionDeadlockError(compactErr) {
			break // Don't retry non-deadlock errors.
		}
	}
	if compactErr != nil {
		return fmt.Errorf("compact: store: %w", compactErr)
	}

	if metrics != nil {
		metrics.AddCompactionEventsDeleted(ctx, int64(compactedStep))
	}
	slog.InfoContext(ctx, "compact completed",
		"workflow_id", workflowID, "events", len(events), "compacted", compactedStep, "kept", len(events)-keepStep, "state_size", len(csJSON))
	return nil
}

// isCompactionDeadlockError returns true if the error is a database deadlock error.
// Recognizes PostgreSQL (40P01), MySQL (1213), and MSSQL (1205) deadlock codes.
func isCompactionDeadlockError(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "40P01" {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "1213") || // MySQL deadlock
		strings.Contains(msg, "deadlock") ||
		strings.Contains(msg, "Deadlock")
}

// extractCompactionState builds a CompactionState from the events being
// compacted away.
func extractCompactionState(events []EventRecord) *CompactionState {
	cs := &CompactionState{
		Version:       1,
		CompactedStep: len(events),
		Events:        make([]CompactedEvent, 0, len(events)),
		PendingDefers: make([]CompactedDefer, 0),
		OpenChildren:  make([]CompactedChild, 0),
	}

	defersSeen := make(map[string]string) // deferID -> description
	openChildren := make(map[string]bool) // runID -> still open

	for _, ev := range events {
		ce := CompactedEvent{Type: eventTypeToCode[ev.EventType]}
		switch ev.EventType {
		case EventTypeCall:
			ce.Service = ev.Service
			ce.Op = ev.Op
			ce.Request = ev.Request
			ce.Response = ev.Response
			ce.Error = ev.Err
			ce.DurationMs = ev.DurationMs
		case EventTypeAwaitSignals:
			ce.SignalNames = ev.SignalNames
			ce.TimeoutMs = ev.TimeoutMs
		case EventTypeSignalReceived:
			ce.SignalName = ev.SignalName
			ce.SignalPayload = ev.SignalPayload
		case EventTypeDefer:
			ce.DeferID = ev.DeferID
			ce.DeferDesc = ev.DeferDescription
			defersSeen[ev.DeferID] = ev.DeferDescription
		case EventTypeChildWorkflow:
			ce.ChildName = ev.ChildName
			ce.ChildInput = ev.ChildInput
			ce.RunID = ev.RunID
			openChildren[ev.RunID] = true
		case EventTypeAwaitChild:
			ce.RunID = ev.RunID
			ce.Response = ev.Response
			ce.Error = ev.Err
			if ev.Response != "" || ev.Err != "" {
				// Child completed — remove from open list.
				delete(openChildren, ev.RunID)
			}
		case EventTypeContinueAsNew:
			ce.NewInput = ev.NewInput
		case EventTypeHeartbeat:
			ce.Service = ev.Service
			ce.Op = ev.Op
		case EventTypeAwaitAllChildren:
			ce.Response = ev.Response
			// All children resolved.
			openChildren = make(map[string]bool)
		case EventTypePluginCall:
			ce.PluginName = ev.PluginName
			ce.PluginFunc = ev.PluginFunc
			ce.PluginInput = ev.PluginInput
			ce.PluginOutput = ev.PluginOutput
			ce.PluginError = ev.PluginError
		case EventTypeCreatePromise:
			ce.PromiseName = ev.PromiseName
			ce.PromiseID = ev.PromiseID
		case EventTypeAwaitPromise:
			ce.PromiseID = ev.PromiseID
		case EventTypePromiseResolved:
			ce.PromiseID = ev.PromiseID
			ce.PromiseResult = ev.PromiseResult
		case EventTypePromiseRejected:
			ce.PromiseID = ev.PromiseID
			ce.PromiseError = ev.PromiseError
		case EventTypeUpdateHandler:
			// Reuse PromiseName/PromiseID fields for storage efficiency.
			ce.PromiseName = ev.UpdateHandlerName
		case EventTypeStateMutation:
			ce.ChildName = ev.StateKey
			ce.Response = ev.StateValue
			ce.DurationMs = ev.StateDelta
			ce.PromiseName = ev.StateOp
		case EventTypeRunDetached:
			ce.ChildName = ev.DetachedName
			ce.ChildInput = ev.DetachedInput
			ce.RunID = ev.DetachedRunID
		case EventTypeDurableLog:
			ce.Response = ev.Message
			ce.Service = ev.LogLevel
			ce.Op = ev.LogKV
		case EventTypeFetch:
			ce.Service = ev.FetchMethod
			ce.Op = ev.FetchURL
			ce.Request = ev.FetchHeaders
			ce.ChildInput = ev.FetchBody
			ce.Response = ev.FetchResponse
			ce.Error = ev.Err
		case EventTypeAcquireLock:
			ce.ChildName = ev.LockKey
			ce.DurationMs = ev.LockTTLMs
			ce.Response = fmt.Sprintf("%d", ev.LockAcquired)
		case EventTypeReleaseLock:
			ce.ChildName = ev.LockKey
		case EventTypeSideEffect:
			ce.Response = ev.SideEffectResult
		case EventTypeScopeAcquired:
			ce.ChildName = ev.ScopeKey
		case EventTypePluginCallStreamChunk:
			ce.PluginName = ev.PluginName
			ce.PluginFunc = ev.PluginFunc
			ce.PluginInput = ev.PluginInput
			ce.PluginOutput = ev.PluginOutput
			ce.PluginError = ev.PluginError
		case EventTypeDurableSend:
			ce.Service = ev.Service
			ce.Op = ev.Op
			ce.Request = ev.Request
		case EventTypeDurableScheduleInvoke:
			ce.Service = ev.Service
			ce.Op = ev.Op
			ce.Request = ev.Request
			ce.DurationMs = ev.DurationMs
		}
		cs.Events = append(cs.Events, ce)
	}

	// Build list of pending defers (defers that were registered but not yet
	// executed in the compacted portion).
	for id, desc := range defersSeen {
		cs.PendingDefers = append(cs.PendingDefers, CompactedDefer{
			ID: id, Description: desc,
		})
	}

	// Build list of still-open children with their details.
	for runID := range openChildren {
		for _, ev := range events {
			if ev.EventType == EventTypeChildWorkflow && ev.RunID == runID {
				cs.OpenChildren = append(cs.OpenChildren, CompactedChild{
					RunID: runID,
					Name:  ev.ChildName,
					Input: ev.ChildInput,
				})
				break
			}
		}
	}

	// Cap the compacted events list to prevent unbounded JSONB growth.
	if len(cs.Events) > DefaultMaxCompactedEvents {
		truncated := len(cs.Events) - DefaultMaxCompactedEvents
		cs.Events = cs.Events[truncated:]
		cs.CompactedStep -= truncated
		cs.Summary = &TruncationSummary{TruncatedCount: truncated}
	}

	return cs
}

// buildFullHistoryFromCompaction reconstructs the full event history by
// prepending virtual events reconstructed from the compaction state to the
// tail events loaded from the database. This gives the engine a complete,
// contiguous history for deterministic replay.
func buildFullHistoryFromCompaction(tail []EventRecord, cs *CompactionState) []EventRecord {
	totalLen := len(cs.Events) + len(tail)
	full := make([]EventRecord, 0, totalLen)

	for i, ce := range cs.Events {
		rec := EventRecord{
			Step:      i,
			EventType: codeToEventType[ce.Type],
		}
		switch ce.Type {
		case EventCodeCall:
			rec.Service = ce.Service
			rec.Op = ce.Op
			rec.Request = ce.Request
			rec.Response = ce.Response
			rec.Err = ce.Error
			rec.DurationMs = ce.DurationMs
		case EventCodeAwaitSignals:
			rec.SignalNames = ce.SignalNames
			rec.TimeoutMs = ce.TimeoutMs
		case EventCodeSignalReceived:
			rec.SignalName = ce.SignalName
			rec.SignalPayload = ce.SignalPayload
		case EventCodeDefer:
			rec.DeferID = ce.DeferID
			rec.DeferDescription = ce.DeferDesc
		case EventCodeChildWorkflow:
			rec.ChildName = ce.ChildName
			rec.ChildInput = ce.ChildInput
			rec.RunID = ce.RunID
		case EventCodeAwaitChild:
			rec.Response = ce.Response
			rec.Err = ce.Error
			rec.RunID = ce.RunID
		case EventCodeContinueAsNew:
			rec.NewInput = ce.NewInput
		case EventCodeHeartbeat:
			rec.Service = ce.Service
			rec.Op = ce.Op
		case EventCodeAwaitAllChildren:
			rec.Response = ce.Response
		case EventCodePluginCall:
			rec.PluginName = ce.PluginName
			rec.PluginFunc = ce.PluginFunc
			rec.PluginInput = ce.PluginInput
			rec.PluginOutput = ce.PluginOutput
			rec.PluginError = ce.PluginError
		case EventCodeCreatePromise:
			rec.PromiseName = ce.PromiseName
			rec.PromiseID = ce.PromiseID
		case EventCodeAwaitPromise:
			rec.PromiseID = ce.PromiseID
		case EventCodePromiseResolved:
			rec.PromiseID = ce.PromiseID
			rec.PromiseResult = ce.PromiseResult
		case EventCodePromiseRejected:
			rec.PromiseID = ce.PromiseID
			rec.PromiseError = ce.PromiseError
		case EventCodeUpdateHandler:
			rec.UpdateHandlerName = ce.PromiseName
		case EventCodeStateMutation:
			rec.StateKey = ce.ChildName
			rec.StateValue = ce.Response
			rec.StateDelta = ce.DurationMs
			rec.StateOp = ce.PromiseName
		case EventCodeRunDetached:
			rec.DetachedName = ce.ChildName
			rec.DetachedInput = ce.ChildInput
			rec.DetachedRunID = ce.RunID
		case EventCodeDurableLog:
			rec.Message = ce.Response
			rec.LogLevel = ce.Service
			rec.LogKV = ce.Op
		case EventCodeFetch:
			rec.FetchMethod = ce.Service
			rec.FetchURL = ce.Op
			rec.FetchHeaders = ce.Request
			rec.FetchBody = ce.ChildInput
			rec.FetchResponse = ce.Response
			rec.Err = ce.Error
		case EventCodeAcquireLock:
			rec.LockKey = ce.ChildName
			rec.LockTTLMs = ce.DurationMs
			_, _ = fmt.Sscanf(ce.Response, "%d", &rec.LockAcquired)
		case EventCodeReleaseLock:
			rec.LockKey = ce.ChildName
		case EventCodeScopeAcquired:
			rec.ScopeKey = ce.ChildName
		case EventCodeSideEffect:
			rec.SideEffectResult = ce.Response
		case EventCodePluginCallStreamChunk:
			rec.PluginName = ce.PluginName
			rec.PluginFunc = ce.PluginFunc
			rec.PluginInput = ce.PluginInput
			rec.PluginOutput = ce.PluginOutput
			rec.PluginError = ce.PluginError
		}
		full = append(full, rec)
	}

	// Append tail events. Their Step fields already reflect their original
	// positions, which align with the reconstructed array indices.
	full = append(full, tail...)

	return full
}
