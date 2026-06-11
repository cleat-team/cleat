package engine

import (
	"sync/atomic"
	"testing"
)

func TestReplayStepCount(t *testing.T) {
	orig := atomic.LoadInt64(&replayStepCount)
	defer atomic.StoreInt64(&replayStepCount, orig)

	atomic.StoreInt64(&replayStepCount, 42)
	if got := ReplayStepCount(); got != 42 {
		t.Errorf("ReplayStepCount() = %d, want 42", got)
	}

	atomic.StoreInt64(&replayStepCount, 0)
	if got := ReplayStepCount(); got != 0 {
		t.Errorf("ReplayStepCount() = %d, want 0", got)
	}
}

func TestFreshStepCount(t *testing.T) {
	orig := atomic.LoadInt64(&freshStepCount)
	defer atomic.StoreInt64(&freshStepCount, orig)

	atomic.StoreInt64(&freshStepCount, 99)
	if got := FreshStepCount(); got != 99 {
		t.Errorf("FreshStepCount() = %d, want 99", got)
	}

	atomic.StoreInt64(&freshStepCount, 7)
	if got := FreshStepCount(); got != 7 {
		t.Errorf("FreshStepCount() = %d, want 7", got)
	}
}

