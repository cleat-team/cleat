package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// THE HELPER'S TESTS DO NOT PROVE THE CLAIM CALLS IT. logClaimKeyDecision is
// exercised directly in the_claim_logs_its_key_decision_test.go, and a call site
// that stopped calling it would leave every one of those tests green -- the
// caller drifting away from a well-tested callee. This drives the real
// ClaimWorkflows against a real database instead, so the wiring is the thing
// under test.
//
// It runs on every registered backend, which is what makes it worth writing:
// the decision is logged from three separate dialect implementations
// (store_lifecycle.go, mysql_lifecycle.go, mssql_lifecycle.go) and a helper
// wired into two of the three would otherwise be invisible.
//
// The scenario is cleat#1955's own shape: a queue of limit 1 with two runs
// waiting, so one is admitted and one refused. That pair is exactly what the
// failing nightly's artifact could not distinguish from two unrelated keys.
func TestTheClaimActuallyLogsItsKeyDecision(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			var buf syncBuffer
			logged := withStoreLogger(t, store, slog.New(slog.NewJSONHandler(&buf,
				&slog.HandlerOptions{Level: slog.LevelInfo})))

			name := queueTestName("wired")
			db := queueClaimTestDB(t, store)
			if err := NewQueueStore(db, backend.Name()).CreateQueue(ctx, DefaultTenantUUID, name, 1, nil, nil); err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}

			starter, ok := logged.(claimQueueConcurrencyKeyStarter)
			if !ok {
				t.Fatalf("%T cannot record a concurrency key", logged)
			}
			for i := 0; i < 2; i++ {
				if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
					fmt.Sprintf("wired-%d", i), "test-workflow", 1, json.RawMessage(`{}`),
					"", DefaultTenantUUID, 0, name); err != nil {
					t.Fatalf("start run %d: %v", i, err)
				}
			}

			claimed, err := logged.ClaimWorkflows(ctx, "worker-1", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			// The behaviour this log describes, asserted too: a limit of 1 admits
			// one. Without this the log could faithfully describe a broken claim.
			if len(claimed) != 1 {
				t.Fatalf("claimed %d, want 1 (the declared queue limit)", len(claimed))
			}

			var admits, refusals int
			for _, m := range decodeLogLines(t, buf.String()) {
				if m["msg"] != "concurrency key decision" {
					continue
				}
				if got := m["concurrency_key"]; got != name {
					t.Errorf("decision line names key %v, want %q", got, name)
				}
				if got := m["registered"]; got != true {
					t.Errorf("decision line says registered=%v for a queue created via CreateQueue", got)
				}
				if m["admitted"] == true {
					admits++
				} else {
					refusals++
				}
			}

			if admits != 1 {
				t.Errorf("log recorded %d admission(s), want 1.\n"+
					"Zero means ClaimWorkflows on %s no longer calls logClaimKeyDecision -- the "+
					"helper's own tests cannot see that.", admits, backend.Name())
			}
			// The refusal is the half that says the queue was AT CAPACITY rather
			// than idle, which is the question cleat#1955 could not answer.
			if refusals != 1 {
				t.Errorf("log recorded %d refusal(s), want 1; an admissions-only log cannot "+
					"distinguish a semaphore at its limit from runs that never overlapped", refusals)
			}
		})
	}
}

// withStoreLogger attaches a logger to whichever concrete store the backend
// built. Returned as a WorkflowStore so the test drives the same interface
// production does.
func withStoreLogger(t *testing.T, store WorkflowStore, l *slog.Logger) WorkflowStore {
	t.Helper()
	switch s := store.(type) {
	case *PostgresStore:
		return s.WithLogger(l)
	case *MySQLStore:
		return s.WithLogger(l)
	case *MSSQLStore:
		return s.WithLogger(l)
	default:
		// Not a skip: a new store type reaching this test silently unlogged is
		// the condition the test exists to prevent.
		t.Fatalf("%T has no WithLogger; the claim decision log would be invisible on it", store)
		return nil
	}
}

// syncBuffer is a bytes.Buffer safe for a logger writing from another goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// decodeLogLines parses captured JSON log output. A line that does not parse is
// a failure rather than a skip: silently dropping it would let the assertions
// below count zero and report a wiring problem that is really a parse problem.
func decodeLogLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("captured log line is not JSON: %v\n%s", err, line)
		}
		lines = append(lines, m)
	}
	return lines
}
