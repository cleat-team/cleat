// Package main implements the memory-aware concurrency controller and
// hysteresis state machine for the cleat-worker daemon. It ties together
// the OS-level memory monitor, the workflow store (for queue depth and
// memory estimates), and dynamic concurrency scaling with recovery
// backoff.
package main

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// MemoryControllerState is an immutable snapshot of the memory controller
// state, intended for metrics and healthz consumers.
type MemoryControllerState struct {
	AvailableBytes     uint64
	UsedBytes          uint64
	TotalBytes         uint64
	Pressure           float64
	DynamicConcurrency int
	ScalingPressure    float64
	QueueDepth         int64
	IsDegraded         bool
}

const (
	defaultAlpha            = 0.3 // EWMA smoothing factor
	defaultRecoveryInterval = 5 * time.Second
	defaultScalingThreshold = 50
	defaultMemoryEstimate   = 32 * 1024 * 1024 // 32 MB safe default
)

// MemoryController ties together the memory monitor, DB stats store, and
// hysteresis logic to dynamically adjust worker concurrency based on
// system memory pressure and task queue depth.
type MemoryController struct {
	monitor               *MemoryMonitor
	store                 engine.WorkflowStore
	workerID              string
	configuredConcurrency int

	softLimit float64
	hardLimit float64

	mu                 sync.RWMutex
	lastInfo           MemoryInfo
	pressure           float64
	dynamicConcurrency int
	queueDepth         int64
	scalingPressure    float64

	scalingThreshold  int64 // queue depth that saturates scaling_pressure
	recoveryAllowedAt time.Time
	recoveryInterval  time.Duration

	defEstimates map[memoryEstimateKey]float64 // (tenant, def_name) -> EWMA mean bytes

	logger *slog.Logger
}

func (c *MemoryController) log() *slog.Logger {
	if c.logger != nil {
		return c.logger
	}
	return slog.Default()
}

// NewMemoryController creates a MemoryController that reads memory via the
// given monitor, queries the store for queue depth and estimates, and
// targets the configuredConcurrency when under no pressure.
func NewMemoryController(
	monitor *MemoryMonitor,
	store engine.WorkflowStore,
	workerID string,
	configuredConcurrency int,
	softLimit, hardLimit float64,
) *MemoryController {
	return &MemoryController{
		monitor:               monitor,
		store:                 store,
		workerID:              workerID,
		configuredConcurrency: configuredConcurrency,
		softLimit:             softLimit,
		hardLimit:             hardLimit,
		dynamicConcurrency:    configuredConcurrency,
		scalingThreshold:      defaultScalingThreshold,
		recoveryInterval:      defaultRecoveryInterval,
		defEstimates:          make(map[memoryEstimateKey]float64),
	}
}

// LoadEstimates loads per-definition memory estimates from the workflow store
// into the controller's in-memory EWMA map, under the tenant that store was
// opened as.
//
// tenantID is a parameter rather than read from the store because
// engine.WorkflowStore exposes no TenantID accessor, and adding one is public
// surface. The caller built the store and knows: both call sites pass the
// worker's storeTenantID.
//
// A LIMITATION THIS MAKES VISIBLE RATHER THAN INTRODUCES. LoadMemoryEstimates
// is already tenant-scoped -- `WHERE tenant_id = $1` against s.tenantID -- so
// this only ever warms ONE tenant's estimates, the default tenant the worker's
// store is opened as. Before cleat#1097 that was invisible: the rows landed in
// a map keyed by def_name, so they silently became every tenant's starting
// point. Now they are confined to the tenant they belong to, and another
// tenant simply starts cold and learns from its own samples.
func (c *MemoryController) LoadEstimates(ctx context.Context, tenantID string) error {
	estimates, err := c.store.LoadMemoryEstimates(ctx)
	if err != nil {
		return err
	}
	keyed := make(map[memoryEstimateKey]float64, len(estimates))
	for defName, mean := range estimates {
		keyed[memoryEstimateKey{tenantID: tenantID, defName: defName}] = mean
	}
	c.mu.Lock()
	c.defEstimates = keyed
	c.mu.Unlock()
	return nil
}

// Tick performs one iteration of the control loop. It should be called
// each dispatch loop iteration.
//
//  1. Read current memory stats via the monitor (cached within interval)
//  2. Compute memory pressure from the reading
//  3. Sample the store's queue depth
//  4. Compute composite scaling pressure (pressure * queue_depth / threshold)
//  5. Adjust dynamic concurrency with hysteresis and recovery backoff
func (c *MemoryController) Tick(ctx context.Context) {
	info := c.monitor.Read()
	p := PressureLevel(info, c.softLimit, c.hardLimit)

	qd, err := c.store.QueueDepth(ctx)
	if err != nil {
		c.log().WarnContext(context.Background(), "queue depth query failed", "worker_id", c.workerID, "error", err)
		qd = 0
	}

	qdFrac := min(float64(qd)/float64(c.scalingThreshold), 1.0)
	sp := p * qdFrac

	c.mu.Lock()

	// Adjust dynamic concurrency with hysteresis.
	switch {
	case p >= 1.0:
		c.dynamicConcurrency = 0
	case p > 0:
		target := int(math.Round(float64(c.configuredConcurrency) * (1.0 - p)))
		if target < 0 {
			target = 0
		}
		c.dynamicConcurrency = target
	default:
		// p == 0: recover toward configuredConcurrency with backoff.
		if c.dynamicConcurrency < c.configuredConcurrency && time.Now().After(c.recoveryAllowedAt) {
			c.dynamicConcurrency++
			c.recoveryAllowedAt = time.Now().Add(c.recoveryInterval)
		}
	}

	c.lastInfo = info
	c.pressure = p
	c.queueDepth = qd
	c.scalingPressure = sp

	c.mu.Unlock()
}

// CanClaim returns true if the controller allows claiming new workflows.
// Returns false when dynamicConcurrency reaches zero (hard limit).
func (c *MemoryController) CanClaim() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dynamicConcurrency > 0
}

// CanAcceptAPIWorkflows returns true if the controller allows accepting
// new workflow starts via the API. Returns false at hard memory pressure.
func (c *MemoryController) CanAcceptAPIWorkflows() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pressure < 1.0
}

// RecordWorkflowMemory updates the in-memory EWMA estimate for defName
// with the observed deltaBytes and asynchronously persists the sample
// through persistStore.
//
// persistStore is the TENANT-SCOPED store for the workflow that produced the
// sample, not the controller's own. The controller holds exactly one store --
// the worker's, opened as storeTenantID -- while a worker executes workflows
// for any tenant it can claim, so persisting through c.store would attribute
// every tenant's sample to the worker's own tenant.
//
// That distinction is invisible to a store-level test: engine's
// TestTheMemoryProfileIsScopedToTenant drives two per-tenant stores directly
// and passes whether or not this parameter exists. It is the "watch which
// layer is holding the test up" case from CLAUDE.md -- the scoping the test
// observes is the store's, and the routing that decides WHICH store is here.
// See cleat#1040.
//
// A nil persistStore falls back to c.store, which is correct for the
// single-tenant case and for callers that have no workflow in hand.
func (c *MemoryController) RecordWorkflowMemory(ctx context.Context, persistStore engine.WorkflowStore, tenantID, defName string, deltaBytes uint64) {
	key := memoryEstimateKey{tenantID: tenantID, defName: defName}
	c.mu.Lock()
	prev, exists := c.defEstimates[key]
	if !exists {
		c.defEstimates[key] = float64(deltaBytes)
	} else {
		c.defEstimates[key] = defaultAlpha*float64(deltaBytes) + (1-defaultAlpha)*prev
	}
	c.mu.Unlock()

	target := persistStore
	if target == nil {
		target = c.store
	}

	// Async persist to DB; don't fail the workflow if stats recording fails.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := target.RecordWorkflowMemorySample(ctx, defName, int64(deltaBytes)); err != nil {
			c.log().WarnContext(context.Background(), "record memory sample failed", "worker_id", c.workerID, "workflow", defName, "error", err)
		}
	}()
}

// WorkflowMemoryEstimate returns the EWMA memory estimate for one tenant's
// workflow definition, or a safe default (32 MB) if no estimate exists.
//
// NO PRODUCTION CALLER, and I nearly deleted it on that basis. An
// exact-identifier search finds none -- a substring grep is misleading here,
// because it matches the unrelated Metrics.RecordWorkflowMemoryEstimate. But
// that search was scoped to non-test files, and its tests are real callers
// asserting a real contract: the 32 MB fallback for an unknown definition.
//
// Kept and re-keyed rather than deleted, because deleting it would have meant
// deleting the tests that document that fallback, and a tenant parameter costs
// nothing here. cleat#1097.
func (c *MemoryController) WorkflowMemoryEstimate(tenantID, defName string) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	est, ok := c.defEstimates[memoryEstimateKey{tenantID: tenantID, defName: defName}]
	if !ok || est <= 0 {
		return defaultMemoryEstimate
	}
	return uint64(est)
}

// DynamicConcurrency returns the current effective concurrency cap.
func (c *MemoryController) DynamicConcurrency() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dynamicConcurrency
}

// Pressure returns the current memory pressure value (0.0-1.0).
func (c *MemoryController) Pressure() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pressure
}

// State returns an immutable snapshot of the controller state.
func (c *MemoryController) State() MemoryControllerState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return MemoryControllerState{
		AvailableBytes:     c.lastInfo.AvailableBytes,
		UsedBytes:          c.lastInfo.UsedBytes,
		TotalBytes:         c.lastInfo.TotalBytes,
		Pressure:           c.pressure,
		DynamicConcurrency: c.dynamicConcurrency,
		ScalingPressure:    c.scalingPressure,
		QueueDepth:         c.queueDepth,
		IsDegraded:         c.pressure >= 1.0,
	}
}

// memoryEstimateKey scopes a memory estimate to the tenant that produced it.
//
// cleat#1097: defEstimates was keyed by def_name alone, so two tenants running
// a same-named workflow fed one EWMA. cleat#1040 fixed the PERSISTED half --
// workflow_memory_stats carries tenant_id and the store is per-tenant -- and
// this in-memory half was left behind, which is why the database could be right
// and the gauge wrong at the same time.
type memoryEstimateKey struct {
	tenantID string
	defName  string
}

// DefEstimates returns a copy of the per-tenant, per-definition estimate map.
func (c *MemoryController) DefEstimates() map[memoryEstimateKey]float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cp := make(map[memoryEstimateKey]float64, len(c.defEstimates))
	for k, v := range c.defEstimates {
		cp[k] = v
	}
	return cp
}
