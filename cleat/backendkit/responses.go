package backendkit

import (
	"encoding/json"
	"errors"
	"net/http"
)

// WriteJSON marshals data as JSON and writes it to the response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// WriteError writes a JSON error response with the given status code and message.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// WriteValidationError is a shortcut for 400 Bad Request.
func WriteValidationError(w http.ResponseWriter, msg string) {
	WriteError(w, http.StatusBadRequest, msg)
}

// WriteNotFound is a shortcut for 404 Not Found.
func WriteNotFound(w http.ResponseWriter) {
	WriteError(w, http.StatusNotFound, "not found")
}

// WriteInternalError is a shortcut for 500 Internal Server Error.
func WriteInternalError(w http.ResponseWriter) {
	WriteError(w, http.StatusInternalServerError, "internal server error")
}

// upstreamStatusesToPassThrough are the worker statuses that describe the
// CALLER's own request, not the backend's credential to the worker.
//
// cleat-review (cleat#2810 R1): every example backend that uses this holds
// the worker's API key server-side and never lets a browser request supply
// one -- authTransport (order-lifecycle/backend/main.go, and the same shape
// in ai-agent-platform and integration-hub) deletes any Authorization/Cookie
// header the browser sent and injects its own. So a 401 or 403 from the
// worker is ALWAYS about the backend's own key, never the browser caller's
// credentials -- and the browser sent none, so passing that status straight
// through tells it "your credentials were rejected" for credentials it never
// had. That is the same wrong-layer defect cleat#2718 exists to fix, in the
// other direction: mapping the CALLER's request status onto the BACKEND's
// own auth failure.
//
// An explicit allowlist, not "every 4xx except 401/403/407": a denylist has
// to name every auth-shaped status the worker could ever return to stay
// correct, and a new one added later would silently pass through until
// someone noticed. Adding a status here is a deliberate, one-line decision
// that it describes the caller's request.
var upstreamStatusesToPassThrough = map[int]bool{
	http.StatusBadRequest:          true, // 400: the request itself was malformed
	http.StatusNotFound:            true, // 404: no such workflow/run
	http.StatusConflict:            true, // 409: e.g. an idempotency-key mismatch
	http.StatusUnprocessableEntity: true, // 422: well-formed, semantically invalid
	http.StatusTooManyRequests:     true, // 429: caller is rate-limited
}

// WriteUpstreamError writes a response for an error returned by a Client
// call (StartWorkflow, GetWorkflow, ...), passing the worker's own status
// through when upstreamStatusesToPassThrough says it describes the caller's
// own request. Anything else -- a 5xx from the worker, a transport failure,
// an auth status (see upstreamStatusesToPassThrough's doc comment), an error
// with no recoverable status (errors.As finds no *UpstreamStatusError) --
// maps to 502 Bad Gateway. The body still carries the real status in its
// text ("unexpected status 401: ..."), so an operator reading logs is not
// blind to it; only the HTTP status line is normalized.
//
// cleat#2718: every example backend called
// WriteError(w, http.StatusBadGateway, err.Error()) directly for any Client
// error, so a 404 from the worker (no such run) was indistinguishable from
// the worker being unreachable -- both read 502 to the caller. classifyError
// now attaches the real status to every error it returns; this is the one
// place that reads it back out, so a caller does not have to import errors
// and repeat the errors.As dance at every site.
func WriteUpstreamError(w http.ResponseWriter, err error) {
	var se *UpstreamStatusError
	if errors.As(err, &se) && upstreamStatusesToPassThrough[se.Status] {
		WriteError(w, se.Status, err.Error())
		return
	}
	WriteError(w, http.StatusBadGateway, err.Error())
}
