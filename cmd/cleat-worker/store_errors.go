package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"

	"github.com/cleat-team/cleat/engine"
)

// storeDomainErrors are the failures a store call reports on purpose, each one
// a statement WE wrote about the caller's request (this does not exist, that
// already does) rather than something the database said. They keep their own
// status and their own text. The text is the sentinel's, not the error's: a
// store may wrap a sentinel with detail, and the wrap is where a driver string
// would ride in.
//
// An error not in this table is an infrastructure failure to this layer, and
// writeStoreError does not describe it to the caller (cleat#3878). Add a
// sentinel here when a handler's route can legitimately return it; do not add
// one so that a failure stops being logged.
var storeDomainErrors = []struct {
	err    error
	status int
}{
	{engine.ErrWorkflowNotFound, http.StatusNotFound},
	{engine.ErrPromiseNotFound, http.StatusNotFound},
	{engine.ErrRoutingRuleNotFound, http.StatusNotFound},
	{engine.ErrScheduleNotFound, http.StatusNotFound},
	{engine.ErrScheduleExists, http.StatusConflict},
	{engine.ErrIdempotencyKeyDefMismatch, http.StatusConflict},
	{engine.ErrIdempotencyKeyInputMismatch, http.StatusConflict},
}

// writeStoreError answers a failed store call.
//
// A domain error (storeDomainErrors) keeps its status and its message. Anything
// else -- a driver, network or unknown error -- answers a fixed body that
// carries NO text from the error: 503 with Retry-After when
// engine.StoreErrorIsTransient says a retry can succeed, 500 otherwise. The
// full error is logged here, with a request id, and the same id is in the
// response body, so an operator can go from a client's report to the log line.
//
// Why not send the text: a driver's message can carry hostnames, statement
// text and role names, and a client can do nothing with it. Why 503 and not
// 500 for a deadlock: a 500 tells a client the request is not worth sending
// again, and that is wrong for a failure that was a collision with another
// transaction (cleat#3878; the same reasoning as writeAccessCheckUnavailable).
//
// Use it where a handler would have written `writeError(w, 500, err.Error())`.
// scripts/check-no-error-text-in-500.py fails the build when that shape comes
// back.
func (s *apiServer) writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	for _, d := range storeDomainErrors {
		if errors.Is(err, d.err) {
			s.writeError(w, d.status, d.err.Error())
			return
		}
	}
	id := newRequestID()
	status := http.StatusInternalServerError
	msg := "the server could not complete this request"
	level := slog.LevelError
	if engine.StoreErrorIsTransient(err) {
		status = http.StatusServiceUnavailable
		msg = "the server could not complete this request just now; retry shortly"
		level = slog.LevelWarn
		w.Header().Set("Retry-After", "1")
	}
	slog.Log(r.Context(), level, "store call failed; answering without the error text",
		"request_id", id, "method", r.Method, "path", r.URL.Path, "status", status, "error", err)
	w.Header().Set("X-Request-Id", id)
	s.writeJSON(w, status, map[string]string{"error": msg, "request_id": id})
}

// newRequestID is 16 hex characters of crypto/rand. It exists to be quoted by a
// client and searched for in a log, so it is short; it is not a secret and
// carries no information.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(b[:])
}
