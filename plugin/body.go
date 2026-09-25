package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// defaultBodyLimitKnob is the operator-facing name for whatever produced a
// request's effective body-size ceiling when no more specific knob applies:
// the bare default (no MaxBody/MaxBodyFromConfig at all), or a MaxBody
// ceiling, which always sits under this flag regardless of which of the two
// values (the route's own n, or the flag) ends up binding -- see MaxBody's
// doc comment.
const defaultBodyLimitKnob = "--plugin-max-body-size"

// MaxBody wraps h so the host's plugin-route adapter applies n (bytes) as a
// TIGHTER ceiling than the configured default (--plugin-max-body-size) for
// this route -- never a looser one. The adapter computes
// min(n, --plugin-max-body-size) as the effective limit, and a 413 from this
// route always names --plugin-max-body-size, whichever of the two values
// was actually binding: an operator can always turn the flag down and have
// it take effect here too, so the message names the knob that is always in
// play, not whichever value happened to lose the min().
//
// Register it with mux.Handle, not mux.HandleFunc -- the declared limit
// travels on the http.Handler value itself, which HandleFunc's plain
// func(ResponseWriter, *Request) signature has no room to carry:
//
//	mux.Handle("POST /slack/interactive", plugin.MaxBody(interactiveMaxBodySize, h))
//
// cleat#2232. A route that does not call MaxBody or MaxBodyFromConfig gets
// the default ceiling; there is no separate pattern-keyed table for the
// host to keep in sync with what a plugin actually registers.
//
// Use MaxBodyFromConfig instead when the ceiling is not a tighter cap under
// the operator's flag but the plugin's OWN operator-configured limit (for
// example, a maximum blob size) that must apply regardless of what
// --plugin-max-body-size is set to.
func MaxBody(n int64, h http.HandlerFunc) http.Handler {
	return &maxBodyHandler{limit: n, h: h}
}

// MaxBodyFromConfig wraps h so the host's plugin-route adapter applies n
// (bytes) as this route's request-body ceiling UNCONDITIONALLY -- ignoring
// --plugin-max-body-size entirely, in either direction. Use this when n is
// itself an operator-facing setting (for example, blobstore's
// --plugin-config max_blob_size), so the operator who set n did not also
// mean it to be silently overridden by an unrelated global flag.
//
// knob names the setting that produced n, and appears in this route's 413
// response instead of --plugin-max-body-size -- an operator who sees the
// message needs to know which of possibly several settings to change.
//
// REFUSED on every route this codebase exempts from tenant auth
// (auth.Middleware's and auth.HostBindingMiddleware's publicPatterns in
// cmd/cleat-worker/main.go): those routes are reachable with no credential
// at all, so the operator's global --plugin-max-body-size ceiling must
// always bound them regardless of what a plugin's own config claims. The
// host's plugin-route adapter (cmd/cleat-worker/plugin_body_limit.go)
// enforces this by falling back to the default ceiling and knob on those
// patterns even when a plugin registers MaxBodyFromConfig there --
// see TestPluginRouteBodyLimitAppliesOnBothAuthExemptRoutes and
// TestMaxBodyFromConfigIsClampedOnAnAuthExemptRoute.
func MaxBodyFromConfig(n int64, knob string, h http.HandlerFunc) http.Handler {
	return &maxBodyHandler{limit: n, knob: knob, fromConfig: true, h: h}
}

type maxBodyHandler struct {
	limit      int64
	knob       string
	fromConfig bool
	h          http.HandlerFunc
}

func (m *maxBodyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.h(w, r) }

// MaxBodyLimit reports the ceiling and knob a route declared via MaxBody or
// MaxBodyFromConfig, if h is a value one of them produced. fromConfig is
// true only for MaxBodyFromConfig, and distinguishes "n is an unconditional
// ceiling" from "n participates in min() with --plugin-max-body-size". knob
// is only meaningful when fromConfig is true; MaxBody-declared routes always
// report their 413 under --plugin-max-body-size regardless of knob's zero
// value here.
//
// The host's plugin-route adapter is the only intended caller -- it is how
// the adapter tells "this route declared its own limit" apart from "apply
// the default", without a plugin having to register anywhere other than the
// Router it was handed.
func MaxBodyLimit(h http.Handler) (limit int64, fromConfig bool, knob string, ok bool) {
	m, ok := h.(*maxBodyHandler)
	if !ok {
		return 0, false, "", false
	}
	return m.limit, m.fromConfig, m.knob, true
}

type bodyLimitKnobKey struct{}

// WithBodyLimitKnob attaches the operator-facing name of whichever setting
// produced ctx's body-size ceiling, so ReadBody's 413 message names the
// right one. Called by the host's plugin-route adapter
// (cmd/cleat-worker/plugin_body_limit.go) alongside http.MaxBytesReader,
// once per request, using the same knob MaxBodyLimit reported (or
// defaultBodyLimitKnob when the route declared no ceiling of its own, or was
// clamped back to it) -- a plugin author never calls this directly.
func WithBodyLimitKnob(ctx context.Context, knob string) context.Context {
	return context.WithValue(ctx, bodyLimitKnobKey{}, knob)
}

func bodyLimitKnobFromContext(ctx context.Context) string {
	if knob, ok := ctx.Value(bodyLimitKnobKey{}).(string); ok && knob != "" {
		return knob
	}
	return defaultBodyLimitKnob
}

// ReadBody reads an ALREADY-BOUNDED request body (the host's plugin-route
// adapter sets r.Body's ceiling before a plugin's handler ever runs -- see
// Router, MaxBody, and MaxBodyFromConfig) and writes the response itself on
// failure, the same division of responsibility decodeBody/readBody use in
// cmd/cleat-worker (cleat#1332, cleat#1338): a plugin that calls this never
// hand-rolls the *http.MaxBytesError -> 413 translation, so it cannot get it
// wrong or omit it the way seven core handlers once did. Reports whether the
// caller should proceed.
//
// The limit named in a too-large response comes from the error itself
// (*http.MaxBytesError.Limit), not from a value ReadBody was told -- it has
// none: a route's effective ceiling is decided entirely by the adapter
// before ReadBody ever runs. Which SETTING that limit came from -- the
// default, this route's own MaxBody, or this route's own MaxBodyFromConfig
// -- travels separately, on r.Context() via WithBodyLimitKnob, because
// ReadBody has no other way to learn it.
func ReadBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, true
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeTooLargeError(w, r, maxErr.Limit)
			return nil, false
		}
		writeBodyError(w, http.StatusBadRequest, "failed to read request body")
		return nil, false
	}
	return body, true
}

// ReadJSONBody is ReadBody plus a json.Unmarshal, for the common case of a
// plugin route that takes a required JSON body. An empty body is a 400,
// exactly like an empty body reaching json.Unmarshal always has been on
// every route that has not opted into ReadOptionalJSONBody -- this function
// does not special-case it. It reports whether the caller should proceed,
// exactly as ReadBody does.
func ReadJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, ok := ReadBody(w, r)
	if !ok {
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		writeBodyError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return false
	}
	return true
}

// ReadOptionalJSONBody is ReadJSONBody for a route whose body is genuinely
// optional: an empty body leaves dst untouched and reports ok=true rather
// than 400ing. Opt in by calling this instead of ReadJSONBody -- do not make
// an empty body acceptable everywhere, only where a caller has decided that
// for its own route. It reports whether the caller should proceed, exactly
// as ReadBody and ReadJSONBody do.
func ReadOptionalJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
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

func writeTooLargeError(w http.ResponseWriter, r *http.Request, limit int64) {
	knob := bodyLimitKnobFromContext(r.Context())
	writeBodyError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
		"request body too large: the limit is %d bytes, set by %s", limit, knob))
}

func writeBodyError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
