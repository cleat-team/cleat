package main

import (
	"context"
	"testing"
	"time"
)

func newTestMonitor() *MemoryMonitor {
	return &MemoryMonitor{
		readFn: func() (MemoryInfo, error) {
			return MemoryInfo{TotalBytes: 1000000, UsedBytes: 500000, AvailableBytes: 500000, Source: "test", CollectedAt: time.Now()}, nil
		},
	}
}

func newTestController(monitor *MemoryMonitor, concurrency int, soft, hard float64) *MemoryController {
	return &MemoryController{
		monitor:               monitor,
		store:                 &mockStore{},
		workerID:              "test",
		configuredConcurrency: concurrency,
		softLimit:             soft,
		hardLimit:             hard,
		dynamicConcurrency:    concurrency,
		scalingThreshold:      defaultScalingThreshold,
		recoveryInterval:      5 * time.Millisecond,
		defEstimates:          make(map[string]float64),
	}
}

func TestController_Healthy(t *testing.T) {
	mc := newTestController(newTestMonitor(), 10, 0.80, 0.95)
	if !mc.CanClaim() || mc.DynamicConcurrency() != 10 || mc.Pressure() != 0 || mc.State().IsDegraded {
		t.Error("expected healthy state")
	}
}

func TestController_HardPressure(t *testing.T) {
	mc := newTestController(newTestMonitor(), 10, 0.80, 0.95)
	mc.monitor.readFn = func() (MemoryInfo, error) {
		return MemoryInfo{TotalBytes: 1000000, UsedBytes: 970000, AvailableBytes: 30000, Source: "test", CollectedAt: time.Now()}, nil
	}
	mc.Tick(context.Background())
	if mc.CanClaim() || mc.DynamicConcurrency() != 0 || mc.Pressure() < 1.0 || !mc.State().IsDegraded {
		t.Error("expected hard-pressure state")
	}
}

func TestController_SoftPressure(t *testing.T) {
	mc := newTestController(newTestMonitor(), 10, 0.80, 0.95)
	mc.monitor.readFn = func() (MemoryInfo, error) {
		return MemoryInfo{TotalBytes: 1000000, UsedBytes: 875000, AvailableBytes: 125000, Source: "test", CollectedAt: time.Now()}, nil
	}
	mc.Tick(context.Background())
	if mc.DynamicConcurrency() < 3 || mc.DynamicConcurrency() > 7 {
		t.Errorf("expected ~5 concurrency, got %d", mc.DynamicConcurrency())
	}
}

func TestController_ScalingPressure(t *testing.T) {
	mc := newTestController(newTestMonitor(), 10, 0.80, 0.95)
	mc.monitor.readFn = func() (MemoryInfo, error) {
		return MemoryInfo{TotalBytes: 1000000, UsedBytes: 875000, AvailableBytes: 125000, Source: "test", CollectedAt: time.Now()}, nil
	}
	mc.Tick(context.Background())
	sp := mc.State().ScalingPressure
	if sp < 0.4 || sp > 0.6 {
		t.Errorf("expected ~0.5 scaling pressure, got %f", sp)
	}
}

func TestController_ScalingPressureZero(t *testing.T) {
	mc := newTestController(newTestMonitor(), 10, 0.80, 0.95)
	mc.Tick(context.Background())
	if mc.State().ScalingPressure != 0.0 {
		t.Error("expected 0 scaling pressure when healthy")
	}
}

func TestController_Recovery(t *testing.T) {
	mc := newTestController(newTestMonitor(), 5, 0.80, 0.95)
	mc.monitor.readFn = func() (MemoryInfo, error) {
		return MemoryInfo{TotalBytes: 1000000, UsedBytes: 970000, AvailableBytes: 30000, Source: "test", CollectedAt: time.Now()}, nil
	}
	mc.Tick(context.Background())
	if mc.DynamicConcurrency() != 0 {
		t.Fatal("expected 0 under hard pressure")
	}
	mc.monitor.readFn = func() (MemoryInfo, error) {
		return MemoryInfo{TotalBytes: 1000000, UsedBytes: 500000, AvailableBytes: 500000, Source: "test", CollectedAt: time.Now()}, nil
	}
	mc.Tick(context.Background())
	if mc.DynamicConcurrency() != 1 {
		t.Errorf("expected 1 after recovery, got %d", mc.DynamicConcurrency())
	}
}

func TestController_RecordWorkflowMemory(t *testing.T) {
	mc := newTestController(newTestMonitor(), 10, 0.80, 0.95)
	mc.RecordWorkflowMemory(context.Background(), "wf-a", 50*1024*1024)
	if mc.WorkflowMemoryEstimate("wf-a") != 50*1024*1024 {
		t.Errorf("expected 50MB first estimate")
	}
	mc.RecordWorkflowMemory(context.Background(), "wf-a", 100*1024*1024)
	expected := uint64(0.3*100*1024*1024 + 0.7*50*1024*1024)
	if mc.WorkflowMemoryEstimate("wf-a") != expected {
		t.Errorf("expected EWMA=%d, got %d", expected, mc.WorkflowMemoryEstimate("wf-a"))
	}
}

func TestController_DefaultEstimate(t *testing.T) {
	mc := newTestController(newTestMonitor(), 10, 0.80, 0.95)
	if mc.WorkflowMemoryEstimate("unknown") != defaultMemoryEstimate {
		t.Error("expected default estimate for unknown def")
	}
}

func TestController_LoadEstimates(t *testing.T) {
	mc := newTestController(newTestMonitor(), 10, 0.80, 0.95)
	if err := mc.LoadEstimates(context.Background()); err != nil {
		t.Fatalf("LoadEstimates: %v", err)
	}
}
