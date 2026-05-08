package main

import "testing"

func TestPressureLevel_ZeroWhenBelowSoft(t *testing.T) {
	info := MemoryInfo{TotalBytes: 1000, UsedBytes: 500, AvailableBytes: 500}
	if p := PressureLevel(info, 0.80, 0.95); p != 0.0 {
		t.Errorf("expected 0 at 50%%, got %f", p)
	}
}

func TestPressureLevel_OneWhenAboveHard(t *testing.T) {
	info := MemoryInfo{TotalBytes: 1000, UsedBytes: 970, AvailableBytes: 30}
	if p := PressureLevel(info, 0.80, 0.95); p != 1.0 {
		t.Errorf("expected 1.0 at 97%%, got %f", p)
	}
}

func TestPressureLevel_LinearRamp(t *testing.T) {
	info := MemoryInfo{TotalBytes: 1000, UsedBytes: 875, AvailableBytes: 125}
	p := PressureLevel(info, 0.80, 0.95)
	if p < 0.49 || p > 0.51 {
		t.Errorf("expected ~0.5 at 87.5%%, got %f", p)
	}
}

func TestPressureLevel_AtBoundaries(t *testing.T) {
	if p := PressureLevel(MemoryInfo{TotalBytes: 1000, UsedBytes: 800, AvailableBytes: 200}, 0.80, 0.95); p != 0.0 {
		t.Error("expected 0 at soft boundary")
	}
	if p := PressureLevel(MemoryInfo{TotalBytes: 1000, UsedBytes: 950, AvailableBytes: 50}, 0.80, 0.95); p != 1.0 {
		t.Error("expected 1.0 at hard boundary")
	}
}

func TestPressureLevel_UsesAvailableBytes(t *testing.T) {
	info := MemoryInfo{TotalBytes: 1000, UsedBytes: 999, AvailableBytes: 200}
	if p := PressureLevel(info, 0.80, 0.95); p != 0.0 {
		t.Errorf("expected 0 when available-based, got %f", p)
	}
}

func TestPressureLevel_NoAvailableBytes(t *testing.T) {
	info := MemoryInfo{TotalBytes: 1000, UsedBytes: 900, AvailableBytes: 0}
	p := PressureLevel(info, 0.80, 0.95)
	if p < 0.66 || p > 0.67 {
		t.Errorf("expected ~0.667, got %f", p)
	}
}
