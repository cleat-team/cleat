package engine

import "errors"

// ErrExportNotFound marks "this module does not have that export" as distinct
// from "that export ran and failed".
//
// IMPROVEMENT-PLAN §3.35. Both arrived as a bare fmt.Errorf, so the one caller
// that has to tell them apart -- the defer fallback in runDefers -- could not,
// and logged `defer execution failed ... export "cleat_defer_defer-0" not
// found` for every killed workflow. No guest in any language emits an export
// by that name, so the message described a convention the guest was never
// expected to follow, in the voice of a cleanup that had gone wrong. After
// #559 and #560 it could be emitted immediately *after* the cleanup succeeded.
//
// Matched with errors.Is, never by substring. A message-substring check is the
// same mistake one layer up: it matches whatever the wording happens to be
// today, and this repo has already had a check that matched an error message
// rather than the condition and reported a broken database as healthy.
var ErrExportNotFound = errors.New("export not found")
