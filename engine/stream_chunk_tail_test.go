package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// The durable stream tail, cleat#1639: a reader on any worker can follow a run
// from event_history, which means every real store has to be able to serve it.
//
// COMPILE-TIME, NOT A TEST ASSERTION, and deliberately so. StreamChunkTailReader
// is an OPTIONAL interface -- the SSE route type-asserts for it and degrades
// with a stated reason when a store does not implement it. That escape hatch is
// right for a test double and wrong for a dialect: a store that silently stops
// implementing this does not fail, it just stops serving readers on every worker
// but one, which is the exact bug #1639 reports.
//
// So the four real stores are pinned here, where the failure is a build error
// rather than a behaviour nobody notices.
var _ = []StreamChunkTailReader{
	(*PostgresStore)(nil),
	(*MySQLStore)(nil),
	(*MSSQLStore)(nil),
	(*ShardedStore)(nil),
}

// The cursor is EXCLUSIVE and -1 is not 0, on every dialect.
//
// Both halves have a way to be wrong that no single-dialect test would catch. An
// inclusive cursor re-sends the reader's last chunk on every poll, which shows up
// as a stuttering transcript rather than as an error; and treating -1 as 0 drops
// a run's first chunk, because step 0 is a real step that a run's first chunk can
// occupy.
func TestTheStreamChunkTailIsAnExclusiveCursorOnEveryDialect(t *testing.T) {
	for _, d := range []struct {
		dialect testutil.Dialect
		setup   func(t *testing.T, db *sql.DB)
	}{
		{testutil.DialectPostgres, func(t *testing.T, db *sql.DB) {
			testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		}},
		{testutil.DialectMySQL, testutil.SetupMySQLFullSchema},
		{testutil.DialectMSSQL, testutil.SetupMSSQLFullSchema},
	} {
		t.Run(string(d.dialect), func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			defer db.Close()
			d.setup(t, db)

			wfID := "tail-cursor-" + string(d.dialect)
			seedWorkflowInstance(t, db, d.dialect, wfID)

			store := storeFor(t, d.dialect, db)
			tail, ok := store.(StreamChunkTailReader)
			if !ok {
				t.Fatalf("the %s store does not implement StreamChunkTailReader, so a "+
					"reader on a worker not executing the run cannot follow it at all "+
					"(cleat#1639)", d.dialect)
			}
			ctx := context.Background()

			// Three chunks at steps 0,1,2 and one NON-chunk event at step 3.
			// The non-chunk event is the control: a tail that returns it is
			// selecting on the workflow rather than on the event type, and the
			// client would render a heartbeat as a token.
			for i, tok := range []string{"Hel", "lo ", "world"} {
				rec := EventRecord{
					Step: i, EventType: EventTypePluginCallStreamChunk,
					PluginName: "llm", PluginFunc: "chat_stream",
					PluginOutput: tok, StreamChunkIndex: i, StreamFinish: i == 2,
				}
				if err := store.AppendEventHistory(ctx, wfID, rec); err != nil {
					t.Fatalf("seed chunk %d on %s: %v", i, d.dialect, err)
				}
			}
			if err := store.AppendEventHistory(ctx, wfID, EventRecord{
				Step: 3, EventType: EventTypeHeartbeat,
			}); err != nil {
				t.Fatalf("seed non-chunk event on %s: %v", d.dialect, err)
			}

			content := func(recs []EventRecord) string {
				var s string
				for _, r := range recs {
					s += r.PluginOutput
				}
				return s
			}

			// -1 is "from the beginning" and must include step 0.
			all, err := tail.LoadStreamChunksAfter(ctx, wfID, -1, 100)
			if err != nil {
				t.Fatalf("tail from -1 on %s: %v", d.dialect, err)
			}
			if len(all) != 3 {
				t.Fatalf("tail from -1 on %s returned %d chunks, want 3.\n\n"+
					"If it returned 2, the cursor is treating -1 as 0 and the run's "+
					"FIRST chunk is being dropped -- step 0 is a real step.\n"+
					"If it returned 4, a non-chunk event is being served as a token.",
					d.dialect, len(all))
			}
			if got := content(all); got != "Hello world" {
				t.Errorf("%s: tail from -1 reassembled %q, want %q", d.dialect, got, "Hello world")
			}
			if all[0].StreamChunkIndex != 0 || !all[2].StreamFinish {
				t.Errorf("%s: the tail did not carry index/finish off the payload "+
					"(index[0]=%d finish[2]=%v) -- a client cannot segment the "+
					"transcript without them",
					d.dialect, all[0].StreamChunkIndex, all[2].StreamFinish)
			}

			// A cursor at step 0 is EXCLUSIVE: it owes 1 and 2, not 0.
			after0, err := tail.LoadStreamChunksAfter(ctx, wfID, 0, 100)
			if err != nil {
				t.Fatalf("tail from 0 on %s: %v", d.dialect, err)
			}
			if len(after0) != 2 || content(after0) != "lo world" {
				t.Errorf("%s: tail from cursor 0 returned %d chunks (%q), want 2 (%q).\n\n"+
					"An INCLUSIVE cursor re-sends the reader's last chunk on every "+
					"poll, which reaches a browser as a stuttering transcript rather "+
					"than as an error.",
					d.dialect, len(after0), content(after0), "lo world")
			}

			// A caught-up reader gets nothing, which is the steady state.
			none, err := tail.LoadStreamChunksAfter(ctx, wfID, 2, 100)
			if err != nil {
				t.Fatalf("tail from 2 on %s: %v", d.dialect, err)
			}
			if len(none) != 0 {
				t.Errorf("%s: a caught-up reader was owed %d chunks, want 0", d.dialect, len(none))
			}

			// The limit is honoured and takes the OLDEST, so a reader behind a
			// backlog makes forward progress rather than skipping to the end.
			first, err := tail.LoadStreamChunksAfter(ctx, wfID, -1, 2)
			if err != nil {
				t.Fatalf("tail with limit on %s: %v", d.dialect, err)
			}
			if len(first) != 2 || content(first) != "Hello " {
				t.Errorf("%s: a limit of 2 returned %d chunks (%q), want the two OLDEST (%q)",
					d.dialect, len(first), content(first), "Hello ")
			}
		})
	}
}

// The tail returns its connection to the pool, every time.
//
// A leak here is invisible in ordinary tests and catastrophic in production: the
// SSE route calls this once per reader per poll interval, so a connection held
// by one poll would exhaust the pool within seconds of a single reader
// attaching, and the symptom would be workflows failing to claim -- nowhere near
// the streaming code.
//
// MaxOpenConns(1) turns that from a slow leak into an immediate deadlock, which
// is the only way to assert it without timing.
func TestTheStreamChunkTailReturnsItsConnection(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	wfID := "tail-conn-leak"
	seedWorkflowInstance(t, db, testutil.DialectPostgres, wfID)

	db.SetMaxOpenConns(1)
	store := NewPostgresStore(db).WithTenant(DefaultTenantUUID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		if _, err := store.LoadStreamChunksAfter(ctx, wfID, -1, 10); err != nil {
			t.Fatalf("read %d of 3 against a one-connection pool: %v\n\n"+
				"A context deadline here means the PREVIOUS read did not return its "+
				"connection -- the transaction was neither committed nor rolled back.",
				i+1, err)
		}
	}
}
