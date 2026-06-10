package engine

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

type driverValue = driver.Value

// ---------------------------------------------------------------------------
// flushEvent tests
// ---------------------------------------------------------------------------

// TestFlushEvent_Success verifies that flushEvent successfully writes an event.
func TestFlushEvent_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
		Request:   `{"key":"val"}`,
		Response:  `{"result":"ok"}`,
	}

	err := engine.flushEvent(context.Background(), "wf-123", rec)
	if err != nil {
		t.Fatalf("flushEvent: %v", err)
	}
}

// TestFlushEvent_Success_WithChecksum verifies flushEvent succeeds when step > 1,
// which triggers a checksum lookup query.
func TestFlushEvent_Success_WithChecksum(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "COALESCE", data: [][]driverValue{{"prev-checksum"}}},
	}, []mockExecResult{
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      5,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.flushEvent(context.Background(), "wf-123", rec)
	if err != nil {
		t.Fatalf("flushEvent: %v", err)
	}
}

// TestFlushEvent_NilDB verifies that flushEvent returns nil when db is not set.
func TestFlushEvent_NilDB(t *testing.T) {
	engine := NewEngine(nil, nil)
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.flushEvent(context.Background(), "wf-123", rec)
	if err != nil {
		t.Fatalf("flushEvent with nil db: %v", err)
	}
}

// TestFlushEvent_RetryThenSuccess verifies that flushEvent retries on transient
// failures and succeeds when the database eventually accepts the write.
func TestFlushEvent_RetryThenSuccess(t *testing.T) {
	transientErr := errors.New("transient database error")

	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", err: transientErr, consume: true},
		{match: "INSERT INTO event_history", err: transientErr, consume: true},
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	// Set a deadline so the test doesn't hang if retries fail.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := engine.flushEvent(ctx, "wf-123", rec)
	if err != nil {
		t.Fatalf("flushEvent after retries: %v", err)
	}
}

// TestFlushEvent_AllRetriesExhausted verifies that flushEvent returns an error
// when all retry attempts fail.
func TestFlushEvent_AllRetriesExhausted(t *testing.T) {
	persistentErr := errors.New("persistent database error")

	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", err: persistentErr},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := engine.flushEvent(ctx, "wf-123", rec)
	if err == nil {
		t.Fatal("expected error from flushEvent after retries exhausted, got nil")
	}
}

// TestFlushEvent_ContextCancelled verifies that flushEvent returns early when
// the context is cancelled during backoff.
func TestFlushEvent_ContextCancelled(t *testing.T) {
	transientErr := errors.New("transient error")

	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", err: transientErr},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	// Use an already-cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := engine.flushEvent(ctx, "wf-123", rec)
	if err == nil {
		t.Fatal("expected error from flushEvent with cancelled context, got nil")
	}
}

// ---------------------------------------------------------------------------
// flushCallIntent tests
// ---------------------------------------------------------------------------

// TestFlushCallIntent_Success verifies that flushCallIntent inserts a pending
// event successfully.
func TestFlushCallIntent_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
		Request:   `{"key":"val"}`,
	}

	err := engine.flushCallIntent(context.Background(), "wf-123", rec)
	if err != nil {
		t.Fatalf("flushCallIntent: %v", err)
	}
}

// TestFlushCallIntent_Success_WithChecksum verifies flushCallIntent with step > 1.
func TestFlushCallIntent_Success_WithChecksum(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "COALESCE", data: [][]driverValue{{"prev-checksum"}}},
	}, []mockExecResult{
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      5,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.flushCallIntent(context.Background(), "wf-123", rec)
	if err != nil {
		t.Fatalf("flushCallIntent: %v", err)
	}
}

// TestFlushCallIntent_NilDB verifies that flushCallIntent returns nil when db
// is not set.
func TestFlushCallIntent_NilDB(t *testing.T) {
	engine := NewEngine(nil, nil)
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.flushCallIntent(context.Background(), "wf-123", rec)
	if err != nil {
		t.Fatalf("flushCallIntent with nil db: %v", err)
	}
}

// TestFlushCallIntent_ExecError verifies that flushCallIntent returns an error
// when the INSERT fails.
func TestFlushCallIntent_ExecError(t *testing.T) {
	execErr := errors.New("exec failed")
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", err: execErr},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.flushCallIntent(context.Background(), "wf-123", rec)
	if err == nil {
		t.Fatal("expected error from flushCallIntent when exec fails, got nil")
	}
}

// TestFlushCallIntent_BeginError verifies that flushCallIntent returns an error
// when the transaction cannot be started.
func TestFlushCallIntent_BeginError(t *testing.T) {
	beginErr := errors.New("begin failed")
	db := newMockDBWithErrors(t, nil, nil, beginErr, nil)
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.flushCallIntent(context.Background(), "wf-123", rec)
	if err == nil {
		t.Fatal("expected error from flushCallIntent when begin fails, got nil")
	}
}

// ---------------------------------------------------------------------------
// completeCallEvent tests
// ---------------------------------------------------------------------------

// TestCompleteCallEvent_Success verifies that completeCallEvent updates a pending
// event with the actual response.
func TestCompleteCallEvent_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE event_history", affected: 1},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
		Request:   `{"key":"val"}`,
		Response:  `{"result":"ok"}`,
	}

	err := engine.completeCallEvent(context.Background(), "wf-123", rec, "")
	if err != nil {
		t.Fatalf("completeCallEvent: %v", err)
	}
}

// TestCompleteCallEvent_NilDB verifies that completeCallEvent returns nil when
// db is not set.
func TestCompleteCallEvent_NilDB(t *testing.T) {
	engine := NewEngine(nil, nil)
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.completeCallEvent(context.Background(), "wf-123", rec, "")
	if err != nil {
		t.Fatalf("completeCallEvent with nil db: %v", err)
	}
}

// TestCompleteCallEvent_WithError verifies that completeCallEvent records an
// error response.
func TestCompleteCallEvent_WithError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE event_history", affected: 1},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
		Request:   `{"key":"val"}`,
	}

	err := engine.completeCallEvent(context.Background(), "wf-123", rec, "something went wrong")
	if err != nil {
		t.Fatalf("completeCallEvent with error: %v", err)
	}
}

// TestCompleteCallEvent_NoRowsUpdated verifies that completeCallEvent returns
// an error when no rows are affected by the UPDATE.
func TestCompleteCallEvent_NoRowsUpdated(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE event_history", affected: 0},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.completeCallEvent(context.Background(), "wf-123", rec, "")
	if err == nil {
		t.Fatal("expected error when no rows updated, got nil")
	}
}

// TestCompleteCallEvent_BeginError verifies that completeCallEvent returns an
// error when the transaction cannot be started.
func TestCompleteCallEvent_BeginError(t *testing.T) {
	beginErr := errors.New("begin failed")
	db := newMockDBWithErrors(t, nil, nil, beginErr, nil)
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.completeCallEvent(context.Background(), "wf-123", rec, "")
	if err == nil {
		t.Fatal("expected error from completeCallEvent when begin fails, got nil")
	}
}

func TestFlushEvent_BeginTxError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.flushEvent(context.Background(), "wf-123", rec)
	if err == nil {
		t.Fatal("expected error from flushEvent when begin fails, got nil")
	}
}

func TestFlushEvent_EncryptionError(t *testing.T) {
	// Use a short key that will fail to initialize encryption.
	// This means flushEvent will not encrypt (encryption == nil), so
	// we test the encryptSensitivePayloads=true path with a nil encryption.
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	engine := NewEngine(nil, nil,
		WithDB(db),
	)
	// Set encryption flag without an actual encryption instance.
	// With encryptSensitivePayloads=true and encryption=nil, the encryption
	// block in flushEvent is skipped (nil check first), so it succeeds.
	engine.encryptSensitivePayloads = true
	engine.encryption = nil

	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
		Request:   `{"key":"val"}`,
		Response:  `{"result":"ok"}`,
	}

	err := engine.flushEvent(context.Background(), "wf-123", rec)
	if err != nil {
		t.Fatalf("flushEvent with encryptSensitivePayloads=true and nil encryption: %v", err)
	}
}

func TestFlushEvent_QuotaExceeded(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "event_count", data: [][]driverValue{{int64(100)}}},
	}, nil)
	defer db.Close()

	engine := NewEngine(nil, nil,
		WithDB(db),
		WithMaxQuotaEvents(50),
		WithWorkflowStore(&stubWorkflowStore{}),
	)

	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.flushEvent(context.Background(), "wf-123", rec)
	if err == nil {
		t.Fatal("expected error from flushEvent when quota exceeded, got nil")
	}
	if !strings.Contains(err.Error(), "event quota exceeded") {
		t.Errorf("expected 'event quota exceeded' error, got: %v", err)
	}
}

func TestFlushEvent_QuotaCheckError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "event_count", err: errors.New("quota query failed")},
	}, nil)
	defer db.Close()

	engine := NewEngine(nil, nil,
		WithDB(db),
		WithMaxQuotaEvents(100),
		WithWorkflowStore(&stubWorkflowStore{}),
	)

	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.flushEvent(context.Background(), "wf-123", rec)
	if err == nil {
		t.Fatal("expected error from flushEvent when quota check fails, got nil")
	}
}

func TestCompleteCallEvent_SuccessWithChecksum(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "COALESCE", data: [][]driverValue{{"prev-checksum"}}},
	}, []mockExecResult{
		{match: "UPDATE event_history", affected: 1},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      5,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
		Request:   `{"key":"val"}`,
		Response:  `{"result":"ok"}`,
	}

	err := engine.completeCallEvent(context.Background(), "wf-123", rec, "")
	if err != nil {
		t.Fatalf("completeCallEvent: %v", err)
	}
}

func TestCompleteCallEvent_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE event_history", err: errors.New("exec failed")},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.completeCallEvent(context.Background(), "wf-123", rec, "")
	if err == nil {
		t.Fatal("expected error when exec fails, got nil")
	}
}

func TestCompleteCallEvent_RowsAffectedError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE event_history", affected: 0},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
	}

	err := engine.completeCallEvent(context.Background(), "wf-123", rec, "")
	if err == nil {
		t.Fatal("expected error when no rows updated, got nil")
	}
	if !strings.Contains(err.Error(), "no rows updated") {
		t.Errorf("expected 'no rows updated' error, got: %v", err)
	}
}
