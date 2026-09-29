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
