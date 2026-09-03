package engine

// Event is the interface for all typed workflow events. Each concrete event
// type carries only the fields relevant to that event, eliminating the need
// to consult a 30-field god struct.
type Event interface {
	Step() int
	Type() EventType
}

// ---- Call event ----

// CallEvent records a durable service call (the most common event type).
type CallEvent struct {
	step     int
	Service  string
	Op       string
	Request  string
	Response string
	Err      string
}

func (e CallEvent) Step() int       { return e.step }
func (e CallEvent) Type() EventType { return EventTypeCall }

// ---- AwaitSignals event ----

// AwaitSignalsEvent records the start of a signal-waiting period.
type AwaitSignalsEvent struct {
	step        int
	SignalNames string
	TimeoutMs   int64
}

func (e AwaitSignalsEvent) Step() int       { return e.step }
func (e AwaitSignalsEvent) Type() EventType { return EventTypeAwaitSignals }

// ---- SignalReceived event ----

// SignalReceivedEvent records that a signal was delivered to the workflow.
type SignalReceivedEvent struct {
	step          int
	SignalName    string
	SignalPayload string
}

func (e SignalReceivedEvent) Step() int       { return e.step }
func (e SignalReceivedEvent) Type() EventType { return EventTypeSignalReceived }

// ---- Defer event ----

// DeferEvent records a registered defer callback.
type DeferEvent struct {
	step        int
	Description string
	DeferID     string
}

func (e DeferEvent) Step() int       { return e.step }
func (e DeferEvent) Type() EventType { return EventTypeDefer }

// ---- ChildWorkflow event ----

// ChildWorkflowEvent records the start of a child workflow.
type ChildWorkflowEvent struct {
	step             int
	DefName          string
	Input            string
	ChildID          string
	ParentWorkflowID string
}

func (e ChildWorkflowEvent) Step() int       { return e.step }
func (e ChildWorkflowEvent) Type() EventType { return EventTypeChildWorkflow }

// ---- AwaitChild event ----

// AwaitChildEvent records the result of awaiting a single child workflow.
type AwaitChildEvent struct {
	step     int
	RunID    string
	Response string
	Err      string
}

func (e AwaitChildEvent) Step() int       { return e.step }
func (e AwaitChildEvent) Type() EventType { return EventTypeAwaitChild }

// ---- AwaitAllChildren event ----

// AwaitAllChildrenEvent records the result of awaiting all child workflows.
type AwaitAllChildrenEvent struct {
	step         int
	RunIDsJSON   string
	OutcomesJSON string
}

func (e AwaitAllChildrenEvent) Step() int       { return e.step }
func (e AwaitAllChildrenEvent) Type() EventType { return EventTypeAwaitAllChildren }

// ---- ContinueAsNew event ----

// ContinueAsNewEvent records a continue-as-new instruction.
type ContinueAsNewEvent struct {
	step     int
	NewInput string
}

func (e ContinueAsNewEvent) Step() int       { return e.step }
func (e ContinueAsNewEvent) Type() EventType { return EventTypeContinueAsNew }

// ---- Heartbeat event ----

// HeartbeatEvent records a heartbeat tick during a long-running call.
type HeartbeatEvent struct {
	step int
}

func (e HeartbeatEvent) Step() int       { return e.step }
func (e HeartbeatEvent) Type() EventType { return EventTypeHeartbeat }

// ---- PluginCall event ----

// PluginCallEvent records a plugin host function invocation.
type PluginCallEvent struct {
	step       int
	PluginName string
	FuncName   string
	Input      string
	Output     string
	Err        string
	Idempotent bool
}

func (e PluginCallEvent) Step() int       { return e.step }
func (e PluginCallEvent) Type() EventType { return EventTypePluginCall }

// ---- PluginCallStreamChunk event ----

// PluginCallStreamChunkEvent records one chunk from a streaming plugin call.
type PluginCallStreamChunkEvent struct {
	step       int
	PluginName string
	FuncName   string
	Input      string
	Output     string
	ChunkIndex int
	// ErrCode is the call error code the guest was told when this chunk
	// records a stream-level failure; zero on an ordinary chunk. See
	// EventRecord.StreamErrCode.
	ErrCode int
	Finish  bool
}

func (e PluginCallStreamChunkEvent) Step() int       { return e.step }
func (e PluginCallStreamChunkEvent) Type() EventType { return EventTypePluginCallStreamChunk }

// ---- CreatePromise event ----

// CreatePromiseEvent records the creation of a durable promise.
type CreatePromiseEvent struct {
	step        int
	PromiseName string
	PromiseID   string
}

func (e CreatePromiseEvent) Step() int       { return e.step }
func (e CreatePromiseEvent) Type() EventType { return EventTypeCreatePromise }

// ---- AwaitPromise event ----

// AwaitPromiseEvent records that a workflow began awaiting a promise.
type AwaitPromiseEvent struct {
	step      int
	PromiseID string
}

func (e AwaitPromiseEvent) Step() int       { return e.step }
func (e AwaitPromiseEvent) Type() EventType { return EventTypeAwaitPromise }

// ---- PromiseResolved event ----

// PromiseResolvedEvent records that a promise resolved successfully.
type PromiseResolvedEvent struct {
	step      int
	PromiseID string
	Result    string
}

func (e PromiseResolvedEvent) Step() int       { return e.step }
func (e PromiseResolvedEvent) Type() EventType { return EventTypePromiseResolved }

// ---- PromiseRejected event ----

// PromiseRejectedEvent records that a promise was rejected.
type PromiseRejectedEvent struct {
	step      int
	PromiseID string
	Err       string
}

func (e PromiseRejectedEvent) Step() int       { return e.step }
func (e PromiseRejectedEvent) Type() EventType { return EventTypePromiseRejected }

// ---- UpdateHandler event ----

// UpdateHandlerEvent records an update handler registration.
type UpdateHandlerEvent struct {
	step        int
	HandlerName string
}

func (e UpdateHandlerEvent) Step() int       { return e.step }
func (e UpdateHandlerEvent) Type() EventType { return EventTypeUpdateHandler }

// ---- StateMutation event ----

// StateMutationEvent records a state mutation operation.
type StateMutationEvent struct {
	step  int
	Key   string
	Value string
	Delta int64
	Op    string
}

func (e StateMutationEvent) Step() int       { return e.step }
func (e StateMutationEvent) Type() EventType { return EventTypeStateMutation }

// ---- RunDetached event ----

// RunDetachedEvent records a run-detached operation.
type RunDetachedEvent struct {
	step int
}

func (e RunDetachedEvent) Step() int       { return e.step }
func (e RunDetachedEvent) Type() EventType { return EventTypeRunDetached }

// ---- AdminAction event ----

// AdminActionEvent records an administrative action performed on a workflow.
type AdminActionEvent struct {
	step     int
	Action   string // "force_complete", "force_fail", "re_replay"
	Operator string // identity from auth context
	Reason   string // optional detail
}

func (e AdminActionEvent) Step() int       { return e.step }
func (e AdminActionEvent) Type() EventType { return EventTypeAdminAction }

// ---------------------------------------------------------------------------
// Conversion functions between the typed Event hierarchy and the flat
// EventRecord struct used at the database boundary.
// ---------------------------------------------------------------------------

// EventRecordFromEvent converts a typed Event to a flat EventRecord suitable
// for database persistence.
func EventRecordFromEvent(e Event) EventRecord {
	switch ev := e.(type) {
	case CallEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeCall,
			Service: ev.Service, Op: ev.Op,
			Request: ev.Request, Response: ev.Response, Err: ev.Err,
		}
	case AwaitSignalsEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeAwaitSignals,
			SignalNames: ev.SignalNames, TimeoutMs: ev.TimeoutMs,
		}
	case SignalReceivedEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeSignalReceived,
			SignalName: ev.SignalName, SignalPayload: ev.SignalPayload,
		}
	case DeferEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeDefer,
			DeferDescription: ev.Description, DeferID: ev.DeferID,
		}
	case ChildWorkflowEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeChildWorkflow,
			ChildName: ev.DefName, ChildInput: ev.Input,
			RunID: ev.ChildID, ParentWorkflowID: ev.ParentWorkflowID,
		}
	case AwaitChildEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeAwaitChild,
			RunID: ev.RunID, Response: ev.Response, Err: ev.Err,
		}
	case AwaitAllChildrenEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeAwaitAllChildren,
			Request: ev.RunIDsJSON, Response: ev.OutcomesJSON,
		}
	case ContinueAsNewEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeContinueAsNew,
			NewInput: ev.NewInput,
		}
	case HeartbeatEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeHeartbeat,
		}
	case PluginCallEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypePluginCall,
			PluginName: ev.PluginName, PluginFunc: ev.FuncName,
			PluginInput: ev.Input, PluginOutput: ev.Output,
			PluginError: ev.Err, Idempotent: ev.Idempotent,
		}
	case PluginCallStreamChunkEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypePluginCallStreamChunk,
			PluginName: ev.PluginName, PluginFunc: ev.FuncName,
			PluginInput: ev.Input, PluginOutput: ev.Output,
			StreamChunkIndex: ev.ChunkIndex, StreamFinish: ev.Finish,
			StreamErrCode: ev.ErrCode,
		}
	case CreatePromiseEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeCreatePromise,
			PromiseName: ev.PromiseName, PromiseID: ev.PromiseID,
		}
	case AwaitPromiseEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeAwaitPromise,
			PromiseID: ev.PromiseID,
		}
	case PromiseResolvedEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypePromiseResolved,
			PromiseID: ev.PromiseID, PromiseResult: ev.Result,
		}
	case PromiseRejectedEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypePromiseRejected,
			PromiseID: ev.PromiseID, PromiseError: ev.Err,
		}
	case UpdateHandlerEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeUpdateHandler,
			UpdateHandlerName: ev.HandlerName,
		}
	case StateMutationEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeStateMutation,
			StateKey: ev.Key, StateValue: ev.Value,
			StateDelta: ev.Delta, StateOp: ev.Op,
		}
	case RunDetachedEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeRunDetached,
		}
	case AdminActionEvent:
		return EventRecord{
			Step: e.Step(), EventType: EventTypeAdminAction,
			Service: ev.Operator, Op: ev.Action, Err: ev.Reason,
		}
	default:
		return EventRecord{Step: e.Step(), EventType: e.Type()}
	}
}

// EventFromRecord converts a flat EventRecord to the appropriate typed Event.
// It returns nil for unrecognised event types.
func EventFromRecord(r EventRecord) Event {
	switch r.EventType {
	case EventTypeCall:
		return CallEvent{
			step: r.Step, Service: r.Service, Op: r.Op,
			Request: r.Request, Response: r.Response, Err: r.Err,
		}
	case EventTypeAwaitSignals:
		return AwaitSignalsEvent{
			step: r.Step, SignalNames: r.SignalNames, TimeoutMs: r.TimeoutMs,
		}
	case EventTypeSignalReceived:
		return SignalReceivedEvent{
			step: r.Step, SignalName: r.SignalName, SignalPayload: r.SignalPayload,
		}
	case EventTypeDefer:
		return DeferEvent{
			step: r.Step, Description: r.DeferDescription, DeferID: r.DeferID,
		}
	case EventTypeChildWorkflow:
		return ChildWorkflowEvent{
			step: r.Step, DefName: r.ChildName, Input: r.ChildInput,
			ChildID: r.RunID, ParentWorkflowID: r.ParentWorkflowID,
		}
	case EventTypeAwaitChild:
		return AwaitChildEvent{
			step: r.Step, RunID: r.RunID, Response: r.Response, Err: r.Err,
		}
	case EventTypeAwaitAllChildren:
		return AwaitAllChildrenEvent{
			step: r.Step, RunIDsJSON: r.Request, OutcomesJSON: r.Response,
		}
	case EventTypeContinueAsNew:
		return ContinueAsNewEvent{
			step: r.Step, NewInput: r.NewInput,
		}
	case EventTypeHeartbeat:
		return HeartbeatEvent{
			step: r.Step,
		}
	case EventTypePluginCall:
		return PluginCallEvent{
			step: r.Step, PluginName: r.PluginName, FuncName: r.PluginFunc,
			Input: r.PluginInput, Output: r.PluginOutput,
			Err: r.PluginError, Idempotent: r.Idempotent,
		}
	case EventTypePluginCallStreamChunk:
		return PluginCallStreamChunkEvent{
			step: r.Step, PluginName: r.PluginName, FuncName: r.PluginFunc,
			Input: r.PluginInput, Output: r.PluginOutput,
			ChunkIndex: r.StreamChunkIndex, Finish: r.StreamFinish,
			ErrCode: r.StreamErrCode,
		}
	case EventTypeCreatePromise:
		return CreatePromiseEvent{
			step: r.Step, PromiseName: r.PromiseName, PromiseID: r.PromiseID,
		}
	case EventTypeAwaitPromise:
		return AwaitPromiseEvent{
			step: r.Step, PromiseID: r.PromiseID,
		}
	case EventTypePromiseResolved:
		return PromiseResolvedEvent{
			step: r.Step, PromiseID: r.PromiseID, Result: r.PromiseResult,
		}
	case EventTypePromiseRejected:
		return PromiseRejectedEvent{
			step: r.Step, PromiseID: r.PromiseID, Err: r.PromiseError,
		}
	case EventTypeUpdateHandler:
		return UpdateHandlerEvent{
			step: r.Step, HandlerName: r.UpdateHandlerName,
		}
	case EventTypeStateMutation:
		return StateMutationEvent{
			step: r.Step, Key: r.StateKey, Value: r.StateValue,
			Delta: r.StateDelta, Op: r.StateOp,
		}
	case EventTypeRunDetached:
		return RunDetachedEvent{
			step: r.Step,
		}
	case EventTypeAdminAction:
		return AdminActionEvent{
			step: r.Step, Action: r.Op, Operator: r.Service, Reason: r.Err,
		}
	default:
		return nil
	}
}

// EventsFromRecords converts a slice of EventRecords to a slice of Events.
func EventsFromRecords(records []EventRecord) []Event {
	events := make([]Event, len(records))
	for i, r := range records {
		events[i] = EventFromRecord(r)
	}
	return events
}

// RecordsFromEvents converts a slice of Events to a slice of EventRecords.
func RecordsFromEvents(events []Event) []EventRecord {
	records := make([]EventRecord, len(events))
	for i, e := range events {
		records[i] = EventRecordFromEvent(e)
	}
	return records
}
