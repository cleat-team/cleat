package engine

// W2a of cleat#4152, over a REAL event_history: a multi-record step is cut after EVERY record of its run, replayed, and let continue. Whatever the
// cut, the history the store ends up holding must be the history an uninterrupted run wrote, the effect must have happened exactly once per record
// the run was missing (never again for one it already had), and a replay that finds the whole run must change nothing. The history is read from the
// REAL store and the prefix is written by the engine's own recordEvent, not by hand (cleat#4177: a hand-written record is a record the engine
// never wrote). This is the property that matters for a retried call and for SetScope, and it is cheap to get wrong by consuming a prefix twice.
//
// Run against each dialect that has a DSN, like the other store-backed tests; an unset DSN skips that dialect (read the skip names).

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

const (
	rtSvc, rtOp = "svc", "op"
	rtFailures  = 2 // the run is: rtFailures failed attempts, then the call
)

func rtRecord(step int) EventRecord {
	if step < rtFailures {
		return EventRecord{Step: step, EventType: EventTypeCallAttemptFailed, Service: rtSvc, Op: rtOp, Request: `{}`, Err: "boom"}
	}
	return EventRecord{Step: step, EventType: EventTypeCall, Service: rtSvc, Op: rtOp, Request: `{}`, Response: `"ok"`}
}

// rtSpec is a retry-shaped spec: failed attempts of this service and op are the prefix, the call is the final. Fresh makes the attempts the
// history is missing (counting each as one effect) and records them through c.Record.
func rtSpec(effects *int32) *stepSpec {
	return &stepSpec{
		Name: "rt_retry", Result: layoutSimple, Effect: effectEffect, Records: recordsMany, Want: EventTypeCall,
		Identity: func(a stepArgs, rec *EventRecord) string {
			if rec.Service != rtSvc || rec.Op != rtOp {
				return fmt.Sprintf("%s.%s, recorded %s.%s", rtSvc, rtOp, rec.Service, rec.Op)
			}
			return ""
		},
		Role: func(a stepArgs, rec *EventRecord) recordRole {
			switch rec.EventType {
			case EventTypeCallAttemptFailed:
				return rolePrefix
			case EventTypeCall:
				return roleFinal
			}
			return roleForeign
		},
		DecodeRun: func(c *stepCtx, run []EventRecord) int64 { return 7 },
		Fresh: func(c *stepCtx) int64 {
			for i := len(c.Prefix); i <= rtFailures; i++ {
				atomic.AddInt32(effects, 1)
				c.Record(rtRecord(c.s.stepCount))
			}
			return 9
		},
	}
}

func TestAMultiRecordStepCutAtEveryRecordContinuesToTheSameHistory(t *testing.T) {
	for _, d := range []struct {
		name    string
		dialect testutil.Dialect
		setup   func(*testing.T, *sql.DB)
	}{
		{"postgres", testutil.DialectPostgres, func(t *testing.T, db *sql.DB) { testutil.SetupFullSchema(t, db, testutil.DialectPostgres) }},
		{"mssql", testutil.DialectMSSQL, func(t *testing.T, db *sql.DB) { testutil.SetupMSSQLFullSchema(t, db) }},
		{"mysql", testutil.DialectMySQL, func(t *testing.T, db *sql.DB) { testutil.SetupMySQLFullSchema(t, db) }},
	} {
		d := d
		t.Run(d.name, func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			d.setup(t, db)
			defer db.Close()
			total := rtFailures + 1
			for cut := 0; cut <= total; cut++ {
				cut := cut
				t.Run(fmt.Sprintf("cut_after_%d_of_%d", cut, total), func(t *testing.T) { rtCutAndContinue(t, d.dialect, db, cut, total) })
			}
		})
	}
}

func rtCutAndContinue(t *testing.T, dialect testutil.Dialect, db *sql.DB, cut, total int) {
	t.Helper()
	ctx := context.Background()
	wfID := fmt.Sprintf("run-trip-%s-%s-%d", dialect, time.Now().Format("150405.000000"), cut)
	seedWorkflowInstance(t, db, dialect, wfID)
	store := storeFor(t, dialect, db)
	eng := NewEngine(nil, nil, WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID), WithWorkflowStore(store), WithDB(db))
	session := func(history []EventRecord) *execSession {
		return &execSession{
			engine: eng, workflowID: wfID, nowMs: time.Now().UnixMilli(),
			isReplay: len(history) > 0, history: history,
			deferrals: map[string]string{}, queryState: map[string]string{},
		}
	}
	load := func() []EventRecord {
		h, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("load history: %v", err)
		}
		return h
	}

	// The original run, lost after `cut` records: written by the engine's own recordEvent, as a run that did that much would have.
	seed := session(nil)
	for i := 0; i < cut; i++ {
		seed.recordEvent(rtRecord(i))
	}
	history := load()
	if len(history) != cut {
		t.Fatalf("PRECONDITION FAILED: seeded %d records, the store holds %d", cut, len(history))
	}

	var effects int32
	s := session(history)
	got := s.step(ctx, nil, rtSpec(&effects), stepArgs{})
	wantRes, wantEffects := int64(9), int32(total-cut)
	if cut == total {
		wantRes, wantEffects = 7, 0 // the whole run was there: decoded, nothing done again
	}
	if got != wantRes {
		t.Errorf("cut after %d: answered %d, want %d", cut, got, wantRes)
	}
	if effects != wantEffects {
		t.Errorf("cut after %d: made %d attempts, want %d (one per record the history was missing, none again for one it had)", cut, effects, wantEffects)
	}
	if len(s.stepViolations) != 0 {
		t.Errorf("cut after %d: the dispatcher noted %v", cut, s.stepViolations)
	}

	final := load()
	if len(final) != total {
		t.Fatalf("cut after %d: the store holds %d events, want the run's %d", cut, len(final), total)
	}
	for i, rec := range final {
		want := rtRecord(i)
		if rec.Step != i || rec.EventType != want.EventType || rec.Service != want.Service || rec.Err != want.Err {
			t.Errorf("cut after %d: event %d is %s/step %d err=%q, want what an uninterrupted run wrote (%s, err=%q)", cut, i, rec.EventType, rec.Step, rec.Err, want.EventType, want.Err)
		}
	}

	// Replaying the now-complete history changes nothing: decoded again, no attempt, nothing written.
	var again int32
	r := session(final)
	if res := r.step(ctx, nil, rtSpec(&again), stepArgs{}); res != 7 || again != 0 || r.stepCount != total {
		t.Errorf("cut after %d: replaying the finished run returned %d with %d attempts at step %d, want 7, 0, %d", cut, res, again, r.stepCount, total)
	}
	if h := load(); len(h) != total {
		t.Errorf("cut after %d: replaying the finished run changed the history to %d events", cut, len(h))
	}
}
