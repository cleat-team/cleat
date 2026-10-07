package plugin

import (
	"context"
	"strings"
	"testing"
)

// fakeRegistry captures whatever PluginFunc RegisterTyped hands to
// Register, so a test can invoke it directly with a raw JSON string --
// exactly what the engine does at a real call site, and the only way to
// prove RegisterTyped's wrapping (not just its own body) behaves correctly.
type fakeRegistry struct {
	opts FuncOptions
	fn   PluginFunc
}

func (f *fakeRegistry) Register(opts FuncOptions, fn PluginFunc) error {
	f.opts = opts
	f.fn = fn
	return nil
}

type addRequest struct {
	A int `json:"a"`
	B int `json:"b"`
}

type addResponse struct {
	Sum int `json:"sum"`
}

func TestRegisterTypedRoundTripsRequestAndResponse(t *testing.T) {
	reg := &fakeRegistry{}
	var gotReq addRequest
	err := RegisterTyped(reg, FuncOptions{Name: "add"}, func(_ context.Context, req addRequest) (addResponse, error) {
		gotReq = req
		return addResponse{Sum: req.A + req.B}, nil
	})
	if err != nil {
		t.Fatalf("RegisterTyped: %v", err)
	}

	out, err := reg.fn(context.Background(), `{"a":2,"b":3}`)
	if err != nil {
		t.Fatalf("wrapped fn: %v", err)
	}
	if gotReq != (addRequest{A: 2, B: 3}) {
		t.Errorf("fn received %+v, want {A:2 B:3}", gotReq)
	}
	if out != `{"sum":5}` {
		t.Errorf("output = %q, want {\"sum\":5}", out)
	}
}

func TestRegisterTypedRefusesMalformedInput(t *testing.T) {
	reg := &fakeRegistry{}
	called := false
	if err := RegisterTyped(reg, FuncOptions{Name: "add"}, func(_ context.Context, _ addRequest) (addResponse, error) {
		called = true
		return addResponse{}, nil
	}); err != nil {
		t.Fatalf("RegisterTyped: %v", err)
	}

	_, err := reg.fn(context.Background(), `not json`)
	if err == nil {
		t.Fatal("malformed input was accepted")
	}
	if !strings.Contains(err.Error(), "add") || !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("error %q does not name the function or say what was wrong", err)
	}
	if called {
		t.Error("fn ran despite malformed input -- it must never see a request that failed to unmarshal")
	}
}

// TestRegisterTypedRejectsUnknownFields is cleat#2660, case (a): a typed
// CLIENT is compiled against whatever SDK version a customer pinned, but
// calls a plugin running at whatever version the operator's worker
// deployed. A client newer than the plugin can send a Req field the
// plugin's version of the struct does not declare -- and before this,
// plain json.Unmarshal silently dropped it, so a version-mismatched call
// looked identical to a correct one from both sides. This is the
// concrete fix the owner decided on for cleat#2597 (plugin-side only,
// documented in docs/reference/sdk-api.md's "Plugin Clients" section):
// the mismatch must fail the call loudly instead of succeeding quietly.
func TestRegisterTypedRejectsUnknownFields(t *testing.T) {
	reg := &fakeRegistry{}
	called := false
	if err := RegisterTyped(reg, FuncOptions{Name: "add"}, func(_ context.Context, _ addRequest) (addResponse, error) {
		called = true
		return addResponse{}, nil
	}); err != nil {
		t.Fatalf("RegisterTyped: %v", err)
	}

	// "c" is a field a NEWER client's Req struct has and this (older)
	// plugin's addRequest does not -- exactly cleat#2660 case (a).
	_, err := reg.fn(context.Background(), `{"a":2,"b":3,"c":99}`)
	if err == nil {
		t.Fatal("UNMEASURED: a Req field unknown to this plugin's struct was silently accepted -- " +
			"cleat#2660 case (a) is not fixed, a version-mismatched client would still get a " +
			"silent 200 instead of a loud failure")
	}
	if !strings.Contains(err.Error(), "add") || !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("error %q does not name the function or say what was wrong", err)
	}
	if called {
		t.Error("fn ran despite an unknown field in the request -- it must never see a request " +
			"that failed to decode strictly, the same guarantee TestRegisterTypedRefusesMalformedInput " +
			"already holds for malformed JSON")
	}
}

// TestRegisterTypedStillRejectsTrailingDataAfterTheJSONValue guards against
// a regression the strictness fix above could silently introduce:
// json.Decoder.Decode (needed for DisallowUnknownFields, which
// json.Unmarshal has no equivalent option for) reads exactly one JSON value
// and stops -- unlike json.Unmarshal, it does NOT complain about non-
// whitespace bytes left over afterward. `{"a":1,"b":2}garbage` decodes
// cleanly under a bare Decoder. This asserts decodeStrict does not
// regress that half of json.Unmarshal's behaviour while gaining the other.
func TestRegisterTypedStillRejectsTrailingDataAfterTheJSONValue(t *testing.T) {
	reg := &fakeRegistry{}
	if err := RegisterTyped(reg, FuncOptions{Name: "add"}, func(_ context.Context, req addRequest) (addResponse, error) {
		return addResponse{Sum: req.A + req.B}, nil
	}); err != nil {
		t.Fatalf("RegisterTyped: %v", err)
	}

	_, err := reg.fn(context.Background(), `{"a":2,"b":3}garbage`)
	if err == nil {
		t.Fatal("UNMEASURED: trailing garbage after a valid JSON value was silently accepted -- " +
			"json.Unmarshal would have rejected this, and switching to json.Decoder for " +
			"DisallowUnknownFields must not quietly drop that guarantee")
	}
}

// A business-logic error from fn must reach the caller UNCHANGED, not
// wrapped a second time -- RegisterTyped's own error wrapping is for its
// own two failure modes (bad input, unmarshalable output), not for fn's.
func TestRegisterTypedPassesThroughTheFunctionsOwnError(t *testing.T) {
	reg := &fakeRegistry{}
	wantErr := errAdd
	if err := RegisterTyped(reg, FuncOptions{Name: "add"}, func(_ context.Context, _ addRequest) (addResponse, error) {
		return addResponse{}, wantErr
	}); err != nil {
		t.Fatalf("RegisterTyped: %v", err)
	}

	_, err := reg.fn(context.Background(), `{"a":1,"b":1}`)
	if err != wantErr {
		t.Errorf("error = %v, want the exact error fn returned (%v), unwrapped", err, wantErr)
	}
}

var errAdd = &businessError{"add: deliberately refused"}

type businessError struct{ msg string }

func (e *businessError) Error() string { return e.msg }

func TestRegisterTypedPropagatesTheUnderlyingRegisterError(t *testing.T) {
	reg := &refusingRegistry{}
	err := RegisterTyped[addRequest, addResponse](reg, FuncOptions{Name: "add"}, nil)
	if err == nil {
		t.Fatal("a Register that refuses was not surfaced")
	}
}

type refusingRegistry struct{}

func (*refusingRegistry) Register(FuncOptions, PluginFunc) error {
	return errRefused
}

var errRefused = &businessError{"refused"}
