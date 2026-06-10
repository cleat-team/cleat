package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

type acquireErrorStore struct {
	mockConcurrencyKeyStore
}

func (a *acquireErrorStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	return false, errors.New("store error")
}

type acquireNotAcquiredStore struct{}

func (a *acquireNotAcquiredStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	return false, nil
}

func (a *acquireNotAcquiredStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	return nil
}

func TestFreshAcquireLock_Success(t *testing.T) {
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = newMockConcurrencyKeyStore()

	result := s.freshAcquireLock(context.Background(), nil, "my-lock", 60000)

	acquired := (result>>8)&1 != 0
	errCode := byte(result & 0xFF)
	if !acquired {
		t.Error("expected lock acquired")
	}
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeAcquireLock {
		t.Errorf("expected EventTypeAcquireLock, got %q", s.history[0].EventType)
	}
}

func TestFreshAcquireLock_StoreError(t *testing.T) {
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = &acquireErrorStore{}

	result := s.freshAcquireLock(context.Background(), nil, "my-lock", 60000)

	acquired := (result>>8)&1 != 0
	if acquired {
		t.Error("expected lock not acquired on store error")
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].Err == "" {
		t.Error("expected error message in history")
	}
}

func TestFreshAcquireLock_AlreadyLocked(t *testing.T) {
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = &acquireNotAcquiredStore{}

	result := s.freshAcquireLock(context.Background(), nil, "my-lock", 60000)

	acquired := (result>>8)&1 != 0
	if acquired {
		t.Error("expected lock not acquired (already locked)")
	}
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].LockAcquired != 0 {
		t.Errorf("expected LockAcquired 0, got %d", s.history[0].LockAcquired)
	}
}

func TestFreshAcquireLock_NoStore(t *testing.T) {
	s := newTestExecSession()

	result := s.freshAcquireLock(context.Background(), nil, "my-lock", 60000)

	acquired := (result>>8)&1 != 0
	if acquired {
		t.Error("expected lock not acquired when no store")
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
}

type releaseLockErrorStore struct{}

func (r *releaseLockErrorStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	return false, nil
}

func (r *releaseLockErrorStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	return errors.New("release failed")
}

func TestFreshReleaseLock_Success(t *testing.T) {
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = newMockConcurrencyKeyStore()

	result := s.freshReleaseLock(context.Background(), nil, "my-lock")

	if result != 0 {
		t.Errorf("expected 0 for successful release, got %d", result)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeReleaseLock {
		t.Errorf("expected EventTypeReleaseLock, got %q", s.history[0].EventType)
	}
}

func TestFreshReleaseLock_StoreError(t *testing.T) {
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = &releaseLockErrorStore{}

	result := s.freshReleaseLock(context.Background(), nil, "my-lock")

	if result != 1 {
		t.Errorf("expected 1 for failed release, got %d", result)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].Err == "" {
		t.Error("expected error message in history entry")
	}
}

func TestFreshReleaseLock_NoStore(t *testing.T) {
	s := newTestExecSession()

	result := s.freshReleaseLock(context.Background(), nil, "my-lock")

	if result != 0 {
		t.Errorf("expected 0 for release with no store, got %d", result)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
}

func TestAcquireLock_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeAcquireLock,
		LockKey: "my-lock", LockTTLMs: 60000, LockAcquired: 1,
	}}

	result := s.AcquireLock(context.Background(), nil, "my-lock", 60000)

	acquired := (result>>8)&1 != 0
	errCode := byte(result & 0xFF)
	if !acquired {
		t.Error("expected lock acquired from replay")
	}
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
}

func TestAcquireLock_ReplayWithError(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeAcquireLock,
		LockKey: "my-lock", LockTTLMs: 60000, LockAcquired: 0,
		Err: "previous error",
	}}

	result := s.AcquireLock(context.Background(), nil, "my-lock", 60000)

	acquired := (result>>8)&1 != 0
	if acquired {
		t.Error("expected lock not acquired (replayed error)")
	}
	errCode := byte(result & 0xFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
}

func TestAcquireLock_ReplayWrongEventType(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeCall,
	}}

	result := s.AcquireLock(context.Background(), nil, "my-lock", 60000)

	acquired := (result>>8)&1 != 0
	if acquired {
		t.Error("expected lock not acquired (replay divergence)")
	}
	errCode := byte(result & 0xFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
}

func TestAcquireLock_ReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = newMockConcurrencyKeyStore()
	s.isReplay = true
	s.history = nil

	result := s.AcquireLock(context.Background(), nil, "my-lock", 60000)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	acquired := (result>>8)&1 != 0
	if !acquired {
		t.Error("expected lock acquired (fresh path after exitReplay)")
	}
}

func TestReleaseLock_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeReleaseLock,
		LockKey: "my-lock",
	}}

	result := s.ReleaseLock(context.Background(), nil, "my-lock")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestReleaseLock_ReplayWithError(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeReleaseLock,
		LockKey: "my-lock", Err: "previous error",
	}}

	result := s.ReleaseLock(context.Background(), nil, "my-lock")

	if result != 1 {
		t.Errorf("expected 1 (replayed error), got %d", result)
	}
}

func TestReleaseLock_ReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = newMockConcurrencyKeyStore()
	s.isReplay = true
	s.history = nil

	result := s.ReleaseLock(context.Background(), nil, "my-lock")

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}
