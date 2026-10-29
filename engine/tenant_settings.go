package engine

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrLimitsUnavailable is returned by an execution that could not read the
// limits it must run under: this tenant's settings, or this run's own.
//
// It is not a failure OF the workflow. The guest never ran, so there is no
// outcome to record, and a caller that records one destroys a run that did
// nothing wrong. The worker releases the run for another attempt (with the
// unservable backoff) instead.
var ErrLimitsUnavailable = errors.New("the limits this execution must run under could not be read")

// limitsReadBackoff is the pause before a failed limits read is retried. A var
// so a test does not wait on it.
var limitsReadBackoff = 100 * time.Millisecond

// TenantSettings is one tenant's overrides for the execution limits that are
// otherwise process-wide worker flags.
//
// # Why this exists
//
// Every execution limit cleat has is a flag on cleat-worker, so a deployment
// shared by several microservices -- or several organisations -- gives all of
// them one value. IMPROVEMENT-PLAN 3.94 is the requirement that each tenant be
// able to manage its own without affecting the others.
//
// # A zero field means "no override", never "unbounded"
//
// The executor tests `ceiling > 0` before applying a bound, so zero already
// means unbounded at the point of use. This struct cannot reuse it for that:
// a tenant able to store zero could unbound its own limits, which is the one
// direction ClampToCeiling exists to forbid. The database refuses non-positive
// values (CHECK constraints on all three dialects) and readTenantSettings
// refuses them again, so a zero here can only be an absent override.
type TenantSettings struct {
	// WasmInstanceTimeout overrides --wasm-instance-timeout: guest EXECUTION
	// time, the epoch fence (3.90).
	//
	// NOT YET CONSUMED. Applying it means setting the epoch deadline on the
	// per-execution backend, which needs a WasmBackend interface change --
	// 3.94 step 5b. Until then the flag alone governs, and
	// TestOnlyTheWallClockCeilingIsWiredYet asserts exactly that, so this
	// sentence cannot quietly stop being true.
	WasmInstanceTimeout time.Duration

	// WasmWallClockCeiling overrides --wasm-wall-clock-ceiling: WALL CLOCK for
	// one invocation, including time spent waiting inside host calls (3.90).
	//
	// This is the one field wired today, in Engine.wallClockCeiling.
	WasmWallClockCeiling time.Duration

	// HostRetryBudget overrides the threshold above which a retry policy is
	// suspended and replayed rather than run in-process (3.88).
	//
	// Read by Engine.hostRetryBudget, clamped to --host-retry-budget, and
	// applied in execSession.DurableCallWithRetry: a policy whose worst-case
	// backoff exceeds it is refused with callErrorCode 6 and the guest runs it
	// itself, suspending between attempts.
	//
	// The threshold used to be a constant compiled into each guest module, so
	// the guest had already chosen a path before the host was consulted and no
	// tenant could change it. 3.94 step 4 moved the decision here.
	HostRetryBudget time.Duration

	// MaxWorkflowDuration overrides --max-workflow-duration.
	//
	// Why it exists (cleat#1117): a worker process is not a tenancy boundary.
	// One worker serves many tenants, so a process-wide deadline applies one
	// tenant's operational policy to another's workflows. The other three had
	// the same defect and 3.94 fixed it for them; this one was left on the flag.
	//
	// SCOPE, stated because the name invites the wrong reading and I made it
	// while writing this: despite "workflow" in the name, this bounds ONE
	// EXECUTION SEGMENT, not a workflow's whole lifetime. executor.go applies
	// it to execCtx, which derives from the per-invocation context, so a
	// workflow that suspends and resumes gets a fresh deadline each time. That
	// is pre-existing behaviour of the flag and cleat#1117 does not change it --
	// the issue is about WHO may set the bound, not what it spans. See the note
	// at executor.go's event-cap comment for the same distinction biting a
	// different mechanism.
	MaxWorkflowDuration time.Duration
}

// TenantSettingsReader is implemented by stores that can read the settings row
// for the tenant they are scoped to.
//
// It is deliberately NOT part of WorkflowStore. That interface has 99 methods
// and ten implementations, all but four of them mocks, so adding a hundredth
// would mean ten edits to make one feature work.
//
// The cost of an optional interface is the failure mode it invites: a store
// that stops satisfying it degrades to flag defaults with nothing failing --
// "a per-tenant limit that is never exercised", which is the specific hazard
// 3.94 names. tenant_settings_wiring_test.go closes it, asserting at compile
// time that each of the three real stores implements this, and asserting
// separately that ShardedStore does not (see below).
type TenantSettingsReader interface {
	// GetTenantSettings returns the settings row for this store's tenant.
	// A tenant with no row is not an error: it returns the zero value, which
	// resolves to the operator's flags for every field.
	GetTenantSettings(ctx context.Context) (TenantSettings, error)
}

// ClampToCeiling resolves one tenant-supplied limit against the operator's.
//
// A tenant may only ever TIGHTEN a limit. Without that, per-tenant settings
// are not a feature but a hole: any tenant on a shared deployment could raise
// its own bounds past what the operator granted and hold workers indefinitely,
// which is worse than having no per-tenant settings at all.
//
//	tenant <= 0   no override        -> the operator's value
//	ceiling <= 0  operator unbounded -> the tenant's value (it can only tighten)
//	otherwise                        -> the smaller of the two
//
// # The invariant this depends on
//
// "Smaller is safer" is true of every setting in TenantSettings because each
// one bounds a resource the tenant consumes -- CPU, a worker slot, in-process
// wait. It is NOT a general rule. A future setting where a larger value is the
// safer one (a minimum retention, say, or a floor on an interval) would need
// the opposite comparison, and using this function for it would grant exactly
// the escalation the paragraph above rules out. Check the direction before
// adding a field, not after.
func ClampToCeiling(tenant, ceiling time.Duration) time.Duration {
	if tenant <= 0 {
		return ceiling
	}
	if ceiling <= 0 {
		return tenant
	}
	if tenant < ceiling {
		return tenant
	}
	return ceiling
}

// tenantSettingsFromMillis builds a TenantSettings from the nullable
// millisecond columns, shared by all three dialect read paths.
//
// Non-positive values are dropped rather than carried. The CHECK constraints
// already refuse them, so this is the second layer: a database restored from
// before those constraints, or one where a CHECK was dropped, cannot turn into
// an unbounded worker. Dropping is right rather than erroring -- a settings row
// that has gone strange should cost the tenant its overrides, not its ability
// to run workflows.
//
// Taking one parameter per column rather than a struct is deliberate: adding a
// field here is a compile error at every dialect read path, which is what makes
// it impossible to add a limit and silently leave one dialect reading NULL.
// cleat#1117 added the fourth parameter and the compiler found all six sites.
func tenantSettingsFromMillis(instanceMs, wallClockMs, retryMs, maxWorkflowMs *int64) TenantSettings {
	ms := func(v *int64) time.Duration {
		if v == nil || *v <= 0 {
			return 0
		}
		return time.Duration(*v) * time.Millisecond
	}
	return TenantSettings{
		WasmInstanceTimeout:  ms(instanceMs),
		WasmWallClockCeiling: ms(wallClockMs),
		HostRetryBudget:      ms(retryMs),
		MaxWorkflowDuration:  ms(maxWorkflowMs),
	}
}

// limitsReadAttempts is how many times a limits read is tried before it is
// final. A dropped connection is the case this exists for: the pool hands out
// the dead one once, and the next attempt gets a live one.
const limitsReadAttempts = 3

// readLimits reads one tier of limits, retrying a failure, and reports a final
// one as ErrLimitsUnavailable.
//
// A cancelled context is NOT a final failure of the limits, and is returned as
// the context's own error. A fence or a shutdown cancels the context, the read
// fails with it, and what that describes is a run being stopped; the worker
// tells a stopped run from a broken one by the context.
func (e *Engine) readLimits(ctx context.Context, what string, read func() (TenantSettings, error)) (TenantSettings, error) {
	var err error
	for attempt := 1; attempt <= limitsReadAttempts; attempt++ {
		var s TenantSettings
		if s, err = read(); err == nil {
			return s, nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return TenantSettings{}, fmt.Errorf("reading %s: %v: %w", what, err, cerr)
		}
		e.log().WarnContext(ctx, "reading limits failed",
			"what", what, "attempt", attempt, "of", limitsReadAttempts,
			"tenant_id", e.tenantID, "workflow_id", e.workflowID, "error", err)
		if attempt == limitsReadAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return TenantSettings{}, fmt.Errorf("reading %s: %v: %w", what, err, ctx.Err())
		case <-time.After(time.Duration(attempt) * limitsReadBackoff):
		}
	}
	return TenantSettings{}, fmt.Errorf("%w: reading %s failed %d times: %w",
		ErrLimitsUnavailable, what, limitsReadAttempts, err)
}

// ensureLimits reads this execution's two tiers of limits, the tenant's
// settings and the run's own, and keeps what it read.
//
// Only a SUCCESSFUL read is kept. A failure is retried and then returned, and
// the next call tries again. cleat#3565: this used to log a failure and
// memoise "no overrides" for the whole execution, on the argument that the
// operator's own flags are "never wider". They are wider for exactly the
// tenants that set a tighter limit -- a 2s instance timeout against a 30s flag
// -- so one dropped connection at the first read let a guest run for fifteen
// times its bound, and nothing said so but a log line.
//
// An execution that cannot read its limits therefore does not run. The cost is
// real and is the one the old comment named: a settings table that stays
// unreadable stops executions on that worker, where it used to run them
// unbounded. It stops them as a RELEASE (see ErrLimitsUnavailable), not a
// failure, so each run keeps its history and waits; and a database that cannot
// answer this query is not one that can append the run's events either.
//
// A store that does not implement a tier's reader has no overrides to read,
// which is a successful read of nothing and not a failure.
//
// Called at the top of every execution entry point, so the accessors below are
// reading values that are already there. They call it as well, so an engine
// driven some other way still reads; see tenantSettings.
func (e *Engine) ensureLimits(ctx context.Context) error {
	e.limitsMu.Lock()
	defer e.limitsMu.Unlock()

	if !e.tenantSettingsLoaded {
		if reader, ok := e.workflowStore.(TenantSettingsReader); ok {
			s, err := e.readLimits(ctx, "per-tenant settings", func() (TenantSettings, error) {
				return reader.GetTenantSettings(ctx)
			})
			if err != nil {
				return err
			}
			e.tenantSettingsValue = s
			e.warnIfTenantLimitsConflict(ctx, s)
		}
		e.tenantSettingsLoaded = true
	}
	if !e.runLimitsLoaded {
		if reader, ok := e.workflowStore.(RunLimitsReader); ok && e.workflowID != "" {
			s, err := e.readLimits(ctx, "per-run limits", func() (TenantSettings, error) {
				return reader.GetRunLimits(ctx, e.workflowID)
			})
			if err != nil {
				return err
			}
			e.runLimitsValue = s
		}
		e.runLimitsLoaded = true
	}
	return nil
}

// tenantSettings returns this execution's tenant settings, reading them if
// ensureLimits has not.
//
// Every execution entry point has called ensureLimits and returned its error
// before reaching a caller of this, so in a real execution the read cannot fail
// here. If it does -- an engine driven by a path that skipped the entry points
// -- the answer is "no overrides" for THIS call only: it is logged, not kept,
// and the next call reads again.
func (e *Engine) tenantSettings(ctx context.Context) TenantSettings {
	if err := e.ensureLimits(ctx); err != nil {
		e.log().ErrorContext(ctx, "per-tenant settings are unavailable; this call sees no overrides",
			"tenant_id", e.tenantID, "workflow_id", e.workflowID, "error", err)
	}
	return e.tenantSettingsValue
}

// warnIfTenantLimitsConflict is the per-tenant half of §3.90's startup warning,
// and it is 3.94 step 6.
//
// The hazard is the same one §3.90 found in the flags, now reachable by a
// tenant on its own: a wall-clock ceiling BELOW the instance timeout means the
// invocation's context deadline expires before the epoch fence can, so the
// guest's execution bound silently becomes the ceiling and the instance
// timeout stops being the number that decides. Nothing fails; the wrong number
// just wins, which is why this is worth saying out loud.
//
// Called from ensureLimits when the settings are first read, so it logs once per
// engine rather than once per execution. A per-execution warning for a durable workflow is a log
// line per replay segment, which is how a real signal becomes noise nobody
// reads.
//
// # What this can and cannot see
//
// It compares the tenant's OWN two overrides. It deliberately does not try to
// judge a tenant's wall-clock ceiling against the operator's instance timeout,
// because the engine cannot see that number -- it lives on the backend, set
// from --wasm-instance-timeout when the worker builds it (see
// WasmBackend.PerExecution). Warning on a comparison against a value this code
// does not have would mean guessing, and a confident wrong warning is worse
// than none: the next reader tunes against it.
//
// So this does NOT replace the startup warning in cmd/cleat-worker, which
// checks the operator's own two flags against each other. The two cover
// different misconfigurations and both are kept.
func (e *Engine) warnIfTenantLimitsConflict(ctx context.Context, s TenantSettings) {
	if s.WasmWallClockCeiling <= 0 || s.WasmInstanceTimeout <= 0 {
		return
	}
	if s.WasmWallClockCeiling >= s.WasmInstanceTimeout {
		return
	}
	e.log().WarnContext(ctx,
		"this tenant's wall-clock ceiling is below its instance timeout, so its guest execution bound is effectively the ceiling",
		"tenant_id", e.tenantID,
		"wall_clock_ceiling", s.WasmWallClockCeiling,
		"instance_timeout", s.WasmInstanceTimeout)
}
