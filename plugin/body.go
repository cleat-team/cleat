package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// MaxBody wraps h so the host's plugin-route adapter applies limit (bytes) as
// this ROUTE's request-body ceiling instead of the configured default
// (--plugin-max-body-size). Register it with mux.Handle, not mux.HandleFunc --
// the declared limit travels on the http.Handler value itself, which
// HandleFunc's plain func(ResponseWriter, *Request) signature has no room to
// carry:
//
//	mux.Handle("PUT /blobs/{key...}", plugin.MaxBody(p.cfg.MaxBlobSize, h))
//
// cleat#2232. A route that does not call MaxBody gets the default ceiling;
// there is no separate pattern-keyed table for the host to keep in sync with
// what a plugin actually registers.
func MaxBody(limit int64, h http.HandlerFunc) http.Handler {
	return &maxBodyHandler{limit: limit, h: h}
}

type maxBodyHandler struct {
	limit int64
	h     http.HandlerFunc
}

func (m *maxBodyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.h(w, r) }

// MaxBodyLimit reports the ceiling a route declared via MaxBody, if h is a
// value MaxBody produced. The host's plugin-route adapter is the only
// intended caller -- it is how the adapter tells "this route declared its
// own limit" apart from "apply the default", without a plugin having to
// register anywhere other than the Router it was handed.
func MaxBodyLimit(h http.Handler) (limit int64, ok bool) {
	m, ok := h.(*maxBodyHandler)
	if !ok {
		return 0, false
	}
	return m.limit, true
}

// ReadBody reads an ALREADY-BOUNDED request body (the host's plugin-route
// adapter sets r.Body's ceiling before a plugin's handler ever runs -- see
// Router and MaxBody) and writes the response itself on failure, the same
// division of responsibility decodeBody/readBody use in cmd/cleat-worker
// (cleat#1332, cleat#1338): a plugin that calls this never hand-rolls the
// *http.MaxBytesError -> 413 translation, so it cannot get it wrong or omit
// it the way seven core handlers once did. Reports whether the caller should
// proceed.
//
// The limit named in a too-large response comes from the error itself
// (*http.MaxBytesError.Limit), not from a value ReadBody was told -- it has
// none: a route's effective ceiling is the adapter's default unless the
// route declared its own with MaxBody, and ReadBody runs inside the plugin,
// after that decision was already made. Reading it off the error is the one
// place both cases agree.
func ReadBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, true
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeTooLargeError(w, maxErr.Limit)
			return nil, false
		}
		writeBodyError(w, http.StatusBadRequest, "failed to read request body")
		return nil, false
	}
	return body, true
}

// ReadJSONBody is ReadBody plus a json.Unmarshal, for the common case of a
// plugin route that takes a JSON body. It reports whether the caller should
// proceed, exactly as ReadBody does.
func ReadJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, ok := ReadBody(w, r)
	if !ok {
		return false
	}
	if len(body) == 0 {
		return true
	}
	if err := json.Unmarshal(body, dst); err != nil {
		writeBodyError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return false
	}
	return true
}

func writeTooLargeError(w http.ResponseWriter, limit int64) {
	writeBodyError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
		"request body too large: the limit is %d bytes, set by --plugin-max-body-size "+
			"unless this route declares a larger one", limit))
}

func writeBodyError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
