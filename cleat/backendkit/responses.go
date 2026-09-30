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

// WriteUpstreamError writes a response for an error returned by a Client
// call (StartWorkflow, GetWorkflow, ...), passing the worker's own status
// through when it is a 4xx client error the caller can act on -- the request
// itself was refused, which is not the same as the gateway failing. Anything
// else -- a 5xx from the worker, a transport failure, an error with no
// recoverable status (errors.As finds no *UpstreamStatusError) -- maps to
// 502 Bad Gateway.
//
// cleat#2718: every example backend called
// WriteError(w, http.StatusBadGateway, err.Error()) directly for any Client
// error, so a 401 from the worker (wrong credentials, an expired key) was
// indistinguishable from the worker being unreachable -- both read 502 to
// the caller. classifyError now attaches the real status to every error it
// returns; this is the one place that reads it back out, so a caller does
// not have to import errors and repeat the errors.As dance at every site.
func WriteUpstreamError(w http.ResponseWriter, err error) {
	var se *UpstreamStatusError
	if errors.As(err, &se) && se.Status >= 400 && se.Status < 500 {
		WriteError(w, se.Status, err.Error())
		return
	}
	WriteError(w, http.StatusBadGateway, err.Error())
}
