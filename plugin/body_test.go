package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMaxBodyRoundTripsTheLimit is cleat#2232: a route declares its own
// ceiling by wrapping its handler in MaxBody, and the host's plugin-route
// adapter (cmd/cleat-worker/plugin_body_limit.go) reads it back with
// MaxBodyLimit before applying it against --plugin-max-body-size. If either
// side of that round trip broke, every route would silently get the default
// ceiling regardless of what it declared -- slacknotify's fixed
// interactive-callback size would inherit whatever the operator set the
// default to.
func TestMaxBodyRoundTripsTheLimit(t *testing.T) {
	var called bool
	h := MaxBody(4096, func(w http.ResponseWriter, r *http.Request) { called = true })

	limit, fromConfig, knob, ok := MaxBodyLimit(h)
	if !ok {
		t.Fatal("MaxBodyLimit reported false for a value MaxBody produced")
	}
	if limit != 4096 {
		t.Errorf("MaxBodyLimit returned limit %d, want 4096", limit)
	}
	if fromConfig {
		t.Error("MaxBodyLimit reported fromConfig=true for a value MaxBody (not MaxBodyFromConfig) produced")
	}
	if knob != "" {
		t.Errorf("MaxBodyLimit reported knob %q for a plain MaxBody value, want empty", knob)
	}

	// MaxBody must still be a working http.Handler, not just a limit carrier
	// -- the host wraps it and then calls ServeHTTP like any other handler.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Error("MaxBody's Handler did not invoke the wrapped HandlerFunc")
	}
}

// TestMaxBodyFromConfigRoundTripsTheLimitAndKnob is cleat#2273: unlike
// MaxBody, this ceiling is unconditional and carries its own operator-facing
// name, which the host's adapter must read back verbatim to report in a 413
// -- blobstore's max_blob_size is meaningless to an operator staring at a
// message that instead names --plugin-max-body-size.
func TestMaxBodyFromConfigRoundTripsTheLimitAndKnob(t *testing.T) {
	var called bool
	h := MaxBodyFromConfig(10*1024*1024, "max_blob_size in --plugin-config", func(w http.ResponseWriter, r *http.Request) { called = true })

	limit, fromConfig, knob, ok := MaxBodyLimit(h)
	if !ok {
		t.Fatal("MaxBodyLimit reported false for a value MaxBodyFromConfig produced")
	}
	if limit != 10*1024*1024 {
		t.Errorf("MaxBodyLimit returned limit %d, want 10485760", limit)
	}
	if !fromConfig {
		t.Error("MaxBodyLimit reported fromConfig=false for a value MaxBodyFromConfig produced")
	}
	if knob != "max_blob_size in --plugin-config" {
		t.Errorf("MaxBodyLimit returned knob %q, want %q", knob, "max_blob_size in --plugin-config")
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Error("MaxBodyFromConfig's Handler did not invoke the wrapped HandlerFunc")
	}
}

// TestMaxBodyLimitIsFalseForAnUndeclaredRoute is the other half: a route that
// never called MaxBody or MaxBodyFromConfig must report ok=false, not a zero
// limit that could be mistaken for "this route wants a zero-byte body". The
// host's adapter relies on exactly this to fall back to its own default only
// when ok is false.
func TestMaxBodyLimitIsFalseForAnUndeclaredRoute(t *testing.T) {
	plain := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if limit, fromConfig, knob, ok := MaxBodyLimit(plain); ok {
		t.Errorf("MaxBodyLimit reported ok=true (limit=%d, fromConfig=%v, knob=%q) for a plain http.HandlerFunc",
			limit, fromConfig, knob)
	}
}

// errReader always fails, for a reason MaxBytesReader would never produce --
// distinguishing ReadBody's "the read itself is broken" branch from its
// "the body was too large" branch.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestReadBodyUnderTheLimit(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}`))
	w := httptest.NewRecorder()

	body, ok := ReadBody(w, req)
	if !ok {
		t.Fatalf("ok=false for a body under the limit; response: %s", w.Body.String())
	}
	if string(body) != `{"a":1}` {
		t.Errorf("got body %q", body)
	}
	if w.Code != http.StatusOK { // ReadBody must not touch the response on success
		t.Errorf("ReadBody wrote status %d on success; it must write nothing on the happy path", w.Code)
	}
}

func TestReadBodyNilBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Body = nil
	w := httptest.NewRecorder()

	body, ok := ReadBody(w, req)
	if !ok || body != nil {
		t.Errorf("ReadBody(nil Body) = (%q, %v), want (nil, true)", body, ok)
	}
}

// TestReadBodyOverTheLimit is the property #2232 exists for: an ALREADY
// bounded body (the host's boundPluginRequestBody wraps r.Body in
// http.MaxBytesReader before a plugin handler ever runs -- see
// plugin_body_limit.go) comes back as 413 naming the exact limit, not a
// generic read failure and not a hang trying to buffer an unbounded body.
// With no knob attached to the request context, the message falls back to
// naming --plugin-max-body-size -- see TestReadBodyOverTheLimitNamesTheKnob
// for the case where a knob is attached.
func TestReadBodyOverTheLimit(t *testing.T) {
	const limit = 8
	oversized := strings.Repeat("x", limit+1)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(oversized))
	w := httptest.NewRecorder()
	req.Body = http.MaxBytesReader(w, req.Body, limit)

	body, ok := ReadBody(w, req)
	if ok {
		t.Fatalf("ok=true for a body over the limit (got %d bytes)", len(body))
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413 (body: %s)", w.Code, w.Body.String())
	}
	var decoded map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("413 body is not JSON: %v (%s)", err, w.Body.String())
	}
	if want := "8 bytes"; !strings.Contains(decoded["error"], want) {
		t.Errorf("413 message %q does not name the limit it hit (%s)", decoded["error"], want)
	}
	if !strings.Contains(decoded["error"], "--plugin-max-body-size") {
		t.Errorf("413 message %q does not name the knob that moves the limit", decoded["error"])
	}
}

// TestReadBodyOverTheLimitNamesTheKnob is cleat#2273: when the host's adapter
// attaches a knob via WithBodyLimitKnob (MaxBodyFromConfig's case, on a
// non-exempt route), ReadBody's 413 must name THAT setting, not the global
// flag -- an operator staring at "the limit is set by max_blob_size in
// --plugin-config" knows exactly which knob to turn; one naming
// --plugin-max-body-size would send them to the wrong flag entirely.
func TestReadBodyOverTheLimitNamesTheKnob(t *testing.T) {
	const limit = 8
	oversized := strings.Repeat("x", limit+1)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(oversized))
	req = req.WithContext(WithBodyLimitKnob(context.Background(), "max_blob_size in --plugin-config"))
	w := httptest.NewRecorder()
	req.Body = http.MaxBytesReader(w, req.Body, limit)

	if _, ok := ReadBody(w, req); ok {
		t.Fatal("ok=true for a body over the limit")
	}
	msg := bodyError(t, w)
	if !strings.Contains(msg, "max_blob_size in --plugin-config") {
		t.Errorf("413 message %q does not name the attached knob", msg)
	}
	if strings.Contains(msg, "--plugin-max-body-size") {
		t.Errorf("413 message %q names --plugin-max-body-size even though a different knob was attached", msg)
	}
}

// TestReadBodyOnAGenuineReadError is the case ReadBody's *http.MaxBytesError
// branch must NOT catch: a body that fails for a reason that has nothing to
// do with size gets 400, not 413 -- a 413 here would send an operator
// chasing a body-size limit that was never the problem.
func TestReadBodyOnAGenuineReadError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", errReader{})
	w := httptest.NewRecorder()

	if _, ok := ReadBody(w, req); ok {
		t.Fatal("ok=true for a request whose body errors on every read")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "failed to read request body") {
		t.Errorf("400 body %q does not say the read failed", w.Body.String())
	}
}

func TestReadJSONBodyDecodesIntoDst(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"a","n":3}`))
	w := httptest.NewRecorder()

	var dst struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}
	if !ReadJSONBody(w, req, &dst) {
		t.Fatalf("ok=false for valid JSON; response: %s", w.Body.String())
	}
	if dst.Name != "a" || dst.N != 3 {
		t.Errorf("decoded %+v, want {a 3}", dst)
	}
}

// TestReadJSONBodyEmptyBodyIsA400 pins ReadJSONBody to develop's pre-cleat#2232
// behavior: an empty body is invalid JSON like any other, and reported as
// 400. This is the coordinator's #2273 nit -- ReadJSONBody briefly treated an
// empty body as "nothing to decode" universally, which silently changed
// behavior at roughly 25 create/update call sites whose bodies are required.
// A route that genuinely wants an optional body (jobqueue's enqueue) opts in
// via ReadOptionalJSONBody instead; see
// TestReadOptionalJSONBodyEmptyBodyIsNotAnError.
func TestReadJSONBodyEmptyBodyIsA400(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	w := httptest.NewRecorder()

	dst := struct{ Name string }{Name: "unchanged"}
	if ReadJSONBody(w, req, &dst) {
		t.Fatalf("ok=true for an empty body; want a 400, matching every non-opt-in call site's "+
			"pre-cleat#2232 behavior. dst=%+v", dst)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
	if dst.Name != "unchanged" {
		t.Errorf("ReadJSONBody touched dst on a rejected empty body: %+v", dst)
	}
}

// TestReadOptionalJSONBodyEmptyBodyIsNotAnError documents the optional-body
// convention jobqueue's enqueue route relies on (cleat#2273): zero bytes is
// treated as "nothing to decode", not as invalid JSON, and dst is left
// untouched rather than zeroed. Only a caller that explicitly calls this
// function gets that behavior -- see TestReadJSONBodyEmptyBodyIsA400 for the
// default every other route keeps.
func TestReadOptionalJSONBodyEmptyBodyIsNotAnError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	w := httptest.NewRecorder()

	dst := struct{ Name string }{Name: "unchanged"}
	if !ReadOptionalJSONBody(w, req, &dst) {
		t.Fatalf("ok=false for an empty body; response: %s", w.Body.String())
	}
	if dst.Name != "unchanged" {
		t.Errorf("ReadOptionalJSONBody touched dst on an empty body: %+v", dst)
	}
	if w.Code != http.StatusOK {
		t.Errorf("ReadOptionalJSONBody wrote status %d for an empty body; it must write nothing", w.Code)
	}
}

// TestReadOptionalJSONBodyDecodesANonEmptyBody is the other half: a caller
// that opts into the optional-body convention must still decode a body that
// IS present, exactly as ReadJSONBody does.
func TestReadOptionalJSONBodyDecodesANonEmptyBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"a"}`))
	w := httptest.NewRecorder()

	var dst struct {
		Name string `json:"name"`
	}
	if !ReadOptionalJSONBody(w, req, &dst) {
		t.Fatalf("ok=false for valid JSON; response: %s", w.Body.String())
	}
	if dst.Name != "a" {
		t.Errorf("decoded %+v, want {a}", dst)
	}
}

// TestReadJSONBodyInvalidJSON pins the message shape scheduledbackup's and
// other plugins' behavioural tests assert a prefix of
// (TestSB_CreateConfig_InvalidJSON, cleat#2232): "invalid JSON: <cause>", not
// the old fixed "invalid JSON body" / "invalid request body" strings that
// named neither the cause nor, before this issue, the size.
func TestReadJSONBodyInvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{not json`))
	w := httptest.NewRecorder()

	var dst map[string]any
	if ReadJSONBody(w, req, &dst) {
		t.Fatal("ok=true for malformed JSON")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(bodyError(t, w), "invalid JSON: ") {
		t.Errorf("400 body %q does not start with %q", w.Body.String(), "invalid JSON: ")
	}
}

// TestReadJSONBodyOverTheLimit checks the composition, not just each half in
// isolation: ReadJSONBody must surface ReadBody's 413 verbatim rather than
// trying to json.Unmarshal a partially-read body and reporting a confusing
// "invalid JSON" instead of the real cause.
func TestReadJSONBodyOverTheLimit(t *testing.T) {
	const limit = 8
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"x":"`+strings.Repeat("y", limit+1)+`"}`))
	w := httptest.NewRecorder()
	req.Body = http.MaxBytesReader(w, req.Body, limit)

	var dst map[string]any
	if ReadJSONBody(w, req, &dst) {
		t.Fatal("ok=true for a body over the limit")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413 (body: %s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "invalid JSON") {
		t.Errorf("413 body %q was reported as invalid JSON instead of too large", w.Body.String())
	}
}

func bodyError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var decoded map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response body is not JSON: %v (%s)", err, w.Body.String())
	}
	return decoded["error"]
}
