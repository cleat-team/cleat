package engine

import (
	"errors"
	"fmt"
	"strings"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
	mssql "github.com/microsoft/go-mssqldb"
)

// A workflow result the store refuses arrives at the caller as the driver's own
// text, in a 500, with error_code "unknown" -- and it arrives AFTER the workflow
// body and its side effects have already run. cleat#1460.
//
// The cost is unusual for a bad error message because of WHEN it happens. The
// work is done, the record says failed, and the operator reading it is told
//
//	finalize workflow: pq: unsupported Unicode escape sequence (22P05)
//
// which names PostgreSQL's escape handling and does not say that the RESULT was
// what was rejected, which field of it, that another backend would have accepted
// it, or that the body already ran.
//
// This is not detectable before the fact. coerceResultJSON checks json.Valid and
// object shape, and every payload that divides the three dialects passes both.
//
// MEASURED 2026-09-13 against live PostgreSQL 16, MySQL 8 and SQL Server 2022,
// by finalizing a real claimed workflow with each payload. Not read from a
// table -- the table was wrong: cleat#1460 lists MySQL's surrogate rejection as
// 3140 and it is 3141.
//
//	payload                postgres      mysql          mssql
//	{"v":"\u0000"}        22P05         accepted       accepted
//	{"v":"\ud800"}        22P02         3141 (22032)   accepted
//	200 nested arrays      accepted      3157 (22032)   13606
//
// Every column has at least one acceptance, which is the point: none of these is
// "invalid JSON". Each is a value one backend stores happily and another
// refuses, so a workflow that works on MySQL fails on PostgreSQL with a message
// about neither.
//
// ErrResultRejected is the classification; isStoreJSONRejection is the
// detection, done while the driver error is still TYPED so that nothing above
// the store parses driver text -- the same shape as ErrScheduleExists.

// mysqlInvalidJSONSQLState is the SQLSTATE MySQL returns for every
// invalid-JSON-text condition.
//
// Keyed on the SQLSTATE rather than on the error numbers deliberately. 3141 and
// 3157 are the two this repository has measured, and the family has more members
// than that -- 3140 is in it, cited by cleat#1460 for a case that actually
// returns 3141. A number list would silently stop matching the moment MySQL
// used a sibling code; the SQLSTATE covers the family.
const mysqlInvalidJSONSQLState = "22032"

// mssqlJSONNestingLimit is SQL Server's error for JSON past 128 nesting levels.
// SQL Server has no SQLSTATE equivalent to key on here, so this is the measured
// number.
const mssqlJSONNestingLimit = 13606

// isStoreJSONRejection reports whether err is a backend refusing a JSON value
// as it was written, rather than any other kind of failure.
//
// Typed, in every arm. The alternative -- matching on message text -- is what
// this function exists to stop callers doing, and it would also be wrong: the
// three dialects phrase the same refusal three ways and change the phrasing
// between versions.
func isStoreJSONRejection(err error) bool {
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		// 22P05 untranslatable_character -- a NUL escape in a JSONB value.
		// 22P02 invalid_text_representation -- what a lone surrogate produces.
		return pqErr.Code == "22P05" || pqErr.Code == "22P02"
	}
	var myErr *mysqldriver.MySQLError
	if errors.As(err, &myErr) {
		return string(myErr.SQLState[:]) == mysqlInvalidJSONSQLState
	}
	var msErr mssql.Error
	if errors.As(err, &msErr) {
		return msErr.Number == mssqlJSONNestingLimit
	}
	return false
}

// describeRejectedResult names what about result this backend is likely to have
// refused, or returns "" when the result looks portable.
//
// WHY THIS EXISTS AT ALL: a finalize writes more than one JSON value -- the
// result and every new event's payload -- so a JSON rejection does not by itself
// identify the result as the culprit. Saying "the result was refused" when an
// event payload was refused would be a confident wrong answer, which is the
// thing cleat#1460 is complaining about in the first place.
//
// It runs only on a path that has already failed, so its cost is irrelevant, and
// it is deliberately a HINT rather than a verdict: the caller phrases it as such.
func describeRejectedResult(result string) string {
	switch {
	case strings.Contains(result, `\u0000`):
		return `it contains a \u0000 escape, which PostgreSQL refuses in a JSON value ` +
			`and MySQL and SQL Server accept`
	case hasLoneSurrogate(result):
		return `it contains an unpaired UTF-16 surrogate escape, which PostgreSQL and ` +
			`MySQL refuse and SQL Server accepts`
	case jsonNestingDepth(result) > 128:
		return fmt.Sprintf(`it nests %d levels deep; SQL Server refuses past 128 and `+
			`MySQL past its own limit`, jsonNestingDepth(result))
	}
	return ""
}

// hasLoneSurrogate reports whether result carries a \uD800-\uDFFF escape that is
// not followed by its pair. Text-level rather than decoded, because the bytes as
// WRITTEN are what the backend parses.
func hasLoneSurrogate(result string) bool {
	for i := 0; i+6 <= len(result); i++ {
		if result[i] != 92 || result[i+1] != 'u' {
			continue
		}
		var v int
		if _, err := fmt.Sscanf(result[i+2:i+6], "%04x", &v); err != nil {
			continue
		}
		if v < 0xD800 || v > 0xDBFF {
			continue // not a HIGH surrogate; a lone LOW one is caught below
		}
		// A high surrogate must be followed immediately by a low one.
		if i+12 <= len(result) && result[i+6] == 92 && result[i+7] == 'u' {
			var lo int
			if _, err := fmt.Sscanf(result[i+8:i+12], "%04x", &lo); err == nil &&
				lo >= 0xDC00 && lo <= 0xDFFF {
				continue
			}
		}
		return true
	}
	return false
}

// jsonNestingDepth counts the deepest run of open brackets, ignoring those
// inside strings.
func jsonNestingDepth(result string) int {
	depth, max, inString, escaped := 0, 0, false, false
	for i := 0; i < len(result); i++ {
		c := result[i]
		switch {
		case escaped:
			escaped = false
		case c == 92 && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == '{' || c == '[':
			depth++
			if depth > max {
				max = depth
			}
		case c == '}' || c == ']':
			depth--
		}
	}
	return max
}

// wrapRejectedResult classifies a finalize failure that a backend caused by
// refusing a JSON value, and returns err unchanged otherwise.
//
// The returned error is a *CleatError carrying ErrResultRejected, which is what
// puts "result_rejected_by_store" in the error_code column: the worker derives
// the stored code with errors.As against *CleatError and otherwise falls back to
// "unknown" (cmd/cleat-worker/setup.go).
func wrapRejectedResult(err error, workflowID, result string) error {
	if !isStoreJSONRejection(err) {
		return err
	}
	return &CleatError{
		Code:       ErrResultRejected,
		Op:         "finalize workflow",
		WorkflowID: workflowID,
		Err:        fmt.Errorf("%s: %w", ResultRejectionMessage(result), err),
	}
}

// ResultRejectionMessage is the operator-facing text for a refused result.
//
// EXPORTED SO THAT THE CONSUMER CAN BE TESTED AGAINST THE REAL STRING, which is
// not a stylistic preference here. cmd/cleat-worker consults isConnectionError
// BEFORE deriving an error_code, and isConnectionError matches SUBSTRINGS of the
// message -- "EOF", "broken pipe", "connection reset" and six more. A wording
// change in this function that happened to contain one of them would send a
// refused result down releaseWorkflow instead of recordTerminalFailure: the run
// would be retried forever and no error_code written at all, which is worse than
// the raw driver text cleat#1460 is about.
//
// A test in cmd/cleat-worker asserts exactly that against this function's real
// output. Reconstructing the message there instead would have tested a copy of
// the string and gone green on any wording change -- the two halves have to meet
// somewhere, and this is the seam.
func ResultRejectionMessage(result string) string {
	msg := "the database refused a JSON value written while finalizing this workflow. " +
		"The workflow body ALREADY RAN and its side effects have happened; only the " +
		"record of its outcome failed to store"
	if hint := describeRejectedResult(result); hint != "" {
		msg += ". The workflow result is the likely cause: " + hint
	} else {
		msg += ". The workflow result looks portable, so the refused value is " +
			"probably an event payload rather than the result"
	}
	return msg + ". A workflow result must satisfy every backend cleat supports, not " +
		"only the one in front of you -- see docs/reference/database-backends.md " +
		"and cleat#1025"
}
