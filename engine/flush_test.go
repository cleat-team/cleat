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

// ---------------------------------------------------------------------------
// flushEvent encryption tests
// ---------------------------------------------------------------------------

// TestFlushEvent_EncryptSensitivePayloads verifies that flushEvent encrypts
// all encryptable fields when encryption is enabled and the INSERT succeeds.
func TestFlushEvent_EncryptSensitivePayloads(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	engine := NewEngine(nil, nil,
		WithDB(db),
		WithEncryption(enc, true),
	)

	rec := EventRecord{
		Step:           1,
		EventType:      EventTypeCall,
		Service:        "my-svc",
		Op:             "my-op",
		Request:        "sensitive-request",
		Response:       "sensitive-response",
		SignalPayload:  "sig-payload",
		ChildInput:     "child-input",
		NewInput:       "new-input",
		PluginInput:    "plugin-input",
		PluginOutput:   "plugin-output",
		PromiseResult:  "promise-result",
		PromiseError:   "promise-error",
	}

	err = engine.flushEvent(context.Background(), "wf-123", rec)
	if err != nil {
		t.Fatalf("flushEvent with encryption: %v", err)
	}
}

// TestFlushEvent_EncryptionRollbackOnFailure verifies that flushEvent retries
// on a transient INSERT failure when encryption is enabled, and succeeds on
// the retry.
func TestFlushEvent_EncryptionRollbackOnFailure(t *testing.T) {
	transientErr := errors.New("transient INSERT error")

	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", err: transientErr, consume: true},
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	engine := NewEngine(nil, nil,
		WithDB(db),
		WithEncryption(enc, true),
	)

	rec := EventRecord{
		Step:      1,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
		Request:   "sensitive-request",
		Response:  "sensitive-response",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = engine.flushEvent(ctx, "wf-123", rec)
	if err != nil {
		t.Fatalf("flushEvent after retry with encryption: %v", err)
	}
}

// ---------------------------------------------------------------------------
// runDefers tests
// ---------------------------------------------------------------------------

// TestRunDefers_Empty verifies that runDefers handles a nil deferrals map
// without panicking.
func TestRunDefers_Empty(t *testing.T) {
	engine := NewEngine(nil, nil)
	// should not panic with nil deferrals map
	engine.runDefers(context.Background(), nil, nil)
}

// TestRunDefers_Single verifies that runDefers processes a single deferral
// entry without error.
func TestRunDefers_Single(t *testing.T) {
	engine := NewEngine(nil, nil)
	deferrals := map[string]string{
		"defer-1": "first defer",
	}
	// wasmBytes is nil so RunDefer is not invoked; verify no panic
	engine.runDefers(context.Background(), nil, deferrals)
}

// TestRunDefers_Sorted verifies that runDefers handles multiple deferrals
// and sorts them by key without panicking.
func TestRunDefers_Sorted(t *testing.T) {
	engine := NewEngine(nil, nil)
	deferrals := map[string]string{
		"defer-3": "third defer",
		"defer-1": "first defer",
		"defer-2": "second defer",
	}
	// wasmBytes is nil so RunDefer is not invoked; verify no panic
	engine.runDefers(context.Background(), nil, deferrals)
}

// ---------------------------------------------------------------------------
// completeCallEvent additional tests
// ---------------------------------------------------------------------------

// TestCompleteCallEvent_CommitError verifies that completeCallEvent returns an
// error when the transaction commit fails.
func TestCompleteCallEvent_CommitError(t *testing.T) {
	commitErr := errors.New("commit failed")
	db := newMockDBWithErrors(t, nil, []mockExecResult{
		{match: "UPDATE event_history", affected: 1},
	}, nil, commitErr)
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
	if err == nil {
		t.Fatal("expected error from completeCallEvent when commit fails, got nil")
	}
}

// TestCompleteCallEvent_ChecksumQueryError verifies that completeCallEvent
// tolerates a checksum query error (step>1) and still completes successfully
// when the UPDATE succeeds.
func TestCompleteCallEvent_ChecksumQueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "COALESCE", err: errors.New("checksum query error")},
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
		t.Fatalf("completeCallEvent with checksum query error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// completeCallEvent encryption tests
// ---------------------------------------------------------------------------

// TestCompleteCallEvent_EncryptSuccess verifies that completeCallEvent
// encrypts the response and error fields when encryption is enabled.
func TestCompleteCallEvent_EncryptSuccess(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE event_history", affected: 1},
	})
	defer db.Close()

	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	engine := NewEngine(nil, nil,
		WithDB(db),
		WithEncryption(enc, true),
	)

	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
		Request:   `{"key":"val"}`,
		Response:  "sensitive-response",
	}

	err = engine.completeCallEvent(context.Background(), "wf-123", rec, "error-detail")
	if err != nil {
		t.Fatalf("completeCallEvent with encryption: %v", err)
	}
}

// TestCompleteCallEvent_EncryptResponseError verifies that completeCallEvent
// returns an error when encryption of the response field fails.
func TestCompleteCallEvent_EncryptResponseError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE event_history", affected: 1},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	// A nil key causes EncryptString to fail.
	engine.encryption = &PayloadEncryption{key: nil}
	engine.encryptSensitivePayloads = true

	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
		Response:  "sensitive-response",
	}

	err := engine.completeCallEvent(context.Background(), "wf-123", rec, "")
	if err == nil {
		t.Fatal("expected encryption error")
	}
	if !strings.Contains(err.Error(), "encrypt response") {
		t.Errorf("expected 'encrypt response' in error, got: %v", err)
	}
}

// TestFlushEvent_EncryptGeneralFailure verifies that flushEvent returns an
// error when encryption fails (invalid key), testing the encryption-failure
// retry/error handling path in flushEvent.
func TestFlushEvent_EncryptGeneralFailure(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	engine := NewEngine(nil, nil, WithDB(db))
	// A nil key causes EncryptString to fail on the first field (Request).
	engine.encryption = &PayloadEncryption{key: nil}
	engine.encryptSensitivePayloads = true

	rec := EventRecord{
		Step:      1,
		EventType: EventTypeCall,
		Service:   "my-svc",
		Op:        "my-op",
		Request:   "sensitive-request",
	}

	err := engine.flushEvent(context.Background(), "wf-123", rec)
	if err == nil {
		t.Fatal("expected encryption error")
	}
	if !strings.Contains(err.Error(), "encrypt request") {
		t.Errorf("expected 'encrypt request' in error, got: %v", err)
	}
}
