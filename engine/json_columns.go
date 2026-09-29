package engine

import "encoding/json"

// marshalQueryState renders a workflow's query state for the JSON column it is
// stored in.
//
// The obvious spelling is wrong in a way that only one dialect notices:
//
//	qsJSON, _ := json.Marshal(queryState)
//	if qsJSON == nil {
//	    qsJSON = []byte("{}")
//	}
//
// json.Marshal of a nil map returns the four bytes `null`, not nil, so that
// guard never fires and `null` is what reaches the database. PostgreSQL's
// JSONB and MySQL's JSON both accept it -- a JSON null is valid JSON -- so the
// row goes in and the workflow's query state reads back as null instead of an
// empty object. SQL Server's shipped schema does not accept it:
// migrations/mssql/001_schema.sql guards the column with
// `CHECK (ISJSON(query_state) = 1)`, and `ISJSON('null')` is 0. So on a SQL
// Server built from the shipped schema, CompleteWorkflow, FailWorkflow and
// ContinueAsNew all failed for any workflow with no query handlers -- which is
// most of them. IMPROVEMENT-PLAN 3.17.
//
// Nothing caught it because engine/testutil's MSSQL schema declares no CHECK
// constraint on that column, so every dialect quietly stored `null` and the
// suite was green.
func marshalQueryState(queryState map[string]string) []byte {
	b, err := json.Marshal(queryState)
	if err != nil || len(b) == 0 || string(b) == "null" {
		return []byte("{}")
	}
	return b
}

// queryStateUpdateParam is marshalQueryState's counterpart for a terminal
// write that may have NOTHING new to say, as opposed to a write with nothing
// to preserve.
//
// A nil queryState means the caller never ran a replay that could have
// published anything -- a panic recovery, or a failure before the segment
// started (see writeTerminalFailure's doc comment, cmd/cleat-worker/setup.go).
// Passing that through marshalQueryState writes the literal string "{}",
// which the UPDATE below (query_state = $N) applies unconditionally --
// wiping whatever the last successfully-finalized segment had persisted.
// cleat#2520.
//
// Returning a real, untyped Go nil here -- not the string "{}" -- makes the
// database driver bind an actual SQL NULL, so `query_state = COALESCE($N,
// query_state)` at the call site leaves the column exactly as it was. A
// non-nil queryState, even an empty map, IS something to say (this replay
// ran and published nothing) and is marshalled and written as usual.
func queryStateUpdateParam(queryState map[string]string) any {
	if queryState == nil {
		return nil
	}
	return string(marshalQueryState(queryState))
}
