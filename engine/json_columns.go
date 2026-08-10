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
