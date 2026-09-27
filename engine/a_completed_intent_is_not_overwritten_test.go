package engine

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// A call intent that completed with an EMPTY response stored the empty string
// where every INSERT path stores NULL, and insertEventSQL's
//
//	ON CONFLICT ... DO UPDATE SET response = EXCLUDED.response, error = EXCLUDED.error
//	  WHERE event_history.response = '' AND event_history.error IS NULL
//
// then fired on it. The clause exists to complete a row written without a
// result and to leave finished rows immutable; a row written without a result
// has response NULL and `NULL = ”` is not true, so it could only ever fire on
// the finished rows it was meant to protect. cleat#1379.
//
// What that cost, measured before the fix: the DO UPDATE overwrote the
// response column and left the payload and checksum untouched, so the row's
// displayed response disagreed with the payload replay reads and the workflow
// became permanently unverifiable.
func TestACompletedIntentIsNotOverwrittenByALaterAppend(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	store := NewPostgresStore(db)
	ctx := context.Background()
	runID := appendChainWorkflow(t, store)

	// A call that succeeded with an empty response and no error. That is the
	// only shape for which `response = '' AND error IS NULL` can be true.
	rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{"a":1}`}
	if err := store.WriteCallIntent(ctx, runID, rec, "", 0); err != nil {
		t.Fatalf("write intent: %v", err)
	}
	payload, _ := eventRecordToPayload(rec)
	checksum := computeEventChecksum(rec, "")
	if err := store.CompleteCallIntent(ctx, runID, rec, payload, checksum, "", 0); err != nil {
		t.Fatalf("complete intent: %v", err)
	}

	// The control, and the whole mechanism in one assertion: NULL, not ''.
	// With '' the clause below is true and the row is writable by anything
	// that re-inserts the step.
	var resp sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT response FROM event_history WHERE workflow_id = $1 AND step = 0`, runID).Scan(&resp); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Valid {
		t.Fatalf("a completed intent with an empty response stored %q rather than NULL, so "+
			"insertEventSQL's `WHERE response = ''` clause is true for it and any later append "+
			"of this step overwrites the outcome", resp.String)
	}

	// The re-append that used to overwrite it. FinalizeWorkflowSegment appends
	// the whole segment routinely, so this is the ordinary path with a
	// different response rather than an exotic one.
	rec.Response = `{"late":"outcome"}`
	if err := store.AppendEventHistoryBatch(ctx, runID, []EventRecord{rec}); err != nil {
		t.Fatalf("re-append: %v", err)
	}

	got, err := store.LoadEventHistory(ctx, runID)
	if err != nil || len(got) != 1 {
		t.Fatalf("load history: %v len=%d", err, len(got))
	}
	if got[0].Response != "" {
		t.Errorf("a later append overwrote the completed intent's response with %q.\n\n"+
			"The DO UPDATE leaves payload and checksum alone, so the column now disagrees with "+
			"the payload replay reads -- and `response` is not in shadowFields, so "+
			"verifyShadowColumns cannot see it.", got[0].Response)
	}

	// And the row still verifies. That is the consequence that made this worth
	// fixing rather than tidying: the overwrite left a checksum computed over
	// the original payload against a column that had moved.
	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Errorf("VerifyWorkflowEvents on a completed intent that was re-appended: %v", err)
	}
}

// The same NULL/” property on all three dialects, because the raw bind was in
// all six statements and leaving two inverted is the divergence #1379's other
// half was about.
func TestACompletedIntentStoresNullNotEmptyOnEveryDialect(t *testing.T) {
	for _, d := range []struct {
		dialect testutil.Dialect
		setup   func(*testing.T, *sql.DB)
		query   string
	}{
		{testutil.DialectPostgres, func(t *testing.T, db *sql.DB) {
			testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		}, `SELECT response FROM event_history WHERE workflow_id = $1 AND step = 0`},
		{testutil.DialectMySQL, testutil.SetupMySQLFullSchema,
			"SELECT response FROM event_history WHERE workflow_id = ? AND step = 0"},
		{testutil.DialectMSSQL, testutil.SetupMSSQLFullSchema,
			`SELECT response FROM event_history WHERE workflow_id = @p1 AND step = 0`},
	} {
		t.Run(string(d.dialect), func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			defer db.Close()
			d.setup(t, db)

			wfID := "intent-null-" + string(d.dialect)
			seedWorkflowInstance(t, db, d.dialect, wfID)
			admin := testutil.AdminDB(t, db, d.dialect)
			admin.Exec(deleteEventsSQL(d.dialect), wfID)

			store := storeFor(t, d.dialect, db).(callIntentStore)
			ctx := context.Background()
			rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{"a":1}`}
			if err := store.WriteCallIntent(ctx, wfID, rec, "", 0); err != nil {
				t.Fatalf("write intent on %s: %v", d.dialect, err)
			}
			payload, _ := eventRecordToPayload(rec)
			if err := store.CompleteCallIntent(ctx, wfID, rec, payload, "chk", "", 0); err != nil {
				t.Fatalf("complete intent on %s: %v", d.dialect, err)
			}

			var resp sql.NullString
			if err := admin.QueryRow(d.query, wfID).Scan(&resp); err != nil {
				t.Fatalf("read response on %s: %v", d.dialect, err)
			}
			if resp.Valid {
				t.Errorf("%s stored %q rather than NULL for a completed intent with an empty "+
					"response", d.dialect, resp.String)
			}
		})
	}
}
