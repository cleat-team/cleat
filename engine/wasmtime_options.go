package engine

import (
	"log/slog"
	"time"
)

// This file has no build constraint (unlike backend_wasmtime.go, which
// requires cgo) so that cmd/cleat-worker and other callers can construct
// WasmtimeOption values unconditionally, regardless of whether the binary
// was built with CGO. When CGO is disabled, NewWasmtimeBackend (the stub in
// backend_wasmtime_stub.go) simply ignores the options and returns an error.

// DefaultWasmtimeExecutionTimeout bounds a single wasmtime invocation (one
// fresh execution or one replay pass) via epoch interruption when the
// caller has not configured a tighter one. This is a required safety net,
// not just a convenience default: wasmtime-go does not observe Go context
// cancellation while a WASM export call is in progress (see the comment on
// wasmtimeBackend.Execute in backend_wasmtime.go), so without a baked-in
// bound here an infinite loop in a workflow hangs the worker permanently
// regardless of any engine- or worker-level context timeout.
//
// 30s matches the wazero backend's hardcoded Runtime.callTimeout default
// (engine/runtime.go), so both backends behave the same way out of the box.
const DefaultWasmtimeExecutionTimeout = 30 * time.Second

// DefaultWasmtimeMemoryLimitBytes bounds linear memory per wasmtime store
// when the caller has not configured a tighter one via
// WithWasmtimeMemoryLimits. It matches DefaultMemoryLimitPages (32 MiB),
// the wazero backend's default (engine/runtime.go), so an operator sees the
// same memory ceiling regardless of which backend happens to run a given
// workflow.
const DefaultWasmtimeMemoryLimitBytes = int64(DefaultMemoryLimitPages) * int64(wasmPageSize)

// DefaultWasmtimeTableElementsLimit bounds indirect-function-table growth
// per wasmtime store.
//
// The 1,048,576-element figure it is derived from came from `tblMinSize` in
// wasmtimeBackend.ExecuteComponent -- the decomposition path, deleted
// 2026-09-01 (IMPROVEMENT-PLAN 3.65). The number is kept rather than
// re-derived: it was measured from real componentize-py bundles, those bundles
// have not changed, and the native Component Model path instantiates the same
// core modules with the same tables. 8x that headroom keeps existing workflows
// working while still capping unbounded/attacker-controlled table growth.
//
// If it ever needs re-deriving, the source is the largest `(table ...)` minimum
// in a componentize-py component's core modules, not anything in this repo.
const DefaultWasmtimeTableElementsLimit = 8 * 1024 * 1024

// DefaultWasmtimeInstancesLimit bounds how many module instances a single
// wasmtime store may create. Component bundles observed in this codebase
// (CPython runtime + adapters) use at most a few dozen instances; 256
// leaves generous headroom while still bounding runaway instantiation.
const DefaultWasmtimeInstancesLimit = 256

// DefaultWasmtimeDeferBudget bounds the cleanup pass the host runs on a guest
// it has just killed (IMPROVEMENT-PLAN 3.35 phase 4).
//
// This is EXTRA execution granted to a workflow the fence has already stopped,
// so it has to be small enough not to undo the bound it is being granted
// against. 5s next to the 30s DefaultWasmtimeExecutionTimeout raises the
// worst case a workflow can hold a worker by about a sixth rather than
// doubling it, and a cleanup pass is a handful of host calls -- releasing a
// lock, refunding a charge -- not a workload.
//
// It is a wall-clock bound and it is the binding one: it stops a defer body
// that loops forever exactly the way the fence stops an entry point that does.
const DefaultWasmtimeDeferBudget = 5 * time.Second

// DefaultWasmtimeInterruptMarginFuel is the fuel reserve at which the soft kill asks the guest to
// stop cleanly instead of letting wasmtime's fuel trap kill the instance mid-borrow (cleat#3922).
//
// Sized from the largest amount of fuel measured between two consecutive handler calls, by
// TestMeasureInterruptMargin (`go test -tags measure ./tests/typescript/`) over the shapes of
// endless loop it keeps: 1,965,040 (tight numeric
// loop), 3,840,041 (a native call in a loop) and 26,241,715 (JSON.parse of a 200k-element payload).
// A fourth, a deliberately exponential regex at 2,116,250, was removed with its workload when code
// scanning flagged the literal; it was never load bearing. 33,554,432 fuel units -- 32 x 1024 x 1024, wasmtime's own count and not
// bytes -- is the largest of those with about a fifth of headroom.
//
// IT IS NOT A CEILING, and the margin is a best-effort reserve rather than a guarantee. QuickJS
// polls its interrupt handler once per ~10,000 BYTECODE operations, while fuel is spent by whatever
// that operation does, so an operation that spends a lot of wasm fuel in one go widens the gap
// without bound: a loop of `"x".repeat(10_000)` did not reach a single poll inside a 400M budget
// (measured 2026-11-17). A guest dominated by that shape still ends on the hard fuel trap, and its
// defers then do not run. That residue is recorded in packages/cleat-ts/DIVERGENCES.md.
//
// It applies only when fuel is metered at all: with --wasm-instruction-limit 0 (the default) there
// is no budget to approach and no soft stop to offer.
const DefaultWasmtimeInterruptMarginFuel = 32 * 1024 * 1024

// DefaultWasmtimeInterruptMarginTime is the epoch arm's reserve: how much of the instance-timeout
// budget must be left for the soft kill to stop the guest cleanly (cleat#3922).
//
// This arm is the one that matters in the default configuration, where --wasm-instruction-limit is
// 0 and the kill is the epoch fence. Sized from the larger of one epoch tick (epochTickInterval,
// 50ms -- a guest stops at the first tick after the deadline, so a reserve smaller than one tick
// can pass the deadline before the handler is asked again) and the longest time measured between
// two consecutive handler calls; see TestMeasureInterruptMargin (built only with `-tags measure`).
//
// The effective reserve is the smaller of this, a tenth of the budget and half of it, with a floor
// of interruptMinMarginTime (two epoch ticks plus a poll gap): a reserve under two ticks can be
// overtaken by the hard trap before any poll sees it, and one above half the budget stops a
// workflow almost at once. On a sub-second budget the floor is what binds, so the kill can arrive up
// to half the budget early.
const DefaultWasmtimeInterruptMarginTime = 250 * time.Millisecond

// interruptUnwindGrant is how much budget the soft kill gives back before the guest unwinds
// (cleat#3922). Measured 2026-11-17, from the handler answering 1 to the host observing the call's
// error, over the workloads in TestMeasureInterruptMargin (`-tags measure`): 0.10-0.46ms in the ordinary shapes and
// 1.33ms for the largest (JSON.parse of a 200k-element payload), spending at most 9.16M fuel.
//
// It has to exceed ONE EPOCH TICK, and that is the real reason it exists rather than the unwind's
// cost. hostBudget.setDeadline arms ticks = remaining/50ms floored at one, and a deadline of N
// ticks expires 50*(N-1) to 50*N ms after it is armed. So the tight case is a budget under 100ms at
// the moment of the stop: that arms ONE tick, whose expiry is a uniform 0-50ms away, and a 0.15ms
// unwind loses that race about once in 330 stops.
//
// WHERE THAT CASE ARISES. With --wasm-instance-timeout at its 30s default the margin is 250ms and
// the bracket arms 5 ticks at the stop, so the guest has 200-250ms to unwind and no race exists.
// A budget under 100ms at the stop needs the reserve to be under two ticks, which the review of this
// PR measured at an 800ms budget -- TestTypeScriptSoftKillsWithASubSecondBudget -- and which lost 7
// drains in 20 before the floor above existed. The floor removes the race there; the grant remains
// for the case where a poll gap or a short ctx deadline still lands the stop inside one tick.
const interruptUnwindGrant = 100 * time.Millisecond

// wasmtimeLimits bundles the resource bounds applied to a wasmtime
// execution. Zero/negative fields mean "use the backend's built-in
// default" (see the Default* constants above) except instructionLimit,
// where 0 legitimately means "fuel metering disabled" — this matches the
// existing --wasm-instruction-limit flag semantics ("0 = no limit").
type wasmtimeLimits struct {
	executionTimeout   time.Duration
	instructionLimit   uint64
	memoryLimitBytes   int64
	tableElementsLimit int64
	instancesLimit     int64
	deferBudget        time.Duration

	// interruptMargin is the fuel reserve the soft kill stops at (cleat#3922). Zero takes
	// DefaultWasmtimeInterruptMarginFuel; it is only consulted when instructionLimit > 0.
	interruptMargin uint64
}

// wasmtimeConfig is everything NewWasmtimeBackend accepts: the resource
// bounds, plus the things that are not bounds.
//
// The options used to be func(*wasmtimeLimits), which was accurate while
// limits were all there was. A logger is not a limit, and putting one in a
// struct called "limits" is the kind of small dishonesty that later gets read
// as a fact about the type.
type wasmtimeConfig struct {
	limits wasmtimeLimits
	logger *slog.Logger

	// moduleCacheMaxEntries bounds the compiled-module cache. 0 means
	// DefaultModuleCacheMaxEntries; see moduleLRU. Not in wasmtimeLimits
	// because it bounds a process-wide CACHE rather than anything one
	// execution may consume, which is what the fields in that struct are.
	moduleCacheMaxEntries int

	// moduleCacheMaxBytes bounds the same cache by estimated resident size.
	// 0 means DefaultModuleCacheMaxBytes. Both bounds apply; see moduleLRU
	// for why a count alone could not say what a hundred entries cost.
	moduleCacheMaxBytes int64
}

// WasmtimeOption configures a wasmtimeBackend, applied at construction time
// (NewWasmtimeBackend). Resource bounds are enforced on every store it creates
// thereafter (see wasmtimeBackend.configureStore).
type WasmtimeOption func(*wasmtimeConfig)

// WithWasmtimeLogger routes the backend's own log records to l.
//
// Without it the backend writes to slog.Default(), which is not where an
// operator who configured a logger is looking. That mattered most on the one
// path where the backend has something to say that nothing else can: whether a
// KILLED workflow's defers ran (§3.35 phase 4). Three records -- the success
// line, the "could not be run" line, and the refuel warning -- all went to the
// default logger, so a worker with a configured handler reported nothing at
// all about the cleanup of a workflow it had just killed.
//
// Found while writing a test that asserted on that line and could not see it.
//
// nil keeps slog.Default().
func WithWasmtimeLogger(l *slog.Logger) WasmtimeOption {
	return func(c *wasmtimeConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithWasmtimeExecutionTimeout bounds a single wasmtime invocation via
// epoch interruption. d <= 0 keeps DefaultWasmtimeExecutionTimeout. A
// per-call context deadline (e.g. from engine.WithWASMInstanceTimeout or
// engine.WithDefaultWorkflowTimeout), when tighter than this, still wins —
// see wasmtimeBackend.configureStore.
func WithWasmtimeExecutionTimeout(d time.Duration) WasmtimeOption {
	return func(c *wasmtimeConfig) { c.limits.executionTimeout = d }
}

// WithWasmtimeInstructionLimit bounds fuel (roughly one unit per WASM
// instruction executed) consumed per invocation. 0 disables fuel
// metering — the wasmtime analogue of the wazero-only
// --wasm-instruction-limit flag defaulting to "no limit".
//
// Note for anyone tempted to raise this as the primary defense: fuel
// exhaustion mid-replay could in principle make replay diverge from the
// original execution if the configured limit changes between the two runs
// (e.g. a flag change), or on hardware where the same WASM instructions
// consume different amounts of fuel due to a wasmtime version skew. Epoch
// interruption (WithWasmtimeExecutionTimeout) does not have this property
// any more than wall-clock time already does for CPU-bound workflows, which
// is why it is the primary, always-on bound and fuel is an optional,
// opt-in secondary one.
func WithWasmtimeInstructionLimit(n uint64) WasmtimeOption {
	return func(c *wasmtimeConfig) { c.limits.instructionLimit = n }
}

// WithWasmtimeMemoryLimits bounds linear memory, table elements, and
// instance counts per store (wasmtime's StoreLimits / ResourceLimiter).
// Values <= 0 keep the backend's built-in default for that dimension
// (see the Default* constants above).
func WithWasmtimeMemoryLimits(memoryBytes, tableElements, instances int64) WasmtimeOption {
	return func(c *wasmtimeConfig) {
		c.limits.memoryLimitBytes = memoryBytes
		c.limits.tableElementsLimit = tableElements
		c.limits.instancesLimit = instances
	}
}

// WithWasmtimeDeferBudget bounds the cleanup pass the host runs on a killed
// guest. d <= 0 keeps DefaultWasmtimeDeferBudget.
//
// Raise it for workflows whose cleanup genuinely needs longer -- a defer that
// makes several slow external calls -- knowing that the worst case a runaway
// workflow can occupy a worker is the execution timeout plus this.
func WithWasmtimeDeferBudget(d time.Duration) WasmtimeOption {
	return func(c *wasmtimeConfig) { c.limits.deferBudget = d }
}

// WithWasmtimeModuleCacheMaxEntries bounds how many compiled modules the
// backend retains, evicting least-recently-used beyond that.
//
// cleat#1563: this cache had no eviction at all, so a long-lived worker
// retained one compiled module per distinct WASM artifact it had ever run,
// across every tenant, for the life of the process.
//
// ENTRIES, NOT BYTES, and the flag help says so rather than implying a memory
// It bounds map and list overhead independently of artifact size, which is why
// it survives alongside the byte bound rather than being replaced by it: a
// deployment with thousands of tiny artifacts is bounded by this one.
//
// n <= 0 uses DefaultModuleCacheMaxEntries.
func WithWasmtimeModuleCacheMaxEntries(n int) WasmtimeOption {
	return func(c *wasmtimeConfig) {
		c.moduleCacheMaxEntries = n
	}
}

// WithWasmtimeModuleCacheMaxBytes bounds the compiled-module cache by
// ESTIMATED resident size -- CompiledSizeEstimate over each entry's wasm.
//
// This is the bound a deployment can actually size against. The entry count
// could not say whether a hundred modules was 8.6 MB or 4.6 GB; measured on
// this repo's own artifacts, both are true depending on the guest language.
// cleat#1907.
//
// n <= 0 uses DefaultModuleCacheMaxBytes.
func WithWasmtimeModuleCacheMaxBytes(n int64) WasmtimeOption {
	return func(c *wasmtimeConfig) {
		c.moduleCacheMaxBytes = n
	}
}

// DefaultModuleCacheMaxEntries bounds the compiled-module cache when the
// operator sets no limit.
//
// 100 matches --wasm-cache-max-entries, the BYTE cache's default, deliberately:
// the two caches are keyed the same way and hold entries for the same
// artifacts, so a worker that can hold 100 distinct WASM binaries has no reason
// to hold a different number of compiled forms of them.
//
// DECLARED HERE, IN AN UNTAGGED FILE, and that placement is load-bearing.
// moduleLRU lives in a //go:build cgo file because it names *wasmtime.Module,
// but cmd/cleat-worker/config.go reads this constant for its flag default and
// is built in every configuration. Putting it beside the type would break the
// CGO-less build of the whole command -- which `go build ./...` does not catch,
// because CGO is on by default and that is what CLAUDE.md tells you to use.
const DefaultModuleCacheMaxEntries = 100
