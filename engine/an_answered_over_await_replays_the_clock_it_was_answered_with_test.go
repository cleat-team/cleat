package engine

// cleat#4465. A history record's TimestampMs is rebuilt from event_history.created_at on every read path, and a replay's Now() is made from it
// (execSession.Now() is max(nowMs, history[stepCount-1].TimestampMs), and advanceReplayStep sets nowMs to the record's TimestampMs). An await
// recorded EMPTY and then answered at the SAME step is an upsert OVER the pending row, and none of the answer-over writers set created_at, so the row
// kept the await's START time while the live run had read the time of the ANSWER. A call whose request carried Now() then replayed with a request
// earlier than the one it recorded, and #4424's request comparison refused it:
//
//	replay divergence at step 1: call asked for something other than the call recorded there: call notifications.send_webhook with a different request
//
// The write-ahead-intent completion (CompleteCallIntent) is the same defect on a different statement: it UPDATEs the pending row and left created_at
// at the intent time, while the completed record carries the post-call time.
//
// Every test below that touches a database is red on 4de6096f and green with the fix, except the two controls, which are green on both:
// an await_promise (answered at a NEW step, so there is no row to answer over) and the history written by an earlier build.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// answerGap is how long the await (or the call) is left pending before it is answered. The assertions are equalities, so the gap only has to be
// nonzero in milliseconds; every test also asserts that it really was, so a fast machine cannot turn a missing gap into a green.
const answerGap = 60 * time.Millisecond

// eachDialectDB runs fn once per dialect, in a subtest named for it. The dialect's TestDB skips the subtest where its DSN is not set, which is what the
// skip-ledger fragments for these tests declare.
func eachDialectDB(t *testing.T, fn func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore)) {
	t.Helper()
	for _, d := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		d := d
		t.Run(string(d), func(t *testing.T) {
			db := testutil.TestDB(t, d)
			defer db.Close()
			switch d {
			case testutil.DialectMySQL:
				testutil.SetupMySQLFullSchema(t, db)
			case testutil.DialectMSSQL:
				testutil.SetupMSSQLFullSchema(t, db)
			default:
				testutil.SetupFullSchema(t, db, d)
			}
			fn(t, d, db, storeFor(t, d, db))
		})
	}
}

// uniqueRunID keeps a re-run against a database a previous run left rows in from colliding on the run id.
func uniqueRunID(prefix string) string { return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()) }

// anAnswerOverSession builds the session an engine builds on each wake: the history is loaded from the store, so a later wake replays what an
// earlier one wrote.
func anAnswerOverSession(t *testing.T, eng *Engine, store WorkflowStore, wfID string) *execSession {
	t.Helper()
	history, err := store.LoadEventHistory(context.Background(), wfID)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	return &execSession{
		engine:     eng,
		workflowID: wfID,
		nowMs:      eng.seedNowMs(history),
		isReplay:   len(history) > 0,
		history:    history,
		deferrals:  map[string]string{},
		queryState: map[string]string{},
	}
}

// A child await that was pending on one wake and answered on a later one. The live run reads Now() right after the answer; the replay must read the same.
//
// The child is still running on the SECOND wake as well, so the await is re-recorded pending over its own pending row before it is answered: the row's
// created_at then carries the latest wake's time, which the changelog says and this asserts.
//
// Each case runs twice. The worker passes WithWorkflowStartTime (the workflow row's created_at), which anchors a replay's clock before the first step; an
// embedder that does not seeds it from the first record's timestamp instead (seedNowMs). When that first record is an answered-over await, the fix moves
// that timestamp from the await's start to its answer, so a Now() read BEFORE the await is later on a replay than it was live. That is asserted for the
// worker's shape and only logged for the other, which is the caveat in the upgrade note.
func TestAnAnsweredOverAwaitReplaysTheClockItWasAnsweredWith(t *testing.T) {
	eachDialectDB(t, func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore) {
		for _, kind := range []string{"await_child", "await_all_children"} {
			for _, withStart := range []bool{true, false} {
				kind, withStart := kind, withStart
				name := kind + "/the_worker_with_the_workflow_start_time"
				if !withStart {
					name = kind + "/an_embedder_without_it"
				}
				t.Run(name, func(t *testing.T) {
					wfID := uniqueRunID("answer-over-clock-" + string(d) + "-" + kind)
					seedWorkflowInstance(t, db, d, wfID)
					opts := []EngineOption{WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID), WithWorkflowStore(store), WithDB(db)}
					if withStart {
						opts = append(opts, WithWorkflowStartTime(time.Now().UnixMilli()-1000))
					}
					eng := NewEngine(nil, nil, opts...)
					const childRun = "child-run-1"
					childDone := false
					eng.childWfStore = &mockChildStore{
						getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
							if childDone {
								return ChildOutcome{Completed: true, Result: `{"ok":true}`}, nil
							}
							return ChildOutcome{}, nil
						},
					}
					ctx := contextWithRawMemBuf(context.Background(), make([]byte, 600))
					await := func(s *execSession) int64 {
						if kind == "await_all_children" {
							return s.AwaitAllChildren(ctx, nil, `["`+childRun+`"]`, 0, 400)
						}
						return s.AwaitChild(ctx, nil, childRun, 0, 400)
					}

					// Wake 1: the child is running. The await is recorded EMPTY and the run suspends. A first execution with no start time is seeded from the
					// process clock, which the worker refreshes on every poll (UpdateNowMs).
					UpdateNowMs()
					s1 := anAnswerOverSession(t, eng, store, wfID)
					liveBefore := s1.Now(ctx)
					await(s1)
					if s1.suspendErr == nil {
						t.Fatalf("wake 1: the await did not suspend")
					}
					startMs := s1.history[len(s1.history)-1].TimestampMs

					// Wake 2: still running. The pending record is written again over the pending row, with the time of this wake.
					time.Sleep(answerGap)
					s2 := anAnswerOverSession(t, eng, store, wfID)
					await(s2)
					if s2.suspendErr == nil {
						t.Fatalf("wake 2: the await did not suspend")
					}
					if again := s2.history[len(s2.history)-1].TimestampMs; again <= startMs {
						t.Errorf("a pending await re-recorded on a later wake kept its first timestamp %d (now %d): the changelog says it carries the latest wake's time", startMs, again)
					}

					// Wake 3: the child has finished. The answer is written OVER the same row.
					time.Sleep(answerGap)
					childDone = true
					s3 := anAnswerOverSession(t, eng, store, wfID)
					await(s3)
					if s3.suspendErr != nil {
						t.Fatalf("wake 3: the await suspended again: %v", s3.suspendErr)
					}
					liveNow := s3.Now(ctx)
					if liveNow-startMs < 1 {
						t.Fatalf("the answer was recorded at %d, the await started at %d: no gap, so this test would pass on the defect", liveNow, startMs)
					}

					// Wake 4 replays that history from the database and asks the clock at the same points.
					s4 := anAnswerOverSession(t, eng, store, wfID)
					replayBefore := s4.Now(ctx)
					await(s4)
					if s4.suspendErr != nil {
						t.Fatalf("replay: the await suspended: %v", s4.suspendErr)
					}
					if replayNow := s4.Now(ctx); replayNow != liveNow {
						t.Errorf("Now() after an answered-over %s on %s: the live run read %d, the replay reads %d (%dms earlier, the await started at %d)",
							kind, d, liveNow, replayNow, liveNow-replayNow, startMs)
					}
					switch {
					case withStart && replayBefore != liveBefore:
						t.Errorf("Now() before the first step with the workflow start time set: the live run read %d, the replay reads %d", liveBefore, replayBefore)
					case !withStart:
						t.Logf("embedder without WithWorkflowStartTime, %s on %s: Now() before the first step read %d live and %d on the replay (%+dms)",
							kind, d, liveBefore, replayBefore, replayBefore-liveBefore)
					}
				})
			}
		}
	})
}

// slowIntentCaller takes a while to answer, as a service does.
type slowIntentCaller struct{ d time.Duration }

func (c *slowIntentCaller) Call(ctx context.Context, service, operation, request string) (string, error) {
	time.Sleep(c.d)
	return `{"charged":true}`, nil
}

// A write-ahead-intent call is recorded PENDING before the call and completed after it, by an UPDATE of the same row.
func TestAWriteAheadIntentCallReplaysTheClockItCompletedWith(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "intent-clock")
		s := &execSession{
			engine: NewEngine(nil, &slowIntentCaller{d: answerGap},
				WithWorkflowStore(store),
				WithWorkflowID(wfID),
				WithWriteAheadIntentOps(intentService+"."+intentOperation)),
			workflowID: wfID,
		}
		before := time.Now().UnixMilli()
		s.DurableCall(ctx, nil, intentService, intentOperation, `{"amount":100}`, 0, 0)
		liveNow := s.Now(ctx)
		if liveNow-before < answerGap.Milliseconds()/2 {
			t.Fatalf("the call took %dms of virtual time, want about %dms: no gap, so this test would pass on the defect", liveNow-before, answerGap.Milliseconds())
		}
		stored := stepRecord(t, ctx, store, wfID, 0)
		replay := &execSession{workflowID: wfID, history: []EventRecord{stored}, stepCount: 1}
		if replayNow := replay.Now(ctx); replayNow != liveNow {
			t.Errorf("Now() after a write-ahead-intent call on %T: the live run read %d, the replay reads %d (%dms earlier)", store, liveNow, replayNow, liveNow-replayNow)
		}
	})
}

// Every Postgres statement that answers an await OVER its pending row, driven directly: a revert of any one of them goes red by itself. The pending row
// is written at T0 by flushEvent; the answer carries T0+1500ms; what LoadEventHistory reads back for step 0 is what a replay's Now() is made from.
func TestEveryPostgresAnswerOverWriterStoresTheAnswersClock(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	defer db.Close()
	ctx := context.Background()
	store := storeFor(t, testutil.DialectPostgres, db).(*PostgresStore)

	type writer struct {
		name  string
		write func(t *testing.T, eng *Engine, wfID string, answer EventRecord)
	}
	writers := []writer{
		{"flushEvent (insertEventSQL, flush.go)", func(t *testing.T, eng *Engine, wfID string, a EventRecord) {
			if err := eng.flushEvent(ctx, wfID, a, ""); err != nil {
				t.Fatal(err)
			}
		}},
		{"AppendEventHistoryBatch, ONE event (appendOneEvent)", func(t *testing.T, eng *Engine, wfID string, a EventRecord) {
			if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{a}); err != nil {
				t.Fatal(err)
			}
		}},
		{"AppendEventHistoryBatch, TWO events (prepared statement)", func(t *testing.T, eng *Engine, wfID string, a EventRecord) {
			next := EventRecord{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op", Response: `{}`, TimestampMs: a.TimestampMs + 1}
			if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{a, next}); err != nil {
				t.Fatal(err)
			}
		}},
		{"AdaptiveFlusher batch (flushAndNotify)", func(t *testing.T, eng *Engine, wfID string, a EventRecord) {
			af := NewAdaptiveFlusher(db, DefaultTenantUUID, 5*time.Millisecond, 200, 1, 0.5, 0)
			af.batchMode = true
			done, batched := af.Flush(ctx, wfID, a, computeEventChecksum(a, ""), "", 0)
			if !batched {
				t.Fatal("the adaptive flusher did not batch")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the batch never flushed")
			}
		}},
		{"retryBatchFlush (adaptive_flush.go)", func(t *testing.T, eng *Engine, wfID string, a EventRecord) {
			af := NewAdaptiveFlusher(db, DefaultTenantUUID, 5*time.Millisecond, 200, 1, 0.5, 0)
			entry, err := af.prepareEntry(wfID, a, computeEventChecksum(a, ""))
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal([]map[string]interface{}{batchEntryJSONRow(entry)})
			if err != nil {
				t.Fatal(err)
			}
			if err := retryBatchFlush(ctx, af, b, 1); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for i, w := range writers {
		w := w
		wfID := uniqueRunID(fmt.Sprintf("answer-over-writer-%d", i))
		t.Run(w.name, func(t *testing.T) {
			seedWorkflowInstance(t, db, testutil.DialectPostgres, wfID)
			eng := NewEngine(nil, nil, WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID), WithWorkflowStore(store), WithDB(db))
			t0 := time.Now().UnixMilli() - 5000
			pending := EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", TimestampMs: t0}
			if err := eng.flushEvent(ctx, wfID, pending, ""); err != nil {
				t.Fatal(err)
			}
			answer := EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", Response: `{"ok":true}`, TimestampMs: t0 + 1500}
			w.write(t, eng, wfID, answer)
			h, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatal(err)
			}
			if len(h) == 0 || h[0].Response == "" {
				t.Fatalf("the answer was not written over the pending row: %+v", h)
			}
			if h[0].TimestampMs != answer.TimestampMs {
				t.Errorf("stored timestamp %d, the answer carried %d (%dms stale)", h[0].TimestampMs, answer.TimestampMs, answer.TimestampMs-h[0].TimestampMs)
			}
		})
	}
}

// The clew shape end to end: a durable call whose request carries Now(), read right after an answered-over child await. A later wake replays the history,
// and the replayed call must find the request it recorded.
//
// The second case is a documented, asserted behaviour and not a fix: a history written BEFORE this change holds the await's start time in the answered-over
// row, and nothing in the row says when the answer was recorded, so no read can repair it. Such a replay still diverges, so a run in flight across the
// upgrade must finish (or be restarted) first; the UPGRADE NOTE says so. The case pins that, and the first case is its control: the same sequence with
// nothing pinned back replays cleanly.
func TestAWakeRepeatsTheRequestThatReadNowAfterAnAnsweredOverAwait(t *testing.T) {
	eachDialectDB(t, func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore) {
		for _, tc := range []struct {
			name         string
			staleRow     bool
			wantDiverges bool
		}{
			{"history_written_by_this_code", false, false},
			{"history_left_by_an_earlier_build_with_the_await_start_in_created_at", true, true},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				wfID := uniqueRunID("answer-over-request-" + string(d))
				seedWorkflowInstance(t, db, d, wfID)
				caller := &mockCaller{}
				eng := NewEngine(nil, caller, WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID), WithWorkflowStore(store), WithDB(db))
				childDone := false
				eng.childWfStore = &mockChildStore{getChildResultFn: func(context.Context, string) (ChildOutcome, error) {
					if childDone {
						return ChildOutcome{Completed: true, Result: `{"ok":true}`}, nil
					}
					return ChildOutcome{}, nil
				}}
				wake := func() (*execSession, int64, string) {
					buf := make([]byte, 1024)
					ctx := contextWithRawMemBuf(context.Background(), buf)
					s := anAnswerOverSession(t, eng, store, wfID)
					res := s.AwaitChild(ctx, nil, "child-run-1", 0, 400)
					if s.suspendErr != nil {
						return s, res, ""
					}
					req := fmt.Sprintf(`{"payload":{"escalated_at":%d}}`, s.Now(ctx))
					res = s.DurableCall(ctx, nil, "notifications", "send_webhook", req, 0, 400)
					return s, res, strings.TrimRight(string(buf), "\x00")
				}

				s1, _, _ := wake()
				if s1.suspendErr == nil {
					t.Fatal("wake 1 did not suspend")
				}
				startMs := s1.history[0].TimestampMs
				time.Sleep(answerGap)
				childDone = true
				_, r2, msg2 := wake()
				if errCodeOf(r2) != 0 || len(caller.calls) != 1 {
					t.Fatalf("wake 2 (the live answer): result %#x, %d calls, want a clean call and one call; message %q", r2, len(caller.calls), msg2)
				}
				if tc.staleRow {
					// What an earlier build left: the answered-over row holding the await's START time.
					ph := map[testutil.Dialect][2]string{
						testutil.DialectPostgres: {"$1", "$2"},
						testutil.DialectMySQL:    {"?", "?"},
						testutil.DialectMSSQL:    {"@p1", "@p2"},
					}[d]
					if _, err := testutil.AdminDB(t, db, d).Exec(
						"UPDATE event_history SET created_at = "+ph[0]+" WHERE step = 0 AND workflow_id = "+ph[1], time.UnixMilli(startMs), wfID); err != nil {
						t.Fatalf("pin created_at back to the await's start: %v", err)
					}
				}

				_, r3, msg3 := wake()
				diverged := errCodeOf(r3) != 0
				switch {
				case tc.wantDiverges && !diverged:
					t.Errorf("the replay of a history with a stale answered-over created_at read cleanly (result %#x): the pin did not take, or the replay stopped refusing it", r3)
				case tc.wantDiverges && !strings.Contains(msg3, "with a different request"):
					t.Errorf("the replay failed, but not as a request divergence: result %#x, message %q", r3, msg3)
				case !tc.wantDiverges && diverged:
					t.Errorf("the replay of the call that read Now() after the answered-over await did not find its recorded request: result %#x, message %q", r3, msg3)
				}
				if len(caller.calls) != 1 {
					t.Errorf("the caller was invoked %d times across the three wakes, want 1 (the replay must answer from history, never re-call)", len(caller.calls))
				}
			})
		}
	})
}

// The control: an await_promise is NOT answered over its pending row. deliver() records promise_resolved at a NEW step, so there is no row to answer over
// and this stays green on the unfixed code. It runs through the same engine, store and session as the child await above, so a red child await beside a green
// promise await is a statement about answer-over writers and not about the harness.
func TestAPromiseAwaitReplaysTheClockItAnsweredWith(t *testing.T) {
	eachDialectDB(t, func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore) {
		wfID := uniqueRunID("answer-over-promise-" + string(d))
		seedWorkflowInstance(t, db, d, wfID)
		eng := NewEngine(nil, &mockCaller{}, WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID), WithWorkflowStore(store), WithDB(db),
			WithPromiseStore(store.(PromiseStore)))
		ctx := contextWithRawMemBuf(context.Background(), make([]byte, 1024))
		wake := func() *execSession {
			s := anAnswerOverSession(t, eng, store, wfID)
			s.CreatePromise(ctx, nil, "p", 0, 200)
			pid := ""
			for _, r := range s.history {
				if r.EventType == EventTypeCreatePromise {
					pid = r.PromiseID
				}
			}
			s.AwaitPromise(ctx, nil, pid, 600000, 300, 300)
			return s
		}
		s1 := wake()
		if s1.suspendErr == nil {
			t.Fatalf("wake 1 did not suspend; history %+v", s1.history)
		}
		pid := ""
		for _, r := range s1.history {
			if r.EventType == EventTypeCreatePromise {
				pid = r.PromiseID
			}
		}
		awaitTimestamp := func(s *execSession) int64 {
			for _, r := range s.history {
				if r.EventType == EventTypeAwaitPromise {
					return r.TimestampMs
				}
			}
			t.Fatalf("no await_promise record in %+v", s.history)
			return 0
		}
		firstAwait := awaitTimestamp(s1)

		// A wake that finds the promise still pending must NOT write the await again: its deadline is that record's timestamp plus the timeout, and a
		// created_at that moved with each wake would push the deadline out on every wake and never reach it. (This is why the writers above may set
		// created_at: the one await whose timestamp is a deadline is not one of the pending awaits they re-record.)
		time.Sleep(answerGap)
		if again := awaitTimestamp(wake()); again != firstAwait {
			t.Errorf("a pending await_promise re-recorded on a later wake moved its timestamp from %d to %d: its deadline moves with it", firstAwait, again)
		}

		time.Sleep(answerGap)
		if err := store.(PromiseStore).ResolvePromise(context.Background(), pid, `{"ok":true}`); err != nil {
			t.Fatal(err)
		}
		s2 := wake()
		liveNow := s2.Now(ctx)
		if liveNow-s1.history[len(s1.history)-1].TimestampMs < 1 {
			t.Fatalf("the promise was resolved at %d, the await started at %d: no gap", liveNow, s1.history[len(s1.history)-1].TimestampMs)
		}
		s3 := wake()
		if replayNow := s3.Now(ctx); replayNow != liveNow {
			t.Errorf("promise await on %s: the live run read %d, the replay reads %d (%dms earlier)", d, liveNow, replayNow, liveNow-replayNow)
		}
	})
}

// The fix writes the answer's timestamp into created_at and nothing else about the row. That is safe for the event chain only because the checksum does not
// cover the timestamp; this is the measurement the PR rests on. The second half is its control: the checksum does cover the response.
func TestTheEventChecksumDoesNotCoverTheTimestampAnAnswerOverRewrites(t *testing.T) {
	a := EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "r", Response: `{"ok":true}`, TimestampMs: 1000}
	b := a
	b.TimestampMs = 999999
	c := a
	c.Response = `{"ok":false}`
	if computeEventChecksum(a, "") != computeEventChecksum(b, "") {
		t.Errorf("the checksum covers TimestampMs: an answer-over that rewrites created_at would break the chain")
	}
	if computeEventChecksum(a, "") == computeEventChecksum(c, "") {
		t.Errorf("control: the checksum does not cover the response, so the comparison above measured nothing")
	}
}

// The guard that lets a writer set created_at is the same one that freezes the row once it is answered: only a PENDING await is written over. A stray write
// of the same step that arrives LATER (a batch flushed twice, a retried flush) must change neither the answer nor its time. The existing immutability tests
// re-flush with the same TimestampMs, which cannot tell a frozen created_at from a rewritten one, so this one gives the stray write a much later time.
//
// Without the guard, the MySQL assignment `created_at = VALUES(created_at)` would store the stray's time beside a response that stayed frozen, and a replay
// would read a clock from the future of the answer.
func TestAStrayReflushOverAnAnsweredAwaitKeepsTheAnswersTimestamp(t *testing.T) {
	eachDialectDB(t, func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore) {
		// batch is how many records the stray travels with. One goes through appendOneEvent (a direct Exec); two or more go through
		// appendEventsInTx's prepared statement, which is a SEPARATE copy of the guard on PostgreSQL and the one a terminal write (a finalize, a
		// defer-phase finalize, a continue-as-new) uses for its whole batch of new events. A guard removed from that copy alone left every
		// test here green (cleat#4475, measured 2026-11-26), because every stray below used to travel alone.
		pendingAgain := func(t0 int64) EventRecord {
			return EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", TimestampMs: t0 + 5000}
		}
		answerAgain := func(t0 int64) EventRecord {
			return EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", Response: `{"ok":true}`, TimestampMs: t0 + 5000}
		}
		for _, tc := range []struct {
			name  string
			stray func(t0 int64) EventRecord
			batch int
		}{
			{"the_pending_record_written_again_later", pendingAgain, 1},
			{"the_answer_written_again_later", answerAgain, 1},
			{"the_pending_record_written_again_later_in_a_batch_of_two", pendingAgain, 2},
			{"the_answer_written_again_later_in_a_batch_of_two", answerAgain, 2},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				ctx := context.Background()
				wfID := uniqueRunID("answer-over-stray-" + string(d))
				seedWorkflowInstance(t, db, d, wfID)
				t0 := time.Now().UnixMilli() - 10000
				write := func(rec EventRecord) {
					t.Helper()
					if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{rec}); err != nil {
						t.Fatalf("append %+v: %v", rec, err)
					}
				}
				write(EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", TimestampMs: t0})
				answer := EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", Response: `{"ok":true}`, TimestampMs: t0 + 100}
				write(answer)
				read := func() EventRecord {
					t.Helper()
					h, err := store.LoadEventHistory(ctx, wfID)
					if err != nil || len(h) < 1 {
						t.Fatalf("load history: %v, %d records", err, len(h))
					}
					for _, r := range h {
						if r.Step == 0 {
							return r
						}
					}
					t.Fatalf("no step 0 in the history: %+v", h)
					return EventRecord{}
				}
				if got := read(); got.Response != answer.Response || got.TimestampMs != answer.TimestampMs {
					t.Fatalf("precondition: the answer was not stored with its own time: response %q at %d, want %q at %d", got.Response, got.TimestampMs, answer.Response, answer.TimestampMs)
				}
				batch := []EventRecord{tc.stray(t0)}
				for i := 1; i < tc.batch; i++ {
					// Companions at steps nothing else has written, so the only conflict in the batch is the stray itself.
					batch = append(batch, EventRecord{Step: i, EventType: EventTypeCall, Service: "svc", Op: "op", Response: `{}`, TimestampMs: t0 + 5000 + int64(i)})
				}
				if err := store.AppendEventHistoryBatch(ctx, wfID, batch); err != nil {
					t.Fatalf("append a batch of %d: %v", len(batch), err)
				}
				if got := read(); got.Response != answer.Response || got.TimestampMs != answer.TimestampMs {
					t.Errorf("a stray write of an answered await changed it: response %q at %d (%+dms), want %q at %d", got.Response, got.TimestampMs, got.TimestampMs-answer.TimestampMs, answer.Response, answer.TimestampMs)
				}
			})
		}
	})
}
