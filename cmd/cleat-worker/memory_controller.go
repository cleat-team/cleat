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

	defEstimates map[string]float64 // def_name -> EWMA mean bytes

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
		defEstimates:          make(map[string]float64),
	}
}

// LoadEstimates loads per-definition memory estimates from the workflow
// store into the controller's in-memory EWMA map.
func (c *MemoryController) LoadEstimates(ctx context.Context) error {
	estimates, err := c.store.LoadMemoryEstimates(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.defEstimates = estimates
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
// to the workflow store.
func (c *MemoryController) RecordWorkflowMemory(ctx context.Context, defName string, deltaBytes uint64) {
	c.mu.Lock()
	prev, exists := c.defEstimates[defName]
	if !exists {
		c.defEstimates[defName] = float64(deltaBytes)
	} else {
		c.defEstimates[defName] = defaultAlpha*float64(deltaBytes) + (1-defaultAlpha)*prev
	}
	c.mu.Unlock()

	// Async persist to DB; don't fail the workflow if stats recording fails.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.store.RecordWorkflowMemorySample(ctx, defName, int64(deltaBytes)); err != nil {
			c.log().WarnContext(context.Background(), "record memory sample failed", "worker_id", c.workerID, "workflow", defName, "error", err)
		}
	}()
}

// WorkflowMemoryEstimate returns the EWMA memory estimate for a workflow
// definition, or a safe default (32 MB) if no estimate exists.
func (c *MemoryController) WorkflowMemoryEstimate(defName string) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	est, ok := c.defEstimates[defName]
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

// DefEstimates returns a copy of the per-definition memory estimate map.
func (c *MemoryController) DefEstimates() map[string]float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cp := make(map[string]float64, len(c.defEstimates))
	for k, v := range c.defEstimates {
		cp[k] = v
	}
	return cp
}
