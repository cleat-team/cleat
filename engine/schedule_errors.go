package engine

import "errors"

// ErrScheduleNotFound is returned by DeleteSchedule and SetScheduleEnabled
// when no schedule of that name exists for the caller's tenant. cleat#1297.
//
// Before this existed, all three dialects discarded the statement result, so
// zero rows affected was indistinguishable from one and the API answered
// `200 {"status":"disabled"}` for a name that does not exist. An operator who
// mistypes a schedule name during an incident is told it is disabled while it
// keeps firing.
//
// WHY AN EXISTENCE CHECK RATHER THAN RowsAffected. The obvious fix --
// `RowsAffected == 0 -> not found` -- is correct on PostgreSQL and SQL Server
// and WRONG ON MYSQL, where an UPDATE that sets a column to the value it
// already holds reports **0** affected rows unless the connection sets
// CLIENT_FOUND_ROWS, which cleat does nowhere. That would turn disabling an
// already-disabled schedule into a 404 on one dialect only -- the kind of
// split that passes every test on the dialect the author ran.
//
// An explicit existence check costs one statement and depends on no
// dialect-specific semantics at all, which is also why it is used for DELETE:
// a DELETE's affected-row count is believed unambiguous everywhere, but that
// belief could not be measured here (no MySQL instance was available to this
// author), and an unverifiable assumption is not worth the round trip it
// saves on an admin endpoint.
var ErrScheduleNotFound = errors.New("schedule not found")
