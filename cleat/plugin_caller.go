package cleat

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
