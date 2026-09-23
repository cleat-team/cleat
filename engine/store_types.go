package engine

import (
	"encoding/json"
	"time"
)

type WorkflowDef struct {
	Name       string            `json:"name"`
	Version    int               `json:"version"`
	WASMBytes  []byte            `json:"wasm_bytes,omitempty"`
	ABIVersion int               `json:"abi_version"`
	MinVersion int               `json:"min_version"`
	PluginDeps map[string]string `json:"plugin_deps,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`

	// DisabledAt carries ADMISSION CONTROL, and only that. A disabled version
	// cannot be started, cannot be routed to, cannot be pointed at by a tag,
	// and is not chosen for a child workflow. NULL means live.
	//
	// It does NOT make the version collectable. That is GCEligible, and the
	// separation is the point of cleat#1702: a generic entity helper reaching
	// for the contract's `disabled_at` must not arm a permanent deletion.
	DisabledAt *time.Time `json:"disabled_at,omitempty"`

	// GCEligible carries COLLECTION ELIGIBILITY, and only that.
	// engine/version_gc.go keys on this to decide what PurgeWorkflowDef may
	// delete permanently once it is older than --version-gc-max-age.
	//
	// THE EQUALITY OF THESE TWO FIELDS IS INCIDENTAL, NOT INVARIANT. Today
	// `cleatctl versions deprecate` writes both and is the only shipped path
	// that writes either, so every row in every deployment has them agreeing.
	// That is not a reason to merge them -- it is the state cleat#1702 calls
	// "bound but never executed", and the difference is pinned by
	// engine/gc_eligibility_is_not_retirement_test.go rather than by any
	// production row.
	GCEligible bool `json:"gc_eligible"`

	// MaxHistoryLength caps this definition's event history before compaction,
	// overriding the global threshold. 0 means "use the global", which is the
	// column default, so a deploy that does not set it changes nothing.
	//
	// It lives on the deploy payload rather than behind an admin endpoint
	// because the column is keyed (tenant_id, name, version): the value is
	// already a per-VERSION property, so setting it out of band would create a
	// value whose lifecycle does not match its key -- roll the code back and an
	// endpoint-set cap would stay, silently mis-tuning the version it was not
	// chosen for. See cleat#889.
	MaxHistoryLength int `json:"max_history_length,omitempty"`
}

// RetiredAt returns a pointer for DisabledAt. Paired with GCEligible: true it
// is the state `cleatctl versions deprecate` produces, and it exists because a
// struct literal cannot take the address of a time.Time.
func RetiredAt(t time.Time) *time.Time { return &t }

// Disabled reports whether admission control refuses this version.
//
// A predicate rather than exported pointer comparison at every call site: the
// question callers ask is "may this version be used", and DisabledAt != nil is
// easy to write as DisabledAt == nil by accident in a negated condition.
func (d WorkflowDef) Disabled() bool { return d.DisabledAt != nil }

// WorkflowInstance is a row from workflow_instances.
type WorkflowInstance struct {
	ID         string          `json:"id"`
	DefName    string          `json:"def_name"`
	DefVersion int             `json:"def_version"`
	MinVersion int             `json:"min_version"`
	Status     string          `json:"status"`
	Input      json.RawMessage `json:"input"`
	Result     string          `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	ErrorCode  string          `json:"error_code,omitempty"`
	ErrorOp    string          `json:"error_op,omitempty"`
	AssignedTo string          `json:"assigned_to"`

	// CompletedBy is the worker that took this run to a terminal status, or
	// empty if none did. cleat#1118.
	//
	// A PLAIN STRING, UNLIKE CompletedAt BELOW, and the difference is which
	// absence is ambiguous. CompletedAt is a pointer because a zero time.Time
	// serialises as a parsable timestamp -- every running workflow would claim
	// to have finished in the year 1. An empty string has no such second
	// reading, and `status` already distinguishes "not finished" from
	// "finished"; what this adds is WHICH worker, with empty meaning none is
	// recorded -- a run terminated before it was ever claimed, or one finished
	// before this column existed.
	//
	// It is NOT assigned_to. That field is a LEASE and every terminal write
	// clears it while fencing on it, so it is blank on every completed run by
	// construction -- measured 185 of 185 on a ports database. The two answer
	// different questions and conflating them is what produced cleat#1118.
	//
	// POPULATED ON BOTH READ PATHS: GetWorkflowByID and ListWorkflows, the same
	// rule ReclaimCount states below and for a related reason. Several
	// neighbouring columns are deliberately NOT in the listing -- completed_at
	// and started_at among them -- so carrying this one is a decision rather
	// than the default, and the argument is that the high-value question is
	// AGGREGATE. "Which worker is producing these failures" is a list scan, and
	// a listing without the column turns it into one detail read per row. The
	// size objection that keeps `result` and `cancellation_reason` out does not
	// reach a short identifier.
	//
	// engine/list_workflows_returns_every_field_test.go is what forced the
	// question, and it will force it again for the next column.
	CompletedBy string    `json:"completed_by,omitempty"`
	NextWakeAt  time.Time `json:"next_wake_at"`
	TenantID    string    `json:"tenant_id,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`

	// CompletedAt is when the run reached a terminal status, or nil while it
	// has not. A POINTER because its absence is the meaningful case: a plain
	// time.Time is not omitted by `omitempty`, so a running workflow would
	// serialise completed_at as "0001-01-01T00:00:00Z" -- a timestamp a client
	// can parse and compare, reporting every in-flight run as having finished
	// in the year 1. Nil is the only spelling of "not finished" that a JSON
	// consumer cannot mistake for a value.
	//
	// The column has existed since the first schema and every dialect's
	// GetWorkflowByID has always SELECTed it; all three scanned it into a local
	// and never assigned it, so no client could read when a run finished
	// (cleat#1091). CreatedAt above keeps its non-pointer shape: a row cannot
	// exist without one.
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	// CancellationRequested reports that someone asked this run to stop, and
	// CancellationReason says why, when a reason was given.
	//
	// COOPERATIVE CANCELLATION IS WHY THESE HAVE TO BE READABLE. The workflow
	// polls PollCancellation() and may legitimately ignore the request -- the
	// ports suite asserts exactly that -- so "cancelled but still running" is a
	// normal, expected and possibly permanent state. It was also the one state
	// no read path could show: both columns have existed since the first schema
	// on all three dialects, POST /cancel wrote them, the guest read them, and
	// no API route returned either, so a cancelled run reported status "ready"
	// and an operator could not tell a request that had not landed from one the
	// workflow was deliberately ignoring (cleat#1351).
	//
	// The reason is omitempty and the flag is not: a false flag is the answer to
	// "has this been cancelled", and omitting it would make "no" and "the field
	// is missing" the same response. An absent reason means none was given.
	CancellationRequested bool   `json:"cancellation_requested"`
	CancellationReason    string `json:"cancellation_reason,omitempty"`

	// StartedAt is when a worker FIRST began executing this run, or nil for a
	// run that has never been claimed -- and for every run claimed before
	// migration 055, which is not backfilled because such a run has no knowable
	// start (cleat#1090).
	//
	// First claim, not latest: the claim stamps it with COALESCE, so a reclaim
	// does not move it. That makes started_at - created_at the queue latency and
	// completed_at - started_at the elapsed execution, which are the two
	// quantities completed_at - created_at could not be separated into.
	//
	// A pointer for the same reason as CompletedAt above.
	StartedAt *time.Time `json:"started_at,omitempty"`

	// ParentWorkflowID is the run that spawned this one, or nil for a run
	// nobody spawned. cleat#1103.
	//
	// The column has been written since children existed and was selected by
	// nothing: unlike completed_at (cleat#1091), which reached Go and was
	// dropped, this never left the row. "Is a field returned" and "is a field
	// read" are separate questions, and a column can fail either one alone.
	//
	// A pointer because absence is the answer for every top-level run, which is
	// the common case -- same reasoning as CompletedAt and StartedAt.
	//
	// NOT ContinuedFrom, which is already exposed and names the opposite
	// relation: a continuation is not a child, and GetChildCount and
	// enforceParentClosePolicy key off THIS column rather than that one.
	ParentWorkflowID *string `json:"parent_workflow_id,omitempty"`
	Generation       int64   `json:"generation"`
	Priority         int     `json:"priority"`
	TraceID          string  `json:"trace_id,omitempty"`

	// PendingTerminalStatus is the outcome a two-phase terminal transition
	// has already decided and has not yet applied: "" for the overwhelming
	// majority of rows, and the status to finalize with once this claim's
	// defer phase completes. See engine/defer_phase.go.
	//
	// It is what tells the dispatch loop that the claim it just took is a
	// defer segment rather than ordinary work, which is why it is carried on
	// the claim rather than read back afterwards. Status cannot carry it:
	// every claim sets status = 'running' and returns the new value.
	PendingTerminalStatus string `json:"pending_terminal_status,omitempty"`

	// The signal counters live on the ROW and are never read into Go, which
	// is why there is no field for them here.
	//
	// signal_seq (bumped by DeliverSignal), signal_seq_at_claim (stamped by
	// every claim), signal_consumed_seq (bumped by ConsumeSignal) and
	// signal_consumed_at_claim are compared by finalize_workflow_status inside
	// its own transaction. Nothing carries a value between them, which is the
	// whole point of the shape: the alternative was a parameter through
	// FinalizeWorkflowSegment and its nine implementations (cleat#953).
	//
	// A `SignalSeq int64` field was added here by #981 and removed in the
	// follow-up: nothing ever read or wrote it. It shipped describing a
	// mechanism that does not exist -- "the worker carries this value from
	// claim to finalize" -- which is the design that was proposed and then
	// replaced by the on-row capture before the PR was written. A struct
	// field with a confident comment and no reader is worse than no field:
	// the next person to touch this reads it as the mechanism.

	// ContinuedFrom is the run that continued into this one -- the id of the
	// predecessor in a ContinueAsNew chain, or "" for the overwhelming
	// majority of rows, which are not continuations. cleat#826, cleat#887.
	//
	// POPULATED ON THE READ PATH ONLY: GetWorkflowByID sets it, and nothing
	// else does. A claim does not, because the claim queries share one scanner
	// (Dialect.scanWorkflowInstanceExtra) with one column list, and the
	// dispatch loop has no use for the link -- adding it there would widen the
	// hottest query in the system to carry a field nobody reads.
	//
	// So "" means either "not a continuation" OR "you did not get this from
	// GetWorkflowByID". Stated rather than left to be discovered, because a
	// field that is populated on one path and empty on another is exactly the
	// shape that reads as data.
	ContinuedFrom string `json:"continued_from,omitempty"`

	// ReclaimCount is how many times ReapStaleInstances has taken this
	// workflow back from a worker that stopped heartbeating. cleat#1008.
	//
	// It is NOT a bound and nothing compares it to a limit. Every cause of
	// repeated reclaim that survives the worker's own default limits is
	// infrastructure rather than workload -- a runaway guest is interrupted by
	// --wasm-instance-timeout, --wasm-wall-clock-ceiling or --wasm-memory-max-mb
	// and becomes a terminal workflow failure, not a worker death -- so what
	// reaches this counter is host OOM, node failure, deploy and SIGKILL.
	// Dead-lettering past a threshold would turn a node being redeployed into
	// permanent failure of a workflow that did nothing wrong. The count is
	// recorded so an operator can SEE a reclaim loop; whether anything should
	// act on it is a separate decision that now has something to read.
	//
	// Generation cannot answer this question, which is why the column exists:
	// it counts claims, and the ordinary suspend/resume path drives it. A
	// workflow that completed successfully without ever being reclaimed has
	// been measured at generation 12. See
	// engine/generation_is_not_a_reclaim_count_test.go.
	//
	// POPULATED ON BOTH READ PATHS: GetWorkflowByID and ListWorkflows. Not on
	// the claim path, and the distinction is the whole of the argument.
	//
	// This comment said "GetWorkflowByID sets it and nothing else does" until
	// cleat#1123, and cited "the hottest query in the system" for not widening
	// the others. That objection is about the CLAIM query -- the dispatch loop
	// runs it continuously and has no use for the value -- and the claim path
	// has its own column list and its own scanner
	// (Dialect.scanWorkflowInstanceExtra), so it is untouched. ListWorkflows is
	// a user-facing endpoint reached once per page view, and it reads
	// Dialect.workflowInstanceColumns(), which now selects the column.
	//
	// Carrying it there is not a nicety. The field has no omitempty, and
	// deliberately so -- see engine/reclaim_count_records_reclaims_only_test.go,
	// where dropping a zero is argued against because 0 is the answer for most
	// workflows. That decision is only safe if every path that serialises the
	// field has actually read it: a 0 from a path that never SELECTed the
	// column is indistinguishable from "never reclaimed", so the list reported
	// a confident 0 for every run, including one in the reclaim loop the
	// column exists to surface.
	//
	// So 0 now means "never reclaimed" on either read path.
	// engine/list_workflows_returns_every_field_test.go enforces it.
	ReclaimCount int64 `json:"reclaim_count"`
}

// GenerationKey identifies one execution's claim on a run: the run it is
// executing and the generation it was claimed at. cleat#2008.
//
// A worker's in-flight executions are not interchangeable with the run IDs
// alone -- a run can be reclaimed and re-claimed by the SAME worker (the
// probe in cleat#2008 is exactly that), and the two executions share a
// WorkflowID but not a Generation. HeartbeatBatchFenced is keyed on the
// pair for that reason: it must tell the two apart.
type GenerationKey struct {
	WorkflowID string
	Generation int64
}

// Disabled reports whether this schedule has been retired.
//
// Exists for the same reason WorkflowDef.Disabled() does: the question callers
// ask is "should this schedule fire", and DisabledAt != nil is easy to write as
// DisabledAt == nil by accident in a negated condition. That risk is higher
// here than anywhere else in the entity class, because this is the one member
// whose previous spelling ran the other way -- code and habits written against
// `enabled` read true for the live case.
func (s Schedule) Disabled() bool { return s.DisabledAt != nil }

// Schedule is a row from workflow_schedules.
type Schedule struct {
	Name           string          `json:"name"`
	DefName        string          `json:"def_name"`
	EntryPoint     string          `json:"entry_point"`
	CronExpression string          `json:"cron_expression"`
	Input          json.RawMessage `json:"input"`

	// DisabledAt is when this schedule was retired. nil means live, and a live
	// schedule is the only kind GetDueSchedules and admin.get_due_schedules()
	// return.
	//
	// cleat#1702 replaced an `enabled BOOLEAN` column with this one (migration
	// 089/077/081), and that conversion INVERTED POLARITY: enabled=true meant
	// live. It is the reason the contract picked a timestamp -- `enabled` and
	// `deprecated` were the same idea at opposite polarity, so a generic
	// "is this live?" helper over the entity class was silently backwards for
	// exactly one member. Prefer Disabled() to reading this field in a negated
	// condition, for the same reason WorkflowDef.Disabled() exists.
	//
	// Values backfilled by the conversion are an UPPER BOUND: the boolean
	// recorded that a schedule was disabled and never when.
	DisabledAt *time.Time `json:"disabled_at,omitempty"`

	NextRunAt time.Time  `json:"next_run_at"`
	LastRunAt *time.Time `json:"last_run_at,omitempty"`

	// Timezone is the IANA zone the cron expression's wall-clock fields are
	// evaluated in (see engine.NextCronTimeIn). Empty means
	// DefaultScheduleTimezone; the stores write 'UTC' rather than '' so the
	// column is never ambiguous about whether a zone was chosen.
	Timezone string `json:"timezone"`

	// MisfirePolicy decides what a firing missed during an outage means:
	// "catch_up" (default) delivers the backlog one instant per tick up to
	// CatchUpLimit, "skip" resumes at the next future instant. Empty means
	// the default.
	MisfirePolicy string `json:"misfire_policy"`

	// CatchUpLimit bounds how many owed firings catch_up will work through
	// before giving up and resuming in the future. Zero means the default;
	// see engine.DefaultCatchUpLimit.
	CatchUpLimit int `json:"catch_up_limit"`

	// OverlapPolicy decides what happens when an instant arrives and the run
	// this schedule started last has not finished: "allow" (default, and what
	// the scheduler has always done) or "skip". Empty means the default.
	OverlapPolicy string `json:"overlap_policy"`

	// LastRunID is the run this schedule started most recently. It is what
	// makes OverlapPolicy "skip" answerable at all -- without it there is no
	// way to tell a run this schedule started from any other run of the same
	// definition.
	LastRunID string `json:"last_run_id,omitempty"`

	// TenantID owns this schedule, and is the tenant the runs it starts belong
	// to.
	//
	// Populated on read. It is NOT read from the caller on CreateSchedule --
	// the stores write their own s.tenantID there, so a caller cannot create a
	// schedule for a tenant it is not scoped to.
	TenantID string `json:"tenant_id"`

	// IdempotencyKey is the caller-supplied Idempotency-Key that created this
	// schedule, and is what lets POST /api/schedules tell a retry from a name
	// collision. Empty means the schedule was created without one, which is a
	// fact about it rather than a gap -- see migrations/postgres/072.
	//
	// WRITE-ONLY, and `json:"-"` is the point rather than tidiness. A key is a
	// secret in the weak sense that knowing one lets a caller join or displace
	// another caller's retry, and ListSchedules is readable by anything holding
	// the tenant's credentials. It goes in and is never handed back.
	//
	// There is deliberately no InputDigest field beside it. The digest is
	// computed by the store, over the same value the store writes to `input`,
	// so a caller cannot present a digest that disagrees with the input it is
	// sending -- which is the one way a mismatch check can be made to pass
	// against input it has not seen.
	IdempotencyKey string `json:"-"`
}

// PromiseInfo holds the state of a cleat promise.
type PromiseInfo struct {
	PromiseID   string     `json:"promise_id"`
	PromiseName string     `json:"promise_name"`
	Status      string     `json:"status"`
	Result      string     `json:"result,omitempty"`
	ErrorMsg    string     `json:"error_msg,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
}

// ConcurrencyKeyInfo holds the state of an acquired concurrency key.
type ConcurrencyKeyInfo struct {
	KeyHash    []byte    `json:"key_hash"`
	KeyText    string    `json:"key_text"`
	WorkflowID string    `json:"workflow_id"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// UpdateRequestInfo holds the state of an incoming update request.
type UpdateRequestInfo struct {
	WorkflowID string `json:"workflow_id"`
	// RequestID identifies this request. UpdateName does NOT: cleat#1416 made a
	// name reusable, so a workflow can hold several requests that share one.
	RequestID  string    `json:"request_id"`
	UpdateName string    `json:"update_name"`
	Payload    string    `json:"payload"`
	PromiseID  string    `json:"promise_id,omitempty"`
	Status     string    `json:"status"`
	Result     string    `json:"result,omitempty"`
	ErrorMsg   string    `json:"error_msg,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// WorkflowMemoryStats holds distribution statistics for per-definition memory usage.
type WorkflowMemoryStats struct {
	DefName     string  `json:"def_name"`
	MinBytes    int64   `json:"min_bytes"`
	AvgBytes    float64 `json:"avg_bytes"`
	MaxBytes    int64   `json:"max_bytes"`
	P10         int64   `json:"p10"`
	P25         int64   `json:"p25"`
	P50         int64   `json:"p50"`
	P75         int64   `json:"p75"`
	P90         int64   `json:"p90"`
	P99         int64   `json:"p99"`
	SampleCount int     `json:"sample_count"`
}

// WorkflowFilter contains optional filter parameters for listing workflow instances.
// Empty/zero values mean "no filter" for that parameter.
type WorkflowFilter struct {
	Status        string
	InputContains string
	// ResultContains is the counterpart to InputContains for the result
	// payload. Both are UNINDEXED on every dialect and deliberately so: a
	// trigram index over serialized JSON amplifies every write, which
	// migrations/postgres/033 records rejecting for that reason. They are
	// separate parameters so that the cost is chosen rather than inherited --
	// Search used to include both and made every search pay for them.
	ResultContains string
	ErrorContains  string
	// Search is a substring match over def_name and error_msg -- the two short,
	// indexable columns. It deliberately does NOT cover input or result; ask
	// for those with InputContains and ResultContains.
	Search string
	Offset int
	Limit  int

	// The four below address columns the row already carries. Before them the
	// only way to ask "the runs of workflow X" was Search, a four-way substring
	// LIKE over input, result, error_msg and def_name -- so it also matched
	// unrelated runs whose PAYLOAD contained the string, and gave no signal
	// about which column matched. cleat#1183, and cleat#1122 from a second
	// upstream asking for the same surface.

	// DefName matches def_name exactly. Search is substring and spans four
	// columns; this is the targeted form.
	DefName string
	// ErrorCode matches error_code exactly. Cancellation is an error code
	// rather than a status, so without this a cancelled run cannot be selected
	// as a class at all.
	ErrorCode string
	// IDPrefix matches the start of the run id. `id` is text on all three
	// dialects (TEXT / VARCHAR(255) / NVARCHAR(255)), so this needs no cast.
	IDPrefix string

	// ConcurrencyKey matches the key a run asked for, exactly.
	//
	// cleat#1172: when a start is refused for a key conflict the two questions
	// are what holds it and for how long. The refusal now names the holder
	// (#1230), but an operator looking at a STUCK key has no run id to start
	// from -- they have the key, which is the thing they typed. This is the
	// route from the key back to the runs.
	//
	// Matches on concurrency_key, the text, not concurrency_key_hash. An
	// operator has the key string; making them hash it first to search for it
	// would be a worse API than not having the filter.
	ConcurrencyKey string
	// StartedAfter and StartedBefore bound created_at, inclusive of after and
	// exclusive of before -- the usual half-open interval, so adjacent windows
	// tile without double-counting a row on the boundary.
	StartedAfter  time.Time
	StartedBefore time.Time
}

// RoutingRule represents a traffic-splitting rule for A/B testing.
type RoutingRule struct {
	ID            string
	WorkflowName  string
	TargetVersion int
	Weight        float64
}

// WorkflowStore is the database interface for the worker.
