package engine

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// FaultType represents the type of fault to inject.
type FaultType int

const (
	// FaultNetworkPartition simulates a network partition where the worker
	// cannot reach the database.
	FaultNetworkPartition FaultType = iota

	// FaultDiskFull simulates a full disk where write operations fail.
	FaultDiskFull

	// FaultDiskSlow simulates slow disk I/O.
	FaultDiskSlow

	// FaultClockSkew simulates clock skew between worker and database.
	FaultClockSkew

	// FaultWorkerCrash simulates a worker crashing mid-execution.
	FaultWorkerCrash
)

// String returns a human-readable name for the fault type.
func (ft FaultType) String() string {
	switch ft {
	case FaultNetworkPartition:
		return "network_partition"
	case FaultDiskFull:
		return "disk_full"
	case FaultDiskSlow:
		return "disk_slow"
	case FaultClockSkew:
		return "clock_skew"
	case FaultWorkerCrash:
		return "worker_crash"
	default:
		return fmt.Sprintf("unknown(%d)", int(ft))
	}
}

// FaultInjector provides programmable fault injection for testing.
// It simulates infrastructure failures by manipulating database state,
// connection behavior, or timing.
type FaultInjector struct {
	mu     sync.Mutex
	db     *sql.DB
	active map[FaultType]bool

	// Configuration for current faults.
	networkPartitionCtx    context.Context
	networkPartitionCancel context.CancelFunc
	diskLatencyMin         time.Duration
	diskLatencyMax         time.Duration
	clockSkewOffset        time.Duration
}

// NewFaultInjector creates a new FaultInjector for the given database.
func NewFaultInjector(db *sql.DB) *FaultInjector {
	return &FaultInjector{
		db:     db,
		active: make(map[FaultType]bool),
	}
}

// InjectNetworkPartition simulates a network partition by blocking all
// database operations. It cancels the current context and prevents new
// operations from succeeding. Call Cleanup() to restore connectivity.
func (fi *FaultInjector) InjectNetworkPartition() {
	fi.mu.Lock()
	defer fi.mu.Unlock()

	if fi.active[FaultNetworkPartition] {
		return
	}

	// Create a cancelled context to simulate "connection refused".
	fi.networkPartitionCtx, fi.networkPartitionCancel = context.WithCancel(context.Background())
	fi.networkPartitionCancel() // Immediately cancel to simulate partition.

	fi.active[FaultNetworkPartition] = true
}

// InjectDiskLatency configures the injector to simulate slow disk operations.
// Each subsequent database operation will be delayed by a random duration
// between min and max.
func (fi *FaultInjector) InjectDiskLatency(min, max time.Duration) {
	fi.mu.Lock()
	defer fi.mu.Unlock()

	fi.diskLatencyMin = min
	fi.diskLatencyMax = max
	fi.active[FaultDiskSlow] = true
}

// InjectClockSkew simulates clock skew by modifying the database time offset.
// The offset is applied to all subsequent time-based operations.
// A positive offset simulates the database being ahead of the worker.
// A negative offset simulates the database being behind the worker.
//
// Returns the error from the database write, and marks the fault active only if
// that write succeeded. Both halves matter: this used to discard the error and
// set active unconditionally, so a fault that never reached the database was
// indistinguishable from one that did, and IsActive(FaultClockSkew) reported
// true either way. A test asserting the system survives clock skew would then
// pass without any skew ever being applied -- a green result that measured
// nothing, in the harness built to catch exactly that.
func (fi *FaultInjector) InjectClockSkew(offset time.Duration) error {
	fi.mu.Lock()
	defer fi.mu.Unlock()

	// For positive offset (DB ahead), set heartbeat_at to a future time
	// on all running instances.
	if offset > 0 && fi.db != nil {
		if _, err := fi.db.ExecContext(context.Background(),
			`UPDATE workflow_instances SET heartbeat_at = heartbeat_at + $1::interval WHERE status = 'running'`,
			fmt.Sprintf("%d seconds", int(offset.Seconds()))); err != nil {
			return fmt.Errorf("inject clock skew: %w", err)
		}
	}

	fi.clockSkewOffset = offset
	fi.active[FaultClockSkew] = true
	return nil
}

// InjectWorkerCrash simulates a worker crash by releasing all workflows
// assigned to the given workerID back to the ready queue.
//
// Returns the error from the database write, and marks the fault active only if
// that write succeeded -- see InjectClockSkew for why both halves matter. This
// one is the more consequential of the two: releasing the workflows IS the
// fault, so a failed write means the crash never happened and any assertion
// about recovery is measuring an untouched system.
func (fi *FaultInjector) InjectWorkerCrash(workerID string) error {
	fi.mu.Lock()
	defer fi.mu.Unlock()

	if fi.db != nil {
		if _, err := fi.db.ExecContext(context.Background(),
			`UPDATE workflow_instances
			 SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL
			 WHERE status = 'running' AND assigned_to = $1`,
			workerID); err != nil {
			return fmt.Errorf("inject worker crash: %w", err)
		}
	}

	fi.active[FaultWorkerCrash] = true
	return nil
}

// Cleanup restores normal operation, clearing all active faults.
//
// Returns the first error from any restore write. The in-memory state is
// cleared regardless: a failed restore must not leave the injector believing
// faults are still active, but the caller does need to know the database was
// not put back -- a clock-skew restore that silently failed leaves every
// running instance with a future heartbeat_at, which the next test in the same
// database inherits as an unexplained failure.
func (fi *FaultInjector) Cleanup() error {
	fi.mu.Lock()
	defer fi.mu.Unlock()

	var firstErr error
	for ft := range fi.active {
		switch ft {
		case FaultNetworkPartition:
			if fi.networkPartitionCancel != nil {
				fi.networkPartitionCancel()
			}
		case FaultDiskSlow:
			fi.diskLatencyMin = 0
			fi.diskLatencyMax = 0
		case FaultClockSkew:
			fi.clockSkewOffset = 0
			// Restore normal heartbeat times.
			if fi.db != nil {
				if _, err := fi.db.ExecContext(context.Background(),
					`UPDATE workflow_instances SET heartbeat_at = now() WHERE heartbeat_at > now() + interval '1 minute'`); err != nil && firstErr == nil {
					firstErr = fmt.Errorf("cleanup clock skew: %w", err)
				}
			}
		case FaultWorkerCrash:
			// Already handled by InjectWorkerCrash, no cleanup needed.
		}
	}

	fi.active = make(map[FaultType]bool)
	return firstErr
}

// IsActive reports whether a particular fault type is currently active.
func (fi *FaultInjector) IsActive(ft FaultType) bool {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	return fi.active[ft]
}

// ActiveFaults returns a slice of all currently active fault types.
func (fi *FaultInjector) ActiveFaults() []FaultType {
	fi.mu.Lock()
	defer fi.mu.Unlock()

	result := make([]FaultType, 0, len(fi.active))
	for ft := range fi.active {
		result = append(result, ft)
	}
	return result
}

// applyLatencySleep sleeps for a duration within the configured latency range.
// This is a no-op if disk latency injection is not active.
func (fi *FaultInjector) applyLatencySleep() {
	fi.mu.Lock()
	min, max := fi.diskLatencyMin, fi.diskLatencyMax
	fi.mu.Unlock()

	if min > 0 && max > 0 {
		dur := min
		if max > min {
			dur += time.Duration(rand.Int63n(int64(max - min)))
		}
		time.Sleep(dur)
	}
}

// Context returns a context that is cancelled if a network partition fault
// is active. Use this to simulate connection failures in store operations.
func (fi *FaultInjector) Context(ctx context.Context) context.Context {
	fi.mu.Lock()
	defer fi.mu.Unlock()

	if fi.active[FaultNetworkPartition] && fi.networkPartitionCtx != nil {
		return fi.networkPartitionCtx
	}
	return ctx
}

// Reset clears all active faults and restores the database to normal state.
//
// Returns Cleanup's error rather than discarding it. Reset is Cleanup under
// another name, so swallowing the error here would reopen exactly what making
// Cleanup return one closed: a restore that failed silently leaves every
// running instance with a future heartbeat_at, and the next test sharing the
// database inherits it as an unexplained failure.
func (fi *FaultInjector) Reset() error {
	return fi.Cleanup()
}
