package cleat

import (
	"encoding/json"
	"fmt"
	"time"
)

// PluginCaller is a marker interface implemented by typed wrappers around
// plugin function calls. The transformer recognizes types implementing this
// interface and treats their methods as opaque HostCalls leaves — no threading
// analysis needed, no forbidden-construct validation, no callgraph tracing
// into the method body.
//
// The single marker method cleatPlugin is never called; it exists solely
// for the transformer to detect plugin call wrappers at the type level.
type PluginCaller interface {
	cleatPlugin()
}

// PluginFunc wraps a (pluginName, functionName) pair with typed generics.
// It implements PluginCaller, so the transformer trusts it automatically.
//
// Use NewPluginFunc to create instances. Call h is passed at each Call
// site rather than stored, so a single PluginFunc can be shared across
// workflows.
type PluginFunc[Req, Resp any] struct {
	plugin   string
	function string
	// wake names the signals a claim-shaped function's registration wakes; see
	// WithWake and WaitFor. Nil for a plain PluginFunc.
	wake func(Req) []string
}

// NewPluginFunc creates a typed plugin function wrapper.
//
// Usage:
//
//	var BlobstorePut = cleat.NewPluginFunc[PutRequest, PutResult]("blobstore", "put")
//	result, err := BlobstorePut.Call(h, PutRequest{Key: "k", Data: "d"})
func NewPluginFunc[Req, Resp any](plugin, function string) *PluginFunc[Req, Resp] {
	return &PluginFunc[Req, Resp]{plugin: plugin, function: function}
}

// cleatPlugin satisfies the PluginCaller marker interface.
func (*PluginFunc[Req, Resp]) cleatPlugin() {}

// Call invokes the plugin function with the given HostCalls and request.
// The request is marshaled to JSON, the plugin function is called via
// h.PluginCall, and the JSON response is unmarshaled into Resp.
func (pf *PluginFunc[Req, Resp]) Call(h HostCalls, req Req) (Resp, error) {
	return PluginCallTyped[Resp](h, pf.plugin, pf.function, req)
}

// WithWake returns a PluginFunc for the same plugin function that also knows
// which signals its registration wakes, so WaitFor can suspend on them. wake is
// computed from the request because the signal depends on it (for a webhook
// await, "__evt:"+the event type). The receiver is not modified.
//
// This is for a CLAIM-SHAPED function: one whose output carries a boolean
// "found". A package that wraps such a function hands its callers the result and
// keeps the wake signal's name to itself; see pluginclients/webhookingest.
func (pf *PluginFunc[Req, Resp]) WithWake(wake func(Req) []string) *PluginFunc[Req, Resp] {
	cp := *pf
	cp.wake = wake
	return &cp
}

// WaitFor blocks durably until the function claims an event for req, and returns
// it, or returns an error when timeout has passed. It is typed access to
// h.WaitForEvent, which owns the loop: claim, suspend on the signal the claim's
// registration wakes, claim again (a wake is not proof of a claimable event).
// The deadline comes from the durable clock, so a replay computes the same one.
//
// A timeout is an ERROR, not a flag: that is how h.WaitForEvent reports it. An
// error whose text names "no event for <plugin>/<function>" is the window
// closing; any other is the claim failing.
//
// COST OF A LONG WAIT. While no event arrives the engine re-checks the claim,
// waiting 6 s, then 12 s, doubling to a cap of 5 minutes, and records two history
// events each time. Measured on a real worker over 35 minutes: 24 events, and
// 12 of those in the first five minutes, so about 24 an hour once past the
// ramp. At a constant 6 s the same wait recorded 556 events (about 950 an hour).
// A window of minutes, such as a payment confirmation, costs tens of events. A
// window of hours or days costs about 24 an hour and still grows: it is bounded
// in practice, not constant. A wake that fails to deliver (the publisher stopped
// between storing the event and signalling it) can take up to those 5 minutes to
// be noticed (cleat#3806).
//
// The PluginFunc must have been given a wake with WithWake, and timeout must be
// at least a millisecond. Both are refused rather than allowed to wait forever.
//
// The build finds the host calls a workflow makes through this method by reading its body (analyzer.DepHostCalls,
// cleat#3803): h.WaitForEvent here is what links cleat_wait_for_event. Before that it was named in two hand lists,
// and cleat#3794 measured what an unlisted helper does: a package-level function making the same call compiled
// clean and died on a real worker with a nil pointer. testdata/sdkhelpers/waitfor is the fixture.
func (pf *PluginFunc[Req, Resp]) WaitFor(h HostCalls, req Req, timeout time.Duration) (Resp, error) {
	var zero Resp
	if pf.wake == nil {
		return zero, fmt.Errorf("cleat: WaitFor on %s/%s, which has no wake signal; use WithWake first", pf.plugin, pf.function)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return zero, fmt.Errorf("cleat: WaitFor %s/%s: marshal request: %w", pf.plugin, pf.function, err)
	}
	out, err := h.WaitForEvent(pf.plugin, pf.function, string(payload), pf.wake(req), timeout)
	if err != nil {
		return zero, err
	}
	var resp Resp
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return zero, fmt.Errorf("cleat: WaitFor %s/%s: decode response: %w", pf.plugin, pf.function, err)
	}
	return resp, nil
}

// BoundPluginFunc is a PluginFunc pre-bound to a specific HostCalls value.
// It implements PluginCaller. Use PluginFunc.Bind to create one.
//
// BoundPluginFunc is useful when you want to pass a client object around
// without threading h through every call site:
//
//	client := cleat.NewPluginFunc[Req, Resp]("svc", "op").Bind(h)
//	result, err := client.Call(req)
type BoundPluginFunc[Req, Resp any] struct {
	h        HostCalls
	plugin   string
	function string
}

// Bind returns a BoundPluginFunc that holds the given HostCalls.
func (pf *PluginFunc[Req, Resp]) Bind(h HostCalls) *BoundPluginFunc[Req, Resp] {
	return &BoundPluginFunc[Req, Resp]{h: h, plugin: pf.plugin, function: pf.function}
}

// cleatPlugin satisfies the PluginCaller marker interface.
func (*BoundPluginFunc[Req, Resp]) cleatPlugin() {}

// Call invokes the pre-bound plugin function with the given request.
func (bpf *BoundPluginFunc[Req, Resp]) Call(req Req) (Resp, error) {
	return PluginCallTyped[Resp](bpf.h, bpf.plugin, bpf.function, req)
}
